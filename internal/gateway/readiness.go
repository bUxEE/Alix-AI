package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Inference readiness (M0-18, from the live spike M0-13 findings F4 and F5).
//
// The runtime's own /readyz answers 200 with no backend installed and with a
// model that cannot load, and the first call to a cold model can take longer
// than the 60 s call budget. So /readyz must say whether the model was loaded
// and answered, and the service must not call itself ready before that happened.
//
// ReadinessWarmth is that condition as a Check. It does not probe on its own
// schedule: the readiness evaluation drives it. The first evaluation starts one
// warm-up call (the caller supplies it; the composition root sends a one-token
// chat under a first-call budget that covers a cold load) and waits a short
// while for it. A runtime that answers at once makes the first /readyz ready; a
// cold load is reported as "warming up" until the call returns. The call runs on
// the service's own context, never on the request that happened to trigger it,
// so a probe that hangs up cannot cancel the model load.
//
// /readyz is anonymous, so it never runs inference per request: a warm model
// stays warm until Invalidate, a failed warm-up is retried no more often than
// once per RetryAfter, and a warm-up in flight is joined, not repeated.

// ReadinessWarmthState is where the warm-up stands.
type ReadinessWarmthState string

const (
	// ReadinessWarmthCold: no warm-up has run since the start (or since
	// Invalidate). The next evaluation starts one.
	ReadinessWarmthCold ReadinessWarmthState = "cold"
	// ReadinessWarmthWarming: a warm-up call is in flight; the model may still
	// be loading.
	ReadinessWarmthWarming ReadinessWarmthState = "warming"
	// ReadinessWarmthWarm: the model loaded and answered a call.
	ReadinessWarmthWarm ReadinessWarmthState = "warm"
	// ReadinessWarmthFailed: the last warm-up call returned an error. The next
	// attempt waits for RetryAfter.
	ReadinessWarmthFailed ReadinessWarmthState = "failed"
)

const (
	// ReadinessWarmthDefaultWaitFor is how long one evaluation waits for a
	// warm-up it started or joined. It stays under checkTimeout so the wait
	// ends before the Check's own deadline.
	ReadinessWarmthDefaultWaitFor = 1500 * time.Millisecond
	// ReadinessWarmthDefaultRetryAfter is the least time between a failed
	// warm-up and the next attempt.
	ReadinessWarmthDefaultRetryAfter = 10 * time.Second
)

// ReadinessWarmthOptions configures NewReadinessWarmth.
type ReadinessWarmthOptions struct {
	// Warm sends the warm-up call and returns nil only when the model loaded and
	// answered. It runs on the context Start was given, with whatever deadline
	// it applies itself: that deadline is the first-call budget and must cover a
	// cold load. Required.
	Warm func(ctx context.Context) error
	// Clock times the retry back-off. Nil uses the wall clock.
	Clock clock.Clock
	// Logger records each outcome. Nil discards.
	Logger *slog.Logger
	// WaitFor bounds how long one evaluation waits. Zero is
	// ReadinessWarmthDefaultWaitFor; it must stay below the Check timeout.
	WaitFor time.Duration
	// RetryAfter is the back-off after a failed warm-up. Zero is
	// ReadinessWarmthDefaultRetryAfter.
	RetryAfter time.Duration
}

// ReadinessWarmth reports whether the model has been loaded and has answered.
// It is safe for concurrent use.
type ReadinessWarmth struct {
	warm    func(ctx context.Context) error
	clk     clock.Clock
	log     *slog.Logger
	waitFor time.Duration
	retry   time.Duration

	wg sync.WaitGroup

	mu       sync.Mutex
	base     context.Context // nil until Start
	state    ReadinessWarmthState
	done     chan struct{} // closed when the call in flight ends
	failedAt time.Time
	failure  contracts.ErrCode
}

// NewReadinessWarmth builds the tracker. Nothing runs until Start.
func NewReadinessWarmth(opts ReadinessWarmthOptions) (*ReadinessWarmth, error) {
	if opts.Warm == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "gateway: readiness warm-up needs a Warm function")
	}
	if opts.WaitFor < 0 || opts.RetryAfter < 0 {
		return nil, contracts.New(contracts.ErrValidationFailed, "gateway: readiness warm-up durations must not be negative")
	}
	w := &ReadinessWarmth{
		warm: opts.Warm, clk: opts.Clock, log: opts.Logger,
		waitFor: opts.WaitFor, retry: opts.RetryAfter, state: ReadinessWarmthCold,
	}
	if w.clk == nil {
		w.clk = clock.Real{}
	}
	if w.log == nil {
		w.log = slog.New(slog.DiscardHandler)
	}
	if w.waitFor == 0 {
		w.waitFor = ReadinessWarmthDefaultWaitFor
	}
	if w.waitFor >= checkTimeout {
		return nil, contracts.Newf(contracts.ErrValidationFailed,
			"gateway: readiness warm-up wait (%s) must be shorter than the check timeout (%s)", w.waitFor, checkTimeout)
	}
	if w.retry == 0 {
		w.retry = ReadinessWarmthDefaultRetryAfter
	}
	return w, nil
}

// Start gives the tracker the context its warm-up calls run under (the
// service's, cancelled when the service stops) and returns the function that
// waits for a call still in flight. Before Start every evaluation fails closed
// as unavailable; a second Start is ignored.
func (w *ReadinessWarmth) Start(ctx context.Context) (stop func()) {
	w.mu.Lock()
	if w.base == nil {
		w.base = ctx
	}
	w.mu.Unlock()
	return w.wg.Wait
}

// State returns where the warm-up stands.
func (w *ReadinessWarmth) State() ReadinessWarmthState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// Invalidate marks a warm model cold again, because it may have been unloaded:
// the runtime was unavailable and came back, so it restarted. The next
// evaluation warms it again. A warm-up in flight or a failed one is untouched.
func (w *ReadinessWarmth) Invalidate() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == ReadinessWarmthWarm {
		w.state = ReadinessWarmthCold
	}
}

// Check is the readiness condition under name. The name is part of /readyz.
func (w *ReadinessWarmth) Check(name string) Check {
	return Check{Name: name, Fn: w.Evaluate}
}

// Evaluate returns nil when the model has loaded and answered. Otherwise it
// returns a typed unavailable error whose message is safe for an anonymous
// reader: it names the state and, for a failure, the error code, never the
// runtime's own text.
func (w *ReadinessWarmth) Evaluate(ctx context.Context) error {
	w.mu.Lock()
	if w.base == nil {
		w.mu.Unlock()
		return contracts.New(contracts.ErrUnavailable, "the model warm-up has not started")
	}
	if w.base.Err() != nil {
		// The service is stopping: no new call starts, and nothing is ready.
		w.mu.Unlock()
		return contracts.New(contracts.ErrUnavailable, "the service is stopping")
	}
	switch w.state {
	case ReadinessWarmthWarm:
		w.mu.Unlock()
		return nil
	case ReadinessWarmthFailed:
		if w.clk.Now().Sub(w.failedAt) < w.retry {
			err := w.failureLocked()
			w.mu.Unlock()
			return err
		}
		w.startLocked()
	case ReadinessWarmthCold:
		w.startLocked()
	}
	done := w.done
	w.mu.Unlock()

	timer := time.NewTimer(w.waitFor)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
	case <-timer.C:
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	switch w.state {
	case ReadinessWarmthWarm:
		return nil
	case ReadinessWarmthFailed:
		return w.failureLocked()
	}
	return contracts.New(contracts.ErrUnavailable, "the model is warming up: the first call loads it into memory")
}

// startLocked begins one warm-up call. The caller holds w.mu and has checked
// that none is in flight.
func (w *ReadinessWarmth) startLocked() {
	done := make(chan struct{})
	w.done, w.state = done, ReadinessWarmthWarming
	ctx, began := w.base, w.clk.Now()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer close(done)
		err := w.call(ctx)
		w.mu.Lock()
		defer w.mu.Unlock()
		dur := w.clk.Now().Sub(began)
		if err == nil {
			w.state, w.failure = ReadinessWarmthWarm, ""
			w.log.Info("the model is warm", slog.Duration("dur", dur))
			return
		}
		w.state, w.failedAt, w.failure = ReadinessWarmthFailed, w.clk.Now(), contracts.CodeOf(err)
		w.log.Warn("the model warm-up failed", slog.String("code", string(w.failure)), slog.String("err_text", err.Error()),
			slog.Duration("retry_after", w.retry))
	}()
}

// call runs the warm-up and turns a panic into a failure: a bug in it must not
// leave the state warming for ever.
func (w *ReadinessWarmth) call(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = contracts.New(contracts.ErrInternal, fmt.Sprintf("warm-up panicked: %v", r))
		}
	}()
	if err := w.warm(ctx); err != nil {
		return err
	}
	if cerr := ctx.Err(); cerr != nil {
		// The service is stopping: an answer that arrived as it did is not proof
		// of anything the next process can rely on.
		return errors.Join(contracts.New(contracts.ErrUnavailable, "the service is stopping"), cerr)
	}
	return nil
}

func (w *ReadinessWarmth) failureLocked() error {
	return contracts.Newf(contracts.ErrUnavailable,
		"the model did not answer the warm-up call (%s); it is retried every %s", w.failure, w.retry)
}
