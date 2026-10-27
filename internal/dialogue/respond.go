package dialogue

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/analyst/loop"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// turnAudit is the data of the dialogue.turn audit event: what was decided and how
// big the model calls were. It holds identifiers, codes, names and integers, never
// the user's text, a prompt, a fact or a confirmation secret.
type turnAudit struct {
	TurnID        string   `json:"turn_id"`
	Act           string   `json:"act"`
	CapabilityID  string   `json:"capability_id,omitempty"`
	Code          string   `json:"code,omitempty"`
	Language      string   `json:"language"`
	Route         string   `json:"route,omitempty"`
	Decision      string   `json:"decision,omitempty"`
	ModelCalls    int      `json:"model_calls"`
	PromptToks    int      `json:"prompt_toks"`
	Response      string   `json:"response"`
	Transitions   []string `json:"transitions,omitempty"`
	UIAction      string   `json:"ui_action,omitempty"`
	PreparationID string   `json:"preparation_id,omitempty"`
	ReceiptID     string   `json:"receipt_id,omitempty"`
}

// eventTurn is the audit event type of a completed turn. Policy decisions and the
// action lifecycle are recorded by the gateway and the coordinator, each fail
// closed; this one summarises the turn and is written after the fact.
var eventTurn = audit.MustRegister[turnAudit](audit.TypeSpec{
	Name:        "dialogue.turn",
	Description: "a dialogue turn finished: the act, the capability, the typed code and the model call sizes",
	Levels:      audit.LevelTenant,
	Required:    []string{"turn_id", "act"},
})

// persistTimeout bounds the work done after a commit, on a context that no longer
// depends on the caller.
const persistTimeout = 15 * time.Second

// complete is the last two steps of the pipeline: the verified natural response,
// and the state the next turn starts from.
func (t *turn) complete(ctx context.Context, out outcome) (Result, error) {
	defer loop.ForgetClientCapabilities(t.id)
	d := out.dir
	opts := verify.Options{Language: t.lang, FieldLabels: t.labelsFor(out.capID)}
	if d.Act == conversation.DialogueDirectiveActPresentResult {
		opts.Presentation = t.presentationFor(out.capID)
		opts.Table = verify.BuildRecordTable(out.resultIDs, out.resultValues, opts.FieldLabels)
	}

	resp, rerr := t.respond(ctx, out, opts)
	t.stage(StageVerify)
	if rerr == nil {
		t.trace.Response, t.trace.ResponseReason = resp.Source, resp.Reason
	}

	// Once a commit may have happened, the rest of the turn does not depend on the
	// caller still being there: the receipt must be recorded and returned.
	pctx := ctx
	if t.committed {
		var cancel context.CancelFunc
		pctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
		defer cancel()
	}

	// A nested goal that finished returns control to its parent before the turn is
	// audited and saved. After a commit the turn does not fail for it: the receipt must
	// reach the caller, and the finished child is completed by the user's next request at
	// the latest.
	if err := t.settle(pctx, out); err != nil {
		if !t.committed {
			return Result{}, err
		}
		t.e.log.Error("nested goal not settled after a commit", "turn_id", t.id, "code", string(contracts.CodeOf(err)))
	}

	// The turn is audited before its state is saved: an unaudited turn is not
	// acknowledged (audit.Appender: an append error means do not proceed). If a
	// commit may already have happened, the Result still carries its receipt, with
	// the error beside it.
	auditErr := t.writeAudit(pctx, out)
	if auditErr != nil && !t.committed {
		return Result{}, auditErr
	}

	t.stage(StagePersist)
	t.finishState(d)
	// Dual form (ADR 0049): session_turns get the model-window body (tokens);
	// transcript_turns get the resolved client text via AssistantDisplay.
	assist := resp.Text
	if out.analystModelText != "" {
		assist = out.analystModelText
	}
	rec := &sessions.TurnRecord{TurnID: t.id, Act: string(d.Act), Assistant: assist}
	if assist != resp.Text {
		rec.AssistantDisplay = resp.Text
	}
	if t.in.Text != "" {
		rec.User = t.userText
	}
	if _, err := t.e.sessions.Save(pctx, t.sess, rec); err != nil {
		if !t.committed {
			return Result{}, err
		}
		// The commit happened and its receipt is durable in the action store; only
		// the conversation state could not be written. The user is still told.
		t.e.log.Error("turn state not saved after a commit", "turn_id", t.id, "code", string(contracts.CodeOf(err)))
	}

	res := Result{
		TurnID: t.id, Language: t.lang, Directive: *d, Response: resp, Prepared: out.prepared,
		Confirmation: out.token, Receipt: out.receipt, Code: out.code, Details: out.details,
		Trace: t.trace, UIAction: out.ui, MemoryProposals: t.memProps,
	}
	t.log("turn done", "act", string(d.Act), "code", string(out.code), "model_calls", len(t.trace.ModelCalls),
		"response", string(t.trace.Response), "stages", strings.Join(t.trace.Stages, ">"))
	if auditErr != nil {
		return res, auditErr
	}
	if rerr != nil {
		// The directive or the template could not be verified: a bug in the core, not
		// something to paper over. The structured parts are still returned.
		return res, contracts.Wrap(contracts.ErrInternal, rerr, "dialogue: the response could not be produced")
	}
	// The reply is final here: verify.Realise or verify.Degraded produced it, whoever wrote the text, and the turn
	// is acknowledged. Only now is it announced as a verified reply (Deps.OnReply).
	t.announceReply(pctx, resp)
	return res, nil
}

// respond produces the response for a directive. The deterministic template is the
// answer when the act is one a model has nothing to add to, when no model is
// available, and whenever the model's text fails the verifier; the model's text is
// used only when it passes. Unverified model text is never returned.
func (t *turn) respond(ctx context.Context, out outcome, opts verify.Options) (verify.Outcome, error) {
	d := out.dir
	if out.analystReply != "" || len(out.analystBlocks) > 0 {
		src, reason := verify.SourceModel, verify.ReasonVerified
		if out.analystReply == "" {
			out.analystReply = " " // non-empty so callers treat it as a finished reply
		}
		return verify.Outcome{
			Text: out.analystReply, Source: src, Reason: reason,
			Blocks: out.analystBlocks, CoreMaterialised: out.analystCore,
		}, nil
	}
	if t.modelDown || t.templateOnly(d.Act) {
		return verify.Degraded(d, out.receipt, opts)
	}
	if o, ok, err := t.realiseExternally(ctx, out, opts); ok {
		return o, err
	}
	built, err := buildResponse(responseInput{
		App: t.scope.ApplicationID, Lang: t.lang, Directive: d, Receipt: out.receipt, Labels: opts.FieldLabels,
	}, t.e.respBudget, t.e.cfg.Profile.MaxOutputTokens)
	if err != nil {
		t.recordCall(ModelCall{Purpose: "response", Err: failureCode(err, contracts.ErrValidationFailed)})
		return verify.Degraded(d, out.receipt, opts)
	}
	if t.e.cfg.StreamVerified {
		if o, ok, err := t.respondStream(ctx, built, out, opts); ok {
			return o, err
		}
	}
	res, call, err := t.callModel(ctx, built)
	t.recordCall(call)
	if err != nil {
		return verify.Degraded(d, out.receipt, opts)
	}
	return verify.RealiseRich(res.Text, decodeProposedBlocks(res), d, out.receipt, opts)
}

// realiseExternally hands the directive to the engine's ResponseRealiser, when it has one that takes the
// turn (see ResponseRealiser). ok=false: no realiser, or it declined, and the engine's own local path
// runs unchanged. When it took the turn, the outcome is the realiser's, checked again: model text is
// verified here against the directive and receipt, anything else is the deterministic template. A
// realiser error is the template too, never a different answer.
func (t *turn) realiseExternally(ctx context.Context, out outcome, opts verify.Options) (verify.Outcome, bool, error) {
	r := t.e.responseRealiser()
	if r == nil {
		return verify.Outcome{}, false, nil
	}
	d := out.dir
	got, handled, err := r.RealiseResponse(ctx, ResponseInput{Scope: t.scope.Clone(), Directive: d, Receipt: out.receipt, Options: opts})
	if !handled {
		return verify.Outcome{}, false, nil
	}
	call := ModelCall{Purpose: "response", Provider: "realiser"}
	if err != nil {
		call.Err = failureCode(err, contracts.ErrUnavailable)
	}
	t.recordCall(call)
	if err != nil {
		o, derr := verify.Degraded(d, out.receipt, opts)
		return o, true, derr
	}
	if got.Source == verify.SourceModel {
		o, verr := verify.RealiseRich(got.Text, got.Blocks, d, out.receipt, opts)
		return o, true, verr
	}
	o, derr := verify.Degraded(d, out.receipt, opts)
	if derr == nil && got.Reason == verify.ReasonVerificationFailed {
		o.Reason, o.Violations = got.Reason, got.Violations
	}
	return o, true, derr
}

// presentationFor reads the optional presentation hint of the capability's conversation contract.
func (t *turn) presentationFor(capID string) *verify.Presentation {
	if t.pkg == nil || capID == "" {
		return nil
	}
	ct, ok := t.pkg.ContractFor(capID, string(t.lang))
	if !ok || ct.Presentation == nil || ct.Presentation.Chart == nil {
		return nil
	}
	ch := ct.Presentation.Chart
	return &verify.Presentation{Chart: &verify.ChartHint{
		Type: string(ch.Type), X: string(ch.X), Y: string(ch.Y),
	}}
}

// decodeProposedBlocks reads rich blocks a provider left on ChatResult.JSON (extmode packs
// candidate JSON objects there for a response turn). Malformed payloads are ignored;
// RealiseRich validates every block against the directive's facts.
func decodeProposedBlocks(res providers.ChatResult) []conversation.RichBlock {
	if len(res.JSON) == 0 {
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(res.JSON, &raw); err != nil || len(raw) == 0 {
		return nil
	}
	var out []conversation.RichBlock
	for _, r := range raw {
		var b conversation.RichBlock
		if err := json.Unmarshal(r, &b); err != nil || b.Value == nil {
			continue
		}
		out = append(out, b)
	}
	return out
}

func (t *turn) templateOnly(a conversation.DialogueDirectiveAct) bool {
	for _, x := range t.e.cfg.TemplateOnly {
		if x == a {
			return true
		}
	}
	return false
}

// finishState records what the next turn needs: the goal's idle expiry and a
// summary of the directive.
func (t *turn) finishState(d *conversation.DialogueDirective) {
	if g := t.sess.State.Goal; g != nil {
		g.UpdatedAt = t.now
		g.ExpiresAt = t.now.Add(t.e.sessions.GoalTTL())
		t.sess.State.Last = summarise(d)
		return
	}
	t.sess.State.Last = nil
}

// labelsFor returns the labels the application gives the fields of a capability in
// the response language, so a response (and the Italian templates) name a field the
// way the application does. A term whose ID is the field's name supplies a label for
// a field the contract has no prompt for.
func (t *turn) labelsFor(capID string) map[string]string {
	if t.pkg == nil {
		return nil
	}
	labels := map[string]string{}
	lang := string(t.lang)
	for _, term := range t.pkg.Terminology() {
		if l, ok := term.Labels[lang]; ok && strings.TrimSpace(string(l.Preferred)) != "" {
			labels[strings.ReplaceAll(string(term.ID), "-", "_")] = string(l.Preferred)
		}
	}
	if capID != "" {
		if ct, ok := t.pkg.ContractFor(capID, lang); ok {
			for _, fp := range ct.FieldPrompts {
				if s := strings.TrimSpace(string(fp.Label)); s != "" {
					labels[string(fp.Field)] = s
				}
			}
		}
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

// writeAudit appends the dialogue.turn event. Policy decisions and the action
// lifecycle are recorded by their own packages, each fail closed; this event
// summarises the turn, and it fails closed too: a turn that cannot be audited is not
// acknowledged. With no Appender configured no event is written.
func (t *turn) writeAudit(ctx context.Context, out outcome) error {
	if t.e.audit == nil {
		return nil
	}
	loc, err := audit.ForScope(t.scope)
	if err != nil {
		return err
	}
	d := out.dir
	ev := turnAudit{
		TurnID: t.id, Act: string(d.Act), Language: string(t.lang), Route: string(t.trace.Route),
		Decision: t.trace.Decision, ModelCalls: len(t.trace.ModelCalls), Response: string(t.trace.Response),
		Transitions: t.trace.Transitions, UIAction: t.trace.UIAction,
	}
	if d.CapabilityID != nil {
		ev.CapabilityID = string(*d.CapabilityID)
	}
	if out.code != "" {
		ev.Code = string(out.code)
	}
	for _, c := range t.trace.ModelCalls {
		ev.PromptToks += c.PromptToks
	}
	if d.PreparationID != nil {
		ev.PreparationID = string(*d.PreparationID)
	}
	if d.ReceiptID != nil {
		ev.ReceiptID = string(*d.ReceiptID)
	}
	if _, err := eventTurn.Append(ctx, t.e.audit, loc, ev); err != nil {
		t.e.log.Warn("turn audit event not written", "turn_id", t.id, "code", string(contracts.CodeOf(err)))
		return contracts.Wrap(contracts.ErrUnavailable, err, "dialogue: the turn could not be audited")
	}
	return nil
}
