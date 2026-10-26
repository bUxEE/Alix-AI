package mistral

import (
	"github.com/buxee/alix-ai/adapters/external/chatcompat"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
)

var (
	_ providers.MultimodalGenerator = (*Client)(nil)
	_ providers.CapabilityProvider  = (*Client)(nil)
	_ providers.AIRuntimeProvider   = (*Client)(nil)
)

type KeySource = chatcompat.KeySource

type ServiceKeys = chatcompat.ServiceKeys

type Client struct{ *chatcompat.Client }

type Option = chatcompat.Option

var (
	WithClock   = chatcompat.WithClock
	WithLogger  = chatcompat.WithLogger
	WithSleeper = chatcompat.WithSleeper
)

func New(cfg Config, dialer *egress.Dialer, keys KeySource, opts ...Option) (*Client, error) {
	inner, err := chatcompat.New(ProviderName, cfg.compat(), dialer, keys, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{Client: inner}, nil
}
