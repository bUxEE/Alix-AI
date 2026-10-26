package mistral

import (
	"fmt"
	"time"

	"github.com/buxee/alix-ai/adapters/external"
	"github.com/buxee/alix-ai/adapters/external/chatcompat"
	"github.com/buxee/alix-ai/internal/providers"
)

const (
	ProviderName   = "mistral"
	DefaultBaseURL = "https://api.mistral.ai"
)

type Config struct {
	BaseURL         string
	APIPrefix       string
	CredentialRef   string
	Model           string
	ReplyModel      string
	Profiles        map[string]string
	ReasoningEffort string

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

func (c Config) baseURL() string {
	if c.BaseURL == "" {
		return DefaultBaseURL
	}
	return c.BaseURL
}

func (c Config) String() string {
	return fmt.Sprintf("mistral.Config{BaseURL:%s Model:%s}", c.baseURL(), c.Model)
}

func (c Config) compat() chatcompat.Config {
	cc := chatcompat.Config{
		BaseURL: c.baseURL(), CredentialRef: c.CredentialRef,
		WireModel: c.Model, ReplyModel: c.ReplyModel, ReasoningEffort: c.ReasoningEffort, Profiles: c.Profiles,
		MaxContextToks: c.MaxContextToks, MaxOutputToks: c.MaxOutputToks, DefaultMaxOutputToks: c.DefaultMaxOutputToks,
		MaxTools: c.MaxTools, Prices: c.Prices, NoForcedToolChoice: c.NoForcedToolChoice,
		RequestTimeout: c.RequestTimeout, StreamTimeout: c.StreamTimeout, MaxResponseBytes: c.MaxResponseBytes, Retry: c.Retry,
	}
	if c.APIPrefix != "" {
		cc.APIPrefix = c.APIPrefix
	}
	return cc
}

func (c Config) Validate() error { return c.compat().Validate() }
