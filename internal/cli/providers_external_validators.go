//go:build !noexternal

package cli

import (
	"context"
	"errors"

	"github.com/buxee/alix-ai/adapters/external/anthropic"
	"github.com/buxee/alix-ai/adapters/external/gemini"
	"github.com/buxee/alix-ai/adapters/external/meta"
	"github.com/buxee/alix-ai/adapters/external/openai"
	"github.com/buxee/alix-ai/internal/credentials"
	"github.com/buxee/alix-ai/internal/egress"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// The manual-key validators of every provider that has an adapter (additive plan 4: manual credential entry is
// the fallback where OAuth is not offered). Each makes the one minimal authenticated request that proves the key
// works, through the destination allowlist, with the candidate key and nothing else: no tenant, no prompt.
//
//	openai     GET /v1/models (the model list)
//	meta       GET <prefix>/models (the model list)
//	anthropic  GET /v1/models/<model> (a model of the adapter's table)
//	gemini     GET /v1beta/models/<model>, key in the x-goog-api-key header (never the URL)
//
// The model of the two single-model probes is only the probe's: an unknown model answers model_not_found AFTER
// the key authenticated, so it counts as an accepted key. No provider is contacted unless a key is submitted.

// extGapProbeModel is the model id the single-model probes ask for: the cheapest tier of each adapter's table. It
// is never used to generate anything.
const (
	extGapProbeModelAnthropic = "claude-haiku-4-5"
	extGapProbeModelGemini    = "gemini-2.5-flash-lite"
	extGapProbeModelOpenAI    = "gpt-4.1-mini"
	extGapProbeModelMeta      = "llama-4-maverick"
)

// extGapProbeSpec is the adapter spec of a provider for the key probe, over a fresh destination registry that
// holds only the provider's own origin and refuses the platform's own listeners, as every other registry does.
func (v externalKeyValidator) extGapProbeSpec(provider string, key credentials.Secret) (ExternalProviderSpec, bool) {
	spec := ExternalProviderSpec{Provider: provider, Egress: extFinalProviderRegistry(), Keys: externalOneKey{key: key}}
	const ref = "cred_validation" // not a key: the one-key source answers whatever the reference is
	switch provider {
	case ExternalProviderOpenAI:
		spec.OpenAI = openai.Config{BaseURL: v.base, CredentialRef: ref, Model: extGapProbeModelOpenAI}
	case ExternalProviderAnthropic:
		spec.Anthropic = anthropic.Config{BaseURL: v.sec.AnthropicBaseURL, CredentialRef: ref, Model: extGapProbeModelAnthropic}
	case ExternalProviderGemini:
		spec.Gemini = gemini.Config{BaseURL: v.sec.GeminiBaseURL, CredentialRef: ref, Model: extGapProbeModelGemini}
	case ExternalProviderMeta:
		spec.Meta = meta.Config{
			Dialect: meta.Dialect(v.sec.MetaDialect), BaseURL: v.sec.MetaBaseURL, APIPrefix: v.sec.MetaAPIPrefix, CredentialRef: ref,
			Model: extGapProbeModelMeta,
		}
	default:
		return ExternalProviderSpec{}, false
	}
	return spec, true
}

// ValidateKey implements credentials.KeyValidator. A key the provider rejects is Accepted=false; a failure to make
// the request (or a provider with no validator) is an error, so the key is not stored on a guess.
func (v externalKeyValidator) ValidateKey(ctx context.Context, req credentials.ValidationRequest) (credentials.ValidationResult, error) {
	spec, ok := v.extGapProbeSpec(string(req.ProviderID), req.Key)
	if !ok {
		return credentials.ValidationResult{}, contracts.New(contracts.ErrUnavailable, "no key validator exists for this provider")
	}
	p, err := NewExternalProvider(spec)
	if err != nil {
		return credentials.ValidationResult{}, err
	}
	// The adapter wants a well-formed scope; this one names nothing of any tenant.
	probe := contracts.Scope{
		DeploymentID: v.deployment, ApplicationID: "key-validation", TenantID: "key-validation", WorkspaceID: "key-validation",
		PrincipalID: "key-validation", Roles: []string{"validator"}, CapabilityScopes: []string{"models.list"}, SessionID: "key-validation",
	}
	switch c := p.(type) {
	case interface {
		Models(context.Context, contracts.Scope) ([]string, error)
	}:
		_, err = c.Models(ctx, probe)
	case interface {
		CheckModel(context.Context, contracts.Scope) error
	}:
		err = c.CheckModel(ctx, probe)
	default:
		return credentials.ValidationResult{}, contracts.New(contracts.ErrUnavailable, "the provider adapter cannot probe a key")
	}
	return extGapProbeResult(err)
}

// extGapProbeResult maps the outcome of the probe: no error or a model the key cannot see is an accepted key, an
// authentication or permission refusal is a rejected one, and anything else (the provider unreachable, rate
// limited, malformed) is an error that stores nothing.
func extGapProbeResult(err error) (credentials.ValidationResult, error) {
	if err == nil {
		return credentials.ValidationResult{Accepted: true}, nil
	}
	if egress.IsDenied(err) {
		// The request never left (the destination allowlist or the platform deny-list refused it): the key was not
		// judged, so it is neither accepted nor rejected.
		return credentials.ValidationResult{}, contracts.New(contracts.ErrUnavailable, "the provider could not be reached to validate the key")
	}
	switch contracts.CodeOf(err) {
	case contracts.ErrUnauthenticated, contracts.ErrPermissionDenied:
		return credentials.ValidationResult{Accepted: false}, nil
	case contracts.ErrValidationFailed:
		var ce *contracts.Error
		if errors.As(err, &ce) && ce.Details["provider_error_kind"] == string(pv.ProviderErrorKindModelNotFound) {
			return credentials.ValidationResult{Accepted: true}, nil
		}
	}
	return credentials.ValidationResult{}, contracts.New(contracts.ErrUnavailable, "the provider could not validate the key")
}
