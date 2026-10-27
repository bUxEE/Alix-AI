package gateway

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

type analystFake struct {
	mu        sync.Mutex
	tenant    map[string]an.TenantSettings
	denied    bool
	writes    int
	eff       an.Abilities
	authRead  int
	authWrite int
}

func (f *analystFake) AuthorizeTenantRead(_ context.Context, _ contracts.Scope, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authRead++
	if f.denied {
		return contracts.New(contracts.ErrPermissionDenied, "denied")
	}
	return nil
}
func (f *analystFake) AuthorizeTenantWrite(_ context.Context, _ contracts.Scope, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authWrite++
	if f.denied {
		return contracts.New(contracts.ErrPermissionDenied, "denied")
	}
	return nil
}
func (f *analystFake) GetTenant(_ context.Context, _ contracts.Scope, tenantID string) (an.TenantSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tenant == nil {
		return an.TenantSettings{}, nil
	}
	return f.tenant[tenantID], nil
}
func (f *analystFake) PutTenant(_ context.Context, _ contracts.Scope, tenantID string, in an.TenantSettings) (an.TenantSettings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	ts := common.Timestamp(time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano))
	in.UpdatedAt = &ts
	if f.tenant == nil {
		f.tenant = map[string]an.TenantSettings{}
	}
	f.tenant[tenantID] = in
	return in, nil
}
func (f *analystFake) Effective(context.Context, contracts.Scope) (an.Abilities, error) {
	return f.eff, nil
}

func init() {
	registerTestServices("v1_analyst", func(t testing.TB, e *apiEnv, s *Services) {
		f := &analystFake{eff: an.Abilities{
			Enabled: false, AnalystCapable: true,
			Abilities:  an.AbilitiesAbilities{},
			ResolvedAt: common.Timestamp("2026-10-27T12:00:00.000Z"),
		}}
		if err := Provide[AnalystService](s, f); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAnalystRoutesRegistered(t *testing.T) {
	e := newAPIEnv(t)
	routes := map[string]bool{}
	for _, r := range e.srv.AreaRoutes("v1_analyst") {
		routes[r] = true
	}
	for _, r := range []string{
		"GET /v1/ops/tenants/{tenant_id}/analyst",
		"PUT /v1/ops/tenants/{tenant_id}/analyst",
		"GET /v1/me/analyst",
	} {
		if !routes[r] {
			t.Errorf("missing %s", r)
		}
	}
}

func TestAnalystOpsAuthzAndWrite(t *testing.T) {
	e := newAPIEnv(t)
	rec := e.call(http.MethodGet, "/v1/ops/tenants/"+apiTenant+"/analyst", nil, withScopes(defaultCaps...))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no capability: %d %s", rec.Code, rec.Body.String())
	}
	caps := append(append([]string{}, defaultCaps...), AnalystCapabilityRead, AnalystCapabilityWrite)
	rec = e.call(http.MethodPut, "/v1/ops/tenants/"+apiTenant+"/analyst", map[string]any{
		"abilities": map[string]any{"loop": false, "web_search": false},
	}, withScopes(caps...))
	if rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	rec = e.call(http.MethodGet, "/v1/ops/tenants/"+apiTenant+"/analyst", nil, withScopes(caps...))
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAnalystMeEffective(t *testing.T) {
	e := newAPIEnv(t)
	rec := e.call(http.MethodGet, "/v1/me/analyst", nil, withScopes(defaultCaps...))
	if rec.Code != http.StatusOK {
		t.Fatalf("me: %d %s", rec.Code, rec.Body.String())
	}
}
