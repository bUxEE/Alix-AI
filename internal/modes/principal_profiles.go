package modes

import (
	"context"

	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// PrincipalCredSource resolves the caller's active credential ref for a provider (reserved bucket).
type PrincipalCredSource interface {
	ActivePrincipalCred(ctx context.Context, scope contracts.Scope, provider pv.ProviderID) (pv.CredentialRefID, bool, error)
}

// PrincipalBindingLookup returns the binding for a principal LLM role.
type PrincipalBindingLookup interface {
	PrincipalBindingFor(ctx context.Context, scope contracts.Scope, role pv.PrincipalLlmRole) (providerreg.PrincipalBinding, bool, error)
}

// PrincipalProfiles wraps a Profiles so a principal-bound profile uses the principal's
// credential for Check (ADR 0041). Denied credentials end selection; no alternate is tried.
type PrincipalProfiles struct {
	Inner    Profiles
	Registry *providerreg.Registry
	Bindings PrincipalBindingLookup
	Creds    PrincipalCredSource
}

// Get implements Profiles.
func (p PrincipalProfiles) Get(ctx context.Context, scope contracts.Scope, id pv.ProfileID) (pv.ProviderProfile, error) {
	if p.Inner == nil {
		return pv.ProviderProfile{}, contracts.New(contracts.ErrInternal, "modes: principal profiles need an inner profiles")
	}
	return p.Inner.Get(ctx, scope, id)
}

// Check implements Profiles.
func (p PrincipalProfiles) Check(ctx context.Context, scope contracts.Scope, id pv.ProfileID, req providerreg.Requirement) (providerreg.Admission, error) {
	if p.Registry == nil || p.Bindings == nil || p.Creds == nil || p.Inner == nil {
		if p.Inner == nil {
			return providerreg.Admission{}, contracts.New(contracts.ErrInternal, "modes: principal profiles need an inner profiles")
		}
		return p.Inner.Check(ctx, scope, id, req)
	}
	role, ok := principalLlmRoleFromReq(req.Role)
	if !ok {
		return p.Inner.Check(ctx, scope, id, req)
	}
	b, found, err := p.Bindings.PrincipalBindingFor(ctx, scope, role)
	if err != nil {
		return providerreg.Admission{}, err
	}
	if !found || b.ProfileID != id {
		return p.Inner.Check(ctx, scope, id, req)
	}
	cred, ok, err := p.Creds.ActivePrincipalCred(ctx, scope, b.ProviderID)
	if err != nil {
		return providerreg.Admission{}, err
	}
	if !ok {
		return providerreg.Admission{}, contracts.New(contracts.ErrPermissionDenied, "modes: the principal has no active credential for this provider").
			WithDetails("reason", string(providerreg.ReasonCredentialMissing))
	}
	return p.Registry.CheckWithCred(ctx, scope, id, req, cred)
}

// Fallback implements Profiles.
func (p PrincipalProfiles) Fallback(ctx context.Context, scope contracts.Scope, from pv.ProfileID, cond providerreg.FallbackCondition, req providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	if p.Inner == nil {
		return providerreg.FallbackRecord{}, providerreg.Admission{}, contracts.New(contracts.ErrInternal, "modes: principal profiles need an inner profiles")
	}
	return p.Inner.Fallback(ctx, scope, from, cond, req)
}

func principalLlmRoleFromReq(role pv.Role) (pv.PrincipalLlmRole, bool) {
	switch role {
	case pv.RoleConversationalController:
		return pv.PrincipalLlmRoleConversationalController, true
	case pv.RoleLanguageRealization:
		return pv.PrincipalLlmRoleLanguageRealization, true
	case pv.RoleSTT:
		return pv.PrincipalLlmRoleSTT, true
	case pv.RoleTTS:
		return pv.PrincipalLlmRoleTTS, true
	}
	return "", false
}
