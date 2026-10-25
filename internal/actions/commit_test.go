package actions

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// opsAfter returns the host operations after the first n calls.
func (e *env) opsAfter(n int) []adapters.Operation {
	ops := e.hostOps()
	if n > len(ops) {
		return nil
	}
	return ops[n:]
}

// A prepared action whose data changed is not committed: the host is asked to
// validate again, the version it reports must be the one stored at prepare, and
// a host that finds the change itself at commit is believed (SEC-042).
//
// SEC-042
func TestStaleDataVersionInvalidates(t *testing.T) {
	t.Run("the data changed before the commit", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		sc := scopeOf("user-1", "ses-1")
		p := e.prepare(sc, "Alpha")
		e.confirm(sc, p)
		e.app.Seed(tenantA, ws1, "item_x", "Someone else's item") // bumps the collection's version

		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrStale)
		if e.state(id(p)) != act.PreparedActionStateInvalidated {
			t.Errorf("state = %s, want invalidated", e.state(id(p)))
		}
		if e.hostCalls(adapters.OpCommit) != 0 || e.app.Effects() != 0 {
			t.Fatalf("a stale action was committed: %v", e.hostOps())
		}
		if want := []string{"action.prepared", "action.confirmed", "action.invalidated"}; !cmp.Equal(e.events(), want) {
			t.Errorf("events = %v, want %v", e.events(), want)
		}
		// Terminal: neither another commit nor another confirmation revives it.
		_, err = e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrStale)
		_, err = e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p), Token: p.Confirmation})
		wantCode(t, err, contracts.ErrReplayed)
		if e.hostCalls(adapters.OpCommit) != 0 {
			t.Error("reached the host")
		}
	})

	t.Run("a commit made first makes the other preparation stale", func(t *testing.T) {
		e := newEnv(t)
		sc := scopeOf("user-1", "ses-1")
		first, second := e.prepare(sc, "First"), e.prepare(sc, "Second")
		_, err := e.co.Commit(ctxb(), sc, id(first))
		noErr(t, err)
		_, err = e.co.Commit(ctxb(), sc, id(second))
		wantCode(t, err, contracts.ErrStale)
		if e.app.Effects() != 1 {
			t.Errorf("effects = %d, want 1", e.app.Effects())
		}
	})

	t.Run("the host finds the change at commit", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		sc := scopeOf("user-1", "ses-1")
		p := e.prepare(sc, "Beta")
		// The data moves between the re-check and the commit.
		e.sc.set(func(s *scripted) {
			s.onCommit = func(_ adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				e.app.Seed(tenantA, ws1, "item_y", "Raced")
				return next()
			}
		})
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrStale)
		if rc.Status != act.ActionReceiptStatusRejected || len(rc.Errors) != 1 || rc.Errors[0].Code != "stale" || len(rc.Records) != 0 {
			t.Fatalf("receipt = %+v", rc)
		}
		validateAgainst(t, schemaReceipt, rc)
		if e.state(id(p)) != act.PreparedActionStateInvalidated || e.app.Effects() != 0 {
			t.Errorf("state %s effects %d", e.state(id(p)), e.app.Effects())
		}
		// Idempotent: the same refusal again, nothing sent.
		before := len(e.app.Calls())
		rc2, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrStale)
		if diff := cmp.Diff(rc, rc2); diff != "" || len(e.app.Calls()) != before {
			t.Errorf("second commit differs or sent something: %s", diff)
		}
	})

	t.Run("the host refuses the re-check", func(t *testing.T) {
		e := newEnv(t)
		sc := scopeOf("user-1", "ses-1")
		p := e.prepare(sc, "Gamma")
		e.app.Inject(adapters.OpValidate, fake.FaultDeny)
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrPermissionDenied)
		if e.state(id(p)) != act.PreparedActionStateInvalidated || e.hostCalls(adapters.OpCommit) != 0 {
			t.Errorf("state %s ops %v", e.state(id(p)), e.hostOps())
		}
	})

	t.Run("the session layer invalidates", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		sc := scopeOf("user-1", "ses-1")
		p := e.prepare(sc, "Delta")
		_, err := e.co.Invalidate(ctxb(), sc, id(p), act.PreparedActionInvalidationConditionsItem("not_a_condition"))
		wantCode(t, err, contracts.ErrValidationFailed)
		got, err := e.co.Invalidate(ctxb(), sc, id(p), act.PreparedActionInvalidationConditionsItemFieldCorrected)
		noErr(t, err)
		if got.State != act.PreparedActionStateInvalidated {
			t.Fatalf("state = %s", got.State)
		}
		_, err = e.co.Invalidate(ctxb(), sc, id(p), act.PreparedActionInvalidationConditionsItemFieldCorrected)
		noErr(t, err) // idempotent
		_, err = e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p), Token: p.Confirmation})
		wantCode(t, err, contracts.ErrStale)
		_, err = e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrStale)
		if n := e.eventCount("action.invalidated"); n != 1 {
			t.Errorf("%d invalidated events", n)
		}
	})
}

// Policy decides again at commit, with the preparation's own stored binding,
// the package as it is now and the evidence obtained (SEC-043).
func TestPolicyDecidesAgainAtCommit(t *testing.T) {
	sc := scopeOf("user-1", "ses-1")

	t.Run("both phases are decided and recorded", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		p := e.prepare(sc, "Alpha")
		e.confirm(sc, p)
		_, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if got, want := e.recs.phases(), []policy.Phase{policy.PhasePrepare, policy.PhaseCommit}; !cmp.Equal(got, want) {
			t.Fatalf("policy phases = %v, want %v", got, want)
		}
		rec := e.recs.last()
		if rec.Outcome != policy.OutcomeAllow || rec.Subject.TenantID != tenantA || rec.Subject.WorkspaceID != ws1 ||
			rec.Subject.PrincipalID != "user-1" || rec.Subject.SessionID != "ses-1" {
			t.Errorf("commit decision = %+v", rec)
		}
		var evidence bool
		for _, c := range rec.Checks {
			if c.Name == policy.CheckEvidence && c.Passed {
				evidence = true
			}
		}
		if !evidence {
			t.Error("the commit decision did not weigh the confirmation evidence")
		}
	})

	t.Run("a missing confirmation is a denial, not a default", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		p := e.prepare(sc, "Beta")
		// Force the state past the coordinator's own gate: the gateway says no too.
		e.exec(`UPDATE action_preparations SET confirmation_mode = 'none' WHERE id = ?`, id(p))
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrConfirmationRequired)
		rec := e.recs.last()
		if rec.Phase != policy.PhaseCommit || rec.Outcome != policy.OutcomeDeny || rec.Code != contracts.ErrConfirmationRequired {
			t.Errorf("record = %+v", rec)
		}
		if e.hostCalls(adapters.OpCommit) != 0 {
			t.Error("reached the host")
		}
	})

	t.Run("a permission revoked after prepare voids the preparation", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Gamma")
		readOnly, err := policy.ParseHostPolicy([]byte(`{"policy_id":"read-only","role_grants":[{"role":"member","capabilities":["items.get"]}],"risk_overrides":[],"field_projection":[]}`))
		noErr(t, err)
		e.setPackage(newPackage(t, level1, func(s *policy.PackageSpec) { s.Policies = []*policy.HostPolicy{readOnly} }))
		_, err = e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrPermissionDenied)
		if e.state(id(p)) != act.PreparedActionStateInvalidated || e.hostCalls(adapters.OpCommit) != 0 {
			t.Errorf("state %s ops %v", e.state(id(p)), e.hostOps())
		}
		if n := e.count(`SELECT COUNT(*) FROM audit_events WHERE event_type = 'action.invalidated' AND instr(data, 'permission_changed') > 0`); n != 1 {
			t.Errorf("the invalidation does not name permission_changed")
		}
	})

	t.Run("a token that no longer names the capability is refused and nothing else changes", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Delta")
		narrow := scopeOf("user-1", "ses-1")
		narrow.CapabilityScopes = []string{capRead}
		_, err := e.co.Commit(ctxb(), narrow, id(p))
		wantCode(t, err, contracts.ErrScopeViolation)
		if e.state(id(p)) != act.PreparedActionStatePrepared {
			t.Errorf("state = %s", e.state(id(p)))
		}
		_, err = e.co.Commit(ctxb(), sc, id(p)) // the right token still works
		noErr(t, err)
	})

	t.Run("a quarantined application is not committed to", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Epsilon")
		e.setPackage(newPackage(t, level1).WithStatus(policy.StatusQuarantined))
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrQuarantined)
		if e.state(id(p)) != act.PreparedActionStateInvalidated || e.hostCalls(adapters.OpCommit) != 0 {
			t.Errorf("state %s ops %v", e.state(id(p)), e.hostOps())
		}
	})

	t.Run("a higher level assigned since prepare voids the preparation", func(t *testing.T) {
		// The preparation was level 1 with an explicit confirmation rule; the
		// policy now says level 2. The user confirmed something weaker.
		e := newEnv(t, withLevel(level{policy.EffectReversible, contracts.RiskReversible, policy.ConfirmExplicit}))
		p := e.prepare(sc, "Zeta")
		e.confirm(sc, p)
		e.setPackage(newPackage(t, level2))
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrStale)
		if e.state(id(p)) != act.PreparedActionStateInvalidated || e.hostCalls(adapters.OpCommit) != 0 {
			t.Errorf("state %s ops %v", e.state(id(p)), e.hostOps())
		}
	})

	t.Run("an audit sink that is down denies", func(t *testing.T) {
		// The gateway fails closed when a decision cannot be recorded.
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Eta")
		e.exec(`CREATE TRIGGER no_policy_events BEFORE INSERT ON audit_events WHEN NEW.event_type = 'policy.decision' BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrUnavailable)
		if e.state(id(p)) != act.PreparedActionStatePrepared || e.hostCalls(adapters.OpCommit) != 0 {
			t.Errorf("state %s ops %v", e.state(id(p)), e.hostOps())
		}
	})

	t.Run("a session that changed principal cannot prepare", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		intruder := scopeOf("user-2", "ses-1")
		_, err := e.co.Prepare(ctxb(), intruder, PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": "x"}})
		wantCode(t, err, contracts.ErrScopeViolation)
		_, err = e.co.Prepare(ctxb(), sc, PrepareInput{CapabilityID: capCreate, Fields: map[string]any{"name": "x"}})
		wantCode(t, err, contracts.ErrScopeMissing) // no stored session scope, no default
		if len(e.app.Calls()) != 0 {
			t.Error("the host was called")
		}
	})
}

// Commit is idempotent: the same preparation, the same key, the same receipt,
// and the host is asked once (SEC-041).
//
// SEC-041
func TestIdempotentCommitReturnsSameReceipt(t *testing.T) {
	e := newEnv(t, withLevel(level2))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "Alpha")
	e.confirm(sc, p)

	first, err := e.co.Commit(ctxb(), sc, id(p))
	noErr(t, err)
	if first.Status != act.ActionReceiptStatusCommitted || len(first.Records) != 1 || first.Records[0].EntityType != fake.ItemEntity {
		t.Fatalf("receipt = %+v", first)
	}
	validateAgainst(t, schemaReceipt, first)
	if len(first.Facts) != 1 || first.Facts[0].Field != "name" {
		t.Errorf("facts = %+v", first.Facts)
	}

	again, err := e.co.Commit(ctxb(), sc, id(p))
	noErr(t, err)
	if diff := cmp.Diff(first, again); diff != "" {
		t.Errorf("the second commit returned another receipt (-first +again):\n%s", diff)
	}
	if got := e.hostCalls(adapters.OpCommit); got != 1 || e.app.Effects() != 1 {
		t.Fatalf("commit calls %d, effects %d, want 1 and 1", got, e.app.Effects())
	}
	// The receipt is the one the receipts endpoint returns, and it names its audit record.
	got, err := e.co.Receipt(ctxb(), sc, id(p))
	noErr(t, err)
	if diff := cmp.Diff(first, got); diff != "" {
		t.Errorf("Receipt differs from the commit's (-commit +stored):\n%s", diff)
	}
	if n := e.count(`SELECT COUNT(*) FROM audit_events WHERE id = ? AND event_type = 'action.committed'`, string(first.AuditRef)); n != 1 {
		t.Error("the receipt's audit_ref is not the committed event")
	}

	t.Run("also after a restart", func(t *testing.T) {
		path := dbFile(t)
		e := newEnv(t, withDBPath(path), withLevel(level1))
		p := e.prepare(sc, "Beta")
		first, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		e2 := e.restart(path)
		again, err := e2.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if diff := cmp.Diff(first, again); diff != "" {
			t.Errorf("receipt after restart differs:\n%s", diff)
		}
		if e2.hostCalls(adapters.OpCommit) != 1 || e2.app.Effects() != 1 {
			t.Errorf("effects %d", e2.app.Effects())
		}
	})

	t.Run("a commit already in progress is not sent twice", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Gamma")
		entered, release := make(chan struct{}), make(chan struct{})
		e.sc.set(func(s *scripted) {
			s.onCommit = func(_ adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				close(entered)
				actionsWaitFor(t, release, "the test to release the first commit")
				return next()
			}
		})
		var wg sync.WaitGroup
		var firstReceipt contracts.ActionReceipt
		var firstErr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			firstReceipt, firstErr = e.co.Commit(ctxb(), sc, id(p))
		}()
		actionsWaitFor(t, entered, "the first commit to reach the adapter")
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrConflict)
		close(release)
		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()
		actionsWaitFor(t, finished, "the first commit to finish")
		noErr(t, firstErr)
		third, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if diff := cmp.Diff(firstReceipt, third); diff != "" {
			t.Errorf("receipts differ:\n%s", diff)
		}
		if e.hostCalls(adapters.OpCommit) != 1 || e.app.Effects() != 1 {
			t.Errorf("commit calls %d effects %d", e.hostCalls(adapters.OpCommit), e.app.Effects())
		}
	})

	t.Run("many callers at once: one effect, one receipt", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Delta")
		const n = 8
		type result struct {
			rc  contracts.ActionReceipt
			err error
		}
		out := make(chan result, n)
		for range n {
			go func() {
				rc, err := e.co.Commit(ctxb(), sc, id(p))
				out <- result{rc, err}
			}()
		}
		var receipts []contracts.ActionReceipt
		for range n {
			var r result
			select {
			case r = <-out:
			case <-time.After(actionsWaitLimit):
				t.Fatalf("a commit did not come back within %s", actionsWaitLimit)
			}
			switch {
			case r.err == nil:
				receipts = append(receipts, r.rc)
			case contracts.CodeOf(r.err) == contracts.ErrConflict:
			default:
				t.Errorf("unexpected error: %v", r.err)
			}
		}
		if len(receipts) == 0 {
			t.Fatal("nobody got the receipt")
		}
		for _, rc := range receipts[1:] {
			if diff := cmp.Diff(receipts[0], rc); diff != "" {
				t.Errorf("receipts differ:\n%s", diff)
			}
		}
		if e.hostCalls(adapters.OpCommit) != 1 || e.app.Effects() != 1 {
			t.Errorf("commit calls %d effects %d", e.hostCalls(adapters.OpCommit), e.app.Effects())
		}
	})
}

// An uncertain commit is never retried blindly: the host's Status is asked
// first, and only its no-record marker allows the commit to be sent again. Any
// other answer, including a denial of the Status call, leaves the outcome
// unknown (SEC-041).
//
// SEC-041
func TestUncertainCommitQueriesStatusBeforeRetry(t *testing.T) {
	sc := scopeOf("user-1", "ses-1")

	t.Run("the reply was lost but the host applied it: Status says so", func(t *testing.T) {
		e := newEnv(t)
		p := e.prepare(sc, "Alpha")
		e.app.Inject(adapters.OpCommit, fake.FaultDropReply)
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || len(rc.Records) != 1 {
			t.Fatalf("receipt = %+v", rc)
		}
		if got, want := e.opsAfter(1), []adapters.Operation{adapters.OpValidate, adapters.OpCommit, adapters.OpStatus}; !cmp.Equal(got, want) {
			t.Errorf("host operations = %v, want %v", got, want)
		}
		if e.app.Effects() != 1 {
			t.Errorf("effects = %d", e.app.Effects())
		}
		// The identifiers are the host's, from its recorded reply.
		if want := e.app.Items(tenantA, ws1); len(want) != 1 {
			t.Errorf("items = %v", want)
		}
		again, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if diff := cmp.Diff(rc, again); diff != "" || e.hostCalls(adapters.OpCommit) != 1 {
			t.Errorf("second commit: %s commit calls %d", diff, e.hostCalls(adapters.OpCommit))
		}
	})

	t.Run("a garbled reply to an applied commit is resolved the same way", func(t *testing.T) {
		e := newEnv(t)
		p := e.prepare(sc, "Beta")
		e.app.Inject(adapters.OpCommit, fake.FaultMalformed)
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || e.app.Effects() != 1 || e.hostCalls(adapters.OpCommit) != 1 {
			t.Errorf("receipt %+v effects %d commits %d", rc, e.app.Effects(), e.hostCalls(adapters.OpCommit))
		}
	})

	t.Run("the request was lost: Status shows no record, and only then may it be sent again", func(t *testing.T) {
		e := newEnv(t)
		p := e.prepare(sc, "Gamma")
		e.app.Inject(adapters.OpCommit, fake.FaultDropRequest)
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrUnavailable)
		if rc.ReceiptID != "" {
			t.Error("a receipt was issued for a commit that was never applied")
		}
		if got, want := e.opsAfter(1), []adapters.Operation{adapters.OpValidate, adapters.OpCommit, adapters.OpStatus}; !cmp.Equal(got, want) {
			t.Fatalf("host operations = %v, want %v", got, want)
		}
		if e.app.Effects() != 0 || e.hostCalls(adapters.OpCommit) != 1 {
			t.Fatal("the coordinator retried by itself")
		}
		if e.state(id(p)) != act.PreparedActionStatePrepared {
			t.Fatalf("state = %s, want prepared (where it was)", e.state(id(p)))
		}
		if want := []string{"action.prepared", "action.committing", "action.prepared"}; !cmp.Equal(e.events(), want) {
			t.Errorf("events = %v, want %v", e.events(), want)
		}
		// The caller may send it again; it goes out with the SAME key.
		rc, err = e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || e.app.Effects() != 1 {
			t.Fatalf("receipt %+v effects %d", rc, e.app.Effects())
		}
		reqs := e.sc.commitRequests
		if len(reqs) != 2 || reqs[0].IdempotencyKey != reqs[1].IdempotencyKey || reqs[0].IdempotencyKey != p.Action.IdempotencyKey {
			t.Errorf("the retry used another idempotency key: %+v", reqs)
		}
	})

	t.Run("a denial of the Status call proves nothing: the outcome is uncertain and nothing is retried", func(t *testing.T) {
		e := newEnv(t)
		p := e.prepare(sc, "Delta")
		e.app.Inject(adapters.OpCommit, fake.FaultDropReply) // applied, reply lost
		e.app.Inject(adapters.OpStatus, fake.FaultDeny)      // and Status is refused
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrUncertainOutcome)
		if rc.Status != act.ActionReceiptStatusUncertain || len(rc.Errors) != 1 || rc.Errors[0].Code != "uncertain_outcome" || len(rc.Records) != 0 {
			t.Fatalf("receipt = %+v", rc)
		}
		validateAgainst(t, schemaReceipt, rc)
		if e.state(id(p)) != act.PreparedActionStateUncertain {
			t.Errorf("state = %s", e.state(id(p)))
		}
		if e.hostCalls(adapters.OpCommit) != 1 {
			t.Fatalf("commit calls = %d", e.hostCalls(adapters.OpCommit))
		}
		// The same answer every time, and no host call at all.
		before := len(e.app.Calls())
		again, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrUncertainOutcome)
		if diff := cmp.Diff(rc, again); diff != "" || len(e.app.Calls()) != before {
			t.Errorf("second commit differs (%s) or sent something: %v", diff, e.hostOps())
		}
		if n := e.eventCount("action.uncertain"); n != 1 {
			t.Errorf("%d uncertain events", n)
		}

		// Reconcile asks Status again with a live scope; the host now answers.
		got, err := e.co.Reconcile(ctxb(), sc, id(p))
		noErr(t, err)
		if got.Status != act.ActionReceiptStatusCommitted || got.ReceiptID == rc.ReceiptID || len(got.Records) != 1 {
			t.Fatalf("reconciled receipt = %+v", got)
		}
		validateAgainst(t, schemaReceipt, got)
		cur, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if diff := cmp.Diff(got, cur); diff != "" || e.hostCalls(adapters.OpCommit) != 1 || e.app.Effects() != 1 {
			t.Errorf("after reconciliation: %s commits %d effects %d", diff, e.hostCalls(adapters.OpCommit), e.app.Effects())
		}
		if e.state(id(p)) != act.PreparedActionStateCommitted {
			t.Errorf("state = %s", e.state(id(p)))
		}
		if n := e.count(`SELECT COUNT(*) FROM action_receipts WHERE preparation_id = ?`, id(p)); n != 2 {
			t.Errorf("%d receipts in the ledger, want the uncertain one and the reconciled one", n)
		}
		if _, err := e.co.Reconcile(ctxb(), sc, id(p)); contracts.CodeOf(err) != contracts.ErrConflict {
			t.Errorf("reconciling a committed preparation: %v", err)
		}
	})

	statusAnswers := map[string]func(s *scripted){
		"Status unavailable": func(s *scripted) {
			s.onStatus = func(func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				return act.DeterministicResult{}, contracts.New(contracts.ErrUnavailable, "down")
			}
		},
		"Status says the host cannot tell": func(s *scripted) {
			s.onStatus = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				res, err := next()
				if err == nil {
					res = act.DeterministicResult{
						Operation: res.Operation, CapabilityID: res.CapabilityID, Status: act.DeterministicResultStatusUncertain,
						Errors: []act.Issue{{Code: "uncertain_outcome", Message: "cannot tell"}},
					}
				}
				return res, err
			}
		},
		"Status rejects for another reason than no record": func(s *scripted) {
			s.onStatus = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				res, err := next()
				if err == nil {
					field := "idempotency_key"
					res = act.DeterministicResult{
						Operation: res.Operation, CapabilityID: res.CapabilityID, Status: act.DeterministicResultStatusRejected,
						Errors: []act.Issue{{Code: "validation_failed", Field: (*common.FieldName)(&field), Message: "bad key"}, {Code: "conflict", Message: "and more"}},
					}
				}
				return res, err
			}
		},
	}
	for name, script := range statusAnswers {
		t.Run(name+": uncertain, never retried", func(t *testing.T) {
			e := newEnv(t)
			p := e.prepare(sc, "Epsilon")
			e.sc.set(script)
			e.app.Inject(adapters.OpCommit, fake.FaultDropReply)
			rc, err := e.co.Commit(ctxb(), sc, id(p))
			wantCode(t, err, contracts.ErrUncertainOutcome)
			if rc.Status != act.ActionReceiptStatusUncertain || e.hostCalls(adapters.OpCommit) != 1 || e.app.Effects() != 1 {
				t.Errorf("receipt %s commits %d effects %d", rc.Status, e.hostCalls(adapters.OpCommit), e.app.Effects())
			}
		})
	}

	t.Run("a caller that gives up does not strand a dispatched commit", func(t *testing.T) {
		e := newEnv(t)
		p := e.prepare(sc, "Zeta")
		ctx, cancel := context.WithCancel(context.Background())
		e.sc.set(func(s *scripted) {
			s.onCommit = func(_ adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				_, _ = next() // the host applies it ...
				cancel()      // ... and the client goes away before the reply is read
				return act.DeterministicResult{}, context.Canceled
			}
		})
		rc, err := e.co.Commit(ctx, sc, id(p))
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || e.state(id(p)) != act.PreparedActionStateCommitted {
			t.Errorf("receipt %+v state %s", rc, e.state(id(p)))
		}
		if e.hostCalls(adapters.OpStatus) != 1 || e.hostCalls(adapters.OpCommit) != 1 {
			t.Errorf("ops = %v", e.hostOps())
		}
	})
}

// An unavailable adapter is reported as no commit, in typed form, and leaves the
// session and the prepared state safe (master plan 18).
//
// SEC-047
func TestAdapterUnavailableReportsNoCommit(t *testing.T) {
	sc := scopeOf("user-1", "ses-1")

	t.Run("down at prepare", func(t *testing.T) {
		e := newEnv(t)
		e.app.SetAvailable(false)
		_, err := e.co.Prepare(ctxb(), sc, PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": "x"}})
		wantCode(t, err, contracts.ErrUnavailable)
		if n := e.count(`SELECT COUNT(*) FROM action_preparations`); n != 0 {
			t.Errorf("%d preparations stored", n)
		}
	})

	t.Run("down after the confirmation: no commit, the preparation waits", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		p := e.prepare(sc, "Alpha")
		e.confirm(sc, p)
		e.app.SetAvailable(false)
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrUnavailable)
		if rc.ReceiptID != "" || e.app.Effects() != 0 {
			t.Fatal("a commit was reported or applied while the host was down")
		}
		if e.state(id(p)) != act.PreparedActionStateConfirmed {
			t.Fatalf("state = %s, want confirmed", e.state(id(p)))
		}
		e.app.SetAvailable(true)
		rc, err = e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || e.app.Effects() != 1 {
			t.Errorf("receipt %+v effects %d", rc, e.app.Effects())
		}
	})

	t.Run("down exactly when the commit is sent: nothing delivered, back to where it was", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		p := e.prepare(sc, "Beta")
		e.confirm(sc, p)
		e.sc.set(func(s *scripted) {
			s.onCommit = func(_ adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				e.app.SetAvailable(false)
				return next()
			}
		})
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrUnavailable)
		if e.state(id(p)) != act.PreparedActionStateConfirmed || e.app.Effects() != 0 {
			t.Fatalf("state %s effects %d", e.state(id(p)), e.app.Effects())
		}
		// Refused connections prove nothing was delivered: no Status is needed.
		if e.hostCalls(adapters.OpStatus) != 0 {
			t.Errorf("asked Status although the host was unreachable: %v", e.hostOps())
		}
		if want := []string{"action.prepared", "action.confirmed", "action.committing", "action.confirmed"}; !cmp.Equal(e.events(), want) {
			t.Errorf("events = %v, want %v", e.events(), want)
		}
		e.sc.set(func(s *scripted) { s.onCommit = nil })
		e.app.SetAvailable(true)
		_, err = e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if e.app.Effects() != 1 {
			t.Errorf("effects = %d", e.app.Effects())
		}
	})

	t.Run("a dependency of the host is down: unavailable is not success", func(t *testing.T) {
		e := newEnv(t)
		p := e.prepare(sc, "Gamma")
		e.app.Inject(adapters.OpCommit, fake.FaultDependencyDown)
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		// The host said unavailable and applied nothing; Status confirms there is no record.
		wantCode(t, err, contracts.ErrUnavailable)
		if rc.ReceiptID != "" || e.app.Effects() != 0 {
			t.Fatalf("receipt %+v effects %d", rc, e.app.Effects())
		}
		if got := e.opsAfter(1); !cmp.Equal(got, []adapters.Operation{adapters.OpValidate, adapters.OpCommit, adapters.OpStatus}) {
			t.Errorf("host operations = %v", got)
		}
		_, err = e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
	})

	t.Run("the application no longer reachable at re-check does not invalidate", func(t *testing.T) {
		e := newEnv(t)
		p := e.prepare(sc, "Delta")
		e.app.Inject(adapters.OpValidate, fake.FaultDependencyDown)
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrUnavailable)
		if e.state(id(p)) != act.PreparedActionStatePrepared || e.hostCalls(adapters.OpCommit) != 0 {
			t.Errorf("state %s ops %v", e.state(id(p)), e.hostOps())
		}
	})
}

// The host reauthorises every prepare and commit with the identity the
// coordinator sends, and a refusal is final: the coordinator reports no commit
// and does not retry around it (SEC-044).
//
// SEC-044
func TestHostReauthorizesAtCommit(t *testing.T) {
	sc := scopeOf("user-1", "ses-1")
	e := newEnv(t, withLevel(level2))
	p := e.prepare(sc, "Alpha")
	e.confirm(sc, p)
	e.app.Inject(adapters.OpCommit, fake.FaultDeny)

	rc, err := e.co.Commit(ctxb(), sc, id(p))
	wantCode(t, err, contracts.ErrPermissionDenied)
	if rc.Status != act.ActionReceiptStatusRejected || len(rc.Errors) != 1 || rc.Errors[0].Code != "permission_denied" || len(rc.Records) != 0 {
		t.Fatalf("receipt = %+v", rc)
	}
	validateAgainst(t, schemaReceipt, rc)
	if e.app.Effects() != 0 || e.state(id(p)) != act.PreparedActionStateRejected {
		t.Errorf("effects %d state %s", e.app.Effects(), e.state(id(p)))
	}
	// One commit, no Status, no second try, however often it is asked.
	for range 3 {
		again, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrPermissionDenied)
		if diff := cmp.Diff(rc, again); diff != "" {
			t.Errorf("receipt changed:\n%s", diff)
		}
	}
	if e.hostCalls(adapters.OpCommit) != 1 || e.hostCalls(adapters.OpStatus) != 0 {
		t.Errorf("the coordinator retried around the denial: %v", e.hostOps())
	}

	// The host saw the verified identity on every operation that matters.
	want := adapters.IdentityFrom(sc)
	seen := map[adapters.Operation]bool{}
	for _, c := range e.app.Calls() {
		seen[c.Operation] = true
		if c.Identity.ApplicationID != want.ApplicationID || c.Identity.TenantID != want.TenantID || c.Identity.WorkspaceID != want.WorkspaceID ||
			c.Identity.PrincipalID != want.PrincipalID || c.Identity.SessionID != want.SessionID || !cmp.Equal(c.Identity.Roles, want.Roles) {
			t.Errorf("%s: the host saw identity %+v, want %+v", c.Operation, c.Identity, want)
		}
	}
	for _, op := range []adapters.Operation{adapters.OpPrepare, adapters.OpValidate, adapters.OpCommit} {
		if !seen[op] {
			t.Errorf("no %s call reached the host", op)
		}
	}

	t.Run("a denial at prepare is final too", func(t *testing.T) {
		e := newEnv(t)
		e.app.Inject(adapters.OpPrepare, fake.FaultDeny)
		_, err := e.co.Prepare(ctxb(), sc, PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": "x"}})
		wantCode(t, err, contracts.ErrPermissionDenied)
		if n := e.count(`SELECT COUNT(*) FROM action_preparations`); n != 0 {
			t.Errorf("%d preparations stored", n)
		}
	})

	t.Run("a denial does not cost a retry later", func(t *testing.T) {
		// The preparation is rejected and stays rejected: a user who is allowed
		// again prepares a new action with a new key.
		_, err := e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p), Token: p.Confirmation})
		wantCode(t, err, contracts.ErrReplayed)
		q := e.prepare(sc, "Alpha again")
		if q.Action.IdempotencyKey == p.Action.IdempotencyKey {
			t.Error("a new preparation reused the idempotency key")
		}
	})
}

// A canceled or timed-out context before anything was sent is a plain error.
func TestCanceledBeforeDispatchSendsNothing(t *testing.T) {
	e := newEnv(t)
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "Alpha")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.co.Commit(ctx, sc, id(p))
	wantCode(t, err, contracts.ErrUnavailable)
	ctx, cancel = context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	// Wait for the deadline to have fired, not for a guessed time to pass: the timer runs on its own
	// goroutine, and under load a fixed sleep could end before it (the flake of M1-22). The wait is
	// bounded, so a timer that never fires fails the test instead of hanging it.
	select {
	case <-ctx.Done():
	case <-time.After(actionsWaitLimit):
		t.Fatalf("a context with a 1 ns deadline did not expire within %s", actionsWaitLimit)
	}
	_, err = e.co.Commit(ctx, sc, id(p))
	wantCode(t, err, contracts.ErrTimeout)
	if e.hostCalls(adapters.OpCommit) != 0 {
		t.Error("sent a commit with a dead context")
	}
}

// actionsWaitLimit is the ceiling of every wait on another goroutine in these tests. It is not a pace
// (the events arrive in milliseconds); it is far above what load adds, and a wait that never ends fails
// the test instead of hanging the package.
const actionsWaitLimit = 30 * time.Second

// actionsWaitFor blocks until ch is closed or receives, at most actionsWaitLimit.
func actionsWaitFor(t testing.TB, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(actionsWaitLimit):
		t.Fatalf("timed out after %s waiting for %s", actionsWaitLimit, what)
	}
}
