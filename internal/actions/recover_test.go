package actions

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// dieDuringCommit makes the adapter's Commit stop the calling goroutine, which
// is the closest a test gets to the process being killed: runtime.Goexit is not
// a panic, so the adapter wrapper's recover does not see it, and nothing after
// the adapter call (the Status check, the outcome write) ever runs. With
// applied the host has already applied the commit when the process dies.
func dieDuringCommit(applied bool) func(*scripted) {
	return func(s *scripted) {
		s.onCommit = func(_ adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
			if applied {
				_, _ = next()
			}
			runtime.Goexit()
			return act.DeterministicResult{}, nil
		}
	}
}

// commitAndDie runs Commit in a goroutine that does not come back.
func (e *env) commitAndDie(sc contracts.Scope, prepID string) {
	e.t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.co.Commit(ctxb(), sc, prepID)
		e.t.Error("Commit returned although the process was supposed to die inside it")
	}()
	<-done
}

// A process that dies between confirming and committing leaves nothing half
// done: a committing row is resolved by the host's Status and never by a blind
// commit, a row that was only confirmed can still be committed, and an outcome
// Status cannot decide is uncertain and says so (SEC-046).
//
// SEC-046
func TestCrashBetweenConfirmAndCommitIsRecoverable(t *testing.T) {
	sc := scopeOf("user-1", "ses-1")

	t.Run("dies after the confirmation, before the commit starts", func(t *testing.T) {
		path := dbFile(t)
		e := newEnv(t, withDBPath(path), withLevel(level2))
		p := e.prepare(sc, "Alpha")
		e.confirm(sc, p)

		e2 := e.restart(path)
		rep, err := e2.co.RecoverAtStartup(ctxb())
		noErr(t, err)
		if rep.Examined != 0 {
			t.Errorf("recovery looked at %d rows; only the confirmation was stored", rep.Examined)
		}
		rc, err := e2.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || e2.app.Effects() != 1 {
			t.Errorf("receipt %+v effects %d", rc, e2.app.Effects())
		}
		// The confirmation was used before the crash and cannot be used again.
		_, err = e2.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p), Token: p.Confirmation})
		wantCode(t, err, contracts.ErrReplayed)
	})

	t.Run("dies after the committing record, before anything was delivered", func(t *testing.T) {
		path := dbFile(t)
		e := newEnv(t, withDBPath(path), withLevel(level2))
		p := e.prepare(sc, "Beta")
		e.confirm(sc, p)
		e.sc.set(dieDuringCommit(false))
		e.commitAndDie(sc, id(p))

		if e.state(id(p)) != act.PreparedActionStateCommitting {
			t.Fatalf("state = %s, want committing (the write-ahead record)", e.state(id(p)))
		}
		if e.eventCount("action.committing") != 1 || e.app.Effects() != 0 || e.hostCalls(adapters.OpCommit) != 0 {
			t.Fatalf("committing events %d effects %d ops %v", e.eventCount("action.committing"), e.app.Effects(), e.hostOps())
		}
		ops := len(e.app.Calls())

		e2 := e.restart(path)
		// A row that is still young may belong to a commit that is in flight: the
		// periodic sweep leaves it alone; the startup sweep does not.
		rep, err := e2.co.Recover(ctxb(), RecoverOptions{})
		noErr(t, err)
		if rep.Examined != 0 {
			t.Errorf("the sweep touched a commit that may still be in flight: %+v", rep)
		}
		rep, err = e2.co.RecoverAtStartup(ctxb())
		noErr(t, err)
		if rep.Examined != 1 || rep.Reopened != 1 || rep.Recorded != 0 || rep.Uncertain != 0 {
			t.Fatalf("report = %+v", rep)
		}
		// Resolved by asking Status, not by committing.
		if got := e2.opsAfter(ops); !cmp.Equal(got, []adapters.Operation{adapters.OpStatus}) {
			t.Errorf("recovery's host operations = %v, want [status]", got)
		}
		if e2.state(id(p)) != act.PreparedActionStateConfirmed || e2.app.Effects() != 0 {
			t.Fatalf("state %s effects %d", e2.state(id(p)), e2.app.Effects())
		}
		// A caller with a live scope sends it, with the key of the first attempt.
		rc, err := e2.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || e2.app.Effects() != 1 {
			t.Fatalf("receipt %+v effects %d", rc, e2.app.Effects())
		}
		var keys []string
		for _, c := range e2.app.Calls() {
			if c.Operation == adapters.OpCommit {
				keys = append(keys, c.IdempotencyKey)
			}
		}
		if len(keys) != 1 || keys[0] != string(p.Action.IdempotencyKey) {
			t.Errorf("commit keys = %v", keys)
		}
		want := []string{"action.prepared", "action.confirmed", "action.committing", "action.confirmed", "action.committing", "action.committed"}
		if !cmp.Equal(e2.events(), want) {
			t.Errorf("events = %v, want %v", e2.events(), want)
		}
	})

	t.Run("dies after the host applied the commit, before the outcome was recorded", func(t *testing.T) {
		path := dbFile(t)
		e := newEnv(t, withDBPath(path), withLevel(level2))
		p := e.prepare(sc, "Gamma")
		e.confirm(sc, p)
		e.sc.set(dieDuringCommit(true))
		e.commitAndDie(sc, id(p))
		if e.app.Effects() != 1 || e.state(id(p)) != act.PreparedActionStateCommitting {
			t.Fatalf("effects %d state %s", e.app.Effects(), e.state(id(p)))
		}

		e2 := e.restart(path)
		rep, err := e2.co.RecoverAtStartup(ctxb())
		noErr(t, err)
		if rep.Examined != 1 || rep.Recorded != 1 {
			t.Fatalf("report = %+v", rep)
		}
		// One commit in all: the one that died. Recovery asked, and did not send.
		if e2.hostCalls(adapters.OpCommit) != 1 || e2.hostCalls(adapters.OpStatus) != 1 || e2.app.Effects() != 1 {
			t.Fatalf("ops %v effects %d", e2.hostOps(), e2.app.Effects())
		}
		rc, err := e2.co.Receipt(ctxb(), sc, id(p))
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || len(rc.Records) != 1 {
			t.Fatalf("receipt = %+v", rc)
		}
		validateAgainst(t, schemaReceipt, rc)
		again, err := e2.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if diff := cmp.Diff(rc, again); diff != "" || e2.hostCalls(adapters.OpCommit) != 1 {
			t.Errorf("commit after recovery: %s, commits %d", diff, e2.hostCalls(adapters.OpCommit))
		}
		if e2.state(id(p)) != act.PreparedActionStateCommitted {
			t.Errorf("state = %s", e2.state(id(p)))
		}
		// A second sweep has nothing to do.
		rep, err = e2.co.RecoverAtStartup(ctxb())
		noErr(t, err)
		if rep.Examined != 0 {
			t.Errorf("second sweep examined %d", rep.Examined)
		}
	})

	t.Run("Status cannot decide: uncertain, the user is told, and it can be reconciled", func(t *testing.T) {
		path := dbFile(t)
		e := newEnv(t, withDBPath(path), withLevel(level1))
		p := e.prepare(sc, "Delta")
		e.sc.set(dieDuringCommit(true))
		e.commitAndDie(sc, id(p))

		e2 := e.restart(path)
		e2.app.Inject(adapters.OpStatus, fake.FaultDeny) // a denial of the Status call says nothing about the commit
		rep, err := e2.co.RecoverAtStartup(ctxb())
		noErr(t, err)
		if rep.Uncertain != 1 || rep.Recorded != 0 || rep.Reopened != 0 {
			t.Fatalf("report = %+v", rep)
		}
		rc, err := e2.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrUncertainOutcome)
		if rc.Status != act.ActionReceiptStatusUncertain || e2.state(id(p)) != act.PreparedActionStateUncertain {
			t.Fatalf("receipt %+v state %s", rc, e2.state(id(p)))
		}
		if e2.hostCalls(adapters.OpCommit) != 1 {
			t.Fatal("recovery committed again")
		}
		got, err := e2.co.Reconcile(ctxb(), sc, id(p))
		noErr(t, err)
		if got.Status != act.ActionReceiptStatusCommitted || e2.app.Effects() != 1 || e2.hostCalls(adapters.OpCommit) != 1 {
			t.Errorf("reconciled %+v effects %d", got, e2.app.Effects())
		}
	})

	t.Run("no adapter at recovery: unknown, not retried", func(t *testing.T) {
		path := dbFile(t)
		e := newEnv(t, withDBPath(path), withLevel(level1))
		p := e.prepare(sc, "Epsilon")
		e.sc.set(dieDuringCommit(false))
		e.commitAndDie(sc, id(p))
		e2 := e.restart(path)
		e2.co.adapters = AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) {
			return nil, contracts.New(contracts.ErrQuarantined, "the application is quarantined")
		})
		rep, err := e2.co.RecoverAtStartup(ctxb())
		noErr(t, err)
		if rep.Uncertain != 1 {
			t.Fatalf("report = %+v", rep)
		}
		if e2.state(id(p)) != act.PreparedActionStateUncertain || e2.hostCalls(adapters.OpCommit) != 0 {
			t.Errorf("state %s ops %v", e2.state(id(p)), e2.hostOps())
		}
	})

	t.Run("an old committing row is resolved by the periodic sweep too", func(t *testing.T) {
		path := dbFile(t)
		e := newEnv(t, withDBPath(path), withLevel(level1))
		p := e.prepare(sc, "Zeta")
		e.sc.set(dieDuringCommit(true))
		e.commitAndDie(sc, id(p))
		rep, err := e.co.Recover(ctxb(), RecoverOptions{})
		noErr(t, err)
		if rep.Examined != 0 {
			t.Fatalf("a fresh row was resolved: %+v", rep)
		}
		e.clk.Advance(DefaultCommitStaleAfter + time.Second)
		rep, err = e.co.Recover(ctxb(), RecoverOptions{})
		noErr(t, err)
		if rep.Recorded != 1 || e.state(id(p)) != act.PreparedActionStateCommitted {
			t.Errorf("report %+v state %s", rep, e.state(id(p)))
		}
	})

	t.Run("dies during prepare: nothing is stored, nothing is applied", func(t *testing.T) {
		path := dbFile(t)
		e := newEnv(t, withDBPath(path), withLevel(level2))
		e.sc.set(func(s *scripted) {
			s.onPrepare = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				_, _ = next()
				runtime.Goexit()
				return act.DeterministicResult{}, nil
			}
		})
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = e.co.Prepare(ctxb(), sc, PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": "Eta"}})
		}()
		<-done
		e2 := e.restart(path)
		if n := e2.count(`SELECT COUNT(*) FROM action_preparations`); n != 0 || e2.app.Effects() != 0 {
			t.Fatalf("preparations %d effects %d", n, e2.app.Effects())
		}
		if rep, err := e2.co.RecoverAtStartup(ctxb()); err != nil || rep.Examined != 0 {
			t.Errorf("report %+v err %v", rep, err)
		}
		p := e2.prepare(sc, "Eta")
		e2.confirm(sc, p)
		_, err := e2.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if e2.app.Effects() != 1 {
			t.Errorf("effects = %d", e2.app.Effects())
		}
	})
}
