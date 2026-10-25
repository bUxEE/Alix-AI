package anthropic

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const healthTimeout = 10 * time.Second

// Capabilities implements providers.CapabilityProvider for the configured
// model. It answers from the built-in table and the operator's overrides and
// makes no call, so capability gating never sends data. CheckModel asks the
// provider.
func (c *Client) Capabilities(context.Context) (providers.ModelCapabilities, error) {
	caps := c.capabilities(c.model)
	if err := caps.Validate(); err != nil {
		return providers.ModelCapabilities{}, err
	}
	return caps.Clone(), nil
}

// Health implements providers.AIRuntimeProvider. The interface carries no
// scope, and a credential is scope-bound, so the probe is anonymous: an
// unauthenticated GET of the model list. Any answer from the API, including the
// 401 that is expected, proves the destination is allowed, TLS verifies and the
// service responds; it says nothing about the key, which CheckModel verifies. A
// network failure is reported as unavailable, not as a failed probe.
func (c *Client) Health(ctx context.Context) (providers.HealthReport, error) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	rep, err := c.call(ctx, contracts.Scope{}, routes[1], nil, "application/json")
	if err == nil {
		_ = rep.resp.Body.Close()
		return providers.HealthReport{Status: providers.HealthOK}, nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		switch status, _ := ce.Details["http_status"].(int); {
		case status == 401:
			return providers.HealthReport{Status: providers.HealthOK, Reason: "reachable; the key is not checked by this probe"}, nil
		case ce.Code == contracts.ErrRateLimited || status == 429:
			return providers.HealthReport{Status: providers.HealthDegraded, Reason: "the provider is rate limiting"}, nil
		}
	}
	return providers.HealthReport{Status: providers.HealthUnavailable, Reason: "the provider did not answer"}, nil
}

// CheckModel verifies the key and that the configured model is offered to it
// (GET /v1/models/<model>): a rejected key is an authentication error, an
// unknown model is model_not_found.
func (c *Client) CheckModel(ctx context.Context, scope contracts.Scope) error {
	if err := scope.Validate(); err != nil {
		return notSent(err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()
	rep, err := c.call(ctx, scope, c.modelRoute, nil, "application/json")
	if err != nil {
		return err
	}
	defer rep.resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(rep.resp.Body, maxErrorBody))
	return nil
}
