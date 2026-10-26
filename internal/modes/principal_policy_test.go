package modes

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

type fakePrincipalBindings map[pv.PrincipalLlmRole]pv.ProfileID

func (f fakePrincipalBindings) PrincipalBindingFor(_ context.Context, _ contracts.Scope, role pv.PrincipalLlmRole) (pv.ProfileID, bool, error) {
	id, ok := f[role]
	return id, ok, nil
}

func TestPrincipalPolicyResolutionOrder(t *testing.T) {
	scope := contracts.Scope{
		DeploymentID: "dep", ApplicationID: "app", TenantID: "ten", WorkspaceID: "wsp",
		PrincipalID: "usr", Roles: []string{"r"}, CapabilityScopes: []string{"c"}, SessionID: "ses",
	}
	def := pv.ProfileID("pprf_01J0000000000000000000000A")
	bound := pv.ProfileID("pprf_01J0000000000000000000000B")
	inner := StaticPolicy{RoleController: {Profiles: []pv.ProfileID{def}}}

	resolver := func(t *testing.T, mode Mode) *Resolver {
		t.Helper()
		src := NewMemoryModes()
		for _, lv := range []Level{LevelDeployment, LevelApplication, LevelTenant} {
			if err := src.Set(lv, scope, mode); err != nil {
				t.Fatal(err)
			}
		}
		r, err := NewResolver(src, &fakeKS{})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("binding wins over default in external", func(t *testing.T) {
		p := PrincipalPolicy{Inner: inner, Bindings: fakePrincipalBindings{pv.PrincipalLlmRoleConversationalController: bound}, Resolver: resolver(t, ModeExternal)}
		rp, err := p.RolePolicy(context.Background(), scope, RoleController)
		if err != nil || len(rp.Profiles) != 1 || rp.Profiles[0] != bound {
			t.Fatalf("got %+v %v", rp, err)
		}
	})
	t.Run("default when no binding", func(t *testing.T) {
		p := PrincipalPolicy{Inner: inner, Bindings: fakePrincipalBindings{}, Resolver: resolver(t, ModeExternal)}
		rp, err := p.RolePolicy(context.Background(), scope, RoleController)
		if err != nil || len(rp.Profiles) != 1 || rp.Profiles[0] != def {
			t.Fatalf("got %+v %v", rp, err)
		}
	})
	t.Run("llm_not_configured when neither", func(t *testing.T) {
		p := PrincipalPolicy{Inner: StaticPolicy{}, Bindings: fakePrincipalBindings{}, Resolver: resolver(t, ModeExternal), RequireConfigured: true}
		_, err := p.RolePolicy(context.Background(), scope, RoleController)
		if detailReason(err, "") != "llm_not_configured" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("hybrid ignores binding", func(t *testing.T) {
		p := PrincipalPolicy{Inner: inner, Bindings: fakePrincipalBindings{pv.PrincipalLlmRoleConversationalController: bound}, Resolver: resolver(t, ModeHybrid)}
		rp, err := p.RolePolicy(context.Background(), scope, RoleController)
		if err != nil || len(rp.Profiles) != 1 || rp.Profiles[0] != def {
			t.Fatalf("got %+v %v", rp, err)
		}
	})
}
