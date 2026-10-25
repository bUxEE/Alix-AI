package hybrid

import (
	"context"

	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// RealisationPolicy returns the routing policy of the realisation role: the external profiles in
// order of preference, and the local provider named for an outage (unavailable, timeout, rate
// limited), a profile that cannot serve the modality, a malformed reply and denied egress. A budget
// that is exhausted, a rejected credential and a stale profile are not listed: they end the external
// route with their own outcome and the turn shows the deterministic template, never another provider.
func RealisationPolicy(profiles ...pv.ProfileID) modes.RolePolicy {
	return modes.RolePolicy{
		Profiles: profiles,
		LocalFallback: []modes.Condition{
			modes.CondUnavailable, modes.CondTimeout, modes.CondRateLimited,
			modes.CondUnsupportedModality, modes.CondMalformedOutput, modes.CondEgressDenied,
		},
	}
}

// ControllerLocalPolicy wraps an operator policy so that in Hybrid mode the controller role has no
// external profiles and stays local. Every other role, and every role in any other mode, is passed
// through to the inner policy unchanged. The composition root gives it to the dialogue runtime
// (extmode) so the controller route stays local under Hybrid while External mode is untouched.
func ControllerLocalPolicy(inner modes.PolicySource, res *modes.Resolver) modes.PolicySource {
	return controllerLocal{inner: inner, res: res}
}

type controllerLocal struct {
	inner modes.PolicySource
	res   *modes.Resolver
}

// RolePolicy implements modes.PolicySource. A mode that cannot be resolved leaves the controller local.
func (p controllerLocal) RolePolicy(ctx context.Context, scope contracts.Scope, role modes.Role) (modes.RolePolicy, error) {
	if role == modes.RoleController {
		r, err := p.res.Resolve(ctx, scope)
		if err != nil || r.Mode == modes.ModeHybrid {
			return modes.RolePolicy{}, nil
		}
	}
	return p.inner.RolePolicy(ctx, scope, role)
}

// realisationOnly is the policy of this package's own router: only the realisation role has external
// profiles, and only in Hybrid mode. Every other role is served locally, whatever the operator lists.
func realisationOnly(inner modes.PolicySource, res *modes.Resolver) modes.PolicySource {
	return hybridPolicy{inner: inner, res: res}
}

type hybridPolicy struct {
	inner modes.PolicySource
	res   *modes.Resolver
}

// RolePolicy implements modes.PolicySource. A policy that cannot be read, or a mode that cannot be
// resolved, leaves the role local.
func (p hybridPolicy) RolePolicy(ctx context.Context, scope contracts.Scope, role modes.Role) (modes.RolePolicy, error) {
	if role != modes.RoleRealisation {
		return modes.RolePolicy{}, nil
	}
	r, err := p.res.Resolve(ctx, scope)
	if err != nil || r.Mode != modes.ModeHybrid {
		return modes.RolePolicy{}, nil
	}
	return p.inner.RolePolicy(ctx, scope, role)
}
