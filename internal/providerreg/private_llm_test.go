package providerreg

import (
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestPrivateLLMTenantPinOverridesPrincipalCatalog(t *testing.T) {
	g := newRig(t)
	epID := providers.PrivateEndpointID("ple_01J0000000000000000000000A")
	_, err := g.reg.PutPrivateEndpoint(bg, g.scope, epID, providers.PrivateLlmEndpointUpdate{
		Origin: "http://127.0.0.1:8099", APIDialect: "openai-compatible",
		ModelIDs: []providers.ModelID{"m1"}, Roles: []providers.PrincipalLlmRole{providers.PrincipalLlmRoleConversationalController},
		OwnerKind: providers.PrivateEndpointOwnerKindShared,
		Lifecycle: ptrLC(providers.PrivateLlmLifecycleReady),
	})
	if err != nil {
		t.Fatal(err)
	}
	other := g.scopeOf(tApp, tTenant2, tWsp)
	other.PrincipalID = "usr_other"
	if _, err := g.reg.PutTenantPrivateLLMPin(bg, g.scope, tTenant, epID); err != nil {
		t.Fatal(err)
	}
	p := g.reg
	scopeTen := g.scope
	scopeOtherTen := other
	role := providers.PrincipalLlmRoleConversationalController
	if ep, _, ok, err := p.TenantPrivateEndpoint(bg, scopeTen, role); err != nil || !ok || ep.EndpointID != epID {
		t.Fatalf("tenant pin: %+v %v %v", ep, ok, err)
	}
	if _, _, ok, _ := p.TenantPrivateEndpoint(bg, scopeOtherTen, role); ok {
		t.Fatal("other tenant should not see pin")
	}
}

func TestPrincipalPrivateBindingFollowsAcrossTenants(t *testing.T) {
	g := newRig(t)
	epID := providers.PrivateEndpointID("ple_01J0000000000000000000000B")
	_, err := g.reg.PutPrivateEndpoint(bg, g.scope, epID, providers.PrivateLlmEndpointUpdate{
		Origin: "http://127.0.0.1:8098", APIDialect: "openai-compatible",
		ModelIDs: []providers.ModelID{"m1"}, Roles: []providers.PrincipalLlmRole{providers.PrincipalLlmRoleConversationalController},
		OwnerKind:        providers.PrivateEndpointOwnerKindPrincipal,
		OwnerPrincipalID: ptrID("usr_owner"),
		Lifecycle:        ptrLC(providers.PrivateLlmLifecycleReady),
	})
	if err != nil {
		t.Fatal(err)
	}
	principal := "usr_owner"
	if _, err := g.reg.PutPrincipalPrivateBindings(bg, g.scope, principal, epID, []providers.PrincipalLlmRole{providers.PrincipalLlmRoleConversationalController}); err != nil {
		t.Fatal(err)
	}
	scope2 := g.scopeOf(tApp, tTenant2, tWsp)
	scope2.PrincipalID = principal
	if ep, _, ok, err := g.reg.PrincipalPrivateEndpoint(bg, scope2, providers.PrincipalLlmRoleConversationalController); err != nil || !ok || ep.EndpointID != epID {
		t.Fatalf("follow principal: %+v %v %v", ep, ok, err)
	}
}

func TestEndpointOwnerPermitsUse(t *testing.T) {
	ep := PrivateEndpoint{OwnerKind: providers.PrivateEndpointOwnerKindPrincipal, OwnerPrincipalID: ptrID("alice")}
	if EndpointOwnerPermitsUse(contracts.Scope{PrincipalID: "bob"}, ep) == nil {
		t.Fatal("bob must not use alice endpoint")
	}
}

func TestErasePrivateBindings(t *testing.T) {
	g := newRig(t)
	epID := providers.PrivateEndpointID("ple_01J0000000000000000000000C")
	_, err := g.reg.PutPrivateEndpoint(bg, g.scope, epID, providers.PrivateLlmEndpointUpdate{
		Origin: "http://127.0.0.1:8097", APIDialect: "openai-compatible",
		ModelIDs: []providers.ModelID{"m1"}, Roles: []providers.PrincipalLlmRole{providers.PrincipalLlmRoleSTT},
		OwnerKind: providers.PrivateEndpointOwnerKindShared,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.reg.PutPrincipalPrivateBindings(bg, g.scope, tAdmin, epID, []providers.PrincipalLlmRole{providers.PrincipalLlmRoleSTT}); err != nil {
		t.Fatal(err)
	}
	n, err := g.reg.ErasePrincipalPrivateBindings(bg, g.scope, tAdmin)
	if err != nil || n != 1 {
		t.Fatalf("erase %d %v", n, err)
	}
}

func ptrLC(v providers.PrivateLlmLifecycle) *providers.PrivateLlmLifecycle { return &v }
func ptrID(v string) *common.Identifier                                    { id := common.Identifier(v); return &id }
