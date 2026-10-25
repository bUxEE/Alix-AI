package actionssec

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// SEC-046
//
// The process dies, or the power fails, during Prepare or at any boundary of
// Commit. On the next boot nothing is half done and nothing is done twice: a
// "committing" row is resolved by asking the host's Status and never by sending
// the commit again; an outcome Status cannot decide is uncertain and says so; a
// preparation that was interrupted before the commit stays uncommitted.
func TestSecKillDuringPrepareAndCommit(t *testing.T) {
	t.Run("the process dies inside Prepare: nothing is stored and nothing is committed", func(t *testing.T) {
		for _, applied := range []bool{false, true} {
			name := "the host never saw the Prepare"
			if applied {
				name = "the host staged the Prepare, then the process died"
			}
			t.Run(name, func(t *testing.T) {
				w := newWorld(t)
				p1 := w.boot(w.path)
				p1.host.diePrepare(applied)
				sc, c := scope(), ctx(t)
				p1.runUntilKilled(func() {
					_, _ = p1.co.Prepare(c, sc, actions.PrepareInput{Session: sc, CapabilityID: capCreate, Fields: map[string]any{"name": "Eta"}})
				})
				if applied && len(w.app.Calls()) == 0 {
					t.Fatal("the host received nothing although it was meant to have staged the preparation")
				}
				p1.stop()

				p2 := w.boot(w.path)
				if n := p2.count(`SELECT COUNT(*) FROM action_preparations`); n != 0 {
					t.Fatalf("an interrupted Prepare left %d preparations behind", n)
				}
				if w.app.Effects() != 0 || len(w.hostCommits()) != 0 {
					t.Fatalf("effects %d, host commits %s: nothing may be committed by a Prepare", w.app.Effects(), describe(w.hostCommits()))
				}
				rep, err := p2.co.RecoverAtStartup(ctx(t))
				noErr(t, err)
				if rep.Examined != 0 {
					t.Fatalf("recovery found %d rows after an interrupted Prepare", rep.Examined)
				}
				// Control: the installation is not simply stuck. A fresh action goes
				// through the whole lifecycle and is committed exactly once.
				pr := p2.prepare("Eta")
				p2.mustConfirm(pr)
				rc, err := p2.commit(pr)
				noErr(t, err)
				if rc.Status != act.ActionReceiptStatusCommitted || w.app.Effects() != 1 || w.hostCommits()[keyOf(pr)] != 1 {
					t.Fatalf("receipt %s effects %d host commits %s", rc.Status, w.app.Effects(), describe(w.hostCommits()))
				}
			})
		}
	})

	t.Run("the process dies after Prepare was stored: not confirmable and not committable after expiry", func(t *testing.T) {
		w := newWorld(t)
		p1 := w.boot(w.path)
		control := p1.prepare("Control")
		afterExpiry := p1.prepare("AfterExpiry")
		swept := p1.prepare("Swept")
		confirmed := p1.prepare("ConfirmedThenExpired")
		p1.mustConfirm(confirmed)
		p1.stop()

		p2 := w.boot(w.path)
		rep, err := p2.co.RecoverAtStartup(ctx(t))
		noErr(t, err)
		if rep.Examined != 0 {
			t.Fatalf("recovery found %d committing rows; nothing had been sent", rep.Examined)
		}
		for _, pr := range []actions.Prepared{control, afterExpiry, swept} {
			if p2.state(pr) != act.PreparedActionStatePrepared {
				t.Fatalf("a stored preparation is %s after the restart, want prepared", p2.state(pr))
			}
		}
		if p2.state(confirmed) != act.PreparedActionStateConfirmed {
			t.Fatalf("a confirmed preparation is %s after the restart, want confirmed", p2.state(confirmed))
		}

		// Control: before it expires the very same kind of preparation is
		// confirmable and committable, once, so what blocks the others below is
		// the expiry and nothing else.
		p2.mustConfirm(control)
		rc, err := p2.commit(control)
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || w.app.Effects() != 1 {
			t.Fatalf("control: receipt %s effects %d", rc.Status, w.app.Effects())
		}
		p2.stop()

		// Past the expiry the coordinator stamped (read from the preparation,
		// not assumed), in yet another process.
		w.clk.Set(expiry(t, afterExpiry).Add(time.Second))
		p3 := w.boot(w.path)
		wantCode(t, p3.confirm(afterExpiry), contracts.ErrExpired)
		_, err = p3.commit(afterExpiry)
		wantCode(t, err, contracts.ErrExpired)
		_, err = p3.commit(confirmed)
		wantCode(t, err, contracts.ErrExpired)
		if _, err := p3.co.ExpireDue(ctx(t)); err != nil {
			t.Fatal(err)
		}
		for name, pr := range map[string]actions.Prepared{"afterExpiry": afterExpiry, "swept": swept, "confirmed": confirmed} {
			if got := p3.state(pr); got != act.PreparedActionStateExpired {
				t.Errorf("%s: state = %s, want expired", name, got)
			}
			if n := w.hostCommits()[keyOf(pr)]; n != 0 {
				t.Errorf("%s: an expired preparation reached the host %d times", name, n)
			}
		}
		if w.app.Effects() != 1 {
			t.Fatalf("effects = %d, want only the control's", w.app.Effects())
		}
	})

	t.Run("the process dies after the confirmation, before the commit starts", func(t *testing.T) {
		w := newWorld(t)
		p1 := w.boot(w.path)
		pr := p1.prepare("Confirmed")
		p1.mustConfirm(pr)
		p1.stop()

		p2 := w.boot(w.path)
		rep, err := p2.co.RecoverAtStartup(ctx(t))
		noErr(t, err)
		if rep.Examined != 0 {
			t.Fatalf("recovery examined %d rows; only a confirmation was stored", rep.Examined)
		}
		// The confirmation was used before the crash: it is single use.
		wantCode(t, p2.confirm(pr), contracts.ErrReplayed)
		rc, err := p2.commit(pr)
		noErr(t, err)
		if rc.Status != act.ActionReceiptStatusCommitted || w.hostCommits()[keyOf(pr)] != 1 || w.app.Effects() != 1 {
			t.Fatalf("receipt %s host commits %s effects %d", rc.Status, describe(w.hostCommits()), w.app.Effects())
		}
		again, err := p2.commit(pr)
		noErr(t, err)
		if diff := cmp.Diff(rc, again); diff != "" || w.hostCommits()[keyOf(pr)] != 1 {
			t.Errorf("a second Commit changed the receipt or reached the host again: %s (host commits %s)", diff, describe(w.hostCommits()))
		}
	})

	// The two boundaries inside Commit, each with a host whose commit is
	// idempotent and one whose commit is not (so a repeated commit shows as a
	// second effect and not only as a second call), each restarted from a clean
	// close and from a crash image of the files as they are on disk.
	for _, host := range []struct {
		name string
		opts []fake.AppOption
	}{
		{"idempotent host", nil},
		{"host whose commit is not idempotent", []fake.AppOption{fake.WithNonIdempotentCommit()}},
	} {
		for _, image := range []bool{false, true} {
			restart := "restart after a clean close"
			if image {
				restart = "restart from a crash image of the files"
			}
			t.Run("the process dies after the committing row was written, before the host answered; "+host.name+"; "+restart, func(t *testing.T) {
				w := newWorld(t, host.opts...)
				dead, pr := crashInCommit(t, w, false)
				p2 := w.restart(dead, image)
				deadKeys := dead.host.commitsDispatched()
				if got := deadKeys[keyOf(pr)]; got != 1 {
					t.Fatalf("the dead process dispatched %d commits for the key, want 1", got)
				}

				// Until recovery has run, the row is in flight: a double submit is
				// refused and dispatches nothing.
				_, err := p2.commit(pr)
				wantCode(t, err, contracts.ErrConflict)
				if len(p2.host.commitsDispatched()) != 0 {
					t.Fatalf("a Commit of a committing row dispatched %s", describe(p2.host.commitsDispatched()))
				}
				// The periodic sweep leaves a commit that may still be in flight alone.
				rep, err := p2.co.Recover(ctx(t), actions.RecoverOptions{})
				noErr(t, err)
				if rep.Examined != 0 {
					t.Fatalf("the periodic sweep resolved a young committing row: %+v", rep)
				}

				// Startup recovery: the host is asked, and only asked.
				calls := len(w.app.Calls())
				rep, err = p2.co.RecoverAtStartup(ctx(t))
				noErr(t, err)
				if rep.Examined != 1 || rep.Reopened != 1 || rep.Recorded != 0 || rep.Uncertain != 0 || rep.Failed != 0 {
					t.Fatalf("recovery report = %+v, want one row reopened", rep)
				}
				if got := w.hostOpsSince(calls); !cmp.Equal(got, []adapters.Operation{adapters.OpStatus}) {
					t.Errorf("recovery's operations at the host = %v, want only [status]", got)
				}
				if len(p2.host.commitsDispatched()) != 0 {
					t.Errorf("recovery dispatched a commit: %s", describe(p2.host.commitsDispatched()))
				}
				if n := maxPerKey(w.hostCommits()); n != 0 || w.app.Effects() != 0 {
					t.Fatalf("the host never received the commit, yet it has %s (effects %d)", describe(w.hostCommits()), w.app.Effects())
				}
				// Not committed, and retryable: the preparation is back where it was
				// and a caller with a live scope can send it.
				if got := p2.state(pr); got != act.PreparedActionStateConfirmed {
					t.Fatalf("state after recovery = %s, want confirmed (retryable)", got)
				}
				rc, err := p2.commit(pr)
				noErr(t, err)
				if rc.Status != act.ActionReceiptStatusCommitted {
					t.Fatalf("the retry gave %s", rc.Status)
				}
				// ...under the SAME idempotency key as the commit that died.
				retried := p2.host.commitsDispatched()
				if len(retried) != 1 || retried[keyOf(pr)] != 1 {
					t.Errorf("the retry used keys %s, want only the original %s", describe(retried), keyOf(pr))
				}
				if got := w.hostCommits(); len(got) != 1 || got[keyOf(pr)] != 1 || w.app.Effects() != 1 {
					t.Errorf("host commits %s effects %d, want exactly one under the original key", describe(got), w.app.Effects())
				}
				if n := countOf(p2.events(), "action.committed"); n != 1 {
					t.Errorf("committed events = %d, want 1 (events %v)", n, p2.events())
				}
			})

			t.Run("the process dies after the host applied the commit, before the receipt was stored; "+host.name+"; "+restart, func(t *testing.T) {
				w := newWorld(t, host.opts...)
				dead, pr := crashInCommit(t, w, true)
				if w.app.Effects() != 1 || w.hostCommits()[keyOf(pr)] != 1 {
					t.Fatalf("effects %d, host commits %s: the host should have applied exactly one commit before the crash",
						w.app.Effects(), describe(w.hostCommits()))
				}
				p2 := w.restart(dead, image)

				_, err := p2.commit(pr)
				wantCode(t, err, contracts.ErrConflict)

				calls := len(w.app.Calls())
				rep, err := p2.co.RecoverAtStartup(ctx(t))
				noErr(t, err)
				if rep.Examined != 1 || rep.Recorded != 1 || rep.Reopened != 0 || rep.Uncertain != 0 || rep.Failed != 0 {
					t.Fatalf("recovery report = %+v, want one commit recorded", rep)
				}
				if got := w.hostOpsSince(calls); !cmp.Equal(got, []adapters.Operation{adapters.OpStatus}) {
					t.Errorf("recovery's operations at the host = %v, want only [status]", got)
				}
				if len(p2.host.commitsDispatched()) != 0 {
					t.Errorf("recovery dispatched a commit: %s", describe(p2.host.commitsDispatched()))
				}
				// ONE commit in all, per key: the one that died. Counting calls and
				// not effects is what catches a repeat on an idempotent host.
				if got := w.hostCommits(); len(got) != 1 || got[keyOf(pr)] != 1 || w.app.Effects() != 1 {
					t.Fatalf("host commits %s effects %d, want one commit under the original key", describe(got), w.app.Effects())
				}

				// The receipt R the host recorded is the receipt the user gets.
				rc, err := p2.co.Receipt(ctx(t), scope(), prepID(pr))
				noErr(t, err)
				items := w.app.Items(tenantA, ws1)
				if rc.Status != act.ActionReceiptStatusCommitted || len(rc.Records) != 1 || len(items) != 1 {
					t.Fatalf("receipt %s records %v, host items %v", rc.Status, rc.Records, items)
				}
				if _, ok := items[string(rc.Records[0].EntityID)]; !ok {
					t.Errorf("the receipt names record %q, the host holds %v", rc.Records[0].EntityID, items)
				}
				if got := p2.state(pr); got != act.PreparedActionStateCommitted {
					t.Errorf("state = %s, want committed", got)
				}
				// Idempotent: Commit again returns the same receipt and sends nothing.
				again, err := p2.commit(pr)
				noErr(t, err)
				if diff := cmp.Diff(rc, again); diff != "" || len(p2.host.commitsDispatched()) != 0 || w.hostCommits()[keyOf(pr)] != 1 {
					t.Errorf("Commit after recovery: receipt diff %q, dispatched %s, host commits %s", diff, describe(p2.host.commitsDispatched()), describe(w.hostCommits()))
				}
				rep, err = p2.co.RecoverAtStartup(ctx(t))
				noErr(t, err)
				if rep.Examined != 0 {
					t.Errorf("a second sweep examined %d rows", rep.Examined)
				}
			})
		}
	}

	t.Run("Status cannot decide: the outcome is uncertain, the user is told, nothing is retried", func(t *testing.T) {
		faults := []fake.Fault{
			fake.FaultDeny, fake.FaultMalformed, fake.FaultTextOnly, fake.FaultGeneric, fake.FaultPartial,
			fake.FaultWrongCapability, fake.FaultWrongKey, fake.FaultDependencyDown, fake.FaultDropReply, fake.FaultDropRequest,
		}
		for _, applied := range []bool{true, false} {
			for _, fault := range faults {
				t.Run(fmt.Sprintf("host applied=%v, Status answers %s", applied, fault), func(t *testing.T) {
					w := newWorld(t)
					dead, pr := crashInCommit(t, w, applied)
					p2 := w.restart(dead, false)
					w.app.Inject(adapters.OpStatus, fault)
					rep, err := p2.co.RecoverAtStartup(ctx(t))
					noErr(t, err)
					if rep.Examined != 1 || rep.Uncertain != 1 || rep.Recorded != 0 || rep.Reopened != 0 {
						t.Fatalf("recovery report = %+v, want one uncertain outcome", rep)
					}
					assertUncertainAndUnretried(t, w, p2, pr, applied)

					// A reconciliation with a Status that works again still does not
					// commit: it adopts what the host recorded, or leaves the action
					// retryable under the same key.
					w.app.ClearFaults()
					got, err := p2.co.Reconcile(ctx(t), scope(), prepID(pr))
					if applied {
						noErr(t, err)
						items := w.app.Items(tenantA, ws1)
						if got.Status != act.ActionReceiptStatusCommitted || len(got.Records) != 1 || len(items) != 1 {
							t.Fatalf("reconciled receipt %s records %v, host items %v", got.Status, got.Records, items)
						}
						if _, ok := items[string(got.Records[0].EntityID)]; !ok {
							t.Errorf("the receipt names record %q, the host holds %v", got.Records[0].EntityID, items)
						}
					} else {
						wantCode(t, err, contracts.ErrUnavailable)
						var ce *contracts.Error
						if !errors.As(err, &ce) || ce.Details["outcome"] != "not_applied" {
							t.Fatalf("reconcile error %v does not say the commit was not applied", err)
						}
						if got := p2.state(pr); got != act.PreparedActionStateConfirmed {
							t.Fatalf("state after reconciliation = %s, want confirmed (retryable)", got)
						}
						rc, err := p2.commit(pr)
						noErr(t, err)
						if rc.Status != act.ActionReceiptStatusCommitted {
							t.Fatalf("the retry gave %s", rc.Status)
						}
						if keys := p2.host.commitsDispatched(); len(keys) != 1 || keys[keyOf(pr)] != 1 {
							t.Errorf("the retry used keys %s, want only the original %s", describe(keys), keyOf(pr))
						}
					}
					if got := w.hostCommits(); len(got) != 1 || got[keyOf(pr)] != 1 || w.app.Effects() != 1 {
						t.Fatalf("host commits %s effects %d, want exactly one under the original key", describe(got), w.app.Effects())
					}
				})
			}
		}
	})

	t.Run("the application cannot be reached at recovery: uncertain, not retried, resolved later", func(t *testing.T) {
		for _, applied := range []bool{true, false} {
			t.Run(fmt.Sprintf("host applied=%v", applied), func(t *testing.T) {
				w := newWorld(t)
				dead, pr := crashInCommit(t, w, applied)
				dead.stop()
				p2 := w.boot(w.path, func(o *bootOptions) {
					o.adapterErr = contracts.New(contracts.ErrQuarantined, "the application is quarantined")
				})
				rep, err := p2.co.RecoverAtStartup(ctx(t))
				noErr(t, err)
				if rep.Examined != 1 || rep.Uncertain != 1 {
					t.Fatalf("recovery report = %+v, want one uncertain outcome", rep)
				}
				if len(p2.host.operations()) != 0 {
					t.Fatalf("recovery reached the host without an adapter: %v", p2.host.operations())
				}
				// The record exists, the user is told it is unknown, and it is not retried.
				p2.stop()
				p3 := w.boot(w.path)
				assertUncertainAndUnretried(t, w, p3, pr, applied)
				if _, err := p3.co.Reconcile(ctx(t), scope(), prepID(pr)); applied {
					noErr(t, err)
				} else {
					wantCode(t, err, contracts.ErrUnavailable)
				}
				if maxPerKey(w.hostCommits()) > 1 {
					t.Fatalf("host commits %s: a key was committed twice", describe(w.hostCommits()))
				}
			})
		}
	})

	t.Run("controls: the repeated-commit detector sees a blind recovery, and recovery is not a no-op", func(t *testing.T) {
		// PLANTED VIOLATION. A recovery that finds the committing row and simply
		// sends the commit again, against a host whose commit is not idempotent.
		w := newWorld(t, fake.WithNonIdempotentCommit())
		dead, pr := crashInCommit(t, w, true)
		if len(dead.host.dispatched) != 1 {
			t.Fatalf("dispatched %d commits before the crash, want 1", len(dead.host.dispatched))
		}
		req := dead.host.dispatched[0]
		dead.stop()
		p2 := w.boot(w.path)
		if _, err := p2.host.Commit(ctx(t), scope(), req); err != nil { // the blind re-commit
			t.Fatalf("the planted blind commit failed: %v", err)
		}
		if got := w.hostCommits()[keyOf(pr)]; got != 2 || maxPerKey(w.hostCommits()) < 2 {
			t.Fatalf("host commits %s: the detector did not see the planted second commit", describe(w.hostCommits()))
		}
		if w.app.Effects() != 2 {
			t.Fatalf("effects = %d, want 2: the host's commit is not idempotent, so the planted violation must show as a second effect", w.app.Effects())
		}
		if got := p2.host.commitsDispatched()[keyOf(pr)]; got != 1 {
			t.Fatalf("the dispatch detector reports %d, want the planted 1", got)
		}

		// RECOVERY IS NOT A NO-OP. With Status reporting a recorded commit the
		// real recovery adopts its receipt, and with Status reporting no record it
		// reopens the preparation; the two differ, and neither commits.
		recorded := newWorld(t)
		deadA, prA := crashInCommit(t, recorded, true)
		pA := recorded.restart(deadA, false)
		repA, err := pA.co.RecoverAtStartup(ctx(t))
		noErr(t, err)
		notRecorded := newWorld(t)
		deadB, prB := crashInCommit(t, notRecorded, false)
		pB := notRecorded.restart(deadB, false)
		repB, err := pB.co.RecoverAtStartup(ctx(t))
		noErr(t, err)
		if repA.Recorded != 1 || repA.Reopened != 0 || repB.Reopened != 1 || repB.Recorded != 0 {
			t.Fatalf("recovery reports %+v and %+v: Status's answer must decide the resolution", repA, repB)
		}
		if pA.state(prA) != act.PreparedActionStateCommitted || pB.state(prB) != act.PreparedActionStateConfirmed {
			t.Fatalf("states %s and %s", pA.state(prA), pB.state(prB))
		}
		if len(pA.host.commitsDispatched())+len(pB.host.commitsDispatched()) != 0 {
			t.Fatal("recovery dispatched a commit")
		}
	})
}

// crashInCommit prepares and confirms an action, then kills the process inside
// Commit: after the "committing" row and its audit event were written, and, when
// applied is true, after the host applied the commit; in both cases before the
// coordinator learned the outcome or wrote the receipt. The store is left as the
// dead process left it (the caller restarts over it).
func crashInCommit(t *testing.T, w *world, applied bool) (*process, actions.Prepared) {
	t.Helper()
	p1 := w.boot(w.path)
	pr := p1.prepare("Doomed")
	p1.mustConfirm(pr)
	p1.host.dieInCommit(applied)
	sc, c := scope(), ctx(t)
	p1.runUntilKilled(func() { _, _ = p1.co.Commit(c, sc, prepID(pr)) })

	// What the dead process left: the write-ahead row and its event, and no outcome.
	if got := p1.state(pr); got != act.PreparedActionStateCommitting {
		t.Fatalf("state after the crash = %s, want committing (the write-ahead record)", got)
	}
	if !p1.hasEvent("action.committing") || p1.hasEvent("action.committed") || p1.hasEvent("action.uncertain") {
		t.Fatalf("audit events after the crash = %v, want committing and no outcome", p1.events())
	}
	if p1.count(`SELECT COUNT(*) FROM action_receipts`) != 0 {
		t.Fatal("a receipt exists although the process died before storing one")
	}
	return p1, pr
}

// restart ends the dead process and boots a new one over the same files: after a
// clean close, or from a copy of the files taken while the dead process still
// had the store open and never closed it.
func (w *world) restart(dead *process, image bool) *process {
	w.t.Helper()
	if image {
		path := dead.crashImage()
		return w.boot(path)
	}
	dead.stop()
	return w.boot(w.path)
}

// assertUncertainAndUnretried checks what the user and the host see after an
// outcome nobody could decide: a typed uncertain result that cannot be read as
// success, stable across asks, no second commit anywhere.
func assertUncertainAndUnretried(t *testing.T, w *world, p *process, pr actions.Prepared, applied bool) {
	t.Helper()
	if got := p.state(pr); got != act.PreparedActionStateUncertain {
		t.Fatalf("state = %s, want uncertain", got)
	}
	// What the user is told, on every ask: an error that is not success, a
	// receipt that says uncertain and claims no record.
	first, err := p.commit(pr)
	wantCode(t, err, contracts.ErrUncertainOutcome)
	if first.Status != act.ActionReceiptStatusUncertain {
		t.Fatalf("receipt status = %s, want uncertain", first.Status)
	}
	if len(first.Records) != 0 {
		t.Fatalf("an uncertain receipt claims records: %v", first.Records)
	}
	again, err := p.commit(pr)
	wantCode(t, err, contracts.ErrUncertainOutcome)
	if diff := cmp.Diff(first, again); diff != "" {
		t.Errorf("the uncertain receipt changed between two asks: %s", diff)
	}
	stored, err := p.co.Receipt(ctx(t), scope(), prepID(pr))
	if err != nil && contracts.CodeOf(err) != contracts.ErrUncertainOutcome {
		t.Fatalf("Receipt: %v", err)
	}
	if stored.Status != act.ActionReceiptStatusUncertain {
		t.Fatalf("the stored receipt says %s, want uncertain", stored.Status)
	}
	got, err := p.co.Get(ctx(t), scope(), prepID(pr))
	noErr(t, err)
	if got.State != act.PreparedActionStateUncertain {
		t.Errorf("the preparation reads %s, want uncertain", got.State)
	}
	if !p.hasEvent("action.uncertain") || p.hasEvent("action.committed") {
		t.Errorf("audit events = %v, want an uncertain event and no committed event", p.events())
	}

	// Nothing was sent again, by recovery or by the user's retries.
	if len(p.host.commitsDispatched()) != 0 {
		t.Errorf("a commit was dispatched for an undecidable outcome: %s", describe(p.host.commitsDispatched()))
	}
	want := 0
	if applied {
		want = 1
	}
	if got := w.hostCommits()[keyOf(pr)]; got != want || maxPerKey(w.hostCommits()) > 1 {
		t.Errorf("host commits %s, want %d for the key and never 2", describe(w.hostCommits()), want)
	}
}

func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}
