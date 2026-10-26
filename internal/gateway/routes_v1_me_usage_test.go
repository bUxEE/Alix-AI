package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

type meUsageTestFake struct {
	mu       sync.Mutex
	calls    []string
	lastQ    MeUsageQuery
	usage    pv.MeUsage
	budget   pv.MeBudget
	otherPID string // if set, MeUsage would have returned this — isolation asserts it is never used
}

func (f *meUsageTestFake) MeUsage(_ context.Context, scope contracts.Scope, q MeUsageQuery) (pv.MeUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "MeUsage:"+scope.PrincipalID)
	f.lastQ = q
	if f.otherPID != "" && scope.PrincipalID == f.otherPID {
		return pv.MeUsage{}, contracts.New(contracts.ErrInternal, "must not read another principal")
	}
	out := f.usage
	if out.Groups == nil {
		out.Groups = []pv.MeUsageGroup{}
	}
	return out, nil
}

func (f *meUsageTestFake) MeBudget(_ context.Context, scope contracts.Scope) (pv.MeBudget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "MeBudget:"+scope.PrincipalID)
	out := f.budget
	if out.Budgets == nil {
		out.Budgets = []pv.Budget{}
	}
	if out.States == nil {
		out.States = []pv.BudgetState{}
	}
	for _, b := range out.Budgets {
		if b.Subject.PrincipalID != nil && string(*b.Subject.PrincipalID) != scope.PrincipalID {
			return pv.MeBudget{}, contracts.New(contracts.ErrInternal, "leaked another principal")
		}
	}
	return out, nil
}

var meUsageFakeSets sync.Map

func init() {
	registerTestServices("v1_me_usage", func(t testing.TB, e *apiEnv, s *Services) {
		pid := common.Identifier(e.scopeOf().PrincipalID)
		f := &meUsageTestFake{
			usage: pv.MeUsage{Groups: []pv.MeUsageGroup{{
				Day: "2026-10-26", ProviderID: "openai", Role: pv.RoleConversationalController,
				ToksIn: 10, ToksOut: 20, Requests: 1, Currency: "USD", ChargedMicroUnits: 40,
			}}},
			budget: pv.MeBudget{
				Budgets: []pv.Budget{{
					BudgetID: "bud_01J0000000000000000000000Z", Revision: 1,
					Subject: pv.BudgetSubject{Kind: pv.BudgetSubjectKindPrincipalApplication, ApplicationID: ptrIdent(e.scopeOf().ApplicationID), PrincipalID: &pid},
					Channel: pv.ChannelInteractive, Currency: "USD",
					Limits: pv.BudgetLimits{DailyMicroUnits: 1000, MonthlyMicroUnits: 10000}, WarnAtPercent: 80,
					UpdatedAt: "2026-10-26T00:00:00Z", UpdatedBy: "usr_admin",
				}},
				States: []pv.BudgetState{},
			},
		}
		meUsageFakeSets.Store(e, f)
		if err := Provide[MeUsageBudgetService](s, f); err != nil {
			t.Fatal(err)
		}
	})
}

func ptrIdent(s string) *common.Identifier {
	id := common.Identifier(s)
	return &id
}

func meUsageFakeOf(e *apiEnv) *meUsageTestFake {
	v, _ := meUsageFakeSets.Load(e)
	return v.(*meUsageTestFake)
}

func TestMeUsageRoutesMatchInventory(t *testing.T) {
	e := newAPIEnv(t)
	have := map[string]bool{}
	for _, r := range e.srv.AreaRoutes("v1_me_usage") {
		have[r] = true
	}
	for _, r := range []string{"GET /v1/me/usage", "GET /v1/me/budget"} {
		if !have[r] {
			t.Errorf("missing route %s (have %v)", r, e.srv.AreaRoutes("v1_me_usage"))
		}
	}
}

func TestMeUsageSelfOnlyAndQuery(t *testing.T) {
	e := newAPIEnv(t)
	f := meUsageFakeOf(e)

	rec := e.call(http.MethodGet, "/v1/me/usage", nil)
	wantStatus(t, rec, http.StatusOK)
	var page pv.MeUsage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Groups) != 1 || page.Groups[0].ProviderID != "openai" {
		t.Fatalf("page = %+v", page)
	}
	if len(f.calls) != 1 || f.calls[0] != "MeUsage:"+e.scopeOf().PrincipalID {
		t.Fatalf("calls = %v", f.calls)
	}

	wantStatus(t, e.call(http.MethodGet, "/v1/me/usage?tenant_id=ten_a&limit=10", nil), http.StatusOK)
	if f.lastQ.TenantID != "ten_a" || f.lastQ.Limit != 10 {
		t.Fatalf("query = %+v", f.lastQ)
	}
	wantError(t, e.call(http.MethodGet, "/v1/me/usage?foo=1", nil), http.StatusBadRequest, contracts.ErrValidationFailed)
	wantError(t, e.call(http.MethodGet, "/v1/me/usage?limit=0", nil), http.StatusBadRequest, contracts.ErrValidationFailed)
}

func TestMeBudgetSelfOnly(t *testing.T) {
	e := newAPIEnv(t)
	rec := e.call(http.MethodGet, "/v1/me/budget", nil)
	wantStatus(t, rec, http.StatusOK)
	var view pv.MeBudget
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Budgets) != 1 {
		t.Fatalf("budgets = %d", len(view.Budgets))
	}
	wantError(t, e.call(http.MethodGet, "/v1/me/budget?x=1", nil), http.StatusBadRequest, contracts.ErrValidationFailed)
}

func TestMeUsageUnavailableWithoutService(t *testing.T) {
	e := newAPIEnv(t)
	h := &meUsageHandler{apps: e.apps, svc: nil}
	scope := e.scopeOf()
	ctx, err := identity.WithScope(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.usage(rec, httptest.NewRequest(http.MethodGet, "/v1/me/usage", nil).WithContext(ctx))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("usage = %d, want 503 (%s)", rec.Code, rec.Body.String())
	}
}
