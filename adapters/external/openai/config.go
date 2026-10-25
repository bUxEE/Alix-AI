package openai

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
const ProviderName = "openai"

// DefaultBaseURL is the API origin. The route table (endpoints.go) holds the
// paths.
const DefaultBaseURL = "https://api.openai.com"

// Config configures one OpenAI provider profile. There is deliberately no field
// that holds a key: CredentialRef is an opaque reference that the credential
// service resolves on every call.
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
	// Profiles maps a request's Profile hint to another model id. A hint
	// that is not listed is refused.
	Profiles map[string]string
	// EmbeddingModel enables Embed (empty: embeddings are unsupported).
	EmbeddingModel string

	// MaxContextToks and MaxOutputToks override the built-in limits of the
	// model; zero takes the built-in value (or a conservative default for an
	// unknown model).
	MaxContextToks int
	MaxOutputToks  int
	// DefaultMaxOutputToks is sent when a request names no cap. It counts the
	// reasoning tokens of a reasoning model too.
	DefaultMaxOutputToks int
	// Prices is the price table in micro-units (USD for the built-in
	// entries). Nil takes the built-in entry of the model; an unknown model
	// without a table has no cost and cannot be budgeted.
	Prices *providers.PriceTable
	// Reasoning overrides whether the model is a reasoning model (it then
	// accepts no temperature). Nil takes the built-in knowledge.
	Reasoning *bool
	// ReasoningEffort is sent for reasoning models when set ("minimal",
	// "low", "medium", "high"). Empty omits it: accepted values differ per
	// model, so it is an operator's explicit choice.
	ReasoningEffort string

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
	effortRE   = regexp.MustCompile(`^(none|minimal|low|medium|high|xhigh)$`)
	profileKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// String describes the configuration without anything sensitive.
func (c Config) String() string {
	return fmt.Sprintf("openai.Config{BaseURL:%s Model:%s}", c.baseURL(), c.Model)
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
	if c.EmbeddingModel != "" && !modelIDRE.MatchString(c.EmbeddingModel) {
		return errors.New("the embedding model id is malformed")
	}
	for k, v := range c.Profiles {
		if !profileKey.MatchString(k) || !modelIDRE.MatchString(v) {
			return errors.New("a profile maps a malformed name or model id")
		}
	}
	if c.ReasoningEffort != "" && !effortRE.MatchString(c.ReasoningEffort) {
		return errors.New("unknown reasoning effort")
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
	return contracts.Wrap(contracts.ErrValidationFailed, err, "openai: invalid configuration")
}

// modelInfo is the built-in knowledge about a hosted model: limits, whether it
// is a reasoning model, and the list price in USD micro-units per million
// tokens. It is a DEFAULT: the operator's profile and Config override it, and
// the numbers are as read from the vendor's pages on the research date (see
// doc.go), not measured.
type modelInfo struct {
	context   int
	output    int
	reasoning bool
	inMicros  int64
	outMicros int64
}

var knownModels = map[string]modelInfo{
	"gpt-5":        {400000, 128000, true, 1_250_000, 10_000_000},
	"gpt-5-mini":   {400000, 128000, true, 250_000, 2_000_000},
	"gpt-5-nano":   {400000, 128000, true, 50_000, 400_000},
	"gpt-4.1-mini": {1047576, 32768, false, 400_000, 1_600_000},
	"gpt-4o-mini":  {128000, 16384, false, 150_000, 600_000},
}

// Conservative limits for a model the table does not know.
const (
	unknownContextToks = 128000
	unknownOutputToks  = 16384
)

var snapshotSuffixRE = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)

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
	id        string
	context   int
	output    int
	reasoning bool
	prices    *providers.PriceTable
}

func (c Config) profileFor(id string) modelProfile {
	p := modelProfile{id: id, context: unknownContextToks, output: unknownOutputToks}
	if m, ok := lookupModel(id); ok {
		p.context, p.output, p.reasoning = m.context, m.output, m.reasoning
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
	if c.Reasoning != nil {
		p.reasoning = *c.Reasoning
	}
	if c.Prices != nil {
		cp := *c.Prices
		p.prices = &cp
	}
	return p
}
