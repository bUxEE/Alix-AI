package actions

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Level 0 is read-only and executes after authorisation: no preparation, no
// confirmation, one host read, and every refusal happens before the host is
// asked. SEC-044: the host sees the identity of every read.
func TestLevel0ExecutesAfterAuthorization(t *testing.T) {
	e := newEnv(t)
	sc := scopeOf("user-1", "ses-1")
	e.app.Seed(tenantA, ws1, "item_7", "Seeded")

	out, err := e.co.Read(ctxb(), sc, ReadInput{Session: sc, CapabilityID: capRead, Fields: map[string]any{"item_id": "item_7"}})
	noErr(t, err)
	if out.Result.Status != act.DeterministicResultStatusOk || len(out.Result.Values) != 1 || out.Result.Values[0].Field != "name" {
		t.Fatalf("result = %+v", out.Result)
	}
	if out.Risk != contracts.RiskRead || len(out.VisibleFields) != 1 || out.VisibleFields[0] != "item_id" {
		t.Errorf("risk %v visible %v", out.Risk, out.VisibleFields)
	}
	// One read, with the identity the host reauthorises on; nothing was prepared.
	calls := e.app.Calls()
	if len(calls) != 1 || calls[0].Operation != adapters.OpRead {
		t.Fatalf("host calls = %v", e.hostOps())
	}
	if want := adapters.IdentityFrom(sc); calls[0].Identity.PrincipalID != want.PrincipalID || calls[0].Identity.TenantID != want.TenantID ||
		calls[0].Identity.WorkspaceID != want.WorkspaceID || calls[0].Identity.SessionID != want.SessionID {
		t.Errorf("host saw identity %+v, want %+v", calls[0].Identity, want)
	}
	if n := e.count(`SELECT COUNT(*) FROM action_preparations`); n != 0 {
		t.Errorf("a read stored %d preparations", n)
	}
	if ph := e.recs.phases(); len(ph) != 1 {
		t.Errorf("policy decisions = %v, want exactly one", ph)
	}

	t.Run("authorisation comes first", func(t *testing.T) {
		before := len(e.app.Calls())
		noGrant := scopeOf("user-1", "ses-1")
		noGrant.Roles = []string{"staff"} // declared by the package, granted nothing
		_, err := e.co.Read(ctxb(), noGrant, ReadInput{Session: noGrant, CapabilityID: capRead, Fields: map[string]any{"item_id": "item_7"}})
		wantCode(t, err, contracts.ErrPermissionDenied)

		otherTenant := scopeOf("user-1", "ses-1")
		otherTenant.TenantID = tenantB
		_, err = e.co.Read(ctxb(), otherTenant, ReadInput{Session: sc, CapabilityID: capRead, Fields: map[string]any{"item_id": "item_7"}})
		wantCode(t, err, contracts.ErrScopeViolation) // the session is bound to tenant-a

		_, err = e.co.Read(ctxb(), sc, ReadInput{Session: sc, CapabilityID: capRead, Fields: map[string]any{}})
		wantCode(t, err, contracts.ErrMissingFields)

		_, err = e.co.Read(ctxb(), contracts.Scope{}, ReadInput{CapabilityID: capRead})
		wantCode(t, err, contracts.ErrScopeMissing)
		if len(e.app.Calls()) != before {
			t.Errorf("the host was called although authorisation failed: %v", e.hostOps())
		}
	})

	t.Run("a capability above level 0 is prepared, not read", func(t *testing.T) {
		before := len(e.app.Calls())
		_, err := e.co.Read(ctxb(), sc, ReadInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": "x"}})
		wantCode(t, err, contracts.ErrValidationFailed)

		// A read that the package says needs a confirmation cannot be satisfied here.
		e.setPackage(newPackage(t, level1, func(s *policy.PackageSpec) {
			s.Capabilities[0].Confirmation = policy.ConfirmExplicit
		}))
		_, err = e.co.Read(ctxb(), sc, ReadInput{Session: sc, CapabilityID: capRead, Fields: map[string]any{"item_id": "item_7"}})
		wantCode(t, err, contracts.ErrConfirmationRequired)
		if len(e.app.Calls()) != before {
			t.Errorf("the host was called: %v", e.hostOps())
		}
	})

	t.Run("a host refusal is typed", func(t *testing.T) {
		e.setPackage(newPackage(t, level1))
		out, err := e.co.Read(ctxb(), sc, ReadInput{Session: sc, CapabilityID: capRead, Fields: map[string]any{"item_id": "nope"}})
		wantCode(t, err, contracts.ErrValidationFailed)
		if out.Result.Status != act.DeterministicResultStatusRejected {
			t.Errorf("result = %+v", out.Result)
		}
	})
}

// Level 2 always needs an explicit confirmation: nothing is committed on the
// strength of a proposal, a level or a claim that the user agreed (SEC-043).
//
// SEC-043
func TestLevel2RequiresExplicitConfirmation(t *testing.T) {
	e := newEnv(t, withLevel(level2))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "Epsilon")
	if p.Action.Confirmation.Mode != act.ConfirmationRequirementModeExplicit {
		t.Fatalf("mode = %s", p.Action.Confirmation.Mode)
	}

	// Not confirmed: refused, and the host never hears of it.
	_, err := e.co.Commit(ctxb(), sc, id(p))
	wantCode(t, err, contracts.ErrConfirmationRequired)
	if e.hostCalls(adapters.OpCommit) != 0 || e.hostCalls(adapters.OpValidate) != 0 {
		t.Fatalf("an unconfirmed level 2 action reached the host: %v", e.hostOps())
	}

	// A confirmation without the secret is not one.
	_, err = e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p)})
	wantCode(t, err, contracts.ErrConfirmationRequired)
	wrong, err := ParseConfirmationToken("cfm_" + strings.Repeat("A", 43))
	noErr(t, err)
	_, err = e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p), Token: wrong})
	wantCode(t, err, contracts.ErrPermissionDenied)
	if e.state(id(p)) != act.PreparedActionStatePrepared {
		t.Fatalf("state = %s", e.state(id(p)))
	}

	// The right secret confirms; then, and only then, the commit goes through.
	got, err := e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p), Token: p.Confirmation})
	noErr(t, err)
	if got.State != act.PreparedActionStateConfirmed {
		t.Errorf("state after confirm = %s", got.State)
	}
	if e.hostCalls(adapters.OpCommit) != 0 {
		t.Fatal("confirming reached the host's Commit")
	}
	rc, err := e.co.Commit(ctxb(), sc, id(p))
	noErr(t, err)
	if rc.Status != act.ActionReceiptStatusCommitted || len(rc.Records) != 1 || e.app.Effects() != 1 {
		t.Fatalf("receipt = %+v effects=%d", rc, e.app.Effects())
	}
	if want := []string{"action.prepared", "action.confirmed", "action.committing", "action.committed"}; !cmp.Equal(e.events(), want) {
		t.Errorf("audit events = %v, want %v", e.events(), want)
	}

	t.Run("level 1 commits without a confirmation, and has nothing to confirm", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Zeta")
		_, err := e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p)})
		wantCode(t, err, contracts.ErrValidationFailed)
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || e.app.Effects() != 1 {
			t.Errorf("receipt %+v effects %d", rc, e.app.Effects())
		}
	})

	t.Run("a package that raises a level 1 capability to explicit confirmation is obeyed", func(t *testing.T) {
		e := newEnv(t, withLevel(level{policy.EffectReversible, contracts.RiskReversible, policy.ConfirmExplicit}))
		p := e.prepare(sc, "Eta")
		if p.Action.Confirmation.Mode != act.ConfirmationRequirementModeExplicit {
			t.Fatalf("mode = %s", p.Action.Confirmation.Mode)
		}
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrConfirmationRequired)
	})
}

// Level 3 cannot be completed through AlixAI: neither Confirm nor Commit gets
// anywhere and the application is never asked to commit (SEC-043).
//
// SEC-043
func TestLevel3RequiresStepUp(t *testing.T) {
	e := newEnv(t, withLevel(level3))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "Theta")
	if p.Action.RiskLevel != act.RiskLevel3 || p.Action.Confirmation.Mode != act.ConfirmationRequirementModeStepUp {
		t.Fatalf("risk %d mode %s", p.Action.RiskLevel, p.Action.Confirmation.Mode)
	}
	if !p.Confirmation.IsZero() {
		t.Error("a confirmation secret was issued for an action the AI cannot complete")
	}
	validateAgainst(t, schemaPrepared, p.Action)

	_, err := e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p), Token: p.Confirmation})
	wantCode(t, err, contracts.ErrStepUpRequired)
	tok, _ := ParseConfirmationToken("cfm_" + strings.Repeat("B", 43))
	_, err = e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p), Token: tok})
	wantCode(t, err, contracts.ErrStepUpRequired)
	_, err = e.co.Commit(ctxb(), sc, id(p))
	wantCode(t, err, contracts.ErrStepUpRequired)
	if e.hostCalls(adapters.OpCommit) != 0 || e.hostCalls(adapters.OpValidate) != 0 || e.app.Effects() != 0 {
		t.Fatalf("a level 3 action reached the host: %v", e.hostOps())
	}
	if e.state(id(p)) != act.PreparedActionStatePrepared {
		t.Errorf("state = %s", e.state(id(p)))
	}

	t.Run("a level the host raised after the declaration is enforced too", func(t *testing.T) {
		// The policy computes level 2 from the declaration; the host says 3 for
		// this preparation. The stored requirement still binds at commit.
		e := newEnv(t, withLevel(level2))
		e.sc.set(func(s *scripted) {
			s.onPrepare = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				res, err := next()
				if err == nil && res.Preparation != nil {
					res.Preparation.DeclaredRiskLevel = act.RiskLevel3
				}
				return res, err
			}
		})
		p := e.prepare(sc, "Iota")
		if p.Action.RiskLevel != act.RiskLevel3 {
			t.Fatalf("risk = %d", p.Action.RiskLevel)
		}
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrStepUpRequired)
		if e.hostCalls(adapters.OpCommit) != 0 {
			t.Fatal("reached the host")
		}
	})

	t.Run("the host-side step-up does not unlock the AI path", func(t *testing.T) {
		// Even with the confirmation row forced to confirmed, a step-up
		// preparation is refused: the stored mode is enforced, not the state.
		e.exec(`UPDATE action_preparations SET state = 'confirmed', confirmed_at = 1 WHERE id = ?`, id(p))
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrStepUpRequired)
		if e.hostCalls(adapters.OpCommit) != 0 {
			t.Fatal("reached the host")
		}
	})
}

// A confirmation issued to one principal in one session cannot be used by
// another principal, another session or against another preparation, and a
// refused attempt does not use it up (SEC-040).
//
// SEC-040
func TestConfirmationBoundToPrincipalAndSession(t *testing.T) {
	e := newEnv(t, withLevel(level2))
	owner := scopeOf("user-1", "ses-1")
	p := e.prepare(owner, "Kappa")
	other := e.prepare(owner, "Lambda") // a second preparation with its own secret

	confirm := func(sc contracts.Scope, prep string, tok ConfirmationToken) error {
		_, err := e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: prep, Token: tok})
		return err
	}
	t.Run("another principal", func(t *testing.T) {
		wantCode(t, confirm(scopeOf("user-2", "ses-1"), id(p), p.Confirmation), contracts.ErrScopeViolation)
	})
	t.Run("another session", func(t *testing.T) {
		wantCode(t, confirm(scopeOf("user-1", "ses-2"), id(p), p.Confirmation), contracts.ErrScopeViolation)
	})
	t.Run("another tenant", func(t *testing.T) {
		sc := scopeOf("user-1", "ses-1")
		sc.TenantID = tenantB
		wantCode(t, confirm(sc, id(p), p.Confirmation), contracts.ErrScopeViolation)
	})
	t.Run("another workspace", func(t *testing.T) {
		sc := scopeOf("user-1", "ses-1")
		sc.WorkspaceID = ws2
		wantCode(t, confirm(sc, id(p), p.Confirmation), contracts.ErrScopeViolation)
	})
	t.Run("another preparation's secret", func(t *testing.T) {
		wantCode(t, confirm(owner, id(p), other.Confirmation), contracts.ErrPermissionDenied)
		wantCode(t, confirm(owner, id(other), p.Confirmation), contracts.ErrPermissionDenied)
	})
	t.Run("data versions that changed under the stored action", func(t *testing.T) {
		// The secret is bound to the data versions it was issued for: a stored
		// action whose versions differ does not verify.
		e2 := newEnv(t, withLevel(level2))
		p2 := e2.prepare(owner, "Mu")
		e2.exec(`UPDATE action_preparations SET prepared_json = replace(prepared_json, '"version":"v0"', '"version":"v9"') WHERE id = ?`, id(p2))
		_, err := e2.co.Confirm(ctxb(), owner, ConfirmInput{PreparationID: id(p2), Token: p2.Confirmation})
		wantCode(t, err, contracts.ErrPermissionDenied)
	})

	// None of that used the secrets up or moved anything.
	if e.state(id(p)) != act.PreparedActionStatePrepared || e.state(id(other)) != act.PreparedActionStatePrepared {
		t.Fatalf("a refused confirmation changed state: %s %s", e.state(id(p)), e.state(id(other)))
	}
	if n := e.eventCount("action.confirmed"); n != 0 {
		t.Errorf("%d confirmed events", n)
	}

	// The owner still can; and then nobody else can commit it.
	e.confirm(owner, p)
	_, err := e.co.Commit(ctxb(), scopeOf("user-2", "ses-1"), id(p))
	wantCode(t, err, contracts.ErrScopeViolation)
	_, err = e.co.Commit(ctxb(), scopeOf("user-1", "ses-2"), id(p))
	wantCode(t, err, contracts.ErrScopeViolation)
	if e.hostCalls(adapters.OpCommit) != 0 || e.app.Effects() != 0 {
		t.Fatalf("an impostor reached the host: %v", e.hostOps())
	}
	_, err = e.co.Commit(ctxb(), owner, id(p))
	noErr(t, err)
	if e.app.Effects() != 1 {
		t.Errorf("effects = %d", e.app.Effects())
	}
	// And the session of the preparation is the one that reads it back.
	_, err = e.co.Get(ctxb(), scopeOf("user-2", "ses-1"), id(p))
	wantCode(t, err, contracts.ErrScopeViolation)
}

// A confirmation is single use. The second presentation is replayed, and
// reaches no adapter (SEC-039).
//
// SEC-039
func TestReplayedConfirmationRejected(t *testing.T) {
	e := newEnv(t, withLevel(level2))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "Nu")
	e.confirm(sc, p)

	calls := len(e.app.Calls())
	in := ConfirmInput{PreparationID: id(p), Token: p.Confirmation}
	_, err := e.co.Confirm(ctxb(), sc, in)
	wantCode(t, err, contracts.ErrReplayed)

	rc, err := e.co.Commit(ctxb(), sc, id(p))
	noErr(t, err)
	if rc.Status != act.ActionReceiptStatusCommitted {
		t.Fatal("not committed")
	}
	_, err = e.co.Confirm(ctxb(), sc, in)
	wantCode(t, err, contracts.ErrReplayed)
	// Replaying the confirmation never commits again.
	for range 3 {
		_, err = e.co.Confirm(ctxb(), sc, in)
		wantCode(t, err, contracts.ErrReplayed)
	}
	if got := e.hostCalls(adapters.OpCommit); got != 1 || e.app.Effects() != 1 {
		t.Errorf("commit calls %d effects %d, want 1 and 1", got, e.app.Effects())
	}
	if len(e.app.Calls()) <= calls {
		t.Fatal("the commit never reached the host")
	}
	if n := e.eventCount("action.confirmed"); n != 1 {
		t.Errorf("%d confirmed events, want 1", n)
	}

	t.Run("two presentations at once: one wins", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		p := e.prepare(sc, "Xi")
		in := ConfirmInput{PreparationID: id(p), Token: p.Confirmation}
		errs := make(chan error, 8)
		for range cap(errs) {
			go func() {
				_, err := e.co.Confirm(ctxb(), sc, in)
				errs <- err
			}()
		}
		var ok, replayed int
		for range cap(errs) {
			switch err := <-errs; {
			case err == nil:
				ok++
			case contracts.CodeOf(err) == contracts.ErrReplayed:
				replayed++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}
		if ok != 1 || replayed != cap(errs)-1 {
			t.Errorf("%d confirmed, %d replayed", ok, replayed)
		}
	})
}

// A preparation expires: the earlier of the platform limit and the host's
// promise, from the core's clock. An expired one is never confirmed or
// committed (SEC-042).
//
// SEC-042
func TestExpiredPreparationRejected(t *testing.T) {
	t.Run("expired before confirmation", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		sc := scopeOf("user-1", "ses-1")
		p := e.prepare(sc, "Omicron")
		e.clk.Advance(300*time.Second - time.Millisecond)
		// One millisecond before expiry it is still valid; at expiry it is not.
		if _, err := e.co.Get(ctxb(), sc, id(p)); err != nil {
			t.Fatal(err)
		}
		e.clk.Advance(time.Millisecond)
		_, err := e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p), Token: p.Confirmation})
		wantCode(t, err, contracts.ErrExpired)
		if e.state(id(p)) != act.PreparedActionStateExpired {
			t.Errorf("state = %s, want expired", e.state(id(p)))
		}
		_, err = e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrExpired)
		if got := e.events(); !cmp.Equal(got, []string{"action.prepared", "action.expired"}) {
			t.Errorf("audit events = %v", got)
		}
		if e.hostCalls(adapters.OpCommit) != 0 || e.hostCalls(adapters.OpValidate) != 0 {
			t.Errorf("an expired preparation reached the host: %v", e.hostOps())
		}
	})

	t.Run("expired between confirmation and commit", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		sc := scopeOf("user-1", "ses-1")
		p := e.prepare(sc, "Pi")
		e.confirm(sc, p)
		e.clk.Advance(301 * time.Second)
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrExpired)
		if e.state(id(p)) != act.PreparedActionStateExpired || e.app.Effects() != 0 || e.hostCalls(adapters.OpCommit) != 0 {
			t.Errorf("state %s effects %d ops %v", e.state(id(p)), e.app.Effects(), e.hostOps())
		}
	})

	t.Run("a level 1 action expires too", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		sc := scopeOf("user-1", "ses-1")
		p := e.prepare(sc, "Rho")
		e.clk.Advance(time.Hour)
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrExpired)
		if e.app.Effects() != 0 {
			t.Error("committed after expiry")
		}
	})

	t.Run("the sweep marks lapsed preparations and leaves live ones", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		sc := scopeOf("user-1", "ses-1")
		old := e.prepare(sc, "Sigma")
		e.clk.Advance(299 * time.Second)
		fresh := e.prepare(sc, "Tau")
		e.clk.Advance(2 * time.Second) // old is now past 300 s, fresh is not
		rep, err := e.co.ExpireDue(ctxb())
		noErr(t, err)
		if rep.Expired != 1 || e.state(id(old)) != act.PreparedActionStateExpired || e.state(id(fresh)) != act.PreparedActionStatePrepared {
			t.Errorf("report %+v old=%s fresh=%s", rep, e.state(id(old)), e.state(id(fresh)))
		}
		rep, err = e.co.ExpireDue(ctxb())
		noErr(t, err)
		if rep.Expired != 0 {
			t.Errorf("a second sweep expired %d more", rep.Expired)
		}
		if n := e.eventCount("action.expired"); n != 1 {
			t.Errorf("%d expired events", n)
		}
	})
}

// A transition that read the preparation before a Confirm moved it still ends it,
// and the audit event names the state it really left: the tamper-evident log never
// records a from-state the preparation was no longer in.
func TestTransitionRecordsTheStateItLeft(t *testing.T) {
	e := newEnv(t, withLevel(level2))
	sc := scopeOf("user-1", "ses-1")
	p := e.prepare(sc, "Upsilon")
	stale, err := e.co.load(ctxb(), e.db, sc, id(p))
	noErr(t, err)
	e.confirm(sc, p)
	noErr(t, e.co.endPreparation(ctxb(), sc, stale, act.PreparedActionStateInvalidated, audit.EventActionInvalidated, "goal_changed"))
	if e.state(id(p)) != act.PreparedActionStateInvalidated {
		t.Fatalf("state = %s, want invalidated", e.state(id(p)))
	}
	var from string
	if err := e.db.QueryRowContext(ctxb(), `SELECT json_extract(data, '$.from') FROM audit_events WHERE event_type = 'action.invalidated'`).Scan(&from); err != nil {
		t.Fatal(err)
	}
	if from != string(act.PreparedActionStateConfirmed) {
		t.Errorf("the invalidation records from %q, want confirmed", from)
	}
}
