package actions

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

func failAppend(e *env, eventType string) {
	e.t.Helper()
	e.exec(`CREATE TRIGGER fail_` + strings.ReplaceAll(eventType, ".", "_") + ` BEFORE INSERT ON audit_events WHEN NEW.event_type = '` +
		eventType + `' BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
}

func allowAppend(e *env, eventType string) {
	e.t.Helper()
	e.exec(`DROP TRIGGER fail_` + strings.ReplaceAll(eventType, ".", "_"))
}

// SEC-087: a consequential commit does not happen without its audit record.
// The committing event is written, with the state change, BEFORE the adapter's
// Commit is called, and if the append fails the commit is aborted.
//
// SEC-087
func TestSecAuditAppendFailureAbortsCommit(t *testing.T) {
	sc := scopeOf("user-1", "ses-1")

	t.Run("the committing record is written before the adapter is called", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Alpha")
		var atCall struct {
			events int
			state  act.PreparedActionState
		}
		e.sc.set(func(s *scripted) {
			s.onCommit = func(_ adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				atCall.events = e.eventCount("action.committing")
				atCall.state = e.state(id(p))
				return next()
			}
		})
		_, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if atCall.events != 1 || atCall.state != act.PreparedActionStateCommitting {
			t.Errorf("when the adapter was called: %d committing events, state %s", atCall.events, atCall.state)
		}
	})

	t.Run("a failed append aborts the commit: the adapter is not called", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		p := e.prepare(sc, "Beta")
		e.confirm(sc, p)
		failAppend(e, "action.committing")
		before := len(e.app.Calls())

		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrUnavailable)
		if got := e.opsAfter(before); contains(got, adapters.OpCommit) {
			t.Fatalf("the commit was sent without its audit record: %v", e.hostOps())
		}
		if e.app.Effects() != 0 || e.hostCalls(adapters.OpCommit) != 0 {
			t.Fatal("the application was changed")
		}
		// The state never became committing, and nothing is stranded.
		if e.state(id(p)) != act.PreparedActionStateConfirmed || e.count(`SELECT COUNT(*) FROM action_receipts`) != 0 {
			t.Errorf("state = %s", e.state(id(p)))
		}
		if rep, err := e.co.RecoverAtStartup(ctxb()); err != nil || rep.Examined != 0 {
			t.Errorf("recovery found work: %+v %v", rep, err)
		}
		// Once the log is back the same preparation commits.
		allowAppend(e, "action.committing")
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || e.app.Effects() != 1 {
			t.Errorf("receipt %+v effects %d", rc, e.app.Effects())
		}
	})

	t.Run("a failed append of the preparation stores nothing", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		failAppend(e, "action.prepared")
		_, err := e.co.Prepare(ctxb(), sc, PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": "x"}})
		wantCode(t, err, contracts.ErrUnavailable)
		if n := e.count(`SELECT COUNT(*) FROM action_preparations`); n != 0 {
			t.Errorf("%d preparations stored without their audit record", n)
		}
	})

	t.Run("a failed append of the confirmation does not use it up", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		p := e.prepare(sc, "Gamma")
		failAppend(e, "action.confirmed")
		_, err := e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p), Token: p.Confirmation})
		wantCode(t, err, contracts.ErrUnavailable)
		if e.state(id(p)) != act.PreparedActionStatePrepared {
			t.Fatalf("state = %s", e.state(id(p)))
		}
		allowAppend(e, "action.confirmed")
		e.confirm(sc, p)
	})

	t.Run("a failed append of the outcome announces nothing and leaves it for recovery", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Delta")
		failAppend(e, "action.committed")
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrUncertainOutcome)
		if rc.ReceiptID != "" {
			t.Fatal("a receipt was announced that was not persisted")
		}
		if e.state(id(p)) != act.PreparedActionStateCommitting || e.count(`SELECT COUNT(*) FROM action_receipts`) != 0 {
			t.Fatalf("state = %s", e.state(id(p)))
		}
		if e.app.Effects() != 1 {
			t.Fatalf("effects = %d", e.app.Effects())
		}
		// Asking again while the row is committing does not send it again.
		_, err = e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrConflict)

		allowAppend(e, "action.committed")
		rep, err := e.co.RecoverAtStartup(ctxb())
		noErr(t, err)
		if rep.Recorded != 1 || e.hostCalls(adapters.OpCommit) != 1 || e.app.Effects() != 1 {
			t.Fatalf("report %+v commits %d effects %d", rep, e.hostCalls(adapters.OpCommit), e.app.Effects())
		}
		got, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		if got.Status != act.ActionReceiptStatusCommitted {
			t.Errorf("receipt = %+v", got)
		}
	})

	t.Run("a failed append of an expiry still refuses", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		p := e.prepare(sc, "Epsilon")
		e.clk.Advance(time10m)
		failAppend(e, "action.expired")
		_, err := e.co.Confirm(ctxb(), sc, ConfirmInput{PreparationID: id(p), Token: p.Confirmation})
		wantCode(t, err, contracts.ErrExpired)
		if e.state(id(p)) != act.PreparedActionStatePrepared {
			t.Errorf("the transition was recorded without its audit event: %s", e.state(id(p)))
		}
		allowAppend(e, "action.expired")
		rep, err := e.co.ExpireDue(ctxb())
		noErr(t, err)
		if rep.Expired != 1 {
			t.Errorf("report = %+v", rep)
		}
	})
}

// Every action state transition emits an audit event, in order, naming the
// preparation, and a receipt names the event that recorded its outcome (SEC-087).
//
// SEC-087
func TestSecEveryActionTransitionAudited(t *testing.T) {
	sc := scopeOf("user-1", "ses-1")
	cases := []struct {
		name string
		run  func(e *env) string
		want []string
	}{
		{"committed", func(e *env) string {
			p := e.prepare(sc, "A")
			e.confirm(sc, p)
			_, err := e.co.Commit(ctxb(), sc, id(p))
			noErr(t, err)
			return id(p)
		}, []string{"action.prepared", "action.confirmed", "action.committing", "action.committed"}},
		{"rejected by the user", func(e *env) string {
			p := e.prepare(sc, "B")
			_, err := e.co.Reject(ctxb(), sc, id(p))
			noErr(t, err)
			return id(p)
		}, []string{"action.prepared", "action.rejected"}},
		{"rejected by the host", func(e *env) string {
			p := e.prepare(sc, "C")
			e.confirm(sc, p)
			e.app.Inject(adapters.OpCommit, fake.FaultDeny)
			_, err := e.co.Commit(ctxb(), sc, id(p))
			wantCode(t, err, contracts.ErrPermissionDenied)
			return id(p)
		}, []string{"action.prepared", "action.confirmed", "action.committing", "action.rejected"}},
		{"expired", func(e *env) string {
			p := e.prepare(sc, "D")
			e.clk.Advance(time10m)
			_, err := e.co.Commit(ctxb(), sc, id(p))
			wantCode(t, err, contracts.ErrExpired)
			return id(p)
		}, []string{"action.prepared", "action.expired"}},
		{"invalidated", func(e *env) string {
			p := e.prepare(sc, "E")
			e.confirm(sc, p)
			e.app.Seed(tenantA, ws1, "item_x", "x")
			_, err := e.co.Commit(ctxb(), sc, id(p))
			wantCode(t, err, contracts.ErrStale)
			return id(p)
		}, []string{"action.prepared", "action.confirmed", "action.invalidated"}},
		{"uncertain", func(e *env) string {
			p := e.prepare(sc, "F")
			e.confirm(sc, p)
			e.app.Inject(adapters.OpCommit, fake.FaultDropReply)
			e.app.Inject(adapters.OpStatus, fake.FaultDeny)
			_, err := e.co.Commit(ctxb(), sc, id(p))
			wantCode(t, err, contracts.ErrUncertainOutcome)
			return id(p)
		}, []string{"action.prepared", "action.confirmed", "action.committing", "action.uncertain"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withLevel(level2))
			prep := tc.run(e)
			if got := e.events(); !cmp.Equal(got, tc.want) {
				t.Fatalf("events = %v, want %v", got, tc.want)
			}
			// Each event names the preparation.
			if n := e.count(`SELECT COUNT(*) FROM audit_events WHERE event_type LIKE 'action.%' AND instr(data, ?) > 0`, prep); n != len(tc.want) {
				t.Errorf("%d of %d events name the preparation", n, len(tc.want))
			}
			// A receipt names the event that recorded its outcome.
			if rc, err := e.co.Receipt(ctxb(), sc, prep); err == nil {
				var typ string
				if err := e.db.QueryRowContext(ctxb(), `SELECT event_type FROM audit_events WHERE id = ?`, string(rc.AuditRef)).Scan(&typ); err != nil {
					t.Fatalf("receipt audit_ref %s does not resolve: %v", rc.AuditRef, err)
				}
				if typ != tc.want[len(tc.want)-1] {
					t.Errorf("audit_ref is a %s event, want %s", typ, tc.want[len(tc.want)-1])
				}
			}
		})
	}

	t.Run("no event holds a field value, an effect or a secret", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		p := e.prepare(sc, "Confidential name 4711")
		e.confirm(sc, p)
		_, err := e.co.Commit(ctxb(), sc, id(p))
		noErr(t, err)
		for _, needle := range []string{"Confidential", "4711", p.Confirmation.Reveal(), "item_"} {
			if n := e.count(`SELECT COUNT(*) FROM audit_events WHERE event_type LIKE 'action.%' AND instr(data, ?) > 0`, needle); n != 0 {
				t.Errorf("%d action events contain %q", n, needle)
			}
		}
	})
}

// SEC-050: a result that does not belong to the action it answers is never a
// decision input. The shape of a reply is checked by the adapter layer; what
// the coordinator adds is the binding of its records and data versions to the
// request it made.
//
// SEC-050
func TestSecForgedResultsNotAccepted(t *testing.T) {
	sc := scopeOf("user-1", "ses-1")
	rec := func(typ, id string) act.RecordRef {
		return act.RecordRef{EntityType: common.Identifier(typ), EntityID: common.Identifier(id)}
	}
	forgeIdentifiers := func(ids ...act.RecordRef) func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
		return func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
			res, err := next()
			if err == nil {
				res.Identifiers = ids
			}
			return res, err
		}
	}

	t.Run("a commit result about a record the action does not target is not the commit's outcome", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Alpha")
		e.sc.set(func(s *scripted) {
			s.onCommit = func(_ adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				return forgeIdentifiers(rec("invoice", "inv_1"))(next)
			}
		})
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		// The forged reply is set aside and the host's Status, which is honest, decides.
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || len(rc.Records) != 1 || rc.Records[0].EntityType != "item" {
			t.Fatalf("receipt = %+v", rc)
		}
		if got := e.opsAfter(1); !cmp.Equal(got, []adapters.Operation{adapters.OpValidate, adapters.OpCommit, adapters.OpStatus}) {
			t.Errorf("host operations = %v: the forged reply was not checked against Status", got)
		}
		if e.hostCalls(adapters.OpCommit) != 1 {
			t.Error("committed again")
		}
	})

	t.Run("a host that forges Commit and Status both is uncertain, never committed", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Beta")
		e.sc.set(func(s *scripted) {
			forge := forgeIdentifiers(rec("invoice", "inv_1"))
			s.onCommit = func(_ adapters.CommitRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				return forge(next)
			}
			s.onStatus = forge
		})
		rc, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrUncertainOutcome)
		if rc.Status != act.ActionReceiptStatusUncertain || len(rc.Records) != 0 {
			t.Fatalf("receipt = %+v", rc)
		}
		if e.state(id(p)) != act.PreparedActionStateUncertain {
			t.Errorf("state = %s", e.state(id(p)))
		}
	})

	t.Run("an update is bound to the record the user confirmed", func(t *testing.T) {
		// The host prepares an update of item_1; a reply that reports another
		// record is not the outcome of this action, one that reports item_1 is.
		prepareUpdate := func(s *scripted) {
			s.onPrepare = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				res, err := next()
				if err == nil && res.Preparation != nil {
					target := common.Identifier("item_1")
					res.Preparation.Effects[0].Operation = act.EffectOperationUpdate
					res.Preparation.Effects[0].EntityID = &target
				}
				return res, err
			}
		}
		// A well-formed ok result that never touches the host: the shape is right,
		// the records are whatever the forger chose.
		forged := func(op act.DeterministicResultOperation, ids []act.RecordRef) act.DeterministicResult {
			return act.DeterministicResult{
				Operation: op, CapabilityID: capCreate, Status: act.DeterministicResultStatusOk,
				Identifiers: ids, Values: []act.Fact{}, Errors: []act.Issue{}, AllowedNextActions: []act.AllowedNextAction{},
				Freshness: &act.Freshness{AsOf: stamp(t0), Version: "v1"},
			}
		}
		for _, tc := range []struct {
			name   string
			ids    []act.RecordRef
			status act.ActionReceiptStatus
		}{
			{"another record", []act.RecordRef{rec("item", "item_999")}, act.ActionReceiptStatusUncertain},
			{"the target and another", []act.RecordRef{rec("item", "item_1"), rec("item", "item_2")}, act.ActionReceiptStatusUncertain},
			{"the target", []act.RecordRef{rec("item", "item_1")}, act.ActionReceiptStatusCommitted},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := newEnv(t, withLevel(level1))
				e.sc.set(prepareUpdate)
				p := e.prepare(sc, "Gamma")
				e.sc.set(func(s *scripted) {
					s.onCommit = func(adapters.CommitRequest, func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
						return forged(act.DeterministicResultOperationCommit, tc.ids), nil
					}
					s.onStatus = func(func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
						return forged(act.DeterministicResultOperationStatus, tc.ids), nil
					}
				})
				rc, _ := e.co.Commit(ctxb(), sc, id(p))
				if rc.Status != tc.status {
					t.Fatalf("receipt status = %s, want %s (%+v)", rc.Status, tc.status, rc)
				}
				if tc.status == act.ActionReceiptStatusUncertain && len(rc.Records) != 0 {
					t.Errorf("an unbound record is in the receipt: %+v", rc.Records)
				}
			})
		}
	})

	t.Run("a Prepare result that gives one record two versions is refused", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		e.sc.set(func(s *scripted) {
			s.onPrepare = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				res, err := next()
				if err == nil && res.Preparation != nil {
					dup := res.Preparation.DataVersions[0]
					dup.Version = "v999"
					res.Preparation.DataVersions = append(res.Preparation.DataVersions, dup)
				}
				return res, err
			}
		})
		_, err := e.co.Prepare(ctxb(), sc, PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": "x"}})
		wantCode(t, err, contracts.ErrSchemaInvalid)
		if n := e.count(`SELECT COUNT(*) FROM action_preparations`); n != 0 {
			t.Errorf("%d preparations stored", n)
		}
	})

	t.Run("a re-check that reports another data version is stale, whatever else it says", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Delta")
		e.sc.set(func(s *scripted) {
			s.onValidate = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				res, err := next()
				if err == nil && res.Freshness != nil {
					res.Freshness.Version = "v999"
				}
				return res, err
			}
		})
		_, err := e.co.Commit(ctxb(), sc, id(p))
		wantCode(t, err, contracts.ErrStale)
		if e.hostCalls(adapters.OpCommit) != 0 {
			t.Error("committed on a forged re-check")
		}
	})

	t.Run("a re-check that reports no freshness cannot confirm the version", func(t *testing.T) {
		e := newEnv(t, withLevel(level1))
		p := e.prepare(sc, "Epsilon")
		e.sc.set(func(s *scripted) {
			s.onValidate = func(next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				res, err := next()
				if err == nil {
					res.Freshness = nil // the wrapper's re-validation refuses an ok result without it
				}
				return res, err
			}
		})
		_, err := e.co.Commit(ctxb(), sc, id(p))
		if err == nil {
			t.Fatal("committed although the version could not be checked")
		}
		if e.hostCalls(adapters.OpCommit) != 0 {
			t.Error("committed on an unverifiable re-check")
		}
	})

	t.Run("text where a typed result is required is never read as an outcome", func(t *testing.T) {
		for _, f := range []fake.Fault{fake.FaultTextOnly, fake.FaultGeneric, fake.FaultPartial, fake.FaultWrongCapability, fake.FaultWrongKey} {
			t.Run(string(f), func(t *testing.T) {
				e := newEnv(t, withLevel(level1))
				p := e.prepare(sc, "Zeta")
				e.app.Inject(adapters.OpCommit, f)
				rc, err := e.co.Commit(ctxb(), sc, id(p))
				// The host applied the effect; the reply is unusable; Status decides.
				noErr(t, err)
				if rc.Status != act.ActionReceiptStatusCommitted || e.app.Effects() != 1 || e.hostCalls(adapters.OpStatus) != 1 || e.hostCalls(adapters.OpCommit) != 1 {
					t.Errorf("receipt %s effects %d ops %v", rc.Status, e.app.Effects(), e.hostOps())
				}
			})
		}
	})
}
