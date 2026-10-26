package providerbudget

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func principalAppBudget(app, user string, daily, monthly int64) providers.Budget {
	b := tenantBudget(app, "x", daily, monthly)
	b.Subject = providers.BudgetSubject{
		Kind: providers.BudgetSubjectKindPrincipalApplication, ApplicationID: idp(app), PrincipalID: idp(user),
	}
	return b
}

func TestApplicationWidePrincipalBudgetAcrossTenants(t *testing.T) {
	g := newRig(t)
	const big = 10_000_000
	g.put(providerBudget("openai", providers.ChannelInteractive, big, big))
	ub := g.put(principalAppBudget(tApp, "usr_1", 1200, big)) // 2 calls of 600, third refused

	call := func() Call { return callOf("openai", out(300)) } // 600 micro-units
	t1 := g.scopeOf(tApp, tTenant, tWsp, "usr_1")
	t2 := g.scopeOf(tApp, "ten_2", "wsp_a", "usr_1")
	other := g.scopeOf(tApp, tTenant, tWsp, "usr_2")

	settle := func(scope contracts.Scope) {
		t.Helper()
		g.mustSettleScope(scope, g.mustReserveScope(scope, call()), Actual{Metering: out(300)})
	}
	settle(t1)
	settle(t2)
	_, err := g.reserve(t1, call())
	ce := wantCode(t, err, contracts.ErrResourceExhausted, ReasonBudgetExhausted)
	if ce.Details["budget_id"] != string(ub.BudgetID) {
		t.Fatalf("refused by %v, want app-wide principal %s", ce.Details["budget_id"], ub.BudgetID)
	}
	// Another principal is not under usr_1's application-wide budget.
	if _, err := g.reserve(other, call()); err != nil {
		t.Fatalf("other principal: %v", err)
	}
}

func TestMeUsageIsolationAndTenantFilter(t *testing.T) {
	g := newRig(t)
	const big = 10_000_000
	g.put(providerBudget("openai", providers.ChannelInteractive, big, big))
	ctx := context.Background()
	u1t1 := g.scopeOf(tApp, tTenant, tWsp, "usr_1")
	u1t2 := g.scopeOf(tApp, "ten_2", "wsp_a", "usr_1")
	u2 := g.scopeOf(tApp, tTenant, tWsp, "usr_2")

	for _, scope := range []contracts.Scope{u1t1, u1t2, u2} {
		g.mustSettleScope(scope, g.mustReserveScope(scope, callOf("openai", out(100))), Actual{Metering: out(100)})
	}

	page, err := g.svc.MeUsage(ctx, u1t1, MeUsageQuery{})
	if err != nil {
		t.Fatalf("MeUsage: %v", err)
	}
	// Without a tenant filter, groups collapse across tenants for the same day/provider/role.
	if len(page.Groups) != 1 {
		t.Fatalf("groups = %d, want 1 (collapsed across tenants)", len(page.Groups))
	}
	if page.Groups[0].Requests != 2 || page.Groups[0].ChargedMicroUnits != 400 {
		t.Fatalf("group = %+v, want 2 requests / 400 charged", page.Groups[0])
	}
	page, err = g.svc.MeUsage(ctx, u1t1, MeUsageQuery{TenantID: tTenant})
	if err != nil {
		t.Fatalf("MeUsage tenant: %v", err)
	}
	if len(page.Groups) != 1 || page.Groups[0].Requests != 1 {
		t.Fatalf("tenant filter groups = %+v", page.Groups)
	}
	page, err = g.svc.MeUsage(ctx, u2, MeUsageQuery{})
	if err != nil {
		t.Fatalf("MeUsage u2: %v", err)
	}
	if len(page.Groups) != 1 || page.Groups[0].Requests != 1 {
		t.Fatalf("u2 must not see usr_1: %+v", page.Groups)
	}
}

func TestMeBudgetOwnOnly(t *testing.T) {
	g := newRig(t)
	const big = 10_000_000
	g.put(providerBudget("openai", providers.ChannelInteractive, big, big))
	mine := g.put(principalAppBudget(tApp, "usr_1", 5000, big))
	g.put(principalBudget(tApp, tTenant, tWsp, "usr_1", 4000, big))
	g.put(principalAppBudget(tApp, "usr_2", 3000, big))

	ctx := context.Background()
	view, err := g.svc.MeBudget(ctx, g.scopeOf(tApp, tTenant, tWsp, "usr_1"))
	if err != nil {
		t.Fatalf("MeBudget: %v", err)
	}
	if len(view.Budgets) != 2 || len(view.States) != 2 {
		t.Fatalf("budgets=%d states=%d, want 2 each", len(view.Budgets), len(view.States))
	}
	for _, b := range view.Budgets {
		if b.Subject.PrincipalID == nil || string(*b.Subject.PrincipalID) != "usr_1" {
			t.Fatalf("leaked budget %+v", b.Subject)
		}
	}
	_ = mine
}
