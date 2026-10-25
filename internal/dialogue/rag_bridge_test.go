package dialogue

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/capabilities"
	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

const (
	// knowledgeSay is a question no capability of the test package handles:
	// the router finds nothing the caller may use, so the engine alone would refuse
	// it, with no model call.
	knowledgeSay = "what is the warranty on the Model X kettle"

	knowledgeClaim  = "The Model X kettle has a warranty of 24 months from the date of purchase."
	knowledgeClaim2 = "Keep the receipt as proof of purchase."
)

// knowledgeFake is the Knowledge of the tests: it records what it was asked, in
// whose scope, and answers what the test says.
type knowledgeFake struct {
	mu     sync.Mutex
	asked  []knowledgeAsk
	answer func(context.Context, rag.AnswerRequest) (rag.AnswerResult, error)
}

type knowledgeAsk struct {
	scope contracts.Scope
	req   rag.AnswerRequest
}

func (k *knowledgeFake) Answer(ctx context.Context, scope contracts.Scope, req rag.AnswerRequest) (rag.AnswerResult, error) {
	k.mu.Lock()
	k.asked = append(k.asked, knowledgeAsk{scope: scope, req: req})
	fn := k.answer
	k.mu.Unlock()
	return fn(ctx, req)
}

func (k *knowledgeFake) calls() []knowledgeAsk {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]knowledgeAsk(nil), k.asked...)
}

// knowledgeCitation is a client citation as the answerer writes it: a reference
// that resolves through the RAG API, never a URL, and the claims the source supports.
func knowledgeCitation(source, name, page string, claims ...string) conversation.Citation {
	section := page
	return conversation.Citation{
		SourceID: act.SourceID(source), DisplayName: name, Version: "v7",
		Locator:      &conversation.CitationLocator{Section: &section},
		Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent,
		OpenRef: conversation.CitationOpenRef{Value: conversation.CitationOpenRefFile{Kind: "file", FileID: act.FileID(ids.New("fil"))}},
		Claims:  claims,
	}
}

func knowledgeGroundedAnswer() rag.GroundedAnswer {
	return rag.GroundedAnswer{
		Claims: []rag.GroundedAnswerClaim{
			{Text: knowledgeClaim, CitationIndexes: []int64{0}},
			{Text: knowledgeClaim2, CitationIndexes: []int64{1}},
		},
		Citations: []conversation.Citation{
			knowledgeCitation(ids.New("src"), "Warranty terms", "Section 2", knowledgeClaim),
			knowledgeCitation(ids.New("src"), "Customer handbook", "Section 5", knowledgeClaim2),
		},
		Notices: rag.RetrievalNotices{},
	}
}

func knowledgeReply(res rag.AnswerResult) *knowledgeFake {
	return &knowledgeFake{answer: func(context.Context, rag.AnswerRequest) (rag.AnswerResult, error) { return res, nil }}
}

func knowledgeFailing(err error) *knowledgeFake {
	return &knowledgeFake{answer: func(context.Context, rag.AnswerRequest) (rag.AnswerResult, error) { return rag.AnswerResult{}, err }}
}

// knowledgeBridge builds the bridge over an environment's engine.
func knowledgeBridge(t testing.TB, e *env, k Knowledge) *RAGBridge {
	t.Helper()
	b, err := NewRAGBridge(e.eng, k)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// say runs one text turn through the bridge, checking what every turn must satisfy.
func (e *env) knowledgeSay(b *RAGBridge, sc contracts.Scope, text, lang string) Result {
	e.t.Helper()
	res, err := b.Turn(ctxb(), sc, TurnInput{Text: text, Language: lang})
	if err != nil {
		e.t.Fatalf("Turn: %v", err)
	}
	e.checkResult(res)
	return res
}

func TestDialogueRoutesKnowledgeQuestionToRAG(t *testing.T) {
	t.Run("a question no capability handles is answered from the sources, with citations", func(t *testing.T) {
		e := newEnv(t)
		ga := knowledgeGroundedAnswer()
		k := knowledgeReply(rag.AnswerResult{Value: ga})
		b := knowledgeBridge(t, e, k)

		res := e.knowledgeSay(b, memberScope(), knowledgeSay, "en")

		wantAct(t, res, "present_result")
		if res.Failed() {
			t.Fatalf("code = %s", res.Code)
		}
		// The question went to the knowledge source exactly once, in exactly the
		// verified scope, as the user wrote it and narrowed to nothing.
		calls := k.calls()
		if len(calls) != 1 {
			t.Fatalf("Knowledge was asked %d times, want 1", len(calls))
		}
		if !reflect.DeepEqual(calls[0].scope, memberScope()) {
			t.Fatalf("Knowledge was asked in %+v, want the verified scope %+v", calls[0].scope, memberScope())
		}
		if string(calls[0].req.Question) != knowledgeSay || len(calls[0].req.SourceIDs) != 0 {
			t.Fatalf("request = %+v", calls[0].req)
		}

		// The citations are the directive's sources, and so what the response may cite.
		if !reflect.DeepEqual(res.Directive.Sources, ga.Citations) {
			t.Fatalf("sources = %+v, want the citations of the answer", res.Directive.Sources)
		}
		if res.Directive.CapabilityID != nil || len(res.Directive.Facts) != 0 {
			t.Fatalf("directive = %+v: a knowledge answer names no capability and states no host fact", res.Directive)
		}
		for i, s := range res.Directive.Sources {
			if _, isFile := s.OpenRef.Value.(conversation.CitationOpenRefFile); !isFile || len(s.Claims) == 0 {
				t.Errorf("source %d = %+v", i, s)
			}
		}

		// The response is the verified template: the claims verbatim with their
		// markers, then the list of the sources. No model was asked for anything.
		if res.Response.Source != verify.SourceTemplate {
			t.Fatalf("response = %+v, want the template", res.Response)
		}
		for _, want := range []string{knowledgeClaim + " [1]", knowledgeClaim2 + " [2]", "Sources:", "[1] Warranty terms (Section 2)", "[2] Customer handbook (Section 5)"} {
			mustContain(t, res.Response.Text, want, "the response")
		}
		if n := len(e.prompts()); n != 0 {
			t.Fatalf("%d model calls for a question the router found nothing for and an answer already verified", n)
		}
		e.noHostWrites()
		if n := len(e.app.Calls()); n != 0 {
			t.Fatalf("the host application saw %d calls: a knowledge answer asks it nothing", n)
		}

		// The route is recorded in the trace, between the goal and the response.
		if !hasStage(res, StageScope, StageRate, StageGoal, StageRules, StageShortlist, StageKnowledge, StageVerify, StagePersist) {
			t.Fatalf("stages = %v", res.Trace.Stages)
		}
		if res.Trace.Route != capabilities.OutcomeOutOfScope {
			t.Fatalf("route = %s", res.Trace.Route)
		}

		// The turn is part of the conversation like any other: the next turn finds
		// the user's question and the answer in the window.
		sess := e.session(memberScope())
		turns, err := e.store.RecentTurns(ctxb(), sess, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) != 2 || turns[0].Role != "user" || turns[0].Body != knowledgeSay || turns[1].Role != "assistant" ||
			turns[1].Act != "present_result" || !strings.Contains(turns[1].Body, knowledgeClaim) {
			t.Fatalf("recent turns = %+v", turns)
		}

		// The citation events of the turn are the directive's sources, in order,
		// valid against the published event schema.
		events := KnowledgeCitationEvents(res)
		if len(events) != 2 {
			t.Fatalf("citation events = %d, want 2", len(events))
		}
		for i, draft := range events {
			ev := draft(conversation.Seq(10 + i))
			validateAgainst(t, "urn:alixai:schema:conversation:turn-event:v1", conversation.TurnEvent{Value: ev})
			ce, ok := ev.(conversation.CitationEvent)
			if !ok || ce.Seq != conversation.Seq(10+i) || string(ce.TurnID) != res.TurnID || !reflect.DeepEqual(ce.Citation, ga.Citations[i]) {
				t.Errorf("event %d = %+v", i, ev)
			}
		}
	})

	t.Run("no authorised evidence is the typed absence, never an uncited answer", func(t *testing.T) {
		for _, reason := range []rag.NoSupportedEvidenceReason{
			rag.NoSupportedEvidenceReasonNoMatch, rag.NoSupportedEvidenceReasonClaimsUnsupported,
			rag.NoSupportedEvidenceReasonEvidenceUnavailable, rag.NoSupportedEvidenceReasonEgressRestricted,
		} {
			e := newEnv(t)
			k := knowledgeReply(rag.AnswerResult{Value: rag.NoSupportedEvidence{Reason: reason, Notices: rag.RetrievalNotices{}}})
			res := e.knowledgeSay(knowledgeBridge(t, e, k), memberScope(), knowledgeSay, "en")

			wantAct(t, res, "report_no_evidence")
			if res.Code != contracts.ErrNoSupportedEvidence || !res.Failed() {
				t.Fatalf("%s: code = %q", reason, res.Code)
			}
			if rc := res.Directive.ReasonCode; rc == nil || string(*rc) != "no_supported_evidence" {
				t.Fatalf("%s: reason_code = %v", reason, rc)
			}
			if len(res.Directive.Sources) != 0 || len(KnowledgeCitationEvents(res)) != 0 {
				t.Fatalf("%s: a turn with no evidence carries sources", reason)
			}
			mustContain(t, res.Response.Text, "couldn't find anything in the available sources", "the response")
			// The reason of the absence is never shown: it must not tell a principal that
			// a source it may not read exists.
			for _, leak := range []string{"egress", "restricted", "unavailable", "claims", "match"} {
				mustNotContain(t, strings.ToLower(res.Response.Text), leak, "the response for "+string(reason))
			}
			if n := len(e.prompts()); n != 0 {
				t.Fatalf("%s: %d model calls", reason, n)
			}
		}
	})

	t.Run("in Italian", func(t *testing.T) {
		e := newEnv(t)
		ga := knowledgeGroundedAnswer()
		res := e.knowledgeSay(knowledgeBridge(t, e, knowledgeReply(rag.AnswerResult{Value: ga})), memberScope(), knowledgeSay, "it")
		wantAct(t, res, "present_result")
		mustContain(t, res.Response.Text, "Fonti:", "the Italian response")
		mustContain(t, res.Response.Text, knowledgeClaim+" [1]", "the Italian response")
		none := newEnv(t)
		res = none.knowledgeSay(knowledgeBridge(t, none, knowledgeReply(rag.AnswerResult{Value: rag.NoSupportedEvidence{Reason: rag.NoSupportedEvidenceReasonNoMatch}})), memberScope(), knowledgeSay, "it")
		mustContain(t, res.Response.Text, "Non ho trovato nulla nelle fonti disponibili", "the Italian absence")
	})

	t.Run("a failure to answer is a typed failure, not an answer and not no evidence", func(t *testing.T) {
		for name, tc := range map[string]struct {
			err  error
			act  string
			code contracts.ErrCode
		}{
			"the model is unavailable":   {contracts.New(contracts.ErrUnavailable, "the model is down"), "report_failure", contracts.ErrUnavailable},
			"the answer ran out of time": {contracts.New(contracts.ErrTimeout, "too slow"), "report_failure", contracts.ErrTimeout},
			"the output was unusable":    {contracts.New(contracts.ErrSchemaInvalid, "bad output"), "report_failure", contracts.ErrSchemaInvalid},
			"the sources refuse the scope": {
				contracts.New(contracts.ErrScopeViolation, "not yours"), "refuse", contracts.ErrScopeViolation,
			},
			"an untyped error": {context.DeadlineExceeded, "report_failure", contracts.ErrUnavailable},
		} {
			e := newEnv(t)
			res := e.knowledgeSay(knowledgeBridge(t, e, knowledgeFailing(tc.err)), memberScope(), knowledgeSay, "en")
			if string(res.Directive.Act) != tc.act || res.Code != tc.code {
				t.Errorf("%s: act = %s, code = %q, want %s %s", name, res.Directive.Act, res.Code, tc.act, tc.code)
			}
			mustNotContain(t, res.Response.Text, "couldn't find anything", name)
			if len(res.Directive.Sources) != 0 {
				t.Errorf("%s: a failure carries sources", name)
			}
		}
	})

	t.Run("an answer without provenance is not presented", func(t *testing.T) {
		good := knowledgeGroundedAnswer()
		for name, mut := range map[string]func(*rag.GroundedAnswer){
			"no claims":                func(a *rag.GroundedAnswer) { a.Claims = nil },
			"no citations":             func(a *rag.GroundedAnswer) { a.Citations = nil },
			"a claim with no citation": func(a *rag.GroundedAnswer) { a.Claims[0].CitationIndexes = nil },
			"a citation out of range":  func(a *rag.GroundedAnswer) { a.Claims[0].CitationIndexes = []int64{5} },
			"a citation with no claims": func(a *rag.GroundedAnswer) {
				c := a.Citations[0]
				c.Claims = nil
				a.Citations[0] = c
			},
		} {
			a := good
			a.Claims = append([]rag.GroundedAnswerClaim(nil), good.Claims...)
			a.Citations = append([]conversation.Citation(nil), good.Citations...)
			mut(&a)
			e := newEnv(t)
			res := e.knowledgeSay(knowledgeBridge(t, e, knowledgeReply(rag.AnswerResult{Value: a})), memberScope(), knowledgeSay, "en")
			wantAct(t, res, "report_failure")
			mustNotContain(t, res.Response.Text, knowledgeClaim, name)
			if len(res.Directive.Sources) != 0 {
				t.Errorf("%s: sources = %+v", name, res.Directive.Sources)
			}
		}
		e := newEnv(t)
		res := e.knowledgeSay(knowledgeBridge(t, e, knowledgeReply(rag.AnswerResult{})), memberScope(), knowledgeSay, "en")
		wantAct(t, res, "report_failure")
	})

	t.Run("a request a capability handles never reaches the sources", func(t *testing.T) {
		e := newEnv(t)
		k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
		b := knowledgeBridge(t, e, k)
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
		e.onResponse("Please confirm: create an item named widget, quantity 3.")
		res := e.knowledgeSay(b, memberScope(), sayCreate, "en")
		wantAct(t, res, "request_confirmation")
		if n := len(k.calls()); n != 0 {
			t.Fatalf("a request the capability took reached Knowledge %d times", n)
		}
		if KnowledgeTurn(res) {
			t.Fatal("the trace says the turn was a knowledge question")
		}
		// A refusal for a permission is a refusal: the visitor may not create, and the
		// answer is not "ask the documents".
		e2 := newEnv(t)
		k2 := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
		b2 := knowledgeBridge(t, e2, k2)
		e2.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
		res2 := e2.knowledgeSay(b2, scopeFor("visitor", capGet), sayCreate, "en")
		wantAct(t, res2, "refuse")
		if n := len(k2.calls()); n != 0 {
			t.Fatalf("a permission refusal reached Knowledge %d times", n)
		}
	})

	t.Run("the controller calling a request out of scope sends it to the sources", func(t *testing.T) {
		e := newEnv(t)
		k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
		b := knowledgeBridge(t, e, k)
		// The router shortlists the capability; the controller says it is not a task.
		e.onProposal(proposalJSON("", "out_of_scope", nil))
		res := e.knowledgeSay(b, memberScope(), "create an item, and also what is the warranty on the Model X kettle", "en")
		wantAct(t, res, "present_result")
		if n := len(k.calls()); n != 1 {
			t.Fatalf("Knowledge was asked %d times", n)
		}
		if n := len(e.prompts()); n != 1 {
			t.Fatalf("model calls = %d, want the one proposal and no response call", n)
		}
		e.noHostWrites()
	})

	t.Run("structured inputs are never questions", func(t *testing.T) {
		e := newEnv(t)
		k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
		b := knowledgeBridge(t, e, k)
		for name, in := range map[string]TurnInput{
			"a choice":       {ChoiceID: "opt_1"},
			"a rejection":    {Reject: "prep_1"},
			"a confirmation": {Confirm: &Confirmation{PreparationID: "prep_1"}},
			"blank text":     {Text: "   "},
			"text and choice": {
				Text: knowledgeSay, ChoiceID: "opt_1",
			},
		} {
			_, _ = b.Turn(ctxb(), memberScope(), in)
			if n := len(k.calls()); n != 0 {
				t.Fatalf("%s reached Knowledge", name)
			}
		}
	})

	t.Run("an invalid scope is refused before anything is asked", func(t *testing.T) {
		e := newEnv(t)
		k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
		b := knowledgeBridge(t, e, k)
		bad := memberScope()
		bad.TenantID = ""
		if _, err := b.Turn(ctxb(), bad, TurnInput{Text: knowledgeSay}); err == nil {
			t.Fatal("a turn without a tenant ran")
		}
		wild := memberScope()
		wild.WorkspaceID = "*"
		if _, err := b.Turn(ctxb(), wild, TurnInput{Text: knowledgeSay}); err == nil {
			t.Fatal("a turn with a wildcard ran")
		}
		if n := len(k.calls()); n != 0 {
			t.Fatalf("Knowledge was asked %d times under an invalid scope", n)
		}
	})

	t.Run("the question is the user's text, bounded as the API bounds it", func(t *testing.T) {
		e := newEnv(t, withCtxProfile(ctxLiteWith(func(p *dctx.Profile) { p.MaxUtteranceRunes = 5000 })))
		k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
		b := knowledgeBridge(t, e, k)
		long := "what about the kettle " + strings.Repeat("é", 2400)
		e.knowledgeSay(b, memberScope(), long, "en")
		calls := k.calls()
		if len(calls) != 1 {
			t.Fatalf("calls = %d", len(calls))
		}
		if got := []rune(string(calls[0].req.Question)); len(got) != knowledgeMaxQuestionRunes {
			t.Fatalf("the question has %d characters, want %d", len(got), knowledgeMaxQuestionRunes)
		}
	})

	t.Run("a caller that goes away ends the turn: no answer, no state", func(t *testing.T) {
		e := newEnv(t)
		ctx, cancel := context.WithCancel(ctxb())
		k := &knowledgeFake{answer: func(context.Context, rag.AnswerRequest) (rag.AnswerResult, error) {
			cancel()
			return rag.AnswerResult{}, context.Canceled
		}}
		b := knowledgeBridge(t, e, k)
		if _, err := b.Turn(ctx, memberScope(), TurnInput{Text: knowledgeSay}); err == nil {
			t.Fatal("a cancelled turn returned a result")
		}
		if n := e.session(memberScope()).TurnCount; n != 0 {
			t.Fatalf("a cancelled turn was saved (%d turns)", n)
		}
	})
}

// A turn that is not a knowledge question runs exactly as the engine runs it: the
// bridge repeats the few lines of turn.run around its one step, and this is what
// keeps the copy honest.
func TestRAGBridgeMatchesEngineForCapabilityTurns(t *testing.T) {
	type step struct {
		say    string
		script func(*env)
		in     func(prev Result) TurnInput
	}
	steps := []step{
		{say: "help"},
		{say: sayCreate, script: func(e *env) {
			e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
			e.onResponse("Please confirm: create an item named widget, quantity 3.")
		}},
		{say: "create an item named gadget", script: func(e *env) {
			e.model.Reset()
			e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "gadget"}))
			e.onResponse("How many would you like?")
		}},
		{say: "cancel"},
		{say: "what is the weather like in Paris today"}, // refused by the engine; with no Knowledge it must stay a refusal
	}
	direct, bridged := newEnv(t), newEnv(t)
	k := &knowledgeFake{answer: func(context.Context, rag.AnswerRequest) (rag.AnswerResult, error) {
		return rag.AnswerResult{Value: rag.NoSupportedEvidence{Reason: rag.NoSupportedEvidenceReasonNoMatch}}, nil
	}}
	b := knowledgeBridge(t, bridged, k)

	for i, s := range steps {
		if s.script != nil {
			s.script(direct)
			s.script(bridged)
		}
		want, err := direct.eng.Turn(ctxb(), memberScope(), TurnInput{Text: s.say, Language: "en"})
		if err != nil {
			t.Fatalf("step %d: engine: %v", i, err)
		}
		got, err := b.Turn(ctxb(), memberScope(), TurnInput{Text: s.say, Language: "en"})
		if err != nil {
			t.Fatalf("step %d: bridge: %v", i, err)
		}
		if i == len(steps)-1 {
			// The one turn that differs: an out-of-scope request is a knowledge question.
			if want.Directive.Act != "refuse" || got.Directive.Act != "report_no_evidence" || !KnowledgeTurn(got) || KnowledgeTurn(want) {
				t.Fatalf("the out-of-scope turn: engine %s, bridge %s", want.Directive.Act, got.Directive.Act)
			}
			continue
		}
		if got.Directive.Act != want.Directive.Act || got.Code != want.Code || got.Response.Text != want.Response.Text ||
			got.Response.Source != want.Response.Source || got.Trace.Route != want.Trace.Route {
			t.Errorf("step %d (%q): bridge = %s/%q/%q, engine = %s/%q/%q", i, s.say,
				got.Directive.Act, got.Code, got.Response.Text, want.Directive.Act, want.Code, want.Response.Text)
		}
		if !reflect.DeepEqual(got.Trace.Stages, want.Trace.Stages) || !reflect.DeepEqual(got.Trace.Transitions, want.Trace.Transitions) {
			t.Errorf("step %d (%q): stages %v vs %v, transitions %v vs %v", i, s.say,
				got.Trace.Stages, want.Trace.Stages, got.Trace.Transitions, want.Trace.Transitions)
		}
		if len(got.Trace.ModelCalls) != len(want.Trace.ModelCalls) {
			t.Errorf("step %d (%q): %d model calls vs %d", i, s.say, len(got.Trace.ModelCalls), len(want.Trace.ModelCalls))
		}
		if (got.Prepared == nil) != (want.Prepared == nil) || got.Confirmation.IsZero() != want.Confirmation.IsZero() {
			t.Errorf("step %d (%q): a preparation on one side only", i, s.say)
		}
	}
	if k.calls() == nil || len(k.calls()) != 1 {
		t.Fatalf("Knowledge was asked %d times over the whole conversation, want only for the out-of-scope turn", len(k.calls()))
	}
	// The conversation state the two leave behind is the same shape.
	ds, bs := direct.session(memberScope()), bridged.session(memberScope())
	if ds.TurnCount != bs.TurnCount || (ds.State.Goal == nil) != (bs.State.Goal == nil) {
		t.Fatalf("sessions differ: %d turns / goal %v vs %d turns / goal %v", ds.TurnCount, ds.State.Goal, bs.TurnCount, bs.State.Goal)
	}
	// The confirmation of a preparation made through the bridge goes through the
	// engine and commits, as it does without the bridge.
	e := newEnv(t)
	bridge := knowledgeBridge(t, e, k)
	e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onResponse("Please confirm: create an item named widget, quantity 3.")
	prepared, err := bridge.Turn(ctxb(), memberScope(), TurnInput{Text: sayCreate, Language: "en"})
	if err != nil || prepared.Prepared == nil {
		t.Fatalf("prepare: %v / %+v", err, prepared.Prepared)
	}
	done, err := bridge.Turn(ctxb(), memberScope(), TurnInput{Confirm: &Confirmation{PreparationID: string(prepared.Prepared.PreparationID), Token: prepared.Confirmation}})
	if err != nil || done.Receipt == nil {
		t.Fatalf("confirm: %v / %+v", err, done.Receipt)
	}
	if e.app.Effects() != 1 {
		t.Fatalf("the host applied %d effects, want the one that was confirmed", e.app.Effects())
	}
}

func TestRAGBridgeNeedsWhatItRunsOn(t *testing.T) {
	e := newEnv(t)
	k := knowledgeReply(rag.AnswerResult{})
	if _, err := NewRAGBridge(nil, k); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("a bridge with no engine: %v", err)
	}
	if _, err := NewRAGBridge(e.eng, nil); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("a bridge with no knowledge: %v", err)
	}
	b := knowledgeBridge(t, e, k)
	if b.Engine() != e.eng {
		t.Error("the bridge runs another engine")
	}
}

// The provenance rule on its own: what is an answer and what is not.
func TestKnowledgeGroundedRule(t *testing.T) {
	good := knowledgeGroundedAnswer()
	if !knowledgeGrounded(good) {
		t.Fatal("a grounded answer is refused")
	}
	many := good
	many.Citations = make([]conversation.Citation, knowledgeMaxCitations+1)
	for i := range many.Citations {
		many.Citations[i] = good.Citations[0]
	}
	if knowledgeGrounded(many) {
		t.Error("more citations than a directive can list were accepted")
	}
	nameless := good
	nameless.Citations = append([]conversation.Citation(nil), good.Citations...)
	nameless.Citations[0].DisplayName = " "
	if knowledgeGrounded(nameless) {
		t.Error("a citation that names no source was accepted")
	}
	neg := good
	neg.Claims = []rag.GroundedAnswerClaim{{Text: "x", CitationIndexes: []int64{-1}}}
	if knowledgeGrounded(neg) {
		t.Error("a negative citation index was accepted")
	}
}

// Citation events are for knowledge turns only.
func TestKnowledgeCitationEventsAreForKnowledgeTurns(t *testing.T) {
	e := newEnv(t)
	e.app.Seed(tenantA, workspace1, "item_1", "Alpha widget")
	e.onProposal(proposalJSON(capGet, "task", map[string]any{"item_id": "item_1"}))
	e.onResponse("Here is what I found. Name: Alpha widget.")
	res := e.say(memberScope(), sayShow)
	wantAct(t, res, "present_result")
	if evs := KnowledgeCitationEvents(res); evs != nil {
		t.Fatalf("a read of the host application has %d citation events", len(evs))
	}
	// Even a directive that happens to carry sources is not a knowledge turn unless
	// the question went there.
	res.Directive.Sources = knowledgeGroundedAnswer().Citations
	if evs := KnowledgeCitationEvents(res); evs != nil {
		t.Fatal("citation events were built for a turn that was not a knowledge question")
	}
}

// The events marshal as the schema says, with the type forced.
func TestKnowledgeCitationEventsEncode(t *testing.T) {
	res := Result{TurnID: ids.New("trn"), Trace: Trace{Stages: []string{StageKnowledge}}}
	res.Directive.Act = conversation.DialogueDirectiveActPresentResult
	res.Directive.Sources = knowledgeGroundedAnswer().Citations
	for _, draft := range KnowledgeCitationEvents(res) {
		b, err := json.Marshal(conversation.TurnEvent{Value: draft(3)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"type":"citation"`) || !strings.Contains(string(b), `"seq":3`) {
			t.Errorf("event = %s", b)
		}
	}
}

// ---- the engine's own hook (M2-18) ----

// knowledgeEngineEnv builds an environment whose engine was given the Knowledge through
// Deps, the way a composition root that has it at hand does.
func knowledgeEngineEnv(t testing.TB, k Knowledge, opts ...envOpt) *env {
	t.Helper()
	return newEnv(t, append([]envOpt{withDeps(func(d *Deps) { d.Knowledge = k })}, opts...)...)
}

// A text request no capability handles is a question for the sources, asked by the engine
// itself at the point where it would refuse: no bridge, no wrapped engine.
func TestEngineRoutesKnowledgeQuestionToRAGBeforeRefusing(t *testing.T) {
	t.Run("the router finds nothing: the question is answered from the sources, with citations", func(t *testing.T) {
		ga := knowledgeGroundedAnswer()
		k := knowledgeReply(rag.AnswerResult{Value: ga})
		e := knowledgeEngineEnv(t, k)

		res := e.say(memberScope(), knowledgeSay)

		wantAct(t, res, "present_result")
		if res.Failed() {
			t.Fatalf("code = %s", res.Code)
		}
		calls := k.calls()
		if len(calls) != 1 || !reflect.DeepEqual(calls[0].scope, memberScope()) || string(calls[0].req.Question) != knowledgeSay {
			t.Fatalf("Knowledge calls = %+v, want one in the verified scope with the user's question", calls)
		}
		if !reflect.DeepEqual(res.Directive.Sources, ga.Citations) {
			t.Fatalf("sources = %+v, want the citations of the answer", res.Directive.Sources)
		}
		// The verify template renders the claims: no second model call, no model call at all.
		if res.Response.Source != verify.SourceTemplate {
			t.Fatalf("response = %+v, want the template", res.Response)
		}
		for _, want := range []string{knowledgeClaim + " [1]", "[1] Warranty terms (Section 2)"} {
			mustContain(t, res.Response.Text, want, "the response")
		}
		if n := len(e.prompts()); n != 0 {
			t.Fatalf("%d model calls for a verified answer", n)
		}
		if !KnowledgeTurn(res) || !hasStage(res, StageScope, StageRate, StageGoal, StageRules, StageShortlist, StageKnowledge, StageVerify, StagePersist) {
			t.Fatalf("stages = %v", res.Trace.Stages)
		}
		if n := len(KnowledgeCitationEvents(res)); n != 2 {
			t.Fatalf("citation events = %d, want 2", n)
		}
		e.noHostWrites()
		turns, err := e.store.RecentTurns(ctxb(), e.session(memberScope()), 4)
		if err != nil || len(turns) != 2 || turns[1].Act != "present_result" {
			t.Fatalf("the turn was not recorded like any other: %+v / %v", turns, err)
		}
	})

	t.Run("the knowledge is given after the engine is built, and taken away again", func(t *testing.T) {
		e := newEnv(t)
		wantAct(t, e.say(memberScope(), knowledgeSay), "refuse") // none: the refusal it always was
		k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
		e.eng.SetKnowledge(k)
		wantAct(t, e.say(memberScope(), knowledgeSay), "present_result")
		e.eng.SetKnowledge(nil)
		res := e.say(memberScope(), knowledgeSay)
		wantAct(t, res, "refuse")
		if res.Code != contracts.ErrUnknownCapability || KnowledgeTurn(res) {
			t.Fatalf("code = %s, knowledge turn = %v", res.Code, KnowledgeTurn(res))
		}
		if n := len(k.calls()); n != 1 {
			t.Fatalf("Knowledge was asked %d times, want only the turn it was set for", n)
		}
	})

	t.Run("the controller calling a request out of scope sends it there too", func(t *testing.T) {
		k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
		e := knowledgeEngineEnv(t, k)
		e.onProposal(proposalJSON("", "out_of_scope", nil))
		res := e.say(memberScope(), "create an item, and also what is the warranty on the Model X kettle")
		wantAct(t, res, "present_result")
		if n := len(k.calls()); n != 1 {
			t.Fatalf("Knowledge was asked %d times", n)
		}
		if n := len(e.prompts()); n != 1 {
			t.Fatalf("model calls = %d, want the one proposal", n)
		}
	})

	t.Run("a request a capability handles never reaches the sources", func(t *testing.T) {
		k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
		e := knowledgeEngineEnv(t, k)
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
		e.onResponse("Please confirm: create an item named widget, quantity 3.")
		res := e.say(memberScope(), sayCreate)
		wantAct(t, res, "request_confirmation")
		if n := len(k.calls()); n != 0 || KnowledgeTurn(res) {
			t.Fatalf("a request the capability took reached Knowledge %d times", n)
		}
	})

	t.Run("no authorised evidence ends the turn failed and never says why", func(t *testing.T) {
		for _, reason := range []rag.NoSupportedEvidenceReason{
			rag.NoSupportedEvidenceReasonNoMatch, rag.NoSupportedEvidenceReasonClaimsUnsupported,
			rag.NoSupportedEvidenceReasonEvidenceUnavailable, rag.NoSupportedEvidenceReasonEgressRestricted,
		} {
			e := knowledgeEngineEnv(t, knowledgeReply(rag.AnswerResult{Value: rag.NoSupportedEvidence{Reason: reason, Notices: rag.RetrievalNotices{}}}))
			res := e.say(memberScope(), knowledgeSay)
			wantAct(t, res, "report_no_evidence")
			if res.Code != contracts.ErrNoSupportedEvidence || !res.Failed() {
				t.Fatalf("%s: code = %q", reason, res.Code)
			}
			if rc := res.Directive.ReasonCode; rc == nil || string(*rc) != "no_supported_evidence" {
				t.Fatalf("%s: reason_code = %v", reason, rc)
			}
			if len(res.Directive.Sources) != 0 || len(KnowledgeCitationEvents(res)) != 0 {
				t.Fatalf("%s: a turn with no evidence carries sources", reason)
			}
			b, _ := json.Marshal(res.Directive)
			for _, leak := range []string{"egress", "restricted", string(reason)} {
				mustNotContain(t, strings.ToLower(res.Response.Text+" "+string(b)), strings.ToLower(leak), "the result for "+string(reason))
			}
		}
	})

	t.Run("a failure to answer is a typed failure, and a caller that goes away ends the turn", func(t *testing.T) {
		e := knowledgeEngineEnv(t, knowledgeFailing(contracts.New(contracts.ErrUnavailable, "the model is down")))
		res := e.say(memberScope(), knowledgeSay)
		wantAct(t, res, "report_failure")
		if res.Code != contracts.ErrUnavailable || len(res.Directive.Sources) != 0 {
			t.Fatalf("code = %s, sources = %v", res.Code, res.Directive.Sources)
		}
		gone := knowledgeEngineEnv(t, &knowledgeFake{answer: func(context.Context, rag.AnswerRequest) (rag.AnswerResult, error) {
			return rag.AnswerResult{}, context.Canceled
		}})
		if _, err := gone.eng.Turn(ctxb(), memberScope(), TurnInput{Text: knowledgeSay}); err == nil {
			t.Fatal("a cancelled turn returned a result")
		}
		if n := gone.session(memberScope()).TurnCount; n != 0 {
			t.Fatalf("a cancelled turn was saved (%d turns)", n)
		}
	})

	t.Run("a bridge over an engine that has the knowledge does not ask twice", func(t *testing.T) {
		k := knowledgeReply(rag.AnswerResult{Value: rag.NoSupportedEvidence{Reason: rag.NoSupportedEvidenceReasonNoMatch}})
		e := knowledgeEngineEnv(t, k)
		res := e.knowledgeSay(knowledgeBridge(t, e, k), memberScope(), knowledgeSay, "en")
		wantAct(t, res, "report_no_evidence")
		if n := len(k.calls()); n != 1 {
			t.Fatalf("Knowledge was asked %d times", n)
		}
	})
}

// A refusal for a permission, a scope or a quarantine is a refusal: the engine's hook sits
// where the router says nothing matches and never on the paths that carry those codes.
func TestPermissionRefusalNeverBecomesAKnowledgeTurn(t *testing.T) {
	asked := func(t *testing.T, k *knowledgeFake, res Result) {
		t.Helper()
		if n := len(k.calls()); n != 0 || KnowledgeTurn(res) {
			t.Fatalf("a refusal reached Knowledge (%d calls, knowledge turn %v)", n, KnowledgeTurn(res))
		}
		if hasStage(res, StageKnowledge) {
			t.Fatalf("stages = %v", res.Trace.Stages)
		}
	}
	t.Run("a capability the package declares and the caller may not use", func(t *testing.T) {
		for name, tc := range map[string]struct {
			sc   contracts.Scope
			cap  string
			want contracts.ErrCode
		}{
			"create, as a visitor":                        {scopeFor("visitor", capGet, capCreate), capCreate, contracts.ErrPermissionDenied},
			"create, with a token that does not carry it": {scopeFor("visitor", capGet), capCreate, contracts.ErrScopeViolation},
			"admin, as a member":                          {memberScope(), capAdmin, contracts.ErrPermissionDenied},
		} {
			k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
			e := knowledgeEngineEnv(t, k)
			e.onProposal(proposalJSON(tc.cap, "task", map[string]any{"name": "widget", "quantity": 3}))
			res := e.say(tc.sc, sayCreate)
			wantAct(t, res, "refuse")
			if res.Code != tc.want {
				t.Fatalf("%s: code = %s, want %s", name, res.Code, tc.want)
			}
			asked(t, k, res)
			e.noHostWrites()
		}
	})
	t.Run("a quarantined application and a package of another application", func(t *testing.T) {
		for name, cause := range map[string]error{
			"quarantined":     contracts.New(contracts.ErrQuarantined, "the package is not active"),
			"scope violation": contracts.New(contracts.ErrScopeViolation, "another application"),
		} {
			k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
			e := knowledgeEngineEnv(t, k, withDeps(func(d *Deps) {
				d.Knowledge = k
				d.Packages = PackageSourceFunc(func(context.Context, string) (capabilities.Package, error) { return nil, cause })
			}))
			res := e.say(memberScope(), knowledgeSay)
			if res.Code != contracts.CodeOf(cause) {
				t.Fatalf("%s: code = %q, want %q", name, res.Code, contracts.CodeOf(cause))
			}
			asked(t, k, res)
		}
	})
	t.Run("an invalid scope is refused before anything is asked", func(t *testing.T) {
		k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
		e := knowledgeEngineEnv(t, k)
		bad := memberScope()
		bad.TenantID = ""
		if _, err := e.eng.Turn(ctxb(), bad, TurnInput{Text: knowledgeSay}); err == nil {
			t.Fatal("a turn without a tenant ran")
		}
		if n := len(k.calls()); n != 0 {
			t.Fatalf("Knowledge was asked %d times under an invalid scope", n)
		}
	})
	t.Run("a choice, a confirmation and a rejection are never questions", func(t *testing.T) {
		k := knowledgeReply(rag.AnswerResult{Value: knowledgeGroundedAnswer()})
		e := knowledgeEngineEnv(t, k)
		for name, in := range map[string]TurnInput{
			"a choice":       {ChoiceID: "opt_1"},
			"a rejection":    {Reject: "prep_1"},
			"a confirmation": {Confirm: &Confirmation{PreparationID: "prep_1"}},
		} {
			_, _ = e.eng.Turn(ctxb(), memberScope(), in)
			if n := len(k.calls()); n != 0 {
				t.Fatalf("%s reached Knowledge", name)
			}
		}
	})
}
