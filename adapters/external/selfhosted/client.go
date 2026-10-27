package selfhosted

import (
	"context"

	"github.com/buxee/alix-ai/adapters/external/chatcompat"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var (
	_ providers.MultimodalGenerator = (*Client)(nil)
	_ providers.CapabilityProvider  = (*Client)(nil)
	_ providers.AIRuntimeProvider   = (*Client)(nil)
)

type KeySource = chatcompat.KeySource

// OpenKeys supplies a placeholder key for endpoints without credential_ref.
type OpenKeys struct{}

func (OpenKeys) Key(_ context.Context, _ contracts.Scope, ref string) (credentials.Secret, error) {
	if ref == openLoopbackKeyRef {
		return credentials.Secret("open-no-auth"), nil
	}
	return credentials.Secret(""), contracts.New(contracts.ErrPermissionDenied, "self_hosted: no credential configured for this endpoint")
}

type Client struct{ *chatcompat.Client }

type Option = chatcompat.Option

var (
	WithClock   = chatcompat.WithClock
	WithLogger  = chatcompat.WithLogger
	WithSleeper = chatcompat.WithSleeper
)

func New(cfg Config, dialer *egress.Dialer, keys KeySource, opts ...Option) (*Client, error) {
	if keys == nil {
		keys = OpenKeys{}
	}
	inner, err := chatcompat.New(ProviderName, cfg.compat(), dialer, keys, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{Client: inner}, nil
}
