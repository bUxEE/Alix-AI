package anthropic

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
// allowlist and the credential binding.
const ProviderName = "anthropic"

// DefaultBaseURL is the API origin. The route table (client.go) holds the paths.
const DefaultBaseURL = "https://api.anthropic.com"

// apiVersion is the value of the anthropic-version header. The API versions
// its wire format with this header; "2023-06-01" is the only stable value.
const apiVersion = "2023-06-01"

// Config configures one Anthropic provider profile. There is deliberately no
// field that holds a key: CredentialRef is an opaque reference that the
// credential service resolves on every call.
type Config struct {
	// BaseURL is the API origin: https, or plain http to loopback for tests.
	// The empty value means DefaultBaseURL. The egress allowlist must list the
	// same origin (egress.DestinationFromURL derives the entry).
	BaseURL string
	// CredentialRef is the credential reference ("cred_..."), never a key.
	CredentialRef string
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
	// DefaultMaxOutputToks is sent when a request names no cap (the API
	// requires one). It counts the thinking tokens of a thinking model too.
	DefaultMaxOutputToks int
	// Prices is the price table in micro-units (USD for the built-in entries).
	// Nil takes the built-in entry of the model; an unknown model without a
	// table has no cost and cannot be budgeted.
	Prices *providers.PriceTable
	// Effort is sent as output_config.effort when set ("low", "medium",
	// "high", "xhigh", "max"). Empty omits it: the levels differ per model, so
	// it is an operator's explicit choice. A model without effort support
	// refuses the request.
	Effort string
	// Sampling overrides whether the configured model accepts a temperature
	// (the newest models reject non-default sampling parameters). Nil takes
	// the built-in knowledge; an unknown model refuses a temperature.
	Sampling *bool
	// ForcedToolChoice overrides whether the configured model accepts
	// tool_choice "any" (the newest models answer 400). Nil takes the built-in
	// knowledge. Where it is false a required proposal is sent as "auto" and
	// enforced after the answer arrives (a missing proposal is an error).
	ForcedToolChoice *bool

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
	modelIDRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	effortRE   = regexp.MustCompile(`^(low|medium|high|xhigh|max)$`)
	profileKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// String describes the configuration without anything sensitive.
func (c Config) String() string {
	return fmt.Sprintf("anthropic.Config{BaseURL:%s Model:%s}", c.baseURL(), c.Model)
}

func (c Config) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

// Validate checks the configuration (fail closed).
func (c Config) Validate() error {
	if c.CredentialRef == "" {
		return errors.New("a credential reference is required")
	}
	if !modelIDRE.MatchString(c.Model) {
		return errors.New("a model id is required")
	}
	for k, v := range c.Profiles {
		if !profileKey.MatchString(k) || !modelIDRE.MatchString(v) {
			return errors.New("a profile maps a malformed name or model id")
		}
	}
	if c.Effort != "" && !effortRE.MatchString(c.Effort) {
		return errors.New("unknown effort level")
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
	return contracts.Wrap(contracts.ErrValidationFailed, err, "anthropic: invalid configuration")
}

// modelInfo is the built-in knowledge about a hosted model: limits, which
// request features it accepts, and the list price in USD micro-units per
// million tokens. It is a DEFAULT: the operator's profile and Config override
// it, and the numbers are as summarised from the vendor's pages on the
// research date (see doc.go), not measured.
type modelInfo struct {
	context int
	output  int
	// forced: tool_choice "any" / "tool" is accepted.
	forced bool
	// sampling: a temperature is accepted.
	sampling bool
	// effort: output_config.effort is accepted.
	effort    bool
	inMicros  int64
	outMicros int64
}

// knownModels. Haiku 4.5 is the cheapest priced tier and the only one in the
// table that still accepts a forced tool choice and a temperature. Its output
// cap is deliberately conservative (the page summary did not state it); the
// operator can raise it with Config.MaxOutputToks. Models released after the
// research date have no entry: they get conservative limits and need an
// operator price table to be budgeted (Claude Haiku 5.5 is named in the rate
// limit tables but no price was read).
var knownModels = map[string]modelInfo{
	"claude-haiku-4-5":  {200_000, 32_000, true, true, false, 1_000_000, 5_000_000},
	"claude-sonnet-5-5": {1_000_000, 128_000, false, false, true, 2_000_000, 10_000_000},
	"claude-sonnet-5":   {1_000_000, 128_000, true, false, true, 2_000_000, 10_000_000},
	"claude-opus-5-5":   {1_000_000, 128_000, false, false, true, 4_000_000, 20_000_000},
	"claude-opus-5":     {1_000_000, 128_000, true, false, true, 5_000_000, 25_000_000},
}

// Conservative limits for a model the table does not know.
const (
	unknownContextToks = 200_000
	unknownOutputToks  = 8_192
)

var snapshotSuffixRE = regexp.MustCompile(`-\d{8}$`)

func lookupModel(id string) (modelInfo, bool) {
	if m, ok := knownModels[id]; ok {
		return m, true
	}
	m, ok := knownModels[snapshotSuffixRE.ReplaceAllString(id, "")]
	return m, ok
}

func usdTable(in, out int64) *providers.PriceTable {
	return &providers.PriceTable{
		Currency: "USD",
		ToksIn:   &providers.Price{PerUnits: 1_000_000, MicroUnits: in},
		ToksOut:  &providers.Price{PerUnits: 1_000_000, MicroUnits: out},
	}
}

// modelProfile is the resolved view of one model for this configuration.
type modelProfile struct {
	id       string
	context  int
	output   int
	forced   bool
	sampling bool
	effort   bool
	prices   *providers.PriceTable
}

func (c Config) profileFor(id string) modelProfile {
	p := modelProfile{id: id, context: unknownContextToks, output: unknownOutputToks}
	if m, ok := lookupModel(id); ok {
		p.context, p.output, p.forced, p.sampling, p.effort = m.context, m.output, m.forced, m.sampling, m.effort
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
	if c.Sampling != nil {
		p.sampling = *c.Sampling
	}
	if c.ForcedToolChoice != nil {
		p.forced = *c.ForcedToolChoice
	}
	if c.Prices != nil {
		cp := *c.Prices
		p.prices = &cp
	}
	return p
}
