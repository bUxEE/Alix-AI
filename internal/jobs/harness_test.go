package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var epoch = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// harness is a queue over a throwaway database and a fake clock. The jitter is
// zero by default so a retry delay is exactly half its nominal value.
type harness struct {
	t      *testing.T
	db     *store.DB
	clk    *clock.Fake
	q      *Queue
	jitter float64
}

// newHarness registers one type per class ("c1.job" .. "c6.job"), "retry.job"
// (class 3, three attempts, 1 s base, 10 s cap) and "once.job" (class 4, one
// attempt).
func newHarness(t *testing.T, mutate ...func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, clk: clock.NewFake(epoch)}
	h.db = storetest.Open(t, nil) // registered before any Pool cleanup, so it closes after them
	opts := Options{Clock: h.clk, Jitter: func() float64 { return h.jitter }}
	for _, m := range mutate {
		m(&opts)
	}
	var err error
	if h.q, err = NewQueue(h.db, opts); err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	for _, c := range Classes() {
		h.register(TypeSpec{Name: fmt.Sprintf("c%d.job", c), Class: c})
	}
	h.register(TypeSpec{Name: "retry.job", Class: ClassRAGQuery, MaxAttempts: 3, BackoffBase: time.Second, BackoffMax: 10 * time.Second})
	h.register(TypeSpec{Name: "once.job", Class: ClassRead, MaxAttempts: 1})
	return h
}

func (h *harness) register(spec TypeSpec) {
	h.t.Helper()
	if err := h.q.Register(spec); err != nil {
		h.t.Fatalf("Register(%s): %v", spec.Name, err)
	}
}

func typeOf(c Class) string { return fmt.Sprintf("c%d.job", c) }

var bg = context.Background()

func scopeA() contracts.Scope { return storetest.Scope("app", "tenantA", "ws1") }
func scopeB() contracts.Scope { return storetest.Scope("app", "tenantB", "ws1") }

// enqueue adds a job for scope and fails the test on error.
func (h *harness) enqueue(scope contracts.Scope, typ string, mutate ...func(*EnqueueRequest)) Job {
	h.t.Helper()
	req := EnqueueRequest{Type: typ, Payload: []byte(`{"n":1}`)}
	for _, m := range mutate {
		if m != nil {
			m(&req)
		}
	}
	res, err := h.q.Enqueue(bg, scope, req)
	if err != nil {
		h.t.Fatalf("Enqueue(%s): %v", typ, err)
	}
	return res.Job
}

// lease leases up to max jobs of the given classes.
func (h *harness) lease(max int, classes ...Class) []Leased {
	h.t.Helper()
	got, err := h.q.Lease(bg, LeaseRequest{Classes: classes, Max: max})
	if err != nil {
		h.t.Fatalf("Lease: %v", err)
	}
	return got
}

func (h *harness) leaseOne(classes ...Class) Leased {
	h.t.Helper()
	got := h.lease(1, classes...)
	if len(got) != 1 {
		h.t.Fatalf("Lease(%v) returned %d jobs, want 1", classes, len(got))
	}
	return got[0]
}

func (h *harness) get(scope contracts.Scope, id string) Job {
	h.t.Helper()
	j, err := h.q.Get(bg, scope, id)
	if err != nil {
		h.t.Fatalf("Get(%s): %v", id, err)
	}
	return j
}

func (h *harness) count(query string, args ...any) int {
	h.t.Helper()
	n, err := countOne(bg, h.db, query, args...)
	if err != nil {
		h.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// poolHarness is a Pool over h.q whose scope checker is switchable.
type poolHarness struct {
	*harness
	pool *Pool

	mu      sync.Mutex
	checker error
	checked int
}

func (h *harness) newPool(t *testing.T, opts ...func(*PoolOptions)) *poolHarness {
	t.Helper()
	ph := &poolHarness{harness: h}
	po := PoolOptions{ScopeChecker: ScopeCheckerFunc(func(context.Context, contracts.Scope) error {
		ph.mu.Lock()
		defer ph.mu.Unlock()
		ph.checked++
		return ph.checker
	})}
	for _, o := range opts {
		o(&po)
	}
	p, err := NewPool(h.q, po)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	ph.pool = p
	// Registered after the database's cleanup, so it runs first: handlers are
	// stopped before the store closes.
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.Shutdown(sctx); err != nil {
			t.Errorf("pool shutdown: %v", err)
		}
	})
	return ph
}

func (ph *poolHarness) setChecker(err error) {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	ph.checker = err
}

func (ph *poolHarness) handle(t *testing.T, typ string, fn Handler) {
	t.Helper()
	if err := ph.pool.Handle(typ, fn); err != nil {
		t.Fatalf("Handle(%s): %v", typ, err)
	}
}

func (ph *poolHarness) poll(t *testing.T) int {
	t.Helper()
	n, err := ph.pool.Poll(bg)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return n
}

// waitFor polls cond every millisecond; the limit is a failure bound, not a delay.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	limit := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(limit) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *harness) waitState(scope contracts.Scope, id string, want State) Job {
	h.t.Helper()
	var j Job
	waitFor(h.t, fmt.Sprintf("job %s to be %s", id, want), func() bool {
		var err error
		j, err = h.q.Get(bg, scope, id)
		return err == nil && j.State == want
	})
	return j
}

// codeIs fails the test unless err carries code.
func codeIs(t *testing.T, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want code %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s (%v), want %s", got, err, code)
	}
}

func errUnavailable() error { return contracts.New(contracts.ErrUnavailable, "dependency down") }

var errPlain = errors.New("plain failure")
