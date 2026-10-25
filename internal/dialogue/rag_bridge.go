package dialogue

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// The knowledge path of the turn pipeline (master plan 11.1, 12.4, 12.5, rule 9):
// a text turn that no capability of the application handles is a question for the
// sources the caller may read, and it is answered from them, with citations, or not
// at all.
//
//	text turn -> the engine's own pipeline (scope and rate, goal, rules, router,
//	             controller): a capability handles it, or it ends in the refusal
//	             unknown_capability with no capability named
//	          -> that refusal, and only that, becomes a question to Knowledge
//	             (the same retrieval and grounded answerer as POST /v1/rag/answer)
//	          -> present_result with the answer's citations as the directive's sources,
//	             or report_no_evidence, or a typed report_failure
//	          -> the engine's own completion: audit, state, the verified response
//
// # Authority
//
// The route is deterministic: it is decided from the engine's own outcome (the
// refusal of the router, or the controller's out_of_scope intent), never from what a
// model says the question is about, and a request any capability handles never
// reaches Knowledge. The verified scope is passed as it is; the question is the
// user's text, clipped to the API's bound, and is data to the sources, never an
// instruction to the core. What comes back is checked before it is used: an answer
// without a claim, a claim without a citation or a citation without claims is not an
// answer (it is a typed failure), so a response can never state a document claim
// without a source. The model of the answerer proposes claims and code decides which
// survive; this package writes none of the text of an answer.
//
// The response is the deterministic template of the directive (internal/verify): the
// claims are already verified against the passages they cite, quoted verbatim with
// their [n] markers and a list of the sources, and a second model call would only
// rephrase them (master plan 3 rule 16: the no-model option first). The turn is
// recorded like any other: the dialogue.turn audit event, the transcript and the
// session's last-directive summary.
//
// # What the outcomes are
//
//   - an answer: act present_result, the citations (client citations only: they carry
//     no data-egress mode and no classification) as the directive's sources; the
//     turn is completed. KnowledgeCitationEvents are its citation events.
//   - no authorised evidence supports an answer (the typed result of the answerer,
//     whatever the reason, egress_restricted included: the reasons never say whether a
//     source the principal may not see exists): act report_no_evidence, reason_code
//     and Result.Code no_supported_evidence.
//   - the question could not be answered (the model or the sources unavailable, a
//     timeout, a scope the sources refuse): the typed report_failure or refuse of the
//     engine. Never "no evidence" for an error, and never uncited text.
//
// # Limits, stated
//
// A knowledge question that a capability of the package also matches is the
// capability's: the router decides that first, and the controller may still call it
// out_of_scope, which sends it here. A message that is not a question ("asdf") is
// answered with no_supported_evidence like any other that finds nothing. The router
// and the controller are not asked to tell a knowledge question from noise: the
// retrieval is cheap, and a model is called only when evidence was found.
//
// # How it is wired
//
// The engine routes the question itself (M2-18): Deps.Knowledge, or Engine.SetKnowledge
// for an engine that is built before the RAG area is, makes every point where a text
// turn would end in the refusal unknown_capability with no capability named (the router
// found nothing the caller may use, the controller called the request out_of_scope or
// proposed a capability that is not offered or declared, no capability could be
// described) ask Knowledge first (turn.unhandled). A refusal for a permission, a scope or
// a quarantine is another code and another place, and never reaches it.
//
// RAGBridge is the earlier shape of the same step, for an engine that was given no
// Knowledge: Bridge.Turn is Engine.Turn with the step added after serve. Both call
// turn.askKnowledge, so the two answer the same question the same way, and
// TestRAGBridgeMatchesEngineForCapabilityTurns fails if the bridge ever diverges from the
// engine for a turn that is not a knowledge question. A serve process does not use the
// bridge.

// Knowledge answers a question from the sources the principal may read, with
// citations, or says that no authorised evidence supports an answer. It is the
// Answer method of the gateway's RAGQueries (NewRAGQueries), over the same retrieval
// pipeline and the one answerer of the process, so a dialogue turn and POST
// /v1/rag/answer give the same answer and a citation of a turn resolves like one of
// the API. The egress filter of the external provider mode acts inside it, before the
// answer is generated; this package knows nothing of it.
type Knowledge interface {
	Answer(ctx context.Context, scope contracts.Scope, req rag.AnswerRequest) (rag.AnswerResult, error)
}

// StageKnowledge is the stage of a turn whose question went to Knowledge. It is
// recorded in Trace.Stages, after StageGoal and before StageVerify.
const StageKnowledge = "knowledge"

// knowledgeSlot holds the Knowledge of an engine behind an atomic pointer (an interface
// value cannot be swapped atomically by itself).
type knowledgeSlot struct{ k Knowledge }

// SetKnowledge gives the engine the Knowledge its text turns fall back to for a request no
// capability handles (see Deps.Knowledge), or, with nil, takes it away. It belongs to the
// composition root and is meant to be called before the engine serves its first turn (the
// engine is built before the area that provides Knowledge is), but it is safe to call at
// any time: a turn reads the value once, when it reaches the point that needs it.
func (e *Engine) SetKnowledge(k Knowledge) {
	if k == nil {
		e.knowledge.Store(nil)
		return
	}
	e.knowledge.Store(&knowledgeSlot{k: k})
}

// knowledgeSource is the engine's Knowledge, nil when it has none.
func (e *Engine) knowledgeSource() Knowledge {
	if s := e.knowledge.Load(); s != nil {
		return s.k
	}
	return nil
}

// knowledgeMaxQuestionRunes is the bound of a question of the RAG API
// (schemas/rag/answer-request).
const knowledgeMaxQuestionRunes = 2000

// knowledgeMaxCitations is the bound of the sources of a directive
// (schemas/conversation/dialogue-directive).
const knowledgeMaxCitations = 32

// RAGBridge runs turns like an Engine and sends a text question that no capability
// handles to Knowledge. It is safe for concurrent use, like the engine under it.
type RAGBridge struct {
	e *Engine
	k Knowledge
}

// NewRAGBridge returns the bridge. Both are required: a bridge with no engine runs
// nothing, and one with no knowledge would answer a question from nowhere.
func NewRAGBridge(e *Engine, k Knowledge) (*RAGBridge, error) {
	switch {
	case e == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "dialogue: the RAG bridge needs an engine")
	case k == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "dialogue: the RAG bridge needs a knowledge source")
	}
	return &RAGBridge{e: e, k: k}, nil
}

// Engine returns the engine the bridge runs the turns of.
func (b *RAGBridge) Engine() *Engine { return b.e }

// Turn runs one turn: exactly Engine.Turn, except that a text turn the engine ends
// in the refusal unknown_capability, naming no capability, is a question to Knowledge
// and ends in its answer. A choice, a confirmation and a rejection are never
// questions and are the engine's alone.
func (b *RAGBridge) Turn(ctx context.Context, scope contracts.Scope, in TurnInput) (Result, error) {
	if strings.TrimSpace(in.Text) == "" || in.ChoiceID != "" || in.Confirm != nil || in.Reject != "" {
		return b.e.Turn(ctx, scope, in)
	}
	e := b.e
	t := &turn{e: e, scope: scope, in: in, id: e.ids.New("trn"), now: e.clk.Now(), hints: map[string]string{}}
	return b.run(ctx, t)
}

// run is turn.run with the knowledge step between serve and complete.
func (b *RAGBridge) run(ctx context.Context, t *turn) (Result, error) {
	if err := t.admit(ctx); err != nil {
		return Result{}, err
	}
	key := t.scope.ApplicationID + "\x00" + t.scope.TenantID + "\x00" + t.scope.WorkspaceID + "\x00" + t.scope.SessionID
	unlock, err := t.e.locks.lock(ctx, key)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	t.stage(StageGoal)
	sess, _, err := t.e.sessions.Reconcile(ctx, t.scope, nil, t.e.actions)
	if err != nil {
		return Result{}, err
	}
	t.sess = sess
	if err := t.expire(ctx); err != nil {
		return Result{}, err
	}

	out, err := t.serve(ctx)
	if err != nil {
		return Result{}, err
	}
	if knowledgeQuestion(out) {
		if out, err = b.knowledge(ctx, t); err != nil {
			return Result{}, err
		}
	}
	return t.complete(ctx, out)
}

// knowledgeQuestion reports whether the engine ended a text turn in the refusal of a
// request no capability handles: the refusal unknown_capability that names no
// capability. A refusal for a permission, a scope or a quarantine is a refusal, and a
// request a capability took is that capability's.
func knowledgeQuestion(out outcome) bool {
	d := out.dir
	return d != nil && d.Act == conversation.DialogueDirectiveActRefuse && out.code == contracts.ErrUnknownCapability &&
		d.CapabilityID == nil && out.receipt == nil && out.prepared == nil
}

// knowledge asks Knowledge the user's question in the verified scope and turns its
// answer into the outcome of the turn. A cancelled caller is an error of the turn; any
// other failure is a typed outcome, never an answer.
func (b *RAGBridge) knowledge(ctx context.Context, t *turn) (outcome, error) {
	return t.askKnowledge(ctx, b.k)
}

// unhandled is the end of a text turn that no capability handles: the refusal
// unknown_capability, naming no capability, unless the engine has a Knowledge, in which
// case the question goes to the sources first and the turn ends in what they answer. It is
// the one place the engine's pipeline asks Knowledge, so a permission, scope or quarantine
// refusal (which are other calls to refuse) can never become a knowledge turn.
func (t *turn) unhandled(ctx context.Context) (outcome, error) {
	if k := t.e.knowledgeSource(); k != nil {
		return t.askKnowledge(ctx, k)
	}
	return t.refuse(contracts.ErrUnknownCapability, ""), nil
}

// askKnowledge asks k the user's question in the verified scope and turns its answer into
// the outcome of the turn. A cancelled caller is an error of the turn; any other failure
// is a typed outcome, never an answer.
func (t *turn) askKnowledge(ctx context.Context, k Knowledge) (outcome, error) {
	t.stage(StageKnowledge)
	question := t.userText
	if utf8.RuneCountInString(question) > knowledgeMaxQuestionRunes {
		question = string([]rune(question)[:knowledgeMaxQuestionRunes])
	}
	res, err := k.Answer(ctx, t.scope.Clone(), rag.AnswerRequest{Question: rag.SearchRequestQuery(question)})
	if err != nil {
		if isCanceled(err) {
			return outcome{}, providers.ContextError(err)
		}
		code := failureCode(err, contracts.ErrUnavailable)
		t.log("knowledge failed", "code", string(code))
		return t.failure(code, ""), nil
	}
	switch v := res.Value.(type) {
	case rag.GroundedAnswer:
		return t.knowledgeAnswered(v), nil
	case *rag.GroundedAnswer:
		if v != nil {
			return t.knowledgeAnswered(*v), nil
		}
	case rag.NoSupportedEvidence:
		return t.knowledgeNoEvidence(), nil
	case *rag.NoSupportedEvidence:
		if v != nil {
			return t.knowledgeNoEvidence(), nil
		}
	}
	t.log("knowledge answered with something that is not an answer")
	return t.failure(contracts.ErrInternal, ""), nil
}

// knowledgeAnswered is the outcome of a grounded answer: present_result, with the
// citations of the answer as the sources the response may cite. An answer that is not
// grounded, whatever it says, is a failure: nothing uncited is ever presented.
func (t *turn) knowledgeAnswered(a rag.GroundedAnswer) outcome {
	if !knowledgeGrounded(a) {
		t.log("knowledge answered without provenance")
		return t.failure(contracts.ErrInternal, "")
	}
	d := t.newDirective(conversation.DialogueDirectiveActPresentResult, "")
	d.Sources = append(d.Sources, a.Citations...)
	// The response is the template of the directive: the claims were verified against
	// the passages they cite and the template quotes them, so a second model call
	// would only rephrase them. The template-only switch of a turn is modelDown.
	t.modelDown = true
	return outcome{dir: d}
}

// knowledgeNoEvidence is the outcome of the typed absence of an answer.
func (t *turn) knowledgeNoEvidence() outcome {
	d := t.newDirective(conversation.DialogueDirectiveActReportNoEvidence, "")
	d.ReasonCode = errorCode(contracts.ErrNoSupportedEvidence)
	return outcome{dir: d, code: contracts.ErrNoSupportedEvidence}
}

// knowledgeGrounded is the provenance rule of master plan 3 rule 9: the answer has at
// least one claim and one citation, every claim cites at least one citation that
// exists, and every citation names a source and the claims it supports.
func knowledgeGrounded(a rag.GroundedAnswer) bool {
	if len(a.Claims) == 0 || len(a.Citations) == 0 || len(a.Citations) > knowledgeMaxCitations {
		return false
	}
	for _, c := range a.Citations {
		if c.SourceID == "" || len(c.Claims) == 0 || strings.TrimSpace(string(c.DisplayName)) == "" {
			return false
		}
	}
	for _, cl := range a.Claims {
		if len(cl.CitationIndexes) == 0 {
			return false
		}
		for _, i := range cl.CitationIndexes {
			if i < 0 || i >= int64(len(a.Citations)) {
				return false
			}
		}
	}
	return true
}

// KnowledgeTurn reports whether a turn's question went to Knowledge.
func KnowledgeTurn(res Result) bool {
	for _, s := range res.Trace.Stages {
		if s == StageKnowledge {
			return true
		}
	}
	return false
}

// KnowledgeCitationEvents are the citation events of a knowledge turn, one per
// source of its directive, in order, as drafts that take the sequence number the
// stream assigns when the event is published: the same shape as the other events a
// route builds before it publishes them. They are the core's own data (the directive's
// sources), never taken from model output. A turn that did not answer a question from
// sources has none.
func KnowledgeCitationEvents(res Result) []func(seq conversation.Seq) conversation.TurnEventValue {
	if !KnowledgeTurn(res) || res.Directive.Act != conversation.DialogueDirectiveActPresentResult {
		return nil
	}
	turnID := act.TurnID(res.TurnID)
	out := make([]func(seq conversation.Seq) conversation.TurnEventValue, 0, len(res.Directive.Sources))
	for _, c := range res.Directive.Sources {
		c := c
		out = append(out, func(seq conversation.Seq) conversation.TurnEventValue {
			return conversation.CitationEvent{TurnID: turnID, Seq: seq, Citation: c}
		})
	}
	return out
}

// HasKnowledge reports whether the engine has a Knowledge: whether a text request no capability
// handles is asked of the sources (Deps.Knowledge, SetKnowledge) or refused. The composition
// root checks it so that a deployment cannot believe it has the route and not have it.
func (e *Engine) HasKnowledge() bool { return e.knowledgeSource() != nil }
