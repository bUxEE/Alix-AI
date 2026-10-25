package dialogue

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/capabilities"
	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The tests of M3-18: the engine builds its prompts through the bounded context builder
// (internal/dialogue/context), within the budget of one profile, and shows the model only what
// the policy gateway's decisions of the turn let it see.

// ctxLiteWith returns the Lite profile with some limits changed: for a test that needs a budget
// the real table does not have (a tiny prompt cap, a short utterance bound).
func ctxLiteWith(f func(p *dctx.Profile)) dctx.Profile {
	p := dctx.Lite()
	f(&p)
	return p
}

// withCtxProfile runs the engine of a test on a context profile.
func withCtxProfile(p dctx.Profile) envOpt { return withConfig(Config{Profile: p}) }

// ctxBuilderFor returns the context builder of a profile, for the tests that call
// buildProposal directly.
func ctxBuilderFor(t testing.TB, p dctx.Profile) *dctx.Builder {
	t.Helper()
	b, err := dctx.New(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ctxLiteCap is the Lite prompt cap: the bound the efficiency figures are checked against.
var ctxLiteCap = dctx.Lite().MaxPromptTokens

// ---- reading what the model was sent ----

// ctxLastProposalPrompt is the newest proposal request that reached the model.
func ctxLastProposalPrompt(t testing.TB, e *env) pfake.Prompt {
	t.Helper()
	ps := e.prompts()
	for i := len(ps) - 1; i >= 0; i-- {
		if ps[i].Chat.Output.Kind() == providers.ConstraintJSONSchema {
			return ps[i]
		}
	}
	t.Fatal("no proposal request reached the model")
	return pfake.Prompt{}
}

// ctxMessageWith returns the first message of a request that starts with the given fence tag.
func ctxMessageWith(p pfake.Prompt, tag string) (string, bool) {
	for _, m := range p.Chat.Messages {
		if strings.HasPrefix(m.Content, "<"+tag+">") {
			return m.Content, true
		}
	}
	return "", false
}

// ctxSeedState writes a session state (and a turn row, so the session has a history) the way a
// finished conversation would have left it, without playing the conversation.
func ctxSeedState(t testing.TB, e *env, sc contracts.Scope, f func(st *sessions.State, now time.Time)) {
	t.Helper()
	sess := e.session(sc)
	f(&sess.State, e.clk.Now())
	rec := sessions.TurnRecord{TurnID: "trn_seed", User: "an earlier request about items", Act: "present_result", Assistant: "Here is what I found about the items."}
	if _, err := e.store.Save(ctxb(), sess, &rec); err != nil {
		t.Fatal(err)
	}
}

func ctxSeedGoal(t testing.TB, capability, phase string, now time.Time, slots map[string]any) *sessions.Goal {
	t.Helper()
	g := &sessions.Goal{CapabilityID: capability, Phase: phase, Slots: map[string]sessions.Slot{}, StartedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	for k, v := range slots {
		if _, err := g.SetSlot(k, v, sessions.OriginUserTurn, sessions.SlotMeta{TurnID: "trn_seed"}); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// SEC-007 neighbourhood: what the model may see of the state is decided by the policy
// gateway, per capability and per field, and by nothing else.
func TestEngineVisibleComesFromPolicyDecisions(t *testing.T) {
	const (
		secretNote  = "SECRET-NOTE-9921"
		secretGoal  = "SECRET-GOAL-NOTE-4417"
		secretSusp  = "SECRET-SUSPENDED-3302"
		adminFact   = "ADMIN-ONLY-FACT-7710"
		ghostFact   = "GHOST-FACT-2288"
		seedReceipt = "rcpt_SEEDRECEIPT"
		seedGoalID  = "goal_SEEDGOAL"
	)
	seed := func(st *sessions.State, now time.Time) {
		g := ctxSeedGoal(t, capCreate, "open", now, map[string]any{"name": "widget", "notes": secretGoal})
		g.Pending = &sessions.Clarification{Field: "quantity", Kind: sessions.ClarifyText}
		st.Goal = g
		sus := ctxSeedGoal(t, capCreate, "open", now, map[string]any{"name": "gizmo", "notes": secretSusp})
		sus.SuspendReason = sessions.SuspendedTopicSwitch
		adminSus := ctxSeedGoal(t, capAdmin, "open", now, map[string]any{"reason": adminFact})
		adminSus.SuspendReason = sessions.SuspendedTopicSwitch
		st.Suspended = []*sessions.Goal{adminSus, sus}
		st.Capsules = []sessions.Capsule{
			{GoalID: seedGoalID, CapabilityID: capCreate, Outcome: sessions.OutcomeSuccess, Fields: []string{"name", "notes", "quantity"},
				Facts: map[string]string{"name": "widget", "notes": secretNote, "quantity": "3"}, ReceiptID: seedReceipt},
			{CapabilityID: capAdmin, Outcome: sessions.OutcomeSuccess, Fields: []string{"reason"}, Facts: map[string]string{"reason": adminFact}},
			{CapabilityID: "ghost.thing", Outcome: sessions.OutcomeSuccess, Fields: []string{"ghost"}, Facts: map[string]string{"ghost": ghostFact}},
			// Allowed for the member and on the shortlist of the turn: the router's own decision is used.
			{CapabilityID: capCount, Outcome: sessions.OutcomeSuccess},
		}
	}
	newScenario := func(t *testing.T) *env {
		e := newEnv(t, withLevel(level2), withCount(), noLimit())
		e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
		e.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(proposalJSON(capGet, "task", map[string]any{"item_id": "item_1"}))
		e.onAct("present_result", "Here is what I found. Name: Alpha widget.")
		e.session(ctxWideScope()) // open it, so there is a session to seed
		ctxSeedState(t, e, ctxWideScope(), seed)
		return e
	}

	t.Run("what the policy allows is shown, what it withholds is not", func(t *testing.T) {
		e := newScenario(t)
		before := len(e.recs.recs)
		res := e.say(ctxWideScope(), "show me item item_1")
		wantAct(t, res, "present_result")
		state, ok := ctxMessageWith(ctxLastProposalPrompt(t, e), "session_state")
		if !ok {
			t.Fatal("the proposal prompt has no session state")
		}

		// Visible: the goal and the fields of it the projection lets through, the capsule and
		// the goal set aside of an allowed capability.
		mustContain(t, state, "Goal: items.create (open)", "the state")
		mustContain(t, state, "Collected: name=widget", "the state")
		mustContain(t, state, "Waiting for: quantity (text)", "the state")
		mustContain(t, state, "- items.create: success; fields: name, quantity; kept: name=widget, quantity=3", "the capsule line")
		mustContain(t, state, "- items.create (topic_switch) collected: name", "the set-aside line")
		mustContain(t, state, "- items.count: success", "a capsule of a shortlisted capability")

		// Withheld: the sensitive field (policy projection), whatever holds a value for it.
		for _, s := range []string{"notes", secretNote, secretGoal, secretSusp} {
			mustNotContain(t, state, s, "the state (a field the policy withholds)")
		}
		// Denied or unknown capabilities are not even named, in a capsule or a goal set aside.
		for _, s := range []string{capAdmin, adminFact, "ghost.thing", ghostFact} {
			mustNotContain(t, state, s, "the state (a capability the caller may not use or that does not exist)")
		}
		// And no identifier of the state reaches the prompt.
		for _, s := range []string{seedGoalID, seedReceipt, "goal_", "rcpt_", "prep_"} {
			mustNotContain(t, e.allPromptText(), s, "the prompt")
		}

		// The capabilities that only the state mentions got an exposure decision of their own,
		// once each however many times the state names them: the gateway is the source of the
		// answer, not a list in the engine.
		decided := map[string]int{}
		for _, r := range e.recs.recs[before:] {
			if r.Phase == policy.PhaseExpose && r.CapabilityID != "" {
				decided[r.CapabilityID]++
			}
		}
		for _, id := range []string{capCreate, capAdmin} {
			if decided[id] != 1 {
				t.Errorf("exposure decisions for %s this turn = %d, want exactly 1 (decided once, from the gateway); all = %v", id, decided[id], decided)
			}
		}
		// items.count was shortlisted: the decision the router made is the one that is used.
		if decided[capCount] != 0 {
			t.Errorf("the gateway was asked again about a shortlisted capability: %v", decided)
		}
		if decided["ghost.thing"] != 0 {
			t.Errorf("the gateway was asked about a capability the package does not declare: %v", decided)
		}
		if e.recs.denied(capAdmin, policy.PhaseExpose) == 0 {
			t.Error("the denial of the admin capability was not recorded")
		}
	})

	t.Run("a narrower token hides the capability from the state as it hides it from the frame", func(t *testing.T) {
		e := newScenario(t)
		e.say(scopeFor("member", capGet, capCount), "show me item item_1")
		p := ctxLastProposalPrompt(t, e)
		mustNotContain(t, strings.Join(p.Texts(), "\n"), capCreate, "a prompt of a caller whose token no longer names items.create (frame, goal, capsules, goals set aside)")
		mustNotContain(t, strings.Join(p.Texts(), "\n"), secretNote, "the prompt")
		// What the token still names is still shown.
		state, ok := ctxMessageWith(p, "session_state")
		if !ok {
			t.Fatal("no state")
		}
		mustContain(t, state, "- items.count: success", "the state")
	})

	t.Run("a nil Visible shows nothing", func(t *testing.T) {
		in := demoPromptInput(nil, nil)
		in.State.Capsules = []sessions.Capsule{{CapabilityID: "a.one", Outcome: sessions.OutcomeSuccess, Fields: []string{"x"}, Facts: map[string]string{"x": "CAPSULE-VALUE"}}}
		in.State.Suspended = []*sessions.Goal{{CapabilityID: "a.one", Slots: map[string]sessions.Slot{"x": {Value: "SUSPENDED-VALUE"}}}}
		in.Visible = nil
		built, err := buildProposal(context.Background(), ctxBuilderFor(t, dctx.Lite()), memberScope(), in)
		if err != nil {
			t.Fatal(err)
		}
		all := ""
		for _, it := range built.Req.Items {
			if it.Meta.Kind == ItemState {
				t.Fatalf("a state message was built without a Visible function: %q", it.Content)
			}
			all += it.Content + "\n"
		}
		for _, s := range []string{"a.one: success", "CAPSULE-VALUE", "SUSPENDED-VALUE", "x=v", "Goal:"} {
			mustNotContain(t, all, s, "a prompt built with a nil Visible")
		}
	})
}

// The field a goal waits for is a task field of that goal's capability only: it does not unlock
// a sensitive field of the same name on another capability of the shortlist.
func TestPendingFieldUnlocksOnlyTheGoalsOwnCapability(t *testing.T) {
	seedPending := func(t *testing.T, e *env, capability string, slots map[string]any) {
		ctxSeedState(t, e, memberScope(), func(st *sessions.State, now time.Time) {
			g := ctxSeedGoal(t, capability, "open", now, slots)
			g.Pending = &sessions.Clarification{Field: "notes", Kind: sessions.ClarifyText}
			st.Goal = g
		})
	}

	t.Run("another capability's sensitive field stays withheld", func(t *testing.T) {
		e := newEnv(t)
		e.session(memberScope())
		seedPending(t, e, capGet, nil)
		e.proposeOnce(capCreate, "task", map[string]any{"name": "widget", "quantity": 3, "notes": "SECRET-NOTE-7731"})
		e.say(memberScope(), sayCreate)

		p := ctxLastProposalPrompt(t, e)
		mustNotContain(t, string(p.Chat.Output.JSONSchema), "notes", "the output constraint (items.create's notes is not a task field)")
		for _, fields := range e.sc.prepared {
			if _, ok := fields["notes"]; ok {
				t.Fatalf("prepared with %v: a withheld field reached the host", fields)
			}
		}
	})

	t.Run("the goal's own capability still takes the answer", func(t *testing.T) {
		e := newEnv(t)
		e.session(memberScope())
		seedPending(t, e, capCreate, map[string]any{"name": "widget", "quantity": 3})
		e.proposeOnce(capCreate, "task", map[string]any{"notes": "fragile"})
		e.say(memberScope(), "the note is fragile")

		mustContain(t, string(ctxLastProposalPrompt(t, e).Chat.Output.JSONSchema), "notes", "the output constraint (the field the goal asks for)")
		if s, ok := e.goal(memberScope()).Slots["notes"]; !ok || s.Value != "fragile" {
			t.Fatalf("notes = %+v, %v: the answer to the pending question was dropped", s, ok)
		}
	})
}

// ctxWideScope is a member whose token names every capability of the demo package, items.count
// and the admin capability included (the role grants still decide: members hold neither the
// admin grant nor, without withCount, items.count).
func ctxWideScope() contracts.Scope { return scopeFor("member", capGet, capCreate, capAdmin, capCount) }

// ---- the pieces of an env, to build a second engine from them ----

func ctxPackagesOf(e *env) func(context.Context, string) (capabilities.Package, error) {
	return func(context.Context, string) (capabilities.Package, error) { return e.currentPkg(), nil }
}

func ctxAdaptersOf(e *env) actions.AdapterSource {
	return actions.AdapterSourceFunc(func(context.Context, string) (adapters.AppAdapter, error) { return e.guard, nil })
}

func ctxLocalSelectorOf(t testing.TB, e *env) ProviderSelector {
	t.Helper()
	sel, err := LocalSelector(e.model, e.budget)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}
