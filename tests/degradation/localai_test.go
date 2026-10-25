package degradation

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/supervisor"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// toggleProbe is the runtime's health as the supervisor sees it, switched by the test: the stand-in for LocalAI that
// is up, then crashes, then is brought back (or is not).
type toggleProbe struct{ up atomic.Bool }

func (p *toggleProbe) Name() string { return "fake-localai" }

func (p *toggleProbe) Health(context.Context) (providers.HealthReport, error) {
	if p.up.Load() {
		return providers.HealthReport{Status: providers.HealthOK, Version: "fake"}, nil
	}
	return providers.HealthReport{Status: providers.HealthUnavailable, Reason: "connection refused"}, nil
}

type events struct {
	mu  sync.Mutex
	all []supervisor.Event
}

func (e *events) handle(ev supervisor.Event) {
	e.mu.Lock()
	e.all = append(e.all, ev)
	e.mu.Unlock()
}

func (e *events) snapshot() []supervisor.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]supervisor.Event(nil), e.all...)
}

func (e *events) types() []supervisor.EventType {
	var out []supervisor.EventType
	for _, ev := range e.snapshot() {
		out = append(out, ev.Type)
	}
	return out
}

func (e *events) of(t supervisor.EventType) []supervisor.Event {
	var out []supervisor.Event
	for _, ev := range e.snapshot() {
		if ev.Type == t {
			out = append(out, ev)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// coordReceipts is the ReceiptSource of the supervisor's ledger: the action coordinator's own store, never anything
// the model said.
type coordReceipts struct{ w *world }

func (c coordReceipts) Receipt(ctx context.Context, id string) (contracts.ActionReceipt, bool, error) {
	r, err := c.w.co.Receipt(ctx, memberScope(), id)
	switch {
	case err == nil:
		return r, true, nil
	case contracts.CodeOf(err) == contracts.ErrConflict: // no outcome exists yet
		return contracts.ActionReceipt{}, false, nil
	}
	return contracts.ActionReceipt{}, false, err
}

// gate puts the supervisor in front of the model provider, as the composition root does with Admit: while the runtime
// is not running, model-driven work fails closed with unavailable.
type gate struct {
	providers.TextGenerator
	sup *atomic.Pointer[supervisor.Supervisor]
}

func (g gate) admit() error {
	if s := g.sup.Load(); s != nil {
		return s.Admit()
	}
	return nil
}

func (g gate) Chat(ctx context.Context, s contracts.Scope, r providers.ChatRequest) (providers.ChatResult, error) {
	if err := g.admit(); err != nil {
		return providers.ChatResult{}, err
	}
	return g.TextGenerator.Chat(ctx, s, r)
}

func (g gate) ChatStream(ctx context.Context, s contracts.Scope, r providers.ChatRequest) (providers.ChatStream, error) {
	if err := g.admit(); err != nil {
		return nil, err
	}
	return g.TextGenerator.ChatStream(ctx, s, r)
}

// newAction prepares an items.create action through the coordinator and confirms it, without committing.
func (w *world) newAction(name string) string {
	w.t.Helper()
	sc := memberScope()
	p, err := w.co.Prepare(ctxb(), sc, actions.PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": name, "quantity": 1}})
	if err != nil {
		w.t.Fatalf("Prepare: %v", err)
	}
	id := string(p.Action.PreparationID)
	if _, err := w.co.Confirm(ctxb(), sc, actions.ConfirmInput{PreparationID: id, Token: p.Confirmation}); err != nil {
		w.t.Fatalf("Confirm: %v", err)
	}
	return id
}

// Section 18, "LocalAI crash": the supervisor restarts it with backoff; an in-flight action remains uncommitted unless
// a receipt proves otherwise.
func TestDegradeLocalAICrash(t *testing.T) {
	const (
		backoff0     = 100 * time.Millisecond
		startTimeout = 5 * time.Second
		probeEvery   = 7 * time.Millisecond
	)
	var supRef atomic.Pointer[supervisor.Supervisor]
	w := newWorldWith(t, func(g providers.TextGenerator) providers.TextGenerator { return gate{TextGenerator: g, sup: &supRef} })

	// Three actions that were in flight against the runtime when it crashed: one whose commit the host recorded
	// (a receipt proves it), one confirmed but never sent, one sent whose outcome nobody could establish.
	done := w.newAction("done")
	if rc, err := w.co.Commit(ctxb(), memberScope(), done); err != nil || rc.Status != act.ActionReceiptStatusCommitted {
		t.Fatalf("setup: commit of the proven action: %+v %v", rc, err)
	}
	unsure := w.newAction("unsure")
	w.app.Inject(adapters.OpCommit, fake.FaultDropReply)
	w.app.Inject(adapters.OpStatus, fake.FaultDropRequest)
	if rc, err := w.co.Commit(ctxb(), memberScope(), unsure); contracts.CodeOf(err) != contracts.ErrUncertainOutcome || rc.Status != act.ActionReceiptStatusUncertain {
		t.Fatalf("setup: the unsure action: %+v %v", rc, err)
	}
	// Prepared last, so the host data it relied on is the data the earlier commits left.
	pending := w.newAction("pending")
	commitsBefore := w.hostCalls(adapters.OpCommit)

	// The supervisor over a unit the service manager keeps (the hardened native install): it attaches, probes, and
	// backs off. The unit's controller heals the runtime on its first restart only, so the second crash is a crash loop.
	probe := &toggleProbe{}
	probe.up.Store(true)
	var restarts atomic.Int32
	unit := &supervisor.Unit{Restart: func(context.Context) error {
		if restarts.Add(1) == 1 {
			probe.up.Store(true)
		}
		return nil
	}}
	clk := clock.NewFake(t0)
	hold := make(chan time.Time) // holds the first backoff open so its state can be looked at
	var held atomic.Bool
	after := func(d time.Duration) <-chan time.Time {
		switch {
		case d == probeEvery:
			return time.After(time.Millisecond) // probing is real time, and short
		case d == backoff0 && held.CompareAndSwap(false, true):
			return hold
		}
		c := make(chan time.Time, 1)
		c <- clk.Advance(d) // every other wait is over at once and moves the fake clock on
		return c
	}
	rec := &events{}
	ledger := supervisor.NewLedger(clk, coordReceipts{w})
	cfg := supervisor.Config{
		MaxRestarts: 3, RestartWindow: time.Hour, StartTimeout: startTimeout, UnhealthyAfter: 2,
		ProbeInterval: probeEvery, HealthInterval: probeEvery,
		Backoff: supervisor.Backoff{Initial: backoff0, Max: time.Second, Factor: 2},
	}
	sup, err := supervisor.New(cfg, unit, probe, supervisor.WithClock(clk), supervisor.WithAfter(after),
		supervisor.WithRand(func() float64 { return 0.5 }), supervisor.WithLedger(ledger), supervisor.WithEvents(rec.handle))
	if err != nil {
		t.Fatal(err)
	}
	supRef.Store(sup)
	ctx, cancel := context.WithCancel(ctxb())
	runDone := make(chan error, 1)
	go func() { runDone <- sup.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return")
		}
	})

	waitFor(t, "the runtime to be ready", func() bool { return sup.Status().State == supervisor.StateRunning })
	if err := sup.Admit(); err != nil {
		t.Fatalf("a healthy runtime must admit model work: %v", err)
	}
	// Control: while the runtime is up a turn reaches the model.
	w.model.On(pfakeProposal()).Reply(createProposal).Times(1)
	w.model.On(pfakeAct("request_confirmation")).Reply("Please confirm: create an item named widget.")
	if res := w.say(sayCreate); res.Prepared == nil {
		t.Fatalf("control: act %s code %q", res.Directive.Act, res.Code)
	}
	for _, id := range []string{done, pending, unsure} {
		if err := ledger.Begin(id); err != nil {
			t.Fatal(err)
		}
	}

	// Crash 1.
	probe.up.Store(false)
	waitFor(t, "the first backoff", func() bool { return sup.Status().State == supervisor.StateBackoff })
	if got := rec.types(); len(got) != 5 || got[0] != supervisor.EventStarted || got[1] != supervisor.EventReady ||
		got[2] != supervisor.EventExited || got[3] != supervisor.EventSettled || got[4] != supervisor.EventBackoff {
		t.Fatalf("events %v: want started, ready, exited, settled, backoff", got)
	}
	if ex := rec.of(supervisor.EventExited)[0]; ex.Reason != supervisor.ReasonUnhealthy {
		t.Errorf("exit reason %q, want unhealthy", ex.Reason)
	}
	// The in-flight actions were settled BEFORE the restart, and only the receipt-backed one is committed.
	got := map[string]supervisor.Settlement{}
	for _, s := range rec.of(supervisor.EventSettled)[0].Settlements {
		got[s.PreparationID] = s
	}
	want := map[string]struct {
		committed, reconcile bool
		reason               string
	}{
		done:    {true, false, supervisor.ReasonReceiptProvesCommit},
		pending: {false, true, supervisor.ReasonNoReceipt},
		unsure:  {false, true, supervisor.ReasonReceiptUncertain},
	}
	if len(got) != len(want) {
		t.Fatalf("settled %d actions, want %d: %+v", len(got), len(want), got)
	}
	for id, wnt := range want {
		s := got[id]
		if s.Committed != wnt.committed || s.NeedsReconcile != wnt.reconcile || s.Reason != wnt.reason {
			t.Errorf("%s settled committed=%v reconcile=%v reason=%s, want committed=%v reconcile=%v reason=%s",
				id, s.Committed, s.NeedsReconcile, s.Reason, wnt.committed, wnt.reconcile, wnt.reason)
		}
	}
	if s := got[done]; s.ReceiptID == "" {
		t.Error("a committed settlement must name the receipt that proves it")
	}
	// While it is down: model work fails closed, health says unavailable, and nothing is executed.
	if err := sup.Admit(); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("Admit during the backoff = %v, want unavailable", err)
	}
	if rep, _ := sup.Health(ctxb()); rep.Status != providers.HealthUnavailable {
		t.Errorf("health %+v during the backoff, want unavailable", rep)
	}
	effects := w.app.Effects()
	if res := w.say(sayCreate); res.Code != contracts.ErrUnavailable || res.Prepared != nil || w.app.Effects() != effects {
		t.Fatalf("a turn while the runtime is down: act %s code %q prepared %v", res.Directive.Act, res.Code, res.Prepared)
	}
	if got := w.prepState(pending); got != "confirmed" {
		t.Fatalf("the unsent action is %s, want confirmed: a crash must not commit it", got)
	}
	if n := w.hostCalls(adapters.OpCommit); n != commitsBefore {
		t.Fatalf("%d Commit calls reached the host (had %d): nothing is resumed or retried by the crash", n, commitsBefore)
	}

	// The backoff ends, the unit is restarted, the runtime is back: admission resumes.
	close(hold)
	waitFor(t, "the runtime to be back", func() bool {
		return sup.Status().State == supervisor.StateRunning && len(rec.of(supervisor.EventReady)) == 2
	})
	if restarts.Load() != 1 {
		t.Fatalf("the unit was asked to restart %d times, want 1", restarts.Load())
	}
	if err := sup.Admit(); err != nil {
		t.Fatalf("Admit after the restart: %v", err)
	}
	// Nothing was resumed: the next in-flight set starts empty.
	if ledger.Len() != 0 {
		t.Fatalf("the ledger still tracks %d actions after the restart", ledger.Len())
	}

	// A crash loop: the unit is not healed again. The waits grow, and after MaxRestarts the supervisor gives up.
	probe.up.Store(false)
	var runErr error
	select {
	case runErr = <-runDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the supervisor never gave up on a crash loop")
	}
	runDone <- runErr // for the cleanup
	if contracts.CodeOf(runErr) != contracts.ErrUnavailable {
		t.Fatalf("Run = %v, want unavailable", runErr)
	}
	var delays []time.Duration
	for _, ev := range rec.of(supervisor.EventBackoff) {
		delays = append(delays, ev.Delay)
	}
	if len(delays) != 3 || delays[0] != backoff0 || delays[1] != 2*backoff0 || delays[2] != 4*backoff0 {
		t.Fatalf("backoff delays %v, want %v: each wait is longer than the last", delays, []time.Duration{backoff0, 2 * backoff0, 4 * backoff0})
	}
	if st := sup.Status().State; st != supervisor.StateGaveUp {
		t.Fatalf("state %s, want gave_up", st)
	}
	if err := sup.Admit(); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("Admit after the supervisor gave up = %v, want unavailable", err)
	}
	if rep, _ := sup.Health(ctxb()); rep.Status != providers.HealthUnavailable {
		t.Fatalf("health %+v after giving up, want unavailable", rep)
	}

	// Recovery: the outcome of an action is established by asking the host, through the coordinator, never by a
	// blind second commit. The unsure one turns out to have been applied; the unsent one commits once.
	rc, err := w.co.Reconcile(ctxb(), memberScope(), unsure)
	if err != nil || rc.Status != act.ActionReceiptStatusCommitted {
		t.Fatalf("Reconcile of the unsure action: %+v %v, want committed", rc, err)
	}
	rc, err = w.co.Commit(ctxb(), memberScope(), pending)
	if err != nil || rc.Status != act.ActionReceiptStatusCommitted {
		t.Fatalf("Commit of the unsent action: %+v %v", rc, err)
	}
	if w.hostCalls(adapters.OpCommit) != commitsBefore+1 || w.app.Effects() != effects+1 {
		t.Fatalf("commits %d (had %d) effects %d (had %d): exactly one new commit, for the action that was never sent",
			w.hostCalls(adapters.OpCommit), commitsBefore, w.app.Effects(), effects)
	}
}
