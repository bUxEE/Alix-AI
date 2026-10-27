//go:build !noexternal

package cli

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestPrivateLLMAuthorizeWritePinRoles(t *testing.T) {
	s := &privateLLMService{}
	scope := contracts.Scope{
		DeploymentID: "dep_1", ApplicationID: "app_1", TenantID: "ten_a", WorkspaceID: "w",
		PrincipalID: "usr_a", Roles: []string{"r"}, CapabilityScopes: []string{"c"}, SessionID: "s",
	}
	if err := s.AuthorizeWrite(context.Background(), scope, "ten_a"); err == nil {
		t.Fatal("nil admin must deny")
	}

	dep := &externalAdmin{
		deployment: "dep_1",
		deployOps:  map[string]bool{externalOperatorKey("app_1", "usr_ops"): true},
	}
	depSvc := &privateLLMService{admin: dep}
	opScope := scope
	opScope.PrincipalID = "usr_ops"
	if err := depSvc.AuthorizeWrite(context.Background(), opScope, "ten_other"); err != nil {
		t.Fatalf("deployment operator: %v", err)
	}

	ten := &externalAdmin{
		deployment: "dep_1",
		tenantOps:  map[string]bool{externalAdminKey("app_1", "ten_a", "usr_admin"): true},
	}
	tenSvc := &privateLLMService{admin: ten}
	adminScope := scope
	adminScope.PrincipalID = "usr_admin"
	if err := tenSvc.AuthorizeWrite(context.Background(), adminScope, "ten_a"); err != nil {
		t.Fatalf("tenant admin same tenant: %v", err)
	}
	if err := tenSvc.AuthorizeWrite(context.Background(), adminScope, "ten_b"); err == nil {
		t.Fatal("tenant admin must not pin another tenant")
	}

	plain := &externalAdmin{deployment: "dep_1"}
	plainSvc := &privateLLMService{admin: plain}
	if err := plainSvc.AuthorizeWrite(context.Background(), scope, "ten_a"); err == nil {
		t.Fatal("plain principal must not pin")
	}
}
