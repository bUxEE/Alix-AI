package identity

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// ContextClaims are the context levels a credential asserts, below the
// deployment: the application the request is bound to, the tenant, workspace
// and principal, the principal's roles, the capability IDs the credential
// permits and the session the context is bound to.
//
// They are asserted values. NewScope is the one place they become a
// contracts.Scope, and only after contracts.Scope.Validate accepted them.
type ContextClaims struct {
	ApplicationID    string
	TenantID         string
	WorkspaceID      string
	PrincipalID      string
	Roles            []string
	CapabilityScopes []string
	SessionID        string
}

// NewScope builds the verified Scope for a request: the deployment level comes
// from deploymentID (deployment configuration, never from a credential) and the
// remaining levels from c. Duplicate roles and capability scopes are dropped,
// keeping first occurrences in order. The result is validated, so any missing
// level is scope_missing and any wildcard, padded, control-character or
// over-long value is scope_violation, and no default is ever supplied.
//
// Both the context-token Verifier and the peer-authenticated local IPC
// transport (M3-14) go through here, so every transport produces the same Scope.
func NewScope(deploymentID string, c ContextClaims) (contracts.Scope, error) {
	s := contracts.Scope{
		DeploymentID:     deploymentID,
		ApplicationID:    c.ApplicationID,
		TenantID:         c.TenantID,
		WorkspaceID:      c.WorkspaceID,
		PrincipalID:      c.PrincipalID,
		Roles:            dedupe(c.Roles),
		CapabilityScopes: dedupe(c.CapabilityScopes),
		SessionID:        c.SessionID,
	}
	if err := s.Validate(); err != nil {
		return contracts.Scope{}, err
	}
	// The reserved principal-credential tenant is a storage bucket only (ADR 0041): never a
	// request scope on a context token or peer-authenticated IPC session.
	if c.TenantID == reservedPrincipalCredentialTenant {
		return contracts.Scope{}, contracts.New(contracts.ErrScopeViolation,
			"identity: the reserved principal-credential tenant is not a request scope").
			WithDetails("reason", "reserved_tenant")
	}
	return s, nil
}

// reservedPrincipalCredentialTenant mirrors schemas/providers/shared reservedPrincipalCredentialTenant.
const reservedPrincipalCredentialTenant = "alixai-principal-credential"

func dedupe(in []string) []string {
	if len(in) < 2 {
		return append([]string(nil), in...)
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// CheckContinuity reports whether presented is the same authenticated context
// as bound, the scope a session was created under. A session cannot change
// deployment, application, tenant, workspace, principal or session without a
// new authenticated context (master plan §10.2): a different value at any of
// those levels is scope_violation naming the levels that changed. Roles and
// capability scopes may differ, because a fresh token legitimately narrows or
// widens what the same principal may do.
//
// Either scope being invalid is reported with its own contracts error.
func CheckContinuity(bound, presented contracts.Scope) error {
	if err := bound.Validate(); err != nil {
		return err
	}
	if err := presented.Validate(); err != nil {
		return err
	}
	var changed []string
	for _, lv := range []struct {
		level   contracts.ScopeLevel
		was, is string
	}{
		{contracts.LevelDeployment, bound.DeploymentID, presented.DeploymentID},
		{contracts.LevelApplication, bound.ApplicationID, presented.ApplicationID},
		{contracts.LevelTenant, bound.TenantID, presented.TenantID},
		{contracts.LevelWorkspace, bound.WorkspaceID, presented.WorkspaceID},
		{contracts.LevelPrincipal, bound.PrincipalID, presented.PrincipalID},
		{contracts.LevelSession, bound.SessionID, presented.SessionID},
	} {
		if lv.was != lv.is {
			changed = append(changed, string(lv.level))
		}
	}
	if len(changed) > 0 {
		return contracts.New(contracts.ErrScopeViolation,
			"the session is bound to another context; a new authenticated context needs a new session").
			WithDetails("levels", changed)
	}
	return nil
}

// Verified is the outcome of authenticating one credential.
type Verified struct {
	// Scope is the validated request context. It is the only identity the rest
	// of AlixAI uses.
	Scope contracts.Scope
	// Issuer and KeyID say whose key vouched for the credential.
	Issuer string
	KeyID  string
	// TokenID is the credential's single-use nonce (jti), for audit records.
	TokenID string
	// IssuedAt and ExpiresAt bound the credential's life.
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// Authenticator turns a presented credential into a Verified context. The
// context-token Verifier implements it; the peer-authenticated local IPC
// transport (M3-14) implements it too, so the HTTP middleware and every other
// consumer see one shape whatever the transport.
type Authenticator interface {
	Authenticate(ctx context.Context, credential string) (*Verified, error)
}
