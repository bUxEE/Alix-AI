package meta

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

const healthTimeout = 10 * time.Second

// Capabilities implements providers.CapabilityProvider for the configured
// model. It answers from the built-in table and the operator's overrides and
// makes no call, so capability gating never sends data. Models and CheckModel
// ask the host.
func (c *Client) Capabilities(context.Context) (providers.ModelCapabilities, error) {
	caps := c.capabilities(c.model)
	if err := caps.Validate(); err != nil {
		return providers.ModelCapabilities{}, err
	}
	return caps.Clone(), nil
}

// Health implements providers.AIRuntimeProvider. The interface carries no scope,
// and a credential is scope-bound, so the probe is anonymous: an unauthenticated
// GET of the model list. Any answer from the host, including the 401 that is
// expected, proves the destination is allowed, TLS verifies and the service
// responds; it says nothing about the key, which Models and CheckModel verify.
// A network failure is reported as unavailable, not as a failed probe.
func (c *Client) Health(ctx context.Context) (providers.HealthReport, error) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	rep, err := c.call(ctx, contracts.Scope{}, c.routes.health, nil, "application/json")
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

const maxListedModels = 4096

// Models lists the model ids the key can use (GET <prefix>/models), sorted. It
// verifies the key as a side effect: a rejected key is an authentication error.
func (c *Client) Models(ctx context.Context, scope contracts.Scope) ([]string, error) {
	if err := scope.Validate(); err != nil {
		return nil, notSent(err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	defer cancel()
	rep, err := c.call(ctx, scope, c.routes.models, nil, "application/json")
	if err != nil {
		return nil, err
	}
	defer rep.resp.Body.Close()
	raw, err := c.readBody(ctx, rep.resp.Body)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Data) > maxListedModels {
		return nil, c.wrapDecode("model list")
	}
	ids := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		if validModelID(m.ID) {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// CheckModel verifies the key and that the configured model is offered to it.
func (c *Client) CheckModel(ctx context.Context, scope contracts.Scope) error {
	ids, err := c.Models(ctx, scope)
	if err != nil {
		return err
	}
	i := sort.SearchStrings(ids, c.cfg.Model)
	if i == len(ids) || ids[i] != c.cfg.Model {
		return apiErr(contracts.ErrValidationFailed, pv.ProviderErrorKindModelNotFound, true, errorText(pv.ProviderErrorKindModelNotFound))
	}
	return nil
}
