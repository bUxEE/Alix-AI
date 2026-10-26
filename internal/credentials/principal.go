package credentials

import (
	"context"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// PrincipalCredentialTenant is the reserved credential-ref.binding.tenant_id for principal-owned
// credentials (ADR 0041). It is not a company tenant and must never appear as a request scope.
const PrincipalCredentialTenant = "alixai-principal-credential"

// IsPrincipalCredentialTenant reports whether tenant is the reserved principal-credential bucket.
func IsPrincipalCredentialTenant(tenant string) bool {
	return tenant == PrincipalCredentialTenant
}

// RefuseReservedTenant returns a typed scope_violation when tenant is the reserved principal
// credential bucket. Call at every place a tenant id enters as a request or policy scope.
func RefuseReservedTenant(tenant string) error {
	if !IsPrincipalCredentialTenant(tenant) {
		return nil
	}
	return contracts.New(contracts.ErrScopeViolation, "credentials: the reserved principal-credential tenant is not a request scope").
		WithDetails("reason", "reserved_tenant")
}

// principalStoreScope rewrites the tenant to the reserved bucket while keeping the caller's
// principal and application. Used only for create/list/revoke of principal-owned credentials.
func principalStoreScope(scope contracts.Scope) contracts.Scope {
	out := scope
	out.TenantID = PrincipalCredentialTenant
	return out
}

// AddPrincipalAPIKey stores a manual key owned by the caller's principal in the reserved tenant
// bucket. The key is validated like AddAPIKey; OwnerPrincipal is always the caller's principal.
func (s *Service) AddPrincipalAPIKey(ctx context.Context, scope contracts.Scope, req AddAPIKeyRequest) (providers.CredentialRef, error) {
	if err := scope.Validate(); err != nil {
		return providers.CredentialRef{}, err
	}
	if scope.PrincipalID == "" {
		return providers.CredentialRef{}, fail(contracts.ErrScopeMissing, ReasonBadRequest, "credentials: a principal is required")
	}
	req.OwnerPrincipal = scope.PrincipalID
	return s.AddAPIKey(ctx, principalStoreScope(scope), req)
}

// AddPrincipalOAuth stores an OAuth grant owned by the caller's principal in the reserved bucket.
func (s *Service) AddPrincipalOAuth(ctx context.Context, scope contracts.Scope, g OAuthGrant) (providers.CredentialRef, error) {
	if err := scope.Validate(); err != nil {
		return providers.CredentialRef{}, err
	}
	if scope.PrincipalID == "" {
		return providers.CredentialRef{}, fail(contracts.ErrScopeMissing, ReasonBadRequest, "credentials: a principal is required")
	}
	g.OwnerPrincipal = scope.PrincipalID
	return s.AddOAuth(ctx, principalStoreScope(scope), g)
}

// AddPrincipalOAuthIssuedKey stores a browser-issued key owned by the caller's principal.
func (s *Service) AddPrincipalOAuthIssuedKey(ctx context.Context, scope contracts.Scope, g OAuthIssuedKeyGrant) (providers.CredentialRef, error) {
	if err := scope.Validate(); err != nil {
		return providers.CredentialRef{}, err
	}
	if scope.PrincipalID == "" {
		return providers.CredentialRef{}, fail(contracts.ErrScopeMissing, ReasonBadRequest, "credentials: a principal is required")
	}
	g.OwnerPrincipal = scope.PrincipalID
	return s.AddOAuthIssuedKey(ctx, principalStoreScope(scope), g)
}

// ListPrincipal returns the caller's own credentials for provider (or every provider when empty)
// from the reserved bucket. Another principal's rows are never returned.
func (s *Service) ListPrincipal(ctx context.Context, scope contracts.Scope, provider providers.ProviderID) ([]providers.CredentialRef, error) {
	if _, err := s.begin(scope); err != nil {
		return nil, err
	}
	if err := s.checkDeployment(scope); err != nil {
		return nil, err
	}
	rows, err := listRows(ctx, s.db, scope.ApplicationID, PrincipalCredentialTenant, provider)
	if err != nil {
		return nil, err
	}
	out := make([]providers.CredentialRef, 0, len(rows))
	for _, r := range rows {
		if r.ownerPrincipal != scope.PrincipalID {
			continue
		}
		out = append(out, s.view(r))
	}
	return out, nil
}

// ActivePrincipalRef returns the best active credential of the caller for provider in the
// reserved bucket, or found=false.
func (s *Service) ActivePrincipalRef(ctx context.Context, scope contracts.Scope, provider providers.ProviderID) (providers.CredentialRef, bool, error) {
	refs, err := s.ListPrincipal(ctx, scope, provider)
	if err != nil {
		return providers.CredentialRef{}, false, err
	}
	var best providers.CredentialRef
	found := false
	for _, r := range refs {
		if r.Status != providers.CredentialRefStatusActive {
			continue
		}
		if !found || string(r.UpdatedAt) > string(best.UpdatedAt) {
			best, found = r, true
		}
	}
	return best, found, nil
}

// RevokePrincipal revokes every reserved-bucket credential of the caller for provider (or all
// providers when empty). Returns how many were newly revoked.
func (s *Service) RevokePrincipal(ctx context.Context, scope contracts.Scope, provider providers.ProviderID) (int, error) {
	if _, err := s.begin(scope); err != nil {
		return 0, err
	}
	if err := s.checkDeployment(scope); err != nil {
		return 0, err
	}
	store := principalStoreScope(scope)
	rows, err := listRows(ctx, s.db, scope.ApplicationID, PrincipalCredentialTenant, provider)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if r.ownerPrincipal != scope.PrincipalID {
			continue
		}
		if r.ref.Status == providers.CredentialRefStatusRevoked {
			continue
		}
		if _, err := s.Revoke(ctx, store, r.ref.RefID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ErasePrincipalCredentials revokes every reserved-bucket credential owned by principal in the
// application (ADR 0041 / ADR 0036). Tenant of the request is ignored: ownership is by principal.
func (s *Service) ErasePrincipalCredentials(ctx context.Context, scope contracts.Scope, principal string) (int, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if principal == "" || !accountRE.MatchString(principal) {
		return 0, fail(contracts.ErrValidationFailed, ReasonBadRequest, "credentials: a principal is required")
	}
	if err := s.checkDeployment(scope); err != nil {
		return 0, err
	}
	rows, err := listRows(ctx, s.db, scope.ApplicationID, PrincipalCredentialTenant, "")
	if err != nil {
		return 0, err
	}
	store := principalStoreScope(scope)
	store.PrincipalID = principal
	n := 0
	for _, r := range rows {
		if r.ownerPrincipal != principal {
			continue
		}
		if r.ref.Status == providers.CredentialRefStatusRevoked {
			continue
		}
		if _, err := s.Revoke(ctx, store, r.ref.RefID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
