package resources

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func candidate(id string, class jobs.Class) jobs.Candidate {
	return jobs.Candidate{ID: id, Class: class, Type: "t." + id}
}

// SEC-078
// background classes cannot starve interactive turns.
func TestBackgroundYieldsToInteractive(t *testing.T) {
	ingestJob := candidate("ingest", jobs.ClassIngest)
	maintJob := candidate("maint", jobs.ClassMaintenance)
	readJob := candidate("read", jobs.ClassRead)

	t.Run("policy leases no background while a turn is live", func(t *testing.T) {
		e := newEnv(t)
		pol := e.gov.Policy(nil)
		if got := pol.Pick([]jobs.Candidate{ingestJob}); got != 0 {
			t.Fatalf("idle: Pick = %d, want 0", got)
		}
		release := e.gov.BeginInteractive(jobs.ClassTurn)
		if got := e.gov.InteractiveActive(); got != 1 {
			t.Fatalf("InteractiveActive = %d, want 1", got)
		}
		for _, c := range []jobs.Candidate{ingestJob, maintJob} {
			if got := pol.Pick([]jobs.Candidate{c}); got != -1 {
				t.Fatalf("live turn: Pick(%s) = %d, want -1", c.ID, got)
			}
		}
		if got := pol.Pick([]jobs.Candidate{ingestJob, readJob}); got != 1 {
			t.Fatalf("live turn, mixed: Pick = %d, want 1 (the interactive-class job)", got)
		}
		release()
		release() // a second release is harmless
		if got := e.gov.InteractiveActive(); got != 0 {
			t.Fatalf("after release: InteractiveActive = %d, want 0", got)
		}
		if got := pol.Pick([]jobs.Candidate{ingestJob}); got != 0 {
			t.Fatalf("after release: Pick = %d, want 0", got)
		}
	})

	t.Run("only classes up to InteractiveUpTo hold background", func(t *testing.T) {
		e := newEnv(t)
		release := e.gov.BeginInteractive(jobs.ClassRead)
		defer release()
		if got := e.gov.InteractiveActive(); got != 0 {
			t.Fatalf("an ordinary read must not hold background work: InteractiveActive = %d", got)
		}
	})

	t.Run("policy defers to the inner policy", func(t *testing.T) {
		e := newEnv(t)
		last := jobs.DequeuePolicyFunc(func(ready []jobs.Candidate) int { return len(ready) - 1 })
		pol := e.gov.Policy(last)
		if got := pol.Pick([]jobs.Candidate{ingestJob, maintJob}); got != 1 {
			t.Fatalf("Pick = %d, want the inner policy's 1", got)
		}
		release := e.gov.BeginInteractive(jobs.ClassAuthCommit)
		defer release()
		if got := pol.Pick([]jobs.Candidate{ingestJob, readJob, candidate("read2", jobs.ClassRead)}); got != 2 {
			t.Fatalf("held: Pick = %d, want the inner policy's choice among the kept jobs mapped back (2)", got)
		}
	})

	t.Run("background memory at its ceiling is held, interactive is not", func(t *testing.T) {
		e := newEnv(t)
		pol := e.gov.Policy(nil)
		e.setRSS(e.gov.backgroundLimit()+mib, nil)
		e.gov.Refresh(bg)
		if got := pol.Pick([]jobs.Candidate{ingestJob}); got != -1 {
			t.Fatalf("Pick = %d, want -1 above the background ceiling", got)
		}
		if got := pol.Pick([]jobs.Candidate{ingestJob, readJob}); got != 1 {
			t.Fatalf("Pick = %d, want the interactive job", got)
		}
		if held, why := e.gov.BackgroundHold(); !held || why != ReasonMemoryPressure {
			t.Fatalf("BackgroundHold = %v, %q", held, why)
		}
	})

	t.Run("unknown memory holds background until the next reading", func(t *testing.T) {
		e := newEnv(t)
		pol := e.gov.Policy(nil)
		e.clk.Advance(e.gov.Config().StaleAfter + time.Second)
		if got := pol.Pick([]jobs.Candidate{ingestJob}); got != -1 {
			t.Fatalf("stale reading: Pick = %d, want -1", got)
		}
		e.gov.Refresh(bg)
		if got := pol.Pick([]jobs.Candidate{ingestJob}); got != 0 {
			t.Fatalf("fresh reading: Pick = %d, want 0", got)
		}
	})

	t.Run("admit holds background but keeps the interactive reserve", func(t *testing.T) {
		e := newEnv(t)
		bgBytes := e.gov.backgroundLimit() - 200*mib - mib // fills the background share exactly (RSS is 200 MiB)
		tk, err := e.gov.Admit(bg, Request{Class: jobs.ClassIngest, Bytes: bgBytes, Label: "ingest"})
		if err != nil {
			t.Fatalf("background Admit: %v", err)
		}
		defer tk.Release()
		// Background is now at its ceiling; a live turn still fits in the reserve.
		turn, err := e.gov.Admit(bg, Request{Class: jobs.ClassTurn, Bytes: 200 * mib, Label: "turn"})
		if err != nil {
			t.Fatalf("interactive Admit with background at its ceiling: %v", err)
		}
		turn.Release()
		_, err = e.gov.Admit(bg, Request{Class: jobs.ClassIngest, Bytes: 10 * mib})
		wantCode(t, err, contracts.ErrResourceExhausted, ReasonMemoryPressure)

		release := e.gov.BeginInteractive(jobs.ClassTurn)
		tk.Release()
		_, err = e.gov.Admit(bg, Request{Class: jobs.ClassIngest, Bytes: mib})
		wantCode(t, err, contracts.ErrResourceExhausted, ReasonInteractiveActive)
		if d := e.gov.Check(Request{Class: jobs.ClassMaintenance, Bytes: mib}); d.Verdict != Hold {
			t.Fatalf("Check during a turn = %v, want hold", d.Verdict)
		}
		release()
		if tk2, err := e.gov.Admit(bg, Request{Class: jobs.ClassIngest, Bytes: mib}); err != nil {
			t.Fatalf("after the turn: %v", err)
		} else {
			tk2.Release()
		}
	})

	t.Run("a running handler yields until the turn ends", func(t *testing.T) {
		e := newEnv(t)
		release := e.gov.BeginInteractive(jobs.ClassTurn)
		e.onSleep = func(n int) {
			if n == 3 {
				release()
			}
		}
		if err := e.gov.Yield(bg, jobs.ClassIngest); err != nil {
			t.Fatalf("Yield: %v", err)
		}
		if got := e.sleepCount(); got != 3 {
			t.Fatalf("slept %d times, want 3", got)
		}
		if err := e.gov.Yield(bg, jobs.ClassIngest); err != nil || e.sleepCount() != 3 {
			t.Fatalf("idle Yield must return at once: err=%v sleeps=%d", err, e.sleepCount())
		}
	})

	t.Run("a handler that cannot get the turns out of the way gives up", func(t *testing.T) {
		e := newEnv(t, func(c *Config) { c.YieldMaxWait, c.YieldPoll = time.Second, 100*time.Millisecond })
		release := e.gov.BeginInteractive(jobs.ClassTurn)
		defer release()
		err := e.gov.Yield(bg, jobs.ClassMaintenance)
		wantCode(t, err, contracts.ErrResourceExhausted, ReasonYieldTimeout)
		if got := e.sleepCount(); got < 9 || got > 11 {
			t.Fatalf("slept %d times, want about YieldMaxWait/YieldPoll = 10", got)
		}
		if got := e.gov.Stats().YieldTimeouts; got != 1 {
			t.Fatalf("YieldTimeouts = %d, want 1", got)
		}
		if err := e.gov.Yield(bg, jobs.ClassTurn); err != nil {
			t.Fatalf("an interactive class never yields: %v", err)
		}
	})

	t.Run("the real queue leases no background job while a turn is live", func(t *testing.T) {
		clk := clock.NewFake(epoch)
		e := newEnv(t)
		db := storetest.Open(t, nil)
		q, err := jobs.NewQueue(db, jobs.Options{Clock: clk, Policy: e.gov.Policy(nil)})
		if err != nil {
			t.Fatalf("NewQueue: %v", err)
		}
		for _, spec := range []jobs.TypeSpec{{Name: "bg.ingest", Class: jobs.ClassIngest}, {Name: "bg.read", Class: jobs.ClassRead}} {
			if err := q.Register(spec); err != nil {
				t.Fatalf("Register: %v", err)
			}
		}
		scope := storetest.Scope("app", "tenant", "ws")
		lease := func() int {
			got, err := q.Lease(bg, jobs.LeaseRequest{Classes: jobs.Classes(), Max: 5})
			if err != nil {
				t.Fatalf("Lease: %v", err)
			}
			return len(got)
		}
		if _, err := q.Enqueue(bg, scope, jobs.EnqueueRequest{Type: "bg.ingest", Payload: []byte(`{}`)}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		release := e.gov.BeginInteractive(jobs.ClassTurn)
		if n := lease(); n != 0 {
			t.Fatalf("leased %d background jobs during a live turn", n)
		}
		if _, err := q.Enqueue(bg, scope, jobs.EnqueueRequest{Type: "bg.read", Payload: []byte(`{}`)}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if n := lease(); n != 1 {
			t.Fatalf("leased %d jobs during a live turn, want only the class-4 read", n)
		}
		release()
		if n := lease(); n != 1 {
			t.Fatalf("leased %d after the turn, want the waiting ingest job", n)
		}
	})

	t.Run("composes with the ingestion pause", func(t *testing.T) {
		e := newEnv(t)
		diskLow := false
		pause := ingest.NewPause(ingest.DiskPressureFunc(func(context.Context) (bool, error) { return diskLow, nil }), e.clk, time.Second, nil)
		pol := e.gov.Policy(pause.Policy(nil))
		job := jobs.Candidate{ID: "j", Class: jobs.ClassIngest, Type: ingest.PausedJobTypes()[0]}
		if got := pol.Pick([]jobs.Candidate{job}); got != 0 {
			t.Fatalf("both clear: Pick = %d", got)
		}
		diskLow = true
		e.clk.Advance(2 * time.Second)
		if got := pol.Pick([]jobs.Candidate{job}); got != -1 {
			t.Fatalf("disk pressure: Pick = %d, want -1", got)
		}
		diskLow = false
		e.clk.Advance(2 * time.Second)
		e.gov.Refresh(bg)
		release := e.gov.BeginInteractive(jobs.ClassTurn)
		defer release()
		if got := pol.Pick([]jobs.Candidate{job}); got != -1 {
			t.Fatalf("live turn: Pick = %d, want -1", got)
		}
	})
}

// SEC-077
// over-budget work is rejected before memory is exhausted, with a
// typed error, never a silent drop.
func TestAdmissionRejectsOverBudget(t *testing.T) {
	t.Run("live work over the budget is rejected, typed", func(t *testing.T) {
		e := newEnv(t) // budget 1024 MiB, RSS 200 MiB
		e.rt.set(300 * mib)
		e.gov.Refresh(bg) // provider-reported runtime memory: 300 MiB

		first, err := e.gov.Admit(bg, Request{Class: jobs.ClassTurn, Bytes: 400 * mib, Label: "turn"})
		if err != nil {
			t.Fatalf("400 MiB turn (200 + 300 + 400 = 900 of 1024): %v", err)
		}
		_, err = e.gov.Admit(bg, Request{Class: jobs.ClassTurn, Bytes: 200 * mib, Label: "turn"})
		wantCode(t, err, contracts.ErrResourceExhausted, ReasonOverBudget)
		if got := e.gov.Stats().ReservedBytes; got != 400*mib {
			t.Fatalf("a rejected request must reserve nothing: reserved = %d MiB", got/mib)
		}
		first.Release()
		first.Release() // harmless
		second, err := e.gov.Admit(bg, Request{Class: jobs.ClassTurn, Bytes: 200 * mib})
		if err != nil {
			t.Fatalf("after release: %v", err)
		}
		second.Release()
		if st := e.gov.Stats(); st.Rejected != 1 || st.Admitted != 2 || st.ReservedBytes != 0 {
			t.Fatalf("stats = %+v", st)
		}
	})

	t.Run("provider-reported memory counts", func(t *testing.T) {
		e := newEnv(t)
		req := Request{Class: jobs.ClassTurn, Bytes: 300 * mib}
		if d := e.gov.Check(req); d.Verdict != Admit {
			t.Fatalf("empty runtime: %v", d.Verdict)
		}
		e.rt.set(700 * mib) // the runtime now holds 700 MiB: 200 + 700 + 300 > 1024
		e.gov.Refresh(bg)
		if d := e.gov.Check(req); d.Verdict != Reject || d.Reason != ReasonOverBudget || d.Used != 900*mib {
			t.Fatalf("decision = %+v", d)
		}
	})

	t.Run("a request larger than the whole budget is rejected for good", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.gov.Admit(bg, Request{Class: jobs.ClassTurn, Bytes: 2 * gib})
		wantCode(t, err, contracts.ErrResourceExhausted, ReasonExceedsBudget)
		// Background work too big for its share can never fit by waiting either.
		_, err = e.gov.Admit(bg, Request{Class: jobs.ClassIngest, Bytes: gib})
		wantCode(t, err, contracts.ErrResourceExhausted, ReasonExceedsBudget)
	})

	t.Run("background work over its share is held, not dropped", func(t *testing.T) {
		e := newEnv(t)
		e.rt.set(300 * mib)
		e.gov.Refresh(bg)
		_, err := e.gov.Admit(bg, Request{Class: jobs.ClassIngest, Bytes: 300 * mib}) // 500 + 300 > 716 MiB
		wantCode(t, err, contracts.ErrResourceExhausted, ReasonMemoryPressure)
		var ce *contracts.Error
		if !errors.As(err, &ce) || !ce.Retryable || ce.Details["verdict"] != "hold" {
			t.Fatalf("a held request must be retryable and say it is held: %+v", ce)
		}
	})

	t.Run("concurrent admits cannot all pass on one reading", func(t *testing.T) {
		e := newEnv(t) // 824 MiB free of 1024
		const workers = 12
		var wg sync.WaitGroup
		var mu sync.Mutex
		var tickets []*Ticket
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if tk, err := e.gov.Admit(bg, Request{Class: jobs.ClassTurn, Bytes: 200 * mib}); err == nil {
					mu.Lock()
					tickets = append(tickets, tk)
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(tickets) != 4 {
			t.Fatalf("admitted %d of %d 200 MiB requests with 824 MiB free, want 4", len(tickets), workers)
		}
	})

	t.Run("an unmeasurable process fails closed", func(t *testing.T) {
		e := newEnv(t)
		e.setRSS(0, context.DeadlineExceeded)
		e.clk.Advance(e.gov.Config().ProbeInterval) // the reading is old enough to be re-taken
		// The last good value is kept for StaleAfter, so admission still works...
		if _, err := e.gov.Admit(bg, Request{Class: jobs.ClassTurn, Bytes: mib}); err != nil {
			t.Fatalf("within StaleAfter: %v", err)
		}
		// ...and after it, nothing is judged on a guess.
		e.clk.Advance(e.gov.Config().StaleAfter)
		_, err := e.gov.Admit(bg, Request{Class: jobs.ClassTurn, Bytes: mib})
		wantCode(t, err, contracts.ErrUnavailable, ReasonMemoryUnknown)
		_, err = e.gov.Admit(bg, Request{Class: jobs.ClassIngest, Bytes: mib})
		wantCode(t, err, contracts.ErrUnavailable, ReasonMemoryUnknown)
		if _, err := e.gov.Admit(bg, Request{Class: 0, Bytes: mib}); err == nil {
			t.Fatal("an invalid class was admitted")
		}
	})

	t.Run("a runtime that stops answering holds background only", func(t *testing.T) {
		e := newEnv(t)
		e.rt.mu.Lock()
		e.rt.reportErr = context.DeadlineExceeded
		e.rt.mu.Unlock()
		e.clk.Advance(e.gov.Config().StaleAfter + time.Second)
		e.setRSS(200*mib, nil)
		e.gov.Refresh(bg)
		// Search and ordinary reads stay usable when the LLM runtime is down (18).
		if tk, err := e.gov.Admit(bg, Request{Class: jobs.ClassRead, Bytes: mib}); err != nil {
			t.Fatalf("interactive Admit with the runtime down: %v", err)
		} else {
			tk.Release()
		}
		_, err := e.gov.Admit(bg, Request{Class: jobs.ClassIngest, Bytes: mib})
		wantCode(t, err, contracts.ErrUnavailable, ReasonMemoryUnknown)
	})

	t.Run("admission never unloads a model to make room", func(t *testing.T) {
		e := newEnv(t)
		e.rt.set(900*mib, LoadedModel{ID: "controller", Bytes: 600 * mib, LoadedAt: epoch.Add(-time.Hour), LastUsed: epoch.Add(-time.Hour)},
			LoadedModel{ID: "reranker", Bytes: 300 * mib, LoadedAt: epoch.Add(-time.Hour), LastUsed: epoch.Add(-time.Hour)})
		e.gov.Refresh(bg)
		for _, class := range []jobs.Class{jobs.ClassTurn, jobs.ClassIngest} {
			if _, err := e.gov.Admit(bg, Request{Class: class, Bytes: 500 * mib}); err == nil {
				t.Fatalf("class %s: a 500 MiB request fits with 900 MiB held?", class)
			}
		}
		if got := e.rt.unloads(); len(got) != 0 {
			t.Fatalf("admission unloaded %v; it must reject, never evict", got)
		}
	})

	t.Run("a model load is gated by role and never evicts", func(t *testing.T) {
		e := newEnv(t) // RSS 200 MiB; background share 716 MiB
		_, err := e.gov.AdmitModelLoad(bg, ModelLoad{ID: "scorer", Role: "router", Bytes: 600 * mib})
		wantCode(t, err, contracts.ErrResourceExhausted, ReasonOverBudget)
		tk, err := e.gov.AdmitModelLoad(bg, ModelLoad{ID: "controller", Role: "controller", Bytes: 600 * mib})
		if err != nil {
			t.Fatalf("the controller may use the whole budget: %v", err)
		}
		// While the controller's load is reserved, an optional worker has no room at all.
		_, err = e.gov.AdmitModelLoad(bg, ModelLoad{ID: "scorer", Role: "router", Bytes: 100 * mib})
		wantCode(t, err, contracts.ErrResourceExhausted, ReasonOverBudget)
		tk.Release()
		if got := e.rt.unloads(); len(got) != 0 {
			t.Fatalf("a load gate unloaded %v", got)
		}
		// A model load does not wait for a live turn.
		release := e.gov.BeginInteractive(jobs.ClassTurn)
		defer release()
		tk, err = e.gov.AdmitModelLoad(bg, ModelLoad{ID: "embedder", Role: "embedder", Bytes: 100 * mib})
		if err != nil {
			t.Fatalf("model load during a turn: %v", err)
		}
		tk.Release()
	})
}

func TestZeroByteLiveWorkIsNeverRefusedForMemory(t *testing.T) {
	e := newEnv(t)
	e.rt.set(2 * gib) // the runtime alone is over the budget
	e.gov.Refresh(bg)
	for _, class := range []jobs.Class{jobs.ClassAuthCommit, jobs.ClassTurn, jobs.ClassRead} {
		tk, err := e.gov.Admit(bg, Request{Class: class})
		if err != nil {
			t.Fatalf("class %s: a commit or read that reserves nothing must be admitted: %v", class, err)
		}
		tk.Release()
	}
	if _, err := e.gov.Admit(bg, Request{Class: jobs.ClassTurn, Bytes: mib}); err == nil {
		t.Fatal("a turn that needs memory must still be rejected")
	}
	if _, err := e.gov.Admit(bg, Request{Class: jobs.ClassIngest}); err == nil {
		t.Fatal("background work is held while memory is over its share, even at zero bytes")
	}
}
