package modes

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

type fakePrivateEndpoints struct {
	tenant    map[pv.PrincipalLlmRole]providerreg.PrivateEndpoint
	principal map[pv.PrincipalLlmRole]providerreg.PrivateEndpoint
}

func (f fakePrivateEndpoints) TenantPrivateEndpoint(_ context.Context, _ contracts.Scope, role pv.PrincipalLlmRole) (providerreg.PrivateEndpoint, pv.ModelID, bool, error) {
	ep, ok := f.tenant[role]
	return ep, "m1", ok, nil
}

func (f fakePrivateEndpoints) PrincipalPrivateEndpoint(_ context.Context, _ contracts.Scope, role pv.PrincipalLlmRole) (providerreg.PrivateEndpoint, pv.ModelID, bool, error) {
	ep, ok := f.principal[role]
	return ep, "m1", ok, nil
}

func TestPrivatePolicyWarmingRefuses(t *testing.T) {
	scope := contracts.Scope{DeploymentID: "d", ApplicationID: "app", TenantID: "ten", WorkspaceID: "wsp", PrincipalID: "u", Roles: []string{"r"}, CapabilityScopes: []string{"c"}, SessionID: "s"}
	inner := StaticPolicy{RoleController: {Profiles: []pv.ProfileID{"pprf_01J0000000000000000000000A"}}}
	res := resolverExt(t)
	ep := providerreg.PrivateEndpoint{
		EndpointID: "ple_01J0000000000000000000000A", Lifecycle: pv.PrivateLlmLifecycleWarming,
		ProfileID: "pprf_01J0000000000000000000000B", OwnerKind: pv.PrivateEndpointOwnerKindShared,
	}
	p := PrivatePolicy{
		Inner: PrincipalPolicy{Inner: inner, Bindings: fakePrincipalBindings{}, Resolver: res, RequireConfigured: false},
		Endpoints: fakePrivateEndpoints{tenant: map[pv.PrincipalLlmRole]providerreg.PrivateEndpoint{
			pv.PrincipalLlmRoleConversationalController: ep,
		}},
		Resolver: res,
	}
	_, err := p.RolePolicy(context.Background(), scope, RoleController)
	if detailReason(err, "") != "private_llm_starting" {
		t.Fatalf("got %v", err)
	}
}

func TestPrivatePolicyFailedNoFallback(t *testing.T) {
	scope := contracts.Scope{DeploymentID: "d", ApplicationID: "app", TenantID: "ten", WorkspaceID: "wsp", PrincipalID: "u", Roles: []string{"r"}, CapabilityScopes: []string{"c"}, SessionID: "s"}
	def := pv.ProfileID("pprf_01J0000000000000000000000A")
	inner := StaticPolicy{RoleController: {Profiles: []pv.ProfileID{def}}}
	res := resolverExt(t)
	ep := providerreg.PrivateEndpoint{
		EndpointID: "ple_01J0000000000000000000000B", Lifecycle: pv.PrivateLlmLifecycleFailed,
		ProfileID: "pprf_01J0000000000000000000000C", OwnerKind: pv.PrivateEndpointOwnerKindShared,
	}
	p := PrivatePolicy{
		Inner: PrincipalPolicy{Inner: inner, Bindings: fakePrincipalBindings{pv.PrincipalLlmRoleConversationalController: def}, Resolver: res},
		Endpoints: fakePrivateEndpoints{tenant: map[pv.PrincipalLlmRole]providerreg.PrivateEndpoint{
			pv.PrincipalLlmRoleConversationalController: ep,
		}},
		Resolver: res,
	}
	_, err := p.RolePolicy(context.Background(), scope, RoleController)
	if detailReason(err, "") != "private_llm_unavailable" {
		t.Fatalf("got %v", err)
	}
}

func TestPrivatePolicyTenantPinWinsOverPrincipalBinding(t *testing.T) {
	scope := contracts.Scope{DeploymentID: "d", ApplicationID: "app", TenantID: "ten", WorkspaceID: "wsp", PrincipalID: "u", Roles: []string{"r"}, CapabilityScopes: []string{"c"}, SessionID: "s"}
	inner := StaticPolicy{RoleController: {Profiles: []pv.ProfileID{"pprf_01J0000000000000000000000A"}}}
	res := resolverExt(t)
	tenantEp := providerreg.PrivateEndpoint{
		EndpointID: "ple_01J0000000000000000000000D", Lifecycle: pv.PrivateLlmLifecycleReady,
		ProfileID: "pprf_01J0000000000000000000000T", OwnerKind: pv.PrivateEndpointOwnerKindShared,
	}
	principalEp := providerreg.PrivateEndpoint{
		EndpointID: "ple_01J0000000000000000000000E", Lifecycle: pv.PrivateLlmLifecycleReady,
		ProfileID: "pprf_01J0000000000000000000000P", OwnerKind: pv.PrivateEndpointOwnerKindShared,
	}
	p := PrivatePolicy{
		Inner: PrincipalPolicy{Inner: inner, Bindings: fakePrincipalBindings{}, Resolver: res},
		Endpoints: fakePrivateEndpoints{
			tenant:    map[pv.PrincipalLlmRole]providerreg.PrivateEndpoint{pv.PrincipalLlmRoleConversationalController: tenantEp},
			principal: map[pv.PrincipalLlmRole]providerreg.PrivateEndpoint{pv.PrincipalLlmRoleConversationalController: principalEp},
		},
		Resolver: res,
	}
	rp, err := p.RolePolicy(context.Background(), scope, RoleController)
	if err != nil || len(rp.Profiles) != 1 || rp.Profiles[0] != tenantEp.ProfileID {
		t.Fatalf("got %+v %v", rp, err)
	}
}

func resolverExt(t *testing.T) *Resolver {
	t.Helper()
	src := NewMemoryModes()
	scope := contracts.Scope{DeploymentID: "d", ApplicationID: "app", TenantID: "ten"}
	for _, lv := range []Level{LevelDeployment, LevelApplication, LevelTenant} {
		if err := src.Set(lv, scope, ModeExternal); err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewResolver(src, &fakeKS{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
