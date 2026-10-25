package decision

import (
	"context"
	"errors"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// DecisionModelProvider is the provider-neutral scorer contract (scoreDecision).
// Implementations score the supplied candidates and nothing else: they receive the
// minimum bounded state and the authorised candidate set, never raw application data,
// files or retrieved text, and what they return is a proposal the Engine verifies.
//
// ScoreDecision must honour ctx, return *contracts.Error values (timeout, unavailable,
// schema_invalid for output it cannot decode, resource_exhausted) or wrap
// ErrUnsupportedRuntime, and never return partial output as a success. It must be safe
// for concurrent use.
type DecisionModelProvider interface {
	// Manifest identifies the provider, model and revision that serve requests. The
	// Engine requires every result to echo it.
	Manifest() gen.ScorerManifest
	// ScoreDecision scores req.CandidateSet.
	ScoreDecision(ctx context.Context, scope contracts.Scope, req gen.DecisionRequest) (gen.DecisionScoreResult, error)
}

// ErrUnsupportedRuntime is wrapped by a provider whose runtime or backend cannot host
// the scorer; it maps to the unsupported_runtime fallback reason.
var ErrUnsupportedRuntime = errors.New("decision: the selected runtime does not support the scorer")

// routerSlot lets a scorer occupy the existing `router` role. The registry accepts
// that role only for a providers.TextGenerator, and this package must not change
// internal/providers, so the scorer is wrapped: it keeps the DecisionModelProvider
// method set (reached with providers.Lookup[DecisionModelProvider]) and its text
// methods fail closed, because a decision scorer is non-generative and never returns
// text.
type routerSlot struct{ DecisionModelProvider }

var errNonGenerative = contracts.New(contracts.ErrValidationFailed,
	"decision: the router role holds a non-generative scorer; it never generates text")

func (routerSlot) Chat(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatResult, error) {
	return providers.ChatResult{}, errNonGenerative
}

func (routerSlot) ChatStream(context.Context, contracts.Scope, providers.ChatRequest) (providers.ChatStream, error) {
	return nil, errNonGenerative
}

var _ providers.TextGenerator = routerSlot{}

// RegisterRouter registers p under the `router` role of reg. It fails like
// Registry.Register (nil provider, role already registered).
func RegisterRouter(reg *providers.Registry, p DecisionModelProvider) error {
	if p == nil {
		return contracts.New(contracts.ErrValidationFailed, "decision: nil scorer")
	}
	return reg.Register(providers.RoleRouter, routerSlot{p})
}

// ProviderFromRegistry returns the scorer registered as the router role. When there is
// none it returns a nil provider and the typed reason the Engine would record:
// unavailable (not registered, or the role is marked unavailable) or
// unsupported_runtime (the router role holds a plain text model, not a scorer). The
// reason is empty when a provider is returned.
func ProviderFromRegistry(reg *providers.Registry) (DecisionModelProvider, gen.FallbackReason) {
	if reg == nil || !reg.Usable(providers.RoleRouter) {
		return nil, gen.FallbackReasonUnavailable
	}
	p, err := providers.Lookup[DecisionModelProvider](reg, providers.RoleRouter)
	if err != nil {
		if contracts.HasCode(err, contracts.ErrUnavailable) {
			return nil, gen.FallbackReasonUnavailable
		}
		return nil, gen.FallbackReasonUnsupportedRuntime
	}
	return p, ""
}
