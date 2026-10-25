package openapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Transport is the transport name the adapter reports in Describe.
const Transport = "openapi"

// Defaults of the adapter configuration.
const (
	// DefaultIdentityHeader carries the minimum identity (adapters.Identity) as
	// unpadded base64url of its JSON, on every call that is made for a scope.
	DefaultIdentityHeader = "X-AlixAI-Identity"
	// DefaultIdempotencyHeader carries the idempotency key of a Commit, Status
	// or Cancel, and is read back from the reply to check that the reply answers
	// the key that was sent.
	DefaultIdempotencyHeader = "Idempotency-Key"
	// DefaultAuthHeader and DefaultAuthScheme say how a credential is sent.
	DefaultAuthHeader = "Authorization"
	DefaultAuthScheme = "Bearer"

	maxEndpoints    = 16
	maxCapabilities = 1024
	maxSpecBytes    = 4 << 20
	// maxRequestBytes bounds the body the adapter sends (a prepared action or a
	// request body assembled from fields).
	maxRequestBytes = 1 << 20
)

var (
	endpointIDRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	operationIDRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,127}$`)
	argumentNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
	headerTokenRE  = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$")
	authSchemeRE   = regexp.MustCompile(`^[A-Za-z0-9._~+/-]{1,32}$`)
	sha256HexRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// credentialRE is the manifest's grammar of a credential reference (defs.schema.json
	// credentialRef): its prefix and alphabet are why a pasted key does not fit.
	credentialRE = regexp.MustCompile(`^cred:[a-z][a-z0-9_.-]{2,63}$`)
)

// reservedHeaders are headers the HTTP stack or the protocol owns. The adapter
// never fills one of them from a field, and configuration may not point the
// credential, identity or idempotency header at one.
var reservedHeaders = []string{
	"authorization", "host", "content-length", "content-type", "accept", "accept-encoding", "transfer-encoding", "connection", "upgrade",
	"te", "trailer", "expect", "cookie", "set-cookie", "user-agent", "proxy-authorization", "proxy-connection",
	"keep-alive", "date", "range", "if-match", "if-none-match", "origin", "referer",
}

func reservedHeader(name string) bool {
	return slices.Contains(reservedHeaders, strings.ToLower(name))
}

// ConfigError is the cause of the validation_failed error New returns for a
// configuration it refuses.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return "openapi adapter config: " + e.Msg }

func cfgErr(format string, args ...any) error {
	return contracts.Wrap(contracts.ErrValidationFailed, &ConfigError{Msg: fmt.Sprintf(format, args...)}, "the OpenAPI adapter configuration is invalid")
}

// Endpoint is one HTTP API: its base URL, the OpenAPI document that describes
// the approved subset of it, and how to talk to it.
type Endpoint struct {
	// ID is the endpoint's ID in the manifest.
	ID string
	// BaseURL is the API's base URL (http or https, no user information, no
	// query, no fragment). Every request goes to it and nowhere else, through
	// Config.Dialer, so it must be on the egress allowlist. The document's own
	// `servers` are ignored.
	BaseURL string
	// Spec is the OpenAPI 3.x document, JSON or YAML, already read from the
	// package. It is loaded without any external reference. Operations in it
	// that no capability maps (and the adapter configuration does not name) are
	// unreachable.
	Spec []byte
	// Settings is the parsed adapter configuration (ParseAdapterConfig).
	Settings AdapterConfig
	// ContractSHA256 is the manifest's optional pin of the approved operation
	// subset (ContractSHA256). A document whose subset hashes differently is
	// refused when the adapter is built.
	ContractSHA256 string
	// CredentialRef is the opaque reference of the endpoint's credential. It
	// needs Config.Secrets.
	CredentialRef string
}

// Binding is one operation of a capability mapped to one operation of one
// endpoint's document.
type Binding struct {
	// Endpoint is the ID of the endpoint that owns the operation.
	Endpoint string
	// OperationID is the operationId, compared byte for byte.
	OperationID string
	// Timeout bounds one call of the operation; 0 leaves the caller's deadline.
	Timeout time.Duration
	// Arguments renames capability fields to the operation's parameter or body
	// property names, for the fields that differ. Fields not listed keep their
	// name.
	Arguments map[string]string
}

// Capability is what a package maps for one capability.
type Capability struct {
	// ID is the capability ID.
	ID string
	// Operations maps adapter operations to API operations. A package maps read,
	// validate, prepare, commit and status; cancel is mapped only by a host that
	// wires the adapter in code, because a package has no cancel operation.
	Operations map[adapters.Operation]Binding
	// Resolve maps an entity type to the operation that resolves a mention of
	// it. A Read whose fields carry the reserved argument entity_type is routed
	// here and nowhere else.
	Resolve map[string]Binding
}

// Config configures New.
type Config struct {
	// ApplicationID names the application, for logs only; the scope check is
	// adapters.Wrap's.
	ApplicationID string
	// Endpoints are the APIs. At least one.
	Endpoints []Endpoint
	// Capabilities is the package's mapping. Only what is mapped here (and the
	// management operations the adapter configuration names) can ever be called.
	Capabilities []Capability
	// Dialer makes every connection. nil refuses everything (default deny).
	Dialer *egress.Dialer
	// Secrets resolves credential references. Required when an endpoint has one.
	Secrets SecretProvider
	// MaxResponseBytes bounds one reply body; 0 means
	// adapters.DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// Logger receives what the adapter logs: the credential provider failing
	// (never the reference or the credential), and, at debug level, one record
	// per call with the operationId, the status and the duration. It never
	// receives a URL, a header, a body, a field value or text the application
	// sent. nil discards.
	Logger *slog.Logger
	// UserAgent is sent with every request; "" means "alixai".
	UserAgent string
}

// validate checks a Config and fills its defaults.
func (c *Config) validate() error {
	if c.ApplicationID == "" {
		return cfgErr("ApplicationID is empty")
	}
	if n := len(c.Endpoints); n == 0 || n > maxEndpoints {
		return cfgErr("between 1 and %d endpoints are required", maxEndpoints)
	}
	if len(c.Capabilities) > maxCapabilities {
		return cfgErr("more than %d capabilities", maxCapabilities)
	}
	seen := map[string]bool{}
	for i := range c.Endpoints {
		ep := &c.Endpoints[i]
		if !endpointIDRE.MatchString(ep.ID) {
			return cfgErr("endpoint %d has an empty or malformed ID", i)
		}
		if seen[ep.ID] {
			return cfgErr("endpoint %q is declared twice", ep.ID)
		}
		seen[ep.ID] = true
		if _, err := parseBaseURL(ep.BaseURL); err != nil {
			return cfgErr("endpoint %q: %v", ep.ID, err)
		}
		if len(ep.Spec) == 0 || len(ep.Spec) > maxSpecBytes {
			return cfgErr("endpoint %q: the OpenAPI document is empty or larger than %d bytes", ep.ID, maxSpecBytes)
		}
		if ep.ContractSHA256 != "" && !sha256HexRE.MatchString(ep.ContractSHA256) {
			return cfgErr("endpoint %q: contract_sha256 is not a lower-case hex SHA-256", ep.ID)
		}
		if ep.CredentialRef != "" {
			if !credentialRE.MatchString(ep.CredentialRef) {
				return cfgErr("endpoint %q: the credential reference is malformed", ep.ID)
			}
			if c.Secrets == nil {
				return cfgErr("endpoint %q declares a credential reference and no credential provider is configured", ep.ID)
			}
		}
		if err := ep.Settings.resolve(ep.ID, ep.CredentialRef != ""); err != nil {
			return err
		}
	}
	checkBinding := func(where string, b Binding) error {
		switch {
		case !seen[b.Endpoint]:
			return cfgErr("%s maps an operation of the unknown endpoint %q", where, truncate(b.Endpoint, 40))
		case !operationIDRE.MatchString(b.OperationID):
			return cfgErr("%s maps the malformed operationId %q", where, truncate(b.OperationID, 40))
		case b.Timeout < 0:
			return cfgErr("%s has a negative timeout", where)
		}
		targets := map[string]bool{}
		for from, to := range b.Arguments {
			if !argumentNameRE.MatchString(from) || !argumentNameRE.MatchString(to) || targets[to] {
				return cfgErr("%s renames argument %q to %q, which is malformed or used twice", where, truncate(from, 40), truncate(to, 40))
			}
			targets[to] = true
		}
		return nil
	}
	ids := map[string]bool{}
	for _, cp := range c.Capabilities {
		if cp.ID == "" || strings.ContainsAny(cp.ID, "* \t\r\n") || ids[cp.ID] {
			return cfgErr("a capability ID is empty, malformed or declared twice")
		}
		ids[cp.ID] = true
		for op, b := range cp.Operations {
			if _, ok := op.Result(); !ok {
				return cfgErr("capability %q maps %q, which is not an operation that returns a result", cp.ID, op)
			}
			if err := checkBinding("capability "+cp.ID+" "+string(op), b); err != nil {
				return err
			}
		}
		for typ, b := range cp.Resolve {
			if typ == "" || strings.ContainsAny(typ, "* \t\r\n") {
				return cfgErr("capability %q resolves a malformed entity type", cp.ID)
			}
			if err := checkBinding("capability "+cp.ID+" resolver of "+typ, b); err != nil {
				return err
			}
		}
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = adapters.DefaultMaxResponseBytes
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.UserAgent == "" {
		c.UserAgent = "alixai"
	}
	return nil
}

// parseBaseURL checks an endpoint's base URL.
func parseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, errors.New("the base URL does not parse")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("the base URL scheme is not http or https")
	case u.Hostname() == "":
		return nil, errors.New("the base URL has no host")
	case u.User != nil:
		return nil, errors.New("the base URL carries user information")
	case u.Fragment != "" || u.RawQuery != "" || u.ForceQuery:
		return nil, errors.New("the base URL has a query or a fragment")
	}
	return u, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// AuthConfig says how a credential is sent: the header and the scheme that
// precedes the value ("Bearer", "Basic", ...). A scheme of "" sends the value
// as it is, for API-key headers.
type AuthConfig struct {
	Header string  `yaml:"header"`
	Scheme *string `yaml:"scheme"`
}

// AdapterConfig is the content of the adapter configuration file of an OpenAPI
// endpoint (the file the manifest's adapter_config names). Unknown members are
// refused, so a setting a newer package relies on is never silently ignored.
//
//	server: clinic-api            # optional; when present it must be the endpoint's ID
//	auth:                         # only with a credential_ref
//	  header: Authorization       # default
//	  scheme: Bearer              # default; "" sends the credential as it is
//	identity:
//	  header: X-AlixAI-Identity   # default
//	idempotency:
//	  header: Idempotency-Key     # default
//	management:                   # operationIds of the adapter's own protocol
//	  describe: describeApplication
//	  health: getHealth
//	  inspect: inspectApplication
//	  provisioning: [createTenant, seedTenant, cleanupTenant]
//
// The management operations are conventions of this adapter, like the management
// tools of the MCP adapter: no capability maps them and a model never sees them
// (see the package comment for their shapes). They are part of the approved
// operation subset. provisioning names operations of the audit's disposable
// tenants: they are approved but can only be called through Adapter.Manage, never
// through the adapter contract.
type AdapterConfig struct {
	Server string      `yaml:"server"`
	Auth   *AuthConfig `yaml:"auth"`

	Identity struct {
		Header string `yaml:"header"`
	} `yaml:"identity"`

	Idempotency struct {
		Header string `yaml:"header"`
	} `yaml:"idempotency"`

	Management struct {
		Describe     string   `yaml:"describe"`
		Health       string   `yaml:"health"`
		Inspect      string   `yaml:"inspect"`
		Provisioning []string `yaml:"provisioning"`
	} `yaml:"management"`
}

// ParseAdapterConfig reads an adapter configuration for the endpoint endpointID.
// It is strict: unknown members, a second document, a server that is not the
// endpoint and malformed names are errors. Defaults are applied by New.
func ParseAdapterConfig(raw []byte, endpointID string) (AdapterConfig, error) {
	var c AdapterConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return AdapterConfig{}, cfgErr("the adapter configuration of endpoint %q does not parse: %s", endpointID, yamlReason(err))
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return AdapterConfig{}, cfgErr("the adapter configuration of endpoint %q holds more than one document", endpointID)
	}
	if c.Server != "" && c.Server != endpointID {
		return AdapterConfig{}, cfgErr("the adapter configuration names server %q but configures endpoint %q", truncate(c.Server, 40), endpointID)
	}
	if err := c.check(endpointID); err != nil {
		return AdapterConfig{}, err
	}
	return c, nil
}

// check validates the members that are present.
func (c *AdapterConfig) check(endpointID string) error {
	header := func(what, name string) error {
		if name == "" {
			return nil
		}
		if !headerTokenRE.MatchString(name) || reservedHeader(name) {
			return cfgErr("endpoint %q: %s header %q is malformed or reserved", endpointID, what, truncate(name, 40))
		}
		return nil
	}
	if a := c.Auth; a != nil {
		if a.Header != "" && (!headerTokenRE.MatchString(a.Header) || reservedHeader(a.Header) && !strings.EqualFold(a.Header, "authorization")) {
			return cfgErr("endpoint %q: auth header %q is malformed or reserved", endpointID, truncate(a.Header, 40))
		}
		if a.Scheme != nil && *a.Scheme != "" && !authSchemeRE.MatchString(*a.Scheme) {
			return cfgErr("endpoint %q: the auth scheme is malformed", endpointID)
		}
	}
	if err := header("identity", c.Identity.Header); err != nil {
		return err
	}
	if err := header("idempotency", c.Idempotency.Header); err != nil {
		return err
	}
	m := &c.Management
	seen := map[string]bool{}
	for what, id := range map[string]string{"describe": m.Describe, "health": m.Health, "inspect": m.Inspect} {
		if id == "" {
			continue
		}
		if !operationIDRE.MatchString(id) {
			return cfgErr("endpoint %q: management.%s names the malformed operationId %q", endpointID, what, truncate(id, 40))
		}
	}
	for _, id := range []string{m.Describe, m.Health, m.Inspect} {
		if id != "" && seen[id] {
			return cfgErr("endpoint %q: one operation serves two management roles", endpointID)
		}
		seen[id] = true
	}
	for _, id := range m.Provisioning {
		if !operationIDRE.MatchString(id) {
			return cfgErr("endpoint %q: management.provisioning names the malformed operationId %q", endpointID, truncate(id, 40))
		}
		if seen[id] {
			return cfgErr("endpoint %q: management.provisioning repeats an operation or reuses a management one", endpointID)
		}
		seen[id] = true
	}
	return nil
}

// resolve checks a configuration against what the endpoint declares and fills
// the defaults.
func (c *AdapterConfig) resolve(endpointID string, hasCredential bool) error {
	if err := c.check(endpointID); err != nil {
		return err
	}
	if c.Auth != nil && !hasCredential {
		return cfgErr("endpoint %q: the adapter configuration sets auth but the endpoint declares no credential_ref", endpointID)
	}
	if hasCredential {
		if c.Auth == nil {
			c.Auth = &AuthConfig{}
		}
		if c.Auth.Header == "" {
			c.Auth.Header = DefaultAuthHeader
		}
		if c.Auth.Scheme == nil {
			s := DefaultAuthScheme
			c.Auth.Scheme = &s
		}
	}
	if c.Identity.Header == "" {
		c.Identity.Header = DefaultIdentityHeader
	}
	if c.Idempotency.Header == "" {
		c.Idempotency.Header = DefaultIdempotencyHeader
	}
	names := map[string]string{}
	add := func(what, name string) error {
		k := strings.ToLower(name)
		if prev, dup := names[k]; dup {
			return cfgErr("endpoint %q: the %s and %s headers are both %q", endpointID, prev, what, name)
		}
		names[k] = what
		return nil
	}
	if err := add("identity", c.Identity.Header); err != nil {
		return err
	}
	if err := add("idempotency", c.Idempotency.Header); err != nil {
		return err
	}
	if c.Auth != nil {
		if err := add("auth", c.Auth.Header); err != nil {
			return err
		}
	}
	return nil
}

// authHeader is the credential header, "" when the endpoint has no credential.
func (c *AdapterConfig) authHeader() string {
	if c.Auth == nil {
		return ""
	}
	return c.Auth.Header
}

// protocolHeader reports whether a header is one the protocol owns, so that no
// field may fill it: the credential, identity and idempotency headers and the
// headers the HTTP stack manages.
func (c *AdapterConfig) protocolHeader(name string) bool {
	switch {
	case reservedHeader(name):
		return true
	case strings.EqualFold(name, c.Identity.Header), strings.EqualFold(name, c.Idempotency.Header):
		return true
	}
	return c.Auth != nil && strings.EqualFold(name, c.Auth.Header)
}

// yamlReason keeps a YAML error short and free of the document's own text.
func yamlReason(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 160)
}
