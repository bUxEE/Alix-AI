package modes

import (
	"context"

	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// PrivateProfileSource loads private endpoints by synthetic profile id.
type PrivateProfileSource interface {
	PrivateProfileFor(ctx context.Context, scope contracts.Scope, profileID pv.ProfileID, role pv.Role) (providerreg.PrivateEndpoint, pv.ModelID, bool, error)
}

// PrivateProfiles wraps inner profile access for self-hosted synthetic profiles (ADR 0045).
type PrivateProfiles struct {
	Inner    Profiles
	Registry *providerreg.Registry
	Private  PrivateProfileSource
	Creds    PrincipalCredSource
}

// Get implements Profiles.
func (p PrivateProfiles) Get(ctx context.Context, scope contracts.Scope, id pv.ProfileID) (pv.ProviderProfile, error) {
	if p.Private == nil || p.Registry == nil {
		if p.Inner == nil {
			return pv.ProviderProfile{}, contracts.New(contracts.ErrInternal, "modes: private profiles need an inner profiles")
		}
		return p.Inner.Get(ctx, scope, id)
	}
	ep, model, ok, err := p.Private.PrivateProfileFor(ctx, scope, id, pv.RoleConversationalController)
	if err != nil || !ok {
		if p.Inner == nil {
			return pv.ProviderProfile{}, contracts.New(contracts.ErrInternal, "modes: private profiles need an inner profiles")
		}
		return p.Inner.Get(ctx, scope, id)
	}
	pr, _ := principalRoleToProviderRole(pv.PrincipalLlmRoleConversationalController)
	return providerreg.SynthesizeProfile(scope.ApplicationID, ep, model, pr), nil
}

// Check implements Profiles.
func (p PrivateProfiles) Check(ctx context.Context, scope contracts.Scope, id pv.ProfileID, req providerreg.Requirement) (providerreg.Admission, error) {
	if p.Private == nil || p.Registry == nil {
		if p.Inner == nil {
			return providerreg.Admission{}, contracts.New(contracts.ErrInternal, "modes: private profiles need an inner profiles")
		}
		return p.Inner.Check(ctx, scope, id, req)
	}
	ep, model, ok, err := p.Private.PrivateProfileFor(ctx, scope, id, req.Role)
	if err != nil {
		return providerreg.Admission{}, err
	}
	if !ok {
		if p.Inner == nil {
			return providerreg.Admission{}, contracts.New(contracts.ErrInternal, "modes: private profiles need an inner profiles")
		}
		return p.Inner.Check(ctx, scope, id, req)
	}
	if err := providerreg.EndpointOwnerPermitsUse(scope, ep); err != nil {
		return providerreg.Admission{}, err
	}
	prof := providerreg.SynthesizeProfile(scope.ApplicationID, ep, model, req.Role)
	if prof.ProfileID != id {
		if p.Inner == nil {
			return providerreg.Admission{}, contracts.New(contracts.ErrInternal, "modes: private profiles need an inner profiles")
		}
		return p.Inner.Check(ctx, scope, id, req)
	}
	return p.Registry.CheckPrivateProfile(ctx, scope, prof, req)
}

// Fallback implements Profiles.
func (p PrivateProfiles) Fallback(ctx context.Context, scope contracts.Scope, from pv.ProfileID, cond providerreg.FallbackCondition, req providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error) {
	if p.Inner == nil {
		return providerreg.FallbackRecord{}, providerreg.Admission{}, contracts.New(contracts.ErrInternal, "modes: private profiles need an inner profiles")
	}
	return p.Inner.Fallback(ctx, scope, from, cond, req)
}

func principalRoleToProviderRole(r pv.PrincipalLlmRole) (pv.Role, bool) {
	switch r {
	case pv.PrincipalLlmRoleConversationalController:
		return pv.RoleConversationalController, true
	case pv.PrincipalLlmRoleLanguageRealization:
		return pv.RoleLanguageRealization, true
	case pv.PrincipalLlmRoleSTT:
		return pv.RoleSTT, true
	case pv.PrincipalLlmRoleTTS:
		return pv.RoleTTS, true
	}
	return "", false
}
