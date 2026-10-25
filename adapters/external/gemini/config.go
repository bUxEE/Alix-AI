package gemini

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ProviderName is what Name reports and the provider id used for the egress
// allowlist, the credential binding and the OAuth engine.
const ProviderName = "gemini"

// DefaultBaseURL is the API origin. The route table (client.go) holds the paths.
const DefaultBaseURL = "https://generativelanguage.googleapis.com"

// apiVersionPath is the API version segment every route carries.
const apiVersionPath = "/v1beta"

// Tool modes of the function calling configuration (toolConfig.
// functionCallingConfig.mode). Config.ToolMode picks the one used when the
// caller leaves the choice to the model.
const (
	// ToolModeValidated (the default) lets the model answer in text or propose a
	// call and constrains the arguments of a call to the declared schema.
	ToolModeValidated = "VALIDATED"
	// ToolModeAuto lets the model decide with no schema guarantee for the
	// arguments of a call: the caller's own validation is then the only check.
	ToolModeAuto = "AUTO"
)

// Config configures one Gemini provider profile. There is deliberately no field
// that holds a key or a token: CredentialRef is an opaque reference that the
// credential service (or the OAuth engine) resolves on every call.
type Config struct {
	// BaseURL is the API origin: https, or plain http to loopback for tests.
	// The empty value means DefaultBaseURL. The egress allowlist must list the
	// same origin (egress.DestinationFromURL derives the entry).
	BaseURL string
	// Auth says how CredentialRef authenticates: AuthAPIKey (the default) or
	// AuthOAuth. The kind is the operator's choice per connection; the adapter
	// supports both (see AuthSupport).
	Auth AuthKind
	// CredentialRef is the credential reference ("cred_..."), never a secret.
	CredentialRef string
	// UserProject is the Google Cloud project id sent as x-goog-user-project
	// with an OAuth bearer token (quota and billing are charged to it). It is
	// an identifier, not a secret. Empty omits the header; an API key carries
	// its project and never sends it.
	UserProject string
	// OAuthScopes are the scopes the token must be permitted. Empty means
	// InferenceScopes.
	OAuthScopes []string
	// Model is the hosted model id the controller role uses. Required: there
	// is no default model, so a model change is always an operator decision.
	Model string
	// Profiles maps a request's Profile hint to another model id. A hint that
	// is not listed is refused.
	Profiles map[string]string

	// MaxContextToks and MaxOutputToks override the built-in limits of the
	// configured model; zero takes the built-in value (or a conservative
	// default for an unknown model).
	MaxContextToks int
	MaxOutputToks  int
	// DefaultMaxOutputToks is sent when a request names no cap, so that every
	// call is bounded. It counts the thinking tokens of a thinking model too.
	DefaultMaxOutputToks int
	// Prices is the price table in micro-units (USD for the built-in entries).
	// Nil takes the built-in entry of the model; an unknown model without a
	// table has no cost and cannot be budgeted.
	Prices *providers.PriceTable
	// ToolMode is the function calling mode used when the caller leaves the
	// choice to the model: ToolModeValidated (default) or ToolModeAuto. A
	// required proposal is always sent as ANY, a cancellation as NONE.
	ToolMode string

	// RequestTimeout bounds a whole non-streaming call including retries.
	RequestTimeout time.Duration
	// StreamTimeout bounds a whole stream.
	StreamTimeout time.Duration
	// MaxResponseBytes bounds a non-streaming response body.
	MaxResponseBytes int64
	// Retry bounds retries of a request the provider refused before
	// processing it. The zero value retries twice.
	Retry external.RetryPolicy
}

var (
	// A model id never carries a path separator, a colon or the "models/"
	// prefix: it is placed in a URL path next to ":generateContent".
	modelIDRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	profileKey  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	projectIDRE = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	scopeRE     = regexp.MustCompile(`^https://www\.googleapis\.com/auth/[a-z0-9._-]{1,64}$`)
)

// String describes the configuration without anything sensitive.
func (c Config) String() string {
	return fmt.Sprintf("gemini.Config{BaseURL:%s Model:%s Auth:%s}", c.baseURL(), c.Model, c.auth())
}

func (c Config) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

func (c Config) auth() AuthKind {
	if c.Auth == "" {
		return AuthAPIKey
	}
	return c.Auth
}

func (c Config) toolMode() string {
	if c.ToolMode == "" {
		return ToolModeValidated
	}
	return c.ToolMode
}

// Validate checks the configuration (fail closed).
func (c Config) Validate() error {
	if c.CredentialRef == "" {
		return errors.New("a credential reference is required")
	}
	if c.auth() != AuthAPIKey && c.auth() != AuthOAuth {
		return errors.New("unknown authentication kind")
	}
	if !modelIDRE.MatchString(c.Model) {
		return errors.New("a model id is required")
	}
	for k, v := range c.Profiles {
		if !profileKey.MatchString(k) || !modelIDRE.MatchString(v) {
			return errors.New("a profile maps a malformed name or model id")
		}
	}
	if c.UserProject != "" && (c.auth() != AuthOAuth || !projectIDRE.MatchString(c.UserProject)) {
		return errors.New("the user project is a Cloud project id and applies to OAuth only")
	}
	if len(c.OAuthScopes) > 0 && c.auth() != AuthOAuth {
		return errors.New("OAuth scopes apply to OAuth only")
	}
	for _, s := range c.OAuthScopes {
		if !scopeRE.MatchString(s) {
			return errors.New("a scope is malformed")
		}
	}
	if m := c.toolMode(); m != ToolModeValidated && m != ToolModeAuto {
		return errors.New("unknown tool mode")
	}
	if c.MaxContextToks < 0 || c.MaxOutputToks < 0 || c.DefaultMaxOutputToks < 0 || c.MaxResponseBytes < 0 ||
		c.RequestTimeout < 0 || c.StreamTimeout < 0 {
		return errors.New("a limit is negative")
	}
	if c.Prices != nil {
		if err := c.Prices.Validate(); err != nil {
			return errors.New("the price table is invalid")
		}
	}
	return nil
}

func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	c.Auth = c.auth()
	if c.Auth == AuthOAuth && len(c.OAuthScopes) == 0 {
		c.OAuthScopes = append([]string(nil), InferenceScopes...)
	}
	c.ToolMode = c.toolMode()
	if c.DefaultMaxOutputToks == 0 {
		c.DefaultMaxOutputToks = 4096
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 2 * time.Minute
	}
	if c.StreamTimeout == 0 {
		c.StreamTimeout = 10 * time.Minute
	}
	if c.MaxResponseBytes == 0 {
		c.MaxResponseBytes = 8 << 20
	}
	if c.Retry == (external.RetryPolicy{}) {
		c.Retry = external.RetryPolicy{MaxRetries: 2, BaseDelay: 500 * time.Millisecond, MaxDelay: 8 * time.Second, MaxRetryAfter: 20 * time.Second}
	}
	c.Retry = c.Retry.Normalised()
	return c
}

func configError(err error) error {
	return contracts.Wrap(contracts.ErrValidationFailed, err, "gemini: invalid configuration")
}

// modelInfo is the built-in knowledge about a hosted model: limits and the
// list price in USD micro-units per million tokens. It is a DEFAULT: the
// operator's profile and Config override it, and the numbers are as summarised
// from the vendor's pages on the research date (see doc.go), not measured. A
// zero limit means "not read": the conservative default applies.
type modelInfo struct {
	context   int
	output    int
	inMicros  int64
	outMicros int64
}

// knownModels. Thinking tokens bill as output, so the output price is the one
// that matters. gemini-3.8-flash is priced at its post-promotion rate (the
// promotional rate ends 2026-12-31) so that an estimate stays an upper bound.
// gemini-2.5-pro is absent on purpose: its price depends on the prompt size
// and the page summary did not state the upper tier.
var knownModels = map[string]modelInfo{
	"gemini-3.5-flash-lite": {1_048_576, 65_536, 300_000, 2_500_000},
	"gemini-3.5-flash":      {0, 0, 1_500_000, 9_000_000},
	"gemini-3.8-flash":      {0, 0, 1_500_000, 7_500_000},
	"gemini-3.8-flash-lite": {0, 0, 300_000, 2_500_000},
	"gemini-2.5-flash-lite": {1_048_576, 65_536, 100_000, 400_000},
	"gemini-2.5-flash":      {1_048_576, 65_536, 300_000, 2_500_000},
}

// Conservative limits for a model the table does not know.
const (
	unknownContextToks = 200_000
	unknownOutputToks  = 8_192
)

func usdTable(in, out int64) *providers.PriceTable {
	return &providers.PriceTable{
		Currency: "USD",
		ToksIn:   &providers.Price{PerUnits: 1_000_000, MicroUnits: in},
		ToksOut:  &providers.Price{PerUnits: 1_000_000, MicroUnits: out},
	}
}

// modelProfile is the resolved view of one model for this configuration.
type modelProfile struct {
	id      string
	context int
	output  int
	prices  *providers.PriceTable
}

func (c Config) profileFor(id string) modelProfile {
	p := modelProfile{id: id, context: unknownContextToks, output: unknownOutputToks}
	if m, ok := knownModels[id]; ok {
		if m.context > 0 {
			p.context, p.output = m.context, m.output
		}
		p.prices = usdTable(m.inMicros, m.outMicros)
	}
	if id != c.Model { // the overrides below describe the configured model only
		return p
	}
	if c.MaxContextToks > 0 {
		p.context = c.MaxContextToks
	}
	if c.MaxOutputToks > 0 {
		p.output = c.MaxOutputToks
	}
	if p.output > p.context {
		p.output = p.context
	}
	if c.Prices != nil {
		cp := *c.Prices
		p.prices = &cp
	}
	return p
}
