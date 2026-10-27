package modes

import (
	"context"

	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// PrivateEndpointSource resolves tenant pins and principal private bindings (ADR 0045).
type PrivateEndpointSource interface {
	TenantPrivateEndpoint(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (providerreg.PrivateEndpoint, pv.ModelID, bool, error)
	PrincipalPrivateEndpoint(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (providerreg.PrivateEndpoint, pv.ModelID, bool, error)
}

// PrivatePolicy prefers tenant pin, then principal private binding, then inner (ADR 0045).
// Applies only when effective mode is External.
type PrivatePolicy struct {
	Inner     PolicySource
	Endpoints PrivateEndpointSource
	Resolver  *Resolver
}

// RolePolicy implements PolicySource.
func (p PrivatePolicy) RolePolicy(ctx context.Context, scope contracts.Scope, role Role) (RolePolicy, error) {
	if p.Inner == nil {
		return RolePolicy{}, contracts.New(contracts.ErrInternal, "modes: private policy needs an inner policy")
	}
	prRole, ok := principalRoleOf(role)
	if !ok || p.Endpoints == nil {
		return p.Inner.RolePolicy(ctx, scope, role)
	}
	external := false
	if p.Resolver != nil {
		res, err := p.Resolver.Resolve(ctx, scope)
		if err != nil {
			return RolePolicy{}, err
		}
		external = res.Mode == ModeExternal
	}
	if !external {
		return p.Inner.RolePolicy(ctx, scope, role)
	}
	if ep, model, found, err := p.TenantPrivateEndpoint(ctx, scope, prRole); err != nil {
		return RolePolicy{}, err
	} else if found {
		return p.resolveEndpoint(ctx, scope, role, ep, model)
	}
	if ep, model, found, err := p.PrincipalPrivateEndpoint(ctx, scope, prRole); err != nil {
		return RolePolicy{}, err
	} else if found {
		return p.resolveEndpoint(ctx, scope, role, ep, model)
	}
	return p.Inner.RolePolicy(ctx, scope, role)
}

func (p PrivatePolicy) TenantPrivateEndpoint(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (providerreg.PrivateEndpoint, pv.ModelID, bool, error) {
	return p.Endpoints.TenantPrivateEndpoint(ctx, scope, role)
}

func (p PrivatePolicy) PrincipalPrivateEndpoint(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (providerreg.PrivateEndpoint, pv.ModelID, bool, error) {
	return p.Endpoints.PrincipalPrivateEndpoint(ctx, scope, role)
}

func (p PrivatePolicy) resolveEndpoint(ctx context.Context, scope contracts.Scope, role Role, ep providerreg.PrivateEndpoint, model pv.ModelID) (RolePolicy, error) {
	if err := providerreg.EndpointOwnerPermitsUse(scope, ep); err != nil {
		return RolePolicy{}, err
	}
	switch ep.Lifecycle {
	case pv.PrivateLlmLifecycleProvisioning, pv.PrivateLlmLifecycleWarming:
		return RolePolicy{}, contracts.New(contracts.ErrUnavailable, "modes: the private LLM endpoint is starting").
			WithDetails("reason", "private_llm_starting").WithDetails("role", string(role))
	case pv.PrivateLlmLifecycleFailed, pv.PrivateLlmLifecycleStopped, pv.PrivateLlmLifecycleDraining:
		return RolePolicy{}, contracts.New(contracts.ErrUnavailable, "modes: the private LLM endpoint is unavailable").
			WithDetails("reason", "private_llm_unavailable").WithDetails("role", string(role))
	case pv.PrivateLlmLifecycleReady:
		out := RolePolicy{Profiles: []pv.ProfileID{ep.ProfileID}}
		if inner, err := p.Inner.RolePolicy(ctx, scope, role); err == nil {
			out = inner
			out.Profiles = []pv.ProfileID{ep.ProfileID}
		}
		_ = model
		return out, nil
	default:
		return RolePolicy{}, contracts.New(contracts.ErrUnavailable, "modes: the private LLM endpoint is unavailable").
			WithDetails("reason", "private_llm_unavailable").WithDetails("role", string(role))
	}
}
