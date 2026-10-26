package providerreg

import (
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestCatalogAddListAndReservedTenantInGrants(t *testing.T) {
	g := newRig(t)
	p := g.qualified(credRef(1))
	entry, err := g.reg.CatalogAdd(bg, g.scope, CatalogEntryInput{
		ProfileID: p.ProfileID, DisplayName: "Test model", TermsSummary: "Test terms for the catalog.",
		BillingNoticeVersion: p.BillingNotice.Version, OAuthAvailable: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.AccountMode != "per_principal" || !entry.Qualified || entry.ProfileID != p.ProfileID {
		t.Fatalf("entry = %+v", entry)
	}
	cat, err := g.reg.Catalog(bg, g.scope)
	if err != nil || len(cat.Entries) != 1 {
		t.Fatalf("catalog = %+v %v", cat, err)
	}
	if err := g.reg.PutPrincipalBinding(bg, g.scope, PrincipalBinding{
		Role: pv.PrincipalLlmRoleConversationalController, ProfileID: p.ProfileID,
		ProviderID: p.ProviderID, ModelID: p.ModelID,
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := g.reg.PrincipalBindingFor(bg, g.scope, pv.PrincipalLlmRoleConversationalController)
	if err != nil || !ok || got.ProfileID != p.ProfileID {
		t.Fatalf("binding = %+v %v %v", got, ok, err)
	}
	other := g.scope
	other.PrincipalID = "usr_other"
	_, ok, err = g.reg.PrincipalBindingFor(bg, other, pv.PrincipalLlmRoleConversationalController)
	if err != nil || ok {
		t.Fatalf("other principal must not see binding: ok=%v err=%v", ok, err)
	}

	reserved := common.Identifier(reservedPrincipalCredentialTenant)
	draft := g.draft()
	draft.PermittedScopes = []pv.ProviderProfileScopeGrant{{ApplicationID: draft.PermittedScopes[0].ApplicationID, TenantID: &reserved}}
	if _, err := g.reg.Register(bg, g.scope, draft); !contracts.HasCode(err, contracts.ErrScopeViolation) {
		t.Fatalf("reserved tenant in permitted_scopes: %v", err)
	}
}
