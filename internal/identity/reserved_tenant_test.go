package identity_test

import (
	"errors"
	"testing"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestNewScopeRefusesReservedPrincipalCredentialTenant(t *testing.T) {
	_, err := identity.NewScope("dep_main", identity.ContextClaims{
		ApplicationID: "app_a", TenantID: "alixai-principal-credential", WorkspaceID: "wsp_a",
		PrincipalID: "usr_a", Roles: []string{"user"}, CapabilityScopes: []string{"c"}, SessionID: "ses_a",
	})
	if !contracts.HasCode(err, contracts.ErrScopeViolation) {
		t.Fatalf("got %v, want scope_violation", err)
	}
	var ce *contracts.Error
	if !errors.As(err, &ce) || ce.Details["reason"] != "reserved_tenant" {
		t.Fatalf("details = %#v", ce)
	}
}
