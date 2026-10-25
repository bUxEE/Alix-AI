package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

var (
	errShutdown  = errors.New("jobs: pool shutting down")
	errLeaseLost = errors.New("jobs: lease lost")
)

const (
	// finalizeTimeout bounds the write that records a job's result. It runs on
	// a context detached from the job's own, which is usually already cancelled
	// when the result is a cancellation or a shutdown.
	finalizeTimeout = 10 * time.Second
	// maintainDivisor makes the maintenance pass run every LeaseTTL/4.
	maintainDivisor = 4
	pruneEvery      = time.Hour
)

// PoolOptions is what NewPool needs besides the queue.
type PoolOptions struct {
	// ScopeChecker re-validates a job's initiating scope before it runs. It is
	// required: a Pool with no checker would run work for principals whose
	// access may have been revoked, so NewPool refuses to start without one.
	ScopeChecker ScopeChecker
	// ServiceAuthorizer accepts declared service identities. Nil means no
	// service identity is accepted, so a job that declares one is denied.
	ServiceAuthorizer ServiceAuthorizer
	// Logger overrides the queue's logger.
	Logger *slog.Logger
}

// Pool runs jobs: one bounded set of workers per priority class. A job is
// leased only when its class has a free worker, so the per-class limits of
// Config hold however much is queued, and a saturated class never blocks
// another. Workers are goroutines started per job, not idle threads; an idle
// Pool is one goroutine waking every PollInterval (or immediately when a job is
// enqueued in this process).
//
// Before a handler runs the Pool checks the job's deadline (a job past it
// fails with timeout) and its initiating scope: the ScopeChecker must accept
// it, or the job fails with permission_denied and the handler is never called.
// A job that declares a service identity must have it accepted by the
// ServiceAuthorizer instead; the principal's scope is then not consulted,
// and a declared identity that is not accepted is a denial, never a fallback.
// A checker or authorizer error whose code is retryable (it could not decide)
// does not run the job either: the attempt is retried with backoff.
type Pool struct {
	q       *Queue
	checker ScopeChecker
	svc     ServiceAuthorizer
	log     *slog.Logger

	pollMu sync.Mutex // serialises Poll

	mu        sync.Mutex
	handlers  map[string]Handler
	types     [numClasses + 1][]string // handled types, per class
	running   [numClasses + 1]int
	stopping  bool
	nextMaint time.Time
	nextPrune time.Time

	base       context.Context
	cancelBase context.CancelCauseFunc
	wg         sync.WaitGroup
	wake       chan struct{}
	unsub      func()
}

// NewPool returns a Pool over q. Register handlers with Handle, then call Run.
func NewPool(q *Queue, opts PoolOptions) (*Pool, error) {
	if q == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "jobs: nil queue")
	}
	if opts.ScopeChecker == nil {
		return nil, contracts.New(contracts.ErrValidationFailed,
			"jobs: a ScopeChecker is required; running jobs without re-validating their scope would fail open")
	}
	p := &Pool{
		q:        q,
		checker:  opts.ScopeChecker,
		svc:      opts.ServiceAuthorizer,
		log:      opts.Logger,
		handlers: map[string]Handler{},
	}
	if p.log == nil {
		p.log = q.log
	}
	p.base, p.cancelBase = context.WithCancelCause(context.Background())
	p.wake, p.unsub = q.subscribe()
	return p, nil
}

// Handle sets the handler for a registered job type. The Pool leases only the
// types it has handlers for.
func (p *Pool) Handle(jobType string, h Handler) error {
	if h == nil {
		return contracts.New(contracts.ErrValidationFailed, "jobs: nil handler")
	}
	spec, ok := p.q.spec(jobType)
	if !ok {
		return contracts.Newf(contracts.ErrValidationFailed, "jobs: job type %q is not registered", jobType)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.handlers[jobType]; dup {
		return contracts.Newf(contracts.ErrConflict, "jobs: job type %q already has a handler", jobType)
	}
	p.handlers[jobType] = h
	p.types[spec.Class] = append(p.types[spec.Class], jobType)
	slices.Sort(p.types[spec.Class])
	return nil
}

// Wake makes the next Poll (or the sleeping Run loop) look for work now.
func (p *Pool) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Running is how many jobs of a class are executing now.
func (p *Pool) Running(class Class) int {
	if !class.Valid() {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running[class]
}

// Run polls until ctx ends, then shuts down: it stops leasing, gives running
// jobs Config.ShutdownGrace to finish, cancels the rest (they return to
// pending without losing an attempt) and returns once every handler has.
func (p *Pool) Run(ctx context.Context) error {
	for {
		n, err := p.Poll(ctx)
		if err != nil && ctx.Err() == nil {
			p.log.Warn("job poll failed", "err", err.Error())
		}
		if ctx.Err() != nil {
			break
		}
		if n > 0 && err == nil {
			continue // free workers may remain, and more work may be ready
		}
		t := time.NewTimer(p.q.cfg.PollInterval)
		select {
		case <-ctx.Done():
		case <-p.wake:
		case <-t.C:
		}
		t.Stop()
		if ctx.Err() != nil {
			break
		}
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.q.cfg.ShutdownGrace)
	defer cancel()
	return p.Shutdown(sctx)
}

// Poll does one scheduling step: the maintenance pass when it is due, then, for
// each class from the most urgent, it leases as many jobs as the class has
// free workers and starts them. It returns how many jobs it started. Run calls
// it in a loop; tests call it directly, with a fake clock, and wait on their
// handlers instead of sleeping.
func (p *Pool) Poll(ctx context.Context) (int, error) {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()
	p.mu.Lock()
	stopping := p.stopping
	p.mu.Unlock()
	if stopping {
		return 0, nil
	}

	p.maintain(ctx)

	started := 0
	var firstErr error
	for _, c := range Classes() {
		types, free := p.slots(c)
		if free <= 0 || len(types) == 0 {
			continue
		}
		leased, err := p.q.Lease(ctx, LeaseRequest{Classes: []Class{c}, Types: types, Max: free})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, l := range leased {
			p.start(l)
			started++
		}
	}
	return started, firstErr
}

// slots returns a class's handled types and its free worker count.
func (p *Pool) slots(c Class) (types []string, free int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.types[c]), p.q.cfg.classConfig(c).Workers - p.running[c]
}

func (p *Pool) start(l Leased) {
	p.mu.Lock()
	p.running[l.Class]++
	h := p.handlers[l.Type]
	p.mu.Unlock()
	p.wg.Add(1)
	go p.run(l, h)
}

// maintain runs the lease-expiry and deadline pass, and retention, when due.
// The schedule follows the queue's clock, so a test that advances a fake clock
// controls it.
func (p *Pool) maintain(ctx context.Context) {
	now := p.q.clk.Now()
	p.mu.Lock()
	doMaint, doPrune := !now.Before(p.nextMaint), !now.Before(p.nextPrune)
	if doMaint {
		p.nextMaint = now.Add(max(p.q.cfg.LeaseTTL/maintainDivisor, time.Millisecond))
	}
	if doPrune {
		p.nextPrune = now.Add(pruneEvery)
	}
	p.mu.Unlock()

	if doMaint {
		if _, err := p.q.Reap(ctx); err != nil && ctx.Err() == nil {
			p.log.Warn("job maintenance failed", "err", err.Error())
		}
	}
	if doPrune {
		for {
			n, err := p.q.Prune(ctx)
			if err != nil {
				if ctx.Err() == nil {
					p.log.Warn("job retention failed", "err", err.Error())
				}
				break
			}
			if n < pruneBatch {
				break
			}
		}
	}
}

// run executes one leased job from authorisation to its recorded result.
func (p *Pool) run(l Leased, h Handler) {
	defer p.wg.Done()
	defer p.slotFreed(l.Class)

	jctx, cancel := context.WithCancelCause(p.base)
	defer cancel(nil)
	untrack := p.q.track(l.ID, cancel)
	defer untrack()

	// A cancel that landed between the lease and track() only set the flag.
	if c, err := p.q.cancelRequested(jctx, l); IsLeaseLost(err) {
		return
	} else if err == nil && c {
		cancel(errJobCancelled)
	}

	hctx := jctx
	if !l.Deadline.IsZero() {
		remaining := l.Deadline.Sub(p.q.clk.Now())
		if remaining <= 0 {
			p.settle(l, jctx, Permanent(deadlineError()))
			return
		}
		var stop context.CancelFunc
		hctx, stop = context.WithTimeout(jctx, remaining)
		defer stop()
	}

	// The lease is kept alive from here, so a slow scope check cannot let it
	// expire and hand the job to a second worker.
	stopHeartbeat := p.heartbeat(l, cancel)
	if err := p.authorize(hctx, l); err != nil {
		stopHeartbeat()
		p.settle(l, hctx, err)
		return
	}
	if cause := context.Cause(hctx); cause != nil { // cancelled or shut down before it started
		stopHeartbeat()
		p.settle(l, hctx, cause)
		return
	}

	err := p.call(h, hctx, l.Job)
	stopHeartbeat()
	p.settle(l, hctx, err)
}

func deadlineError() error {
	return contracts.New(contracts.ErrTimeout, "job deadline passed")
}

// authorize decides whether the job may run now. nil means yes.
func (p *Pool) authorize(ctx context.Context, l Leased) error {
	if l.ServiceIdentity != "" {
		if p.svc != nil {
			err := p.svc.AuthorizeService(ctx, l.ServiceIdentity, l.Job)
			if err == nil {
				return nil
			}
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			if contracts.CodeOf(err).Retryable() {
				return err // the authorizer could not decide; nothing runs, the attempt is retried
			}
		}
		return Permanent(contracts.New(contracts.ErrPermissionDenied, "declared service identity is not authorised"))
	}
	err := p.checker.CheckScope(ctx, l.Scope)
	if err == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause // the check was interrupted; the job's own context says why
	}
	if contracts.CodeOf(err).Retryable() {
		return err // the checker could not decide; nothing runs, the attempt is retried
	}
	return Permanent(contracts.Wrap(contracts.ErrPermissionDenied, err, "initiating scope is no longer valid"))
}

// call runs the handler and turns a panic into a permanent internal failure
// instead of taking the process down.
func (p *Pool) call(h Handler, ctx context.Context, job Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("job handler panicked", "job_id", job.ID, "type", job.Type,
				"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = Permanent(contracts.New(contracts.ErrInternal, "job handler panicked"))
		}
	}()
	if h == nil {
		return Permanent(contracts.New(contracts.ErrInternal, "no handler for the job type"))
	}
	return h(ctx, job)
}

// heartbeat extends the lease every third of its TTL until stopped. A lost
// lease or a cancel seen in the database cancels the handler's context.
func (p *Pool) heartbeat(l Leased, cancel context.CancelCauseFunc) (stop func()) {
	interval := max(p.q.cfg.LeaseTTL/3, time.Millisecond)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			hctx, c := context.WithTimeout(context.WithoutCancel(p.base), max(interval, time.Second))
			res, err := p.q.Heartbeat(hctx, l)
			c()
			switch {
			case IsLeaseLost(err):
				cancel(errLeaseLost)
				return
			case err != nil:
				p.log.Warn("job heartbeat failed", "job_id", l.ID, "err", err.Error())
			case res.CancelRequested:
				cancel(errJobCancelled)
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// settle records how an attempt ended. ctx is the handler's context: why it
// ended (cancel, shutdown, deadline, lost lease) decides the result.
func (p *Pool) settle(l Leased, ctx context.Context, err error) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(p.base), finalizeTimeout)
	defer cancel()
	cause := context.Cause(ctx)

	var werr error
	switch {
	case err == nil:
		werr = p.q.Complete(fctx, l)
	case errors.Is(cause, errLeaseLost):
		p.log.Warn("job lease lost; result not recorded", "job_id", l.ID, "type", l.Type)
		return
	case errors.Is(cause, errShutdown):
		werr = p.q.Release(fctx, l)
	case errors.Is(cause, context.DeadlineExceeded):
		_, werr = p.q.Fail(fctx, l, Permanent(deadlineError()))
	default:
		// Includes a cancelled job: Fail sees the cancel flag and records
		// cancelled instead of a failure.
		_, werr = p.q.Fail(fctx, l, err)
	}
	if werr != nil {
		if IsLeaseLost(werr) {
			p.log.Warn("job lease lost; result not recorded", "job_id", l.ID, "type", l.Type)
			return
		}
		p.log.Error("job result could not be recorded; the lease will expire and the job will be reaped",
			"job_id", l.ID, "type", l.Type, "err", werr.Error())
	}
}

func (p *Pool) slotFreed(c Class) {
	p.mu.Lock()
	p.running[c]--
	p.mu.Unlock()
	p.Wake()
}

// Shutdown stops leasing and waits for running jobs until ctx ends, then
// cancels what is left (those jobs return to pending, attempt not counted)
// and waits for their handlers, for at most Config.ShutdownGrace more. It is
// safe to call more than once.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	p.unsub()
	// Wait out a Poll that passed the stopping check before it can add to the
	// WaitGroup concurrently with the Wait below.
	p.pollMu.Lock()
	p.pollMu.Unlock() //nolint:staticcheck // an empty critical section is the point

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		p.cancelBase(errShutdown)
		return nil
	case <-ctx.Done():
	}
	p.cancelBase(errShutdown)
	t := time.NewTimer(p.q.cfg.ShutdownGrace)
	defer t.Stop()
	select {
	case <-done:
		return nil
	case <-t.C:
		return contracts.New(contracts.ErrTimeout, "jobs: handlers did not stop after cancellation")
	}
}
