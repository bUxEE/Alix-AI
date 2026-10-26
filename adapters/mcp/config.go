package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Transport is the transport name the adapter reports in Describe.
const Transport = "mcp"

// The management tools a server may offer for the three adapter operations that
// are not capability operations. They are conventions of this adapter, not of
// the package: no capability maps them and a model never sees them. A server
// without them is still usable (see Describe, Health and Inspect).
const (
	DefaultDescribeTool = "describe"
	DefaultHealthTool   = "health"
	DefaultInspectTool  = "inspect"
)

// Defaults and bounds.
const (
	// DefaultSurfaceRecheck is how long a listing of a server's tools is trusted
	// before the next call lists them again and re-checks the pins.
	DefaultSurfaceRecheck = 5 * time.Second
	// DefaultConnectTimeout bounds opening a session (dialling and the MCP
	// handshake) when the caller's context has no earlier deadline.
	DefaultConnectTimeout = 15 * time.Second
	// DefaultAuthHeader and DefaultAuthScheme say how a credential is sent on
	// streamable HTTP / SSE (mirroring the OpenAPI adapter).
	DefaultAuthHeader = "Authorization"
	DefaultAuthScheme = "Bearer"

	maxEndpoints    = 16
	maxCapabilities = 1024
	maxListedTools  = 2048
	maxListPages    = 32
	// framingSlack is what the JSON-RPC envelope, the tool result's text
	// mirror and the headers may add to a structured result.
	framingSlack = 64 << 10
)

var (
	toolNameRE     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	endpointIDRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	argumentNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	sha256HexRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	authHeaderRE   = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$")
	authSchemeRE   = regexp.MustCompile(`^[A-Za-z0-9._~+/-]{0,32}$`)
)

// Command describes a local MCP server that is spoken to over stdio. It exists
// for hosts that wire an adapter in code (tests, a bridge an operator installs);
// an application package can never declare one, because a package cannot make
// AlixAI start a process (docs/integration/application-package.md). The command
// is started directly, never through a shell, with only the environment named
// here.
type Command struct {
	// Path is the absolute path of the executable.
	Path string
	// Args are its arguments.
	Args []string
	// Dir is the working directory ("" means the current one).
	Dir string
	// Env is the whole environment of the process, as KEY=value. Nothing is
	// inherited.
	Env []string
}

// Endpoint is one MCP server. Exactly one of URL and Command names where it is.
type Endpoint struct {
	// ID is the endpoint's ID in the manifest.
	ID string
	// URL is the streamable HTTP endpoint (http or https, no user information).
	// Every connection to it is made through Config.Dialer, so it must be on the
	// egress allowlist.
	URL string
	// Command is a stdio server; see Command.
	Command *Command
	// Allow is the adapter configuration's tools.allow: the only tools of this
	// server that the package approves. Every tool a capability maps must be on
	// it, and a tool that is on it and mapped by nobody is approved but not
	// callable by a capability (the integration audit's provisioning tools are an
	// example): only CallApprovedTool reaches it, and only for a host that wires it.
	// nil means the host imposes no list; the adapter configuration of a package
	// always sets one, even if it is empty.
	Allow []string
	// ContractSHA256 is the manifest's optional pin of the approved tool list:
	// the lower-case hex ContractSHA256 of the mapped tools of this endpoint. A
	// listing that hashes differently is drift.
	ContractSHA256 string
	// AuthHeader is the request header that carries the credential (default
	// "Authorization"). AuthScheme is the word before the value (default
	// "Bearer"); "" sends the credential alone. Both are ignored for stdio
	// endpoints: a local process has no HTTP headers.
	AuthHeader string
	AuthScheme string
	// Authorization returns the header value for the server (scheme plus
	// credential, or the bare credential when AuthScheme is ""), or "" for
	// none. It is called per request and its value is only ever sent to this
	// endpoint's own origin. Stdio endpoints ignore it.
	Authorization func(ctx context.Context) (string, error)
}

// Binding is one operation of a capability mapped to one tool of one endpoint.
type Binding struct {
	// Endpoint is the ID of the endpoint that owns the tool.
	Endpoint string
	// Tool is the tool's exact name. It is compared byte for byte.
	Tool string
	// Timeout bounds one call of the tool; 0 leaves the caller's deadline.
	Timeout time.Duration
	// Arguments renames capability fields to the tool's argument names, for
	// the fields that differ. Fields not listed keep their name.
	Arguments map[string]string
}

// Capability is what a package maps for one capability.
type Capability struct {
	// ID is the capability ID.
	ID string
	// Operations maps adapter operations to tools. A package maps read,
	// validate, prepare, commit and status; cancel is mapped only by a host that
	// wires the adapter in code, because a package has no cancel operation.
	Operations map[adapters.Operation]Binding
	// Resolve maps an entity type to the tool that resolves a mention of it. A
	// Read whose fields carry the reserved argument entity_type is routed here
	// and nowhere else.
	Resolve map[string]Binding
}

// Config configures New.
type Config struct {
	// ApplicationID names the application, for logs only; the scope check is
	// adapters.Wrap's.
	ApplicationID string
	// Endpoints are the servers. At least one.
	Endpoints []Endpoint
	// Capabilities is the package's mapping. Only what is mapped here can ever be
	// called.
	Capabilities []Capability
	// Dialer makes every HTTP connection. nil refuses everything (default deny).
	Dialer *egress.Dialer
	// MaxResponseBytes bounds one structured result; 0 means
	// adapters.DefaultMaxResponseBytes. A reply is also cut off at the transport
	// once it is larger than twice the bound plus 64 KiB.
	MaxResponseBytes int64
	// SurfaceRecheck is how often the tool list is checked against the pins;
	// 0 means DefaultSurfaceRecheck and a negative value checks before every
	// call.
	SurfaceRecheck time.Duration
	// ConnectTimeout bounds opening a session; 0 means DefaultConnectTimeout.
	ConnectTimeout time.Duration
	// Clock is the time source of the recheck interval and of Drift.At; nil
	// means the real clock.
	Clock clock.Clock
	// Logger receives what the adapter logs (drift, once per tool; nothing else).
	// nil discards. It never receives a URL, a credential, a tool description or
	// text the server sent.
	Logger *slog.Logger
	// OnDrift is called once for every drift found, from the goroutine of the
	// call that found it. It raises the re-audit flag; it must not block.
	OnDrift func(Drift)
	// Pins are tool hashes taken at audit time, keyed "endpoint/tool". A tool
	// without a pin is pinned when it is first listed.
	Pins map[string]string
	// DescribeTool, HealthTool and InspectTool override the management tool
	// names.
	DescribeTool, HealthTool, InspectTool string
	// ClientName and ClientVersion identify the adapter to the server.
	ClientName, ClientVersion string

	// connect replaces the transport factory in tests of this package.
	connect func(ctx context.Context, ep *endpointConn) (sdkTransport, error)
}

// ConfigError is the cause of the validation_failed error New returns for a
// configuration it refuses.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return "mcp adapter config: " + e.Msg }

func cfgErr(format string, args ...any) error {
	return contracts.Wrap(contracts.ErrValidationFailed, &ConfigError{Msg: fmt.Sprintf(format, args...)}, "the MCP adapter configuration is invalid")
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
	byID := map[string]*Endpoint{}
	for i := range c.Endpoints {
		ep := &c.Endpoints[i]
		if !endpointIDRE.MatchString(ep.ID) {
			return cfgErr("endpoint %d has an empty or malformed ID", i)
		}
		if byID[ep.ID] != nil {
			return cfgErr("endpoint %q is declared twice", ep.ID)
		}
		byID[ep.ID] = ep
		if (ep.URL == "") == (ep.Command == nil) {
			return cfgErr("endpoint %q must name exactly one of a URL and a command", ep.ID)
		}
		if ep.URL != "" {
			if _, err := parseEndpointURL(ep.URL); err != nil {
				return cfgErr("endpoint %q: %v", ep.ID, err)
			}
		}
		if ep.Command != nil && !filepath.IsAbs(ep.Command.Path) {
			return cfgErr("endpoint %q: the command path must be absolute", ep.ID)
		}
		if ep.ContractSHA256 != "" && !sha256HexRE.MatchString(ep.ContractSHA256) {
			return cfgErr("endpoint %q: contract_sha256 is not a lower-case hex SHA-256", ep.ID)
		}
		if ep.AuthHeader == "" {
			ep.AuthHeader = DefaultAuthHeader
		}
		if !authHeaderRE.MatchString(ep.AuthHeader) {
			return cfgErr("endpoint %q: auth header is malformed", ep.ID)
		}
		if ep.AuthScheme != "" && !authSchemeRE.MatchString(ep.AuthScheme) {
			return cfgErr("endpoint %q: auth scheme is malformed", ep.ID)
		}
		for _, t := range ep.Allow {
			if !toolNameRE.MatchString(t) {
				return cfgErr("endpoint %q: %q is not a valid tool name", ep.ID, truncate(t, 40))
			}
		}
	}
	for k, v := range c.Pins {
		epID, tool, ok := strings.Cut(k, "/")
		if byID[epID] == nil || !ok || !toolNameRE.MatchString(tool) || !sha256HexRE.MatchString(v) {
			return cfgErr("pin %q is malformed or names an unknown endpoint", truncate(k, 80))
		}
	}
	seen := map[string]bool{}
	checkBinding := func(where string, b Binding) error {
		ep := byID[b.Endpoint]
		switch {
		case ep == nil:
			return cfgErr("%s maps a tool of the unknown endpoint %q", where, truncate(b.Endpoint, 40))
		case !toolNameRE.MatchString(b.Tool):
			return cfgErr("%s maps the malformed tool name %q", where, truncate(b.Tool, 40))
		case b.Timeout < 0:
			return cfgErr("%s has a negative timeout", where)
		case ep.Allow != nil && !slices.Contains(ep.Allow, b.Tool):
			return cfgErr("%s maps the tool %q, which the adapter configuration of endpoint %q does not allow", where, b.Tool, ep.ID)
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
	for _, cp := range c.Capabilities {
		if cp.ID == "" || strings.ContainsAny(cp.ID, "* \t\r\n") || seen[cp.ID] {
			return cfgErr("a capability ID is empty, malformed or declared twice")
		}
		seen[cp.ID] = true
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
	for _, name := range []*string{&c.DescribeTool, &c.HealthTool, &c.InspectTool} {
		if *name != "" && !toolNameRE.MatchString(*name) {
			return cfgErr("a management tool name is malformed")
		}
	}
	if c.DescribeTool == "" {
		c.DescribeTool = DefaultDescribeTool
	}
	if c.HealthTool == "" {
		c.HealthTool = DefaultHealthTool
	}
	if c.InspectTool == "" {
		c.InspectTool = DefaultInspectTool
	}
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = adapters.DefaultMaxResponseBytes
	}
	if c.SurfaceRecheck == 0 {
		c.SurfaceRecheck = DefaultSurfaceRecheck
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	if c.Clock == nil {
		c.Clock = clock.Real{}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.ClientName == "" {
		c.ClientName = "alixai"
	}
	if c.ClientVersion == "" {
		c.ClientVersion = "0"
	}
	return nil
}

// parseEndpointURL checks a streamable HTTP endpoint.
func parseEndpointURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, errors.New("the URL does not parse")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("the URL scheme is not http or https")
	case u.Hostname() == "":
		return nil, errors.New("the URL has no host")
	case u.User != nil:
		return nil, errors.New("the URL carries user information")
	case u.Fragment != "":
		return nil, errors.New("the URL has a fragment")
	}
	return u, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// AuthConfig says how a credential_ref is sent on streamable HTTP / SSE: the
// header and the scheme that precedes the value ("Bearer", "Basic", …). A
// scheme of "" sends the value as it is. Stdio endpoints ignore auth.
type AuthConfig struct {
	Header string  `yaml:"header"`
	Scheme *string `yaml:"scheme"`
}

// AdapterConfig is the content of the adapter configuration file of an MCP
// endpoint (the file the manifest's adapter_config names). Version 1 of its
// format is deliberately small: which server it configures and which of the
// server's tools the package approves. Unknown members are refused, so a
// setting a newer package relies on is never silently ignored.
//
//	server: clinic-mcp        # optional; when present it must be the endpoint's ID
//	auth:                     # only with a credential_ref; defaults Authorization / Bearer
//	  header: Authorization
//	  scheme: Bearer          # "" sends the credential as it is
//	tools:
//	  allow:                  # the approved tools; everything else on the server is
//	    - availability_list   # invisible to the model and cannot be called
//	    - ...
//
// The tools that capabilities map must all be listed. Tools that are listed and
// mapped by nobody (the provisioning tools of the audit's disposable tenants)
// are approved for the package but are not reachable through the adapter
// contract. The management tools describe, health and inspect are the adapter's
// own protocol and need no entry. auth applies only to URL (HTTP/SSE) endpoints;
// a stdio Command ignores it.
type AdapterConfig struct {
	Server string      `yaml:"server"`
	Auth   *AuthConfig `yaml:"auth"`
	Tools  struct {
		Allow []string `yaml:"allow"`
	} `yaml:"tools"`
}

// ParseAdapterConfig reads an adapter configuration for the endpoint endpointID.
// It is strict: unknown members, a second document, a server that is not the
// endpoint, a malformed or repeated tool name are errors. The returned list is
// never nil, so an empty tools.allow approves nothing.
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
	if c.Auth != nil {
		if c.Auth.Header == "" {
			c.Auth.Header = DefaultAuthHeader
		}
		if !authHeaderRE.MatchString(c.Auth.Header) {
			return AdapterConfig{}, cfgErr("auth.header is malformed")
		}
		if c.Auth.Scheme == nil {
			s := DefaultAuthScheme
			c.Auth.Scheme = &s
		} else if *c.Auth.Scheme != "" && !authSchemeRE.MatchString(*c.Auth.Scheme) {
			return AdapterConfig{}, cfgErr("auth.scheme is malformed")
		}
	}
	seen := map[string]bool{}
	for _, t := range c.Tools.Allow {
		if !toolNameRE.MatchString(t) {
			return AdapterConfig{}, cfgErr("tools.allow holds the malformed tool name %q", truncate(t, 40))
		}
		if seen[t] {
			return AdapterConfig{}, cfgErr("tools.allow lists %q twice", t)
		}
		seen[t] = true
	}
	if c.Tools.Allow == nil {
		c.Tools.Allow = []string{}
	}
	sort.Strings(c.Tools.Allow)
	return c, nil
}

// yamlReason keeps a YAML error short and free of the document's own text.
func yamlReason(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 160)
}
