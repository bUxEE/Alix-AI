package auditsec

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/adapters/fake"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// eventOf is the audit event a state transition must emit.
var eventOf = map[act.PreparedActionState]audit.EventType{
	act.PreparedActionStatePrepared:    audit.EventActionPrepared,
	act.PreparedActionStateConfirmed:   audit.EventActionConfirmed,
	act.PreparedActionStateCommitting:  audit.EventActionCommitting,
	act.PreparedActionStateCommitted:   audit.EventActionCommitted,
	act.PreparedActionStateRejected:    audit.EventActionRejected,
	act.PreparedActionStateExpired:     audit.EventActionExpired,
	act.PreparedActionStateInvalidated: audit.EventActionInvalidated,
	act.PreparedActionStateUncertain:   audit.EventActionUncertain,
}

// trace follows one preparation's stored state through a flow: the state read after each
// operation, and, for a commit, the state the adapter's Commit found when it arrived.
type trace struct {
	r      *actionRig
	sc     contracts.Scope
	p      actions.Prepared
	states []act.PreparedActionState
}

func (r *actionRig) follow(sc contracts.Scope, p actions.Prepared) *trace {
	tr := &trace{r: r, sc: sc, p: p}
	tr.states = []act.PreparedActionState{r.state(sc, p)}
	return tr
}

// after records the state once an operation that is not a Commit has returned.
func (tr *trace) after() {
	tr.r.t.Helper()
	if now := tr.r.state(tr.sc, tr.p); now != tr.states[len(tr.states)-1] {
		tr.states = append(tr.states, now)
	}
}

// afterCommit records the states a Commit went through: the one the adapter saw when it was
// called, if it was called, and the one that is stored once it returned.
func (tr *trace) afterCommit(callsBefore int) {
	tr.r.t.Helper()
	calls := tr.r.commitCalls()
	for _, c := range calls[callsBefore:] {
		tr.states = append(tr.states, c.state)
	}
	tr.states = append(tr.states, tr.r.state(tr.sc, tr.p))
}

// derivedEvents is what the audit log must hold for the states the preparation went through: one
// event per state it entered, in order.
func (tr *trace) derivedEvents() []audit.EventType {
	var out []audit.EventType
	for _, s := range tr.states {
		out = append(out, eventOf[s])
	}
	return out
}

type flow struct {
	name  string
	level level
	// run drives the flow and returns the preparation's trace and the events the audit log must
	// hold for it, written out by hand.
	run func(t *testing.T, r *actionRig) (*trace, []audit.EventType)
}

func evs(t ...audit.EventType) []audit.EventType { return t }

var flows = []flow{
	{"level 2: prepare, confirm, commit", level2, func(t *testing.T, r *actionRig) (*trace, []audit.EventType) {
		p := r.mustPrepare(member, "Alpha")
		tr := r.follow(member, p)
		r.mustConfirm(member, p)
		tr.after()
		before := len(r.commitCalls())
		rc, err := r.commit(member, p)
		if err != nil || rc.Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("Commit = %+v, %v", rc, err)
		}
		tr.afterCommit(before)
		return tr, evs(audit.EventActionPrepared, audit.EventActionConfirmed, audit.EventActionCommitting, audit.EventActionCommitted)
	}},
	{"level 1: prepare and commit, no confirmation", level1, func(t *testing.T, r *actionRig) (*trace, []audit.EventType) {
		p := r.mustPrepare(member, "Beta")
		tr := r.follow(member, p)
		before := len(r.commitCalls())
		if rc, err := r.commit(member, p); err != nil || rc.Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("Commit = %+v, %v", rc, err)
		}
		tr.afterCommit(before)
		return tr, evs(audit.EventActionPrepared, audit.EventActionCommitting, audit.EventActionCommitted)
	}},
	{"the user declines a prepared action", level2, func(t *testing.T, r *actionRig) (*trace, []audit.EventType) {
		p := r.mustPrepare(member, "Gamma")
		tr := r.follow(member, p)
		if _, err := r.co.Reject(context.Background(), member, string(p.Action.PreparationID)); err != nil {
			t.Fatal(err)
		}
		tr.after()
		return tr, evs(audit.EventActionPrepared, audit.EventActionRejected)
	}},
	{"the user declines a confirmed action", level2, func(t *testing.T, r *actionRig) (*trace, []audit.EventType) {
		p := r.mustPrepare(member, "Delta")
		tr := r.follow(member, p)
		r.mustConfirm(member, p)
		tr.after()
		if _, err := r.co.Reject(context.Background(), member, string(p.Action.PreparationID)); err != nil {
			t.Fatal(err)
		}
		tr.after()
		return tr, evs(audit.EventActionPrepared, audit.EventActionConfirmed, audit.EventActionRejected)
	}},
	{"a preparation expires and the sweep records it", level2, func(t *testing.T, r *actionRig) (*trace, []audit.EventType) {
		p := r.mustPrepare(member, "Epsilon")
		tr := r.follow(member, p)
		r.clk.Advance(11 * time.Minute)
		if rep, err := r.co.ExpireDue(context.Background()); err != nil || rep.Expired != 1 {
			t.Fatalf("ExpireDue = %+v, %v", rep, err)
		}
		tr.after()
		return tr, evs(audit.EventActionPrepared, audit.EventActionExpired)
	}},
	{"a preparation that expired is found out at the confirmation", level2, func(t *testing.T, r *actionRig) (*trace, []audit.EventType) {
		p := r.mustPrepare(member, "Zeta")
		tr := r.follow(member, p)
		r.clk.Advance(11 * time.Minute)
		wantCode(t, r.confirm(member, p), contracts.ErrExpired)
		tr.after()
		return tr, evs(audit.EventActionPrepared, audit.EventActionExpired)
	}},
	{"a field is corrected: the confirmed preparation is invalidated", level2, func(t *testing.T, r *actionRig) (*trace, []audit.EventType) {
		p := r.mustPrepare(member, "Eta")
		tr := r.follow(member, p)
		r.mustConfirm(member, p)
		tr.after()
		if _, err := r.co.Invalidate(context.Background(), member, string(p.Action.PreparationID), act.PreparedActionInvalidationConditionsItemFieldCorrected); err != nil {
			t.Fatal(err)
		}
		tr.after()
		return tr, evs(audit.EventActionPrepared, audit.EventActionConfirmed, audit.EventActionInvalidated)
	}},
	{"the data moved after the preparation: the commit is invalidated before it is sent", level2, func(t *testing.T, r *actionRig) (*trace, []audit.EventType) {
		p := r.mustPrepare(member, "Theta")
		tr := r.follow(member, p)
		r.mustConfirm(member, p)
		tr.after()
		r.app.Seed(tenantA, workspace, "item_x", "somebody else's item")
		before := len(r.commitCalls())
		_, err := r.commit(member, p)
		wantCode(t, err, contracts.ErrStale)
		tr.afterCommit(before)
		return tr, evs(audit.EventActionPrepared, audit.EventActionConfirmed, audit.EventActionInvalidated)
	}},
	{"the application refuses the commit", level2, func(t *testing.T, r *actionRig) (*trace, []audit.EventType) {
		p := r.mustPrepare(member, "Iota")
		tr := r.follow(member, p)
		r.mustConfirm(member, p)
		tr.after()
		r.app.Inject(adapters.OpCommit, fake.FaultDeny)
		before := len(r.commitCalls())
		rc, err := r.commit(member, p)
		if err == nil || rc.Status != act.ActionReceiptStatusRejected {
			t.Fatalf("Commit = %+v, %v, want a rejected receipt and an error", rc, err)
		}
		tr.afterCommit(before)
		return tr, evs(audit.EventActionPrepared, audit.EventActionConfirmed, audit.EventActionCommitting, audit.EventActionRejected)
	}},
	{"the outcome of the commit is unknown, then reconciled", level1, func(t *testing.T, r *actionRig) (*trace, []audit.EventType) {
		p := r.mustPrepare(member, "Kappa")
		tr := r.follow(member, p)
		r.app.Inject(adapters.OpCommit, fake.FaultDropReply)
		r.app.Inject(adapters.OpStatus, fake.FaultDeny)
		before := len(r.commitCalls())
		rc, err := r.commit(member, p)
		wantCode(t, err, contracts.ErrUncertainOutcome)
		if rc.Status != act.ActionReceiptStatusUncertain {
			t.Fatalf("receipt = %+v", rc)
		}
		tr.afterCommit(before)
		if got, err := r.co.Reconcile(context.Background(), member, string(p.Action.PreparationID)); err != nil || got.Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("Reconcile = %+v, %v", got, err)
		}
		tr.after()
		return tr, evs(audit.EventActionPrepared, audit.EventActionCommitting, audit.EventActionUncertain, audit.EventActionCommitted)
	}},
	{"the commit never reached the application: the preparation reopens and is sent again", level1, func(t *testing.T, r *actionRig) (*trace, []audit.EventType) {
		p := r.mustPrepare(member, "Lambda")
		tr := r.follow(member, p)
		r.app.Inject(adapters.OpCommit, fake.FaultDropRequest)
		before := len(r.commitCalls())
		_, err := r.commit(member, p)
		wantCode(t, err, contracts.ErrUnavailable)
		tr.afterCommit(before)
		before = len(r.commitCalls())
		if rc, err := r.commit(member, p); err != nil || rc.Status != act.ActionReceiptStatusCommitted {
			t.Fatalf("second Commit = %+v, %v", rc, err)
		}
		tr.afterCommit(before)
		return tr, evs(audit.EventActionPrepared, audit.EventActionCommitting, audit.EventActionPrepared, audit.EventActionCommitting, audit.EventActionCommitted)
	}},
}

// checkFlow compares what a flow did with what the audit log holds and returns every discrepancy
// it finds, as sentences. It is a function of the rig, so the same checks can be pointed at a rig
// whose audit appender was sabotaged and be seen to complain.
func checkFlow(r *actionRig, tr *trace, want []audit.EventType) []string {
	var problems []string
	add := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }
	id := string(tr.p.Action.PreparationID)

	got := r.actionEvents(id)
	if !reflect.DeepEqual(got, want) {
		add("the audit chain holds %v for the preparation, want %v", got, want)
	}
	// The same list, derived from the states the preparation was seen in.
	if derived := tr.derivedEvents(); !reflect.DeepEqual(got, derived) {
		add("the preparation went through %v, which calls for events %v, but the chain holds %v", tr.states, derived, got)
	}

	// Every host Commit came after its committing event and before any outcome event of the
	// preparation: the write-ahead order, read from inside the adapter.
	calls := r.commitCalls()
	committing := 0
	for _, e := range got {
		if e == audit.EventActionCommitting {
			committing++
		}
	}
	if len(calls) != committing {
		add("the adapter's Commit was called %d times and %d committing events were recorded: each must come with exactly one", len(calls), committing)
	}
	for i, c := range calls {
		if c.state != act.PreparedActionStateCommitting {
			add("call %d: the stored state when the adapter was called was %q, want committing", i, c.state)
		}
		if !lastActionEventIsCommitting(c.events) {
			add("call %d: the newest action event of the preparation when the adapter was called was not action.committing: %v", i, c.events)
		}
	}

	// Every policy decision is in the chain, once, in the order it was made.
	var made []decision
	for _, rec := range r.recs.all() {
		made = append(made, decision{Phase: rec.Phase, Outcome: rec.Outcome, CapabilityID: rec.CapabilityID, DecisionID: rec.DecisionID})
	}
	if len(made) == 0 {
		add("the policy gateway made no decision: the flow does not exercise it")
	}
	if recorded := r.decisionEvents(); !reflect.DeepEqual(recorded, made) {
		add("the gateway made %v and the audit chain recorded %v", made, recorded)
	}
	return problems
}

// SEC-087: every action state transition emits an audit event, in the order of the transitions,
// and every policy decision emits one; the committing event is appended BEFORE the adapter's
// Commit is called, and an append that fails aborts the step it belongs to. The real action
// coordinator is driven through every path to every state over the real audit log; the audit
// chain is read back from the store, and the stored state is read back after each operation (and
// from inside the adapter's Commit), so the events are compared with what the preparation really
// did and not with what the test says it did.
//
// Backlog gap G10 (docs/security/test-backlog.md section 7).
func TestSecEveryActionTransitionAudited(t *testing.T) {
	t.Run("every transition emits its event", func(t *testing.T) {
		reached := map[act.PreparedActionState]bool{}
		for _, f := range flows {
			t.Run(f.name, func(t *testing.T) {
				r := newActionRig(t, f.level)
				tr, want := f.run(t, r)
				for _, p := range checkFlow(r, tr, want) {
					t.Error(p)
				}
				for _, s := range tr.states {
					reached[s] = true
				}
				if rep, err := r.log.VerifyChain(context.Background(), tenantChain(), audit.VerifyOptions{}); err != nil {
					t.Errorf("the chain does not verify: %v (%+v)", err, rep.Problems)
				}
			})
		}
		// The flows together reach every state of the lifecycle.
		for s := range eventOf {
			if !reached[s] {
				t.Errorf("no flow reaches the state %s", s)
			}
		}
	})

	// The checks above are only worth something if they complain when an audit record is missing
	// or comes late. These run the same flow over a sabotaged appender and require a complaint.
	t.Run("control: the checks see a committing event that is never written", func(t *testing.T) {
		r := newActionRig(t, level2)
		r.gated.sabotage("swallow", audit.EventActionCommitting)
		tr, want := flows[0].run(t, r)
		problems := checkFlow(r, tr, want)
		if len(problems) == 0 {
			t.Fatal("the checks did not notice that no committing event was written")
		}
		t.Logf("the checks complain, as they must: %v", problems[0])
	})
	t.Run("control: the checks see a committing event that is written after the adapter was called", func(t *testing.T) {
		r := newActionRig(t, level2)
		r.gated.sabotage("late", audit.EventActionCommitting)
		tr, want := flows[0].run(t, r)
		if err := r.gated.flush(context.Background()); err != nil {
			t.Fatal(err)
		}
		problems := checkFlow(r, tr, want)
		if len(problems) == 0 {
			t.Fatal("the checks did not notice that the committing event was written after the commit")
		}
		sawOrder := false
		for _, p := range problems {
			if strings.Contains(p, "when the adapter was called") {
				sawOrder = true
			}
		}
		if !sawOrder {
			t.Errorf("the late write was caught, but not by the write-ahead check: %v", problems)
		}
	})

	t.Run("a denial is a decision, and it is recorded", func(t *testing.T) {
		r := newActionRig(t, level2)
		visitor := scopeOf("visitor", "user-2", "ses-2")
		_, err := r.prepare(visitor, "Mu")
		wantCode(t, err, contracts.ErrPermissionDenied)
		decs := r.decisionEvents()
		if len(decs) != 1 || decs[0].Outcome != policy.OutcomeDeny || decs[0].Phase != policy.PhasePrepare {
			t.Fatalf("decisions in the chain = %+v, want one denial at prepare", decs)
		}
		for _, e := range r.chainTypes() {
			if strings.HasPrefix(string(e), "action.") {
				t.Errorf("a denied request left an %s event", e)
			}
		}
		if got := r.hostCalls(adapters.OpPrepare); got != 0 {
			t.Errorf("a denied request reached the application's Prepare %d times", got)
		}
	})

	t.Run("a decision that cannot be recorded is not allowed", func(t *testing.T) {
		r := newActionRig(t, level2)
		r.gated.fail(audit.EventPolicyDecision, true)
		_, err := r.prepare(member, "Nu")
		if err == nil {
			t.Fatal("an action was prepared although the policy decision could not be recorded")
		}
		if got := r.hostCalls(adapters.OpPrepare); got != 0 {
			t.Errorf("the application's Prepare was called %d times without a recorded decision", got)
		}
		for _, e := range r.chainTypes() {
			if strings.HasPrefix(string(e), "action.") {
				t.Errorf("an %s event exists without a recorded decision", e)
			}
		}
		if n := len(r.gated.failed); n == 0 {
			t.Error("the injected failure never fired: the test does not exercise the decision sink")
		}
		// Control: with the log back, the same request goes through.
		r.gated.fail(audit.EventPolicyDecision, false)
		if _, err := r.prepare(member, "Nu"); err != nil {
			t.Errorf("the request still fails once the audit log is back: %v", err)
		}
	})

	t.Run("a failed append of the committing event aborts the commit", func(t *testing.T) {
		for _, lv := range []struct {
			name      string
			level     level
			confirmed bool
			want      act.PreparedActionState
		}{
			{"level 2, confirmed", level2, true, act.PreparedActionStateConfirmed},
			{"level 1, no confirmation", level1, false, act.PreparedActionStatePrepared},
		} {
			t.Run(lv.name, func(t *testing.T) {
				r := newActionRig(t, lv.level)
				p := r.mustPrepare(member, "Xi")
				if lv.confirmed {
					r.mustConfirm(member, p)
				}
				r.gated.fail(audit.EventActionCommitting, true)

				rc, err := r.commit(member, p)
				wantCode(t, err, contracts.ErrUnavailable)
				if rc.ReceiptID != "" {
					t.Errorf("a receipt %s was issued for a commit that was never sent", rc.ReceiptID)
				}
				if len(r.gated.failed) != 1 || r.gated.failed[0] != audit.EventActionCommitting {
					t.Fatalf("the injected failure fired %v, want exactly one committing append: the test does not exercise the write-ahead step", r.gated.failed)
				}
				// Commit is NEVER called: not by the adapter's seam, not as a host operation.
				if n := len(r.commitCalls()); n != 0 {
					t.Errorf("the adapter's Commit was called %d times without its audit record", n)
				}
				if n := r.hostCalls(adapters.OpCommit); n != 0 || r.app.Effects() != 0 {
					t.Errorf("the application received %d commits and applied %d effects", n, r.app.Effects())
				}
				// It ends in a typed non-success state: where it was, not committing, no receipt.
				if got := r.state(member, p); got != lv.want {
					t.Errorf("state = %s, want %s", got, lv.want)
				}
				if _, err := r.co.Receipt(context.Background(), member, string(p.Action.PreparationID)); contracts.CodeOf(err) != contracts.ErrConflict {
					t.Errorf("Receipt = %v, want conflict: there is no outcome", err)
				}
				for _, e := range r.chainTypes() {
					if e == audit.EventActionCommitting || e == audit.EventActionCommitted {
						t.Errorf("an %s event exists although the commit was aborted", e)
					}
				}
				if rep, err := r.co.RecoverAtStartup(context.Background()); err != nil || rep.Examined != 0 {
					t.Errorf("recovery found a stranded commit: %+v, %v", rep, err)
				}

				// Control: once the log is back, the same preparation commits, and the
				// committing event is in the chain when the adapter is called.
				r.gated.fail(audit.EventActionCommitting, false)
				rc, err = r.commit(member, p)
				if err != nil || rc.Status != act.ActionReceiptStatusCommitted {
					t.Fatalf("after the audit log came back, Commit = %+v, %v", rc, err)
				}
				calls := r.commitCalls()
				if len(calls) != 1 || !lastActionEventIsCommitting(calls[0].events) || calls[0].state != act.PreparedActionStateCommitting {
					t.Errorf("when the adapter was called: %+v", calls)
				}
			})
		}
	})

	t.Run("a failed append of any other transition leaves the state where it was", func(t *testing.T) {
		cases := []struct {
			name  string
			event audit.EventType
			run   func(t *testing.T, r *actionRig) (state act.PreparedActionState, p actions.Prepared)
			want  act.PreparedActionState
		}{
			{"confirmed: the confirmation is not used up", audit.EventActionConfirmed, func(t *testing.T, r *actionRig) (act.PreparedActionState, actions.Prepared) {
				p := r.mustPrepare(member, "Omicron")
				r.gated.fail(audit.EventActionConfirmed, true)
				wantCode(t, r.confirm(member, p), contracts.ErrUnavailable)
				if got := r.state(member, p); got != act.PreparedActionStatePrepared {
					t.Errorf("after the failed confirmation the state is %s, want prepared", got)
				}
				r.gated.fail(audit.EventActionConfirmed, false)
				r.mustConfirm(member, p) // the same token still works
				return r.state(member, p), p
			}, act.PreparedActionStateConfirmed},
			{"rejected", audit.EventActionRejected, func(t *testing.T, r *actionRig) (act.PreparedActionState, actions.Prepared) {
				p := r.mustPrepare(member, "Pi")
				r.gated.fail(audit.EventActionRejected, true)
				_, err := r.co.Reject(context.Background(), member, string(p.Action.PreparationID))
				wantCode(t, err, contracts.ErrUnavailable)
				return r.state(member, p), p
			}, act.PreparedActionStatePrepared},
			{"invalidated", audit.EventActionInvalidated, func(t *testing.T, r *actionRig) (act.PreparedActionState, actions.Prepared) {
				p := r.mustPrepare(member, "Rho")
				r.gated.fail(audit.EventActionInvalidated, true)
				_, err := r.co.Invalidate(context.Background(), member, string(p.Action.PreparationID), act.PreparedActionInvalidationConditionsItemFieldCorrected)
				wantCode(t, err, contracts.ErrUnavailable)
				return r.state(member, p), p
			}, act.PreparedActionStatePrepared},
			{"expired", audit.EventActionExpired, func(t *testing.T, r *actionRig) (act.PreparedActionState, actions.Prepared) {
				p := r.mustPrepare(member, "Sigma")
				r.clk.Advance(11 * time.Minute)
				r.gated.fail(audit.EventActionExpired, true)
				if rep, _ := r.co.ExpireDue(context.Background()); rep.Expired != 0 {
					t.Errorf("the sweep reported an expiry it could not record: %+v", rep)
				}
				return r.state(member, p), p
			}, act.PreparedActionStatePrepared},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := newActionRig(t, level2)
				state, p := c.run(t, r)
				if state != c.want {
					t.Errorf("state = %s, want %s", state, c.want)
				}
				if len(r.gated.failed) == 0 {
					t.Error("the injected failure never fired")
				}
				if c.event == audit.EventActionConfirmed {
					return // its event exists once the log is back
				}
				for _, e := range r.actionEvents(string(p.Action.PreparationID)) {
					if e == c.event {
						t.Errorf("an %s event exists although its append failed", e)
					}
				}
			})
		}
	})

	t.Run("a failed append of the committed event: nothing is announced, and recovery finishes it without a second commit", func(t *testing.T) {
		r := newActionRig(t, level1)
		p := r.mustPrepare(member, "Tau")
		r.gated.fail(audit.EventActionCommitted, true)
		rc, err := r.commit(member, p)
		wantCode(t, err, contracts.ErrUncertainOutcome)
		if rc.ReceiptID != "" {
			t.Error("a receipt was announced that was not persisted")
		}
		if n := len(r.commitCalls()); n != 1 || r.app.Effects() != 1 {
			t.Fatalf("commits = %d, effects = %d, want one of each: the commit was sent once", n, r.app.Effects())
		}
		if got := r.state(member, p); got != act.PreparedActionStateCommitting {
			t.Errorf("state = %s, want committing (left for recovery)", got)
		}
		r.gated.fail(audit.EventActionCommitted, false)
		rep, err := r.co.RecoverAtStartup(context.Background())
		if err != nil || rep.Recorded != 1 {
			t.Fatalf("recovery = %+v, %v", rep, err)
		}
		if n := len(r.commitCalls()); n != 1 || r.app.Effects() != 1 {
			t.Errorf("recovery committed again: commits = %d, effects = %d", n, r.app.Effects())
		}
		want := evs(audit.EventActionPrepared, audit.EventActionCommitting, audit.EventActionCommitted)
		if got := r.actionEvents(string(p.Action.PreparationID)); !reflect.DeepEqual(got, want) {
			t.Errorf("events = %v, want %v", got, want)
		}
	})
}

// tenantChain is the tenant chain of the action rig.
func tenantChain() audit.ChainKey { return audit.ChainKey{ApplicationID: appID, TenantID: tenantA} }

// lastActionEventIsCommitting reports whether the newest action.* event in a chain snapshot taken
// inside the adapter's Commit was action.committing. The rig holds one preparation at a time, so
// the newest action event is the preparation's own.
func lastActionEventIsCommitting(types []audit.EventType) bool {
	for i := len(types) - 1; i >= 0; i-- {
		if strings.HasPrefix(string(types[i]), "action.") {
			return types[i] == audit.EventActionCommitting
		}
	}
	return false
}
