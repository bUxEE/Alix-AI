package chatcompat

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

// DefaultAPIPrefix is the path prefix when Config.APIPrefix is empty.
const DefaultAPIPrefix = "/v1"

// Config configures one OpenAI-compatible Chat Completions endpoint.
type Config struct {
	BaseURL       string
	APIPrefix     string
	CredentialRef string
	// WireModel is the model id sent on the wire (may contain "/").
	WireModel string
	// ReplyModel, when set, is the id replies name when the service named WireModel.
	ReplyModel      string
	ReasoningEffort string
	Profiles        map[string]string

	MaxContextToks       int
	MaxOutputToks        int
	DefaultMaxOutputToks int
	MaxTools             int
	Prices               *providers.PriceTable
	NoForcedToolChoice   bool

	RequestTimeout   time.Duration
	StreamTimeout    time.Duration
	MaxResponseBytes int64
	Retry            external.RetryPolicy
}

var (
	modelIDRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+/-]{0,127}$`)
	prefixRE   = regexp.MustCompile(`^(/[A-Za-z0-9._~-]{1,32}){1,4}$`)
	effortRE   = regexp.MustCompile(`^(minimal|low|medium|high|xhigh|max)$`)
	profileKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

func validModelID(id string) bool {
	return modelIDRE.MatchString(id) && !strings.Contains(id, "..") && !strings.Contains(id, "//") && !strings.HasSuffix(id, "/")
}

// String describes the configuration without secrets.
func (c Config) String() string {
	return fmt.Sprintf("chatcompat.Config{BaseURL:%s WireModel:%s}", c.BaseURL, c.WireModel)
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if c.CredentialRef == "" {
		return errors.New("a credential reference is required")
	}
	if c.BaseURL == "" {
		return errors.New("a base URL is required")
	}
	if u, err := url.Parse(c.BaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("the base URL must be an http(s) origin")
	}
	if c.ReasoningEffort != "" && !effortRE.MatchString(c.ReasoningEffort) {
		return errors.New("unknown reasoning effort")
	}
	if c.APIPrefix != "" && (!prefixRE.MatchString(c.APIPrefix) || strings.Contains(c.APIPrefix, "..")) {
		return errors.New("the API prefix is malformed")
	}
	if !validModelID(c.WireModel) {
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
	if c.APIPrefix == "" {
		c.APIPrefix = DefaultAPIPrefix
	}
	if c.DefaultMaxOutputToks == 0 {
		c.DefaultMaxOutputToks = 4096
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

func configError(provider string, err error) error {
	return contracts.Wrap(contracts.ErrValidationFailed, err, provider+": invalid configuration")
}

type modelInfo struct {
	context   int
	output    int
	inMicros  int64
	outMicros int64
}

var knownModels = map[string]modelInfo{
	"llama-4-scout": {131072, 8192, 100_000, 300_000},
}

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
	if id != c.WireModel {
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
