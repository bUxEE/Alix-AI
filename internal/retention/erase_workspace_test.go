package retention

import (
	"testing"
)

func TestPrincipalWorkspaceScopeValidateAndTargetKey(t *testing.T) {
	req := Request{
		ApplicationID: "app", TenantID: "ten", WorkspaceID: "ws_a",
		PrincipalID: "usr_a", Level: LevelPrincipal, Scope: PrincipalScopeWorkspace,
		Reason: "left site", Operator: "usr_admin",
	}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	if req.EffectiveScope() != PrincipalScopeWorkspace {
		t.Fatalf("scope = %s", req.EffectiveScope())
	}
	missingWS := req
	missingWS.WorkspaceID = ""
	if err := missingWS.Validate(); err == nil {
		t.Fatal("workspace scope without workspace_id accepted")
	}
	tenant := req
	tenant.Scope = PrincipalScopeTenant
	tenant.WorkspaceID = ""
	if req.TargetKey() == tenant.TargetKey() {
		t.Fatal("workspace and tenant principal targets must differ")
	}
}
