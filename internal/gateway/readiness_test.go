package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	readinessWarmCheck      = "inference_warmth"
	readinessWarmWait       = 40 * time.Millisecond
	readinessWarmRetryAfter = 30 * time.Second
)

// readinessWarmFixture is a warm-up call the test controls: each call blocks
// until the test sends its outcome on gate, which is how a cold model load is
// stood in for.
type readinessWarmFixture struct {
	calls atomic.Int32
	gate  chan error

	mu   sync.Mutex
	ctxs []context.Context
}

func newReadinessWarmFixture() *readinessWarmFixture {
	return &readinessWarmFixture{gate: make(chan error, 8)}
}

func (f *readinessWarmFixture) warm(ctx context.Context) error {
	f.calls.Add(1)
	f.mu.Lock()
	f.ctxs = append(f.ctxs, ctx)
	f.mu.Unlock()
	select {
	case err := <-f.gate:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *readinessWarmFixture) lastCtx() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctxs[len(f.ctxs)-1]
}

// readinessWarmHarness is a server whose only extra readiness check is the
// warm-up tracker, started on a context the test can cancel.
func readinessWarmHarness(t *testing.T, fx *readinessWarmFixture) (*harness, *ReadinessWarmth, context.CancelFunc) {
	t.Helper()
	var w *ReadinessWarmth
	h := newHarness(t, func(o *Options) {
		var err error
		w, err = NewReadinessWarmth(ReadinessWarmthOptions{
			Warm: fx.warm, Clock: o.Clock, WaitFor: readinessWarmWait, RetryAfter: readinessWarmRetryAfter,
		})
		if err != nil {
			t.Fatal(err)
		}
		o.Checks = append(o.Checks, w.Check(readinessWarmCheck))
	})
	ctx, cancel := context.WithCancel(context.Background())
	stop := w.Start(ctx)
	t.Cleanup(func() {
		cancel()
		stop()
	})
	return h, w, cancel
}

// readinessWarmPoll reads /readyz once, past the report cache.
func readinessWarmPoll(t *testing.T, h *harness) (int, ReadyReport, string, http.Header) {
	t.Helper()
	h.clk.Advance(DefaultHealthTTL + time.Second)
	rec := h.do(http.MethodGet, "/readyz", nil)
	var rep ReadyReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("/readyz body is not a ReadyReport: %v\n%s", err, rec.Body.String())
	}
	return rec.Code, rep, rec.Body.String(), rec.Header()
}

func readinessWarmCheckOf(t *testing.T, rep ReadyReport) CheckReport {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == readinessWarmCheck {
			return c
		}
	}
	t.Fatalf("/readyz has no %q check: %+v", readinessWarmCheck, rep.Checks)
	return CheckReport{}
}

func readinessWarmEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// F4 and F5 of the M0-13 spike: /readyz said nothing about inference, and the
// first call to a cold model outran the call budget. /readyz now reports whether
// the model loaded and answered, never calls the model per request, and says
// "warming up" (not ready) while the load is under way.
func TestReadyzReportsInferenceWarmth(t *testing.T) {
	t.Run("a cold load is warming up and not ready, then ready once it answered", func(t *testing.T) {
		fx := newReadinessWarmFixture()
		h, w, _ := readinessWarmHarness(t, fx)

		// The load never finishes on its own here, so a poll that waited for it would hang:
		// it answers after its short bound and reports warming.
		code, rep, body, hdr := readinessWarmPoll(t, h)
		check := readinessWarmCheckOf(t, rep)
		if code != http.StatusServiceUnavailable || rep.Ready || rep.Status != providers.HealthUnavailable {
			t.Fatalf("while the model loads: %d %+v, want 503 unavailable", code, rep)
		}
		if check.OK || !strings.Contains(check.Reason, "warming up") {
			t.Errorf("check = %+v, want not ok and a warming-up reason", check)
		}
		if hdr.Get("Retry-After") == "" {
			t.Error("a not-ready answer has no Retry-After")
		}
		if got := w.State(); got != ReadinessWarmthWarming {
			t.Errorf("state = %q, want warming", got)
		}
		if fx.calls.Load() != 1 {
			t.Fatalf("warm-up calls = %d, want 1", fx.calls.Load())
		}

		// Another poll while the load is still going joins it: no second call.
		if _, _, _, _ = readinessWarmPoll(t, h); fx.calls.Load() != 1 {
			t.Errorf("a poll during the load started another warm-up call (%d)", fx.calls.Load())
		}

		// The model answers. The next poll past the cache says ready, and the
		// check carries no reason.
		fx.gate <- nil
		readinessWarmEventually(t, "the warm-up to finish", func() bool { return w.State() == ReadinessWarmthWarm })
		code, rep, body, _ = readinessWarmPoll(t, h)
		check = readinessWarmCheckOf(t, rep)
		if code != http.StatusOK || !rep.Ready || rep.Status != providers.HealthOK || !check.OK || check.Reason != "" {
			t.Fatalf("after the model answered: %d %+v (%s)", code, rep, body)
		}

		// A warm model is not called again by /readyz, however often it is polled.
		for i := 0; i < 5; i++ {
			if code, _, _, _ = readinessWarmPoll(t, h); code != http.StatusOK {
				t.Fatalf("poll %d = %d", i, code)
			}
		}
		if fx.calls.Load() != 1 {
			t.Errorf("/readyz called the model again: %d warm-up calls", fx.calls.Load())
		}
	})

	t.Run("a runtime that answers at once is ready on the first poll", func(t *testing.T) {
		fx := newReadinessWarmFixture()
		fx.gate <- nil
		h, _, _ := readinessWarmHarness(t, fx)
		code, rep, body, _ := readinessWarmPoll(t, h)
		if code != http.StatusOK || !rep.Ready || !readinessWarmCheckOf(t, rep).OK {
			t.Fatalf("first poll: %d %+v (%s)", code, rep, body)
		}
	})

	t.Run("a failed warm-up reports its code, not the runtime's text, and is retried after the back-off", func(t *testing.T) {
		fx := newReadinessWarmFixture()
		secretText := "dial tcp 10.9.8.7:8080: connection refused (model path /srv/SECRET-MODEL-DIR/x.gguf)"
		fx.gate <- contracts.Wrap(contracts.ErrUnavailable, context.DeadlineExceeded, secretText)
		h, w, _ := readinessWarmHarness(t, fx)

		code, rep, body, _ := readinessWarmPoll(t, h)
		check := readinessWarmCheckOf(t, rep)
		if code != http.StatusServiceUnavailable || check.OK || !strings.Contains(check.Reason, "did not answer") ||
			!strings.Contains(check.Reason, string(contracts.ErrUnavailable)) {
			t.Fatalf("failed warm-up: %d %+v (%s)", code, check, body)
		}
		for _, leak := range []string{"10.9.8.7", "SECRET-MODEL-DIR", "connection refused"} {
			if strings.Contains(body, leak) {
				t.Errorf("/readyz exposes the runtime's error text (%q): %s", leak, body)
			}
		}
		if w.State() != ReadinessWarmthFailed || fx.calls.Load() != 1 {
			t.Fatalf("state %q after %d calls, want failed after 1", w.State(), fx.calls.Load())
		}

		// Inside the back-off a poll reports the failure and does not call again.
		h.clk.Advance(DefaultHealthTTL)
		if code, _, _, _ = readinessWarmPoll(t, h); code != http.StatusServiceUnavailable || fx.calls.Load() != 1 {
			t.Errorf("inside the back-off: %d with %d calls, want 503 with 1", code, fx.calls.Load())
		}

		// Past it the next poll tries again and a good answer makes it ready.
		h.clk.Advance(readinessWarmRetryAfter)
		fx.gate <- nil
		code, rep, body, _ = readinessWarmPoll(t, h)
		if code != http.StatusOK || !readinessWarmCheckOf(t, rep).OK || fx.calls.Load() != 2 {
			t.Fatalf("after the back-off: %d %s with %d calls, want ready after 2", code, body, fx.calls.Load())
		}
	})

	t.Run("a runtime that came back is warmed again", func(t *testing.T) {
		fx := newReadinessWarmFixture()
		fx.gate <- nil
		h, w, _ := readinessWarmHarness(t, fx)
		if code, _, _, _ := readinessWarmPoll(t, h); code != http.StatusOK {
			t.Fatalf("first poll = %d", code)
		}
		w.Invalidate() // the runtime restarted: the model is unloaded
		fx.gate <- nil
		if code, _, body, _ := readinessWarmPoll(t, h); code != http.StatusOK || fx.calls.Load() != 2 {
			t.Fatalf("after Invalidate: %d %s with %d calls, want ready after 2", code, body, fx.calls.Load())
		}
	})

	t.Run("the warm-up outlives the request that started it", func(t *testing.T) {
		fx := newReadinessWarmFixture()
		_, w, cancelService := readinessWarmHarness(t, fx)

		reqCtx, cancelReq := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- w.Evaluate(reqCtx) }()
		readinessWarmEventually(t, "the warm-up call to start", func() bool { return fx.calls.Load() == 1 })
		cancelReq() // the prober hangs up mid-load
		if err := <-errc; err == nil {
			t.Fatal("an evaluation that was cut off reported ready")
		}
		if fx.lastCtx().Err() != nil {
			t.Fatal("the warm-up call was cancelled with the request that started it")
		}
		fx.gate <- nil
		readinessWarmEventually(t, "the warm-up to finish", func() bool { return w.State() == ReadinessWarmthWarm })

		// It runs on the service's context: stopping the service cancels it, and
		// a stopping service is never ready.
		cancelService()
		if err := w.Evaluate(context.Background()); err == nil {
			t.Error("a stopped service reported ready")
		}
	})

	t.Run("before Start it fails closed and calls nothing", func(t *testing.T) {
		fx := newReadinessWarmFixture()
		w, err := NewReadinessWarmth(ReadinessWarmthOptions{Warm: fx.warm})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Evaluate(context.Background()); err == nil || contracts.CodeOf(err) != contracts.ErrUnavailable {
			t.Errorf("Evaluate before Start = %v, want unavailable", err)
		}
		if fx.calls.Load() != 0 {
			t.Error("the model was called before Start")
		}
	})

	t.Run("a panic in the warm-up is a failure, not a hang", func(t *testing.T) {
		w, err := NewReadinessWarmth(ReadinessWarmthOptions{
			Warm: func(context.Context) error { panic("boom") }, WaitFor: readinessWarmWait,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		stop := w.Start(ctx)
		defer func() { cancel(); stop() }()
		if err := w.Evaluate(ctx); err == nil || w.State() != ReadinessWarmthFailed {
			t.Errorf("Evaluate = %v, state %q, want a failure", err, w.State())
		}
	})

	t.Run("the wait stays under the check timeout", func(t *testing.T) {
		_, err := NewReadinessWarmth(ReadinessWarmthOptions{Warm: func(context.Context) error { return nil }, WaitFor: checkTimeout})
		if err == nil {
			t.Error("a wait as long as the check timeout was accepted")
		}
		if _, err := NewReadinessWarmth(ReadinessWarmthOptions{}); err == nil {
			t.Error("a tracker without a warm-up function was accepted")
		}
	})
}
