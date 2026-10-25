package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// A crashing runtime is restarted after growing, capped delays, each run is a new
// process, and the final healthy run is stopped cleanly with the supervisor.
func TestSupervisorRestartsWithBackoff(t *testing.T) {
	f := newExecFixture(t, Config{MaxRestarts: 5}, nil, "-crash-runs", "3", "-crash-after", "40ms")
	r := runSup(t, f.sup)
	waitFor(t, 60*time.Second, "the fourth start to be running", func() bool {
		st := f.sup.Status()
		return st.State == StateRunning && st.Starts == 4
	})

	// Three crashes: 100ms, 200ms, then 400ms capped at 250ms.
	var delays []time.Duration
	for _, e := range f.rec.of(EventBackoff) {
		delays = append(delays, e.Delay)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 250 * time.Millisecond}
	if diff := cmp.Diff(want, delays); diff != "" {
		t.Errorf("backoff delays (-want +got):\n%s", diff)
	}

	pids := map[int]bool{}
	for _, e := range f.rec.of(EventStarted) {
		pids[e.PID] = true
	}
	if len(pids) != 4 {
		t.Errorf("started %d distinct processes, want 4: %v", len(pids), f.rec.of(EventStarted))
	}
	for _, e := range f.rec.of(EventExited) {
		if e.Reason != ReasonExited || e.Detail != "exit status 3" {
			t.Errorf("exit event = %q %q, want exited with %q", e.Reason, e.Detail, "exit status 3")
		}
	}
	if st := f.sup.Status(); st.State != StateRunning || st.Starts != 4 {
		t.Errorf("status = %+v, want running after 4 starts", st)
	}
	if rep, err := f.sup.Health(context.Background()); err != nil || rep.Status != providers.HealthOK {
		t.Errorf("health of the recovered runtime = %+v, %v; want ok", rep, err)
	}
	if err := f.sup.Admit(); err != nil {
		t.Errorf("Admit with a running runtime: %v", err)
	}

	if err := r.stop(t); err != nil {
		t.Fatalf("Run after cancellation = %v, want nil", err)
	}
	f.spy.assertAllGone(t)
	if rep, _ := f.sup.Health(context.Background()); rep.Status != providers.HealthUnavailable {
		t.Errorf("health after shutdown = %+v, want unavailable", rep)
	}
	if st := f.sup.Status().State; st != StateStopped {
		t.Errorf("state after shutdown = %s, want stopped", st)
	}
}

// SEC-046: when the runtime dies, an action that was in flight stays uncommitted
// unless a receipt written by the core proves otherwise, and that settlement
// happens before the restart.
func TestInFlightActionUncommittedOnCrash(t *testing.T) {
	rcpt := func(id, prep string, st act.ActionReceiptStatus) contracts.ActionReceipt {
		return contracts.ActionReceipt{ReceiptID: act.ReceiptID(id), PreparationID: act.PreparationID(prep), Status: st}
	}
	src := mapSource{
		receipts: map[string]contracts.ActionReceipt{
			"prep_committed": rcpt("rcpt_1", "prep_committed", act.ActionReceiptStatusCommitted),
			"prep_uncertain": rcpt("rcpt_2", "prep_uncertain", act.ActionReceiptStatusUncertain),
			"prep_rejected":  rcpt("rcpt_3", "prep_rejected", act.ActionReceiptStatusRejected),
			// A receipt that names another preparation proves nothing about this one.
			"prep_forged": rcpt("rcpt_4", "prep_someone_else", act.ActionReceiptStatusCommitted),
		},
		fail: map[string]error{"prep_lookup_fails": errors.New("store unavailable")},
	}
	ledger := NewLedger(clock.Real{}, src)
	crash := filepath.Join(t.TempDir(), "crash")
	f := newExecFixture(t, Config{MaxRestarts: 5}, ledger, "-crash-runs", "1", "-crash-on-file", crash)
	r := runSup(t, f.sup)
	f.waitRunning(t)

	ids := []string{"prep_no_receipt", "prep_committed", "prep_uncertain", "prep_rejected", "prep_forged", "prep_lookup_fails"}
	for _, id := range ids {
		if err := ledger.Begin(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(crash, []byte("now"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 60*time.Second, "the in-flight actions to be settled", func() bool { return len(f.rec.of(EventSettled)) > 0 })

	got := map[string]Settlement{}
	for _, s := range f.rec.of(EventSettled)[0].Settlements {
		got[s.PreparationID] = s
	}
	want := map[string]struct {
		committed, reconcile bool
		reason               string
	}{
		"prep_no_receipt":   {false, true, ReasonNoReceipt},
		"prep_committed":    {true, false, ReasonReceiptProvesCommit},
		"prep_uncertain":    {false, true, ReasonReceiptUncertain},
		"prep_rejected":     {false, false, ReasonReceiptRejected},
		"prep_forged":       {false, true, ReasonReceiptMismatch},
		"prep_lookup_fails": {false, true, ReasonLookupFailed},
	}
	if len(got) != len(want) {
		t.Errorf("settled %d actions, want %d: %+v", len(got), len(want), got)
	}
	for id, w := range want {
		s, ok := got[id]
		if !ok {
			t.Errorf("%s was not settled", id)
			continue
		}
		if s.Committed != w.committed || s.NeedsReconcile != w.reconcile || s.Reason != w.reason {
			t.Errorf("%s settled as committed=%v reconcile=%v reason=%s; want committed=%v reconcile=%v reason=%s",
				id, s.Committed, s.NeedsReconcile, s.Reason, w.committed, w.reconcile, w.reason)
		}
	}
	if s := got["prep_committed"]; s.ReceiptID != "rcpt_1" {
		t.Errorf("the proving receipt = %q, want rcpt_1", s.ReceiptID)
	}
	if n := ledger.Len(); n != 0 {
		t.Errorf("%d actions still tracked after settlement", n)
	}

	// The crash is settled before the runtime is started again.
	waitFor(t, 60*time.Second, "the runtime to come back", func() bool {
		return f.sup.Status().State == StateRunning && len(f.rec.of(EventStarted)) == 2
	})
	wantOrder := []EventType{EventStarted, EventReady, EventExited, EventSettled, EventBackoff, EventStarted}
	got2 := f.rec.types()
	if len(got2) < len(wantOrder) || !slices.Equal(got2[:len(wantOrder)], wantOrder) {
		t.Errorf("event order = %v, want it to begin %v", got2, wantOrder)
	}

	// Nothing is resumed: an action begun after the restart is a fresh one.
	if err := ledger.Begin("prep_after"); err != nil {
		t.Fatal(err)
	}
	if n := ledger.Len(); n != 1 {
		t.Errorf("ledger holds %d actions after the restart, want only the new one", n)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
	// Shutdown settles too, and still commits nothing without a receipt.
	last := f.rec.of(EventSettled)
	if len(last) != 2 || len(last[1].Settlements) != 1 || last[1].Settlements[0].Committed {
		t.Errorf("shutdown settlement = %+v, want one uncommitted action", last)
	}
	f.spy.assertAllGone(t)
}

// After MaxRestarts+1 failures inside the window the supervisor stops starting
// processes, Run reports unavailable and so does Health.
func TestSupervisorGivesUpAfterLimitAndReportsUnavailable(t *testing.T) {
	f := newExecFixture(t, Config{MaxRestarts: 2}, nil, "-crash-runs", "1000", "-crash-after", "10ms")
	r := runSup(t, f.sup)
	err := r.wait(t, 60*time.Second)
	if !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("Run = %v, want an unavailable error", err)
	}
	// One start plus two restarts, then nothing more, however long we wait.
	time.Sleep(300 * time.Millisecond)
	if n := startsCount(t, f.dir); n != 3 {
		t.Errorf("the child was started %d times, want 3 (1 + MaxRestarts)", n)
	}
	if got := len(f.rec.of(EventBackoff)); got != 2 {
		t.Errorf("%d backoffs, want 2", got)
	}
	if got := len(f.rec.of(EventGaveUp)); got != 1 {
		t.Errorf("%d gave_up events, want 1", got)
	}
	if st := f.sup.Status(); st.State != StateGaveUp || st.PID != 0 || st.Starts != 3 {
		t.Errorf("status = %+v, want gave_up after 3 starts", st)
	}
	rep, herr := f.sup.Health(context.Background())
	if herr != nil || rep.Status != providers.HealthUnavailable || rep.Reason != healthGaveUp {
		t.Errorf("health = %+v, %v; want unavailable with the give-up reason", rep, herr)
	}
	if err := f.sup.Admit(); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("Admit = %v, want unavailable", err)
	}
	f.spy.assertAllGone(t)
}

// --- in-process cases over a scripted runtime and a fake clock ---

// fakeInst is an instance with no process behind it.
type fakeInst struct {
	pid     int
	done    chan struct{}
	stopErr error
	stops   atomic.Int32
}

func (i *fakeInst) PID() int              { return i.pid }
func (i *fakeInst) Done() <-chan struct{} { return i.done }
func (i *fakeInst) Stop(context.Context) error {
	i.stops.Add(1)
	return i.stopErr
}

// fakeStep scripts one Start: a start error, or an instance that has already
// exited after living for liveFor (the fake clock is advanced by it).
type fakeStep struct {
	err     error
	exited  bool
	liveFor time.Duration
	stopErr error
}

type fakeRuntime struct {
	clk   *clock.Fake
	mu    sync.Mutex
	steps []fakeStep
	n     int
	insts []*fakeInst
}

func (r *fakeRuntime) Start(context.Context) (Instance, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	step := r.steps[min(r.n, len(r.steps)-1)]
	r.n++
	if step.err != nil {
		return nil, step.err
	}
	r.clk.Advance(step.liveFor)
	inst := &fakeInst{pid: 1000 + r.n, done: make(chan struct{}), stopErr: step.stopErr}
	if step.exited {
		close(inst.done)
	}
	r.insts = append(r.insts, inst)
	return inst, nil
}

func (r *fakeRuntime) starts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// scriptProber answers from a list, repeating the last entry.
type scriptProber struct {
	mu  sync.Mutex
	seq []providers.Health
	n   int
}

func (p *scriptProber) Name() string { return "fake" }

func (p *scriptProber) Health(context.Context) (providers.HealthReport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.seq[min(p.n, len(p.seq)-1)]
	p.n++
	return providers.HealthReport{Status: st}, nil
}

// clockAfter returns timers that fire at once and move the fake clock forward by
// the requested wait.
func clockAfter(clk *clock.Fake) func(time.Duration) <-chan time.Time {
	return func(d time.Duration) <-chan time.Time {
		c := make(chan time.Time, 1)
		c <- clk.Advance(d)
		return c
	}
}

func newFakeSup(t *testing.T, cfg Config, steps []fakeStep, probe providers.AIRuntimeProvider) (*Supervisor, *fakeRuntime, *recorder, *clock.Fake) {
	t.Helper()
	clk := fakeClock()
	rt := &fakeRuntime{clk: clk, steps: steps}
	rec := &recorder{}
	if probe == nil {
		probe = &scriptProber{seq: []providers.Health{providers.HealthOK}}
	}
	cfg.Backoff = Backoff{Initial: time.Second, Max: 30 * time.Second, Factor: 2}
	cfg.RestartWindow = 10 * time.Minute
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = 10 * time.Millisecond // the fake timer advances the clock by it on every start
	}
	sup, err := New(cfg, rt, probe, WithClock(clk), WithAfter(clockAfter(clk)), WithEvents(rec.handle))
	if err != nil {
		t.Fatal(err)
	}
	return sup, rt, rec, clk
}

func delaysOf(rec *recorder) []time.Duration {
	var out []time.Duration
	for _, e := range rec.of(EventBackoff) {
		out = append(out, e.Delay)
	}
	return out
}

// Failures age out of the window: a runtime that stays up for a whole window
// starts again from the shortest delay and its budget is whole again, while a
// crash loop with no such pause is given up on.
func TestStableRunResetsRestartBudget(t *testing.T) {
	crash := fakeStep{exited: true}
	t.Run("a stable run resets the budget", func(t *testing.T) {
		steps := []fakeStep{crash, crash, {exited: true, liveFor: 20 * time.Minute}, crash, {}}
		sup, rt, rec, _ := newFakeSup(t, Config{MaxRestarts: 2}, steps, nil)
		r := runSup(t, sup)
		waitFor(t, 10*time.Second, "the runtime to be running", func() bool { return sup.Status().State == StateRunning })
		want := []time.Duration{time.Second, 2 * time.Second, time.Second, 2 * time.Second}
		if diff := cmp.Diff(want, delaysOf(rec)); diff != "" {
			t.Errorf("delays (-want +got):\n%s", diff)
		}
		if n := rt.starts(); n != 5 || len(rec.of(EventGaveUp)) != 0 {
			t.Errorf("starts = %d, gave up = %d; want 5 starts and no give-up", n, len(rec.of(EventGaveUp)))
		}
		if err := r.stop(t); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a crash loop is given up on", func(t *testing.T) {
		sup, rt, _, _ := newFakeSup(t, Config{MaxRestarts: 2}, []fakeStep{crash}, nil)
		r := runSup(t, sup)
		if err := r.wait(t, 10*time.Second); !contracts.HasCode(err, contracts.ErrUnavailable) {
			t.Fatalf("Run = %v, want unavailable", err)
		}
		if n := rt.starts(); n != 3 {
			t.Errorf("starts = %d, want 3", n)
		}
	})
}

// A running instance that stays unavailable for UnhealthyAfter probes is stopped
// and replaced; a degraded one is up and is left alone.
func TestUnhealthyRuntimeIsRestarted(t *testing.T) {
	probe := &scriptProber{seq: []providers.Health{
		providers.HealthOK, providers.HealthDegraded, providers.HealthUnavailable, providers.HealthOK, // one blip is tolerated
		providers.HealthUnavailable, providers.HealthUnavailable, // then two in a row are not
		providers.HealthOK,
	}}
	sup, rt, rec, _ := newFakeSup(t, Config{MaxRestarts: 5, UnhealthyAfter: 2}, []fakeStep{{}}, probe)
	r := runSup(t, sup)
	waitFor(t, 10*time.Second, "the second start", func() bool { return rt.starts() >= 2 })
	exits := rec.of(EventExited)
	if len(exits) != 1 || exits[0].Reason != ReasonUnhealthy {
		t.Fatalf("exit events = %+v, want one unhealthy", exits)
	}
	if n := rt.insts[0].stops.Load(); n != 1 {
		t.Errorf("the unhealthy instance was stopped %d times, want 1", n)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
}

// A runtime that never becomes healthy within StartTimeout is a failure, not a
// wait without end.
func TestStartTimeoutIsAFailure(t *testing.T) {
	probe := &scriptProber{seq: []providers.Health{providers.HealthUnavailable}}
	sup, _, rec, _ := newFakeSup(t, Config{MaxRestarts: 1, StartTimeout: time.Minute}, []fakeStep{{}}, probe)
	r := runSup(t, sup)
	if err := r.wait(t, 10*time.Second); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("Run = %v, want unavailable", err)
	}
	for _, e := range rec.of(EventExited) {
		if e.Reason != ReasonStartTimeout {
			t.Errorf("exit reason = %q, want %q", e.Reason, ReasonStartTimeout)
		}
	}
}

// An instance that cannot be confirmed stopped is never replaced: that would be
// two LocalAI processes on one port and one set of models.
func TestUnconfirmedStopNeverStartsASecondInstance(t *testing.T) {
	probe := &scriptProber{seq: []providers.Health{providers.HealthUnavailable}}
	steps := []fakeStep{{stopErr: errors.New("still alive")}}
	sup, rt, _, _ := newFakeSup(t, Config{MaxRestarts: 5, StartTimeout: time.Minute}, steps, probe)
	r := runSup(t, sup)
	if err := r.wait(t, 10*time.Second); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("Run = %v, want unavailable", err)
	}
	if n := rt.starts(); n != 1 {
		t.Errorf("%d starts, want exactly 1", n)
	}
	if st := sup.Status().State; st != StateGaveUp {
		t.Errorf("state = %s, want gave_up", st)
	}
}

// A start that fails is a failure like any other.
func TestStartFailureIsBackedOff(t *testing.T) {
	steps := []fakeStep{{err: errors.New("no binary")}, {err: errors.New("no binary")}, {}}
	sup, rt, rec, _ := newFakeSup(t, Config{MaxRestarts: 5}, steps, nil)
	r := runSup(t, sup)
	waitFor(t, 10*time.Second, "the runtime to be running", func() bool { return sup.Status().State == StateRunning })
	if diff := cmp.Diff([]time.Duration{time.Second, 2 * time.Second}, delaysOf(rec)); diff != "" {
		t.Errorf("delays (-want +got):\n%s", diff)
	}
	if n := rt.starts(); n != 3 {
		t.Errorf("starts = %d, want 3", n)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
}

func TestRunRefusesToRunTwice(t *testing.T) {
	sup, _, _, _ := newFakeSup(t, Config{}, []fakeStep{{}}, nil)
	r := runSup(t, sup)
	waitFor(t, 10*time.Second, "the runtime to be running", func() bool { return sup.Status().State == StateRunning })
	if err := sup.Run(context.Background()); !contracts.HasCode(err, contracts.ErrConflict) {
		t.Errorf("second Run = %v, want conflict", err)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
}

// Health is the runtime's own report while running, so degraded and the version
// pass through; every other state is unavailable with a fixed reason.
func TestHealthPassesThroughWhileRunning(t *testing.T) {
	probe := &scriptProber{seq: []providers.Health{providers.HealthDegraded}}
	sup, _, _, _ := newFakeSup(t, Config{}, []fakeStep{{}}, probe)
	if rep, _ := sup.Health(context.Background()); rep.Status != providers.HealthUnavailable || rep.Reason != healthStopped {
		t.Errorf("before Run: %+v", rep)
	}
	if err := sup.Admit(); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Errorf("Admit before Run = %v, want unavailable", err)
	}
	r := runSup(t, sup)
	waitFor(t, 10*time.Second, "running", func() bool { return sup.Status().State == StateRunning })
	if rep, _ := sup.Health(context.Background()); rep.Status != providers.HealthDegraded {
		t.Errorf("running: %+v, want the runtime's degraded report", rep)
	}
	if err := r.stop(t); err != nil {
		t.Fatal(err)
	}
}

// The Unit runtime attaches without touching the service on the first start and
// asks the controller (when there is one) for every restart after it.
func TestUnitRuntimeAttachesThenRestartsThroughController(t *testing.T) {
	var restarts int
	u := &Unit{Restart: func(context.Context) error { restarts++; return nil }}
	for i := range 3 {
		inst, err := u.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if inst.PID() != 0 || inst.Done() != nil || inst.Stop(context.Background()) != nil {
			t.Errorf("start %d: an attached unit has no pid, never finishes and is not stopped by us", i)
		}
	}
	if restarts != 2 {
		t.Errorf("controller called %d times, want 2 (every start but the first)", restarts)
	}

	failing := &Unit{Restart: func(context.Context) error { return errors.New("denied") }}
	_, _ = failing.Start(context.Background())
	if _, err := failing.Start(context.Background()); err == nil {
		t.Error("a failing controller must fail the start")
	}
	bare := &Unit{}
	for range 2 {
		if _, err := bare.Start(context.Background()); err != nil {
			t.Errorf("a unit with no controller waits, it does not fail: %v", err)
		}
	}
}

// With a Unit runtime nothing is ever started by the supervisor: an unhealthy
// unit is only waited for, with backoff, until the limit.
func TestUnitRuntimeUnhealthyUnitIsBackedOffThenGivenUp(t *testing.T) {
	probe := &scriptProber{seq: []providers.Health{providers.HealthUnavailable}}
	clk := fakeClock()
	rec := &recorder{}
	cfg := Config{MaxRestarts: 2, StartTimeout: time.Minute, RestartWindow: 10 * time.Minute}
	cfg.Backoff = Backoff{Initial: time.Second, Max: 30 * time.Second, Factor: 2}
	sup, err := New(cfg, &Unit{}, probe, WithClock(clk), WithAfter(clockAfter(clk)), WithEvents(rec.handle))
	if err != nil {
		t.Fatal(err)
	}
	r := runSup(t, sup)
	if err := r.wait(t, 10*time.Second); !contracts.HasCode(err, contracts.ErrUnavailable) {
		t.Fatalf("Run = %v, want unavailable", err)
	}
	if diff := cmp.Diff([]time.Duration{time.Second, 2 * time.Second}, delaysOf(rec)); diff != "" {
		t.Errorf("delays (-want +got):\n%s", diff)
	}
	for _, e := range rec.of(EventStarted) {
		if e.PID != 0 {
			t.Errorf("an attached unit reported pid %d", e.PID)
		}
	}
}
