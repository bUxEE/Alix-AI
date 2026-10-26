package modes

import (
	"context"

	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// PrincipalBindingSource looks up a principal's LLM binding for a role (ADR 0041).
type PrincipalBindingSource interface {
	PrincipalBindingFor(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (profile pv.ProfileID, ok bool, err error)
}

// PrincipalPolicy prefers the principal's binding for controller, realisation, STT and TTS when the
// effective mode is External, then the deployment default (inner), then typed llm_not_configured.
// Local and Hybrid leave the inner policy unchanged. Each role resolves independently (ADR 0043).
type PrincipalPolicy struct {
	Inner    PolicySource
	Bindings PrincipalBindingSource
	Resolver *Resolver
	// RequireConfigured, when true, returns llm_not_configured for bindable roles with no
	// principal binding and no deployment default while External. When false (tests), empty
	// defaults fall through as local the way StaticPolicy does.
	RequireConfigured bool
}

// RolePolicy implements PolicySource.
func (p PrincipalPolicy) RolePolicy(ctx context.Context, scope contracts.Scope, role Role) (RolePolicy, error) {
	if p.Inner == nil {
		return RolePolicy{}, contracts.New(contracts.ErrInternal, "modes: principal policy needs an inner policy")
	}
	inner, err := p.Inner.RolePolicy(ctx, scope, role)
	if err != nil {
		return RolePolicy{}, err
	}
	prRole, ok := principalRoleOf(role)
	if !ok || p.Bindings == nil {
		return inner, nil
	}
	external := false
	if p.Resolver != nil {
		res, rerr := p.Resolver.Resolve(ctx, scope)
		if rerr != nil {
			return RolePolicy{}, rerr
		}
		external = res.Mode == ModeExternal
	}
	if !external {
		return inner, nil
	}
	profile, found, err := p.Bindings.PrincipalBindingFor(ctx, scope, prRole)
	if err != nil {
		return RolePolicy{}, err
	}
	if found {
		out := inner
		out.Profiles = []pv.ProfileID{profile}
		return out, nil
	}
	if len(inner.Profiles) > 0 {
		return inner, nil
	}
	if p.RequireConfigured {
		return RolePolicy{}, contracts.New(contracts.ErrUnavailable, "modes: no LLM is configured for this principal and role").
			WithDetails("reason", "llm_not_configured").WithDetails("role", string(role))
	}
	return inner, nil
}

func principalRoleOf(role Role) (pv.PrincipalLlmRole, bool) {
	switch role {
	case RoleController:
		return pv.PrincipalLlmRoleConversationalController, true
	case RoleRealisation:
		return pv.PrincipalLlmRoleLanguageRealization, true
	case RoleSTT:
		return pv.PrincipalLlmRoleSTT, true
	case RoleTTS:
		return pv.PrincipalLlmRoleTTS, true
	}
	return "", false
}
