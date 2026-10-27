package selfhosted

import (
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestTenantCacheSaltDiffersAcrossTenants(t *testing.T) {
	a := contracts.Scope{DeploymentID: "dep", ApplicationID: "app", TenantID: "ten_a", WorkspaceID: "w", PrincipalID: "u", Roles: []string{"r"}, CapabilityScopes: []string{"c"}, SessionID: "s"}
	b := a
	b.TenantID = "ten_b"
	sa, sb := tenantCacheSalt(a), tenantCacheSalt(b)
	if sa == "" || sb == "" {
		t.Fatal("expected salts")
	}
	if sa == sb {
		t.Fatal("tenants must not share cache_salt")
	}
	if tenantCacheSalt(a) != sa {
		t.Fatal("salt must be stable for a tenant")
	}
}
