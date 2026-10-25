package dialogue

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
)

// Multi-turn behaviour (master plan 11.3, 22.3): corrections, cancel, restart, topic
// switch and resume, nested goals and relative references. Every transition is decided
// by the engine from the state; the scripted model only ever proposes an intent.

// proposeOnce scripts the next proposal (model call 1) and nothing more.
func (e *env) proposeOnce(capability, intent string, fields map[string]any, refs ...map[string]any) {
	e.t.Helper()
	e.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(proposalJSON(capability, intent, fields, refs...)).Times(1)
}

// proposals counts the model calls that were a task proposal.
func (e *env) proposals() int {
	n := 0
	for _, p := range e.prompts() {
		if p.Chat.Output.Kind() == providers.ConstraintJSONSchema {
			n++
		}
	}
	return n
}

func (e *env) goal(sc contracts.Scope) *sessions.Goal {
	e.t.Helper()
	return e.session(sc).State.Goal
}

// noCommits asserts nothing was committed or applied (a preparation may have been staged).
func (e *env) noCommits() {
	e.t.Helper()
	if n := e.hostCalls(adapters.OpCommit); n != 0 {
		e.t.Fatalf("the host saw %d Commit calls, want 0", n)
	}
	if n := e.app.Effects(); n != 0 {
		e.t.Fatalf("the host applied %d effects, want 0", n)
	}
}

func hasTransition(res Result, name string) bool {
	for _, n := range res.Trace.Transitions {
		if n == name {
			return true
		}
	}
	return false
}

func slotNumber(t *testing.T, s sessions.Slot) string {
	t.Helper()
	n, ok := s.Value.(json.Number)
	if !ok {
		t.Fatalf("slot value %#v is not a number", s.Value)
	}
	return n.String()
}

// ---- corrections ----

func TestCorrectionUpdatesSlot(t *testing.T) {
	e := newEnv(t, withLevel(level2))
	sc := memberScope()
	first := e.prepareCreate(sc)
	before := e.goal(sc)

	e.clk.Advance(time.Minute)
	e.proposeOnce(capCreate, "correction", map[string]any{"quantity": 5})
	res := e.say(sc, "no, make the quantity 5 instead")

	wantAct(t, res, "request_confirmation")
	g := e.goal(sc)
	q := g.Slots["quantity"]
	if slotNumber(t, q) != "5" || q.Origin != sessions.OriginCorrection || q.Type != "integer" || q.TurnID != res.TurnID {
		t.Fatalf("quantity slot = %+v, want 5 recorded as a correction by this turn", q)
	}
	// Only the right slot changed: the name is exactly what it was.
	n := g.Slots["name"]
	if n.Value != "widget" || n.Origin != sessions.OriginUserTurn || n.TurnID != before.Slots["name"].TurnID || n.TurnID == res.TurnID {
		t.Fatalf("name slot = %+v, want it untouched", n)
	}
	if g.ID != before.ID || g.CapabilityID != capCreate {
		t.Fatalf("the correction started another goal: %+v", g)
	}
	// What the host is asked to prepare carries the corrected value and the kept one.
	if got := e.sc.prepared[len(e.sc.prepared)-1]; got["name"] != "widget" || got["quantity"] != json.Number("5") {
		t.Fatalf("prepared with %v", got)
	}
	if res.Prepared == nil || res.Prepared.PreparationID == first.Prepared.PreparationID || g.PreparedID != string(res.Prepared.PreparationID) {
		t.Fatalf("the corrected goal does not reference the new preparation: %+v", g)
	}
	if !hasTransition(res, TransitionCorrected) {
		t.Fatalf("transitions = %v", res.Trace.Transitions)
	}

	t.Run("a value that replaces another is a correction whatever intent the model gave", func(t *testing.T) {
		e.proposeOnce(capCreate, "task", map[string]any{"quantity": 7})
		out := e.say(sc, "quantity 7")
		wantAct(t, out, "request_confirmation")
		if s := e.goal(sc).Slots["quantity"]; slotNumber(t, s) != "7" || s.Origin != sessions.OriginCorrection {
			t.Fatalf("slot = %+v", s)
		}
	})

	t.Run("the same value is not a correction", func(t *testing.T) {
		e.proposeOnce(capCreate, "task", map[string]any{"quantity": 7})
		out := e.say(sc, "quantity 7")
		if hasTransition(out, TransitionCorrected) {
			t.Fatalf("transitions = %v", out.Trace.Transitions)
		}
		if s := e.goal(sc).Slots["quantity"]; s.Origin == sessions.OriginCorrection && s.TurnID == out.TurnID {
			t.Fatalf("an unchanged value was recorded as a correction: %+v", s)
		}
	})

	t.Run("a correction cue reads a task proposal as a correction", func(t *testing.T) {
		e.proposeOnce(capCreate, "task", map[string]any{"name": "gadget"})
		out := e.say(sc, "actually the name is gadget")
		wantAct(t, out, "request_confirmation")
		if s := e.goal(sc).Slots["name"]; s.Value != "gadget" || s.Origin != sessions.OriginCorrection {
			t.Fatalf("slot = %+v", s)
		}
	})

	t.Run("an invalid corrected value is asked again and nothing is replaced", func(t *testing.T) {
		e.proposeOnce(capCreate, "correction", map[string]any{"quantity": "lots"})
		out := e.say(sc, "no, lots")
		wantAct(t, out, "ask_invalid")
		if s := e.goal(sc).Slots["quantity"]; slotNumber(t, s) != "7" {
			t.Fatalf("an invalid value replaced the old one: %+v", s)
		}
	})

	t.Run("a correction for a capability the goal is not on corrects nothing", func(t *testing.T) {
		e2 := newEnv(t, withLevel(level2))
		e2.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
		e2.prepareCreate(sc)
		e2.proposeOnce(capGet, "correction", map[string]any{"item_id": "item_1"})
		out := e2.say(sc, "no, show me item item_1")
		wantAct(t, out, "present_result")
		st := e2.session(sc).State
		if st.Goal.CapabilityID != capGet || len(st.Suspended) != 1 || st.Suspended[0].Slots["name"].Value != "widget" {
			t.Fatalf("the create goal was edited or lost: goal = %+v, suspended = %+v", st.Goal, st.Suspended)
		}
		if st.Goal.Slots["item_id"].Origin == sessions.OriginCorrection {
			t.Fatalf("a request for another capability was recorded as a correction")
		}
	})

	t.Run("a cue does not reopen a finished goal", func(t *testing.T) {
		e4 := newEnv(t, withLevel(level2))
		prep := e4.prepareCreate(sc)
		e4.turn(sc, confirmInput(prep))
		done := e4.goal(sc)
		if done.Phase != capabilities.PhaseSuccess {
			t.Fatalf("goal = %+v", done)
		}
		e4.proposeOnce(capCreate, "task", map[string]any{"name": "gadget", "quantity": 1})
		out := e4.say(sc, "no, create another one named gadget, 1")
		wantAct(t, out, "request_confirmation")
		g := e4.goal(sc)
		if g.ID == done.ID || g.Slots["name"].Origin == sessions.OriginCorrection {
			t.Fatalf("a request for a new item reopened the committed goal: %+v", g)
		}
		if st := e4.session(sc).State; len(st.Capsules) != 1 || st.Capsules[0].Outcome != sessions.OutcomeSuccess {
			t.Fatalf("capsules = %+v", st.Capsules)
		}
	})

	t.Run("a finished goal is reopened by a correction and replaced by a new task", func(t *testing.T) {
		e3 := newEnv(t, withLevel(level2))
		e3.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
		e3.app.Seed(tenantA, workspace1, "item_2", "Beta widget")
		e3.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
		e3.say(sc, sayShow)
		first := e3.goal(sc)
		e3.proposeOnce(capGet, "correction", map[string]any{"item_id": "item_2"})
		out := e3.say(sc, "no, item_2")
		wantAct(t, out, "present_result")
		g := e3.goal(sc)
		if g.ID != first.ID || g.Slots["item_id"].Value != "item_2" || g.Slots["item_id"].Origin != sessions.OriginCorrection {
			t.Fatalf("the finished goal was not reopened by the correction: %+v", g)
		}
		if st := e3.session(sc).State; len(st.Capsules) != 0 {
			t.Fatalf("a correction ended the goal: capsules = %+v", st.Capsules)
		}
		e3.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
		out = e3.say(sc, "show me item item_1")
		wantAct(t, out, "present_result")
		if st := e3.session(sc).State; len(st.Capsules) != 1 || st.Capsules[0].CapabilityID != capGet || st.Capsules[0].Outcome != sessions.OutcomeSuccess {
			t.Fatalf("a new task did not leave the finished goal as a capsule: %+v", st.Capsules)
		}
	})
}

// SEC-042: a prepared action built on a value the user then corrected is never committed.
func TestCorrectionInvalidatesPreparedAction(t *testing.T) {
	e := newEnv(t, withLevel(level2))
	sc := memberScope()
	first := e.prepareCreate(sc)
	stored := e.goal(sc)
	if stored.PreparedID != string(first.Prepared.PreparationID) || stored.Phase != capabilities.PhasePrepared {
		t.Fatalf("goal = %+v", stored)
	}
	// SetPrepared recorded the preparation's own expiry, so the session stops offering a
	// confirmation that can no longer be given.
	if want := preparedExpiry(*first.Prepared); want.IsZero() || !stored.PreparedExpiresAt.Equal(want) {
		t.Fatalf("PreparedExpiresAt = %v, want the preparation's %v", stored.PreparedExpiresAt, want)
	}

	e.proposeOnce(capCreate, "correction", map[string]any{"quantity": 5})
	second := e.say(sc, "no, make the quantity 5 instead")
	wantAct(t, second, "request_confirmation")

	// The coordinator, not only the session, refuses the old preparation.
	if states := e.prepStates(); len(states) != 2 || states[0] != "invalidated" || states[1] != "prepared" {
		t.Fatalf("preparation states = %v, want [invalidated prepared]", states)
	}
	_, err := e.co.Confirm(ctxb(), sc, actions.ConfirmInput{PreparationID: string(first.Prepared.PreparationID), Token: first.Confirmation})
	if err == nil {
		t.Fatal("the coordinator confirmed a preparation built on a corrected value")
	}
	if rc, err := e.co.Commit(ctxb(), sc, string(first.Prepared.PreparationID)); err == nil || rc.ReceiptID != "" {
		t.Fatalf("the coordinator committed a preparation built on a corrected value: %+v, %v", rc, err)
	}
	// And so does the dialogue: the old confirmation is not the one this session waits on.
	old, err := e.eng.Turn(ctxb(), sc, confirmInput(first))
	if err != nil {
		t.Fatal(err)
	}
	if old.Code != contracts.ErrConflict || old.Receipt != nil {
		t.Fatalf("old confirmation: code = %s, receipt = %+v", old.Code, old.Receipt)
	}
	if e.hostCalls(adapters.OpCommit) != 0 || e.app.Effects() != 0 {
		t.Fatalf("the host saw %d commits and applied %d effects before any valid confirmation", e.hostCalls(adapters.OpCommit), e.app.Effects())
	}

	// The new preparation is the one that commits, with the corrected value.
	out := e.turn(sc, confirmInput(second))
	wantAct(t, out, "report_outcome")
	if out.Receipt == nil || e.app.Effects() != 1 || e.hostCalls(adapters.OpCommit) != 1 {
		t.Fatalf("receipt = %+v, effects = %d", out.Receipt, e.app.Effects())
	}

	t.Run("a correction that replaces nothing still ends the old preparation", func(t *testing.T) {
		e2 := newEnv(t, withLevel(level2))
		p := e2.prepareCreate(sc)
		e2.proposeOnce(capCreate, "correction", map[string]any{"quantity": 3}) // the same value
		again := e2.say(sc, "no, quantity 3")
		wantAct(t, again, "request_confirmation")
		if again.Prepared.PreparationID == p.Prepared.PreparationID {
			t.Fatal("the old preparation was reused after a correction")
		}
		if states := e2.prepStates(); states[0] != "invalidated" {
			t.Fatalf("states = %v", states)
		}
	})

	t.Run("a coordinator that cannot void the preparation fails the turn closed", func(t *testing.T) {
		e2 := newEnv(t, withLevel(level2))
		p := e2.prepareCreate(sc)
		if _, err := e2.db.ExecContext(ctxb(), `DROP TABLE action_preparations`); err != nil {
			t.Fatal(err)
		}
		e2.proposeOnce(capCreate, "correction", map[string]any{"quantity": 5})
		_, err := e2.eng.Turn(ctxb(), sc, TurnInput{Text: "no, make the quantity 5 instead", Language: "en"})
		if err == nil {
			t.Fatal("the turn went on with a preparation it could not void")
		}
		_ = p
	})
}

// ---- cancel ----

func TestCancelDiscardsPrepared(t *testing.T) {
	sc := memberScope()

	t.Run("a prepared action is rejected and the goal ends", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		prep := e.prepareCreate(sc)
		calls := e.model.Calls()

		res := e.say(sc, "cancel")

		wantAct(t, res, "acknowledge_cancel")
		if e.model.Calls() != calls {
			t.Fatal("an exact cancel called the model")
		}
		if states := e.prepStates(); len(states) != 1 || states[0] != "rejected" {
			t.Fatalf("preparation states = %v", states)
		}
		st := e.session(sc).State
		if st.Goal != nil || len(st.Suspended) != 0 {
			t.Fatalf("state after a cancel = %+v", st)
		}
		if len(st.Capsules) != 1 || st.Capsules[0].Outcome != sessions.OutcomeCancelled || len(st.Capsules[0].Facts) != 0 {
			t.Fatalf("capsules = %+v, want one cancelled capsule with no value", st.Capsules)
		}
		if got := st.Capsules[0].Fields; len(got) != 2 {
			t.Fatalf("capsule fields = %v: it names what was collected, not the values", got)
		}
		if !hasTransition(res, TransitionCancelled) {
			t.Fatalf("transitions = %v", res.Trace.Transitions)
		}
		// The rejected preparation can never be confirmed, in the dialogue or at the coordinator.
		old, err := e.eng.Turn(ctxb(), sc, confirmInput(prep))
		if err != nil || old.Code != contracts.ErrConflict {
			t.Fatalf("old confirmation: %+v, %v", old.Code, err)
		}
		if _, err := e.co.Confirm(ctxb(), sc, actions.ConfirmInput{PreparationID: string(prep.Prepared.PreparationID), Token: prep.Confirmation}); err == nil {
			t.Fatal("the coordinator confirmed a rejected preparation")
		}
		if e.hostCalls(adapters.OpCommit) != 0 || e.app.Effects() != 0 {
			t.Fatal("a cancel was followed by a commit")
		}
	})

	t.Run("a pending question is forgotten and a later answer starts nothing", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		e.proposeOnce(capCreate, "task", map[string]any{"name": "widget"})
		ask := e.say(sc, "create an item named widget")
		wantAct(t, ask, "ask_missing")
		if g := e.goal(sc); g == nil || g.Pending == nil {
			t.Fatalf("goal = %+v", g)
		}

		res := e.say(sc, "never mind")

		wantAct(t, res, "acknowledge_cancel")
		if g := e.goal(sc); g != nil {
			t.Fatalf("goal after a cancel = %+v", g)
		}
		// "3" would have answered the question; with the goal gone it answers nothing.
		late := e.say(sc, "3")
		wantAct(t, late, "refuse")
		e.noHostWrites()
	})

	t.Run("a typed no declines the pending preparation like a rejection", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		e.prepareCreate(sc)
		calls := e.model.Calls()
		res := e.say(sc, "No.")
		wantAct(t, res, "acknowledge_cancel")
		if states := e.prepStates(); states[0] != "rejected" || e.model.Calls() != calls {
			t.Fatalf("states = %v, model calls = %d", states, e.model.Calls()-calls)
		}
		e.noCommits()
	})

	t.Run("a typed yes never commits: the pending request is shown again", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		prep := e.prepareCreate(sc)
		calls := e.model.Calls()
		res := e.say(sc, "Yes please")
		wantAct(t, res, "request_confirmation")
		if res.Prepared == nil || res.Prepared.PreparationID != prep.Prepared.PreparationID || !res.Confirmation.IsZero() {
			t.Fatalf("prepared = %+v, token zero = %v: the same request, with no new secret", res.Prepared, res.Confirmation.IsZero())
		}
		if e.hostCalls(adapters.OpCommit) != 0 || e.app.Effects() != 0 || e.model.Calls() != calls+1 {
			t.Fatalf("commits = %d, effects = %d, model calls = %d", e.hostCalls(adapters.OpCommit), e.app.Effects(), e.model.Calls()-calls)
		}
		if g := e.goal(sc); g.PreparedID != string(prep.Prepared.PreparationID) || g.Confirmed {
			t.Fatalf("goal = %+v", g)
		}
		// The real confirmation still works.
		out := e.turn(sc, confirmInput(prep))
		if out.Receipt == nil || e.app.Effects() != 1 {
			t.Fatalf("receipt = %+v", out.Receipt)
		}
	})

	t.Run("a yes or a no with nothing prepared is an ordinary answer", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		e.proposeOnce(capCreate, "task", map[string]any{"name": "widget"})
		e.say(sc, "create an item named widget")
		before := e.proposals()
		e.proposeOnce(capCreate, "task", map[string]any{"quantity": 2})
		res := e.say(sc, "no")
		if e.proposals() != before+1 {
			t.Fatalf("a bare no with no preparation pending was not left to the controller")
		}
		wantAct(t, res, "request_confirmation")
	})

	t.Run("cancel all drops the set-aside goals too", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
		e.prepareCreate(sc)
		e.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
		e.say(sc, sayShow)
		if st := e.session(sc).State; len(st.Suspended) != 1 {
			t.Fatalf("suspended = %+v", st.Suspended)
		}
		res := e.say(sc, "cancel everything")
		wantAct(t, res, "acknowledge_cancel")
		if st := e.session(sc).State; st.Goal != nil || len(st.Suspended) != 0 {
			t.Fatalf("state = %+v", st)
		}
	})

	t.Run("a cancel with nothing to cancel is left to the router", func(t *testing.T) {
		e := newEnv(t)
		res := e.say(sc, "cancel")
		wantAct(t, res, "refuse")
	})

	t.Run("a coordinator that cannot reject keeps the goal and claims nothing", func(t *testing.T) {
		e := newEnv(t, withLevel(level2))
		e.prepareCreate(sc)
		if _, err := e.db.ExecContext(ctxb(), `DROP TABLE action_preparations`); err != nil {
			t.Fatal(err)
		}
		res, err := e.eng.Turn(ctxb(), sc, TurnInput{Text: "cancel", Language: "en"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Directive.Act == "acknowledge_cancel" || !res.Failed() {
			t.Fatalf("a cancel that did not discard the preparation was acknowledged: %s (%s)", res.Directive.Act, res.Code)
		}
		if g := e.goal(sc); g == nil || g.PreparedID == "" {
			t.Fatalf("the goal was dropped without its preparation being: %+v", g)
		}
	})
}

// ---- restart ----

func TestRestartGoal(t *testing.T) {
	sc := memberScope()
	e := newEnv(t, withLevel(level2))
	first := e.prepareCreate(sc)
	old := e.goal(sc)
	if old.Slots["name"].Value != "widget" {
		t.Fatalf("goal = %+v", old)
	}
	e.clk.Advance(time.Minute)
	calls := e.model.Calls()

	res := e.say(sc, "start over")

	wantAct(t, res, "ask_missing")
	if e.model.Calls() != calls+1 || e.proposals() != 1 {
		t.Fatalf("a restart called the model for more than the response")
	}
	if got := res.Directive.MissingFields; len(got) != 2 || got[0] != "name" || got[1] != "quantity" {
		t.Fatalf("missing = %v: a restart begins from nothing", got)
	}
	g := e.goal(sc)
	if g.CapabilityID != capCreate || len(g.Slots) != 0 || len(g.Entities) != 0 || g.PreparedID != "" || g.Phase != capabilities.PhaseOpen {
		t.Fatalf("restarted goal = %+v", g)
	}
	if g.Pending == nil || g.Pending.Field != "name" {
		t.Fatalf("pending = %+v, want the first field asked again", g.Pending)
	}
	if g.ID == old.ID || !g.StartedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("goal id = %q (was %q), started %v: not a fresh goal", g.ID, old.ID, g.StartedAt)
	}
	// The preparation of the abandoned attempt is voided, and cannot be confirmed.
	if states := e.prepStates(); len(states) != 1 || states[0] != "invalidated" {
		t.Fatalf("preparation states = %v", states)
	}
	if out, err := e.eng.Turn(ctxb(), sc, confirmInput(first)); err != nil || out.Code != contracts.ErrConflict {
		t.Fatalf("old confirmation: %+v, %v", out.Code, err)
	}
	if !hasTransition(res, TransitionRestarted) {
		t.Fatalf("transitions = %v", res.Trace.Transitions)
	}

	// The goal then proceeds on new values only.
	e.proposeOnce(capCreate, "task", map[string]any{"name": "gadget", "quantity": 2})
	out := e.say(sc, "an item named gadget, 2 of them")
	wantAct(t, out, "request_confirmation")
	if got := e.sc.prepared[len(e.sc.prepared)-1]; got["name"] != "gadget" || got["quantity"] != json.Number("2") {
		t.Fatalf("prepared with %v: a value of the abandoned attempt survived the restart", got)
	}

	t.Run("a restart with no goal is left to the router", func(t *testing.T) {
		e2 := newEnv(t)
		wantAct(t, e2.say(sc, "start over"), "refuse")
	})

	t.Run("a restart keeps a nested goal under its parent", func(t *testing.T) {
		e2 := newEnv(t, withLevel(level2))
		e2.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
		e2.proposeOnce(capCreate, "task", map[string]any{"name": "widget"})
		e2.say(sc, "create an item named widget")
		e2.proposeOnce(capGet, "task", nil)
		e2.say(sc, "first, show me an item")
		child := e2.goal(sc)
		if child.CapabilityID != capGet || child.ParentID == "" {
			t.Fatalf("goal = %+v", child)
		}
		wantAct(t, e2.say(sc, "restart"), "ask_missing")
		again := e2.goal(sc)
		if again.CapabilityID != capGet || again.ParentID != child.ParentID || len(e2.session(sc).State.Suspended) != 1 {
			t.Fatalf("the restart lost the parent: %+v", again)
		}
	})
}

// ---- topic switch and resume ----

func TestTopicSwitchSuspendsAndResumes(t *testing.T) {
	sc := memberScope()
	e := newEnv(t, withLevel(level2))
	e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
	first := e.prepareCreate(sc)
	started := e.goal(sc)

	// A request for another capability sets the goal aside; it is not dropped.
	e.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
	res := e.say(sc, sayShow)

	wantAct(t, res, "present_result")
	st := e.session(sc).State
	if st.Goal == nil || st.Goal.CapabilityID != capGet {
		t.Fatalf("active goal = %+v", st.Goal)
	}
	if len(st.Suspended) != 1 {
		t.Fatalf("suspended = %+v", st.Suspended)
	}
	susp := st.Suspended[0]
	if susp.CapabilityID != capCreate || susp.ID != started.ID || susp.SuspendReason != sessions.SuspendedTopicSwitch {
		t.Fatalf("suspended goal = %+v", susp)
	}
	if susp.Slots["name"].Value != "widget" || slotNumber(t, susp.Slots["quantity"]) != "3" {
		t.Fatalf("the suspended goal lost its slots: %+v", susp.Slots)
	}
	if want := t0.Add(sessions.DefaultSuspendedTTL); !susp.ExpiresAt.Equal(want) {
		t.Fatalf("suspended until %v, want %v", susp.ExpiresAt, want)
	}
	// A suspended goal holds no preparation: a confirmation cannot be answered while the
	// user is somewhere else.
	if susp.PreparedID != "" || !susp.PreparedExpiresAt.IsZero() {
		t.Fatalf("the suspended goal still references a preparation: %+v", susp)
	}
	if states := e.prepStates(); len(states) != 1 || states[0] != "invalidated" {
		t.Fatalf("preparation states = %v", states)
	}
	if old, err := e.eng.Turn(ctxb(), sc, confirmInput(first)); err != nil || old.Code != contracts.ErrConflict {
		t.Fatalf("old confirmation: %+v, %v", old.Code, err)
	}
	if !hasTransition(res, TransitionSuspended) {
		t.Fatalf("transitions = %v", res.Trace.Transitions)
	}

	// Resume restores it, slots and all, and asks for a fresh confirmation.
	proposals := e.proposals()
	back := e.say(sc, "go back to what I was doing")

	wantAct(t, back, "request_confirmation")
	if e.proposals() != proposals {
		t.Fatal("a resume asked the model what the user meant")
	}
	st = e.session(sc).State
	if st.Goal == nil || st.Goal.CapabilityID != capCreate || st.Goal.ID != started.ID || len(st.Suspended) != 0 {
		t.Fatalf("after the resume: goal = %+v, suspended = %+v", st.Goal, st.Suspended)
	}
	if st.Goal.Slots["name"].Value != "widget" || slotNumber(t, st.Goal.Slots["quantity"]) != "3" {
		t.Fatalf("slots after the resume = %+v", st.Goal.Slots)
	}
	if back.Prepared == nil || back.Prepared.PreparationID == first.Prepared.PreparationID {
		t.Fatal("the resumed goal reused the voided preparation")
	}
	if len(st.Capsules) != 1 || st.Capsules[0].CapabilityID != capGet {
		t.Fatalf("the finished lookup did not become a capsule: %+v", st.Capsules)
	}
	if !hasTransition(back, TransitionResumed) {
		t.Fatalf("transitions = %v", back.Trace.Transitions)
	}
	if got := e.sc.prepared[len(e.sc.prepared)-1]; got["name"] != "widget" || got["quantity"] != json.Number("3") {
		t.Fatalf("prepared with %v", got)
	}
	out := e.turn(sc, confirmInput(back))
	if out.Receipt == nil || e.app.Effects() != 1 {
		t.Fatalf("receipt = %+v", out.Receipt)
	}

	t.Run("a goal that waits for an answer is resumed with its question", func(t *testing.T) {
		e2 := newEnv(t, withLevel(level2))
		e2.proposeOnce(capCreate, "task", map[string]any{"name": "widget"})
		wantAct(t, e2.say(sc, "create an item named widget"), "ask_missing")
		// A write is not a lookup: it is a topic switch, not a side trip.
		e2.proposeOnce(capCreate, "task", map[string]any{"name": "other", "quantity": 1})
		e2.say(sc, "create an item named other, 1")
		if st := e2.session(sc).State; len(st.Suspended) != 0 || st.Goal.Slots["name"].Value != "other" {
			t.Fatalf("a new request for the same capability set the goal aside: %+v", st)
		}
		e2.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
		e2.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
		e2.say(sc, sayShow)
		back := e2.say(sc, "resume")
		wantAct(t, back, "request_confirmation") // the goal that was prepared (its confirmation was voided) is prepared again
		if g := e2.goal(sc); g.Slots["name"].Value != "other" {
			t.Fatalf("goal = %+v", g)
		}
	})

	t.Run("an open goal's own question comes back", func(t *testing.T) {
		e2 := newEnv(t, withLevel(level2))
		e2.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
		e2.proposeOnce(capCreate, "task", map[string]any{"name": "widget"})
		wantAct(t, e2.say(sc, "create an item named widget"), "ask_missing")
		// Prepared first so that the next request is not a nested lookup.
		e2.proposeOnce(capCreate, "task", map[string]any{"quantity": 3})
		e2.say(sc, "3")
		e2.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
		e2.say(sc, sayShow)
		e2.say(sc, "resume")
		if g := e2.goal(sc); g == nil || g.CapabilityID != capCreate {
			t.Fatalf("goal = %+v", g)
		}
	})

	t.Run("a suspended goal expires after its own time", func(t *testing.T) {
		e2 := newEnv(t, withLevel(level2))
		e2.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
		e2.prepareCreate(sc)
		e2.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
		e2.say(sc, sayShow)
		e2.clk.Advance(sessions.DefaultSuspendedTTL + time.Minute)
		res := e2.say(sc, "resume")
		wantAct(t, res, "refuse") // nothing to resume: expired state is never reused
		if st := e2.session(sc).State; len(st.Suspended) != 0 {
			t.Fatalf("suspended = %+v", st.Suspended)
		}
		if !hasTransition(res, TransitionExpired) {
			t.Fatalf("transitions = %v", res.Trace.Transitions)
		}
	})

	t.Run("continue does not take over a goal still in progress", func(t *testing.T) {
		e2 := newEnv(t, withLevel(level2))
		e2.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
		e2.prepareCreate(sc)
		e2.proposeOnce(capGet, "task", nil)
		e2.say(sc, "show me an item") // the lookup is waiting for an item
		e2.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
		// "continue" while the lookup is in progress belongs to the lookup, not to the goal set aside.
		before := e2.proposals()
		e2.say(sc, "continue")
		if e2.proposals() != before+1 {
			t.Fatal("a weak resume replaced the goal in progress")
		}
	})

	t.Run("a resume names the goal it wants", func(t *testing.T) {
		e2 := newEnv(t, withLevel(level2))
		e2.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
		e2.prepareCreate(sc)
		e2.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
		e2.say(sc, sayShow)
		// Words that name no set-aside goal do not resume one.
		before := e2.proposals()
		e2.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
		e2.say(sc, "go back to the weather")
		if e2.proposals() != before+1 || len(e2.session(sc).State.Suspended) != 1 {
			t.Fatal("a resume that names nothing set aside resumed something")
		}
		res := e2.say(sc, "go back to the create item")
		wantAct(t, res, "request_confirmation")
	})
}

// ---- nested goals ----

func TestNestedGoalReturnsToParent(t *testing.T) {
	sc := memberScope()
	e := newEnv(t, withLevel(level2))
	e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")

	// The parent waits for a quantity.
	e.proposeOnce(capCreate, "task", map[string]any{"name": "widget"})
	wantAct(t, e.say(sc, "create an item named widget"), "ask_missing")
	parent := e.goal(sc)
	if parent.Pending == nil || parent.Pending.Field != "quantity" {
		t.Fatalf("parent = %+v", parent)
	}

	// A lookup meanwhile is a side trip: nested, then back.
	e.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
	res := e.say(sc, "first, show me item item_1")

	wantAct(t, res, "present_result")
	if !hasTransition(res, TransitionNested) || !hasTransition(res, TransitionReturned) {
		t.Fatalf("transitions = %v", res.Trace.Transitions)
	}
	st := e.session(sc).State
	if st.Goal == nil || st.Goal.CapabilityID != capCreate || st.Goal.ID != parent.ID || len(st.Suspended) != 0 {
		t.Fatalf("after the lookup: goal = %+v, suspended = %+v", st.Goal, st.Suspended)
	}
	if st.Goal.ParentID != "" || st.Goal.Slots["name"].Value != "widget" {
		t.Fatalf("parent = %+v", st.Goal)
	}
	if st.Goal.Pending == nil || st.Goal.Pending.Field != "quantity" {
		t.Fatalf("the parent's question was lost: %+v", st.Goal.Pending)
	}
	// The child left a compact capsule: what it was, how it ended, no value.
	if len(st.Capsules) != 1 || st.Capsules[0].CapabilityID != capGet || st.Capsules[0].Outcome != sessions.OutcomeSuccess || len(st.Capsules[0].Facts) != 0 {
		t.Fatalf("capsules = %+v", st.Capsules)
	}
	// What the lookup presented stays available for "that item".
	if len(st.Results) != 1 || len(st.Results[0].Items) != 1 || st.Results[0].Items[0].EntityID != "item_1" {
		t.Fatalf("results = %+v", st.Results)
	}

	// The next answer goes to the parent.
	e.proposeOnce(capCreate, "task", map[string]any{"quantity": 3})
	out := e.say(sc, "3")
	wantAct(t, out, "request_confirmation")
	g := e.goal(sc)
	if g.ID != parent.ID || g.Slots["name"].Value != "widget" || slotNumber(t, g.Slots["quantity"]) != "3" {
		t.Fatalf("goal = %+v", g)
	}
	if got := e.sc.prepared[len(e.sc.prepared)-1]; got["name"] != "widget" || got["quantity"] != json.Number("3") {
		t.Fatalf("prepared with %v", got)
	}

	t.Run("cancelling the child returns to the parent", func(t *testing.T) {
		e2 := newEnv(t, withLevel(level2))
		e2.proposeOnce(capCreate, "task", map[string]any{"name": "widget"})
		e2.say(sc, "create an item named widget")
		parent := e2.goal(sc)
		e2.proposeOnce(capGet, "task", nil)
		ask := e2.say(sc, "first, show me an item")
		wantAct(t, ask, "ask_missing")
		if g := e2.goal(sc); g.CapabilityID != capGet || g.ParentID != parent.ID {
			t.Fatalf("the lookup is not nested under the goal: %+v", g)
		}
		res := e2.say(sc, "never mind")
		wantAct(t, res, "acknowledge_cancel")
		g := e2.goal(sc)
		if g == nil || g.ID != parent.ID || g.Pending == nil || g.Pending.Field != "quantity" || len(e2.session(sc).State.Suspended) != 0 {
			t.Fatalf("after cancelling the child: %+v", g)
		}
	})

	t.Run("a lookup while the goal is prepared is a topic switch, not a side trip", func(t *testing.T) {
		e2 := newEnv(t, withLevel(level2))
		e2.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
		e2.prepareCreate(sc)
		e2.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
		res := e2.say(sc, "first, show me item item_1")
		if hasTransition(res, TransitionNested) {
			t.Fatalf("a prepared goal was nested: %v", res.Trace.Transitions)
		}
		if st := e2.session(sc).State; len(st.Suspended) != 1 || st.Goal.CapabilityID != capGet {
			t.Fatalf("state = %+v", st)
		}
	})
}

// ---- relative references ----

func TestRelativeReferenceResolves(t *testing.T) {
	sc := memberScope()
	hostList := func(e *env, ids, labels []string) {
		// The host resolves mentions to these candidates, and a plain read presents them as a list.
		e.sc.set(func(s *scripted) {
			s.onRead = func(req adapters.ReadRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				if _, resolving := req.Fields["entity_type"]; resolving {
					return resolveResult(ids, labels), nil
				}
				return resolveResult(ids, labels), nil // a read that lists the records, each with its label
			}
		})
	}

	t.Run("the second one answers the question just asked", func(t *testing.T) {
		e := newEnv(t, withResolver())
		hostList(e, []string{"item_a", "item_b"}, []string{"Alpha one", "Alpha two"})
		e.proposeOnce(capGet, "task", nil, alphaRef())
		wantAct(t, e.say(sc, "show me the alpha item"), "disambiguate")
		proposals := e.proposals()

		// Resolving a mention is not a read: from now on the host answers a real read.
		e.sc.set(func(s *scripted) {
			s.onRead = func(req adapters.ReadRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				return resolveResult([]string{"item_b"}, []string{"Alpha two"}), nil
			}
		})
		res := e.say(sc, "the second one")

		wantAct(t, res, "present_result")
		if e.proposals() != proposals {
			t.Fatal("a reference the state resolves was sent to the model")
		}
		last := e.sc.reads[len(e.sc.reads)-1]
		if last.Fields["item_id"] != "item_b" {
			t.Fatalf("the read used %v, want the second candidate", last.Fields)
		}
		g := e.goal(sc)
		if s := g.Slots["item_id"]; s.Value != "item_b" || s.Origin != sessions.OriginEntityResolution || s.Type != "entity_ref" {
			t.Fatalf("slot = %+v", s)
		}
		if len(g.Entities) != 1 || g.Entities[0].EntityID != "item_b" || g.Entities[0].Version != "v7" {
			t.Fatalf("entities = %+v", g.Entities)
		}
		if !hasTransition(res, TransitionReference) {
			t.Fatalf("transitions = %v", res.Trace.Transitions)
		}
	})

	t.Run("a position the list does not have is not guessed", func(t *testing.T) {
		e := newEnv(t, withResolver())
		hostList(e, []string{"item_a", "item_b"}, []string{"Alpha one", "Alpha two"})
		e.proposeOnce(capGet, "task", nil, alphaRef())
		e.say(sc, "show me the alpha item")
		res := e.say(sc, "the fifth one")
		wantAct(t, res, "ask_invalid")
		if g := e.goal(sc); g.Pending == nil || len(g.Pending.Choices) != 2 {
			t.Fatalf("the question did not stand: %+v", g.Pending)
		}
		e.noCommits()
		last := e.say(sc, "the last one")
		if g := e.goal(sc); g == nil || g.Slots["item_id"].Value != "item_b" {
			t.Fatalf("the last one = %+v (act %s)", g, last.Directive.Act)
		}
	})

	t.Run("an ordinal in Italian", func(t *testing.T) {
		e := newEnv(t, withResolver())
		hostList(e, []string{"item_a", "item_b"}, []string{"Alpha uno", "Alpha due"})
		e.proposeOnce(capGet, "task", nil, alphaRef())
		e.turn(sc, TurnInput{Text: "mostrami l'articolo alpha", Language: "it"})
		e.turn(sc, TurnInput{Text: "il secondo", Language: "it"})
		if g := e.goal(sc); g == nil || g.Slots["item_id"].Value != "item_b" {
			t.Fatalf("goal = %+v", g)
		}
	})

	t.Run("the second one of the list a result presented", func(t *testing.T) {
		e := newEnv(t, withResolver())
		// One mention resolves to one record; the read then presents a list of three.
		e.sc.set(func(s *scripted) {
			s.onRead = func(req adapters.ReadRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
				if _, resolving := req.Fields["entity_type"]; resolving {
					return resolveResult([]string{"item_a"}, []string{"Alpha"}), nil
				}
				return resolveResult([]string{"item_a", "item_b", "item_c"}, []string{"Alpha", "Beta", "Gamma"}), nil
			}
		})
		e.proposeOnce(capGet, "task", nil, alphaRef())
		wantAct(t, e.say(sc, "show me the alpha item"), "present_result")
		st := e.session(sc).State
		if len(st.Results) != 1 || len(st.Results[0].Items) != 3 || st.Results[0].Items[1].EntityID != "item_b" || st.Results[0].Items[1].Label != "Beta" {
			t.Fatalf("results = %+v", st.Results)
		}
		proposals := e.proposals()

		res := e.say(sc, "show the second one")

		wantAct(t, res, "present_result")
		if e.proposals() != proposals {
			t.Fatal("the model was asked which record the second one was")
		}
		last := e.sc.reads[len(e.sc.reads)-1]
		if last.Fields["item_id"] != "item_b" {
			t.Fatalf("the read used %v", last.Fields)
		}
		g := e.goal(sc)
		if s := g.Slots["item_id"]; s.Value != "item_b" || s.Origin != sessions.OriginEntityResolution {
			t.Fatalf("slot = %+v", s)
		}
		if len(g.Entities) != 1 || g.Entities[0].EntityID != "item_b" {
			t.Fatalf("entities = %+v", g.Entities)
		}
	})

	t.Run("that item is the record the conversation just made", func(t *testing.T) {
		e := newEnv(t, withLevel(level2), withResolver())
		prep := e.prepareCreate(sc)
		e.turn(sc, confirmInput(prep))
		created := ""
		for id := range e.app.Items(tenantA, workspace1) {
			created = id
		}
		if created == "" {
			t.Fatal("nothing was created")
		}
		st := e.session(sc).State
		if len(st.Results) != 1 || st.Results[0].Kind != sessions.ResultCommit || st.Results[0].Items[0].EntityID != created {
			t.Fatalf("results = %+v", st.Results)
		}
		readsBefore := len(e.sc.reads)

		e.proposeOnce(capGet, "task", nil, map[string]any{"entity_type": "item", "mention": "that item", "field": "item_id"})
		res := e.say(sc, "show that item")

		wantAct(t, res, "present_result")
		reads := e.sc.reads[readsBefore:]
		for _, r := range reads {
			if _, resolving := r.Fields["mention"]; resolving {
				t.Fatalf("the host was asked to resolve %v: the state already knew the record", r.Fields)
			}
		}
		if len(reads) != 1 || reads[0].Fields["item_id"] != created {
			t.Fatalf("reads = %+v, want one read of %s", reads, created)
		}
		g := e.goal(sc)
		if len(g.Entities) != 1 || g.Entities[0].EntityID != created || g.Slots["item_id"].Origin != sessions.OriginEntityResolution {
			t.Fatalf("goal = %+v", g)
		}
	})

	t.Run("a noun the application does not use for the type is not resolved", func(t *testing.T) {
		e := newEnv(t, withLevel(level2), withResolver())
		prep := e.prepareCreate(sc)
		e.turn(sc, confirmInput(prep))
		resolver(e, nil, nil) // the host finds no such record
		e.proposeOnce(capGet, "task", nil, map[string]any{"entity_type": "item", "mention": "that patient", "field": "item_id"})
		res := e.say(sc, "show that patient")
		wantAct(t, res, "ask_invalid") // not guessed: the mention went to the host, which found nothing
	})

	t.Run("a synonym of the type's term resolves", func(t *testing.T) {
		e := newEnv(t, withLevel(level2), withResolver())
		prep := e.prepareCreate(sc)
		e.turn(sc, confirmInput(prep))
		e.proposeOnce(capGet, "task", nil, map[string]any{"entity_type": "item", "mention": "that thing", "field": "item_id"})
		wantAct(t, e.say(sc, "show that thing"), "present_result")
	})

	t.Run("same time tomorrow is the time of the date and time the goal holds", func(t *testing.T) {
		e := newEnv(t, withLevel(level2), withWhen())
		e.proposeOnce(capCreate, "task", map[string]any{"name": "widget", "quantity": 3, "when": "2026-10-02T15:00:00Z"})
		first := e.say(sc, "create an item named widget, 3, today at 15:00")
		wantAct(t, first, "request_confirmation")
		proposals := e.proposals()

		res := e.say(sc, "same time tomorrow")

		wantAct(t, res, "request_confirmation")
		if e.proposals() != proposals {
			t.Fatal("the model was asked what the time of the goal was")
		}
		g := e.goal(sc)
		s := g.Slots["when"]
		if s.Value != "2026-10-03T15:00:00Z" || s.Origin != sessions.OriginCorrection || s.Type != "datetime" {
			t.Fatalf("when = %+v", s)
		}
		if got := e.sc.prepared[len(e.sc.prepared)-1]["when"]; got != "2026-10-03T15:00:00Z" {
			t.Fatalf("prepared with when = %v", got)
		}
		if states := e.prepStates(); len(states) != 2 || states[0] != "invalidated" || states[1] != "prepared" {
			t.Fatalf("states = %v: the preparation of today's time was not voided", states)
		}
		if g.Slots["name"].Value != "widget" {
			t.Fatalf("a reference changed another slot: %+v", g.Slots)
		}
		if !hasTransition(res, TransitionReference) || !hasTransition(res, TransitionCorrected) {
			t.Fatalf("transitions = %v", res.Trace.Transitions)
		}
	})

	t.Run("same time in other words and other days", func(t *testing.T) {
		for _, tc := range []struct {
			say, lang, want string
		}{
			{"the same time on friday", "en", "2026-10-09T15:00:00Z"}, // today is a Friday: the next one
			{"same time next monday", "en", "2026-10-05T15:00:00Z"},
			{"same time in 3 days", "en", "2026-10-05T15:00:00Z"},
			{"same time next week", "en", "2026-10-09T15:00:00Z"},
			{"tomorrow at the same time please", "en", "2026-10-03T15:00:00Z"},
			{"same time the day after tomorrow", "en", "2026-10-04T15:00:00Z"},
			{"domani alla stessa ora", "it", "2026-10-03T15:00:00Z"},
			{"stesso orario dopodomani", "it", "2026-10-04T15:00:00Z"},
			{"alla stessa ora venerdì", "it", "2026-10-09T15:00:00Z"},
		} {
			e := newEnv(t, withLevel(level2), withWhen())
			e.proposeOnce(capCreate, "task", map[string]any{"name": "widget", "quantity": 3, "when": "2026-10-02T15:00:00Z"})
			e.say(sc, "create an item named widget, 3, today at 15:00")
			e.turn(sc, TurnInput{Text: tc.say, Language: tc.lang})
			if got := e.goal(sc).Slots["when"].Value; got != tc.want {
				t.Errorf("%q: when = %v, want %s", tc.say, got, tc.want)
			}
		}
	})

	t.Run("same time with no time in the state is not guessed", func(t *testing.T) {
		e := newEnv(t, withLevel(level2), withWhen())
		e.proposeOnce(capCreate, "task", map[string]any{"name": "widget", "quantity": 3})
		wantAct(t, e.say(sc, "create an item named widget, 3"), "ask_missing")
		e.proposeOnce(capCreate, "task", nil)
		res := e.say(sc, "same time tomorrow")
		wantAct(t, res, "ask_missing")
		if _, set := e.goal(sc).Slots["when"]; set {
			t.Fatal("a time was invented for a reference with nothing to resolve from")
		}
	})

	t.Run("same time resolves from the goal that was completed", func(t *testing.T) {
		e := newEnv(t, withLevel(level2), withWhen())
		e.proposeOnce(capCreate, "task", map[string]any{"name": "widget", "quantity": 3, "when": "2026-10-02T15:00:00Z"})
		prep := e.say(sc, "create an item named widget, 3, today at 15:00")
		e.turn(sc, confirmInput(prep))
		e.app.Seed(tenantA, workspace1, "item_1", "Alpha")
		// A lookup completes the booking into a capsule; the capsule keeps the time, and only the time.
		e.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
		e.say(sc, sayShow)
		e.proposeOnce(capCreate, "task", map[string]any{"name": "gadget", "quantity": 1})
		res := e.say(sc, "another one, same time tomorrow")
		wantAct(t, res, "request_confirmation")
		g := e.goal(sc)
		if g.Slots["when"].Value != "2026-10-03T15:00:00Z" || g.Slots["name"].Value != "gadget" {
			t.Fatalf("goal = %+v", g.Slots)
		}
		for _, c := range e.session(sc).State.Capsules {
			if c.CapabilityID == capCreate {
				if c.Facts["when"] == "" || c.Facts["name"] != "" || c.Facts["quantity"] != "" {
					t.Fatalf("capsule facts = %v: only the non-sensitive date and time are kept", c.Facts)
				}
			}
		}
	})

	t.Run("same time after a committed action does not commit it again", func(t *testing.T) {
		e := newEnv(t, withLevel(level1), withWhen())
		e.proposeOnce(capCreate, "task", map[string]any{"name": "widget", "quantity": 3, "when": "2026-10-02T15:00:00Z"})
		wantAct(t, e.say(sc, "create an item named widget, 3, today at 15:00"), "report_outcome")
		proposals := e.proposals()

		e.proposeOnce(capCreate, "task", nil)
		res := e.say(sc, "same time tomorrow")

		if n := e.app.Effects(); n != 1 {
			t.Fatalf("effects = %d: a finished goal was committed again from a reference alone", n)
		}
		if e.proposals() == proposals {
			t.Fatal("the reference reopened the finished goal without the model")
		}
		wantAct(t, res, "ask_missing")
		if g := e.goal(sc); g.Slots["when"].Value != "2026-10-03T15:00:00Z" || len(g.Slots) != 1 {
			t.Fatalf("goal = %+v: the new request keeps the time and nothing else", g.Slots)
		}
	})
}

// ---- the session a turn runs on ----

func TestNarrowerTokenEndsTheGoalsThatReliedOnTheLostCapability(t *testing.T) {
	e := newEnv(t, withLevel(level2))
	e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
	prep := e.prepareCreate(memberScope())

	// The next turn carries a token that no longer grants items.create.
	narrow := scopeFor("member", capGet)
	e.proposeOnce(capGet, "task", map[string]any{"item_id": "item_1"})
	res := e.say(narrow, sayShow)

	wantAct(t, res, "present_result")
	if states := e.prepStates(); len(states) != 1 || states[0] != "invalidated" {
		t.Fatalf("preparation states = %v: the preparation outlived the permission", states)
	}
	st := e.session(narrow).State
	if st.Goal == nil || st.Goal.CapabilityID != capGet || len(st.Suspended) != 0 {
		t.Fatalf("state = %+v: the goal on the lost capability was kept", st)
	}
	if out, err := e.eng.Turn(ctxb(), narrow, confirmInput(prep)); err != nil || out.Code != contracts.ErrConflict || e.app.Effects() != 0 {
		t.Fatalf("confirming after the permission change: %+v, %v", out.Code, err)
	}
	if !strings.Contains(strings.Join(res.Trace.Stages, ","), StageGoal) {
		t.Fatalf("stages = %v", res.Trace.Stages)
	}
}

func TestPreparedReferenceLapsesWithThePreparation(t *testing.T) {
	e := newEnv(t, withLevel(level2))
	sc := memberScope()
	prep := e.prepareCreate(sc)
	e.clk.Advance(11 * time.Minute) // past the preparation's own expiry, inside the goal's

	res := e.say(sc, "yes")

	// "yes" with no live preparation is an ordinary utterance, so the controller reads it.
	g := e.goal(sc)
	if g != nil && g.PreparedID != "" {
		t.Fatalf("the session still offers a confirmation that can no longer be given: %+v", g)
	}
	if !hasTransition(res, TransitionExpired) {
		t.Fatalf("transitions = %v", res.Trace.Transitions)
	}
	if out, err := e.eng.Turn(ctxb(), sc, confirmInput(prep)); err != nil || out.Code != contracts.ErrConflict && out.Code != contracts.ErrExpired {
		t.Fatalf("confirming a lapsed preparation: %+v, %v", out.Code, err)
	}
	e.noCommits()
}

func TestTypedYesDoesNotDropAConfirmedPreparationAwaitingARetry(t *testing.T) {
	e := newEnv(t, withLevel(level2))
	sc := memberScope()
	prep := e.prepareCreate(sc)
	e.app.SetAvailable(false)
	down := e.turn(sc, confirmInput(prep))
	wantAct(t, down, "report_failure")

	// The confirmation was used and the commit is to be retried: a typed yes shows the
	// request again and neither commits nor forgets it.
	res := e.say(sc, "yes")
	wantAct(t, res, "request_confirmation")
	if g := e.goal(sc); g == nil || g.PreparedID != string(prep.Prepared.PreparationID) || !g.Confirmed {
		t.Fatalf("goal = %+v, want the confirmed preparation kept for a retry", g)
	}
	if e.app.Effects() != 0 {
		t.Fatal("a typed yes committed")
	}

	e.app.SetAvailable(true)
	retry := e.turn(sc, confirmInput(prep))
	if retry.Receipt == nil || e.app.Effects() != 1 {
		t.Fatalf("retry: receipt = %+v, effects = %d", retry.Receipt, e.app.Effects())
	}
}

// The token names the capabilities a session may use (policy check 8, exact match), and
// Reconcile keeps exactly the goals whose capability the token still names: a goal on a
// capability the role grants and the token names survives the next turn.
func TestReconcileKeepsTheGoalsTheTokenStillNames(t *testing.T) {
	e := newEnv(t, withCount(), withConfig(Config{RouterOptions: []capabilities.RouterOption{
		capabilities.WithConfig(capabilities.Config{ConfidentMargin: 0.2}),
	}}))
	e.sc.set(func(s *scripted) {
		s.onRead = func(req adapters.ReadRequest, next func() (act.DeterministicResult, error)) (act.DeterministicResult, error) {
			if req.CapabilityID != capCount {
				return next()
			}
			res := resolveResult(nil, nil)
			res.CapabilityID = common.CapabilityID(capCount)
			res.Values = []act.Fact{{Field: "total", Value: act.CanonicalValue{Value: act.IntegerValue{Value: 2}}}}
			return res, nil
		}
	})
	sc := scopeFor("member", capGet, capCreate, capAdmin, capCount)
	wantAct(t, e.say(sc, "how many items"), "present_result")
	if g := e.goal(sc); g == nil || g.CapabilityID != capCount {
		t.Fatalf("goal = %+v", g)
	}
	wantAct(t, e.say(sc, "help"), "offer_help")
	if g := e.goal(sc); g == nil || g.CapabilityID != capCount {
		t.Fatalf("the next turn's reconcile discarded a goal on a capability the token names: %+v", g)
	}
	// A token that no longer names it ends the goal.
	e.say(scopeFor("member", capGet, capCreate, capAdmin), "help")
	if g := e.goal(sc); g != nil {
		t.Fatalf("a goal on a capability the token dropped survived: %+v", g)
	}
}
