package jobs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

func TestEnqueueDequeuePriorityOrder(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	// Enqueued out of priority order, two jobs in classes 1 and 5.
	names := map[string]Job{}
	for _, e := range []struct {
		name  string
		class Class
	}{
		{"c5-first", ClassIngest}, {"c3", ClassRAGQuery}, {"c1-first", ClassAuthCommit}, {"c6", ClassMaintenance},
		{"c2", ClassTurn}, {"c4", ClassRead}, {"c1-second", ClassAuthCommit}, {"c5-second", ClassIngest},
	} {
		names[e.name] = h.enqueue(a, typeOf(e.class))
	}

	var got []string
	byID := map[string]string{}
	for n, j := range names {
		byID[j.ID] = n
	}
	// The clock never moves: order must come from priority then enqueue order,
	// not from timestamps.
	for range names {
		l := h.leaseOne(Classes()...)
		got = append(got, byID[l.ID])
	}
	want := []string{"c1-first", "c1-second", "c2", "c3", "c4", "c5-first", "c5-second", "c6"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("lease order (-want +got):\n%s", diff)
	}
	if more := h.lease(1, Classes()...); len(more) != 0 {
		t.Fatalf("queue should be empty, leased %d more", len(more))
	}

	t.Run("a job runs no earlier than its run_at", func(t *testing.T) {
		h := newHarness(t)
		later := h.enqueue(a, typeOf(ClassTurn), func(r *EnqueueRequest) { r.RunAt = epoch.Add(time.Minute) })
		now := h.enqueue(a, typeOf(ClassIngest))
		if l := h.leaseOne(Classes()...); l.ID != now.ID {
			t.Fatalf("a delayed class-2 job must not be leased before its run_at; got %s", l.Type)
		}
		if got := h.lease(1, Classes()...); len(got) != 0 {
			t.Fatalf("delayed job leased early")
		}
		h.clk.Advance(time.Minute)
		if l := h.leaseOne(Classes()...); l.ID != later.ID {
			t.Fatalf("delayed job not leased once due")
		}
	})
}

// SEC-078
func TestBackgroundYieldsToInteractive(t *testing.T) {
	for _, interactive := range []Class{ClassAuthCommit, ClassTurn} {
		t.Run(interactive.String(), func(t *testing.T) {
			h := newHarness(t)
			a := scopeA()
			ing := h.enqueue(a, typeOf(ClassIngest))
			maint := h.enqueue(a, typeOf(ClassMaintenance))
			read := h.enqueue(a, typeOf(ClassRead))
			rag := h.enqueue(a, typeOf(ClassRAGQuery))
			urgent := h.enqueue(a, typeOf(interactive))

			if got := h.lease(5, ClassIngest, ClassMaintenance); len(got) != 0 {
				t.Fatalf("class 5/6 leased %d job(s) while a class-%d job was waiting", len(got), interactive)
			}
			// Classes 3 and 4 are not background: they are not held back.
			if l := h.leaseOne(ClassRAGQuery); l.ID != rag.ID {
				t.Fatalf("class 3 should not yield")
			}
			if l := h.leaseOne(ClassRead); l.ID != read.ID {
				t.Fatalf("class 4 should not yield")
			}
			// Once the interactive job is leased it is running, not waiting, and
			// background work may proceed.
			if l := h.leaseOne(interactive); l.ID != urgent.ID {
				t.Fatalf("interactive job not leased")
			}
			got := h.lease(5, ClassIngest, ClassMaintenance)
			if len(got) != 2 || got[0].ID != ing.ID || got[1].ID != maint.ID {
				t.Fatalf("background work did not resume once no interactive job waited: %d leased", len(got))
			}
		})
	}

	t.Run("one request for every class serves interactive first and then background", func(t *testing.T) {
		h := newHarness(t)
		a := scopeA()
		ing := h.enqueue(a, typeOf(ClassIngest))
		urgent := h.enqueue(a, typeOf(ClassTurn))
		got := h.lease(5, Classes()...)
		if len(got) != 2 || got[0].ID != urgent.ID || got[1].ID != ing.ID {
			t.Fatalf("leased %d job(s), want the class-2 job then the class-5 job", len(got))
		}
	})
}

func TestBackgroundYieldIgnoresDelayedInteractive(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	h.enqueue(a, typeOf(ClassTurn), func(r *EnqueueRequest) { r.RunAt = epoch.Add(time.Hour) })
	ing := h.enqueue(a, typeOf(ClassIngest))
	// A class-2 job that is not yet due is not waiting for a worker.
	if l := h.leaseOne(ClassIngest); l.ID != ing.ID {
		t.Fatalf("background was held back by an interactive job that is not due")
	}
}

func TestBackgroundYieldCanBeDisabled(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Config.BackgroundFrom = 7 })
	a := scopeA()
	h.enqueue(a, typeOf(ClassAuthCommit))
	ing := h.enqueue(a, typeOf(ClassIngest))
	if l := h.leaseOne(ClassIngest); l.ID != ing.ID {
		t.Fatalf("yield rule is off but class 5 was held back")
	}
}

// SEC-079
func TestBoundedDepthRejects(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.Config.Class5 = ClassConfig{Workers: 1, MaxDepth: 3}
		o.Config.Class6 = ClassConfig{Workers: 1, MaxDepth: 1}
	})
	a, b := scopeA(), scopeB()
	var first Job
	for i := 0; i < 3; i++ {
		j := h.enqueue(a, typeOf(ClassIngest), func(r *EnqueueRequest) {
			if i == 0 {
				r.IdempotencyKey = "keep"
			}
		})
		if i == 0 {
			first = j
		}
	}

	// The fourth, from any tenant, is refused with a typed error, and nothing
	// is inserted.
	for _, scope := range []contracts.Scope{a, b} {
		_, err := h.q.Enqueue(bg, scope, EnqueueRequest{Type: typeOf(ClassIngest)})
		codeIs(t, err, contracts.ErrResourceExhausted)
		var ce *contracts.Error
		if !asContractsError(err, &ce) || ce.Details["limit"] != 3 || ce.Details["class"] != "ingest" {
			t.Fatalf("error details = %v, want class ingest and limit 3", ce)
		}
		if !ce.Retryable {
			t.Fatalf("resource_exhausted must be marked retryable")
		}
	}
	if n := h.count(`SELECT count(*) FROM jobs`); n != 3 {
		t.Fatalf("%d rows after rejected enqueues, want 3", n)
	}

	// Depth is per class: others are unaffected.
	h.enqueue(a, typeOf(ClassMaintenance))
	_, err := h.q.Enqueue(bg, a, EnqueueRequest{Type: typeOf(ClassMaintenance)})
	codeIs(t, err, contracts.ErrResourceExhausted)
	h.enqueue(a, typeOf(ClassRead))

	// Repeating an enqueue that is already queued is not new work, so a full
	// queue does not refuse it.
	res, err := h.q.Enqueue(bg, a, EnqueueRequest{Type: typeOf(ClassIngest), Payload: []byte(`{"n":1}`), IdempotencyKey: "keep"})
	if err != nil || !res.Duplicate || res.Job.ID != first.ID {
		t.Fatalf("duplicate on a full queue = %+v, %v; want the first job", res, err)
	}

	// Depth counts waiting jobs: leasing one frees a place.
	h.leaseOne(ClassIngest)
	h.enqueue(a, typeOf(ClassIngest))
	_, err = h.q.Enqueue(bg, a, EnqueueRequest{Type: typeOf(ClassIngest)})
	codeIs(t, err, contracts.ErrResourceExhausted)
}

func asContractsError(err error, target **contracts.Error) bool {
	for err != nil {
		if ce, ok := err.(*contracts.Error); ok {
			*target = ce
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestEnqueueGuard(t *testing.T) {
	var calls atomic.Int32
	var seen []GuardRequest
	var mu sync.Mutex
	var verdict error
	guard := EnqueueGuardFunc(func(_ context.Context, req GuardRequest) error {
		calls.Add(1)
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, req)
		return verdict
	})
	h := newHarness(t, func(o *Options) { o.Guard = guard })
	a, b := scopeA(), scopeB()

	t.Run("admits and reports exact counts", func(t *testing.T) {
		h.enqueue(a, typeOf(ClassIngest))
		h.enqueue(a, typeOf(ClassIngest))
		h.leaseOne(ClassIngest) // one pending, one running for tenant A
		h.enqueue(b, typeOf(ClassIngest))
		h.enqueue(a, typeOf(ClassIngest))
		mu.Lock()
		defer mu.Unlock()
		last := seen[len(seen)-1]
		if last.TenantPending != 1 || last.TenantRunning != 1 || last.ClassPending != 2 || last.Class != ClassIngest || last.Type != typeOf(ClassIngest) {
			t.Fatalf("guard saw %+v, want tenant 1 pending/1 running, class 2 pending", last)
		}
		if last.Scope.TenantID != "tenantA" || last.ClassMaxDepth != 5000 {
			t.Fatalf("guard saw scope %q depth %d", last.Scope.TenantID, last.ClassMaxDepth)
		}
	})

	t.Run("a rejection keeps its code and inserts nothing", func(t *testing.T) {
		before := h.count(`SELECT count(*) FROM jobs`)
		for _, code := range []contracts.ErrCode{contracts.ErrRateLimited, contracts.ErrResourceExhausted} {
			mu.Lock()
			verdict = contracts.New(code, "tenant over quota").WithDetails("retry_after", "30s")
			mu.Unlock()
			_, err := h.q.Enqueue(bg, a, EnqueueRequest{Type: typeOf(ClassIngest)})
			codeIs(t, err, code)
		}
		if n := h.count(`SELECT count(*) FROM jobs`); n != before {
			t.Fatalf("guard rejection inserted a row")
		}
	})

	t.Run("a guard that fails otherwise fails closed", func(t *testing.T) {
		mu.Lock()
		verdict = errPlain
		mu.Unlock()
		before := h.count(`SELECT count(*) FROM jobs`)
		_, err := h.q.Enqueue(bg, a, EnqueueRequest{Type: typeOf(ClassIngest)})
		codeIs(t, err, contracts.ErrInternal)
		if n := h.count(`SELECT count(*) FROM jobs`); n != before {
			t.Fatalf("a failing guard admitted the job")
		}
	})

	t.Run("a repeated idempotent enqueue is not charged to the guard", func(t *testing.T) {
		mu.Lock()
		verdict = nil
		mu.Unlock()
		h.enqueue(a, typeOf(ClassRead), func(r *EnqueueRequest) { r.IdempotencyKey = "once" })
		n := calls.Load()
		res, err := h.q.Enqueue(bg, a, EnqueueRequest{Type: typeOf(ClassRead), Payload: []byte(`{"n":1}`), IdempotencyKey: "once"})
		if err != nil || !res.Duplicate {
			t.Fatalf("duplicate = %+v, %v", res, err)
		}
		if calls.Load() != n {
			t.Fatalf("guard was consulted for a duplicate enqueue")
		}
	})

	t.Run("a per-tenant cap built on the guard", func(t *testing.T) {
		mu.Lock()
		verdict = nil
		mu.Unlock()
		h2 := newHarness(t, func(o *Options) {
			o.Guard = EnqueueGuardFunc(func(_ context.Context, req GuardRequest) error {
				if req.TenantPending+req.TenantRunning >= 2 {
					return contracts.New(contracts.ErrResourceExhausted, "tenant has too many jobs queued")
				}
				return nil
			})
		})
		h2.enqueue(a, typeOf(ClassIngest))
		h2.enqueue(a, typeOf(ClassIngest))
		_, err := h2.q.Enqueue(bg, a, EnqueueRequest{Type: typeOf(ClassIngest)})
		codeIs(t, err, contracts.ErrResourceExhausted)
		h2.enqueue(b, typeOf(ClassIngest)) // another tenant is not affected
		h2.enqueue(a, typeOf(ClassRead))   // nor is another class
	})
}

func TestIdempotentEnqueueKey(t *testing.T) {
	h := newHarness(t)
	a, b := scopeA(), scopeB()
	req := EnqueueRequest{Type: typeOf(ClassIngest), Payload: []byte(`{"doc":"d1"}`), IdempotencyKey: "doc-d1"}

	first, err := h.q.Enqueue(bg, a, req)
	if err != nil || first.Duplicate {
		t.Fatalf("first enqueue = %+v, %v", first, err)
	}
	again, err := h.q.Enqueue(bg, a, req)
	if err != nil || !again.Duplicate || again.Job.ID != first.Job.ID {
		t.Fatalf("repeat = %+v, %v; want duplicate of %s", again, err, first.Job.ID)
	}
	// Whitespace in the JSON is not a different request.
	req2 := req
	req2.Payload = []byte(" { \"doc\" : \"d1\" } ")
	if again, err = h.q.Enqueue(bg, a, req2); err != nil || !again.Duplicate {
		t.Fatalf("reformatted payload = %+v, %v; want duplicate", again, err)
	}
	if n := h.count(`SELECT count(*) FROM jobs`); n != 1 {
		t.Fatalf("%d rows after repeated enqueues, want 1", n)
	}

	t.Run("the same key for a different request is a conflict", func(t *testing.T) {
		other := req
		other.Payload = []byte(`{"doc":"d2"}`)
		_, err := h.q.Enqueue(bg, a, other)
		codeIs(t, err, contracts.ErrConflict)
		other = req
		other.Type = typeOf(ClassMaintenance)
		_, err = h.q.Enqueue(bg, a, other)
		codeIs(t, err, contracts.ErrConflict)
	})

	t.Run("keys do not cross scopes", func(t *testing.T) {
		resB, err := h.q.Enqueue(bg, b, req)
		if err != nil || resB.Duplicate || resB.Job.ID == first.Job.ID {
			t.Fatalf("tenant B with tenant A's key = %+v, %v; want its own job", resB, err)
		}
		ws2 := scopeA()
		ws2.WorkspaceID = "ws2"
		resW, err := h.q.Enqueue(bg, ws2, req)
		if err != nil || resW.Duplicate {
			t.Fatalf("another workspace shares a key: %+v, %v", resW, err)
		}
	})

	t.Run("the key still holds after the job finished", func(t *testing.T) {
		l := h.leaseOne(ClassIngest)
		if err := h.q.Complete(bg, l); err != nil {
			t.Fatal(err)
		}
		res, err := h.q.Enqueue(bg, a, req)
		if err != nil || !res.Duplicate || res.Job.State != StateSucceeded {
			t.Fatalf("after completion = %+v, %v; want the finished job back", res, err)
		}
	})

	t.Run("concurrent producers create exactly one job", func(t *testing.T) {
		h := newHarness(t)
		var wg sync.WaitGroup
		var created atomic.Int32
		ids := make(chan string, 16)
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := h.q.Enqueue(bg, a, req)
				if err != nil {
					t.Errorf("Enqueue: %v", err)
					return
				}
				if !res.Duplicate {
					created.Add(1)
				}
				ids <- res.Job.ID
			}()
		}
		wg.Wait()
		close(ids)
		seen := map[string]bool{}
		for id := range ids {
			seen[id] = true
		}
		if created.Load() != 1 || len(seen) != 1 {
			t.Fatalf("created %d jobs with %d distinct ids, want 1 and 1", created.Load(), len(seen))
		}
	})
}

func TestEnqueueValidation(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Config.MaxPayloadBytes = 32 })
	a := scopeA()
	good := EnqueueRequest{Type: typeOf(ClassRead), Payload: []byte(`{}`)}

	cases := []struct {
		name   string
		scope  contracts.Scope
		mutate func(*EnqueueRequest)
		code   contracts.ErrCode
	}{
		{"no scope", contracts.Scope{}, nil, contracts.ErrScopeMissing},
		{"wildcard tenant", func() contracts.Scope { s := a; s.TenantID = "*"; return s }(), nil, contracts.ErrScopeViolation},
		{"unknown type", a, func(r *EnqueueRequest) { r.Type = "nope.job" }, contracts.ErrValidationFailed},
		{"payload not JSON", a, func(r *EnqueueRequest) { r.Payload = []byte(`{oops`) }, contracts.ErrValidationFailed},
		{"payload too large", a, func(r *EnqueueRequest) { r.Payload = []byte(`{"a":"` + string(make([]byte, 40)) + `"}`) }, contracts.ErrValidationFailed},
		{"deadline not after run_at", a, func(r *EnqueueRequest) { r.Deadline = epoch }, contracts.ErrValidationFailed},
		{"deadline before delayed run_at", a, func(r *EnqueueRequest) {
			r.RunAt = epoch.Add(time.Hour)
			r.Deadline = epoch.Add(time.Minute)
		}, contracts.ErrValidationFailed},
		{"control character in key", a, func(r *EnqueueRequest) { r.IdempotencyKey = "a\nb" }, contracts.ErrValidationFailed},
		{"over-long key", a, func(r *EnqueueRequest) { r.IdempotencyKey = string(make([]byte, 300)) }, contracts.ErrValidationFailed},
		{"control character in service identity", a, func(r *EnqueueRequest) { r.ServiceIdentity = "svc\x00" }, contracts.ErrValidationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := good
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			_, err := h.q.Enqueue(bg, tc.scope, req)
			codeIs(t, err, tc.code)
		})
	}
	if n := h.count(`SELECT count(*) FROM jobs`); n != 0 {
		t.Fatalf("%d rows were inserted by rejected enqueues", n)
	}
	// An empty payload is stored as JSON null.
	j := h.enqueue(a, typeOf(ClassRead), func(r *EnqueueRequest) { r.Payload = nil })
	if string(j.Payload) != "null" {
		t.Fatalf("empty payload stored as %q", j.Payload)
	}
}

func TestRegisterValidation(t *testing.T) {
	h := newHarness(t)
	for _, spec := range []TypeSpec{
		{Name: "", Class: 1},
		{Name: "Upper.case", Class: 1},
		{Name: "trailing.", Class: 1},
		{Name: "ok.name", Class: 0},
		{Name: "ok.name", Class: 7},
		{Name: "ok.name", Class: 1, MaxAttempts: -1},
		{Name: "ok.name", Class: 1, BackoffBase: time.Hour, BackoffMax: time.Minute},
	} {
		codeIs(t, h.q.Register(spec), contracts.ErrValidationFailed)
	}
	codeIs(t, h.q.Register(TypeSpec{Name: typeOf(ClassRead), Class: ClassRead}), contracts.ErrConflict)
}

func TestCancelPending(t *testing.T) {
	h := newHarness(t)
	a, b := scopeA(), scopeB()
	j := h.enqueue(a, typeOf(ClassIngest))
	keep := h.enqueue(a, typeOf(ClassIngest))

	// Another tenant cannot cancel it, and learns nothing about it.
	_, err := h.q.Cancel(bg, b, j.ID)
	codeIs(t, err, contracts.ErrScopeViolation)
	if got := h.get(a, j.ID); got.State != StatePending {
		t.Fatalf("a cross-tenant cancel changed the job to %s", got.State)
	}
	_, missing := h.q.Cancel(bg, a, "job_does_not_exist")
	if contracts.CodeOf(missing) != contracts.ErrScopeViolation || missing.Error() != err.Error() {
		t.Fatalf("a missing job and another tenant's job must look the same: %v vs %v", missing, err)
	}

	h.clk.Advance(time.Second)
	got, err := h.q.Cancel(bg, a, j.ID)
	if err != nil || got.State != StateCancelled || !got.FinishedAt.Equal(epoch.Add(time.Second)) {
		t.Fatalf("Cancel = %+v, %v", got, err)
	}
	// It is never leased; the other job is.
	if l := h.leaseOne(ClassIngest); l.ID != keep.ID {
		t.Fatalf("leased %s, want the job that was not cancelled", l.ID)
	}
	// Cancelling again is a no-op, not an error.
	if got, err = h.q.Cancel(bg, a, j.ID); err != nil || got.State != StateCancelled {
		t.Fatalf("second Cancel = %+v, %v", got, err)
	}
	// A cancelled job frees its place in the queue's depth.
	if n := h.count(`SELECT count(*) FROM jobs WHERE state = 'pending'`); n != 0 {
		t.Fatalf("%d pending after cancel and lease", n)
	}

	t.Run("a finished job cannot be cancelled and the caller is told", func(t *testing.T) {
		done := h.enqueue(a, typeOf(ClassRead))
		l := h.leaseOne(ClassRead)
		if err := h.q.Complete(bg, l); err != nil {
			t.Fatal(err)
		}
		_, err := h.q.Cancel(bg, a, done.ID)
		codeIs(t, err, contracts.ErrConflict)
		if h.get(a, done.ID).State != StateSucceeded {
			t.Fatalf("a failed cancel altered the job")
		}
	})
}

func TestGetAndScopedStatusAreTenantConfined(t *testing.T) {
	h := newHarness(t)
	a, b := scopeA(), scopeB()
	ja := h.enqueue(a, typeOf(ClassIngest))
	h.enqueue(a, typeOf(ClassIngest))
	h.enqueue(b, typeOf(ClassRead))

	if _, err := h.q.Get(bg, b, ja.ID); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("tenant B read tenant A's job: %v", err)
	}
	// A workspace is a scope boundary too.
	ws2 := a
	ws2.WorkspaceID = "ws2"
	if _, err := h.q.Get(bg, ws2, ja.ID); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("another workspace read the job: %v", err)
	}
	if _, err := h.q.Get(bg, contracts.Scope{}, ja.ID); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Fatalf("Get without scope = %v, want scope_missing", err)
	}

	sa, err := h.q.ScopedStatus(bg, a)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := h.q.ScopedStatus(bg, b)
	if err != nil {
		t.Fatal(err)
	}
	if sa.Classes[ClassIngest-1].Pending != 2 || sa.Classes[ClassRead-1].Pending != 0 {
		t.Fatalf("tenant A status = %+v", sa.Classes)
	}
	if sb.Classes[ClassIngest-1].Pending != 0 || sb.Classes[ClassRead-1].Pending != 1 {
		t.Fatalf("tenant B status = %+v", sb.Classes)
	}
	if _, err := h.q.ScopedStatus(bg, contracts.Scope{}); contracts.CodeOf(err) != contracts.ErrScopeMissing {
		t.Fatalf("ScopedStatus without scope = %v", err)
	}
}

func TestStatusCountsPerClassAndState(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	h.enqueue(a, typeOf(ClassIngest))
	h.enqueue(a, typeOf(ClassIngest))
	h.enqueue(a, typeOf(ClassIngest))
	h.enqueue(a, typeOf(ClassTurn))
	h.enqueue(a, typeOf(ClassMaintenance), func(r *EnqueueRequest) { r.RunAt = epoch.Add(time.Hour) })
	done := h.leaseOne(ClassTurn)
	if err := h.q.Complete(bg, done); err != nil {
		t.Fatal(err)
	}
	l := h.leaseOne(ClassIngest)
	if _, err := h.q.Fail(bg, l, contracts.New(contracts.ErrValidationFailed, "bad")); err != nil {
		t.Fatal(err)
	}
	run := h.leaseOne(ClassIngest)
	_ = run
	h.clk.Advance(30 * time.Second)

	st, err := h.q.Status(bg)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Classes) != 6 {
		t.Fatalf("Status lists %d classes, want 6", len(st.Classes))
	}
	want := map[Class]ClassStatus{
		ClassTurn:        {Succeeded: 1},
		ClassIngest:      {Pending: 1, Running: 1, Failed: 1, OldestReadyAgeMs: 30_000},
		ClassMaintenance: {Pending: 1}, // delayed: pending but not ready, so no age
	}
	for _, cs := range st.Classes {
		w := want[cs.Class]
		got := ClassStatus{Pending: cs.Pending, Running: cs.Running, Succeeded: cs.Succeeded, Failed: cs.Failed, Cancelled: cs.Cancelled, OldestReadyAgeMs: cs.OldestReadyAgeMs}
		if got != w {
			t.Errorf("class %s: %+v, want %+v", cs.Class, got, w)
		}
		if cs.Name != cs.Class.String() || cs.Workers == 0 || cs.MaxDepth == 0 {
			t.Errorf("class %s: limits missing: %+v", cs.Class, cs)
		}
	}
	if !st.At.Equal(h.clk.Now()) {
		t.Fatalf("Status.At = %s", st.At)
	}
}

// A fair policy needs one candidate per tenant however many each has queued.
func TestDequeuePolicyOrdersWithinAClass(t *testing.T) {
	var offered [][]string
	var last string
	roundRobin := DequeuePolicyFunc(func(ready []Candidate) int {
		var tenants []string
		for _, c := range ready {
			tenants = append(tenants, c.TenantID)
		}
		offered = append(offered, tenants)
		for i, c := range ready {
			if c.TenantID != last {
				last = c.TenantID
				return i
			}
		}
		last = ready[0].TenantID
		return 0
	})
	h := newHarness(t, func(o *Options) { o.Policy = roundRobin })
	a, b := scopeA(), scopeB()
	ids := map[string]string{}
	for i := 0; i < 4; i++ {
		ids[h.enqueue(a, typeOf(ClassIngest)).ID] = "A"
	}
	ids[h.enqueue(b, typeOf(ClassIngest)).ID] = "B"
	urgent := h.enqueue(a, typeOf(ClassAuthCommit))
	ids[urgent.ID] = "A-urgent"

	var got []string
	for len(got) < 5 {
		// Class 1 first, whatever the policy would like: it only orders inside a class.
		l := h.leaseOne(Classes()...)
		got = append(got, ids[l.ID])
	}
	if want := []string{"A-urgent", "B", "A", "A", "A"}; !cmp.Equal(got, want) {
		t.Fatalf("round-robin order = %v, want %v (default FIFO would serve B last)", got, want)
	}
	// With A having four queued ahead of B, the policy still saw both tenants.
	if len(offered[1]) != 2 {
		t.Fatalf("policy was offered %v, want one head per tenant", offered[1])
	}

	t.Run("a policy that declines leases nothing from the class", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.Policy = DequeuePolicyFunc(func([]Candidate) int { return -1 }) })
		h.enqueue(a, typeOf(ClassIngest))
		if got := h.lease(1, ClassIngest); len(got) != 0 {
			t.Fatalf("leased despite the policy declining")
		}
		if got := h.lease(1, ClassIngest); len(got) != 0 {
			t.Fatalf("leased despite the policy declining")
		}
	})

	t.Run("an out-of-range pick leases nothing", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.Policy = DequeuePolicyFunc(func(r []Candidate) int { return len(r) }) })
		h.enqueue(a, typeOf(ClassIngest))
		if got := h.lease(1, ClassIngest); len(got) != 0 {
			t.Fatalf("leased on an invalid pick")
		}
	})

	t.Run("the window bounds what the policy sees", func(t *testing.T) {
		var n int
		h := newHarness(t, func(o *Options) {
			o.Config.PolicyWindow = 3
			o.Policy = DequeuePolicyFunc(func(r []Candidate) int { n = len(r); return 0 })
		})
		for i := 0; i < 6; i++ {
			s := storetest.Scope("app", "t"+string(rune('a'+i)), "ws")
			h.enqueue(s, typeOf(ClassIngest))
		}
		h.leaseOne(ClassIngest)
		if n != 3 {
			t.Fatalf("policy saw %d candidates, want the window of 3", n)
		}
	})
}

func TestStoredScopeMismatchIsRefused(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	bad := h.enqueue(a, typeOf(ClassRead))
	good := h.enqueue(a, typeOf(ClassRead))
	// Tamper with the indexed copy of the scope; the stored scope no longer matches.
	if _, err := h.db.ExecContext(bg, `UPDATE jobs SET tenant_id = 'tenantB' WHERE id = ?`, bad.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.q.Get(bg, scopeB(), bad.ID); contracts.CodeOf(err) != contracts.ErrInternal {
		t.Fatalf("Get of a tampered row = %v, want internal", err)
	}
	// The tampered job is never leased, and it does not block the jobs behind it.
	l := h.leaseOne(ClassRead)
	if l.ID != good.ID {
		t.Fatalf("leased %s, want the intact job", l.ID)
	}
	var state, code string
	if err := h.db.QueryRowContext(bg, `SELECT state, error_code FROM jobs WHERE id = ?`, bad.ID).Scan(&state, &code); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || code != "internal" {
		t.Fatalf("tampered job is %s/%s, want quarantined as failed/internal", state, code)
	}
}

func TestMigrationHasScopeColumns(t *testing.T) {
	h := newHarness(t)
	storetest.AssertScopedTables(t, h.db)
}

func TestPruneDeletesOldFinishedJobs(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Config.RetainFinished = time.Hour })
	a := scopeA()
	old := h.enqueue(a, typeOf(ClassRead), func(r *EnqueueRequest) { r.IdempotencyKey = "k" })
	l := h.leaseOne(ClassRead)
	if err := h.q.Complete(bg, l); err != nil {
		t.Fatal(err)
	}
	waiting := h.enqueue(a, typeOf(ClassRead))
	running := h.enqueue(a, typeOf(ClassIngest))
	h.leaseOne(ClassIngest)

	h.clk.Advance(59 * time.Minute)
	if n, err := h.q.Prune(bg); err != nil || n != 0 {
		t.Fatalf("Prune inside the retention window = %d, %v", n, err)
	}
	fresh := h.enqueue(a, typeOf(ClassRead))
	fl := h.leaseOne(ClassRead)
	_ = fl
	if err := h.q.Complete(bg, fl); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(2 * time.Minute)
	n, err := h.q.Prune(bg)
	if err != nil || n != 1 {
		t.Fatalf("Prune = %d, %v; want only the old finished job", n, err)
	}
	if _, err := h.q.Get(bg, a, old.ID); contracts.CodeOf(err) != contracts.ErrScopeViolation {
		t.Fatalf("pruned job still readable: %v", err)
	}
	for _, id := range []string{waiting.ID, running.ID, fresh.ID} {
		h.get(a, id) // live and recently finished jobs are untouched
	}
	// The pruned job's idempotency key is free again.
	res, err := h.q.Enqueue(bg, a, EnqueueRequest{Type: typeOf(ClassRead), Payload: []byte(`{"n":1}`), IdempotencyKey: "k"})
	if err != nil || res.Duplicate {
		t.Fatalf("key after prune = %+v, %v", res, err)
	}
}

func TestConcurrentLeasesNeverShareAJob(t *testing.T) {
	h := newHarness(t)
	a := scopeA()
	const total = 120
	for i := 0; i < total; i++ {
		h.enqueue(a, typeOf(ClassRead))
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, err := h.q.Lease(bg, LeaseRequest{Classes: []Class{ClassRead}, Max: 5})
				if err != nil {
					t.Errorf("Lease: %v", err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, l := range got {
					seen[l.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("leased %d distinct jobs, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %s leased %d times", id, n)
		}
	}
	if n := h.count(`SELECT count(*) FROM jobs WHERE state = 'running' AND attempts = 1`); n != total {
		t.Fatalf("%d jobs running with one attempt, want %d", n, total)
	}
}
