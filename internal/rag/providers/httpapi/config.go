package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Defaults of Config.
const (
	// DefaultPageSize is the "limit" sent on a list request.
	DefaultPageSize = 200
	// DefaultMaxPageBytes bounds one list page. A page of the maximum number
	// of entries with long keys fits; anything larger is hostile or broken.
	DefaultMaxPageBytes = 4 << 20
	// DefaultMaxPages bounds the pages of one listing, so a server that never
	// ends its pagination cannot hold a scan forever.
	DefaultMaxPages = 100_000
	// DefaultRequestTimeout bounds one list or metadata request, body
	// included.
	DefaultRequestTimeout = 30 * time.Second
	// DefaultItemTimeout bounds the download of one item.
	DefaultItemTimeout = 5 * time.Minute
	// DefaultRacyWindow is how close to a record's observation time its
	// modification time may be before the record stops being trusted for the
	// size-and-time shortcut (see fs.DefaultRacyWindow).
	DefaultRacyWindow = 2 * time.Second
	// DefaultAuthHeader and DefaultAuthScheme say how a credential is sent.
	DefaultAuthHeader = "Authorization"
	DefaultAuthScheme = "Bearer"

	// maxPageEntries bounds the entries of one page whatever the page size.
	maxPageEntries = 50_000
	// maxCursorBytes bounds a pagination cursor.
	maxCursorBytes = 1024
	// maxVersionLen is the opaque-version limit of the application contract.
	maxVersionLen = 256
	// maxKeyChars bounds an item key (in characters, like the schema's maxLength).
	maxKeyChars = 4096
	// listBytesPerEntry is the average number of bytes of keys and versions a
	// listing may retain per entry it is allowed to hold. A real key is a few
	// dozen bytes; the budget exists so entries with huge keys cannot add up
	// to more memory than the declared size of the source justifies.
	listBytesPerEntry = 1024
	// maxPageSize caps the page size a configuration may ask for.
	maxPageSize = 10_000
)

var (
	paramNameRE  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
	headerNameRE = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]{1,64}$")
	authSchemeRE = regexp.MustCompile(`^[A-Za-z0-9._~+/-]{1,32}$`)
)

// Params are the names of the query parameters the provider itself sends. The
// zero value of a field means its default. The names are reserved: a declared
// fixed parameter may not use any of them, so a declaration cannot override the
// scope.
type Params struct {
	// Cursor and Limit are the pagination parameters of a list request.
	Cursor string
	Limit  string
	// Application, Tenant and Workspace carry the verified scope.
	Application string
	Tenant      string
	Workspace   string
	// Principal is the name of the principal_id parameter when a source opts in
	// via scope_params.include_principal_id. Empty means the provider default
	// (DefaultParamPrincipal) is used for that source; the parameter is only
	// sent when the declaration opts in.
	Principal string
}

// The default parameter names.
const (
	DefaultParamCursor      = "cursor"
	DefaultParamLimit       = "limit"
	DefaultParamApplication = "application_id"
	DefaultParamTenant      = "tenant_id"
	DefaultParamWorkspace   = "workspace_id"
	DefaultParamPrincipal   = "principal_id"
)

func (p Params) withDefaults() Params {
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&p.Cursor, DefaultParamCursor)
	def(&p.Limit, DefaultParamLimit)
	def(&p.Application, DefaultParamApplication)
	def(&p.Tenant, DefaultParamTenant)
	def(&p.Workspace, DefaultParamWorkspace)
	return p
}

func (p Params) all() []string {
	out := []string{p.Cursor, p.Limit, p.Application, p.Tenant, p.Workspace}
	if p.Principal != "" {
		out = append(out, p.Principal)
	}
	return out
}

func (p Params) validate() error {
	seen := map[string]bool{}
	for _, n := range p.all() {
		if !paramNameRE.MatchString(n) {
			return fmt.Errorf("parameter name %q is not a plain name", n)
		}
		k := strings.ToLower(n)
		if seen[k] {
			return fmt.Errorf("parameter name %q is used twice", n)
		}
		seen[k] = true
	}
	return nil
}

// DialerSource gives the egress Dialer of an application. *egress.Registry
// implements it: every application has its own destination allowlist, and one
// without an allowlist reaches nothing.
type DialerSource interface {
	Dialer(applicationID string) *egress.Dialer
}

// EndpointResolver maps the ID of a manifest endpoint (an api root that names
// an endpoint instead of a url) to its absolute base URL. It reports false for
// an endpoint it does not know. Without a resolver an endpoint root is refused.
type EndpointResolver func(applicationID, endpointID string) (baseURL string, ok bool)

// Config configures a Provider.
type Config struct {
	// Clock supplies the time stamped on tombstones, the scan and the probe.
	// Nil means clock.Real.
	Clock clock.Clock
	// Egress supplies the egress Dialer of the source's application. Nil
	// refuses every request (default deny).
	Egress DialerSource
	// Secrets resolves credential references. Required for a source that
	// declares a credential_ref; without it such a source sends nothing.
	Secrets SecretProvider
	// Endpoints resolves a root that names a manifest endpoint. Optional.
	Endpoints EndpointResolver
	// Logger receives operational records (no URLs, no credentials). Nil
	// discards them.
	Logger *slog.Logger

	// Params overrides the parameter names; the zero value uses the defaults.
	Params Params
	// AuthHeader is the request header that carries the credential
	// (default "Authorization") and AuthScheme the word before it (default
	// "Bearer"). AuthBare sends the secret alone as the header value, for APIs
	// that expect a bare key in a header such as X-Api-Key.
	AuthHeader string
	AuthScheme string
	AuthBare   bool

	// PageSize is the "limit" of a list request (DefaultPageSize).
	PageSize int
	// MaxPageBytes bounds one list page (DefaultMaxPageBytes).
	MaxPageBytes int64
	// MaxPages bounds the pages of one listing (DefaultMaxPages).
	MaxPages int
	// RequestTimeout and ItemTimeout bound a list or metadata request and an
	// item download (DefaultRequestTimeout, DefaultItemTimeout).
	RequestTimeout time.Duration
	ItemTimeout    time.Duration
	// RacyWindow overrides DefaultRacyWindow.
	RacyWindow time.Duration
	// UserAgent is sent on every request ("alixai-rag-httpapi" by default).
	UserAgent string
}

// ConfigError is the cause of the validation_failed error New returns.
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string { return "httpapi provider config: " + e.Msg }

func cfgErr(format string, args ...any) error {
	return contracts.Wrap(contracts.ErrValidationFailed, &ConfigError{Msg: fmt.Sprintf(format, args...)}, "the HTTP API source provider configuration is invalid")
}

// normalise fills the defaults and validates the configuration.
func (c Config) normalise() (Config, error) {
	if c.Clock == nil {
		c.Clock = clock.Real{}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	c.Params = c.Params.withDefaults()
	if err := c.Params.validate(); err != nil {
		return c, cfgErr("%v", err)
	}
	if c.AuthHeader == "" {
		c.AuthHeader = DefaultAuthHeader
	}
	if !headerNameRE.MatchString(c.AuthHeader) {
		return c, cfgErr("the credential header name is not a header token")
	}
	switch strings.ToLower(c.AuthHeader) {
	case "host", "content-length", "content-type", "accept", "accept-encoding", "transfer-encoding", "connection", "upgrade",
		"te", "trailer", "expect", "cookie", "set-cookie", "user-agent", "proxy-authorization", "range", "if-match", "if-none-match":
		return c, cfgErr("the credential header may not be a header the HTTP stack or the protocol owns")
	}
	if c.AuthScheme == "" {
		c.AuthScheme = DefaultAuthScheme
	}
	if !c.AuthBare && !authSchemeRE.MatchString(c.AuthScheme) {
		return c, cfgErr("the credential scheme is not a token")
	}
	if c.PageSize == 0 {
		c.PageSize = DefaultPageSize
	}
	if c.PageSize < 1 || c.PageSize > maxPageSize {
		return c, cfgErr("the page size must be between 1 and %d", maxPageSize)
	}
	if c.MaxPageBytes == 0 {
		c.MaxPageBytes = DefaultMaxPageBytes
	}
	if c.MaxPageBytes < 1 {
		return c, cfgErr("the page byte limit must be positive")
	}
	if c.MaxPages == 0 {
		c.MaxPages = DefaultMaxPages
	}
	if c.MaxPages < 1 {
		return c, cfgErr("the page limit must be positive")
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.ItemTimeout == 0 {
		c.ItemTimeout = DefaultItemTimeout
	}
	if c.RequestTimeout < 0 || c.ItemTimeout < 0 {
		return c, cfgErr("timeouts must be positive")
	}
	if c.RacyWindow <= 0 {
		c.RacyWindow = DefaultRacyWindow
	}
	if c.UserAgent == "" {
		c.UserAgent = "alixai-rag-httpapi"
	}
	return c, nil
}

// Secret is a credential value. Every way of printing it, logging it or
// encoding it prints a fixed placeholder, so a Secret that ends up in an error,
// a log record or a JSON document by mistake does not leak. The one way to the
// value is Reveal, which only the code that puts it in a header calls.
type Secret string

const secretPlaceholder = "[redacted]"

// Reveal returns the credential value.
func (s Secret) Reveal() string { return string(s) }

// String implements fmt.Stringer. It also covers %s, %q and %v.
func (Secret) String() string { return secretPlaceholder }

// GoString implements fmt.GoStringer (%#v).
func (Secret) GoString() string { return "httpapi.Secret(" + secretPlaceholder + ")" }

// Format implements fmt.Formatter, so no verb (%x, %d, %+v ...) prints the value.
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, secretPlaceholder) }

// LogValue implements slog.LogValuer.
func (Secret) LogValue() slog.Value { return slog.StringValue(secretPlaceholder) }

// MarshalJSON implements json.Marshaler.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + secretPlaceholder + `"`), nil }

// MarshalText implements encoding.TextMarshaler.
func (Secret) MarshalText() ([]byte, error) { return []byte(secretPlaceholder), nil }

// SecretProvider resolves the opaque credential reference a source declares
// (credential_ref) to the credential. The provider never sees anything but the
// reference, and the reference never leaves the process: it is not sent to the
// application, not logged and not put in an error.
//
// Implementations (the environment, a file, the OS secret store) come with the
// credential service; a composition root supplies one. The provider asks for the
// credential once per request, so a rotated credential is used by the next
// request and nothing here caches it. Implementations must be safe for
// concurrent use, must honour ctx and must not put the credential in the errors
// they return (the provider does not pass those on either).
type SecretProvider interface {
	Secret(ctx context.Context, ref string) (Secret, error)
}

// ErrNoSecret is what a provider returns for a reference it does not know.
var ErrNoSecret = errors.New("httpapi: no such credential reference")

// StaticSecrets is a SecretProvider over a fixed map of reference to credential.
// It is for tests and for hosts that wire the provider in code.
type StaticSecrets map[string]string

// Secret implements SecretProvider.
func (s StaticSecrets) Secret(_ context.Context, ref string) (Secret, error) {
	v, ok := s[ref]
	if !ok {
		return "", ErrNoSecret
	}
	return Secret(v), nil
}
