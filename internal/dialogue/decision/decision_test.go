package decision

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	core "github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/internal/decision/lifecycle"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	actgen "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
	conv "github.com/buxee/alix-ai/tests/conversation"
)

// The utterances of the clinic's conversation corpus (tests/conversation).
const (
	sayBook     = "Book Luca Ferri for 2026-10-14 10:30" // shortlist: book, cancel, list
	sayExact    = "Book an appointment"                  // exactly the title of appointments.book
	sayOutside  = "Tell me a joke about penguins"
	sayParaCanc = "scrap the whole thing"
	sayParaYes  = "yes that looks right to me"
)

func recep() contracts.Scope { return conv.RoleScope(conv.RoleReceptionist) }

// pick scripts the scorer to select one candidate with a calibrated confidence.
func pick(id string, conf float64) fakescorer.Step {
	return fakescorer.Step{Picks: []fakescorer.Pick{{ID: id, Score: 0.9, Confidence: conf}}}
}

// proposalCaps is how many capabilities the (first) proposal prompt listed.
func proposalCaps(t *testing.T, res dialogue.Result) int {
	t.Helper()
	for _, c := range res.Trace.ModelCalls {
		if c.Purpose == "proposal" {
			return c.Capabilities
		}
	}
	t.Fatalf("no proposal call in %+v", res.Trace.ModelCalls)
	return 0
}

func hasStage(res dialogue.Result, name string) bool {
	for _, s := range res.Trace.Stages {
		if s == name {
			return true
		}
	}
	return false
}

// ---- exact deterministic rules ----

func TestExactDeterministicCaseSkipsScorer(t *testing.T) {
	t.Run("the exact title of a shortlisted capability is routed with no scorer", func(t *testing.T) {
		w := newWorld(t)
		w.propose("appointments.book", "task", map[string]any{})

		res := w.say(recep(), sayExact)

		if res.Trace.Decision != "selected" {
			t.Fatalf("decision = %q, want selected", res.Trace.Decision)
		}
		if n := w.scorer.Calls(); n != 0 {
			t.Fatalf("the scorer was called %d times for an exact title", n)
		}
		if n := w.rec.total(); n != 0 {
			t.Fatalf("%d decision audit events for a turn the rules decided", n)
		}
		if got, want := proposalCaps(t, res), 1; got != want {
			t.Fatalf("the proposal prompt lists %d capabilities, want %d (narrowed to the exact one)", got, want)
		}
		if st := w.layer.Stats(); st.ExactSelected != 1 || st.EngineRequests != 0 {
			t.Fatalf("stats = %+v", st)
		}
	})

	t.Run("an exact capability id is routed with no scorer", func(t *testing.T) {
		w := newWorld(t)
		res := w.layer.Stage().Decide(ctxb(), dialogue.DecisionRequest{
			Scope: recep(), Utterance: "appointments.book", Language: "en",
			Candidates: []dialogue.DecisionCandidate{{CapabilityID: "availability.list"}, {CapabilityID: "appointments.book"}},
		})
		if res.Selected == nil || res.Selected.CapabilityID != "appointments.book" || res.Fallback != dialogue.FallbackNone {
			t.Fatalf("result = %+v", res)
		}
		if w.scorer.Calls() != 0 {
			t.Fatal("the scorer was called for an exact id")
		}
	})

	t.Run("an exact id is still re-resolved: a capability the gateway does not show is refused", func(t *testing.T) {
		w := newWorld(t)
		res := w.layer.Stage().Decide(ctxb(), dialogue.DecisionRequest{
			Scope: conv.RoleScope(conv.RoleDoctor), Utterance: "appointments.book", Language: "en",
			Candidates: []dialogue.DecisionCandidate{{CapabilityID: "appointments.book"}},
		})
		if res.Selected != nil || res.Fallback != dialogue.FallbackPolicyConflict {
			t.Fatalf("result = %+v, want policy_conflict and no selection", res)
		}
	})

	t.Run("exact control phrases decide with no scorer, with or without a pending question", func(t *testing.T) {
		w := newWorld(t)
		w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri"})
		first := w.say(recep(), sayExact)
		wantAct(t, first, "ask_missing")
		proposals := w.proposals()

		res := w.say(recep(), "never mind")

		wantAct(t, res, "acknowledge_cancel")
		if w.proposals() != proposals || w.scorer.Calls() != 0 {
			t.Fatalf("an exact cancel phrase reached the controller (%d proposals) or the scorer (%d calls)", w.proposals()-proposals, w.scorer.Calls())
		}
		if st := w.layer.Stats(); st.ControlRuleHits != 1 || st.ControllerCallsAvoidedByRules != 1 {
			t.Fatalf("stats = %+v", st)
		}
	})

	t.Run("ExactCapability is exact: case and punctuation aside, never inside a sentence, never ambiguous", func(t *testing.T) {
		cs := []Named{
			{ID: "appointments.book", Names: []string{"appointments.book", "Book an appointment"}},
			{ID: "appointments.cancel", Names: []string{"appointments.cancel", "Cancel an appointment"}},
			{ID: "twin.one", Names: []string{"Same name"}}, {ID: "twin.two", Names: []string{"same  NAME!"}},
		}
		for _, c := range []struct {
			say, want string
		}{
			{"Book an appointment", "appointments.book"},
			{"  book AN appointment!  ", "appointments.book"},
			{"please book an appointment", "appointments.book"},
			{"appointments.cancel", "appointments.cancel"},
			{"Book an appointment for Luca", ""},
			{"book", ""},
			{"same name", ""},
			{"", ""},
		} {
			got, ok := ExactCapability(c.say, cs)
			if got != c.want || ok != (c.want != "") {
				t.Errorf("ExactCapability(%q) = %q, %v; want %q", c.say, got, ok, c.want)
			}
		}
	})
}

// ---- the scorer's selection is re-resolved ----

func TestScorerSelectionIsReresolvedByAdapter(t *testing.T) {
	t.Run("an accepted selection narrows the controller's prompt and is recorded as resolved", func(t *testing.T) {
		w := newWorld(t, withScript(pick("availability.list", 0.97)))
		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})

		res := w.say(recep(), sayBook)

		if res.Trace.Decision != "selected" {
			t.Fatalf("decision = %q, want selected", res.Trace.Decision)
		}
		if w.scorer.Calls() != 1 {
			t.Fatalf("scorer calls = %d, want 1", w.scorer.Calls())
		}
		if got, want := proposalCaps(t, res), 1; got != want {
			t.Fatalf("the prompt lists %d capabilities, want %d", got, want)
		}
		if len(res.Trace.Shortlist) != 3 {
			t.Fatalf("shortlist = %v, want the router's three", res.Trace.Shortlist)
		}
		for typ, want := range map[string]int{"decision.invoked": 1, "decision.accepted": 1, "decision.final_action": 1, "decision.fallback": 0} {
			if got := w.rec.count(typ); got != want {
				t.Errorf("%s events = %d, want %d", typ, got, want)
			}
		}
		if st := w.layer.Stats(); st.ScorerSelected != 1 || st.PromptCapabilitiesSaved != 2 {
			t.Fatalf("stats = %+v, want one selection that saved two capability descriptions", st)
		}
	})

	t.Run("when the application's state changes before the re-resolution the selection is refused", func(t *testing.T) {
		w := newWorld(t)
		// The scorer answers, and while it does the package is pulled: Resolve asks the
		// policy gateway again, against the state as it is now, and the gateway shows nothing.
		w.scorer.Script(fakescorer.Step{
			Picks:  []fakescorer.Pick{{ID: "availability.list", Score: 0.9, Confidence: 0.99}},
			Mutate: func(*gen.DecisionScoreResult) { w.quarantineNext = true },
		})
		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})

		res := w.say(recep(), sayBook)

		if res.Trace.Decision != string(dialogue.FallbackPolicyConflict) {
			t.Fatalf("decision = %q, want policy_conflict", res.Trace.Decision)
		}
		if got, want := proposalCaps(t, res), 3; got != want {
			t.Fatalf("the controller was offered %d capabilities, want the full shortlist of %d", got, want)
		}
		if got := w.rec.count("decision.final_action"); got != 1 {
			t.Fatalf("final_action events = %d, want the refusal recorded", got)
		}
	})

	t.Run("an id the scorer invented is never used", func(t *testing.T) {
		w := newWorld(t, withScript(pick("admin.purge", 0.99)))
		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})

		res := w.say(recep(), sayBook)

		if res.Trace.Decision != string(dialogue.FallbackMalformedOutput) {
			t.Fatalf("decision = %q, want malformed_output", res.Trace.Decision)
		}
		if got := proposalCaps(t, res); got != 3 {
			t.Fatalf("the controller was offered %d capabilities, want 3", got)
		}
	})
}

func TestLowConfidenceFallsBackToController(t *testing.T) {
	scripted := func(opts ...worldOpt) (*world, dialogue.Result) {
		w := newWorld(t, opts...)
		w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"})
		return w, w.say(recep(), sayBook)
	}
	w, res := scripted(withScript(pick("availability.list", 0.40)))
	if res.Trace.Decision != string(dialogue.FallbackLowConfidence) {
		t.Fatalf("decision = %q, want low_confidence", res.Trace.Decision)
	}
	if got := proposalCaps(t, res); got != 3 {
		t.Fatalf("the controller was offered %d capabilities, want the full shortlist", got)
	}
	if w.rec.count("decision.fallback") != 1 || w.rec.count("decision.final_action") != 1 {
		t.Fatalf("the fallback and its final action must be recorded: %v", w.rec.types)
	}
	if st := w.layer.Stats(); st.Fallbacks["low_confidence"] != 1 {
		t.Fatalf("stats = %+v", st)
	}
	// The outcome is the controller's, exactly as without the layer.
	base, bres := scripted(withoutLayer())
	if d := cmp.Diff(base.shapeOf(recep(), bres), w.shapeOf(recep(), res)); d != "" {
		t.Fatalf("outcome differs from the controller-only path:\n%s", d)
	}
}

// ---- the scorer decides nothing that matters ----

func TestScorerCannotAuthorizeOrCommit(t *testing.T) {
	t.Run("a selected consequential capability is only prepared, and never committed without the client's token", func(t *testing.T) {
		w := newWorld(t, withScript(pick("appointments.book", 0.99)))
		w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"})

		res := w.say(recep(), sayBook)

		if res.Trace.Decision != "selected" {
			t.Fatalf("decision = %q", res.Trace.Decision)
		}
		wantAct(t, res, "request_confirmation")
		if res.Prepared == nil || res.Receipt != nil {
			t.Fatalf("prepared = %v, receipt = %v", res.Prepared, res.Receipt)
		}
		w.noCommits()
	})

	t.Run("a scorer 'yes' is not a confirmation: the pending action is presented again and nothing commits", func(t *testing.T) {
		w := newWorld(t, withScript(pick("appointments.book", 0.99)))
		w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"})
		prep := w.say(recep(), sayBook)
		wantAct(t, prep, "request_confirmation")
		w.scorer.Script(pick(actConfirm.id(), 0.99))

		res := w.say(recep(), sayParaYes)

		if st := w.layer.Stats(); st.ControlScorerHits != 1 {
			t.Fatalf("the scorer's confirm was not used: %+v", st)
		}
		if res.Receipt != nil {
			t.Fatalf("a scorer-detected yes committed: %+v", res.Receipt)
		}
		w.noCommits()
		if g := w.goal(recep()); g == nil || g.PreparedID == "" {
			t.Fatalf("the pending preparation is gone: %+v", g)
		}
	})

	t.Run("a scorer 'reject' or 'cancel' never declines the pending preparation: the controller decides, as with the layer off", func(t *testing.T) {
		const sayUnsure = "hmm, on second thought"
		for _, a := range []act{actReject, actCancel} {
			asked := false
			run := func(opts ...worldOpt) (*world, dialogue.Result, dialogue.Result) {
				w := newWorld(t, opts...)
				w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"})
				prep := w.say(recep(), sayBook)
				wantAct(t, prep, "request_confirmation")
				if w.scorer != nil {
					step := pick(a.id(), 0.99)
					step.Mutate = func(r *gen.DecisionScoreResult) { asked = asked || string(r.ContractVersion) == ContractAct }
					w.scorer.Script(step)
				}
				w.propose("", "out_of_scope", nil)
				return w, prep, w.say(recep(), sayUnsure)
			}
			w, prep, res := run(withScript(pick("appointments.book", 0.99)))
			base, _, bres := run(withoutLayer())

			if !asked {
				t.Fatalf("%s: the scorer was never asked about the control act, so the check proves nothing", a)
			}
			if st := w.layer.Stats(); st.ControlScorerHits != 0 {
				t.Fatalf("%s: the scorer's advice was taken as a control act: %+v", a, st)
			}
			if string(res.Directive.Act) == "acknowledge_cancel" {
				t.Fatalf("%s: the scorer alone declined the pending preparation", a)
			}
			if g := w.goal(recep()); g == nil || g.PreparedID != string(prep.Prepared.PreparationID) {
				t.Fatalf("%s: the pending preparation is gone: %+v", a, g)
			}
			if d := cmp.Diff(base.shapeOf(recep(), bres), w.shapeOf(recep(), res)); d != "" {
				t.Fatalf("%s: outcome differs from the controller-only path:\n%s", a, d)
			}
			if done := w.confirm(recep(), prep); done.Receipt == nil {
				t.Fatalf("%s: the preparation is no longer confirmable: %+v", a, done.Directive)
			}
		}
	})

	t.Run("a result that selects an id outside the set it was given is malformed, never an action", func(t *testing.T) {
		w := newWorld(t, withScript(pick(actConfirm.id(), 0.99)))
		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})
		res := w.say(recep(), sayBook)
		if res.Trace.Decision != string(dialogue.FallbackMalformedOutput) {
			t.Fatalf("decision = %q", res.Trace.Decision)
		}
	})

	t.Run("the advice types offer no way to authorise, validate, prepare, confirm or commit", func(t *testing.T) {
		for _, v := range []any{core.Advice{}, core.Resolution{}, core.ResolvedSelection{}, core.Suggestion{}} {
			ty := reflect.TypeOf(v)
			for i := 0; i < ty.NumMethod(); i++ {
				n := strings.ToLower(ty.Method(i).Name)
				for _, bad := range []string{"authori", "commit", "confirm", "prepare", "approve", "execute", "validate", "send", "delete"} {
					if strings.Contains(n, bad) {
						t.Errorf("%s has method %s", ty, ty.Method(i).Name)
					}
				}
			}
		}
	})
}

func TestScorerCannotLowerRiskOrSkipConfirmation(t *testing.T) {
	t.Run("a confident scorer does not change the confirmation a consequential action needs", func(t *testing.T) {
		run := func(opts ...worldOpt) (*world, dialogue.Result) {
			w := newWorld(t, opts...)
			w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"})
			return w, w.say(recep(), sayBook)
		}
		w, with := run(withScript(pick("appointments.book", 0.999)))
		base, without := run(withoutLayer())
		if with.Trace.Decision != "selected" {
			t.Fatalf("decision = %q", with.Trace.Decision)
		}
		wantAct(t, with, "request_confirmation")
		if d := cmp.Diff(base.shapeOf(recep(), without), w.shapeOf(recep(), with)); d != "" {
			t.Fatalf("outcome differs from the controller-only path:\n%s", d)
		}
		if with.Prepared == nil || without.Prepared == nil || with.Prepared.RiskLevel != without.Prepared.RiskLevel {
			t.Fatalf("risk with scorer = %+v, without = %+v", with.Prepared, without.Prepared)
		}
		w.noCommits()
	})

	t.Run("a low-risk selection does not let the controller reach a high-risk capability", func(t *testing.T) {
		w := newWorld(t, withScript(pick("availability.list", 0.99)))
		// The controller, narrowed to the read, proposes the booking anyway.
		w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"})

		res := w.say(recep(), sayBook)

		if res.Prepared != nil || res.Receipt != nil {
			t.Fatalf("a booking was prepared from a proposal outside the narrowed set: %+v", res.Prepared)
		}
		if prepares, _ := w.counter.counts(); prepares != 0 {
			t.Fatalf("the host saw %d Prepare calls", prepares)
		}
		w.noCommits()
	})
}

func TestOutOfScopeScoreStillRefusedByDeterministicChecks(t *testing.T) {
	t.Run("a request outside the application is refused by the router; the scorer is never asked", func(t *testing.T) {
		w := newWorld(t, withScript(pick("appointments.book", 0.99)))
		res := w.say(recep(), sayOutside)
		if res.Code != contracts.ErrUnknownCapability {
			t.Fatalf("code = %q (act %s), want unknown_capability", res.Code, res.Directive.Act)
		}
		if w.scorer.Calls() != 0 || w.proposals() != 0 {
			t.Fatalf("scorer calls = %d, proposals = %d, want none", w.scorer.Calls(), w.proposals())
		}
	})

	t.Run("a high score for a capability the caller may not use never gets as far as the scorer", func(t *testing.T) {
		w := newWorld(t, withScript(pick("appointments.book", 0.999)))
		res := w.layer.Stage().Decide(ctxb(), dialogue.DecisionRequest{
			Scope: conv.RoleScope(conv.RoleDoctor), Utterance: sayBook, Language: "en",
			Candidates: []dialogue.DecisionCandidate{{CapabilityID: "appointments.book"}, {CapabilityID: "availability.list"}},
		})
		if res.Selected != nil || res.Fallback != dialogue.FallbackCandidateSetReject {
			t.Fatalf("result = %+v", res)
		}
		if w.scorer.Calls() != 0 {
			t.Fatal("the scorer saw a candidate the policy gateway does not show the caller")
		}
	})

	t.Run("the engine keeps validating the selected capability itself", func(t *testing.T) {
		// A selection is only a narrowing: a proposal for something else is refused by the
		// engine's own checks, not accepted because the scorer liked another capability.
		w := newWorld(t, withScript(pick("availability.list", 0.99)))
		w.propose("patients.update_contact", "task", map[string]any{"patient": "Luca Ferri", "email": "x@example.test"})
		res := w.say(recep(), sayBook)
		if res.Prepared != nil || res.Receipt != nil {
			t.Fatalf("outcome = %+v", res)
		}
	})
}

func TestUnauthorizedCandidateNeverReachesScorer(t *testing.T) {
	cases := []struct {
		name string
		role string
		id   string
	}{
		{"a capability the policy gateway hides from the role", conv.RoleDoctor, "appointments.book"},
		{"a capability the package does not declare", conv.RoleReceptionist, "admin.purge"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			res := w.layer.Stage().Decide(ctxb(), dialogue.DecisionRequest{
				Scope: conv.RoleScope(c.role), Utterance: "do the thing for Luca please", Language: "en",
				Candidates: []dialogue.DecisionCandidate{{CapabilityID: "availability.list"}, {CapabilityID: c.id}},
			})
			if res.Selected != nil || res.Fallback != dialogue.FallbackCandidateSetReject {
				t.Fatalf("result = %+v, want candidate_set_rejected", res)
			}
			if n := w.scorer.Calls(); n != 0 {
				t.Fatalf("the scorer was called %d times with an unauthorised candidate in the set", n)
			}
			if w.layer.Stats().RejectCodes[string(core.RejectUnauthorized)] != 1 {
				t.Fatalf("stats = %+v", w.layer.Stats())
			}
			if n := w.rec.count("decision.invoked"); n != 0 {
				t.Fatalf("the invocation was recorded before the guard passed")
			}
		})
	}

	t.Run("a candidate set built for another version of the package is stale", func(t *testing.T) {
		w := newWorld(t)
		ctx := withExposureMemo(ctxb())
		stale := actgen.OpaqueVersion("some-other-package-hash")
		err := w.layer.res.Exposed(ctx, recep(), gen.CandidateSetCandidate{CandidateID: "availability.list", Label: "x", DataVersion: &stale})
		if !contracts.HasCode(err, contracts.ErrStale) {
			t.Fatalf("err = %v, want stale", err)
		}
	})
}

// ---- off and broken give the controller's outcomes ----

// conversation is the scripted exchange the disabled and unavailable runs replay.
func conversationTurns(w *world) []shape {
	sc := recep()
	var out []shape
	for _, step := range []struct {
		propose func()
		say     string
	}{
		{func() { w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri"}) }, sayBook},
		{nil, "never mind"},
		{func() { w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"}) }, "Which appointments are free on 2026-10-14?"},
		{nil, sayOutside},
		{func() {
			w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"})
		}, sayExact},
	} {
		if step.propose != nil {
			step.propose()
		}
		res := w.say(sc, step.say)
		out = append(out, w.shapeOf(sc, res))
	}
	return out
}

func TestDisabledScorerGivesSameOutcomesViaController(t *testing.T) {
	base := newWorld(t, withoutLayer())
	off := newWorld(t, withDisabledLayer(), withScript(pick("availability.list", 0.99)))

	want := conversationTurns(base)
	got := conversationTurns(off)

	if d := cmp.Diff(want, got); d != "" {
		t.Fatalf("a disabled layer changed an outcome:\n%s", d)
	}
	if base.proposals() != off.proposals() {
		t.Fatalf("proposal calls: baseline %d, disabled layer %d", base.proposals(), off.proposals())
	}
	if off.scorer.Calls() != 0 || off.rec.total() != 0 {
		t.Fatalf("a disabled layer called the scorer %d times and wrote %d events", off.scorer.Calls(), off.rec.total())
	}
	st := off.layer.Stats()
	if st.EngineRequests != 0 || st.ExactSelected != 0 || st.Fallbacks["disabled"] == 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestScorerUnavailableGivesSameOutcomesViaController(t *testing.T) {
	want := conversationTurns(newWorld(t, withoutLayer()))
	for _, c := range []struct {
		name   string
		opts   []worldOpt
		reason dialogue.FallbackReason
	}{
		{"no scorer in the router role", []worldOpt{withNoScorer()}, dialogue.FallbackUnavailable},
		{"the router role holds a text model", []worldOpt{withTextRouter()}, dialogue.FallbackUnsupportedRuntime},
		{"the scorer is down", []worldOpt{withScript(fakescorer.Step{Fail: fakescorer.FailUnavailable})}, dialogue.FallbackUnavailable},
		{"the scorer times out", []worldOpt{withScript(fakescorer.Step{Fail: fakescorer.FailTimeout})}, dialogue.FallbackTimeout},
		{"the scorer returns malformed output", []worldOpt{withScript(fakescorer.Step{Fail: fakescorer.FailMalformed})}, dialogue.FallbackMalformedOutput},
		{"the scorer cannot load", []worldOpt{withScript(fakescorer.Step{Fail: fakescorer.FailResourceExhausted})}, dialogue.FallbackResourceGate},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t, c.opts...)
			got := conversationTurns(w)
			if d := cmp.Diff(want, got); d != "" {
				t.Fatalf("outcomes differ from the controller-only path:\n%s", d)
			}
			if n := w.layer.Stats().Fallbacks[string(c.reason)]; n == 0 {
				t.Fatalf("the typed reason %s was not recorded: %+v", c.reason, w.layer.Stats())
			}
		})
	}

	t.Run("a hung scorer is cut off before the dialogue engine's own deadline", func(t *testing.T) {
		w := newWorld(t, withScript(fakescorer.Step{Hang: true}), withCore(func(c *core.Config) { c.Timeout = 5_000_000_000 }))
		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})
		res := w.say(recep(), sayBook)
		if res.Trace.Decision != string(dialogue.FallbackTimeout) {
			t.Fatalf("decision = %q, want timeout", res.Trace.Decision)
		}
		if got := proposalCaps(t, res); got != 3 {
			t.Fatalf("the controller was offered %d capabilities, want 3", got)
		}
		// The engine's inner call ended first, so nothing was recorded as accepted.
		if w.rec.count("decision.accepted") != 0 {
			t.Fatal("an acceptance was recorded for a selection the pipeline had given up on")
		}
	})
}

// ---- the controller still talks ----

func TestControllerStillRealizesNaturalLanguage(t *testing.T) {
	const wantRef = "APT-Q00001"
	outcome := "Done. Luca Ferri is booked with Dr. Anna Verdi on 2026-10-14 10:30 at Clinic North. The appointment reference is " + wantRef + "."
	w := newWorld(t, withModelResponses(), withScript(pick("appointments.book", 0.99)))
	w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"})
	prep := w.say(recep(), sayBook)
	wantAct(t, prep, "request_confirmation")
	if prep.Trace.Decision != "selected" {
		t.Fatalf("decision = %q: the scorer path was not taken", prep.Trace.Decision)
	}
	w.phrase(outcome)

	out := w.confirm(recep(), prep)

	wantAct(t, out, "report_outcome")
	if out.Response.Source != verify.SourceModel || out.Response.Text != outcome {
		t.Fatalf("response = %+v, want the controller's own verified words", out.Response)
	}
	responses := 0
	for _, c := range out.Trace.ModelCalls {
		if c.Purpose == "response" {
			responses++
		}
	}
	if responses != 1 {
		t.Fatalf("response calls of the turn = %d (%+v), want the controller's one", responses, out.Trace.ModelCalls)
	}
	// The scorer produced no prose, and the facts in the words are the host's.
	if out.Receipt == nil || out.Receipt.Status != actgen.ActionReceiptStatusCommitted {
		t.Fatalf("receipt = %+v", out.Receipt)
	}
	raw, _ := json.Marshal(w.scorer.LastRequest())
	if strings.Contains(string(raw), "Clinic North") || strings.Contains(string(raw), wantRef) {
		t.Fatalf("the scorer request carries host facts: %s", raw)
	}
	for _, c := range w.scorer.LastRequest().CandidateSet.Candidates {
		if c.Text != nil {
			t.Fatalf("a candidate carries text: %q", *c.Text)
		}
	}
}

func TestControllerCallsAvoidedAreCounted(t *testing.T) {
	// Turn 1 opens a goal (exact title, no scorer). Turn 2 is a cancel the exact tables do
	// not know: the scorer reads it, the engine takes the transition and the controller
	// is never called. Turn 3 and 4 are an exact phrase and an ordinary request.
	w := newWorld(t, withScript(pick(actCancel.id(), 0.97)))
	w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri"})
	wantAct(t, w.say(recep(), sayExact), "ask_missing")
	proposals := w.proposals()

	res := w.say(recep(), sayParaCanc)

	wantAct(t, res, "acknowledge_cancel")
	if w.proposals() != proposals {
		t.Fatal("the controller was called for a cancel the scorer detected")
	}
	st := w.layer.Stats()
	if st.ControllerCallsAvoidedByScorer != 1 || st.GeneratedTokensAvoidedByScorer <= 0 || st.ControlScorerHits != 1 {
		t.Fatalf("stats = %+v, want one controller call avoided by the scorer", st)
	}

	w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri"})
	wantAct(t, w.say(recep(), sayExact), "ask_missing")
	wantAct(t, w.say(recep(), "never mind"), "acknowledge_cancel")
	st = w.layer.Stats()
	if st.ControllerCallsAvoidedByRules != 1 || st.ControllerCallsAvoided() != 2 || st.GeneratedTokensAvoided() != 2*w.layer.cfg.ProposalTokens {
		t.Fatalf("stats = %+v, want one call avoided by the rules and one by the scorer", st)
	}

	// With the scorer failing, the same paraphrase costs the controller call, with the
	// same outcome: that call is the one the scorer avoided.
	b := newWorld(t, withScript(fakescorer.Step{Fail: fakescorer.FailUnavailable}))
	b.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri"})
	b.say(recep(), sayExact)
	b.propose("", "cancel", nil)
	again := b.say(recep(), sayParaCanc)
	wantAct(t, again, "acknowledge_cancel")
	if b.proposals() != proposals+1 {
		t.Fatalf("proposals = %d, want %d: the fallback path pays the controller call", b.proposals(), proposals+1)
	}
	if got := b.layer.Stats().ControllerCallsAvoided(); got != 0 {
		t.Fatalf("avoided = %d, want 0 when the scorer is down", got)
	}
}

// ---- RAG stays with the controller ----

const ragSentinel = "The Model X kettle has a warranty of 24 months from the date of purchase."

type ragKnowledge struct{ asked []string }

func (k *ragKnowledge) Answer(_ context.Context, _ contracts.Scope, req rag.AnswerRequest) (rag.AnswerResult, error) {
	k.asked = append(k.asked, string(req.Question))
	section := "Section 2"
	return rag.AnswerResult{Value: rag.GroundedAnswer{
		Claims: []rag.GroundedAnswerClaim{{Text: ragSentinel, CitationIndexes: []int64{0}}},
		Citations: []conversation.Citation{{
			SourceID: actgen.SourceID(ids.New("src")), DisplayName: "Warranty terms", Version: "v7",
			Locator:      &conversation.CitationLocator{Section: &section},
			Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent,
			OpenRef: conversation.CitationOpenRef{Value: conversation.CitationOpenRefFile{Kind: "file", FileID: actgen.FileID(ids.New("fil"))}},
			Claims:  []string{ragSentinel},
		}},
		Notices: rag.RetrievalNotices{},
	}}, nil
}

func TestRAGOperationsNeverAnsweredByScorer(t *testing.T) {
	t.Run("a question no capability handles goes to the sources, never to the scorer", func(t *testing.T) {
		k := &ragKnowledge{}
		w := newWorld(t, withKnowledge(k), withScript(pick("appointments.book", 0.99)))

		res := w.say(recep(), sayOutside)

		wantAct(t, res, "present_result")
		if len(k.asked) != 1 || !strings.Contains(res.Response.Text, ragSentinel) {
			t.Fatalf("knowledge asked %v; response %q", k.asked, res.Response.Text)
		}
		if w.scorer.Calls() != 0 || w.rec.total() != 0 {
			t.Fatalf("the scorer was involved in a RAG turn: %d calls, %d events", w.scorer.Calls(), w.rec.total())
		}
	})

	t.Run("when the scorer routed the turn, the controller still hands the question to the sources and the scorer never sees the evidence", func(t *testing.T) {
		k := &ragKnowledge{}
		w := newWorld(t, withKnowledge(k), withScript(pick("appointments.book", 0.99)))
		w.propose("", "out_of_scope", nil)

		res := w.say(recep(), sayBook)

		if res.Trace.Decision != "selected" || w.scorer.Calls() != 1 {
			t.Fatalf("decision = %q, scorer calls = %d: the scorer was not on the path", res.Trace.Decision, w.scorer.Calls())
		}
		if len(k.asked) != 1 || !strings.Contains(res.Response.Text, ragSentinel) {
			t.Fatalf("knowledge asked %v; response %q", k.asked, res.Response.Text)
		}
		raw, _ := json.Marshal(w.scorer.LastRequest())
		mustNotContain(t, string(raw), ragSentinel, "the scorer request")
		mustNotContain(t, string(raw), "Warranty terms", "the scorer request")
	})

	t.Run("no decision type of this layer is a question-answering one", func(t *testing.T) {
		w := newWorld(t)
		for _, ty := range []gen.DecisionType{gen.DecisionTypeCapabilityRouting, gen.DecisionTypeDialogueAct} {
			if !w.layer.d.Core.SupportedTypes[0].Valid() || !ty.Valid() {
				t.Fatal("unexpected decision type")
			}
		}
		if len(testCore().SupportedTypes) != 2 {
			t.Fatalf("supported types = %v", testCore().SupportedTypes)
		}
	})
}

// ---- bounds ----

func TestLayerBounds(t *testing.T) {
	t.Run("a long utterance, a single candidate and an active goal never reach the scorer", func(t *testing.T) {
		w := newWorld(t, withScript(pick("availability.list", 0.99)))
		long := strings.Repeat("book ", 80)
		one := w.layer.Stage().Decide(ctxb(), dialogue.DecisionRequest{Scope: recep(), Utterance: long, Candidates: []dialogue.DecisionCandidate{{CapabilityID: "availability.list"}, {CapabilityID: "appointments.book"}}})
		single := w.layer.Stage().Decide(ctxb(), dialogue.DecisionRequest{Scope: recep(), Utterance: "list free slots", Candidates: []dialogue.DecisionCandidate{{CapabilityID: "availability.list"}}})
		for _, r := range []dialogue.DecisionResult{one, single} {
			if r.Selected != nil || r.Fallback != dialogue.FallbackOODUnsupportedShape {
				t.Fatalf("result = %+v, want ood_unsupported_shape", r)
			}
		}
		if w.scorer.Calls() != 0 {
			t.Fatal("the scorer was called")
		}
	})

	t.Run("control detectors do not ask the scorer about a long utterance or an empty session", func(t *testing.T) {
		w := newWorld(t, withScript(pick(actCancel.id(), 0.99)))
		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})
		w.say(recep(), "drop everything we were ever doing") // no goal: nothing to cancel
		if w.scorer.Calls() != 0 {
			t.Fatalf("scorer calls = %d with nothing to cancel", w.scorer.Calls())
		}
	})

	t.Run("the engine never receives nil collections or an oversized state", func(t *testing.T) {
		w := newWorld(t, withScript(pick("availability.list", 0.99)))
		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})
		w.say(recep(), sayBook)
		req := w.scorer.LastRequest()
		if req.AllowedNextActions == nil || req.BoundedState == nil || req.SchemaVersion != 1 || req.CandidateSet.MaxSelections != 1 {
			t.Fatalf("request = %+v", req)
		}
		raw, _ := json.Marshal(req)
		if strings.Contains(string(raw), "null") {
			t.Fatalf("the request carries a JSON null: %s", raw)
		}
	})
}

// ---- the metrics hook ----

// observerSpy records what the layer tells its metrics hook.
type observerSpy struct {
	mu      sync.Mutex
	advice  []core.Advice
	avoided int
}

func (o *observerSpy) ObserveAdvice(a core.Advice) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.advice = append(o.advice, a)
}

func (o *observerSpy) ControllerCallAvoided() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.avoided++
}

// *lifecycle.Metrics is the hook serve gives the layer.
var _ Observer = (*lifecycle.Metrics)(nil)

// Every Advice the layer's engine produces reaches the hook (an accepted one and a fallback one), the
// controller calls the scorer's detections avoided reach ControllerCallAvoided and the exact rules'
// do not, and a layer with no hook behaves the same. The hook sees no way to change the outcome.
func TestMetricsHookObservesEveryAdvice(t *testing.T) {
	t.Run("an accepted advice and a fallback advice are observed with their outcome", func(t *testing.T) {
		spy := &observerSpy{}
		engineRequests := 0
		for _, conf := range []float64{0.97, 0.40} {
			w := newWorld(t, withObserver(spy), withScript(pick("availability.list", conf)))
			w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})
			w.say(recep(), sayBook)
			engineRequests += w.layer.Stats().EngineRequests
		}

		spy.mu.Lock()
		defer spy.mu.Unlock()
		if got, want := len(spy.advice), engineRequests; got != want || got != 2 {
			t.Fatalf("advice observed = %d, engine requests = %d, want 2 and the same", got, want)
		}
		if !spy.advice[0].Accepted() {
			t.Errorf("the first advice (confidence 0.97) was not accepted")
		}
		if reason, fell := spy.advice[1].FallbackReason(); !fell || reason != gen.FallbackReasonLowConfidence {
			t.Errorf("the second advice = (%q, %v), want a low_confidence fallback", reason, fell)
		}
		if spy.avoided != 0 {
			t.Errorf("controller calls avoided = %d, want 0: a selection only narrows the prompt", spy.avoided)
		}
	})

	t.Run("a control act the scorer detected is a controller call avoided, one the rules caught is not", func(t *testing.T) {
		reg := lifecycle.NewMemoryRegistry()
		m := lifecycle.NewMetrics(reg, lifecycle.Labels{Profile: "lite"})
		w := newWorld(t, withObserver(m), withScript(pick(actCancel.id(), 0.97)))
		w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri"})
		wantAct(t, w.say(recep(), sayExact), "ask_missing")
		wantAct(t, w.say(recep(), sayParaCanc), "acknowledge_cancel")

		if got := reg.CounterTotal(lifecycle.MetricControllerAvoided); got != 1 {
			t.Fatalf("controller calls avoided = %d, want 1", got)
		}
		if got, want := reg.CounterTotal(lifecycle.MetricRequests), int64(w.layer.Stats().EngineRequests); got != want || got == 0 {
			t.Fatalf("requests recorded = %d, engine requests = %d", got, want)
		}
		if reg.CounterTotal(lifecycle.MetricAccepted)+reg.CounterTotal(lifecycle.MetricFallback) != reg.CounterTotal(lifecycle.MetricRequests) {
			t.Errorf("accepted + fallback != requests: every advice is one or the other")
		}

		w.propose("appointments.book", "task", map[string]any{"patient": "Luca Ferri"})
		wantAct(t, w.say(recep(), sayExact), "ask_missing")
		wantAct(t, w.say(recep(), "never mind"), "acknowledge_cancel")
		if got := reg.CounterTotal(lifecycle.MetricControllerAvoided); got != 1 {
			t.Fatalf("controller calls avoided = %d after a rule-detected cancel, want still 1", got)
		}
	})

	t.Run("the layer works with no hook", func(t *testing.T) {
		w := newWorld(t, withScript(pick("availability.list", 0.97)))
		w.propose("availability.list", "task", map[string]any{"date": "2026-10-14"})
		if res := w.say(recep(), sayBook); res.Trace.Decision != "selected" {
			t.Fatalf("decision = %q, want selected", res.Trace.Decision)
		}
	})
}
