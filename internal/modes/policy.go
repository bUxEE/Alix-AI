package modes

import (
	"context"
	"slices"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Role is a model job as the router sees it. The external roles map to the schema roles of
// pkg/contracts/gen/providers; the rest never leave the deployment.
type Role string

// The roles.
const (
	RoleController  Role = "controller"
	RoleRealisation Role = "realisation"
	// RoleDecisionScorer is the optional decision layer (master plan 8.5, role "router"): local and
	// advisory. External providers never receive decision requests.
	RoleDecisionScorer  Role = "router"
	RoleVision          Role = "vision"
	RoleImageGeneration Role = "image_generation"
	RoleSTT             Role = "stt"
	RoleTTS             Role = "tts"
	RoleEmbedder        Role = "embedder"
	RoleReranker        Role = "reranker"
	RoleVAD             Role = "vad"
)

// AllRoles lists every role in report order.
func AllRoles() []Role {
	return []Role{RoleController, RoleRealisation, RoleDecisionScorer, RoleEmbedder, RoleReranker,
		RoleSTT, RoleTTS, RoleVAD, RoleVision, RoleImageGeneration}
}

// External returns the schema role an external provider profile serves for r. It refuses the
// decision-scorer role, the reranker, VAD and the embedder: only the controller, language
// realisation and the multimodal roles (vision, image generation, speech) may leave. The refusal is
// permission_denied and never names a provider.
func (r Role) External() (pv.Role, error) {
	switch r {
	case RoleController:
		return pv.RoleConversationalController, nil
	case RoleRealisation:
		return pv.RoleLanguageRealization, nil
	case RoleVision:
		return pv.RoleVision, nil
	case RoleImageGeneration:
		return pv.RoleImageGeneration, nil
	case RoleSTT:
		return pv.RoleSTT, nil
	case RoleTTS:
		return pv.RoleTTS, nil
	}
	reason := "role_local_only"
	if r == RoleDecisionScorer {
		reason = "decision_scorer_local_only"
	}
	return "", contracts.New(contracts.ErrPermissionDenied, "modes: this role is never routed to an external provider").
		WithDetails("reason", reason).WithDetails("role", string(r))
}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return slices.Contains(AllRoles(), r) }

// runtimeRole maps r to the local runtime registry role, if the local runtime has one.
func (r Role) runtimeRole() (providers.Role, bool) {
	switch r {
	case RoleController, RoleRealisation:
		return providers.RoleController, true // one text generator serves both
	case RoleDecisionScorer:
		return providers.RoleRouter, true
	case RoleEmbedder:
		return providers.RoleEmbedder, true
	case RoleReranker:
		return providers.RoleReranker, true
	case RoleSTT:
		return providers.RoleSTT, true
	case RoleTTS:
		return providers.RoleTTS, true
	case RoleVAD:
		return providers.RoleVAD, true
	}
	return "", false // vision and image generation have no local role
}

// LocalProviders reports whether the local provider of a role can serve right now.
type LocalProviders interface {
	Usable(role Role) bool
}

type registryLocal struct{ r *providers.Registry }

// FromRegistry adapts the local runtime registry (internal/providers) to LocalProviders. A role with
// no local runtime role (vision, image generation) is never usable locally.
func FromRegistry(r *providers.Registry) LocalProviders { return registryLocal{r} }

func (l registryLocal) Usable(role Role) bool {
	rr, ok := role.runtimeRole()
	return ok && l.r != nil && l.r.Usable(rr)
}

// Condition is a failure condition a policy may name for a fallback.
type Condition string

// The conditions. The first four are the ones a provider profile's own fallback may name
// (providerreg); the rest may only be named for the LOCAL fallback of a role, because an alternate
// external provider is never selected for them.
const (
	CondUnavailable         Condition = "unavailable"
	CondTimeout             Condition = "timeout"
	CondRateLimited         Condition = "rate_limited"
	CondUnsupportedModality Condition = "unsupported_modality"
	CondEgressDenied        Condition = "egress_denied"
	CondBudgetExhausted     Condition = "budget_exhausted"
	CondInvalidCredential   Condition = "invalid_credential"
	CondStaleProfile        Condition = "stale_profile"
	CondMalformedOutput     Condition = "malformed_output"
)

// RolePolicy is the routing policy of one role.
type RolePolicy struct {
	// Profiles are the candidate external profiles, in order of preference. Empty means the role is
	// served locally. A candidate that cannot serve the request (capability gate) is skipped for the
	// next; any other refusal ends the request.
	Profiles []pv.ProfileID
	// LocalFallback lists the conditions on which the local provider serves instead. Nothing else
	// falls back to local: a condition not listed here ends the request with its typed outcome.
	LocalFallback []Condition
}

// FallsBackOn reports whether the policy names the local provider for c.
func (p RolePolicy) FallsBackOn(c Condition) bool { return slices.Contains(p.LocalFallback, c) }

// PolicySource returns the routing policy of a role for a scope.
type PolicySource interface {
	RolePolicy(ctx context.Context, scope contracts.Scope, role Role) (RolePolicy, error)
}

// StaticPolicy is a PolicySource that answers the same for every scope.
type StaticPolicy map[Role]RolePolicy

// RolePolicy implements PolicySource.
func (s StaticPolicy) RolePolicy(_ context.Context, _ contracts.Scope, role Role) (RolePolicy, error) {
	return s[role], nil
}

// Validate refuses a policy that lists an external profile for a role that never leaves, or a
// profile id more than once.
func (s StaticPolicy) Validate() error {
	for role, p := range s {
		if !role.Valid() {
			return contracts.New(contracts.ErrValidationFailed, "modes: unknown role in the policy").WithDetails("role", string(role))
		}
		if len(p.Profiles) == 0 {
			continue
		}
		if _, err := role.External(); err != nil {
			return err
		}
		seen := map[pv.ProfileID]bool{}
		for _, id := range p.Profiles {
			if seen[id] {
				return contracts.New(contracts.ErrValidationFailed, "modes: a profile is listed twice for a role").WithDetails("role", string(role))
			}
			seen[id] = true
		}
	}
	return nil
}
