package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

const (
	testApp  = "app_demo"
	testRead = "items.get"
	testAct  = "items.create"
)

func testScope() contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep_1",
		ApplicationID:    testApp,
		TenantID:         "ten_a",
		WorkspaceID:      "wks_a",
		PrincipalID:      "usr_1",
		Roles:            []string{"member"},
		CapabilityScopes: []string{testRead, testAct},
		SessionID:        "ses_1",
	}
}

func testPrepared() actions.PreparedAction {
	return actions.PreparedAction{
		PreparationID:  "prep_01J0000000000000000000000A",
		CapabilityID:   testAct,
		IdempotencyKey: testKey,
	}
}

// stubAdapter is a scriptable AppAdapter that counts every call. A nil handler
// answers with a valid result.
type stubAdapter struct {
	mu     sync.Mutex
	calls  []Operation
	result func(ctx context.Context, op Operation) (actions.DeterministicResult, error)
	health func(ctx context.Context) (HealthReport, error)
}

func (s *stubAdapter) record(op Operation) {
	s.mu.Lock()
	s.calls = append(s.calls, op)
	s.mu.Unlock()
}

func (s *stubAdapter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *stubAdapter) answer(ctx context.Context, op Operation, capID string) (actions.DeterministicResult, error) {
	s.record(op)
	if s.result != nil {
		return s.result(ctx, op)
	}
	return validResult(op, capID), nil
}

// validResult is a valid result for the operation, built through Normalise.
func validResult(op Operation, capID string) actions.DeterministicResult {
	raw := validCommit()
	exp := Expect{Operation: op, CapabilityID: capID}
	if needsKey(op) {
		exp.IdempotencyKey = testKey
	}
	if op == OpPrepare {
		raw = withMemberNoT(raw, "preparation", map[string]any{
			"summary":             "Create an item",
			"effects":             []any{map[string]any{"operation": "create", "entity_type": "item", "description": "Create an item.", "changes": []any{}, "external": false}},
			"data_versions":       []any{map[string]any{"entity_type": "item_collection", "entity_id": "ten_a", "version": "v1"}},
			"declared_risk_level": 1,
		})
	}
	res, err := Normalise([]byte(raw), exp)
	if err != nil {
		panic(err)
	}
	return res
}

func withMemberNoT(doc, key string, value any) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(doc), &m)
	m[key] = value
	b, _ := json.Marshal(m)
	return string(b)
}

func (s *stubAdapter) Describe(context.Context) (Description, error) {
	s.record(OpDescribe)
	return Description{Transport: "stub", ContractVersions: []int{1}}, nil
}
func (s *stubAdapter) Read(ctx context.Context, _ contracts.Scope, r ReadRequest) (actions.DeterministicResult, error) {
	return s.answer(ctx, OpRead, r.CapabilityID)
}
func (s *stubAdapter) Validate(ctx context.Context, _ contracts.Scope, r ValidateRequest) (actions.DeterministicResult, error) {
	return s.answer(ctx, OpValidate, r.CapabilityID)
}
func (s *stubAdapter) Prepare(ctx context.Context, _ contracts.Scope, r PrepareRequest) (actions.DeterministicResult, error) {
	return s.answer(ctx, OpPrepare, r.CapabilityID)
}
func (s *stubAdapter) Commit(ctx context.Context, _ contracts.Scope, r CommitRequest) (actions.DeterministicResult, error) {
	return s.answer(ctx, OpCommit, r.CapabilityID)
}
func (s *stubAdapter) Status(ctx context.Context, _ contracts.Scope, r StatusRequest) (actions.DeterministicResult, error) {
	return s.answer(ctx, OpStatus, r.CapabilityID)
}
func (s *stubAdapter) Cancel(ctx context.Context, _ contracts.Scope, r CancelRequest) (actions.DeterministicResult, error) {
	return s.answer(ctx, OpCancel, r.CapabilityID)
}
func (s *stubAdapter) Health(ctx context.Context) (HealthReport, error) {
	s.record(OpHealth)
	if s.health != nil {
		return s.health(ctx)
	}
	return HealthReport{Status: HealthOK}, nil
}
func (s *stubAdapter) Inspect(context.Context, contracts.Scope, InspectRequest) (Inspection, error) {
	s.record(OpInspect)
	return Inspection{}, nil
}

func wrapStub(t *testing.T, s *stubAdapter, opts Options) AppAdapter {
	t.Helper()
	if opts.ApplicationID == "" {
		opts.ApplicationID = testApp
	}
	a, err := Wrap(s, opts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// callOp invokes one operation of the wrapped adapter with a valid request.
func callOp(ctx context.Context, a AppAdapter, op Operation, scope contracts.Scope) (actions.DeterministicResult, error) {
	switch op {
	case OpRead:
		return a.Read(ctx, scope, ReadRequest{CapabilityID: testRead})
	case OpValidate:
		return a.Validate(ctx, scope, ValidateRequest{CapabilityID: testAct, Fields: map[string]any{"name": "Widget"}})
	case OpPrepare:
		return a.Prepare(ctx, scope, PrepareRequest{CapabilityID: testAct, Fields: map[string]any{"name": "Widget"}})
	case OpCommit:
		return a.Commit(ctx, scope, CommitRequest{CapabilityID: testAct, IdempotencyKey: testKey, Prepared: testPrepared()})
	case OpStatus:
		return a.Status(ctx, scope, StatusRequest{CapabilityID: testAct, IdempotencyKey: testKey})
	case OpCancel:
		return a.Cancel(ctx, scope, CancelRequest{CapabilityID: testAct, IdempotencyKey: testKey})
	}
	panic("not a result operation: " + string(op))
}

// TestGuardRefusesBeforeAnyIO: an invalid or foreign scope, a malformed request
// and a capability the token does not permit are all refused without a call to
// the transport.
//
// SEC-044
func TestGuardRefusesBeforeAnyIO(t *testing.T) {
	mutScope := func(f func(s *contracts.Scope)) contracts.Scope {
		s := testScope().Clone()
		f(&s)
		return s
	}
	scopes := map[string]struct {
		scope contracts.Scope
		want  contracts.ErrCode
	}{
		"zero scope":                {contracts.Scope{}, contracts.ErrScopeMissing},
		"no tenant":                 {mutScope(func(s *contracts.Scope) { s.TenantID = "" }), contracts.ErrScopeMissing},
		"no principal":              {mutScope(func(s *contracts.Scope) { s.PrincipalID = " " }), contracts.ErrScopeMissing},
		"no roles":                  {mutScope(func(s *contracts.Scope) { s.Roles = nil }), contracts.ErrScopeMissing},
		"no session":                {mutScope(func(s *contracts.Scope) { s.SessionID = "" }), contracts.ErrScopeMissing},
		"wildcard tenant":           {mutScope(func(s *contracts.Scope) { s.TenantID = "*" }), contracts.ErrScopeViolation},
		"padded workspace":          {mutScope(func(s *contracts.Scope) { s.WorkspaceID = " wks_a" }), contracts.ErrScopeViolation},
		"another application":       {mutScope(func(s *contracts.Scope) { s.ApplicationID = "app_other" }), contracts.ErrScopeViolation},
		"capability not in token":   {mutScope(func(s *contracts.Scope) { s.CapabilityScopes = []string{"other.cap"} }), contracts.ErrScopeViolation},
		"capability scopes missing": {mutScope(func(s *contracts.Scope) { s.CapabilityScopes = nil }), contracts.ErrScopeMissing},
	}
	for _, op := range ResultOperations() {
		for name, tc := range scopes {
			t.Run(string(op)+"/"+name, func(t *testing.T) {
				s := &stubAdapter{}
				a := wrapStub(t, s, Options{})
				res, err := callOp(context.Background(), a, op, tc.scope)
				if err == nil {
					t.Fatalf("accepted: %+v", res)
				}
				if contracts.CodeOf(err) != tc.want {
					t.Fatalf("code %s (%v), want %s", contracts.CodeOf(err), err, tc.want)
				}
				if s.count() != 0 {
					t.Fatalf("the transport was called %d times for a refused request", s.count())
				}
			})
		}
	}

	t.Run("inspect and describe scope", func(t *testing.T) {
		s := &stubAdapter{}
		a := wrapStub(t, s, Options{})
		if _, err := a.Inspect(context.Background(), contracts.Scope{}, InspectRequest{}); contracts.CodeOf(err) != contracts.ErrScopeMissing {
			t.Fatalf("inspect with no scope: %v", err)
		}
		other := testScope()
		other.ApplicationID = "app_other"
		if _, err := a.Inspect(context.Background(), other, InspectRequest{}); contracts.CodeOf(err) != contracts.ErrScopeViolation {
			t.Fatalf("inspect for another application: %v", err)
		}
		if s.count() != 0 {
			t.Fatal("the transport was called")
		}
	})

	t.Run("malformed requests", func(t *testing.T) {
		s := &stubAdapter{}
		a := wrapStub(t, s, Options{})
		ctx, sc := context.Background(), testScope()
		prep := testPrepared()
		otherCap := testPrepared()
		otherCap.CapabilityID = "items.delete"
		otherKey := testPrepared()
		otherKey.IdempotencyKey = "idem_other_0123456789"
		noPrep := testPrepared()
		noPrep.PreparationID = ""
		longStr := make([]byte, maxFieldStringRunes+1)
		for i := range longStr {
			longStr[i] = 'x'
		}
		calls := map[string]func() error{
			"read with no capability": func() error { _, err := a.Read(ctx, sc, ReadRequest{}); return err },
			"read with bad field name": func() error {
				_, err := a.Read(ctx, sc, ReadRequest{CapabilityID: testRead, Fields: map[string]any{"Bad-Name": 1}})
				return err
			},
			"read with a nested object": func() error {
				_, err := a.Read(ctx, sc, ReadRequest{CapabilityID: testRead, Fields: map[string]any{"q": map[string]any{"a": 1}}})
				return err
			},
			"read with null": func() error {
				_, err := a.Read(ctx, sc, ReadRequest{CapabilityID: testRead, Fields: map[string]any{"q": nil}})
				return err
			},
			"read with a nested list": func() error {
				_, err := a.Read(ctx, sc, ReadRequest{CapabilityID: testRead, Fields: map[string]any{"q": []any{[]any{1}}}})
				return err
			},
			"read with an over-long string": func() error {
				_, err := a.Read(ctx, sc, ReadRequest{CapabilityID: testRead, Fields: map[string]any{"q": string(longStr)}})
				return err
			},
			"read with a channel": func() error {
				_, err := a.Read(ctx, sc, ReadRequest{CapabilityID: testRead, Fields: map[string]any{"q": make(chan int)}})
				return err
			},
			"validate with no capability": func() error { _, err := a.Validate(ctx, sc, ValidateRequest{}); return err },
			"prepare with no capability":  func() error { _, err := a.Prepare(ctx, sc, PrepareRequest{}); return err },
			"commit without a key": func() error {
				_, err := a.Commit(ctx, sc, CommitRequest{CapabilityID: testAct, Prepared: prep})
				return err
			},
			"commit with a short key": func() error {
				p := prep
				p.IdempotencyKey = "short"
				_, err := a.Commit(ctx, sc, CommitRequest{CapabilityID: testAct, IdempotencyKey: "short", Prepared: p})
				return err
			},
			"commit without a prepared action": func() error {
				_, err := a.Commit(ctx, sc, CommitRequest{CapabilityID: testAct, IdempotencyKey: testKey, Prepared: noPrep})
				return err
			},
			"commit with another capability's preparation": func() error {
				_, err := a.Commit(ctx, sc, CommitRequest{CapabilityID: testAct, IdempotencyKey: testKey, Prepared: otherCap})
				return err
			},
			"commit with another key's preparation": func() error {
				_, err := a.Commit(ctx, sc, CommitRequest{CapabilityID: testAct, IdempotencyKey: testKey, Prepared: otherKey})
				return err
			},
			"status without a key": func() error { _, err := a.Status(ctx, sc, StatusRequest{CapabilityID: testAct}); return err },
			"cancel with a bad key": func() error {
				_, err := a.Cancel(ctx, sc, CancelRequest{CapabilityID: testAct, IdempotencyKey: "has spaces in it!!"})
				return err
			},
			"inspect with an unknown aspect": func() error {
				_, err := a.Inspect(ctx, sc, InspectRequest{Aspects: []Aspect{"secrets"}})
				return err
			},
			"inspect with a repeated aspect": func() error {
				_, err := a.Inspect(ctx, sc, InspectRequest{Aspects: []Aspect{AspectHealth, AspectHealth}})
				return err
			},
		}
		for name, call := range calls {
			if err := call(); contracts.CodeOf(err) != contracts.ErrValidationFailed {
				t.Errorf("%s: got %v, want validation_failed", name, err)
			}
		}
		if s.count() != 0 {
			t.Fatalf("the transport was called %d times for malformed requests", s.count())
		}
	})

	t.Run("a valid request reaches the transport once", func(t *testing.T) {
		for _, op := range ResultOperations() {
			s := &stubAdapter{}
			a := wrapStub(t, s, Options{})
			res, err := callOp(context.Background(), a, op, testScope())
			if err != nil {
				t.Fatalf("%s: %v", op, err)
			}
			if string(res.Operation) != string(op) || s.count() != 1 {
				t.Fatalf("%s: result operation %s, %d transport calls", op, res.Operation, s.count())
			}
		}
	})
}

// TestGuardRevalidatesResults: a transport that returns a result the schema
// would refuse, or one for another call, gets schema_invalid, and for Commit
// uncertain_outcome with schema_invalid as the cause.
//
// SEC-050
func TestGuardRevalidatesResults(t *testing.T) {
	bad := map[string]func(op Operation) actions.DeterministicResult{
		"ok with errors": func(op Operation) actions.DeterministicResult {
			r := validResult(op, testAct)
			r.Errors = []actions.Issue{{Code: "conflict", Message: "x"}}
			return r
		},
		"zero result": func(Operation) actions.DeterministicResult { return actions.DeterministicResult{} },
		"another operation": func(op Operation) actions.DeterministicResult {
			r := validResult(op, testAct)
			if op == OpRead {
				r.Operation = actions.DeterministicResultOperationStatus
			} else {
				r.Operation = actions.DeterministicResultOperationRead
			}
			return r
		},
		"another capability": func(op Operation) actions.DeterministicResult {
			r := validResult(op, testAct)
			r.CapabilityID = "items.other"
			return r
		},
	}
	for _, op := range ResultOperations() {
		for name, mk := range bad {
			t.Run(string(op)+"/"+name, func(t *testing.T) {
				capID := testAct
				if op == OpRead {
					capID = testRead
				}
				s := &stubAdapter{result: func(context.Context, Operation) (actions.DeterministicResult, error) {
					r := mk(op)
					if r.CapabilityID == testAct && capID == testRead {
						r.CapabilityID = testRead
					}
					return r, nil
				}}
				a := wrapStub(t, s, Options{})
				res, err := callOp(context.Background(), a, op, testScope())
				if err == nil {
					t.Fatalf("accepted %+v", res)
				}
				if !contracts.HasCode(err, contracts.ErrSchemaInvalid) {
					t.Fatalf("got %v, want schema_invalid", err)
				}
				if op == OpCommit && contracts.CodeOf(err) != contracts.ErrUncertainOutcome {
					t.Fatalf("a commit with an unusable reply is %s, want uncertain_outcome", contracts.CodeOf(err))
				}
				if op != OpCommit && contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
					t.Fatalf("code %s, want schema_invalid", contracts.CodeOf(err))
				}
				if res.Status != "" || len(res.Identifiers) != 0 {
					t.Fatalf("a rejected reply returned a result: %+v", res)
				}
				if s.count() != 1 {
					t.Fatalf("%d transport calls, want 1 (never retry)", s.count())
				}
			})
		}
	}
}

// TestGuardTimeoutsAndCancellation: a hung transport is cut off at the
// deadline, the error type depends on whether the call may have changed the
// host, and nothing is retried.
//
// SEC-051
func TestGuardTimeoutsAndCancellation(t *testing.T) {
	hang := func(ctx context.Context, _ Operation) (actions.DeterministicResult, error) {
		<-ctx.Done()
		return actions.DeterministicResult{}, ctx.Err()
	}

	t.Run("default timeout bounds a call with no deadline", func(t *testing.T) {
		for _, op := range ResultOperations() {
			s := &stubAdapter{result: hang}
			a := wrapStub(t, s, Options{DefaultTimeout: 40 * time.Millisecond})
			start := time.Now()
			_, err := callOp(context.Background(), a, op, testScope())
			if time.Since(start) > 5*time.Second {
				t.Fatalf("%s: took %s", op, time.Since(start))
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s: %v does not wrap context.DeadlineExceeded", op, err)
			}
			want := contracts.ErrTimeout
			if op == OpCommit {
				want = contracts.ErrUncertainOutcome
			}
			if contracts.CodeOf(err) != want || !contracts.HasCode(err, contracts.ErrTimeout) {
				t.Fatalf("%s: code %s (%v), want %s with a timeout cause", op, contracts.CodeOf(err), err, want)
			}
			if s.count() != 1 {
				t.Fatalf("%s: %d calls", op, s.count())
			}
		}
	})

	t.Run("the caller's earlier deadline wins", func(t *testing.T) {
		s := &stubAdapter{result: hang}
		a := wrapStub(t, s, Options{DefaultTimeout: time.Hour})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := callOp(ctx, a, OpRead, testScope())
		if contracts.CodeOf(err) != contracts.ErrTimeout || time.Since(start) > 5*time.Second {
			t.Fatalf("got %v after %s", err, time.Since(start))
		}
	})

	t.Run("MaxTimeout caps a longer deadline", func(t *testing.T) {
		s := &stubAdapter{result: hang}
		a := wrapStub(t, s, Options{MaxTimeout: 40 * time.Millisecond})
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		start := time.Now()
		_, err := callOp(ctx, a, OpRead, testScope())
		if contracts.CodeOf(err) != contracts.ErrTimeout || time.Since(start) > 5*time.Second {
			t.Fatalf("got %v after %s", err, time.Since(start))
		}
	})

	t.Run("cancellation by the caller", func(t *testing.T) {
		for _, op := range []Operation{OpRead, OpCommit} {
			s := &stubAdapter{result: hang}
			a := wrapStub(t, s, Options{})
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				for s.count() == 0 {
					time.Sleep(time.Millisecond)
				}
				cancel()
			}()
			_, err := callOp(ctx, a, op, testScope())
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s: %v does not wrap context.Canceled", op, err)
			}
			if contracts.HasCode(err, contracts.ErrTimeout) {
				t.Fatalf("%s: a cancellation was reported as a timeout", op)
			}
			if op == OpCommit && contracts.CodeOf(err) != contracts.ErrUncertainOutcome {
				t.Fatalf("a commit cancelled after dispatch is %s, want uncertain_outcome", contracts.CodeOf(err))
			}
			if op == OpRead && contracts.CodeOf(err) == contracts.ErrUncertainOutcome {
				t.Fatal("a cancelled read is not uncertain")
			}
		}
	})

	t.Run("a context that is already done sends nothing", func(t *testing.T) {
		for _, mk := range []func() (context.Context, context.CancelFunc){
			func() (context.Context, context.CancelFunc) {
				c, cancel := context.WithCancel(context.Background())
				cancel()
				return c, cancel
			},
			func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
		} {
			ctx, cancel := mk()
			s := &stubAdapter{}
			a := wrapStub(t, s, Options{})
			_, err := callOp(ctx, a, OpCommit, testScope())
			cancel()
			if err == nil || s.count() != 0 {
				t.Fatalf("err %v, %d calls", err, s.count())
			}
			if contracts.CodeOf(err) == contracts.ErrUncertainOutcome {
				t.Fatalf("nothing was sent, but the commit is called uncertain: %v", err)
			}
		}
	})
}

// TestGuardClassifiesTransportFailures: typed errors keep their code, untyped
// ones become typed, and a Commit failure that leaves the outcome unknown is
// uncertain_outcome while one that proves no effect is left alone. Nothing is
// retried.
func TestGuardClassifiesTransportFailures(t *testing.T) {
	typed := func(c contracts.ErrCode) error { return contracts.New(c, "from the transport") }
	type row struct {
		name       string
		err        error
		wantRead   contracts.ErrCode
		wantCommit contracts.ErrCode
	}
	rows := []row{
		{"unavailable", typed(contracts.ErrUnavailable), contracts.ErrUnavailable, contracts.ErrUnavailable},
		{"permission denied", typed(contracts.ErrPermissionDenied), contracts.ErrPermissionDenied, contracts.ErrPermissionDenied},
		{"validation failed", typed(contracts.ErrValidationFailed), contracts.ErrValidationFailed, contracts.ErrValidationFailed},
		{"stale", typed(contracts.ErrStale), contracts.ErrStale, contracts.ErrStale},
		{"conflict", typed(contracts.ErrConflict), contracts.ErrConflict, contracts.ErrConflict},
		{"rate limited", typed(contracts.ErrRateLimited), contracts.ErrRateLimited, contracts.ErrRateLimited},
		{"resource exhausted", typed(contracts.ErrResourceExhausted), contracts.ErrResourceExhausted, contracts.ErrResourceExhausted},
		{"uncertain already", typed(contracts.ErrUncertainOutcome), contracts.ErrUncertainOutcome, contracts.ErrUncertainOutcome},
		{"timeout", typed(contracts.ErrTimeout), contracts.ErrTimeout, contracts.ErrUncertainOutcome},
		{"schema invalid", typed(contracts.ErrSchemaInvalid), contracts.ErrSchemaInvalid, contracts.ErrUncertainOutcome},
		{"internal", typed(contracts.ErrInternal), contracts.ErrInternal, contracts.ErrUncertainOutcome},
		{"untyped", errors.New("EOF"), contracts.ErrInternal, contracts.ErrUncertainOutcome},
		{"wrapped typed", errors.Join(errors.New("ctx"), typed(contracts.ErrUnavailable)), contracts.ErrUnavailable, contracts.ErrUnavailable},
	}
	for _, r := range rows {
		for _, op := range []Operation{OpRead, OpCommit} {
			s := &stubAdapter{result: func(context.Context, Operation) (actions.DeterministicResult, error) {
				return actions.DeterministicResult{}, r.err
			}}
			a := wrapStub(t, s, Options{})
			_, err := callOp(context.Background(), a, op, testScope())
			want := r.wantRead
			if op == OpCommit {
				want = r.wantCommit
			}
			if contracts.CodeOf(err) != want {
				t.Errorf("%s/%s: code %s (%v), want %s", r.name, op, contracts.CodeOf(err), err, want)
			}
			if s.count() != 1 {
				t.Errorf("%s/%s: %d transport calls; the guard must never retry", r.name, op, s.count())
			}
			if op == OpCommit && want == contracts.ErrUncertainOutcome && contracts.CodeOf(err).Retryable() {
				t.Errorf("%s: uncertain_outcome must not be retryable", r.name)
			}
		}
	}

	t.Run("a panic is contained", func(t *testing.T) {
		for _, op := range []Operation{OpRead, OpCommit} {
			s := &stubAdapter{result: func(context.Context, Operation) (actions.DeterministicResult, error) { panic("boom") }}
			a := wrapStub(t, s, Options{})
			_, err := callOp(context.Background(), a, op, testScope())
			want := contracts.ErrInternal
			if op == OpCommit {
				want = contracts.ErrUncertainOutcome
			}
			if contracts.CodeOf(err) != want {
				t.Errorf("%s: %v, want %s", op, err, want)
			}
		}
	})

	t.Run("a host refusal is a result, not an error", func(t *testing.T) {
		s := &stubAdapter{result: func(_ context.Context, op Operation) (actions.DeterministicResult, error) {
			res, err := Normalise([]byte(`{"status":"rejected","identifiers":[],"values":[],"errors":[{"code":"permission_denied","message":"not allowed"}],"allowed_next_actions":[]}`),
				Expect{Operation: op, CapabilityID: testAct, IdempotencyKey: testKey})
			return res, err
		}}
		a := wrapStub(t, s, Options{})
		res, err := callOp(context.Background(), a, OpCommit, testScope())
		if err != nil || res.Status != actions.DeterministicResultStatusRejected || len(res.Errors) != 1 {
			t.Fatalf("got %+v, %v", res, err)
		}
	})
}

// TestGuardBoundsConcurrency: a hung application cannot absorb unbounded
// calls; the call over the limit is refused at once, and Health still answers.
//
// SEC-051
func TestGuardBoundsConcurrency(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int32
	s := &stubAdapter{result: func(ctx context.Context, _ Operation) (actions.DeterministicResult, error) {
		started.Add(1)
		select {
		case <-release:
			return validResult(OpRead, testRead), nil
		case <-ctx.Done():
			return actions.DeterministicResult{}, ctx.Err()
		}
	}}
	a := wrapStub(t, s, Options{MaxInFlight: 2})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := callOp(context.Background(), a, OpRead, testScope()); err != nil {
				t.Errorf("held call: %v", err)
			}
		}()
	}
	for started.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	_, err := callOp(context.Background(), a, OpRead, testScope())
	if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
		t.Fatalf("the call over the limit: %v, want resource_exhausted", err)
	}
	if s.count() != 2 {
		t.Fatalf("the refused call reached the transport (%d calls)", s.count())
	}
	if h, err := a.Health(context.Background()); err != nil || h.Status != HealthOK {
		t.Fatalf("Health while every slot is busy: %+v, %v", h, err)
	}
	close(release)
	wg.Wait()
	if _, err := callOp(context.Background(), a, OpRead, testScope()); err != nil {
		t.Fatalf("after the slots were released: %v", err)
	}
}

func TestGuardHealth(t *testing.T) {
	cases := map[string]struct {
		health func(context.Context) (HealthReport, error)
		want   HealthReport
	}{
		"ok": {func(context.Context) (HealthReport, error) {
			return HealthReport{Status: HealthOK, ApplicationVersion: "1"}, nil
		}, HealthReport{Status: HealthOK, ApplicationVersion: "1"}},
		"degraded": {func(context.Context) (HealthReport, error) {
			return HealthReport{Status: HealthDegraded, Reason: "slow"}, nil
		}, HealthReport{Status: HealthDegraded, Reason: "slow"}},
		"probe unavailable": {func(context.Context) (HealthReport, error) {
			return HealthReport{}, contracts.New(contracts.ErrUnavailable, "SECRET host text")
		}, HealthReport{Status: HealthUnavailable, Reason: "application unreachable"}},
		"probe timed out": {func(ctx context.Context) (HealthReport, error) { <-ctx.Done(); return HealthReport{}, ctx.Err() }, HealthReport{Status: HealthUnavailable, Reason: "health probe timed out"}},
		"rejected": {func(context.Context) (HealthReport, error) {
			return HealthReport{}, contracts.New(contracts.ErrUnauthenticated, "x")
		}, HealthReport{Status: HealthUnavailable, Reason: "application refused the adapter's credentials"}},
		"invalid report": {func(context.Context) (HealthReport, error) { return HealthReport{Status: "fine"}, nil }, HealthReport{Status: HealthUnavailable, Reason: "application returned an invalid health report"}},
		"untyped":        {func(context.Context) (HealthReport, error) { return HealthReport{}, errors.New("EOF") }, HealthReport{Status: HealthUnavailable, Reason: "health probe failed"}},
		"panic":          {func(context.Context) (HealthReport, error) { panic("boom") }, HealthReport{Status: HealthUnavailable, Reason: "health probe failed"}},
	}
	for name, tc := range cases {
		a := wrapStub(t, &stubAdapter{health: tc.health}, Options{DefaultTimeout: 30 * time.Millisecond})
		got, err := a.Health(context.Background())
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v, want %+v", name, got, err, tc.want)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := wrapStub(t, &stubAdapter{health: func(ctx context.Context) (HealthReport, error) { return HealthReport{}, ctx.Err() }}, Options{})
	if _, err := a.Health(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled caller: %v", err)
	}
}

func TestWrapValidatesItsArguments(t *testing.T) {
	if _, err := Wrap(nil, Options{ApplicationID: testApp}); err == nil {
		t.Error("Wrap(nil) succeeded")
	}
	for _, id := range []string{"", " ", "a b", "*"} {
		if _, err := Wrap(&stubAdapter{}, Options{ApplicationID: id}); err == nil {
			t.Errorf("Wrap accepted application id %q", id)
		}
	}
	// A default longer than the cap is clamped, not refused.
	if _, err := Wrap(&stubAdapter{}, Options{ApplicationID: testApp, DefaultTimeout: time.Hour, MaxTimeout: time.Second}); err != nil {
		t.Errorf("clamping: %v", err)
	}
}

func TestGuardDescribeIsValidated(t *testing.T) {
	a := wrapStub(t, &stubAdapter{}, Options{})
	d, err := a.Describe(context.Background())
	if err != nil || d.Transport != "stub" {
		t.Fatalf("Describe = %+v, %v", d, err)
	}
	bad := &describeStub{d: Description{Transport: "stub", ContractVersions: []int{0}}}
	g, _ := Wrap(bad, Options{ApplicationID: testApp})
	if _, err := g.Describe(context.Background()); contracts.CodeOf(err) != contracts.ErrSchemaInvalid {
		t.Fatalf("an invalid description passed the guard: %v", err)
	}
}

// TestIdentityFromCarriesOnlyTheMinimalContext: only application, tenant,
// workspace, principal, roles and session go to the host.
//
// SEC-044
func TestIdentityFromCarriesOnlyTheMinimalContext(t *testing.T) {
	sc := testScope()
	id := IdentityFrom(sc)
	if id.ApplicationID != sc.ApplicationID || id.TenantID != sc.TenantID || id.WorkspaceID != sc.WorkspaceID ||
		id.PrincipalID != sc.PrincipalID || id.SessionID != sc.SessionID || len(id.Roles) != 1 || id.Roles[0] != "member" {
		t.Fatalf("identity = %+v", id)
	}
	id.Roles[0] = "admin"
	if sc.Roles[0] != "member" {
		t.Fatal("the identity shares its roles slice with the scope")
	}
	b, err := json.Marshal(IdentityFrom(sc))
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]any
	_ = json.Unmarshal(b, &members)
	for _, k := range []string{"deployment_id", "capability_scopes"} {
		if _, ok := members[k]; ok {
			t.Errorf("identity carries %s", k)
		}
	}
	if len(members) != 6 {
		t.Errorf("identity has %d members, want 6: %v", len(members), members)
	}
}
