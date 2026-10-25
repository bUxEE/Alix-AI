package meta

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ProviderName is what Name reports and the provider id used for the egress
// allowlist and the credential binding.
const ProviderName = "meta"

// DefaultBaseURL is the origin of Meta's own Model API (dialect DialectModelAPI).
// A hosted-Llama endpoint (DialectOpenAICompat) has no default host.
const DefaultBaseURL = "https://api.meta.ai"

// DefaultAPIPrefix is the path prefix of the API under the origin when
// Config.APIPrefix is empty.
const DefaultAPIPrefix = "/v1"

// Dialect says which service the client talks to. Both speak the Chat
// Completions wire format; they differ in the details that decide whether a
// request is accepted (doc.go has the sources).
type Dialect string

const (
	// DialectModelAPI is Meta's own Model API (the empty value means this):
	// models muse-spark-*, a default host, max_completion_tokens, no stop
	// sequences, tool_choice "auto" only, health at GET /v1/status.
	DialectModelAPI Dialect = "meta-model-api"
	// DialectOpenAICompat is a third-party host of Llama models behind an
	// OpenAI-compatible endpoint: the operator names the host, max_tokens,
	// stop sequences and every tool_choice are sent as the caller asked, health
	// at GET <prefix>/models.
	DialectOpenAICompat Dialect = "openai-compatible"
)

func (d Dialect) resolved() Dialect {
	if d == "" {
		return DialectModelAPI
	}
	return d
}

// Config configures one hosted-Llama provider profile. There is deliberately no
// field that holds a key: CredentialRef is an opaque reference that the
// credential service resolves on every call. There is deliberately no default
// host either: no first-party Meta endpoint exists any more (doc.go), so which
// third-party host receives the data is always the operator's explicit choice.
type Config struct {
	// Dialect selects the service (empty: DialectModelAPI).
	Dialect Dialect
	// BaseURL is the service origin: https, or plain http to loopback for tests.
	// Empty means DefaultBaseURL for DialectModelAPI and is an error for
	// DialectOpenAICompat. The egress allowlist must list the same origin
	// (egress.DestinationFromURL derives the entry).
	BaseURL string
	// APIPrefix is the path under which the host serves the OpenAI-compatible
	// API ("/v1", "/openai/v1", "/api/v1"). Empty means DefaultAPIPrefix.
	APIPrefix string
	// CredentialRef is the credential reference ("cred_..."), never a key.
	CredentialRef string
	// Model is the service's id for the model the controller role uses
	// ("muse-spark-1.3"; on a hosted-Llama endpoint it may contain "/", for
	// example "meta-llama/Llama-3.3-70B-Instruct-Turbo"). Required: there is no
	// default model, so a model change is always an operator decision.
	Model string
	// ReplyModel, when set, is the id a reply that names Model is reported
	// under (the caller's own id for the model, such as a registry id that
	// cannot hold a "/"). A reply that names another model is reported as the
	// service named it.
	ReplyModel string
	// AllowContributorTier permits a "*-contributor" model id of the Model API.
	// That tier is priced far below the standard one, and the documentation
	// read for this adapter does not say what it asks in return (data use,
	// review, eligibility), so it is refused unless the operator has read its
	// terms and accepts them (task X-H01).
	AllowContributorTier bool
	// ReasoningEffort is sent as reasoning_effort when set ("minimal", "low",
	// "medium", "high", "xhigh", "max"). Empty omits it: the service then
	// reasons at a model-determined level. "none" is not accepted by Muse Spark.
	ReasoningEffort string
	// Profiles maps a request's Profile hint to another model id. A hint that
	// is not listed is refused.
	Profiles map[string]string

	// MaxContextToks and MaxOutputToks override the built-in limits of the
	// configured model; zero takes the built-in value (or a conservative
	// default for an unknown model). Hosts cap Llama contexts below the model's
	// own limit, so the operator should state the host's real figures.
	MaxContextToks int
	MaxOutputToks  int
	// DefaultMaxOutputToks is sent when a request names no cap.
	DefaultMaxOutputToks int
	// MaxTools is the most tools one request may carry (0: 32).
	MaxTools int
	// Prices is the price table in micro-units (USD for the built-in entries).
	// Nil takes the built-in entry of the model; an unknown model without a
	// table has no cost and cannot be budgeted.
	Prices *providers.PriceTable
	// NoForcedToolChoice is set for an OpenAI-compatible host that rejects
	// tool_choice "required" or "none" (DialectModelAPI always behaves so): a
	// required proposal is then sent as "auto" and enforced on the answer, and
	// "none" omits the tools.
	NoForcedToolChoice bool

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
	// modelIDRE accepts a host's model id, "/" included.
	modelIDRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+/-]{0,127}$`)
	prefixRE   = regexp.MustCompile(`^(/[A-Za-z0-9._~-]{1,32}){1,4}$`)
	effortRE   = regexp.MustCompile(`^(minimal|low|medium|high|xhigh|max)$`)
	profileKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// String describes the configuration without anything sensitive.
func (c Config) String() string {
	return fmt.Sprintf("meta.Config{Dialect:%s BaseURL:%s Model:%s}", c.Dialect.resolved(), c.BaseURL, c.Model)
}

func anyProfileHasSuffix(m map[string]string, suffix string) bool {
	for _, v := range m {
		if strings.HasSuffix(v, suffix) {
			return true
		}
	}
	return false
}

func validModelID(id string) bool {
	return modelIDRE.MatchString(id) && !strings.Contains(id, "..") && !strings.Contains(id, "//") && !strings.HasSuffix(id, "/")
}

// Validate checks the configuration (fail closed).
func (c Config) Validate() error {
	if c.CredentialRef == "" {
		return errors.New("a credential reference is required")
	}
	switch c.Dialect.resolved() {
	case DialectModelAPI:
	case DialectOpenAICompat:
		if c.BaseURL == "" {
			return errors.New("a base URL is required: a hosted-Llama endpoint has no default host")
		}
	default:
		return errors.New("unknown dialect")
	}
	if c.BaseURL != "" {
		if u, err := url.Parse(c.BaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("the base URL must be an http(s) origin")
		}
	}
	if c.ReasoningEffort != "" && !effortRE.MatchString(c.ReasoningEffort) {
		return errors.New("unknown reasoning effort")
	}
	if c.Dialect.resolved() == DialectModelAPI && !c.AllowContributorTier &&
		(strings.HasSuffix(c.Model, "-contributor") || anyProfileHasSuffix(c.Profiles, "-contributor")) {
		return errors.New("a contributor-tier model is refused unless AllowContributorTier is set")
	}
	if c.APIPrefix != "" && (!prefixRE.MatchString(c.APIPrefix) || strings.Contains(c.APIPrefix, "..")) {
		return errors.New("the API prefix is malformed")
	}
	if !validModelID(c.Model) {
		return errors.New("a model id is required")
	}
	for k, v := range c.Profiles {
		if !profileKey.MatchString(k) || !validModelID(v) {
			return errors.New("a profile maps a malformed name or model id")
		}
	}
	if c.MaxContextToks < 0 || c.MaxOutputToks < 0 || c.DefaultMaxOutputToks < 0 || c.MaxTools < 0 || c.MaxResponseBytes < 0 ||
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
	c.Dialect = c.Dialect.resolved()
	if c.APIPrefix == "" {
		c.APIPrefix = DefaultAPIPrefix
	}
	if c.BaseURL == "" {
		c.BaseURL = DefaultBaseURL
	}
	if c.DefaultMaxOutputToks == 0 {
		c.DefaultMaxOutputToks = 4096
		if c.Dialect == DialectModelAPI {
			c.DefaultMaxOutputToks = 16384 // Muse Spark always reasons, and reasoning tokens count against the cap
		}
	}
	if c.MaxTools == 0 {
		c.MaxTools = 32
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
	return contracts.Wrap(contracts.ErrValidationFailed, err, "meta: invalid configuration")
}

// modelInfo is the built-in knowledge about a Llama model: limits and the list
// price in USD micro-units per million tokens. It is a DEFAULT, not a fact: the
// operator's Config overrides it, and every number is a dated figure read from a
// third-party summary (doc.go), not measured on any host.
type modelInfo struct {
	context   int
	output    int
	inMicros  int64
	outMicros int64
}

// museSpark is the Model API's standard-tier price as summarised on 2026-10-08:
// $1.25 in / $4.25 out per million tokens, with a 1,048,576-token context. The
// output cap is not stated by the pages read, so 32768 is a conservative figure
// the operator can raise. The contributor tier (listed far lower) is deliberately
// absent: see Config.AllowContributorTier.
var museSpark = modelInfo{1_048_576, 32768, 1_250_000, 4_250_000}

// knownModels is keyed by the lower-case last path segment of the model id with
// any dated suffix removed. Besides the Muse Spark rows, Llama 4 Scout stands for
// a hosted-Llama endpoint: an aggregator's list price of $0.10 in / $0.30 out per
// million tokens as summarised on 2026-10-08 (flagged for live measurement,
// X-H02). Its 131072 context is a host-side cap seen on several hosts, not the
// model's own limit, and its output cap is conservative; the operator raises
// either from the chosen host's page.
var knownModels = map[string]modelInfo{
	"llama-4-scout":  {131072, 8192, 100_000, 300_000},
	"muse-spark-1.3": museSpark,
	"muse-spark-1.2": museSpark,
	"muse-spark-1.1": museSpark,
}

// Conservative limits for a model the table does not know.
const (
	unknownContextToks = 131072
	unknownOutputToks  = 4096
)

var snapshotSuffixRE = regexp.MustCompile(`-(\d{4}-\d{2}-\d{2}|\d{8})$`)

func lookupModel(id string) (modelInfo, bool) {
	id = strings.ToLower(id)
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
	}
	id = strings.TrimSuffix(strings.TrimSuffix(snapshotSuffixRE.ReplaceAllString(id, ""), "-instruct"), "-turbo")
	m, ok := knownModels[id]
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
	id      string
	context int
	output  int
	prices  *providers.PriceTable
}

func (c Config) profileFor(id string) modelProfile {
	p := modelProfile{id: id, context: unknownContextToks, output: unknownOutputToks}
	if m, ok := lookupModel(id); ok {
		p.context, p.output = m.context, m.output
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
