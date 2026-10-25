package jobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// gate is a set of handlers whose runs block until released, and which record
// how many of each name ran at once.
type gate struct {
	release map[string]chan struct{} // one send releases one waiting handler
	openAll chan struct{}            // closed: every handler passes
	entered chan string

	mu  sync.Mutex
	cur map[string]int
	max map[string]int
}

func newGate(names ...string) *gate {
	g := &gate{release: map[string]chan struct{}{}, openAll: make(chan struct{}), entered: make(chan string, 64), cur: map[string]int{}, max: map[string]int{}}
	for _, n := range names {
		g.release[n] = make(chan struct{})
	}
	return g
}

func (g *gate) handler(name string) Handler {
	return func(ctx context.Context, _ Job) error {
		g.mu.Lock()
		g.cur[name]++
		g.max[name] = max(g.max[name], g.cur[name])
		g.mu.Unlock()
		g.entered <- name
		defer func() {
			g.mu.Lock()
			g.cur[name]--
			g.mu.Unlock()
		}()
		select {
		case <-g.release[name]:
			return nil
		case <-g.openAll:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *gate) maxOf(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.max[name]
}

func (g *gate) waitEntered(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-g.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d handlers started", i, n)
		}
	}
}

// SEC-078
func TestWorkerConcurrencyLimitPerClass(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Config.Class4 = ClassConfig{Workers: 1, MaxDepth: 100}
		o.Config.Class5 = ClassConfig{Workers: 2, MaxDepth: 100}
	})
	ph := h.newPool(t)
	g := newGate("read", "ingest")
	ph.handle(t, typeOf(ClassRead), g.handler("read"))
	ph.handle(t, typeOf(ClassIngest), g.handler("ingest"))
	a := scopeA()

	var ingest, reads []Job
	for i := 0; i < 5; i++ {
		ingest = append(ingest, h.enqueue(a, typeOf(ClassIngest)))
	}
	for i := 0; i < 3; i++ {
		reads = append(reads, h.enqueue(a, typeOf(ClassRead)))
	}

	// Two ingest workers and one read worker start; nothing more, however much waits.
	if n := ph.poll(t); n != 3 {
		t.Fatalf("first Poll started %d jobs, want 3 (1 read + 2 ingest)", n)
	}
	g.waitEntered(t, 3)
	if n := ph.poll(t); n != 0 {
		t.Fatalf("second Poll started %d jobs with every worker busy", n)
	}
	if r4, r5 := ph.pool.Running(ClassRead), ph.pool.Running(ClassIngest); r4 != 1 || r5 != 2 {
		t.Fatalf("running read=%d ingest=%d, want 1 and 2", r4, r5)
	}
	st, err := h.q.Status(bg)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Classes[ClassIngest-1]; got.Running != 2 || got.Pending != 3 {
		t.Fatalf("ingest status running=%d pending=%d, want 2 and 3", got.Running, got.Pending)
	}

	// Releasing one ingest handler frees exactly one ingest slot, and not a read slot.
	g.release["ingest"] <- struct{}{}
	waitFor(t, "an ingest slot to free", func() bool { return ph.pool.Running(ClassIngest) == 1 })
	if n := ph.poll(t); n != 1 {
		t.Fatalf("Poll after one completion started %d jobs, want 1", n)
	}
	g.waitEntered(t, 1)
	if r4, r5 := ph.pool.Running(ClassRead), ph.pool.Running(ClassIngest); r4 != 1 || r5 != 2 {
		t.Fatalf("running read=%d ingest=%d, want 1 and 2", r4, r5)
	}

	// Drain everything; the limits held throughout.
	close(g.openAll)
	waitFor(t, "every job to finish", func() bool {
		ph.poll(t)
		for _, j := range append(ingest, reads...) {
			if h.get(a, j.ID).State != StateSucceeded {
				return false
			}
		}
		return true
	})
	for _, j := range append(ingest, reads...) {
		if got := h.waitState(a, j.ID, StateSucceeded); got.Attempts != 1 {
			t.Fatalf("job %s ran %d times", j.ID, got.Attempts)
		}
	}
	if m := g.maxOf("ingest"); m != 2 {
		t.Fatalf("at most %d ingest jobs ran at once, want exactly the limit of 2", m)
	}
	if m := g.maxOf("read"); m != 1 {
		t.Fatalf("at most %d read jobs ran at once, want the limit of 1", m)
	}
}

// SEC-078
func TestPoolBackgroundYieldsToInteractiveJobs(t *testing.T) {
	h := newHarness(t)
	// This pool serves only the background class; the interactive job waits for
	// another worker (here: nobody yet).
	ph := h.newPool(t)
	var ran atomic.Int32
	ph.handle(t, typeOf(ClassIngest), func(context.Context, Job) error { ran.Add(1); return nil })
	a := scopeA()
	ing := h.enqueue(a, typeOf(ClassIngest))
	urgent := h.enqueue(a, typeOf(ClassAuthCommit))

	if n := ph.poll(t); n != 0 {
		t.Fatalf("background worker started %d job(s) while a class-1 job waited", n)
	}
	if got := h.get(a, ing.ID); got.State != StatePending || ran.Load() != 0 {
		t.Fatalf("background job moved to %s", got.State)
	}
	// Someone serves the interactive job; the background worker resumes.
	h.leaseOne(ClassAuthCommit)
	_ = urgent
	if n := ph.poll(t); n != 1 {
		t.Fatalf("background worker did not resume: started %d", n)
	}
	h.waitState(a, ing.ID, StateSucceeded)
}

func TestCancelRunningSignalsContext(t *testing.T) {
	h := newHarness(t)
	ph := h.newPool(t)
	entered := make(chan struct{})
	var cause error
	var ctxErr error
	finished := make(chan struct{})
	ph.handle(t, typeOf(ClassIngest), func(ctx context.Context, _ Job) error {
		close(entered)
		<-ctx.Done()
		cause, ctxErr = context.Cause(ctx), ctx.Err()
		defer close(finished)
		return ctx.Err()
	})
	a := scopeA()
	j := h.enqueue(a, typeOf(ClassIngest))
	if n := ph.poll(t); n != 1 {
		t.Fatalf("started %d jobs", n)
	}
	<-entered

	got, err := h.q.Cancel(bg, a, j.ID)
	if err != nil || got.State != StateRunning || !got.CancelRequested {
		t.Fatalf("Cancel of a running job = %+v, %v; want running with the request recorded", got, err)
	}
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler's context was not cancelled")
	}
	if !errors.Is(cause, errJobCancelled) || !errors.Is(ctxErr, context.Canceled) {
		t.Fatalf("context cause = %v, err = %v; want the job-cancelled cause", cause, ctxErr)
	}
	// The job ends cancelled: no retry, no failure code, one attempt.
	final := h.waitState(a, j.ID, StateCancelled)
	if final.Attempts != 1 || final.ErrorCode != "" || final.FinishedAt.IsZero() {
		t.Fatalf("cancelled job = %+v", final)
	}
	waitFor(t, "the worker slot to free", func() bool { return ph.pool.Running(ClassIngest) == 0 })
	if n := ph.poll(t); n != 0 {
		t.Fatalf("a cancelled job was started again")
	}

	t.Run("a cancel that arrives from another process reaches the handler through the heartbeat", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.Config.LeaseTTL = 6 * time.Millisecond }) // heartbeat every 2 ms
		ph := h.newPool(t)
		entered := make(chan struct{})
		stopped := make(chan error, 1)
		ph.handle(t, typeOf(ClassIngest), func(ctx context.Context, _ Job) error {
			close(entered)
			<-ctx.Done()
			stopped <- context.Cause(ctx)
			return ctx.Err()
		})
		j := h.enqueue(a, typeOf(ClassIngest))
		ph.poll(t)
		<-entered
		// Set the flag directly, as another process would, without signalling in-process.
		if _, err := h.db.ExecContext(bg, `UPDATE jobs SET cancel_requested = 1 WHERE id = ?`, j.ID); err != nil {
			t.Fatal(err)
		}
		select {
		case c := <-stopped:
			if !errors.Is(c, errJobCancelled) {
				t.Fatalf("cause = %v", c)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("heartbeat did not deliver the cancel")
		}
		h.waitState(a, j.ID, StateCancelled)
	})
}

// A worker that lost its lease, while the reaped job runs again on another
// worker of the same pool, does not take the new run's cancel with it when it
// ends: Cancel still reaches the new run at once.
func TestCancelReachesTheRunAfterAStaleWorkerEnds(t *testing.T) {
	h := newHarness(t)
	_, cancelA := context.WithCancelCause(bg)
	defer cancelA(nil)
	ctxB, cancelB := context.WithCancelCause(bg)
	defer cancelB(nil)
	untrackA := h.q.track("job_1", cancelA)
	untrackB := h.q.track("job_1", cancelB)
	defer untrackB()
	untrackA()
	h.q.signalCancel("job_1")
	if !errors.Is(context.Cause(ctxB), errJobCancelled) {
		t.Fatalf("the current run's cause = %v, want the job-cancelled cause", context.Cause(ctxB))
	}
}

// SEC-027
func TestRevokedScopeFailsJob(t *testing.T) {
	type outcome struct {
		state State
		code  contracts.ErrCode
		ran   bool
	}
	run := func(t *testing.T, setup func(*poolHarness), enq func(*EnqueueRequest), opts ...func(*PoolOptions)) (outcome, Job) {
		t.Helper()
		h := newHarness(t)
		ph := h.newPool(t, opts...)
		var ran atomic.Bool
		ph.handle(t, typeOf(ClassIngest), func(context.Context, Job) error { ran.Store(true); return nil })
		if setup != nil {
			setup(ph)
		}
		a := scopeA()
		j := h.enqueue(a, typeOf(ClassIngest), enq)
		ph.poll(t)
		waitFor(t, "the job to leave running", func() bool {
			g := h.get(a, j.ID)
			return g.State != StateRunning && ph.pool.Running(ClassIngest) == 0
		})
		g := h.get(a, j.ID)
		return outcome{g.State, g.ErrorCode, ran.Load()}, g
	}
	revoked := func(ph *poolHarness) {
		ph.setChecker(contracts.New(contracts.ErrUnauthenticated, "session revoked"))
	}

	t.Run("a revoked scope fails the job and the handler never runs", func(t *testing.T) {
		got, job := run(t, revoked, nil)
		if got != (outcome{StateFailed, contracts.ErrPermissionDenied, false}) {
			t.Fatalf("outcome = %+v", got)
		}
		if job.Attempts != 1 || job.ErrorMessage == "" {
			t.Fatalf("job = %+v", job)
		}
		// The checker's reason is not copied into the record.
		if job.ErrorMessage == "session revoked" {
			t.Fatalf("checker detail leaked: %q", job.ErrorMessage)
		}
	})

	t.Run("a still-valid scope runs", func(t *testing.T) {
		got, _ := run(t, nil, nil)
		if got != (outcome{StateSucceeded, "", true}) {
			t.Fatalf("outcome = %+v", got)
		}
	})

	t.Run("the checker receives the initiating scope", func(t *testing.T) {
		h := newHarness(t)
		var seen contracts.Scope
		ph := h.newPool(t, func(po *PoolOptions) {
			po.ScopeChecker = ScopeCheckerFunc(func(_ context.Context, s contracts.Scope) error { seen = s; return nil })
		})
		ph.handle(t, typeOf(ClassIngest), func(context.Context, Job) error { return nil })
		a := scopeA()
		a.PrincipalID, a.SessionID, a.Roles = "usr_initiator", "ses_initiator", []string{"editor"}
		j := h.enqueue(a, typeOf(ClassIngest))
		ph.poll(t)
		h.waitState(a, j.ID, StateSucceeded)
		if seen.PrincipalID != "usr_initiator" || seen.SessionID != "ses_initiator" || len(seen.Roles) != 1 || seen.Roles[0] != "editor" || seen.TenantID != "tenantA" {
			t.Fatalf("checker saw %+v, want the full initiating scope", seen)
		}
	})

	t.Run("any non-retryable checker error denies, including a plain error", func(t *testing.T) {
		got, _ := run(t, func(ph *poolHarness) { ph.setChecker(errPlain) }, nil)
		if got != (outcome{StateFailed, contracts.ErrPermissionDenied, false}) {
			t.Fatalf("outcome = %+v", got)
		}
	})

	t.Run("a checker that cannot decide runs nothing and retries later", func(t *testing.T) {
		got, job := run(t, func(ph *poolHarness) { ph.setChecker(contracts.New(contracts.ErrUnavailable, "identity down")) }, nil)
		if got != (outcome{StatePending, contracts.ErrUnavailable, false}) {
			t.Fatalf("outcome = %+v; the job must wait, not run and not be denied", got)
		}
		if !job.RunAt.After(epoch) {
			t.Fatalf("no backoff recorded: run_at %s", job.RunAt)
		}
	})

	t.Run("a declared service identity that is authorised runs without the principal's scope", func(t *testing.T) {
		var asked atomic.Value
		auth := ServiceAuthorizerFunc(func(_ context.Context, id string, j Job) error {
			asked.Store(id + "/" + j.Type)
			return nil
		})
		got, _ := run(t, revoked, func(r *EnqueueRequest) { r.ServiceIdentity = "svc.ingest" },
			func(po *PoolOptions) { po.ServiceAuthorizer = auth })
		if got != (outcome{StateSucceeded, "", true}) {
			t.Fatalf("outcome = %+v", got)
		}
		if asked.Load() != "svc.ingest/"+typeOf(ClassIngest) {
			t.Fatalf("authorizer asked %v", asked.Load())
		}
	})

	t.Run("a declared service identity that is refused is denied", func(t *testing.T) {
		refuse := ServiceAuthorizerFunc(func(context.Context, string, Job) error {
			return contracts.New(contracts.ErrPermissionDenied, "not allowed")
		})
		got, _ := run(t, revoked, func(r *EnqueueRequest) { r.ServiceIdentity = "svc.rogue" },
			func(po *PoolOptions) { po.ServiceAuthorizer = refuse })
		if got != (outcome{StateFailed, contracts.ErrPermissionDenied, false}) {
			t.Fatalf("outcome = %+v", got)
		}
	})

	t.Run("a service authorizer that cannot decide runs nothing and retries later", func(t *testing.T) {
		down := ServiceAuthorizerFunc(func(context.Context, string, Job) error {
			return contracts.New(contracts.ErrUnavailable, "identity down")
		})
		got, _ := run(t, revoked, func(r *EnqueueRequest) { r.ServiceIdentity = "svc.ingest" },
			func(po *PoolOptions) { po.ServiceAuthorizer = down })
		if got != (outcome{StatePending, contracts.ErrUnavailable, false}) {
			t.Fatalf("outcome = %+v; an outage must not permanently deny a service job", got)
		}
	})

	t.Run("a refused identity is denied even when the principal's scope is still valid", func(t *testing.T) {
		refuse := ServiceAuthorizerFunc(func(context.Context, string, Job) error { return errPlain })
		got, _ := run(t, nil, func(r *EnqueueRequest) { r.ServiceIdentity = "svc.rogue" },
			func(po *PoolOptions) { po.ServiceAuthorizer = refuse })
		if got != (outcome{StateFailed, contracts.ErrPermissionDenied, false}) {
			t.Fatalf("outcome = %+v; running under an authority the job did not hold would fail open", got)
		}
	})

	t.Run("naming a service identity is not enough when no authorizer is configured", func(t *testing.T) {
		got, _ := run(t, revoked, func(r *EnqueueRequest) { r.ServiceIdentity = "svc.ingest" })
		if got != (outcome{StateFailed, contracts.ErrPermissionDenied, false}) {
			t.Fatalf("outcome = %+v", got)
		}
	})

	t.Run("a pool cannot be built without a checker", func(t *testing.T) {
		h := newHarness(t)
		_, err := NewPool(h.q, PoolOptions{})
		codeIs(t, err, contracts.ErrValidationFailed)
	})
}

func TestRunningJobDeadlineCancelsContext(t *testing.T) {
	h := newHarness(t)
	ph := h.newPool(t)
	cause := make(chan error, 1)
	ph.handle(t, "retry.job", func(ctx context.Context, _ Job) error {
		<-ctx.Done()
		cause <- context.Cause(ctx)
		// A handler that reports its cancellation as a retryable error must
		// still not be retried: the deadline passed.
		return contracts.New(contracts.ErrUnavailable, "interrupted")
	})
	a := scopeA()
	j := h.enqueue(a, "retry.job", func(r *EnqueueRequest) { r.Deadline = epoch.Add(5 * time.Millisecond) })
	ph.poll(t)
	select {
	case c := <-cause:
		if !errors.Is(c, context.DeadlineExceeded) {
			t.Fatalf("cause = %v, want the deadline", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handler context never reached the deadline")
	}
	got := h.waitState(a, j.ID, StateFailed)
	if got.ErrorCode != contracts.ErrTimeout || got.Attempts != 1 {
		t.Fatalf("job = %s attempts %d, want timeout after one attempt", got.ErrorCode, got.Attempts)
	}
}

func TestPoolRetriesThenSucceeds(t *testing.T) {
	h := newHarness(t)
	ph := h.newPool(t)
	var calls atomic.Int32
	ph.handle(t, "retry.job", func(_ context.Context, j Job) error {
		n := calls.Add(1)
		if j.Attempts != int(n) {
			t.Errorf("handler saw attempt %d on call %d", j.Attempts, n)
		}
		if n < 3 {
			return contracts.New(contracts.ErrUnavailable, "try again")
		}
		return nil
	})
	a := scopeA()
	j := h.enqueue(a, "retry.job")

	for step := 1; step <= 3; step++ {
		waitFor(t, "slot", func() bool { return ph.pool.Running(ClassRAGQuery) == 0 })
		if n := ph.poll(t); n != 1 {
			t.Fatalf("step %d: Poll started %d jobs", step, n)
		}
		waitFor(t, "attempt to settle", func() bool {
			g := h.get(a, j.ID)
			return g.State != StateRunning && ph.pool.Running(ClassRAGQuery) == 0
		})
		if step < 3 {
			// Not due yet: a Poll now starts nothing.
			if n := ph.poll(t); n != 0 {
				t.Fatalf("step %d: retried before its backoff", step)
			}
			h.clk.Advance(30 * time.Second)
		}
	}
	if got := h.get(a, j.ID); got.State != StateSucceeded || got.Attempts != 3 || got.ErrorCode != "" {
		t.Fatalf("job = %s attempts %d code %q", got.State, got.Attempts, got.ErrorCode)
	}
}

func TestPoolMaintenanceReapsExpiredLeases(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	j := h.enqueue(a, typeOf(ClassIngest))
	h.leaseOne(ClassIngest) // a worker that then dies
	var ran atomic.Int32
	ph := h.newPool(t)
	ph.handle(t, typeOf(ClassIngest), func(context.Context, Job) error { ran.Add(1); return nil })

	if n := ph.poll(t); n != 0 {
		t.Fatalf("started %d jobs while the dead worker's lease was valid", n)
	}
	h.clk.Advance(time.Minute)
	if n := ph.poll(t); n != 1 { // the maintenance pass requeues it, the same Poll runs it
		t.Fatalf("Poll after the lease expired started %d jobs, want 1", n)
	}
	got := h.waitState(a, j.ID, StateSucceeded)
	if got.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", got.Attempts)
	}
}

func TestPoolHandlerPanicFailsTheJobNotTheProcess(t *testing.T) {
	h := newHarness(t)
	ph := h.newPool(t)
	ph.handle(t, typeOf(ClassIngest), func(context.Context, Job) error { panic("secret: boom") })
	a := scopeA()
	j := h.enqueue(a, typeOf(ClassIngest))
	ph.poll(t)
	got := h.waitState(a, j.ID, StateFailed)
	if got.ErrorCode != contracts.ErrInternal || got.Attempts != 1 {
		t.Fatalf("job = %s attempts %d", got.ErrorCode, got.Attempts)
	}
	if got.ErrorMessage == "" || got.ErrorMessage == "secret: boom" {
		t.Fatalf("message %q must not carry the panic value", got.ErrorMessage)
	}
}

func TestPoolShutdownReleasesRunningJobs(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Config.ShutdownGrace = 5 * time.Second })
	ph := h.newPool(t)
	g := newGate("ingest")
	ph.handle(t, typeOf(ClassIngest), g.handler("ingest"))
	a := scopeA()
	j := h.enqueue(a, typeOf(ClassIngest))
	ph.poll(t)
	g.waitEntered(t, 1)

	sctx, cancel := context.WithTimeout(bg, 10*time.Millisecond)
	defer cancel()
	if err := ph.pool.Shutdown(sctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// The interrupted job is pending again and the attempt was not used up.
	got := h.get(a, j.ID)
	if got.State != StatePending || got.Attempts != 0 {
		t.Fatalf("after shutdown job is %s with %d attempts, want pending with 0", got.State, got.Attempts)
	}
	// A stopped pool starts nothing.
	if n, err := ph.pool.Poll(bg); n != 0 || err != nil {
		t.Fatalf("Poll after Shutdown = %d, %v", n, err)
	}
}

func TestPoolRunProcessesEnqueuedJobsWithoutPolling(t *testing.T) {
	// A long poll interval: the job can only start through the enqueue wake-up.
	h := newHarness(t, func(o *Options) { o.Config.PollInterval = time.Minute })
	ph := h.newPool(t)
	done := make(chan Job, 1)
	ph.handle(t, typeOf(ClassTurn), func(_ context.Context, j Job) error { done <- j; return nil })

	rctx, cancel := context.WithCancel(bg)
	exited := make(chan error, 1)
	go func() { exited <- ph.pool.Run(rctx) }()

	a := scopeA()
	j := h.enqueue(a, typeOf(ClassTurn))
	select {
	case got := <-done:
		if got.ID != j.ID || got.Scope.TenantID != "tenantA" || string(got.Payload) != `{"n":1}` {
			t.Fatalf("handler got %+v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("enqueue did not wake the pool")
	}
	h.waitState(a, j.ID, StateSucceeded)
	cancel()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

func TestPoolHandleValidation(t *testing.T) {
	h := newHarness(t)
	ph := h.newPool(t)
	ok := func(context.Context, Job) error { return nil }
	codeIs(t, ph.pool.Handle("unregistered.job", ok), contracts.ErrValidationFailed)
	codeIs(t, ph.pool.Handle(typeOf(ClassRead), nil), contracts.ErrValidationFailed)
	if err := ph.pool.Handle(typeOf(ClassRead), ok); err != nil {
		t.Fatal(err)
	}
	codeIs(t, ph.pool.Handle(typeOf(ClassRead), ok), contracts.ErrConflict)

	// A pool leases only the types it handles.
	a := scopeA()
	other := h.enqueue(a, typeOf(ClassIngest))
	if n := ph.poll(t); n != 0 {
		t.Fatalf("pool started a job of a type it has no handler for")
	}
	if got := h.get(a, other.ID); got.State != StatePending {
		t.Fatalf("job is %s", got.State)
	}
}
