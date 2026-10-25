package providerbudget

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

const (
	daily   = providers.BudgetStateWindowWindowDaily
	monthly = providers.BudgetStateWindowWindowMonthly
)

func TestProviderDailyAndMonthlyBudgetHardStop(t *testing.T) {
	g := newRig(t)
	b := g.put(providerBudget("openai", providers.ChannelInteractive, 1000, 2500))
	spend := func() {
		t.Helper()
		g.mustSettle(g.mustReserve(callOf("openai", out(250))), Actual{Metering: out(250)})
	}

	spend()
	spend() // 1000 of the 1000 daily
	_, err := g.reserve(g.scope, callOf("openai", out(250)))
	ce := wantCode(t, err, contracts.ErrResourceExhausted, ReasonBudgetExhausted)
	if ce.Details["window"] != "daily" || ce.Details["dimension"] != "cost" {
		t.Fatalf("details = %v, want the daily cost window", ce.Details)
	}
	if st := g.state(b); st.State != providers.BudgetStatusExhausted || win(st, daily).RemainingMicroUnits != 0 || win(st, daily).SpentMicroUnits != 1000 {
		t.Fatalf("state after the daily stop = %+v", st)
	}

	g.clk.Advance(24 * time.Hour) // a new UTC day: the daily window reopens
	spend()
	spend() // monthly 2000
	g.clk.Advance(24 * time.Hour)
	spend() // monthly 2500
	_, err = g.reserve(g.scope, callOf("openai", out(250)))
	ce = wantCode(t, err, contracts.ErrResourceExhausted, ReasonBudgetExhausted)
	if ce.Details["window"] != "monthly" {
		t.Fatalf("window = %v, want monthly (the day still has room)", ce.Details["window"])
	}
	st := g.state(b)
	if win(st, daily).RemainingMicroUnits != 500 || win(st, monthly).RemainingMicroUnits != 0 || st.State != providers.BudgetStatusExhausted {
		t.Fatalf("state at the monthly stop = %+v", st)
	}

	g.clk.Set(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) // a new month
	spend()
	if st := g.state(b); win(st, monthly).SpentMicroUnits != 500 || win(st, monthly).StartsAt != "2026-11-01T00:00:00Z" {
		t.Fatalf("state in the new month = %+v", st)
	}
}

func TestTenantWorkspacePrincipalBudgets(t *testing.T) {
	g := newRig(t)
	const big = 10_000_000
	pb := g.put(providerBudget("openai", providers.ChannelInteractive, big, big))
	tb := g.put(tenantBudget(tApp, tTenant, 4200, big))
	wb := g.put(workspaceBudget(tApp, tTenant, tWsp, 3000, big))
	ub := g.put(principalBudget(tApp, tTenant, tWsp, "usr_1", 1000, big))
	narrow := principalBudget(tApp, tTenant, tWsp, "usr_1", 1, big)
	narrow.Subject.ProviderID = ptr(providers.ProviderID("anthropic"))
	g.put(narrow)

	call := func() Call { return callOf("openai", out(300)) } // 600 micro-units
	u1, u2 := g.scope, g.scopeOf(tApp, tTenant, tWsp, "usr_2")
	u3 := g.scopeOf(tApp, tTenant, "wsp_2", "usr_3")
	other := g.scopeOf(tApp, "ten_2", tWsp, "usr_1")
	refusedBy := func(scope contracts.Scope, want providers.Budget) {
		t.Helper()
		_, err := g.reserve(scope, call())
		ce := wantCode(t, err, contracts.ErrResourceExhausted, ReasonBudgetExhausted)
		if ce.Details["budget_id"] != string(want.BudgetID) {
			t.Fatalf("refused by %v, want budget %s (%s)", ce.Details["budget_id"], want.BudgetID, want.Subject.Kind)
		}
	}
	ok := func(scope contracts.Scope) {
		t.Helper()
		if _, err := g.reserve(scope, call()); err != nil {
			t.Fatalf("Reserve as %s/%s/%s: %v", scope.TenantID, scope.WorkspaceID, scope.PrincipalID, err)
		}
	}

	ok(u1)
	refusedBy(u1, ub) // the principal's own 1000 is the tightest
	for range 4 {
		ok(u2) // another principal in the workspace is not under u1's budget
	}
	refusedBy(u2, wb) // the workspace's 3000 is full
	ok(u3)            // another workspace is not under that workspace's budget
	ok(u3)
	refusedBy(u3, tb) // the tenant's 4200 is full, across both workspaces
	ok(other)         // another tenant is not under it

	count := func(scope contracts.Scope, provider string) int {
		t.Helper()
		st, err := g.svc.States(context.Background(), scope, providers.ProviderID(provider), providers.ChannelInteractive)
		if err != nil {
			t.Fatalf("States: %v", err)
		}
		return len(st)
	}
	if n := count(u1, "openai"); n != 4 { // provider, tenant, workspace, principal
		t.Fatalf("u1 sees %d openai budgets, want 4", n)
	}
	if n := count(u1, "anthropic"); n != 4 { // tenant, workspace, principal, the narrowed principal budget
		t.Fatalf("u1 sees %d anthropic budgets, want 4", n)
	}
	if n := count(u3, "openai"); n != 2 { // provider, tenant
		t.Fatalf("u3 sees %d budgets, want 2", n)
	}
	if n := count(other, "openai"); n != 1 { // the provider budget only
		t.Fatalf("another tenant sees %d budgets, want 1", n)
	}
	_ = pb
}

func TestQuotasRequestsToksImagesAudioFiles(t *testing.T) {
	q := func(daily, monthly int64) *providers.BudgetQuota {
		bq := &providers.BudgetQuota{}
		if daily >= 0 {
			bq.Daily = ptr(providers.Counter(daily))
		}
		if monthly >= 0 {
			bq.Monthly = ptr(providers.Counter(monthly))
		}
		return bq
	}
	cases := []struct {
		unit   string
		quotas providers.BudgetQuotas
		units  Units
		ok     int // calls admitted before the quota stops the next
		window string
	}{
		{"requests", providers.BudgetQuotas{Requests: q(2, -1)}, Units{}, 2, "daily"},
		{"toks_in", providers.BudgetQuotas{ToksIn: q(1000, -1)}, Units{ToksIn: 600}, 1, "daily"},
		{"toks_out", providers.BudgetQuotas{ToksOut: q(800, -1)}, out(500), 1, "daily"},
		{"images_in", providers.BudgetQuotas{ImagesIn: q(2, -1)}, Units{ImagesIn: 1}, 2, "daily"},
		{"images_out", providers.BudgetQuotas{ImagesOut: q(-1, 1)}, Units{ImagesOut: 1}, 1, "monthly"},
		{"audio_seconds_in", providers.BudgetQuotas{AudioSecondsIn: q(-1, 120)}, Units{AudioSecondsIn: 60}, 2, "monthly"},
		{"file_bytes_in", providers.BudgetQuotas{FileBytesIn: q(5000, -1)}, Units{FileBytesIn: 3000}, 1, "daily"},
	}
	for _, c := range cases {
		t.Run(c.unit, func(t *testing.T) {
			g := newRig(t)
			b := tenantBudget(tApp, tTenant, 1_000_000_000, 1_000_000_000)
			b.Quotas = &c.quotas
			g.put(b)
			for i := 0; i < c.ok; i++ {
				if _, err := g.reserve(g.scope, callOf("openai", c.units)); err != nil {
					t.Fatalf("call %d: %v", i+1, err)
				}
			}
			_, err := g.reserve(g.scope, callOf("openai", c.units))
			ce := wantCode(t, err, contracts.ErrResourceExhausted, ReasonQuotaExhausted)
			if ce.Details["dimension"] != c.unit || ce.Details["window"] != c.window {
				t.Fatalf("details = %v, want %s in the %s window", ce.Details, c.unit, c.window)
			}
		})
	}

	t.Run("settling_returns_unused_quota", func(t *testing.T) {
		g := newRig(t)
		b := tenantBudget(tApp, tTenant, 1_000_000_000, 1_000_000_000)
		b.Quotas = &providers.BudgetQuotas{ToksIn: q(1000, -1)}
		g.put(b)
		r := g.mustReserve(callOf("openai", Units{ToksIn: 900}))
		if _, err := g.reserve(g.scope, callOf("openai", Units{ToksIn: 900})); err == nil {
			t.Fatal("a second 900 fits a 1000 quota")
		}
		g.mustSettle(r, Actual{Metering: Units{ToksIn: 100}}) // only 100 were really used
		g.mustReserve(callOf("openai", Units{ToksIn: 900}))
	})

	t.Run("a_quota_of_zero_allows_none", func(t *testing.T) {
		g := newRig(t)
		b := tenantBudget(tApp, tTenant, 1_000_000_000, 1_000_000_000)
		b.Quotas = &providers.BudgetQuotas{ImagesIn: q(0, -1)}
		g.put(b)
		g.mustReserve(callOf("openai", out(10))) // an unrelated unit is unaffected
		_, err := g.reserve(g.scope, callOf("openai", Units{ImagesIn: 1}))
		wantCode(t, err, contracts.ErrResourceExhausted, ReasonQuotaExhausted)
	})
}

func TestRateAndConcurrencyLimits(t *testing.T) {
	g := newRig(t)
	b := providerBudget("openai", providers.ChannelInteractive, 1_000_000, 1_000_000)
	b.RateLimits = &providers.BudgetRateLimits{RequestsPerMin: ptr(int64(3)), ConcurrentRequests: ptr(int64(2))}
	g.put(b)
	call := func() Call { return callOf("openai", out(10)) }

	r1 := g.mustReserve(call())
	g.mustReserve(call())
	_, err := g.reserve(g.scope, call())
	wantCode(t, err, contracts.ErrResourceExhausted, ReasonConcurrencyLimit)

	g.mustSettle(r1, Actual{Metering: out(10)}) // frees a slot
	r3 := g.mustReserve(call())
	if _, err := g.reserve(g.scope, call()); contracts.CodeOf(err) != contracts.ErrResourceExhausted || ReasonOf(err) != ReasonConcurrencyLimit {
		t.Fatalf("4th while 2 are open: %v", err)
	}
	// three requests have been admitted in this minute: the fourth is rate limited once none is open
	g.clk.Advance(20 * time.Second)
	for _, r := range []Reservation{r3} {
		g.mustSettle(r, Actual{Metering: out(10)})
	}
	g.mustSettle(Reservation{ID: g.openReservation(), Provider: "openai"}, Actual{Metering: out(10)})
	_, err = g.reserve(g.scope, call())
	ce := wantCode(t, err, contracts.ErrRateLimited, ReasonRateLimited)
	if ms, _ := ce.Details["retry_after_ms"].(int64); ms != 40_000 {
		t.Fatalf("retry_after_ms = %v, want 40000 (the oldest request leaves the minute in 40 s)", ce.Details["retry_after_ms"])
	}
	if !ce.Retryable {
		t.Fatal("a rate limit is retryable")
	}
	g.clk.Advance(41 * time.Second)
	g.mustReserve(call()) // the minute moved on
}

// openReservation returns the ID of the one reservation still open.
func (g *rig) openReservation() providers.ReservationID {
	g.t.Helper()
	var id string
	if err := g.db.QueryRowContext(context.Background(),
		`SELECT reservation_id FROM providerbudget_reservations WHERE state = 'reserved'`).Scan(&id); err != nil {
		g.t.Fatalf("one open reservation: %v", err)
	}
	return providers.ReservationID(id)
}

func TestReserveSettleRelease(t *testing.T) {
	g := newRig(t)
	b := g.put(providerBudget("openai", providers.ChannelInteractive, 10_000, 100_000))
	units := Units{ToksIn: 1000, ToksOut: 500}

	r := g.mustReserve(callOf("openai", units))
	if r.Estimated != 2000 { // 1000 in at 1 + 500 out at 2
		t.Fatalf("estimate = %d, want 2000", r.Estimated)
	}
	if st := g.state(b); win(st, daily).ReservedMicroUnits != 2000 || win(st, daily).SpentMicroUnits != 0 || win(st, daily).RemainingMicroUnits != 8000 {
		t.Fatalf("after reserve: %+v", win(g.state(b), daily))
	}

	s := g.mustSettle(r, Actual{Metering: Units{ToksIn: 800, ToksOut: 300}})
	if s.ChargedMicroUnits != 1400 {
		t.Fatalf("charged = %d, want 1400", s.ChargedMicroUnits)
	}
	rec := s.Record
	if rec.Cost.Basis != providers.UsageRecordCostBasisPriceTable || rec.Cost.ReservedMicroUnits != 2000 ||
		rec.Cost.ActualMicroUnits == nil || *rec.Cost.ActualMicroUnits != 1400 || rec.Metering.Requests != 1 ||
		rec.ReservationID == nil || *rec.ReservationID != r.ID || rec.Outcome != providers.UsageRecordOutcomeSucceeded {
		t.Fatalf("usage record = %+v", rec)
	}
	assertValid(t, urnUsageRecord, rec)
	if w := win(g.state(b), daily); w.SpentMicroUnits != 1400 || w.ReservedMicroUnits != 0 || w.RemainingMicroUnits != 8600 {
		t.Fatalf("after settle: %+v", w)
	}
	_, err := g.svc.Settle(context.Background(), g.scope, r.ID, Actual{Outcome: providers.UsageRecordOutcomeSucceeded})
	wantCode(t, err, contracts.ErrConflict, "already_settled")
	if err := g.svc.Release(context.Background(), g.scope, r.ID); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("releasing a settled reservation: %v", err)
	}

	// release: nothing is charged and the hold goes
	r2 := g.mustReserve(callOf("openai", units))
	if err := g.svc.Release(context.Background(), g.scope, r2.ID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := g.svc.Release(context.Background(), g.scope, r2.ID); err != nil {
		t.Fatalf("a second Release is idempotent: %v", err)
	}
	if w := win(g.state(b), daily); w.SpentMicroUnits != 1400 || w.ReservedMicroUnits != 0 {
		t.Fatalf("after release: %+v", w)
	}
	_, err = g.svc.Settle(context.Background(), g.scope, r2.ID, Actual{Outcome: providers.UsageRecordOutcomeSucceeded})
	wantCode(t, err, contracts.ErrConflict, "released")

	// a provider-reported cost beats the price table
	r3 := g.mustReserve(callOf("openai", units))
	s3 := g.mustSettle(r3, Actual{Metering: units, ReportedMicroUnits: ptr(providers.MicroUnits(1234))})
	if s3.ChargedMicroUnits != 1234 || s3.Record.Cost.Basis != providers.UsageRecordCostBasisProviderReported {
		t.Fatalf("reported cost: %+v", s3)
	}

	// an uncertain outcome settles at the reserved upper bound, cost basis none
	r4 := g.mustReserve(callOf("openai", units))
	s4 := g.mustSettle(r4, Actual{Outcome: providers.UsageRecordOutcomeUncertain, ErrorKind: ptr(providers.ProviderErrorKind("timeout"))})
	if s4.ChargedMicroUnits != 2000 || s4.Record.Cost.Basis != providers.UsageRecordCostBasisNone || s4.Record.Cost.ActualMicroUnits != nil ||
		s4.Record.MeteringBasis != providers.UsageRecordMeteringBasisEstimated {
		t.Fatalf("uncertain: %+v", s4)
	}
	assertValid(t, urnUsageRecord, s4.Record)

	// a failed call that consumed nothing costs nothing; a bad actual is refused before it changes anything
	r5 := g.mustReserve(callOf("openai", units))
	if _, err := g.svc.Settle(context.Background(), g.scope, r5.ID, Actual{Outcome: providers.UsageRecordOutcomeFailed}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("failed without an error kind: %v", err)
	}
	s5 := g.mustSettle(r5, Actual{Outcome: providers.UsageRecordOutcomeFailed, ErrorKind: ptr(providers.ProviderErrorKind("timeout"))})
	if s5.ChargedMicroUnits != 0 {
		t.Fatalf("failed call charged %d", s5.ChargedMicroUnits)
	}
	if w := win(g.state(b), daily); w.SpentMicroUnits != 1400+1234+2000 {
		t.Fatalf("spent = %d, want %d", w.SpentMicroUnits, 1400+1234+2000)
	}

	// spend that happened is recorded even past the reservation
	r6 := g.mustReserve(callOf("openai", Units{ToksOut: 10})) // reserved 20
	s6 := g.mustSettle(r6, Actual{Metering: Units{ToksOut: 1000}})
	if s6.ChargedMicroUnits != 2000 {
		t.Fatalf("overrun charged %d, want 2000", s6.ChargedMicroUnits)
	}
}

func TestUnsettledReservationReturnsAfterTimeout(t *testing.T) {
	ctx := context.Background()
	reg := store.NewRegistry()
	if err := reg.Register(Module, migrations); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "crash.db")
	open := func() *store.DB {
		db, err := store.Open(ctx, store.Options{Path: path, Migrations: reg})
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		return db
	}
	db := open()
	g := newRigOn(t, db)
	b := g.put(providerBudget("openai", providers.ChannelInteractive, 1000, 100_000))
	r := g.mustReserve(callOf("openai", out(500))) // the whole daily budget
	if _, err := g.reserve(g.scope, callOf("openai", out(10))); contracts.CodeOf(err) != contracts.ErrResourceExhausted {
		t.Fatalf("the budget is fully reserved: %v", err)
	}

	// the process dies without settling: only the database file is left
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2 := open()
	t.Cleanup(func() { _ = db2.Close() })
	g2 := newRigOn(t, db2)
	if st := g2.state(b); win(st, daily).ReservedMicroUnits != 1000 {
		t.Fatalf("the reservation did not survive the restart: %+v", st)
	}
	if _, err := g2.reserve(g2.scope, callOf("openai", out(10))); contracts.CodeOf(err) != contracts.ErrResourceExhausted {
		t.Fatalf("the restart gave the reserved amount back early: %v", err)
	}

	g2.clk.Advance(DefaultReservationTTL - time.Second)
	if _, err := g2.reserve(g2.scope, callOf("openai", out(10))); err == nil {
		t.Fatal("the reservation returned before its timeout")
	}
	g2.clk.Advance(time.Second) // the timeout
	if st := g2.state(b); win(st, daily).ReservedMicroUnits != 0 || win(st, daily).RemainingMicroUnits != 1000 {
		t.Fatalf("after the timeout: %+v", st)
	}
	if n, err := g2.svc.Sweep(ctx, g2.scope); err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want 1 reservation expired", n, err)
	}
	if n := count(t, db2, `SELECT COUNT(*) FROM providerbudget_holds`); n != 0 {
		t.Fatalf("%d holds left after the timeout", n)
	}
	r2, err := g2.reserve(g2.scope, callOf("openai", out(500)))
	if err != nil {
		t.Fatalf("the amount did not return after the timeout: %v", err)
	}

	// the late call did happen: settling it still records the spend
	s := g2.mustSettle(Reservation{ID: r.ID}, Actual{Metering: out(500)})
	if s.ChargedMicroUnits != 1000 {
		t.Fatalf("late settle charged %d", s.ChargedMicroUnits)
	}
	w := win(g2.state(b), daily)
	if w.SpentMicroUnits != 1000 || w.ReservedMicroUnits != 1000 {
		t.Fatalf("after the late settle: %+v", w)
	}
	_ = r2
}

func TestEstimateIsUpperBound(t *testing.T) {
	p := prices()
	// a rate that does not divide: 1 micro-unit per 3 toks, rounded up per unit
	odd := providers.PriceTable{Currency: "USD", Prices: providers.PriceTablePrices{
		ToksIn: &providers.PriceTablePrice{PerUnits: 3, MicroUnits: 1}, ToksOut: &providers.PriceTablePrice{PerUnits: 7, MicroUnits: 5},
	}}
	if got, _ := Cost(odd, Units{ToksIn: 1, ToksOut: 1}); got != 2 { // ceil(1/3) + ceil(5/7)
		t.Fatalf("Cost rounds each unit up: got %d, want 2", got)
	}
	// cached input is cheaper here and dearer there: the estimate takes the higher rate
	dear := p
	dear.Prices.ToksInCached = &providers.PriceTablePrice{PerUnits: 1000, MicroUnits: 3000}
	for name, table := range map[string]providers.PriceTable{"cached cheaper": p, "cached dearer": dear, "odd": odd} {
		for _, units := range []Units{
			{ToksIn: 1000, ToksOut: 500}, {ToksIn: 7, ToksOut: 13, ToksInCached: 3}, {ToksIn: 1, ToksOut: 1},
			{ToksIn: 123456, ToksOut: 98765, ImagesIn: 2, FileBytesIn: 4096},
		} {
			if table.Prices.ToksInCached == nil && units.ToksInCached > 0 || table.Prices.ImagesIn == nil && units.ImagesIn > 0 || table.Prices.FileBytesIn == nil && units.FileBytesIn > 0 {
				continue
			}
			est, err := Estimate(table, units)
			if err != nil {
				t.Fatalf("%s %+v: %v", name, units, err)
			}
			// every actual that stays inside the reserved units, with any split of cached input
			for _, num := range []int64{0, 1, 3, 5, 10} {
				actual := units
				actual.ToksIn = providers.Counter(int64(units.ToksIn) * num / 10)
				actual.ToksOut = providers.Counter(int64(units.ToksOut) * num / 10)
				actual.ToksInCached = providers.Counter(int64(units.ToksInCached) * num / 10)
				got, err := Cost(table, actual)
				if err != nil {
					t.Fatalf("%s actual %+v: %v", name, actual, err)
				}
				if got > est {
					t.Fatalf("%s: actual cost %d exceeds the estimate %d for %+v within %+v", name, got, est, actual, units)
				}
			}
		}
	}
	// the cached rate is higher: the estimate prices all input at it
	if est, _ := Estimate(dear, Units{ToksIn: 1000}); est != 3000 {
		t.Fatalf("estimate = %d, want 3000 (1000 input toks at the higher cached rate)", est)
	}

	// an unpriced unit cannot be estimated, so the request is not sent
	noImages := p
	noImages.Prices.ImagesIn = nil
	if _, err := Estimate(noImages, Units{ImagesIn: 1}); ReasonOf(err) != "unpriced_unit" {
		t.Fatalf("unpriced images_in: %v", err)
	}
	onlyCached := providers.PriceTable{Currency: "USD", Prices: providers.PriceTablePrices{ToksInCached: p.Prices.ToksInCached}}
	if _, err := Estimate(onlyCached, Units{ToksIn: 5}); ReasonOf(err) != "unpriced_unit" {
		t.Fatalf("toks_in with only a cached price: %v", err)
	}
	// a table with no per-request price bills nothing per request
	noReq := p
	noReq.Prices.Requests = nil
	if est, err := Estimate(noReq, Units{Requests: 5}); err != nil || est != 0 {
		t.Fatalf("unpriced requests: %d, %v", est, err)
	}
	// exact arithmetic: no float, no overflow
	huge := providers.PriceTable{Currency: "USD", Prices: providers.PriceTablePrices{
		ToksOut: &providers.PriceTablePrice{PerUnits: 1, MicroUnits: providers.MicroUnits(MaxAmount)},
	}}
	if _, err := Estimate(huge, Units{ToksOut: 2}); ReasonOf(err) != "cost_overflow" {
		t.Fatalf("2 x (2^53-1): %v", err)
	}
	if est, err := Estimate(huge, Units{ToksOut: 1}); err != nil || int64(est) != MaxAmount {
		t.Fatalf("1 x (2^53-1) = %d, %v", est, err)
	}
	if _, err := Estimate(providers.PriceTable{Currency: "usd"}, Units{}); ReasonOf(err) != "bad_price_table" {
		t.Fatalf("bad currency: %v", err)
	}

	// Reserve reserves the estimate, never less than the caller's own floor, and refuses a price table in another currency
	g := newRig(t)
	g.put(providerBudget("openai", providers.ChannelInteractive, 100_000, 100_000))
	r := g.mustReserve(callOf("openai", Units{ToksIn: 1000, ToksOut: 500}))
	if want, _ := Estimate(p, Units{ToksIn: 1000, ToksOut: 500, Requests: 1}); r.Estimated != want {
		t.Fatalf("reserved %d, estimate %d", r.Estimated, want)
	}
	c := callOf("openai", out(10))
	c.EstimateFloor = 5000
	if r := g.mustReserve(c); r.Estimated != 5000 {
		t.Fatalf("floor: reserved %d, want 5000", r.Estimated)
	}
	c = callOf("openai", out(10))
	c.Prices.Currency = "EUR"
	_, err := g.reserve(g.scope, c)
	wantCode(t, err, contracts.ErrValidationFailed, "currency_mismatch")
}

func TestWarningThenHardStop(t *testing.T) {
	g := newRig(t)
	b := g.put(providerBudget("openai", providers.ChannelInteractive, 1000, 10_000_000)) // warn at 80
	ctx := context.Background()

	r1 := g.mustReserve(callOf("openai", out(350))) // 700 of 1000
	if len(r1.Warnings) != 0 || g.state(b).State != providers.BudgetStatusOk {
		t.Fatalf("70%% warns: %+v / %+v", r1.Warnings, g.state(b))
	}
	r2 := g.mustReserve(callOf("openai", out(100))) // 900
	if len(r2.Warnings) != 1 || r2.Warnings[0].Window != daily || r2.Warnings[0].UsedMicroUnits != 900 {
		t.Fatalf("90%% did not warn: %+v", r2.Warnings)
	}
	if st := g.state(b); st.State != providers.BudgetStatusWarning || win(st, daily).State != providers.BudgetStatusWarning || win(st, monthly).State != providers.BudgetStatusOk {
		t.Fatalf("warning state: %+v", st)
	}
	r3 := g.mustReserve(callOf("openai", out(50))) // exactly 1000: still admitted, and warns
	if len(r3.Warnings) == 0 {
		t.Fatal("the last admitted call carries the warning")
	}
	st := g.state(b)
	if st.State != providers.BudgetStatusExhausted || win(st, daily).RemainingMicroUnits != 0 {
		t.Fatalf("at 100%%: %+v", st)
	}
	_, err := g.reserve(g.scope, callOf("openai", out(1)))
	wantCode(t, err, contracts.ErrResourceExhausted, ReasonBudgetExhausted)
	if _, err := g.reserve(g.scope, Call{}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("an empty call: %v", err)
	}

	if err := g.svc.Release(ctx, g.scope, r3.ID); err != nil {
		t.Fatal(err)
	}
	if st := g.state(b); st.State != providers.BudgetStatusWarning {
		t.Fatalf("after releasing the last call: %+v", st)
	}
	g.mustReserve(callOf("openai", out(50)))
}

func TestBackgroundBudgetSeparateFromInteractive(t *testing.T) {
	g := newRig(t)
	inter := g.put(providerBudget("openai", providers.ChannelInteractive, 1000, 100_000))
	back := g.put(providerBudget("openai", providers.ChannelBackground, 500, 100_000))
	call := func(ch providers.Channel, n int) Call {
		c := callOf("openai", out(n))
		c.Channel = ch
		return c
	}

	g.mustReserve(call(providers.ChannelBackground, 250)) // the whole background budget
	_, err := g.reserve(g.scope, call(providers.ChannelBackground, 1))
	ce := wantCode(t, err, contracts.ErrResourceExhausted, ReasonBudgetExhausted)
	if ce.Details["budget_id"] != string(back.BudgetID) {
		t.Fatalf("refused by %v, want the background budget", ce.Details["budget_id"])
	}
	if w := win(g.state(inter), daily); w.ReservedMicroUnits != 0 || w.RemainingMicroUnits != 1000 {
		t.Fatalf("background work touched the interactive budget: %+v", w)
	}
	g.mustReserve(call(providers.ChannelInteractive, 500)) // interactive is untouched by the background stop
	if w := win(g.state(back), daily); w.ReservedMicroUnits != 500 {
		t.Fatalf("interactive work touched the background budget: %+v", w)
	}

	// background work with no background budget cannot fall back to the interactive one
	g2 := newRig(t)
	g2.put(providerBudget("openai", providers.ChannelInteractive, 1_000_000, 1_000_000))
	_, err = g2.reserve(g2.scope, call(providers.ChannelBackground, 1))
	wantCode(t, err, contracts.ErrResourceExhausted, ReasonNoBudget)

	// the channel follows the job priority class (master plan 17.1)
	for c, want := range map[jobs.Class]providers.Channel{
		jobs.ClassAuthCommit: providers.ChannelInteractive, jobs.ClassTurn: providers.ChannelInteractive,
		jobs.ClassRAGQuery: providers.ChannelInteractive, jobs.ClassRead: providers.ChannelInteractive,
		jobs.ClassIngest: providers.ChannelBackground, jobs.ClassMaintenance: providers.ChannelBackground,
		jobs.Class(0): providers.ChannelBackground, jobs.Class(99): providers.ChannelBackground,
	} {
		if got := ChannelForClass(c); got != want {
			t.Errorf("ChannelForClass(%v) = %s, want %s", c, got, want)
		}
	}
}

func TestExhaustedBudgetIsTypedAndNoAlternateProvider(t *testing.T) {
	g := newRig(t)
	g.put(providerBudget("openai", providers.ChannelInteractive, 0, 0)) // nothing may be spent
	anth := g.put(providerBudget("anthropic", providers.ChannelInteractive, 1_000_000, 1_000_000))
	g.fallback["openai"] = true

	_, err := g.reserve(g.scope, callOf("openai", out(1)))
	ce := wantCode(t, err, contracts.ErrResourceExhausted, ReasonBudgetExhausted)
	if ce.Details["alternate_provider"] != "none" || ce.Details["local_fallback_available"] != true {
		t.Fatalf("details = %v", ce.Details)
	}
	st, ok := ce.Details["budget_state"].(providers.BudgetState)
	if !ok || st.State != providers.BudgetStatusExhausted || !st.LocalFallbackAvailable {
		t.Fatalf("budget_state detail = %#v", ce.Details["budget_state"])
	}
	assertValid(t, urnBudgetState, st)
	// the refusal took nothing, anywhere, and nothing was charged to the other provider
	if n := count(t, g.db, `SELECT COUNT(*) FROM providerbudget_reservations`) + count(t, g.db, `SELECT COUNT(*) FROM providerbudget_holds`); n != 0 {
		t.Fatalf("a refused call left %d rows", n)
	}
	if w := win(g.state(anth), daily); w.ReservedMicroUnits != 0 || w.SpentMicroUnits != 0 {
		t.Fatalf("the other provider's budget changed: %+v", w)
	}

	// no applicable budget is also a typed stop, never an unmetered call
	g.fallback["gemini"] = false
	_, err = g.reserve(g.scope, callOf("gemini", out(1)))
	ce = wantCode(t, err, contracts.ErrResourceExhausted, ReasonNoBudget)
	if ce.Details["alternate_provider"] != "none" || ce.Details["local_fallback_available"] != false {
		t.Fatalf("no_budget details = %v", ce.Details)
	}
	// the hint is only a hint: without a policy there is none
	_, err = g.reserve(g.scope, callOf("openai", out(1)))
	if ce := wantCode(t, err, contracts.ErrResourceExhausted, ReasonBudgetExhausted); ce.Details["local_fallback_available"] != true {
		t.Fatal("the hint follows the policy hook")
	}
	g.fallback["openai"] = false
	_, err = g.reserve(g.scope, callOf("openai", out(1)))
	if ce := wantCode(t, err, contracts.ErrResourceExhausted, ReasonBudgetExhausted); ce.Details["local_fallback_available"] != false {
		t.Fatal("no policy fallback, no hint")
	}
}

// failRowsDriver is a database/sql driver whose result sets yield one row and then fail, for the queries whose
// text contains failOn; the others yield one row and end.
type failRowsDriver struct{ failOn string }

type failRowsConn struct{ d failRowsDriver }

type failRowsStmt struct {
	d     failRowsDriver
	query string
}

type failRowsRows struct {
	n    int
	cols []string
	fail bool
}

func (d failRowsDriver) Open(string) (driver.Conn, error) { return failRowsConn{d}, nil }

func (d failRowsDriver) Connect(context.Context) (driver.Conn, error) { return failRowsConn{d}, nil }
func (d failRowsDriver) Driver() driver.Driver                        { return d }

func (c failRowsConn) Prepare(q string) (driver.Stmt, error) { return failRowsStmt{c.d, q}, nil }
func (failRowsConn) Close() error                            { return nil }
func (failRowsConn) Begin() (driver.Tx, error)               { return nil, errors.New("no transactions") }

func (failRowsStmt) Close() error  { return nil }
func (failRowsStmt) NumInput() int { return -1 }
func (failRowsStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("no writes")
}
func (s failRowsStmt) Query([]driver.Value) (driver.Rows, error) {
	cols := []string{"dimension", "window_kind", "used"}
	if strings.Contains(s.query, "providerbudget_holds") {
		cols = []string{"dimension", "sum"}
	}
	return &failRowsRows{cols: cols, fail: strings.Contains(s.query, s.d.failOn)}, nil
}

func (r *failRowsRows) Columns() []string { return r.cols }
func (*failRowsRows) Close() error        { return nil }
func (r *failRowsRows) Next(dest []driver.Value) error {
	r.n++
	switch {
	case r.n == 1:
		dest[0], dest[len(dest)-1] = dimCost, int64(1)
		if len(dest) == 3 {
			dest[1] = "daily"
		}
		return nil
	case r.fail:
		return errors.New("the read broke off")
	}
	return io.EOF
}

// applicableBeforeUnion is the statement applicable ran before it became one indexed branch per
// subject kind: the reference the new one must agree with, row for row and in order.
const applicableBeforeUnion = `SELECT tenant_id, workspace_id, document FROM providerbudget_budgets
	  WHERE application_id = ? AND channel = ? AND (
	        (kind = 'provider'  AND tenant_id = '' AND workspace_id = '' AND provider_id = ?)
	     OR (kind = 'tenant'    AND tenant_id = ? AND workspace_id = '' AND (provider_id = '' OR provider_id = ?))
	     OR (kind = 'workspace' AND tenant_id = ? AND workspace_id = ? AND (provider_id = '' OR provider_id = ?))
	     OR (kind = 'principal' AND tenant_id = ? AND workspace_id = ? AND principal_id = ? AND (provider_id = '' OR provider_id = ?)))
	  ORDER BY seq`

// Every call reads the budgets that apply to it, so each kind of subject must be found through the
// subject index, never by walking the application's budgets; and the budgets found, and their order,
// are those of the statement it replaced, for every mix of tenant, workspace, principal, provider,
// channel and narrowing.
func TestApplicableBudgetsUseTheSubjectIndexAndAgree(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	const big = 10_000_000
	on := func(b providers.Budget, ch providers.Channel, narrow string) providers.Budget {
		b.Channel = ch
		if narrow != "" && b.Subject.Kind != providers.BudgetSubjectKindProvider {
			b.Subject.ProviderID = ptr(providers.ProviderID(narrow))
		}
		return b
	}
	bg, in := providers.ChannelBackground, providers.ChannelInteractive
	for _, b := range []providers.Budget{
		on(principalBudget(tApp, tTenant, tWsp, tUser, big, big), in, ""),
		providerBudget("openai", in, big, big),
		on(workspaceBudget(tApp, tTenant, tWsp, big, big), in, "openai"),
		on(tenantBudget(tApp, tTenant, big, big), in, ""),
		on(principalBudget(tApp, tTenant, tWsp, tUser, big, big), in, "anthropic"),
		providerBudget("anthropic", in, big, big),
		on(tenantBudget(tApp, tTenant, big, big), in, "anthropic"),
		on(workspaceBudget(tApp, tTenant, tWsp, big, big), in, ""),
		on(tenantBudget(tApp, "ten_2", big, big), in, ""),
		providerBudget("openai", bg, big, big),
		on(workspaceBudget(tApp, tTenant, "wsp_2", big, big), in, ""),
		on(principalBudget(tApp, tTenant, tWsp, "usr_2", big, big), in, ""),
		on(tenantBudget(tApp, tTenant, big, big), bg, "openai"),
		on(principalBudget(tApp, tTenant, "wsp_2", tUser, big, big), in, "openai"),
		on(workspaceBudget(tApp, "ten_2", tWsp, big, big), bg, ""),
		on(principalBudget(tApp, "ten_2", tWsp, tUser, big, big), in, ""),
	} {
		g.put(b)
	}
	other := g.scopeOf("app_other", tTenant, tWsp, tUser)
	if _, err := g.svc.PutBudget(ctx, other, providerBudget("openai", in, big, big)); err != nil {
		t.Fatal(err)
	}
	if _, err := g.svc.PutBudget(ctx, other, tenantBudget("app_other", tTenant, big, big)); err != nil {
		t.Fatal(err)
	}

	plan := func(query string, args ...any) string {
		t.Helper()
		rows, err := g.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			out = append(out, detail)
		}
		return strings.Join(out, "\n")
	}
	p := plan(applicableSQL, applicableArgs(g.scope, "openai", in)...)
	t.Logf("plan:\n%s", p)
	if strings.Contains(p, "SCAN providerbudget_budgets") || strings.Count(p, "USING INDEX providerbudget_budgets_subject") != 4 {
		t.Errorf("the applicable budgets are not found through the subject index, once per kind:\n%s", p)
	}

	ids := func(query string, args ...any) []string {
		t.Helper()
		rows, err := g.db.QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			r, err := scanBudget(rows)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, string(r.b.BudgetID))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	scopes := []contracts.Scope{
		g.scope, g.scopeOf(tApp, tTenant, tWsp, "usr_2"), g.scopeOf(tApp, tTenant, "wsp_2", tUser),
		g.scopeOf(tApp, "ten_2", tWsp, tUser), g.scopeOf(tApp, "ten_3", "wsp_9", "usr_9"), other,
	}
	matched := 0
	for _, sc := range scopes {
		for _, provider := range []providers.ProviderID{"openai", "anthropic", "gemini"} {
			for _, ch := range []providers.Channel{in, bg} {
				want := ids(applicableBeforeUnion, applicableArgsBeforeUnion(sc, provider, ch)...)
				got, err := applicable(ctx, g.db, sc, provider, ch)
				if err != nil {
					t.Fatal(err)
				}
				var gotIDs []string
				for _, r := range got {
					gotIDs = append(gotIDs, string(r.b.BudgetID))
				}
				if strings.Join(gotIDs, ",") != strings.Join(want, ",") {
					t.Errorf("%s/%s/%s/%s %s %s: applicable = %v, the statement it replaced = %v",
						sc.ApplicationID, sc.TenantID, sc.WorkspaceID, sc.PrincipalID, provider, ch, gotIDs, want)
				}
				matched += len(want)
			}
		}
	}
	if matched < 30 {
		t.Fatalf("only %d budgets matched across the cases: the comparison proves little", matched)
	}
}

// StatesOf is State for several budgets in one read: the same objects, in the order asked for, and a
// budget that is not the application's fails the whole read.
func TestStatesOfIsStateForSeveral(t *testing.T) {
	g := newRig(t)
	ctx := context.Background()
	a := g.put(providerBudget("openai", providers.ChannelInteractive, 100_000, 1_000_000))
	b := g.put(tenantBudget(tApp, tTenant, 50_000, 500_000))
	g.mustSettle(g.mustReserve(callOf("openai", out(100))), Actual{Metering: out(100)})
	g.mustReserve(callOf("openai", out(50)))
	g.fallback["openai"] = true

	got, err := g.svc.StatesOf(ctx, g.operator(), []providers.BudgetID{b.BudgetID, a.BudgetID})
	if err != nil {
		t.Fatal(err)
	}
	if want := []providers.BudgetState{g.state(b), g.state(a)}; !reflect.DeepEqual(got, want) {
		t.Errorf("StatesOf = %+v\nwant the States %+v", got, want)
	}
	if got, err := g.svc.StatesOf(ctx, g.operator(), nil); err != nil || len(got) != 0 {
		t.Errorf("StatesOf of nothing = %v, %v", got, err)
	}
	if _, err := g.svc.StatesOf(ctx, g.operator(), []providers.BudgetID{a.BudgetID, "bud_nope"}); ReasonOf(err) != "unknown_budget" {
		t.Errorf("an unknown budget: %v", err)
	}
	if _, err := g.svc.StatesOf(ctx, g.scopeOf("app_other", tTenant, tWsp, tUser), []providers.BudgetID{a.BudgetID}); ReasonOf(err) != "unknown_budget" {
		t.Errorf("another application reads a budget state: %v", err)
	}
}

// applicableArgsBeforeUnion are the arguments of applicableBeforeUnion.
func applicableArgsBeforeUnion(scope contracts.Scope, provider providers.ProviderID, channel providers.Channel) []any {
	p := string(provider)
	return []any{
		scope.ApplicationID, string(channel),
		p,
		scope.TenantID, p,
		scope.TenantID, scope.WorkspaceID, p,
		scope.TenantID, scope.WorkspaceID, scope.PrincipalID, p,
	}
}

// A read that breaks off part way is an error, never a partial spent or reserved figure that lets Reserve
// admit more than the budget holds.
func TestBudgetSnapshotReadThatBreaksOffFails(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	r := budgetRow{b: providers.Budget{BudgetID: "bud_01"}}
	for _, table := range []string{"providerbudget_counters", "providerbudget_holds"} {
		db := sql.OpenDB(failRowsDriver{failOn: table})
		t.Cleanup(func() { _ = db.Close() })
		if _, err := loadSnapshot(context.Background(), db, "app_demo", r, windowsAt(now), now); err == nil {
			t.Errorf("%s: a read that broke off gave a snapshot", table)
		}
	}
}
