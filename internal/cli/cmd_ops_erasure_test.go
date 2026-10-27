package cli

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/retention"
	"github.com/buxee/alix-ai/pkg/contracts"
)

type opsErasureAuthFake struct {
	deployment map[string]bool
	tenant     map[string]bool
}

func (f *opsErasureAuthFake) AuthorizeOperator(_ context.Context, scope contracts.Scope, level gateway.ProvidersOperatorLevel) error {
	key := scope.ApplicationID + "/" + scope.TenantID + "/" + scope.PrincipalID
	switch level {
	case gateway.ProvidersOperatorDeployment:
		if f.deployment[scope.ApplicationID+"/"+scope.PrincipalID] {
			return nil
		}
		return contracts.New(contracts.ErrPermissionDenied, "not a deployment operator")
	default:
		if f.deployment[scope.ApplicationID+"/"+scope.PrincipalID] || f.tenant[key] {
			return nil
		}
		return contracts.New(contracts.ErrPermissionDenied, "not a tenant administrator")
	}
}

func TestOpsErasureRequestTenantScopedVsApplicationWide(t *testing.T) {
	auth := &opsErasureAuthFake{
		deployment: map[string]bool{"app/usr_ops": true},
		tenant:     map[string]bool{"app/ten_a/usr_admin_a": true},
	}
	adminA := contracts.Scope{ApplicationID: "app", TenantID: "ten_a", WorkspaceID: "ws", PrincipalID: "usr_admin_a"}
	ops := contracts.Scope{ApplicationID: "app", TenantID: "ten_a", WorkspaceID: "ws", PrincipalID: "usr_ops"}

	t.Run("tenant admin may start tenant-scoped principal erasure", func(t *testing.T) {
		req, err := opsErasureRequest(context.Background(), adminA, gateway.ErasureRequest{
			Level: gateway.ErasureLevelPrincipal, PrincipalID: "usr_x", Reason: "leave company A",
		}, auth)
		if err != nil {
			t.Fatal(err)
		}
		if req.Scope != retention.PrincipalScopeTenant || req.TenantID != "ten_a" {
			t.Fatalf("req = %+v", req)
		}
	})

	t.Run("tenant admin cannot start application-wide principal erasure", func(t *testing.T) {
		_, err := opsErasureRequest(context.Background(), adminA, gateway.ErasureRequest{
			Level: gateway.ErasureLevelPrincipal, Scope: gateway.ErasureScopeApplication,
			PrincipalID: "usr_x", Reason: "gdpr",
		}, auth)
		if !contracts.HasCode(err, contracts.ErrPermissionDenied) {
			t.Fatalf("err = %v, want permission_denied", err)
		}
	})

	t.Run("deployment operator may start application-wide principal erasure", func(t *testing.T) {
		req, err := opsErasureRequest(context.Background(), ops, gateway.ErasureRequest{
			Level: gateway.ErasureLevelPrincipal, Scope: gateway.ErasureScopeApplication,
			PrincipalID: "usr_x", Reason: "gdpr",
		}, auth)
		if err != nil {
			t.Fatal(err)
		}
		if req.Scope != retention.PrincipalScopeApplication || req.TenantID != "" {
			t.Fatalf("req = %+v", req)
		}
	})

	t.Run("tenant admin may start workspace-scoped principal erasure", func(t *testing.T) {
		req, err := opsErasureRequest(context.Background(), adminA, gateway.ErasureRequest{
			Level: gateway.ErasureLevelPrincipal, Scope: gateway.ErasureScopeWorkspace,
			PrincipalID: "usr_x", Reason: "leave site",
		}, auth)
		if err != nil {
			t.Fatal(err)
		}
		if req.Scope != retention.PrincipalScopeWorkspace || req.TenantID != "ten_a" || req.WorkspaceID != "ws" {
			t.Fatalf("req = %+v", req)
		}
	})
}
