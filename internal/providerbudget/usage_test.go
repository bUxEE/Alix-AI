package providerbudget

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func TestUsageExportReconciles(t *testing.T) {
	g := newRig(t)
	const big = 100_000_000
	openai := g.put(providerBudget("openai", providers.ChannelInteractive, big, big))
	g.put(providerBudget("anthropic", providers.ChannelInteractive, big, big))
	ctx := context.Background()
	u1 := g.scope
	t2 := g.scopeOf(tApp, "ten_2", "wsp_9", "usr_9")
	run := func(scope contracts.Scope, provider string, units Units, a Actual) Settlement {
		t.Helper()
		r, err := g.reserve(scope, callOf(provider, units))
		if err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		if a.Outcome == "" {
			a.Outcome = providers.UsageRecordOutcomeSucceeded
		}
		s, err := g.svc.Settle(ctx, scope, r.ID, a)
		if err != nil {
			t.Fatalf("Settle: %v", err)
		}
		return s
	}

	run(u1, "openai", out(100), Actual{Metering: out(100)})                                                                                    // 200
	run(t2, "openai", out(100), Actual{Metering: out(100), ReportedMicroUnits: ptr(providers.MicroUnits(777))})                                // 777 reported
	run(u1, "openai", out(50), Actual{Outcome: providers.UsageRecordOutcomeUncertain, ErrorKind: ptr(providers.ProviderErrorKind("timeout"))}) // 100 at the bound
	run(u1, "anthropic", out(150), Actual{Metering: out(150)})                                                                                 // 300

	// reserved at 23:59, settled at 00:01: the spend counts in the day it was reserved
	g.clk.Set(time.Date(2026, 10, 15, 23, 59, 30, 0, time.UTC))
	r, err := g.reserve(u1, callOf("openai", out(100)))
	if err != nil {
		t.Fatal(err)
	}
	g.clk.Advance(2 * time.Minute)
	if _, err := g.svc.Settle(ctx, u1, r.ID, Actual{Outcome: providers.UsageRecordOutcomeSucceeded, Metering: out(100)}); err != nil { // 200
		t.Fatal(err)
	}
	run(u1, "openai", out(150), Actual{Metering: out(150)}) // 300, on Oct 16

	wide := UsageFilter{ApplicationWide: true}
	exp, err := g.svc.ExportUsage(ctx, g.operator(), wide)
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Rows) != 6 {
		t.Fatalf("%d rows, want 6", len(exp.Rows))
	}
	var openaiCharged int64
	for _, tt := range exp.Totals {
		if tt.ProviderID == "openai" {
			openaiCharged = int64(tt.ChargedMicroUnits)
			if tt.Records != 5 || tt.Uncertain != 1 || tt.ReservedMicroUnits != 200+200+100+200+300 ||
				tt.ActualMicroUnits != 200+777+200+300 { // the uncertain call has no known actual
				t.Fatalf("openai totals = %+v", tt)
			}
		}
	}
	if want := int64(200 + 777 + 100 + 200 + 300); openaiCharged != want {
		t.Fatalf("openai charged = %d, want %d", openaiCharged, want)
	}

	// JSON: every record validates against the schema and the document round-trips
	var js bytes.Buffer
	if err := exp.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var back UsageExport
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || len(back.Rows) != 6 || len(back.Totals) != len(exp.Totals) {
		t.Fatalf("JSON round trip: %v, %d rows", err, len(back.Rows))
	}
	for _, row := range back.Rows {
		assertValid(t, urnUsageRecord, row.Record)
	}

	// CSV: one line per call, and its charged column adds up to the totals
	var cs bytes.Buffer
	if err := exp.WriteCSV(&cs); err != nil {
		t.Fatal(err)
	}
	recs, err := csv.NewReader(&cs).ReadAll()
	if err != nil || len(recs) != 7 {
		t.Fatalf("CSV: %v, %d lines", err, len(recs))
	}
	col := map[string]int{}
	for i, h := range recs[0] {
		col[h] = i
	}
	sums := map[string]int64{}
	for _, line := range recs[1:] {
		n, err := strconv.ParseInt(line[col["charged_micro_units"]], 10, 64)
		if err != nil {
			t.Fatalf("charged cell %q: %v", line[col["charged_micro_units"]], err)
		}
		sums[line[col["provider_id"]]] += n
	}
	if sums["openai"] != openaiCharged || sums["anthropic"] != 300 {
		t.Fatalf("CSV sums = %v, want openai %d and anthropic 300", sums, openaiCharged)
	}

	// the export agrees with the settled spend the budget state shows
	if w := win(g.state(openai), monthly); int64(w.SpentMicroUnits) != openaiCharged {
		t.Fatalf("budget state spent %d, usage export charged %d", w.SpentMicroUnits, openaiCharged)
	}

	// reconciliation against the counters and an invoice, by the day the spend was charged to
	day1 := UsageFilter{ApplicationWide: true, From: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)}
	rec, err := g.svc.Reconcile(ctx, g.operator(), day1, []Invoice{{ProviderID: "openai", Currency: "USD", MicroUnits: 1267}})
	if err != nil {
		t.Fatal(err)
	}
	var got *ReconcileRow
	for i := range rec.Rows {
		if rec.Rows[i].ProviderID == "openai" {
			got = &rec.Rows[i]
		}
	}
	if got == nil || got.Records != 4 || got.ChargedMicroUnits != 200+777+100+200 {
		t.Fatalf("day 1 (the 23:59 call counts here, the Oct 16 one does not): %+v", got)
	}
	if got.LedgerMicroUnits == nil || *got.LedgerMicroUnits != got.ChargedMicroUnits || got.LedgerMatches == nil || !*got.LedgerMatches {
		t.Fatalf("the counters do not match the usage: %+v", got)
	}
	if got.DifferenceMicroUnits == nil || *got.DifferenceMicroUnits != 10 { // 1277 charged, invoice 1267
		t.Fatalf("difference = %v, want 10", got.DifferenceMicroUnits)
	}
	all, err := g.svc.Reconcile(ctx, g.operator(), UsageFilter{ApplicationWide: true, From: day1.From, To: time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range all.Rows {
		if r.LedgerMatches == nil || !*r.LedgerMatches {
			t.Fatalf("two days do not reconcile: %+v", r)
		}
	}

	// without the application-wide right a caller sees only its own tenant and workspace
	own, err := g.svc.ExportUsage(ctx, u1, UsageFilter{})
	if err != nil || len(own.Rows) != 5 {
		t.Fatalf("own export: %d rows, %v", len(own.Rows), err)
	}
	if _, err := g.svc.ExportUsage(ctx, u1, UsageFilter{TenantID: "ten_2"}); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("another tenant's usage without the right: %v", err)
	}
	// a tenant-wide export covers every workspace of the caller's tenant, and never another tenant
	sibling := g.scopeOf(tApp, tTenant, "wsp_2", tUser)
	if own, err := g.svc.ExportUsage(ctx, sibling, UsageFilter{}); err != nil || len(own.Rows) != 0 {
		t.Fatalf("another workspace's own export: %d rows, %v", len(own.Rows), err)
	}
	if tenant, err := g.svc.ExportUsage(ctx, sibling, UsageFilter{TenantWide: true}); err != nil || len(tenant.Rows) != 5 {
		t.Fatalf("the tenant-wide export: %d rows, %v; want the 5 of the tenant", len(tenant.Rows), err)
	}
	if _, err := g.svc.ExportUsage(ctx, u1, UsageFilter{TenantWide: true, TenantID: "ten_2"}); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("another tenant's usage in a tenant-wide export: %v", err)
	}
	other, err := g.svc.ExportUsage(ctx, g.scopeOf("app_other", tTenant, tWsp, tUser), UsageFilter{ApplicationWide: true})
	if err != nil || len(other.Rows) != 0 {
		t.Fatalf("another application's export: %d rows, %v", len(other.Rows), err)
	}
	if _, err := g.svc.ExportUsage(ctx, u1, UsageFilter{Channel: "nope"}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("bad filter: %v", err)
	}
}

func TestCSVCellsCannotRunAsFormulas(t *testing.T) {
	for in, want := range map[string]string{"=1+1": "'=1+1", "+x": "'+x", "-1": "'-1", "@a": "'@a", "plain": "plain", "": ""} {
		if got := csvCell(in); got != want {
			t.Errorf("csvCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBudgetRowsScoped(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	storetest.AssertScopedTables(t, g.db)

	pb := g.put(providerBudget("openai", providers.ChannelInteractive, 100_000, 100_000))
	tb := g.put(tenantBudget(tApp, tTenant, 100_000, 100_000))
	r := g.mustReserve(callOf("openai", out(10)))

	otherApp := g.scopeOf("app_other", tTenant, tWsp, tUser)
	otherTenant := g.scopeOf(tApp, "ten_2", tWsp, tUser)
	otherWsp := g.scopeOf(tApp, tTenant, "wsp_2", tUser)
	otherUser := g.scopeOf(tApp, tTenant, tWsp, "usr_2")

	// no other application, tenant, workspace or principal can settle or release the reservation
	for name, sc := range map[string]contracts.Scope{"application": otherApp, "tenant": otherTenant, "workspace": otherWsp, "principal": otherUser} {
		_, err := g.svc.Settle(ctx, sc, r.ID, Actual{Outcome: providers.UsageRecordOutcomeSucceeded})
		if ReasonOf(err) != "unknown_reservation" {
			t.Errorf("another %s settled: %v", name, err)
		}
		if err := g.svc.Release(ctx, sc, r.ID); ReasonOf(err) != "unknown_reservation" {
			t.Errorf("another %s released: %v", name, err)
		}
	}
	if w := win(g.state(pb), daily); w.ReservedMicroUnits != 20 {
		t.Fatalf("the reservation changed: %+v", w)
	}

	// budgets are an application's own
	if list, err := g.svc.ListBudgets(ctx, otherApp); err != nil || len(list) != 0 {
		t.Fatalf("another application lists %d budgets, %v", len(list), err)
	}
	if _, err := g.svc.State(ctx, otherApp, pb.BudgetID); ReasonOf(err) != "unknown_budget" {
		t.Fatalf("another application reads a budget state: %v", err)
	}
	if _, err := g.svc.PutBudget(ctx, otherApp, tenantBudget(tApp, tTenant, 1, 1)); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("a budget for another application: %v", err)
	}
	changed := pb
	changed.Limits.DailyMicroUnits = 1
	if _, err := g.svc.PutBudget(ctx, otherApp, changed); ReasonOf(err) != "unknown_budget" {
		t.Fatalf("another application changed a budget: %v", err)
	}
	// a tenant budget applies to its own tenant only
	for name, sc := range map[string]contracts.Scope{"application": otherApp, "tenant": otherTenant} {
		st, err := g.svc.States(ctx, sc, "openai", providers.ChannelInteractive)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range st {
			if s.BudgetID == tb.BudgetID {
				t.Errorf("a tenant budget applies to another %s", name)
			}
		}
	}
	// usage rows are the caller's own
	g.mustSettle(r, Actual{Metering: out(10)})
	if exp, err := g.svc.ExportUsage(ctx, otherTenant, UsageFilter{}); err != nil || len(exp.Rows) != 0 {
		t.Fatalf("another tenant exports %d rows, %v", len(exp.Rows), err)
	}
	// a scope with a missing level is refused everywhere
	bad := g.scope
	bad.TenantID = ""
	if _, err := g.reserve(bad, callOf("openai", out(1))); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Fatalf("Reserve without a tenant: %v", err)
	}
	if _, err := g.svc.ExportUsage(ctx, bad, UsageFilter{ApplicationWide: true}); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Fatalf("ExportUsage without a tenant: %v", err)
	}
	// every row of every table carries a non-empty application
	for _, table := range []string{"budgets", "counters", "holds", "rate_events", "reservations", "usage"} {
		if n := count(t, g.db, `SELECT COUNT(*) FROM providerbudget_`+table+` WHERE application_id <> ?`, tApp); n != 0 {
			t.Errorf("%s: %d rows of another application", table, n)
		}
	}
}

func TestPutBudgetValidationAndRevisions(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	cases := map[string]func(*providers.Budget){
		"provider budget naming a tenant":  func(b *providers.Budget) { b.Subject.TenantID = idp("ten_1") },
		"provider budget without provider": func(b *providers.Budget) { b.Subject.ProviderID = nil },
		"tenant budget naming a workspace": func(b *providers.Budget) {
			b.Subject = tenantBudget(tApp, tTenant, 1, 1).Subject
			b.Subject.WorkspaceID = idp("wsp_1")
		},
		"tenant budget without a tenant": func(b *providers.Budget) {
			b.Subject = tenantBudget(tApp, tTenant, 1, 1).Subject
			b.Subject.TenantID = nil
		},
		"principal budget without a principal": func(b *providers.Budget) {
			b.Subject = principalBudget(tApp, tTenant, tWsp, tUser, 1, 1).Subject
			b.Subject.PrincipalID = nil
		},
		"wildcard tenant": func(b *providers.Budget) {
			b.Subject = tenantBudget(tApp, "*", 1, 1).Subject
		},
		"warn 0":               func(b *providers.Budget) { b.WarnAtPercent = 0 },
		"warn 100":             func(b *providers.Budget) { b.WarnAtPercent = 100 },
		"negative daily limit": func(b *providers.Budget) { b.Limits.DailyMicroUnits = -1 },
		"limit above 2^53-1":   func(b *providers.Budget) { b.Limits.MonthlyMicroUnits = providers.MicroUnits(MaxAmount + 1) },
		"bad currency":         func(b *providers.Budget) { b.Currency = "usd" },
		"bad channel":          func(b *providers.Budget) { b.Channel = "other" },
		"empty rate limits":    func(b *providers.Budget) { b.RateLimits = &providers.BudgetRateLimits{} },
		"zero concurrency": func(b *providers.Budget) {
			b.RateLimits = &providers.BudgetRateLimits{ConcurrentRequests: ptr(int64(0))}
		},
		"empty quota": func(b *providers.Budget) { b.Quotas = &providers.BudgetQuotas{Requests: &providers.BudgetQuota{}} },
		"negative quota": func(b *providers.Budget) {
			b.Quotas = &providers.BudgetQuotas{Requests: &providers.BudgetQuota{Daily: ptr(providers.Counter(-1))}}
		},
		"provider id with a slash": func(b *providers.Budget) { b.Subject.ProviderID = ptr(providers.ProviderID("a/b")) },
	}
	for name, mutate := range cases {
		b := providerBudget("openai", providers.ChannelInteractive, 100, 100)
		mutate(&b)
		if _, err := g.svc.PutBudget(ctx, g.operator(), b); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: %v", name, err)
		}
	}
	if list, _ := g.svc.ListBudgets(ctx, g.operator()); len(list) != 0 {
		t.Fatalf("%d invalid budgets were stored", len(list))
	}

	b := providerBudget("openai", providers.ChannelInteractive, 100, 1000)
	b.Quotas = &providers.BudgetQuotas{Requests: &providers.BudgetQuota{Daily: ptr(providers.Counter(5))}}
	b.RateLimits = &providers.BudgetRateLimits{RequestsPerMin: ptr(int64(10))}
	b.UpdatedBy = "someone_else" // ignored: the core records the verified principal
	b.BudgetID, b.Revision = "bud_01J0000000000000000000000B", 99
	stored := g.put(withoutID(b))
	if stored.Revision != 1 || stored.UpdatedBy != "usr_admin" || stored.BudgetID == b.BudgetID {
		t.Fatalf("stored = %+v", stored)
	}
	assertValid(t, urnBudget, stored)
	if _, err := g.svc.PutBudget(ctx, g.operator(), withoutID(b)); contracts.CodeOf(err) != contracts.ErrConflict || ReasonOf(err) != "budget_exists" {
		t.Fatalf("a second budget for the same subject and channel: %v", err)
	}

	stored.Limits.DailyMicroUnits = 200
	next, err := g.svc.PutBudget(ctx, g.operator(), stored)
	if err != nil || next.Revision != 2 || next.Limits.DailyMicroUnits != 200 {
		t.Fatalf("update: %+v, %v", next, err)
	}
	if _, err := g.svc.PutBudget(ctx, g.operator(), stored); ReasonOf(err) != "stale_revision" {
		t.Fatalf("an update from a stale revision: %v", err)
	}
	moved := next
	moved.Channel = providers.ChannelBackground
	if _, err := g.svc.PutBudget(ctx, g.operator(), moved); ReasonOf(err) != "immutable_budget" {
		t.Fatalf("changing the channel: %v", err)
	}
	// A request naming another subject learns nothing of the budget, not even its revision.
	moved.Revision = 1
	if _, err := g.svc.PutBudget(ctx, g.operator(), moved); ReasonOf(err) != "immutable_budget" {
		t.Fatalf("changing the channel from a stale revision: %v", err)
	}
	if _, err := g.svc.PutBudget(ctx, g.operator(), providers.Budget{BudgetID: "bud_01J0000000000000000000000Z", Revision: 1,
		Subject: b.Subject, Channel: b.Channel, Currency: b.Currency, Limits: b.Limits, WarnAtPercent: 80}); ReasonOf(err) != "unknown_budget" {
		t.Fatalf("an unknown budget: %v", err)
	}
}

func withoutID(b providers.Budget) providers.Budget {
	b.BudgetID, b.Revision = "", 0
	return b
}

func TestConcurrentReservesNeverExceedLimit(t *testing.T) {
	g := newRig(t)
	b := g.put(providerBudget("openai", providers.ChannelInteractive, 1000, 1000))
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, refused := 0, 0
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := g.reserve(g.scope, callOf("openai", out(50))) // 100 each
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				admitted++
			case contracts.CodeOf(err) == contracts.ErrResourceExhausted:
				refused++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if admitted != 10 || refused != 14 {
		t.Fatalf("admitted %d, refused %d; want exactly the 10 that fit", admitted, refused)
	}
	if w := win(g.state(b), daily); w.ReservedMicroUnits != 1000 || w.RemainingMicroUnits != 0 {
		t.Fatalf("state: %+v", w)
	}
}
