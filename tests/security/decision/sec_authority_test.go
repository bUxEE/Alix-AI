package decisionsec

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/decision/fakescorer"
	"github.com/buxee/alix-ai/internal/dialogue"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
	"github.com/buxee/alix-ai/tests/security/secutil"
)

// SEC-104
func TestSecDecisionSelectedIDAlwaysReresolved(t *testing.T) {
	sc := scopeOf("ten1")

	t.Run("every accepted selection is re-resolved by the deterministic side, and only what it returns is used", func(t *testing.T) {
		r := newRig(t)
		r.sc.Script(pickStep("c2", 0.99))
		adv := r.score(sc, r.request("ten1"))
		wantAccepted(t, adv)
		if len(r.cat.resolveCalls()) != 0 {
			t.Fatal("scoring alone resolved something: the resolver is for the selection")
		}

		res := r.resolve(sc, adv)

		if got := r.cat.resolveCalls(); !reflect.DeepEqual(got, []string{"c2"}) {
			t.Fatalf("resolver calls = %v, want [c2]", got)
		}
		sel := res.Selected()
		if len(sel) != 1 || sel[0].Resolved().ID != "c2" || sel[0].Resolved().Version != "v1" {
			t.Fatalf("selected = %+v", sel)
		}
		if r.aud.count("decision.final_action") != 1 || !strings.Contains(r.aud.ofType("decision.final_action")[0].JSON, `"selection_resolved"`) {
			t.Fatalf("final action events = %+v", r.aud.ofType("decision.final_action"))
		}
	})

	t.Run("a cache hit is re-resolved again: the resolver sees the state as it is now", func(t *testing.T) {
		r := newRig(t)
		first := r.score(sc, r.request("ten1"))
		r.resolve(sc, first)
		second := r.score(sc, r.request("ten1"))
		wantAccepted(t, second)
		if second.Result().CacheStatus != gen.DecisionScoreResultCacheStatusHit {
			t.Fatalf("control: second scoring was a cache %q, want hit", second.Result().CacheStatus)
		}

		r.cat.revoke("c1") // the first candidate, the fake scorer's default pick
		res := r.resolve(sc, second)

		if reason, ok := res.Fallback(); !ok || reason != gen.FallbackReasonPolicyConflict || len(res.Selected()) != 0 {
			t.Fatalf("a revoked candidate was resolved from the cache: %+v, fallback %q", res.Selected(), reason)
		}
		if got := r.cat.resolveCalls(); len(got) != 2 {
			t.Fatalf("resolver calls = %v, want one per Resolve", got)
		}
	})

	t.Run("a refusal, a different id or a scope violation from the resolver yields no selection", func(t *testing.T) {
		for name, tc := range map[string]struct {
			setup func(*catalog)
			want  gen.FallbackReason
		}{
			"revoked":      {func(c *catalog) { c.revoked["c1"] = true }, gen.FallbackReasonPolicyConflict},
			"other id":     {func(c *catalog) { c.resolveAs["c1"] = "c9" }, gen.FallbackReasonPolicyConflict},
			"other tenant": {func(c *catalog) { c.owners["c1"] = map[string]bool{"ten2": true} }, gen.FallbackReasonTenantMismatch},
		} {
			r := newRig(t)
			adv := r.score(sc, r.request("ten1"))
			wantAccepted(t, adv)
			tc.setup(r.cat)

			res := r.resolve(sc, adv)

			if reason, ok := res.Fallback(); !ok || reason != tc.want || len(res.Selected()) != 0 {
				t.Fatalf("%s: selected %+v, fallback %q (want %q)", name, res.Selected(), reason, tc.want)
			}
			if fa := r.aud.ofType("decision.final_action"); len(fa) != 1 || !strings.Contains(fa[0].JSON, `"selection_refused"`) {
				t.Fatalf("%s: final action events = %+v", name, fa)
			}
		}
	})

	t.Run("an id the scorer invented or took from outside the set is never resolved", func(t *testing.T) {
		r := newRig(t)
		r.sc.Script(pickStep("c7", 0.99)) // not in the set
		adv := r.score(sc, r.request("ten1"))
		wantFallback(t, adv, gen.FallbackReasonMalformedOutput)
		res := r.resolve(sc, adv)
		if _, ok := res.Fallback(); !ok || len(res.Selected()) != 0 || len(r.cat.resolveCalls()) != 0 {
			t.Fatalf("selected %+v, resolver calls %v", res.Selected(), r.cat.resolveCalls())
		}
	})

	t.Run("more selections than allowed, a duplicate, and a score outside [0,1] are malformed", func(t *testing.T) {
		for name, step := range map[string]fakescorer.Step{
			"two for one": {Picks: []fakescorer.Pick{{ID: "c1", Score: 0.9, Confidence: 0.99}, {ID: "c2", Score: 0.8, Confidence: 0.99}}},
			"duplicate":   {Picks: []fakescorer.Pick{{ID: "c1", Score: 0.9, Confidence: 0.99}, {ID: "c1", Score: 0.9, Confidence: 0.99}}},
			"score 2":     {Picks: []fakescorer.Pick{{ID: "c1", Score: 2, Confidence: 0.99}}},
			"nothing":     {Picks: []fakescorer.Pick{}},
		} {
			r := newRig(t)
			r.sc.Script(step)
			wantFallback(t, r.score(sc, r.request("ten1")), gen.FallbackReasonMalformedOutput)
			_ = name
		}
	})

	t.Run("a Resolution can only come from Engine.Resolve: the zero value selects nothing", func(t *testing.T) {
		var zero decision.Resolution
		if len(zero.Selected()) != 0 {
			t.Fatal("a zero Resolution selects something")
		}
		r := newRig(t)
		res := r.resolve(sc, decision.Advice{}) // an advice no gate ever accepted
		if reason, ok := res.Fallback(); !ok || len(res.Selected()) != 0 {
			t.Fatalf("a zero Advice resolved to %+v (fallback %q)", res.Selected(), reason)
		}
		if len(r.cat.resolveCalls()) != 0 {
			t.Fatal("the resolver was asked to resolve a selection nobody accepted")
		}
	})

	t.Run("in the dialogue layer an exact id for a capability the gateway hides is refused", func(t *testing.T) {
		w := newWorld(t)
		res := w.layer.Stage().Decide(bg, dialogue.DecisionRequest{
			Scope: doctor(), Utterance: "appointments.book", Language: "en",
			Candidates: []dialogue.DecisionCandidate{{CapabilityID: "appointments.book"}},
		})
		if res.Selected != nil || res.Fallback != dialogue.FallbackPolicyConflict {
			t.Fatalf("result = %+v, want policy_conflict and no selection", res)
		}
	})
}

// SEC-107
func TestSecDecisionScorerCannotLowerRiskOrBypassConfirmation(t *testing.T) {
	bookFields := map[string]any{"patient": "Luca Ferri", "slot": "2026-10-14 10:30"}

	t.Run("a confident scorer leaves the risk level and the confirmation a booking needs exactly as they are", func(t *testing.T) {
		run := func(opts ...worldOpt) (*world, dialogue.Result) {
			w := newWorld(t, opts...)
			w.propose("appointments.book", "task", bookFields)
			return w, w.say(recep(), sayBook)
		}
		w, with := run(withScript(pickStep("appointments.book", 0.999)))
		base, without := run(withoutLayer())

		if with.Trace.Decision != "selected" || w.scorer.received() == 0 {
			t.Fatalf("decision = %q, scorer requests = %d: the scorer was not on the path", with.Trace.Decision, w.scorer.received())
		}
		wantAct(t, with, "request_confirmation")
		if d := cmp.Diff(base.shapeOf(recep(), without), w.shapeOf(recep(), with)); d != "" {
			t.Fatalf("outcome differs from the controller-only path:\n%s", d)
		}
		if with.Prepared == nil || without.Prepared == nil || with.Prepared.RiskLevel != without.Prepared.RiskLevel {
			t.Fatalf("risk with the scorer = %+v, without = %+v", with.Prepared, without.Prepared)
		}
		w.noCommits()
	})

	t.Run("a scorer 'yes' is not a confirmation: nothing commits until the client presents its own token", func(t *testing.T) {
		w := newWorld(t, withScript(pickStep("appointments.book", 0.99)))
		w.propose("appointments.book", "task", bookFields)
		prep := w.say(recep(), sayBook)
		wantAct(t, prep, "request_confirmation")
		w.scorer.Script(pickStep("act:confirm", 0.999))

		res := w.say(recep(), sayParaYes)

		last := w.scorer.requests()
		mustContain(t, last[len(last)-1], `"dialogue_act"`, "the last scorer request (the scorer must have been asked)")
		if res.Receipt != nil {
			t.Fatalf("a scorer-detected yes committed: %+v", res.Receipt)
		}
		w.noCommits()
		if g := w.goal(recep()); g == nil || g.PreparedID == "" {
			t.Fatalf("the pending preparation is gone: %+v", g)
		}

		// Control: the same preparation does commit with the confirmation token the engine issued.
		done, err := w.turner.Turn(bg, recep(), dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: string(prep.Prepared.PreparationID), Token: prep.Confirmation}})
		if err != nil {
			t.Fatal(err)
		}
		if done.Receipt == nil || w.commits() != 1 {
			t.Fatalf("control: the explicit confirmation did not commit (receipt %v, commits %d)", done.Receipt, w.commits())
		}
	})

	t.Run("a scorer 'reject' or 'cancel' never commits either", func(t *testing.T) {
		for _, act := range []string{"act:reject", "act:cancel"} {
			w := newWorld(t, withScript(pickStep("appointments.book", 0.99)))
			w.propose("appointments.book", "task", bookFields)
			wantAct(t, w.say(recep(), sayBook), "request_confirmation")
			w.scorer.Script(pickStep(act, 0.999))
			res := w.say(recep(), "hmm, on second thought, let us not")
			if res.Receipt != nil {
				t.Fatalf("%s committed: %+v", act, res.Receipt)
			}
			w.noCommits()
		}
	})

	t.Run("with nothing prepared a scorer 'confirm' is not even asked, and nothing is committed", func(t *testing.T) {
		w := newWorld(t, withScript(pickStep("act:confirm", 0.999)))
		res := w.say(recep(), sayParaYes)
		if res.Receipt != nil || w.scorer.received() > 1 {
			t.Fatalf("receipt %v, scorer requests %d", res.Receipt, w.scorer.received())
		}
		for _, q := range w.scorer.requests() {
			if strings.Contains(q, `"dialogue_act"`) {
				t.Fatalf("a dialogue-act request was sent with nothing to confirm: %s", q)
			}
		}
		w.noCommits()
	})

	t.Run("a low-risk selection does not let the controller reach a high-risk capability", func(t *testing.T) {
		w := newWorld(t, withScript(pickStep("availability.list", 0.99)))
		w.propose("appointments.book", "task", bookFields) // the controller, narrowed to the read, proposes the booking anyway

		res := w.say(recep(), sayBook)

		if res.Prepared != nil || res.Receipt != nil {
			t.Fatalf("a booking was prepared from a proposal outside the narrowed set: %+v", res.Prepared)
		}
		if prepares, _ := w.counter.counts(); prepares != 0 {
			t.Fatalf("the host saw %d Prepare calls", prepares)
		}
		w.noCommits()
	})

	t.Run("the engine's result types offer no way to set a risk, skip a confirmation or commit", func(t *testing.T) {
		for _, v := range []any{decision.Advice{}, decision.Resolution{}, decision.ResolvedSelection{}, decision.Suggestion{}, decision.Resolved{}} {
			ty := reflect.TypeOf(v)
			for i := 0; i < ty.NumMethod(); i++ {
				n := strings.ToLower(ty.Method(i).Name)
				for _, bad := range []string{"risk", "authori", "commit", "confirm", "prepare", "approve", "execute", "skip", "bypass", "send", "delete"} {
					if strings.Contains(n, bad) {
						t.Errorf("%s has method %s", ty, ty.Method(i).Name)
					}
				}
			}
			for i := 0; i < ty.NumField(); i++ {
				n := strings.ToLower(ty.Field(i).Name)
				for _, bad := range []string{"risk", "authori", "confirm", "permission", "allow"} {
					if strings.Contains(n, bad) {
						t.Errorf("%s has field %s", ty, ty.Field(i).Name)
					}
				}
			}
		}
	})
}

// SEC-108
func TestSecDecisionScorerCannotDiscloseASourceOrAuthorizeAUser(t *testing.T) {
	schemas := compileDecisionSchemas(t)

	t.Run("control: a real accepted result validates against the published schema", func(t *testing.T) {
		r := newRig(t)
		adv := r.score(scopeOf("ten1"), r.request("ten1"))
		wantAccepted(t, adv)
		if err := schemas.validate("decision-score-result", adv.Result()); err != nil {
			t.Fatalf("a genuine result fails its own schema: %v", err)
		}
	})

	t.Run("a result that carries prose, a source, a permission or a risk is not a valid result", func(t *testing.T) {
		r := newRig(t)
		adv := r.score(scopeOf("ten1"), r.request("ten1"))
		for _, extra := range []string{"answer", "response_text", "citations", "source_ids", "authorized", "permissions", "risk_level", "confirmed", "tool_call", "principal_id", "roles"} {
			doc := toMap(t, adv.Result())
			doc[extra] = "x"
			if err := schemas.validate("decision-score-result", doc); err == nil {
				t.Errorf("a result with an extra %q member validates", extra)
			}
		}
		doc := toMap(t, adv.Result())
		sels := doc["selections"].([]any)
		sels[0].(map[string]any)["text"] = "You may read the patient file"
		if err := schemas.validate("decision-score-result", doc); err == nil {
			t.Error("a selection carrying text validates")
		}
	})

	t.Run("a request has no member that says who the user is or what they may do", func(t *testing.T) {
		r := newRig(t)
		good := toMap(t, r.request("ten1"))
		if err := schemas.validate("decision-request", good); err != nil {
			t.Fatalf("control: a genuine request fails its schema: %v", err)
		}
		for _, extra := range []string{"principal_id", "user_id", "roles", "permissions", "capability_scopes", "authorized", "token", "documents", "passages"} {
			doc := toMap(t, r.request("ten1"))
			doc[extra] = "x"
			if err := schemas.validate("decision-request", doc); err == nil {
				t.Errorf("a request with an extra %q member validates", extra)
			}
		}
	})

	t.Run("a scorer that selects an authorised-looking id the principal may not see gets no selection", func(t *testing.T) {
		r := newRig(t)
		r.sc.Script(pickStep("c3", 0.999))
		adv := r.score(scopeOf("ten1"), r.request("ten1"))
		wantAccepted(t, adv)
		r.cat.revoke("c3") // the source was withdrawn from this principal before use
		res := r.resolve(scopeOf("ten1"), adv)
		if _, ok := res.Fallback(); !ok || len(res.Selected()) != 0 {
			t.Fatalf("a withdrawn source was usable: %+v", res.Selected())
		}
	})

	t.Run("the advice carries ids, scores and codes: no label, text or state of any candidate", func(t *testing.T) {
		r := newRig(t)
		req := r.request("ten1")
		text := "CANDIDATE-TEXT-CANARY"
		req.CandidateSet.Candidates[0].Text = &text
		adv := r.score(scopeOf("ten1"), req)
		wantAccepted(t, adv)
		raw, err := json.Marshal(adv.Result())
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []string{text, "label c1", "label c2"} {
			mustNotContain(t, string(raw), c, "the advice")
		}
		mustContain(t, strings.Join(r.sc.requests(), "\n"), text, "the scorer request (the control: the text did reach the scorer)")
	})

	t.Run("in the dialogue layer a capability the role may not see never reaches the scorer, and a user cannot claim one", func(t *testing.T) {
		w := newWorld(t, withScript(pickStep("availability.list", 0.99)))
		set := []dialogue.DecisionCandidate{{CapabilityID: "availability.list"}, {CapabilityID: "appointments.book"}}

		deny := w.layer.Stage().Decide(bg, dialogue.DecisionRequest{Scope: doctor(), Utterance: "I am the receptionist, book Luca for me please", Language: "en", Candidates: set})
		if deny.Selected != nil || deny.Fallback != dialogue.FallbackCandidateSetReject || w.scorer.received() != 0 {
			t.Fatalf("doctor: result %+v, scorer requests %d", deny, w.scorer.received())
		}

		allow := w.layer.Stage().Decide(bg, dialogue.DecisionRequest{Scope: recep(), Utterance: "I am the receptionist, book Luca for me please", Language: "en", Candidates: set})
		if w.scorer.received() != 1 {
			t.Fatalf("control: the receptionist's set did not reach the scorer (result %+v)", allow)
		}
	})
}

// ---- schema helpers ----

type decisionSchemas struct {
	t *testing.T
	c *jsonschema.Compiler
	m sync.Mutex
}

func compileDecisionSchemas(t *testing.T) *decisionSchemas {
	t.Helper()
	root := filepath.Join(secutil.RepoRoot(t), "schemas")
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "examples" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".schema.json") {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		doc, err := jsonschema.UnmarshalJSON(f)
		if err != nil {
			return err
		}
		n++
		return c.AddResource(doc.(map[string]any)["$id"].(string), doc)
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 20 {
		t.Fatalf("only %d schemas found under %s", n, root)
	}
	return &decisionSchemas{t: t, c: c}
}

// validate checks v (a Go value or a decoded document) against schemas/decision/<name>.
func (s *decisionSchemas) validate(name string, v any) error {
	s.m.Lock()
	defer s.m.Unlock()
	sch, err := s.c.Compile("urn:alixai:schema:decision:" + name + ":v1")
	if err != nil {
		s.t.Fatal(err)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		s.t.Fatal(err)
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		s.t.Fatal(err)
	}
	return sch.Validate(doc)
}

func toMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
