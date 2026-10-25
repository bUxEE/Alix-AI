package jobs

import (
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestLeaseExpiryRequeues(t *testing.T) {
	h := newHarness(t) // lease TTL 30 s
	a := scopeA()
	j := h.enqueue(a, typeOf(ClassIngest))

	l := h.leaseOne(ClassIngest)
	if l.Attempts != 1 || l.State != StateRunning || !l.Until.Equal(epoch.Add(30*time.Second)) {
		t.Fatalf("leased job = attempts %d state %s until %s", l.Attempts, l.State, l.Until)
	}

	// Within the TTL nothing happens: a running job is not reaped or re-leased.
	h.clk.Advance(29 * time.Second)
	if r, err := h.q.Reap(bg); err != nil || r.Total() != 0 {
		t.Fatalf("Reap before expiry = %+v, %v", r, err)
	}
	if got := h.lease(1, ClassIngest); len(got) != 0 {
		t.Fatalf("a held job was leased again")
	}

	// The worker crashes: no heartbeat. The lease expires and the job is pending again.
	h.clk.Advance(2 * time.Second)
	r, err := h.q.Reap(bg)
	if err != nil || r.Requeued != 1 || r.Total() != 1 {
		t.Fatalf("Reap after expiry = %+v, %v; want one requeued", r, err)
	}
	got := h.get(a, j.ID)
	if got.State != StatePending || got.Attempts != 1 {
		t.Fatalf("after expiry job is %s with %d attempts, want pending with the attempt counted", got.State, got.Attempts)
	}
	if n := h.count(`SELECT count(*) FROM jobs WHERE lease_token IS NOT NULL OR lease_until IS NOT NULL`); n != 0 {
		t.Fatalf("lease columns not cleared")
	}

	// It runs again, and the first worker can no longer touch it.
	l2 := h.leaseOne(ClassIngest)
	if l2.ID != j.ID || l2.Attempts != 2 || l2.Token == l.Token {
		t.Fatalf("re-lease = %+v, want attempt 2 with a fresh token", l2)
	}
	if err := h.q.Complete(bg, l); !IsLeaseLost(err) {
		t.Fatalf("stale worker Complete = %v, want lease lost", err)
	}
	if _, err := h.q.Fail(bg, l, errUnavailable()); !IsLeaseLost(err) {
		t.Fatalf("stale worker Fail = %v, want lease lost", err)
	}
	if _, err := h.q.Heartbeat(bg, l); !IsLeaseLost(err) {
		t.Fatalf("stale worker Heartbeat = %v, want lease lost", err)
	}
	if err := h.q.Complete(bg, l2); err != nil {
		t.Fatalf("current worker Complete: %v", err)
	}
	if h.get(a, j.ID).State != StateSucceeded {
		t.Fatalf("job not succeeded")
	}
}

func TestHeartbeatExtendsLease(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	j := h.enqueue(a, typeOf(ClassIngest))
	l := h.leaseOne(ClassIngest)

	for i := 0; i < 4; i++ {
		h.clk.Advance(20 * time.Second) // far beyond one TTL in total
		res, err := h.q.Heartbeat(bg, l)
		if err != nil || res.CancelRequested || !res.Until.Equal(h.clk.Now().Add(30*time.Second)) {
			t.Fatalf("Heartbeat %d = %+v, %v", i, res, err)
		}
		if r, err := h.q.Reap(bg); err != nil || r.Total() != 0 {
			t.Fatalf("a heartbeated job was reaped: %+v, %v", r, err)
		}
	}
	if h.get(a, j.ID).State != StateRunning {
		t.Fatalf("heartbeated job is not running")
	}

	// A cancel recorded in the database (for example by another process) is
	// reported by the next heartbeat.
	if _, err := h.q.Cancel(bg, a, j.ID); err != nil {
		t.Fatal(err)
	}
	res, err := h.q.Heartbeat(bg, l)
	if err != nil || !res.CancelRequested {
		t.Fatalf("Heartbeat after cancel = %+v, %v; want CancelRequested", res, err)
	}
}

func TestLeaseExpiryOutcomes(t *testing.T) {
	t.Run("last attempt: the outcome is unknown, so it fails instead of looping", func(t *testing.T) {
		h := newHarness(t)
		a := scopeA()
		j := h.enqueue(a, "once.job")
		h.leaseOne(ClassRead)
		h.clk.Advance(time.Minute)
		r, err := h.q.Reap(bg)
		if err != nil || r.Exhausted != 1 {
			t.Fatalf("Reap = %+v, %v", r, err)
		}
		got := h.get(a, j.ID)
		if got.State != StateFailed || got.ErrorCode != contracts.ErrUncertainOutcome || got.FinishedAt.IsZero() {
			t.Fatalf("job = %s/%s, want failed/uncertain_outcome", got.State, got.ErrorCode)
		}
	})

	t.Run("a crash loop is bounded by the attempt limit", func(t *testing.T) {
		h := newHarness(t)
		a := scopeA()
		j := h.enqueue(a, "retry.job") // 3 attempts
		for i := 1; i <= 3; i++ {
			l := h.leaseOne(ClassRAGQuery)
			if l.Attempts != i {
				t.Fatalf("attempt %d leased as %d", i, l.Attempts)
			}
			h.clk.Advance(time.Minute)
			if _, err := h.q.Reap(bg); err != nil {
				t.Fatal(err)
			}
		}
		if got := h.get(a, j.ID); got.State != StateFailed || got.ErrorCode != contracts.ErrUncertainOutcome {
			t.Fatalf("after three crashes job is %s/%s", got.State, got.ErrorCode)
		}
		if got := h.lease(1, ClassRAGQuery); len(got) != 0 {
			t.Fatalf("a failed job was leased again")
		}
	})

	t.Run("cancel requested: the expired job is cancelled, not requeued", func(t *testing.T) {
		h := newHarness(t)
		a := scopeA()
		j := h.enqueue(a, typeOf(ClassIngest))
		h.leaseOne(ClassIngest)
		if _, err := h.q.Cancel(bg, a, j.ID); err != nil {
			t.Fatal(err)
		}
		h.clk.Advance(time.Minute)
		if r, err := h.q.Reap(bg); err != nil || r.Cancelled != 1 {
			t.Fatalf("Reap = %+v, %v", r, err)
		}
		if got := h.get(a, j.ID); got.State != StateCancelled {
			t.Fatalf("job is %s, want cancelled", got.State)
		}
	})

	t.Run("deadline passed while running: timeout, not requeued", func(t *testing.T) {
		h := newHarness(t)
		a := scopeA()
		j := h.enqueue(a, typeOf(ClassIngest), func(r *EnqueueRequest) { r.Deadline = epoch.Add(10 * time.Second) })
		h.leaseOne(ClassIngest)
		h.clk.Advance(time.Minute)
		if r, err := h.q.Reap(bg); err != nil || r.TimedOut != 1 {
			t.Fatalf("Reap = %+v, %v", r, err)
		}
		if got := h.get(a, j.ID); got.State != StateFailed || got.ErrorCode != contracts.ErrTimeout {
			t.Fatalf("job is %s/%s, want failed/timeout", got.State, got.ErrorCode)
		}
	})
}

func TestRetryBackoff(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	j := h.enqueue(a, "retry.job") // 3 attempts, base 1 s, cap 10 s
	t0 := h.clk.Now()

	// Attempt 1 fails with a retryable code. Zero jitter gives half the nominal
	// 1 s delay.
	l := h.leaseOne(ClassRAGQuery)
	out, err := h.q.Fail(bg, l, errUnavailable())
	if err != nil || out.State != StatePending || !out.RunAt.Equal(t0.Add(500*time.Millisecond)) || out.Code != contracts.ErrUnavailable {
		t.Fatalf("first failure = %+v, %v", out, err)
	}
	got := h.get(a, j.ID)
	if got.State != StatePending || got.ErrorCode != contracts.ErrUnavailable || !got.RunAt.Equal(out.RunAt) || got.Attempts != 1 {
		t.Fatalf("job after first failure = %+v", got)
	}
	if n := h.count(`SELECT count(*) FROM jobs WHERE lease_token IS NOT NULL`); n != 0 {
		t.Fatalf("a retrying job kept its lease")
	}

	// It is not leased until the backoff has elapsed.
	h.clk.Advance(499 * time.Millisecond)
	if got := h.lease(1, ClassRAGQuery); len(got) != 0 {
		t.Fatalf("leased during backoff")
	}
	h.clk.Advance(time.Millisecond)
	l = h.leaseOne(ClassRAGQuery)
	if l.Attempts != 2 {
		t.Fatalf("second lease is attempt %d", l.Attempts)
	}

	// The delay doubles: 2 s nominal, 1 s with zero jitter.
	t1 := h.clk.Now()
	if out, err = h.q.Fail(bg, l, errUnavailable()); err != nil || !out.RunAt.Equal(t1.Add(time.Second)) {
		t.Fatalf("second failure = %+v, %v; want a 1 s delay", out, err)
	}
	h.clk.Advance(time.Second)

	// Attempt 3 is the last: the failure is terminal and keeps the error code.
	l = h.leaseOne(ClassRAGQuery)
	out, err = h.q.Fail(bg, l, errUnavailable())
	if err != nil || out.State != StateFailed || out.Code != contracts.ErrUnavailable {
		t.Fatalf("third failure = %+v, %v", out, err)
	}
	got = h.get(a, j.ID)
	if got.State != StateFailed || got.ErrorCode != contracts.ErrUnavailable || got.Attempts != 3 ||
		!strings.Contains(got.ErrorMessage, "attempts exhausted") || got.FinishedAt.IsZero() {
		t.Fatalf("job after exhausting attempts = %+v", got)
	}
	h.clk.Advance(time.Hour)
	if got := h.lease(1, ClassRAGQuery); len(got) != 0 {
		t.Fatalf("a failed job was leased again")
	}
}

func TestBackoffShape(t *testing.T) {
	h := newHarness(t)
	spec := TypeSpec{BackoffBase: 4 * time.Second, BackoffMax: 20 * time.Second}

	t.Run("doubles and is capped", func(t *testing.T) {
		h.jitter = 0.999999
		for attempt, want := range map[int]time.Duration{1: 4 * time.Second, 2: 8 * time.Second, 3: 16 * time.Second, 4: 20 * time.Second, 40: 20 * time.Second} {
			got := h.q.backoff(spec, attempt)
			if got > want || got < want-time.Millisecond {
				t.Errorf("attempt %d: delay %s, want just under %s", attempt, got, want)
			}
		}
		h.jitter = 0
		if got := h.q.backoff(spec, 3); got != 8*time.Second {
			t.Errorf("zero jitter: %s, want half of 16s", got)
		}
	})

	t.Run("jitter spreads over [d/2, d)", func(t *testing.T) {
		seen := map[time.Duration]bool{}
		real, err := NewQueue(h.db, Options{Clock: h.clk}) // default random jitter
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 500; i++ {
			d := real.backoff(spec, 2)
			if d < 4*time.Second || d >= 8*time.Second {
				t.Fatalf("delay %s outside [4s, 8s)", d)
			}
			seen[d] = true
		}
		if len(seen) < 50 {
			t.Fatalf("only %d distinct delays in 500 draws: not jittered", len(seen))
		}
	})

	t.Run("an out-of-range jitter source cannot break the bounds", func(t *testing.T) {
		for _, j := range []float64{-5, 7, 1} {
			h.jitter = j
			if d := h.q.backoff(spec, 1); d < 2*time.Second || d > 4*time.Second {
				t.Errorf("jitter %v gave %s", j, d)
			}
		}
		h.jitter = 0
	})
}

func TestFailClassification(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantState State
		wantCode  contracts.ErrCode
	}{
		{"retryable code retries", contracts.New(contracts.ErrUnavailable, "x"), StatePending, contracts.ErrUnavailable},
		{"timeout code retries", contracts.New(contracts.ErrTimeout, "x"), StatePending, contracts.ErrTimeout},
		{"rate_limited retries", contracts.New(contracts.ErrRateLimited, "x"), StatePending, contracts.ErrRateLimited},
		{"wrapped retryable code retries", wrap(contracts.New(contracts.ErrDegraded, "x")), StatePending, contracts.ErrDegraded},
		{"validation_failed is final", contracts.New(contracts.ErrValidationFailed, "x"), StateFailed, contracts.ErrValidationFailed},
		{"permission_denied is final", contracts.New(contracts.ErrPermissionDenied, "x"), StateFailed, contracts.ErrPermissionDenied},
		{"a plain error is final and internal", errPlain, StateFailed, contracts.ErrInternal},
		{"Permanent overrides a retryable code", Permanent(contracts.New(contracts.ErrUnavailable, "x")), StateFailed, contracts.ErrUnavailable},
		{"nil is internal", nil, StateFailed, contracts.ErrInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			a := scopeA()
			j := h.enqueue(a, "retry.job")
			l := h.leaseOne(ClassRAGQuery)
			out, err := h.q.Fail(bg, l, tc.err)
			if err != nil || out.State != tc.wantState || out.Code != tc.wantCode {
				t.Fatalf("Fail = %+v, %v; want %s/%s", out, err, tc.wantState, tc.wantCode)
			}
			got := h.get(a, j.ID)
			if got.State != tc.wantState || got.ErrorCode != tc.wantCode {
				t.Fatalf("stored %s/%s", got.State, got.ErrorCode)
			}
			// The text of a plain or wrapped cause is not stored.
			if strings.Contains(got.ErrorMessage, "plain failure") || strings.Contains(got.ErrorMessage, "secret") {
				t.Fatalf("error message leaks the cause: %q", got.ErrorMessage)
			}
		})
	}
}

type wrapped struct{ err error }

func (w wrapped) Error() string { return "context: secret detail: " + w.err.Error() }
func (w wrapped) Unwrap() error { return w.err }
func wrap(err error) error      { return wrapped{err} }

func TestFailOfACancelledJobRecordsCancelled(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	j := h.enqueue(a, "retry.job")
	l := h.leaseOne(ClassRAGQuery)
	if _, err := h.q.Cancel(bg, a, j.ID); err != nil {
		t.Fatal(err)
	}
	out, err := h.q.Fail(bg, l, errUnavailable()) // retryable, attempts left: still cancelled
	if err != nil || out.State != StateCancelled || out.Code != "" {
		t.Fatalf("Fail after cancel = %+v, %v", out, err)
	}
	if got := h.get(a, j.ID); got.State != StateCancelled || got.ErrorCode != "" {
		t.Fatalf("job = %s/%s", got.State, got.ErrorCode)
	}
}

func TestCompleteAfterCancelRequestIsSucceeded(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	j := h.enqueue(a, typeOf(ClassIngest))
	l := h.leaseOne(ClassIngest)
	if _, err := h.q.Cancel(bg, a, j.ID); err != nil {
		t.Fatal(err)
	}
	// The handler finished anyway: the work was done, and it is reported as such.
	if err := h.q.Complete(bg, l); err != nil {
		t.Fatal(err)
	}
	if got := h.get(a, j.ID); got.State != StateSucceeded {
		t.Fatalf("job = %s, want succeeded", got.State)
	}
}

func TestDeadlineExceededFails(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	late := h.enqueue(a, typeOf(ClassIngest), func(r *EnqueueRequest) { r.Deadline = epoch.Add(10 * time.Second) })
	later := h.enqueue(a, typeOf(ClassIngest), func(r *EnqueueRequest) { r.Deadline = epoch.Add(time.Hour) })
	none := h.enqueue(a, typeOf(ClassIngest))

	h.clk.Advance(11 * time.Second)
	// The expired job is skipped, not leased; the others run in order.
	got := h.lease(5, ClassIngest)
	if len(got) != 2 || got[0].ID != later.ID || got[1].ID != none.ID {
		t.Fatalf("leased %d jobs; the expired one must not be among them", len(got))
	}
	failed := h.get(a, late.ID)
	if failed.State != StateFailed || failed.ErrorCode != contracts.ErrTimeout || failed.Attempts != 0 || !failed.FinishedAt.Equal(h.clk.Now()) {
		t.Fatalf("expired job = %s/%s attempts %d finished %s; want failed/timeout, never attempted", failed.State, failed.ErrorCode, failed.Attempts, failed.FinishedAt)
	}

	t.Run("Reap fails an expired job nobody asked for", func(t *testing.T) {
		h := newHarness(t)
		j := h.enqueue(a, typeOf(ClassMaintenance), func(r *EnqueueRequest) { r.Deadline = epoch.Add(time.Second) })
		h.clk.Advance(time.Minute)
		r, err := h.q.Reap(bg)
		if err != nil || r.TimedOut != 1 {
			t.Fatalf("Reap = %+v, %v", r, err)
		}
		if got := h.get(a, j.ID); got.State != StateFailed || got.ErrorCode != contracts.ErrTimeout {
			t.Fatalf("job = %s/%s", got.State, got.ErrorCode)
		}
		// Expired jobs stop counting against the depth.
		if n := h.count(`SELECT count(*) FROM jobs WHERE state = 'pending'`); n != 0 {
			t.Fatalf("%d pending after expiry", n)
		}
	})

	t.Run("a retry that would start after the deadline fails with timeout", func(t *testing.T) {
		h := newHarness(t)
		j := h.enqueue(a, "retry.job", func(r *EnqueueRequest) { r.Deadline = epoch.Add(400 * time.Millisecond) })
		l := h.leaseOne(ClassRAGQuery)
		out, err := h.q.Fail(bg, l, errUnavailable()) // backoff 500 ms > 400 ms left
		if err != nil || out.State != StateFailed || out.Code != contracts.ErrTimeout {
			t.Fatalf("Fail = %+v, %v; want failed/timeout", out, err)
		}
		if got := h.get(a, j.ID); got.State != StateFailed {
			t.Fatalf("job = %s", got.State)
		}
	})
}
