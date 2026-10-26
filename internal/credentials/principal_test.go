package credentials

import (
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestPrincipalCredentialCrossTenantResolve(t *testing.T) {
	g := newRig(t)
	owner := "usr_plan_owner"
	g.scope.PrincipalID = owner

	ref, err := g.svc.AddPrincipalAPIKey(bg, g.scope, AddAPIKeyRequest{ProviderID: "fake", Key: Secret(tKey)})
	if err != nil {
		t.Fatal(err)
	}
	if string(ref.Binding.TenantID) != PrincipalCredentialTenant {
		t.Fatalf("tenant = %s, want reserved", ref.Binding.TenantID)
	}

	use := Use{Provider: "fake", Purpose: "inference"}
	otherTenant := g.scope
	otherTenant.TenantID = tTenant2
	if v, err := g.svc.Resolve(bg, otherTenant, string(ref.RefID), use); err != nil || v.Reveal() != tKey {
		t.Fatalf("same principal other tenant: %v %q", err, v.Reveal())
	}

	otherPrincipal := otherTenant
	otherPrincipal.PrincipalID = "usr_other"
	_, err = g.svc.Resolve(bg, otherPrincipal, string(ref.RefID), use)
	mustCode(t, err, contracts.ErrScopeViolation, ReasonPrincipalMismatch)

	admin := g.scope
	admin.PrincipalID = "usr_admin"
	_, err = g.svc.Get(bg, admin, ref.RefID)
	mustCode(t, err, contracts.ErrScopeViolation, ReasonPrincipalMismatch)
}

func TestRefuseReservedTenant(t *testing.T) {
	if err := RefuseReservedTenant("ten_acme"); err != nil {
		t.Fatal(err)
	}
	err := RefuseReservedTenant(PrincipalCredentialTenant)
	if contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("got %v", err)
	}
}

func TestOwnerPrincipalResolveGateAllowsReservedCrossTenant(t *testing.T) {
	// Existing SIWC-in-company-tenant behaviour: other tenant still NotBound.
	g := newRig(t)
	owner := "usr_plan_owner"
	g.scope.PrincipalID = owner
	use := Use{Provider: "fake", Purpose: "inference"}

	siwc, err := g.svc.AddOAuth(bg, g.scope, OAuthGrant{
		ProviderID: "fake", Scopes: []string{"chat.write"}, Access: Secret(tKey),
		OwnerPrincipal: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherTenant := g.scope
	otherTenant.TenantID = tTenant2
	_, err = g.svc.Resolve(bg, otherTenant, string(siwc.RefID), use)
	mustCode(t, err, contracts.ErrScopeViolation, ReasonNotBound)
}
