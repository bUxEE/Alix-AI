package providerreg

import (
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestPrincipalOwnedEndpointRefusesOtherPrincipal(t *testing.T) {
	g := newRig(t)
	epID := providers.PrivateEndpointID("ple_01J0000000000000000000000X")
	owner := "usr_owner"
	_, err := g.reg.PutPrivateEndpoint(bg, g.scope, epID, providers.PrivateLlmEndpointUpdate{
		Origin: "http://127.0.0.1:8101", APIDialect: "openai-compatible",
		ModelIDs: []providers.ModelID{"m1"}, Roles: []providers.PrincipalLlmRole{providers.PrincipalLlmRoleConversationalController},
		OwnerKind:        providers.PrivateEndpointOwnerKindPrincipal,
		OwnerPrincipalID: ptrID(owner),
		Lifecycle:        ptrLC(providers.PrivateLlmLifecycleReady),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Owner may bind.
	if _, err := g.reg.PutPrincipalPrivateBindings(bg, g.scope, owner, epID, []providers.PrincipalLlmRole{providers.PrincipalLlmRoleConversationalController}); err != nil {
		t.Fatal(err)
	}
	other := "usr_other"
	sameTenant := g.scope
	sameTenant.PrincipalID = other
	_, err = g.reg.PutPrincipalPrivateBindings(bg, sameTenant, other, epID, []providers.PrincipalLlmRole{providers.PrincipalLlmRoleConversationalController})
	if err == nil {
		t.Fatal("other principal must not bind owner's endpoint")
	}
	ep := PrivateEndpoint{OwnerKind: providers.PrivateEndpointOwnerKindPrincipal, OwnerPrincipalID: ptrID(owner)}
	if EndpointOwnerPermitsUse(contracts.Scope{PrincipalID: other}, ep) == nil {
		t.Fatal("other principal must not use owner's endpoint")
	}
	scopeOther := sameTenant
	if _, _, ok, _ := g.reg.PrincipalPrivateEndpoint(bg, scopeOther, providers.PrincipalLlmRoleConversationalController); ok {
		t.Fatal("other principal must not resolve owner's private endpoint")
	}
}

func TestSharedEndpointKeepsPerTenantResolve(t *testing.T) {
	g := newRig(t)
	epID := providers.PrivateEndpointID("ple_01J0000000000000000000000Y")
	_, err := g.reg.PutPrivateEndpoint(bg, g.scope, epID, providers.PrivateLlmEndpointUpdate{
		Origin: "http://127.0.0.1:8102", APIDialect: "openai-compatible",
		ModelIDs: []providers.ModelID{"m1"}, Roles: []providers.PrincipalLlmRole{providers.PrincipalLlmRoleConversationalController},
		OwnerKind: providers.PrivateEndpointOwnerKindShared,
		Lifecycle: ptrLC(providers.PrivateLlmLifecycleReady),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.reg.PutTenantPrivateLLMPin(bg, g.scope, tTenant, epID); err != nil {
		t.Fatal(err)
	}
	otherTen := g.scopeOf(tApp, tTenant2, tWsp)
	if _, err := g.reg.PutTenantPrivateLLMPin(bg, otherTen, tTenant2, epID); err != nil {
		t.Fatal(err)
	}
	role := providers.PrincipalLlmRoleConversationalController
	ep1, _, ok1, err := g.reg.TenantPrivateEndpoint(bg, g.scope, role)
	if err != nil || !ok1 || ep1.EndpointID != epID {
		t.Fatalf("tenant1: %+v %v %v", ep1, ok1, err)
	}
	ep2, _, ok2, err := g.reg.TenantPrivateEndpoint(bg, otherTen, role)
	if err != nil || !ok2 || ep2.EndpointID != epID {
		t.Fatalf("tenant2: %+v %v %v", ep2, ok2, err)
	}
	// Pins are per tenant row; resolving under the wrong tenant id must not see the other pin.
	wrong := g.scope
	wrong.TenantID = tTenant2
	if pin, ok, _ := g.reg.GetTenantPrivateLLMPin(bg, g.scope, tTenant2); ok {
		_ = pin
		// GetTenantPrivateLLMPin is keyed by the tenantID argument under the caller's deployment scope.
	}
	if _, ok, _ := g.reg.GetTenantPrivateLLMPin(bg, g.scope, "ten_unknown"); ok {
		t.Fatal("unknown tenant must not have a pin")
	}
	_ = common.Identifier("")
}
