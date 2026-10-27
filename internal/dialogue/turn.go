package dialogue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/capabilities"
	dctxpkg "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	mem "github.com/buxee/alix-ai/pkg/contracts/gen/memory"
)

// turn is the state of one turn while the pipeline runs. A turn is used by one
// goroutine; the engine serialises the turns of a session.
type turn struct {
	e     *Engine
	scope contracts.Scope
	in    TurnInput
	id    string
	now   time.Time

	// userText is the user's text, bounded.
	userText string
	// attachments are the bounded files of a text turn (TurnInput.Attachments), kept apart from
	// userText: they reach the proposal prompt of this turn and nothing else.
	attachments []Attachment
	trace       Trace
	sess        sessions.Session
	pkg         capabilities.Package
	lang        verify.Language
	langSet     bool
	route       capabilities.Result
	// hints are entity IDs the model proposed, by field: a hint for the host's
	// resolver, never a value.
	hints map[string]string
	// committed is true once a receipt exists, or a commit was sent with an outcome
	// that could not be recorded: from then on the turn reports it even if a later
	// step fails.
	committed bool
	// modelDown is true once the model was unavailable in this turn, so the
	// response is the template without a second doomed call.
	modelDown bool
	// lapsed is the preparation whose own expiry passed before this turn, if any.
	lapsed lapsedPrep
	// ui is what the turn knows about the UI tools of the client (nil when none were declared) and uiAct the UI
	// action of the validated proposal, if any: data for the client, never read by the pipeline.
	ui    *uiState
	uiAct *UIAction
	// memProps are assistant memory proposals stored this turn (ADR 0042).
	memProps []mem.MemoryItem
}

// Turn runs one turn of the pipeline of master plan 11.1:
//
//	scope and rate checks -> active goal lookup -> deterministic rules for exact
//	cases -> capability shortlist -> optional decision stage -> bounded context ->
//	constrained TaskProposal -> schema validation -> entity resolution -> host
//	reauthorisation and deterministic validation -> DialogueDirective or
//	PreparedAction -> confirmation -> idempotent commit -> ActionReceipt ->
//	verified response.
//
// An error is returned when the turn could not be run at all: no valid scope, an
// invalid input, an unsupported language, a rate limit, a session that is not the
// presented context, a store that failed before anything was decided. Everything
// else is a Result: a refusal or failure is a directive with a typed Code and a
// response that says so, never a success-looking one. The model never decides a
// permission, a fact, a validation or a commit: it proposes a task and phrases a
// directive, and everything it writes is validated or verified before it is used.
func (e *Engine) Turn(ctx context.Context, scope contracts.Scope, in TurnInput) (Result, error) {
	t := &turn{e: e, scope: scope, in: in, id: e.ids.New("trn"), now: e.clk.Now(), hints: map[string]string{}}
	return t.run(ctx)
}

func (t *turn) stage(name string) { t.trace.Stages = append(t.trace.Stages, name) }

func (t *turn) log(msg string, args ...any) {
	t.e.log.Info(msg, append([]any{"turn_id", t.id}, args...)...)
}

// admit is the scope and rate check: a valid scope, one well-formed input, a
// supported language and a turn within the rate limit. There is no default scope.
func (t *turn) admit(ctx context.Context) error {
	if ctx == nil {
		return contracts.New(contracts.ErrInternal, "dialogue: no context")
	}
	if err := ctx.Err(); err != nil {
		return providers.ContextError(err)
	}
	t.stage(StageScope)
	if err := t.scope.Validate(); err != nil {
		return err
	}
	modes := 0
	text := strings.TrimSpace(t.in.Text)
	if text != "" {
		modes++
		t.userText = clipRunesPlain(text, t.e.cfg.Profile.MaxUtteranceRunes)
	}
	if t.in.ChoiceID != "" {
		modes++
	}
	if c := t.in.Confirm; c != nil {
		modes++
		if c.PreparationID == "" || c.Token.IsZero() {
			return contracts.New(contracts.ErrConfirmationRequired, "a confirmation needs the preparation ID and its token")
		}
	}
	if t.in.Reject != "" {
		modes++
	}
	if modes != 1 {
		return contracts.New(contracts.ErrValidationFailed, "a turn carries exactly one of text, a choice, a confirmation or a rejection")
	}
	if len(t.in.Attachments) > 0 {
		// Attachments belong to a text message: a choice, a confirmation or a rejection has none to
		// attach to. They are checked and bounded here, before any state is read or changed.
		if text == "" {
			return attachmentError("attachments_need_text", "attachments go with a text message")
		}
		bounded, err := BoundAttachments(t.in.Attachments)
		if err != nil {
			return err
		}
		t.attachments = bounded
	}
	if tag := strings.TrimSpace(t.in.Language); tag != "" {
		l, err := verify.ParseLanguage(tag)
		if err != nil {
			return err // a typed validation_failed, not a verdict about the text
		}
		t.lang, t.langSet = l, true
	}
	t.stage(StageRate)
	return t.e.limiter.Allow(ctx, t.scope)
}

// clipRunesPlain bounds s to n runes without a marker: it is user text read as is.
func clipRunesPlain(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func (t *turn) run(ctx context.Context) (Result, error) {
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
	// Reconcile is Open that also applies the token this turn was presented with: a
	// permission the token no longer carries discards the goals that relied on it.
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
	return t.complete(ctx, out)
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// serve loads the package and runs the path the input asks for.
func (t *turn) serve(ctx context.Context) (outcome, error) {
	pkg, err := t.e.packages.Package(ctx, t.scope.ApplicationID)
	if err == nil {
		err = t.checkPackage(ctx, pkg)
	}
	if err != nil {
		if isCanceled(err) {
			return outcome{}, providers.ContextError(err)
		}
		t.defaultLanguage(nil)
		return t.failure(failureCode(err, contracts.ErrUnavailable), ""), nil
	}
	t.pkg = pkg
	t.defaultLanguage(pkg)

	switch {
	case t.in.Confirm != nil:
		return t.confirm(ctx)
	case t.in.Reject != "":
		return t.reject(ctx)
	case t.in.ChoiceID != "":
		return t.choice(ctx)
	}
	return t.text(ctx)
}

// failureCode is the typed code of an error, or def for an untyped one.
func failureCode(err error, def contracts.ErrCode) contracts.ErrCode {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce.Code.Valid() {
		return ce.Code
	}
	return def
}

// checkPackage verifies the package a turn is served from: the verified
// application's, active, and the content the registry routes against.
func (t *turn) checkPackage(ctx context.Context, pkg capabilities.Package) error {
	if pkg == nil {
		return contracts.New(contracts.ErrUnavailable, "the application has no active package")
	}
	if pkg.ApplicationID() != t.scope.ApplicationID {
		return contracts.New(contracts.ErrScopeViolation, "the package belongs to another application")
	}
	if pkg.Status() != policy.StatusActive {
		return contracts.New(contracts.ErrQuarantined, "the application's package is not active")
	}
	if t.e.registry != nil {
		info, err := t.e.registry.Info(ctx, t.scope.ApplicationID)
		if err != nil {
			return err
		}
		if info.ContentHash != pkg.ContentHash() {
			return contracts.New(contracts.ErrUnavailable, "the application's package changed while the turn started; try again")
		}
	}
	return nil
}

// defaultLanguage chooses the response language when the host gave none: the
// configured default if the package supports it, else the package's first language
// the verifier supports, else English. Only en and it are supported.
func (t *turn) defaultLanguage(pkg capabilities.Package) {
	if t.langSet {
		return
	}
	t.lang = verify.LangEN
	def, derr := verify.ParseLanguage(t.e.cfg.DefaultLanguage)
	if derr != nil {
		def = verify.LangEN
	}
	if pkg == nil {
		t.lang = def
		return
	}
	var first verify.Language
	for _, tag := range pkg.SupportedLanguages() {
		l, err := verify.ParseLanguage(tag)
		if err != nil {
			continue
		}
		if first == "" {
			first = l
		}
		if l == def {
			t.lang = def
			return
		}
	}
	if first != "" {
		t.lang = first
		return
	}
	t.lang = def
}

// ---- free text ----

// text runs the pipeline for a user message: rules, shortlist, decision stage,
// context, proposal, validation, resolution, host. When the analyst step loop is
// wired and effective abilities include loop + analyst_capable, that path runs
// instead of the single-capability path (ADR 0047).
func (t *turn) text(ctx context.Context) (outcome, error) {
	t.stage(StageRules)
	if out, done, err := t.rules(ctx); done || err != nil {
		return out, err
	}

	if out, handled, err := t.tryAnalyst(ctx); handled || err != nil {
		return out, err
	}

	t.stage(StageShortlist)
	g := t.sess.State.Goal
	route, err := t.e.router.Route(ctx, capabilities.Request{
		Scope:     t.scope,
		Binding:   policy.BindingOf(t.sess.Scope),
		Utterance: t.userText,
		Language:  t.in.Language,
		Goal:      g.Active(),
	})
	if err != nil {
		return t.routeError(err)
	}
	// The field the goal waits for is a task field of the goal's own capability only: the
	// router exposed every candidate without it, and the goal's is decided again with it.
	if task := pendingFields(g); len(task) > 0 {
		for i := range route.Candidates {
			c := &route.Candidates[i]
			if c.CapabilityID != g.CapabilityID || c.Decision.FieldVisible(task[0]) {
				continue
			}
			d := t.e.policy.Decide(ctx, policy.Request{
				Scope: t.scope, Package: t.pkg, CapabilityID: c.CapabilityID, Binding: policy.BindingOf(t.sess.Scope),
				Phase: policy.PhaseExpose, TaskFields: task,
			})
			if !d.Allowed {
				return t.failure(failureCode(d.Err(), contracts.ErrPermissionDenied), c.CapabilityID), nil
			}
			c.Decision = d
		}
	}
	t.route = route
	t.trace.Route = route.Outcome
	t.trace.Shortlist = route.IDs()
	if route.Outcome != capabilities.OutcomeShortlist {
		// Nothing the caller may use matches: the question is for the sources the caller may
		// read, if the engine has any, and a refusal, not a guess, if it has none.
		return t.unhandled(ctx)
	}

	cands := capShortlist(t.decide(ctx, route.Candidates), t.e.cfg.Profile.MaxShortlist)

	tp, out, done, err := t.proposal(ctx, cands)
	if done || err != nil {
		return out, err
	}
	out, err = t.apply(ctx, tp, cands)
	if err == nil {
		t.carryUIAction(&out)
	}
	return out, err
}

// carryUIAction puts the validated UI action of the proposal on the outcome, if the turn ended in an act it may ride
// on. It never rides on a confirmation, a step-up, a refusal, a failure or a cancellation: those turns end with
// nothing for the client's screen to do.
func (t *turn) carryUIAction(out *outcome) {
	if t.uiAct == nil {
		return
	}
	if out.code == "" && out.prepared == nil && out.receipt == nil && out.dir != nil && uiActionAct(out.dir.Act) {
		out.ui = t.uiAct
		t.trace.UIAction = UIActionEmitted
		return
	}
	t.trace.UIAction = UIActionDropped
}

func (t *turn) routeError(err error) (outcome, error) {
	if isCanceled(err) {
		return outcome{}, providers.ContextError(err)
	}
	switch code := contracts.CodeOf(err); code {
	case contracts.ErrScopeMissing, contracts.ErrScopeViolation, contracts.ErrValidationFailed, contracts.ErrTimeout:
		return outcome{}, err
	case contracts.ErrUnavailable:
		return t.failure(contracts.ErrUnavailable, ""), nil
	}
	return t.failure(contracts.ErrInternal, ""), nil
}

func pendingFields(g *sessions.Goal) []string {
	if g != nil && g.Pending != nil {
		return []string{g.Pending.Field}
	}
	return nil
}

// ---- decision stage ----

// decide is the optional decision-layer seam: after the shortlist, before context
// assembly. The result is advisory. A valid selection narrows the candidates the
// controller is offered to the selected one (still a policy-visible capability from
// the shortlist); anything else leaves them as they are, so the pipeline is the same
// as without a stage. The stage is bounded in time and a panic or a hang is a
// fallback, never a failure of the turn.
func (t *turn) decide(ctx context.Context, cands []capabilities.Candidate) []capabilities.Candidate {
	t.stage(StageDecision)
	req := DecisionRequest{Scope: t.scope.Clone(), Utterance: t.userText, Language: t.in.Language, Goal: t.sess.State.Goal.Active()}
	for _, c := range cands {
		dc := DecisionCandidate{CapabilityID: c.CapabilityID, Risk: c.Decision.EffectiveRisk, Score: c.Score}
		for _, s := range c.Signals {
			dc.Signals = append(dc.Signals, s.Kind)
		}
		req.Candidates = append(req.Candidates, dc)
	}
	res := t.runDecision(ctx, req)
	sel, reason := validateDecision(res, cands)
	if sel == nil {
		t.trace.Decision = string(reason)
		return cands
	}
	t.trace.Decision = "selected"
	return []capabilities.Candidate{*sel}
}

func (t *turn) runDecision(ctx context.Context, req DecisionRequest) DecisionResult {
	dctx, cancel := context.WithTimeout(ctx, t.e.cfg.DecisionTimeout)
	defer cancel()
	done := make(chan DecisionResult, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- DecisionResult{Fallback: FallbackUnavailable}
			}
		}()
		done <- t.e.decision.Decide(dctx, req)
	}()
	select {
	case r := <-done:
		return r
	case <-dctx.Done():
		return DecisionResult{Fallback: FallbackTimeout}
	}
}

// validateDecision checks a stage's answer against the candidates it was given. A
// selection outside them, a confidence that is not a probability, or an answer that
// both selects and falls back (or does neither) is not accepted.
func validateDecision(res DecisionResult, cands []capabilities.Candidate) (*capabilities.Candidate, FallbackReason) {
	switch {
	case res.Selected != nil && res.Fallback != FallbackNone:
		return nil, FallbackMalformedOutput
	case res.Selected == nil:
		if res.Fallback == FallbackNone || !res.Fallback.Valid() {
			return nil, FallbackMalformedOutput
		}
		return nil, res.Fallback
	}
	c := res.Selected.Confidence
	if math.IsNaN(c) || c < 0 || c > 1 {
		return nil, FallbackMalformedOutput
	}
	for i := range cands {
		if cands[i].CapabilityID == res.Selected.CapabilityID {
			return &cands[i], FallbackNone
		}
	}
	return nil, FallbackCandidateSetReject
}

// ---- the proposal (model call 1) ----

// proposal produces the TaskProposal of the turn: from the model, constrained to
// the shortlist, or deterministically when the request is exactly a read that
// takes no fields. done reports that the turn ended here with out.
func (t *turn) proposal(ctx context.Context, cands []capabilities.Candidate) (tp contracts.TaskProposal, out outcome, done bool, err error) {
	t.stage(StageContext)
	if tp, ok := t.shortcut(cands); ok {
		t.stage(StageNoModelUse)
		return tp, outcome{}, false, nil
	}

	caps := capabilitiesFor(t.pkg, cands, t.lang, t.e.cfg.MaxTerms, t.e.cfg.MaxSynonyms)
	if len(caps) == 0 {
		out, uerr := t.unhandled(ctx)
		return tp, out, true, uerr
	}
	// The history the prompt may draw on is the bounded recent window of the session
	// (the profile's CandidateRows newest rows, never the transcript); which of its rows
	// are replayed, and how many, is the builder's choice within the profile.
	turns, terr := t.e.builder.Window(ctx, t.e.sessions, t.sess)
	if terr != nil {
		return tp, outcome{}, true, terr
	}
	in := proposalInput{
		App: t.scope.ApplicationID, Now: t.now, Lang: t.lang, Caps: caps, State: t.sess.State,
		Visible: t.visibleFromPolicy(ctx), Turns: turns, Utterance: t.userText, Attachments: t.attachments,
	}
	if store := t.e.memoryStore(); store != nil {
		in.AllowMemory = true
		if got, err := store.RetrieveActive(ctx, t.scope, t.userText); err == nil {
			for _, r := range got {
				cls := dctxpkg.DataClass(r.Item.DataClass)
				in.Memories = append(in.Memories, dctxpkg.MemoryFact{
					ID: string(r.Item.ID), Text: string(r.Item.Text), Class: cls,
					LocalOnly: cls == dctxpkg.ClassRestricted,
				})
			}
		}
	}
	built, berr := t.buildWithClientData(ctx, in)
	if berr != nil {
		if isCanceled(berr) {
			return tp, outcome{}, true, providers.ContextError(berr)
		}
		// Not even the smallest prompt fits the budget: nothing is sent.
		t.recordCall(ModelCall{Purpose: "proposal", Err: failureCode(berr, contracts.ErrValidationFailed)})
		return tp, t.failure(failureCode(berr, contracts.ErrValidationFailed), ""), true, nil
	}

	t.stage(StageProposal)
	res, call, cerr := t.callModel(ctx, built)
	t.recordCall(call)
	if cerr != nil {
		if isCanceled(cerr) {
			return tp, outcome{}, true, providers.ContextError(cerr)
		}
		code := failureCode(cerr, contracts.ErrUnavailable)
		if modelDownCode(code) {
			t.modelDown = true
		}
		return tp, t.failure(code, ""), true, nil
	}

	t.stage(StageValidate)
	raw := []byte(res.Text)
	if len(res.JSON) > 0 {
		raw = res.JSON
	}
	tp, perr := parseProposal(raw, t.e.ids.New("trc"))
	if perr != nil {
		t.log("model output is not a task proposal", "code", string(contracts.CodeOf(perr)))
		return tp, t.failure(contracts.ErrSchemaInvalid, ""), true, nil
	}
	if t.e.memoryStore() != nil {
		t.acceptMemoryProposal(ctx, draftFromProposal(tp.MemoryProposal))
	}
	if tp.UIAction != nil {
		// A UI action is checked before anything else of the proposal is used: a name the client did not declare, the
		// host did not register, that names a capability, a prepare or a commit, or arguments that do not satisfy the
		// declared schema fail the whole proposal closed. Nothing has been executed or stored at this point.
		ua, reason := t.checkUIAction(*tp.UIAction, t.ui)
		if reason != "" {
			t.trace.UIAction = UIActionRefusedPrefix + reason
			t.log("ui action refused", "reason", reason)
			return tp, t.failure(contracts.ErrValidationFailed, ""), true, nil
		}
		t.uiAct = ua
	}
	return tp, outcome{}, false, nil
}

func modelDownCode(c contracts.ErrCode) bool {
	switch c {
	case contracts.ErrUnavailable, contracts.ErrTimeout, contracts.ErrResourceExhausted, contracts.ErrDegraded:
		return true
	}
	return false
}

// shortcut builds the proposal without a model when the request is exactly a read
// that takes no fields: the router found one decisive candidate, the policy gateway
// sees it as level 0 with nothing to confirm, and it has no visible field to fill.
// Nothing is guessed (there is no value to extract), and the read still goes
// through the policy gateway and the host.
func (t *turn) shortcut(cands []capabilities.Candidate) (contracts.TaskProposal, bool) {
	var none contracts.TaskProposal
	g := t.sess.State.Goal
	if len(cands) == 0 || !t.route.Decisive || (g != nil && g.Pending != nil) {
		return none, false
	}
	c := cands[0]
	if len(t.route.Candidates) == 0 || t.route.Candidates[0].CapabilityID != c.CapabilityID {
		return none, false
	}
	if !isReadDecision(c.Decision) || len(c.Decision.VisibleFields) > 0 {
		return none, false
	}
	spec, ok := t.pkg.Spec(c.CapabilityID)
	if !ok {
		return none, false
	}
	for _, f := range spec.Fields {
		if f.Required {
			return none, false
		}
	}
	id := common.CapabilityID(c.CapabilityID)
	return contracts.TaskProposal{
		CapabilityID: &id, Fields: map[string]conversation.TaskProposalCandidateValue{},
		EntityRefs: []conversation.TaskProposalEntityRef{}, Intent: conversation.TaskProposalIntentTask,
		Uncertainty: conversation.TaskProposalUncertainty{Overall: 1}, TraceID: common.TraceID(t.e.ids.New("trc")),
	}, true
}

// isReadDecision reports whether the gateway's exposure decision treats a
// capability as a plain read: level 0, nothing to confirm.
func isReadDecision(d policy.Decision) bool {
	return d.Allowed && d.EffectiveRisk == contracts.RiskRead && !d.Requires.Confirmation && !d.Requires.StepUp
}

// callModel runs one model call through the seams, in order: the provider for the
// role, the prompt guard, the budget, the provider. The call is recorded whether
// or not it succeeded.
func (t *turn) callModel(ctx context.Context, built builtPrompt) (providers.ChatResult, ModelCall, error) {
	purpose := "proposal"
	if built.Req.Output.Kind() == providers.ConstraintNone {
		purpose = "response"
	}
	call := ModelCall{
		Purpose: purpose, PromptToks: built.EstTokens, PromptBytes: built.Bytes,
		Capabilities: len(built.Caps), TurnsReplayed: built.TurnsKept, HistoryToks: built.HistoryToks, Trimmed: built.Trimmed,
	}
	prov, err := t.e.providers.Select(ctx, t.scope, built.Req.Role)
	if err != nil {
		call.Err = failureCode(err, contracts.ErrUnavailable)
		return providers.ChatResult{}, call, err
	}
	if prov.Generator == nil {
		call.Err = contracts.ErrUnavailable
		return providers.ChatResult{}, call, contracts.New(contracts.ErrUnavailable, "no model is available for the role")
	}
	call.Provider = prov.Name
	guarded, err := t.e.guard.Guard(ctx, GuardInput{Scope: t.scope.Clone(), Provider: ProviderInfo{Name: prov.Name, Local: prov.Local}, Request: built.Req})
	if err != nil {
		call.Err = failureCode(err, contracts.ErrPermissionDenied)
		return providers.ChatResult{}, call, contracts.Wrap(call.Err, err, "the prompt guard denied the request")
	}
	chat := guarded.ChatRequest()
	chat, err = t.e.budget.CheckChat(chat) // a guard may filter a prompt, never grow it past the budget
	if err != nil {
		call.Err = failureCode(err, contracts.ErrValidationFailed)
		return providers.ChatResult{}, call, err
	}
	res, err := prov.Generator.Chat(ctx, t.scope, chat)
	if err != nil {
		call.Err = failureCode(err, contracts.ErrUnavailable)
		if isCanceled(err) {
			call.Err = ""
		}
		return providers.ChatResult{}, call, err
	}
	call.TokIn, call.TokOut = res.Usage.InputTokens, res.Usage.OutputTokens
	return res, call, nil
}

func (t *turn) recordCall(c ModelCall) {
	t.trace.ModelCalls = append(t.trace.ModelCalls, c)
	t.log("model call", "purpose", c.Purpose, "est_toks", c.PromptToks, "prompt_len", c.PromptBytes,
		"tok_in", c.TokIn, "tok_out", c.TokOut, "provider", c.Provider, "code", string(c.Err))
}

// ---- applying a proposal ----

// apply acts on a validated TaskProposal. The proposal is a candidate: the
// capability must be on the shortlist the model was offered, the policy gateway
// must allow it, and every value is validated before it is kept.
func (t *turn) apply(ctx context.Context, tp contracts.TaskProposal, cands []capabilities.Candidate) (outcome, error) {
	switch tp.Intent {
	case conversation.TaskProposalIntentOutOfScope:
		return t.unhandled(ctx)
	case conversation.TaskProposalIntentHelp:
		return t.help(ctx)
	case conversation.TaskProposalIntentCancel:
		return t.cancelGoals(ctx, false)
	}
	if tp.CapabilityID == nil || *tp.CapabilityID == "" {
		return t.failure(contracts.ErrSchemaInvalid, ""), nil
	}
	capID := string(*tp.CapabilityID)
	for _, c := range cands {
		if c.CapabilityID == capID {
			return t.task(ctx, tp, c)
		}
	}
	// Not on the list the model was given. If the package declares it, the gateway
	// says why the caller may not use it (and records the denial); otherwise it is
	// not a capability of this application.
	if _, declared := t.pkg.Capability(capID); declared {
		d := t.e.policy.Decide(ctx, policy.Request{
			Scope: t.scope, Package: t.pkg, CapabilityID: capID, Binding: policy.BindingOf(t.sess.Scope),
			Phase: policy.PhaseExpose, TaskFields: pendingFields(t.sess.State.Goal),
		})
		if !d.Allowed {
			code := d.Code
			if !code.Valid() {
				code = contracts.ErrPermissionDenied
			}
			return t.refuse(code, ""), nil
		}
	}
	return t.unhandled(ctx)
}

// task merges a proposal for a capability into the goal and advances it. The model
// proposed values and an intent; what the intent MEANS is decided here, from the state:
//
//   - a proposal for the active goal's own capability continues that goal. A value that
//     differs from one already collected replaces it and is a correction, whatever
//     intent the model gave it; a correction cue in the words (Detectors.Correct) or the
//     intent "correction" make a finished goal reopen instead of starting a new one;
//   - a "correction" for a capability the active goal is not on corrects nothing: it is
//     a plain request for that capability (a new goal, the old one set aside), never a
//     silent edit of another goal;
//   - a proposal for another capability sets the active goal aside (see setAside), and a
//     finished goal becomes a capsule;
//   - a preparation built on values that changed is voided before the new values are
//     stored, with the reason the change deserves (field_corrected when a collected
//     value was replaced, goal_changed otherwise).
func (t *turn) task(ctx context.Context, tp contracts.TaskProposal, cand capabilities.Candidate) (outcome, error) {
	capID := cand.CapabilityID
	spec, ok := t.pkg.Spec(capID)
	if !ok {
		return t.failure(contracts.ErrUnknownCapability, capID), nil
	}
	m := messagesFor(t.lang)
	wantCorrection := tp.Intent == conversation.TaskProposalIntentCorrection || t.correctionCue(ctx)

	fields := make(map[string]any, len(tp.Fields))
	for k, v := range tp.Fields {
		fields[k] = v
	}
	for _, ref := range tp.EntityRefs {
		if ref.Field == nil {
			continue
		}
		name := string(*ref.Field)
		if _, has := fields[name]; !has {
			fields[name] = ref.Mention
		}
		if ref.CandidateID != nil {
			t.hints[name] = string(*ref.CandidateID)
		}
	}

	// Relative references are resolved against the state as it is now, before the active
	// goal is set aside or completed: they may refer to what it holds.
	cur := t.sess.State.Goal
	var curSlots map[string]sessions.Slot
	var curPending *sessions.Clarification
	if cur != nil && cur.CapabilityID == capID && !(finished(cur) && !wantCorrection) {
		curSlots, curPending = cur.Slots, cur.Pending
	}
	refs := t.stateRefs(spec, cand.Decision.VisibleFields, curSlots, curPending, fields)
	for name := range refs {
		delete(fields, name) // whatever the model wrote for a field the state resolved is not trusted
	}

	g, correction, err := t.settleFor(ctx, cand, wantCorrection)
	if err != nil {
		return outcome{}, err
	}
	if g == nil {
		g = &sessions.Goal{CapabilityID: capID, Slots: map[string]sessions.Slot{}, StartedAt: t.now}
		t.sess.State.Goal = g
	}

	chk := checkFields(spec, cand.Decision.VisibleFields, fields, m)
	for name, note := range tp.Uncertainty.Fields {
		// A value the model says it is unsure of, for a field the package wants asked
		// again when confidence is low, is not accepted.
		if strings.TrimSpace(note) != "" && lowConfidenceAsks(spec, name) {
			delete(chk.Clean, name)
		}
	}
	names := make([]string, 0, len(chk.Clean))
	for name := range chk.Clean {
		names = append(names, name)
	}
	sortStrings(names)
	var writes []slotWrite
	for _, name := range names {
		writes = append(writes, slotWrite{name: name, value: chk.Clean[name], origin: sessions.OriginUserTurn, typ: fieldType(spec, name)})
	}
	for _, a := range refs {
		writes = append(writes, slotWrite{name: a.field, value: a.value, origin: a.origin, typ: fieldType(spec, a.field), entity: a.entity})
	}
	issues, err := t.writeSlots(ctx, g, writes, correction)
	if err != nil {
		return outcome{}, err
	}
	if len(refs) > 0 {
		t.transition(TransitionReference)
	}
	return t.advance(ctx, spec, cand.Decision, g, append(chk.Issues, issues...))
}

// slotWrite is one value to store in a goal.
type slotWrite struct {
	name   string
	value  any
	origin sessions.SlotOrigin
	typ    string
	// entity is the record the value is, when it came from the state.
	entity *sessions.ResolvedEntity
}

// fieldType is the capability field type of a field, as the slot records it.
func fieldType(spec ap.Capability, name string) string {
	for _, f := range spec.Fields {
		if string(f.Name) == name {
			return string(f.Type)
		}
	}
	return ""
}

// writeSlots stores validated values in the goal. A preparation built on the old values
// is voided first, so a failure to void it leaves the goal as it was: the reason is
// field_corrected when a collected value is replaced (or the user said it is a
// correction) and goal_changed otherwise. A value that replaces a different one is
// recorded as a correction, and the entity the old value was resolved to is dropped
// with it. The goal is open again afterwards. It returns the issues of values the
// state could not hold.
func (t *turn) writeSlots(ctx context.Context, g *sessions.Goal, writes []slotWrite, correction bool) ([]act.Issue, error) {
	replaced := false
	for _, w := range writes {
		if prev, has := g.Slots[w.name]; has && !sameSlotValue(prev.Value, w.value) {
			replaced = true
		}
	}
	if g.PreparedID != "" {
		reason := act.PreparedActionInvalidationConditionsItemGoalChanged
		if replaced || correction {
			reason = act.PreparedActionInvalidationConditionsItemFieldCorrected
		}
		if err := t.voidPrepared(ctx, g, reason); err != nil {
			return nil, err
		}
	}
	m := messagesFor(t.lang)
	var issues []act.Issue
	for _, w := range writes {
		origin := w.origin
		if prev, has := g.Slots[w.name]; has && !sameSlotValue(prev.Value, w.value) && origin == sessions.OriginUserTurn {
			origin = sessions.OriginCorrection
		}
		if _, err := g.SetSlot(w.name, w.value, origin, sessions.SlotMeta{Type: w.typ, TurnID: t.id}); err != nil {
			issues = append(issues, issue(contracts.ErrValidationFailed, w.name, fmt.Sprintf(m.tooLong, maxSlotRunes)))
			continue
		}
		g.Entities = dropEntity(g.Entities, w.name) // a new value is resolved again
		if w.entity != nil {
			g.Entities = append(g.Entities, *w.entity)
		}
	}
	if replaced {
		t.transition(TransitionCorrected)
	}
	g.Phase = capabilities.PhaseOpen
	return issues, nil
}

// maxSlotRunes is the longest text value a slot holds (internal/sessions bounds it).
const maxSlotRunes = 4000

func sameSlotValue(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// settleFor makes the session's active goal ready for a request on the candidate's
// capability: it returns the goal the values are to be merged into (nil: start a new
// one) and whether the request is a correction of it. Goals on other capabilities are
// set aside (setAside) and finished ones completed into capsules; completing a nested
// goal returns control to its parent, which is then asked the same question.
func (t *turn) settleFor(ctx context.Context, cand capabilities.Candidate, wantCorrection bool) (*sessions.Goal, bool, error) {
	for range 8 { // a chain of nested goals is bounded by the suspended stack
		g := t.sess.State.Goal
		if g == nil {
			return nil, false, nil
		}
		same := g.CapabilityID == cand.CapabilityID
		switch {
		case same && !(finished(g) && !wantCorrection):
			return g, wantCorrection, nil
		case finished(g):
			// A finished goal and a new request: the goal becomes a capsule.
			if err := t.completeGoal(ctx, "", ""); err != nil {
				return nil, false, err
			}
		default:
			child, err := t.setAside(ctx, g, cand)
			return child, false, err
		}
	}
	return nil, false, contracts.New(contracts.ErrInternal, "dialogue: the goal stack did not settle")
}

// setAside puts the unfinished active goal away for a request on another capability.
// A lookup (a level 0 read) asked while the goal is waiting for an answer is a side trip:
// the goal is NESTED, the lookup is its child, and when the child completes the parent is
// active again with its question still pending. Anything else is a topic switch: the
// goal is SUSPENDED with its collected slots, resolved entities and question, to be
// resumed on request (sessions: two hours, three goals). Either way its preparation is
// voided (goal_changed): a confirmation must not be answerable while the user is
// somewhere else. It returns the child goal when one was nested.
func (t *turn) setAside(ctx context.Context, g *sessions.Goal, cand capabilities.Candidate) (*sessions.Goal, error) {
	cfg := t.e.sessions.Config()
	if g.Phase == capabilities.PhaseOpen && g.Pending != nil && isReadDecision(cand.Decision) {
		child := &sessions.Goal{CapabilityID: cand.CapabilityID, Slots: map[string]sessions.Slot{}, StartedAt: t.now}
		imp, err := t.sess.State.Nest(child, t.now, cfg)
		if err != nil {
			return nil, err
		}
		t.transition(TransitionNested)
		return child, t.applyImpact(ctx, imp)
	}
	imp, err := t.sess.State.Suspend(t.now, sessions.SuspendedTopicSwitch, cfg)
	if err != nil {
		return nil, err
	}
	t.transition(TransitionSuspended)
	return nil, t.applyImpact(ctx, imp)
}

// correctionCue asks the correction detector whether the user's words read as a
// correction of what the active goal holds. It only matters for a goal in progress: a
// goal that already finished is reopened only by the model's own "correction" intent,
// and a cue ("no, another one named gadget") leaves a new request a new goal.
func (t *turn) correctionCue(ctx context.Context) bool {
	if g := t.sess.State.Goal; g == nil || finished(g) {
		return false
	}
	return safeDetect(ctx, t.e.detect.Correct, t.controlRequest()).Matched
}

func lowConfidenceAsks(spec ap.Capability, field string) bool {
	for _, r := range spec.Ambiguity {
		if string(r.Field) == field && r.When == ap.CapabilityAmbiguityRuleWhenLowConfidence && r.Action == ap.CapabilityAmbiguityRuleActionAskUser {
			return true
		}
	}
	return false
}

// maxChoicesFor is how many candidates to offer for a field: the package's own
// bound for ambiguous matches, if it has one, within the engine's.
func maxChoicesFor(spec ap.Capability, field string, limit int) int {
	for _, r := range spec.Ambiguity {
		if string(r.Field) == field && r.When == ap.CapabilityAmbiguityRuleWhenMultipleMatches && r.MaxChoices != nil {
			if n := int(*r.MaxChoices); n >= 2 && n < limit {
				return n
			}
		}
	}
	return limit
}

func dropEntity(in []sessions.ResolvedEntity, field string) []sessions.ResolvedEntity {
	out := in[:0:0]
	for _, e := range in {
		if e.Field != field {
			out = append(out, e)
		}
	}
	return out
}

// ---- advancing a goal ----

// taskFields are the sensitive fields the current task needs, decided here and
// never by the model: the capability's required fields and the one the session is
// asking for. The policy gateway withholds every other sensitive field.
func taskFields(spec ap.Capability, g *sessions.Goal) []string {
	var out []string
	for _, f := range spec.Fields {
		if f.Required {
			out = append(out, string(f.Name))
		}
	}
	if g != nil && g.Pending != nil {
		out = append(out, g.Pending.Field)
	}
	return out
}

// visibleSet returns the decision's visible fields as a set.
func visibleSet(d policy.Decision) map[string]bool {
	s := make(map[string]bool, len(d.VisibleFields))
	for _, f := range d.VisibleFields {
		s[f] = true
	}
	return s
}

// missingFields lists the required, visible fields of the capability that the goal
// has no value for, in the capability's order.
func missingFields(spec ap.Capability, d policy.Decision, g *sessions.Goal) []string {
	vis := visibleSet(d)
	var out []string
	for _, f := range spec.Fields {
		name := string(f.Name)
		if !f.Required || !vis[name] {
			continue
		}
		if _, ok := g.Slots[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}

// advance takes a goal as far as it can go without the user: invalid values are
// asked again, missing fields asked, mentions resolved by the host, and then the
// host validates and the capability is read or prepared.
func (t *turn) advance(ctx context.Context, spec ap.Capability, dec policy.Decision, g *sessions.Goal, issues []act.Issue) (outcome, error) {
	if len(issues) > 0 {
		return t.askInvalid(spec, g, issues), nil
	}
	if missing := missingFields(spec, dec, g); len(missing) > 0 {
		return t.askMissing(spec, g, missing), nil
	}
	if out, done, err := t.resolveEntities(ctx, spec, dec, g); done || err != nil {
		return out, err
	}
	g.Pending = nil
	if isReadDecision(dec) {
		return t.read(ctx, spec, g)
	}
	return t.prepare(ctx, spec, g)
}

// resolveEntities maps the mentions of entity_ref fields to the host's record IDs.
// It stops at the first mention that needs the user: several matches become a
// clarification listing the host's candidates, no match is refused as invalid.
func (t *turn) resolveEntities(ctx context.Context, spec ap.Capability, dec policy.Decision, g *sessions.Goal) (outcome, bool, error) {
	vis := visibleSet(dec)
	m := messagesFor(t.lang)
	for _, f := range spec.Fields {
		name := string(f.Name)
		slot, ok := g.Slots[name]
		if !ok || !vis[name] || f.Type != ap.CapabilityFieldTypeEntityRef || f.EntityType == nil {
			continue
		}
		etype := string(*f.EntityType)
		if slot.Origin == sessions.OriginEntityResolution {
			continue
		}
		et, declared := spec.EntityTypes[etype]
		if !declared || et.Resolve == nil {
			continue // no resolver: the value is the ID, and the host validates it
		}
		mention, _ := slot.Value.(string)
		t.stage(StageResolve)
		limit := maxChoicesFor(spec, name, t.e.cfg.MaxChoices)
		res, err := t.e.resolver.Resolve(ctx, t.scope, ResolveRequest{
			CapabilityID: string(spec.ID), Field: name, EntityType: etype, Mention: mention,
			CandidateID: t.hints[name], Limit: limit,
		})
		if err != nil {
			if isCanceled(err) {
				return outcome{}, true, providers.ContextError(err)
			}
			return t.fromHostError(spec, g, err, nil), true, nil
		}
		switch n := len(res.Candidates); {
		case n == 0:
			delete(g.Slots, name)
			return t.askInvalid(spec, g, []act.Issue{issue(contracts.ErrValidationFailed, name, m.noMatch)}), true, nil
		case n == 1:
			c := res.Candidates[0]
			g.Slots[name] = sessions.Slot{Value: c.ID, Origin: sessions.OriginEntityResolution}
			g.Entities = append(dropEntity(g.Entities, name), sessions.ResolvedEntity{Field: name, EntityType: etype, EntityID: c.ID, Version: res.Version})
		default:
			delete(g.Slots, name)
			if len(res.Candidates) > limit {
				res.Candidates = res.Candidates[:limit]
			}
			return t.disambiguate(spec, g, name, etype, res.Version, res.Candidates), true, nil
		}
	}
	return outcome{}, false, nil
}

// read performs a level 0 read through the coordinator and presents the result.
func (t *turn) read(ctx context.Context, spec ap.Capability, g *sessions.Goal) (outcome, error) {
	t.stage(StageHost)
	capID := string(spec.ID)
	ro, err := t.e.actions.Read(ctx, t.scope, actions.ReadInput{
		Session: t.sess.Scope, CapabilityID: capID, Fields: g.SlotValues(), TaskFields: taskFields(spec, g),
	})
	if err != nil {
		if isCanceled(err) {
			return outcome{}, providers.ContextError(err)
		}
		return t.fromHostError(spec, g, err, &ro.Result), nil
	}
	t.stage(StageDirective)
	d := t.newDirective(conversation.DialogueDirectiveActPresentResult, capID)
	d.Facts = boundFacts(ro.Result.Values, t.e.cfg.MaxFacts)
	d.MustState = mustStateAll(d.Facts)
	d.PermittedNextActions = t.permitted(ctx, ro.Result.AllowedNextActions)
	g.Phase, g.Pending = capabilities.PhaseSuccess, nil
	t.recordRead(capID, ro.Result)
	return outcome{dir: d, capID: capID, resultIDs: ro.Result.Identifiers, resultValues: ro.Result.Values}, nil
}

// prepare validates and stages a consequential action through the coordinator and
// asks for what the preparation requires: a confirmation, a step-up in the
// application, or (confirmation mode none) the commit itself.
func (t *turn) prepare(ctx context.Context, spec ap.Capability, g *sessions.Goal) (outcome, error) {
	t.stage(StageHost)
	capID := string(spec.ID)
	p, err := t.e.actions.Prepare(ctx, t.scope, actions.PrepareInput{
		Session: t.sess.Scope, CapabilityID: capID, Fields: g.SlotValues(), TaskFields: taskFields(spec, g),
	})
	if err != nil {
		if isCanceled(err) {
			return outcome{}, providers.ContextError(err)
		}
		return t.fromHostError(spec, g, err, &p.Result), nil
	}
	t.stage(StageDirective)
	pa := p.Action
	// The expiry is the preparation's own, copied so the session stops offering a
	// confirmation that can no longer be given; the coordinator's is authoritative.
	g.SetPrepared(string(pa.PreparationID), preparedExpiry(pa))
	g.Pending = nil
	switch pa.Confirmation.Mode {
	case act.ConfirmationRequirementModeExplicit:
		return t.requestConfirmation(capID, pa, p.Confirmation), nil
	case act.ConfirmationRequirementModeStepUp:
		return t.requestStepUp(capID, pa), nil
	}
	// Confirmation mode none: the action is level 0 or 1 and the package asks for no
	// confirmation. It is still prepared first, and still committed only through the
	// coordinator, which asks the policy gateway again.
	return t.commit(ctx, g, capID, string(pa.PreparationID)), nil
}

// preparedFacts are the facts a confirmation shows: the host's canonical summary
// and the exact values each effect sets. They are the host's; the response may
// state these and nothing else about the action.
func (t *turn) preparedFacts(pa contracts.PreparedAction) []act.Fact {
	facts := []act.Fact{textFact("summary", pa.Summary)}
	for _, e := range pa.Effects {
		if d := strings.TrimRight(strings.TrimSpace(e.Description), ". "); d != "" && d != strings.TrimRight(strings.TrimSpace(pa.Summary), ". ") {
			facts = append(facts, textFact("effect", e.Description))
		}
		facts = append(facts, e.Changes...)
	}
	return boundFacts(facts, t.e.cfg.MaxFacts)
}

func (t *turn) requestConfirmation(capID string, pa contracts.PreparedAction, token actions.ConfirmationToken) outcome {
	d := t.newDirective(conversation.DialogueDirectiveActRequestConfirmation, capID)
	id := pa.PreparationID
	d.PreparationID = &id
	d.Confirmation = pa.Confirmation
	d.Facts = t.preparedFacts(pa)
	d.MustState = mustStateAll(d.Facts)
	d.PermittedNextActions = []act.PermittedNextAction{
		{Action: act.PermittedNextActionActionConfirm},
		{Action: act.PermittedNextActionActionModify},
		{Action: act.PermittedNextActionActionReject},
	}
	p := pa
	return outcome{dir: d, prepared: &p, token: token, capID: capID}
}

// requestStepUp sends the user to the application's own authenticated interface: a
// level 3 action cannot be completed through the AI path, and nothing here confirms
// or commits it.
func (t *turn) requestStepUp(capID string, pa contracts.PreparedAction) outcome {
	d := t.newDirective(conversation.DialogueDirectiveActRequestStepUp, capID)
	id := pa.PreparationID
	d.PreparationID = &id
	d.Confirmation = pa.Confirmation
	d.ReasonCode = errorCode(contracts.ErrStepUpRequired)
	d.Facts = t.preparedFacts(pa)
	d.MustState = mustStateAll(d.Facts)
	d.PermittedNextActions = []act.PermittedNextAction{{Action: act.PermittedNextActionActionContinueInApp}}
	p := pa
	return outcome{dir: d, prepared: &p, capID: capID}
}

// permitted intersects the actions the host will accept with what the core
// permits: an invoke_capability only for a capability the policy gateway exposes to
// this caller, and never a confirmation (that is the core's, not the host's).
func (t *turn) permitted(ctx context.Context, allowed []act.AllowedNextAction) []act.PermittedNextAction {
	out := []act.PermittedNextAction{}
	seen := map[string]bool{}
	for _, a := range allowed {
		if len(out) == 8 {
			break
		}
		pa := act.PermittedNextAction{}
		switch a.Action {
		case act.AllowedNextActionActionInvokeCapability:
			if a.CapabilityID == nil {
				continue
			}
			id := string(*a.CapabilityID)
			if _, ok := t.pkg.Spec(id); !ok {
				continue
			}
			d := t.e.policy.Decide(ctx, policy.Request{
				Scope: t.scope, Package: t.pkg, CapabilityID: id, Binding: policy.BindingOf(t.sess.Scope), Phase: policy.PhaseExpose,
			})
			if !d.Allowed {
				continue
			}
			cid := common.CapabilityID(id)
			pa.Action, pa.CapabilityID = act.PermittedNextActionActionInvokeCapability, &cid
		case act.AllowedNextActionActionContinueInApp, act.AllowedNextActionActionRefresh, act.AllowedNextActionActionRetry:
			pa.Action = act.PermittedNextActionAction(a.Action)
		default:
			continue
		}
		if a.Label != nil && *a.Label != "" {
			pa.Label = strPtr(*a.Label)
		}
		key := string(pa.Action)
		if pa.CapabilityID != nil {
			key += ":" + string(*pa.CapabilityID)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, pa)
	}
	return out
}

// fromHostError turns a failure of a read or a prepare into the right directive:
// missing fields ask, values the host found invalid ask again, everything else is a
// typed refusal or failure. Nothing was executed.
func (t *turn) fromHostError(spec ap.Capability, g *sessions.Goal, err error, res *act.DeterministicResult) outcome {
	capID := string(spec.ID)
	code := failureCode(err, contracts.ErrInternal)
	var ce *contracts.Error
	if code == contracts.ErrMissingFields && errors.As(err, &ce) {
		if names, ok := ce.Details["fields"].([]string); ok {
			var usable []string
			for _, f := range spec.Fields {
				for _, n := range names {
					if n == string(f.Name) {
						usable = append(usable, n)
					}
				}
			}
			if len(usable) > 0 {
				for _, n := range usable {
					delete(g.Slots, n)
				}
				return t.askMissing(spec, g, usable)
			}
		}
	}
	if res != nil {
		var invalid []act.Issue
		for _, is := range res.Errors {
			c := contracts.ErrCode(is.Code)
			if is.Field != nil && (c == contracts.ErrValidationFailed || c == contracts.ErrAmbiguous || c == contracts.ErrMissingFields) {
				invalid = append(invalid, is)
			}
		}
		if len(invalid) > 0 {
			for _, is := range invalid {
				delete(g.Slots, string(*is.Field))
			}
			return t.askInvalid(spec, g, invalid)
		}
	}
	if g != nil && modelDownCode(code) {
		g.Phase = capabilities.PhaseOpen
	}
	return t.failure(code, capID)
}

// ---- commit and receipt ----

// commit sends a prepared action through the coordinator and reports the receipt.
// The coordinator is the only path to the adapter's Commit; it asks the policy
// gateway again, checks the confirmation the level requires, writes the write-ahead
// record and never retries. A receipt is reported whatever its status: committed,
// queued, rejected, partial or uncertain, each in its own words.
func (t *turn) commit(ctx context.Context, g *sessions.Goal, capID, preparationID string) outcome {
	t.stage(StageCommit)
	rc, err := t.e.actions.Commit(ctx, t.scope, preparationID)
	if rc.ReceiptID != "" {
		t.stage(StageReceipt)
		t.committed = true
		return t.reportOutcome(g, capID, rc)
	}
	// No receipt: the commit was refused before anything was sent, or could not be
	// reached. Nothing is claimed.
	code := failureCode(err, contracts.ErrInternal)
	if code == contracts.ErrUncertainOutcome {
		// It was sent and its outcome could not be recorded: it may have happened, so the
		// turn reports it whatever fails after this, as it would a receipt.
		t.committed = true
	}
	switch code {
	case contracts.ErrExpired, contracts.ErrStale, contracts.ErrConflict, contracts.ErrReplayed, contracts.ErrPermissionDenied, contracts.ErrScopeViolation:
		g.ClearPrepared()
		g.Phase = capabilities.PhaseFailure
	}
	t.log("commit not made", "code", string(code), "preparation_id", preparationID)
	return t.failure(code, capID)
}

// reportOutcome is the directive for an outcome: its facts are the receipt's, which
// are the host's canonical result facts, and the receipt is passed to the verifier
// so a response can claim completion only against a committed one.
func (t *turn) reportOutcome(g *sessions.Goal, capID string, rc contracts.ActionReceipt) outcome {
	d := t.newDirective(conversation.DialogueDirectiveActReportOutcome, capID)
	rid := rc.ReceiptID
	d.ReceiptID = &rid
	d.Facts = boundFacts(rc.Facts, t.e.cfg.MaxFacts)
	d.MustState = mustStateAll(d.Facts)
	r := rc
	out := outcome{dir: d, receipt: &r, capID: capID}
	g.ClearPrepared()
	g.Pending = nil
	t.recordCommit(rc)
	switch rc.Status {
	case act.ActionReceiptStatusCommitted, act.ActionReceiptStatusQueued:
		g.Phase = capabilities.PhaseSuccess
	case act.ActionReceiptStatusUncertain:
		g.Phase, out.code = capabilities.PhaseFailure, contracts.ErrUncertainOutcome
	case act.ActionReceiptStatusPartial:
		g.Phase, out.code = capabilities.PhaseFailure, contracts.ErrPartialFailure
	default: // rejected
		g.Phase, out.code = capabilities.PhaseFailure, firstIssueCode(rc.Errors, contracts.ErrConflict)
	}
	return out
}

func firstIssueCode(issues []act.Issue, def contracts.ErrCode) contracts.ErrCode {
	if len(issues) > 0 {
		if c := contracts.ErrCode(issues[0].Code); c.Valid() {
			return c
		}
	}
	return def
}

// ---- confirm, reject, choice, cancel, help ----

// confirm presents the client's confirmation of the pending prepared action and
// commits it. The confirmation is a structured input, never a model's reading of
// "yes", and it must be for the preparation this session is waiting on.
func (t *turn) confirm(ctx context.Context) (outcome, error) {
	g := t.sess.State.Goal
	c := t.in.Confirm
	if lp, ok := t.lapsedFor(c.PreparationID); ok && g != nil && g.CapabilityID == lp.capID {
		// The preparation's own expiry passed: the session already let go of it, and the
		// confirmation is refused for that reason.
		g.Phase = capabilities.PhaseFailure
		return t.failure(contracts.ErrExpired, lp.capID), nil
	}
	if g == nil || g.PreparedID == "" || g.PreparedID != c.PreparationID {
		return t.failure(contracts.ErrConflict, goalCap(g)), nil
	}
	capID := g.CapabilityID
	if !g.Confirmed {
		t.stage(StageConfirm)
		_, err := t.e.actions.Confirm(ctx, t.scope, actions.ConfirmInput{PreparationID: c.PreparationID, Token: c.Token})
		if err != nil {
			if isCanceled(err) {
				return outcome{}, providers.ContextError(err)
			}
			return t.confirmFailed(ctx, g, capID, err), nil
		}
		g.Confirmed = true
	}
	return t.commit(ctx, g, capID, g.PreparedID), nil
}

func goalCap(g *sessions.Goal) string {
	if g == nil {
		return ""
	}
	return g.CapabilityID
}

// confirmFailed is the directive for a confirmation that was refused.
func (t *turn) confirmFailed(ctx context.Context, g *sessions.Goal, capID string, err error) outcome {
	code := failureCode(err, contracts.ErrInternal)
	switch code {
	case contracts.ErrStepUpRequired:
		if pa, gerr := t.e.actions.Get(ctx, t.scope, g.PreparedID); gerr == nil {
			return t.requestStepUp(capID, pa)
		}
	case contracts.ErrExpired, contracts.ErrStale, contracts.ErrConflict:
		g.ClearPrepared()
		g.Phase = capabilities.PhaseFailure
	}
	return t.failure(code, capID)
}

// choice applies the selection of one of the pending clarification's choices.
func (t *turn) choice(ctx context.Context) (outcome, error) {
	g := t.sess.State.Goal
	if g == nil || g.Pending == nil || len(g.Pending.Choices) == 0 {
		return t.failure(contracts.ErrConflict, goalCap(g)), nil
	}
	spec, ok := t.pkg.Spec(g.CapabilityID)
	if !ok {
		return t.failure(contracts.ErrUnknownCapability, g.CapabilityID), nil
	}
	dec, err := t.decisionFor(ctx, g.CapabilityID, g)
	if err != nil {
		return t.failure(failureCode(err, contracts.ErrPermissionDenied), g.CapabilityID), nil
	}
	pending := g.Pending
	var picked *act.Choice
	for i := range pending.Choices {
		if string(pending.Choices[i].ChoiceID) == t.in.ChoiceID {
			picked = &pending.Choices[i]
			break
		}
	}
	if picked == nil {
		// The question stands: the same choices are offered again.
		return t.notAChoice(spec, g), nil
	}
	return t.pickChoice(ctx, spec, dec, g, picked)
}

// pickChoice applies the selection of one of the pending clarification's choices: the
// choice's value becomes the field's value (a record the host offered is a resolved
// entity, with the data version the resolution relied on), the question is answered and
// the goal goes on.
func (t *turn) pickChoice(ctx context.Context, spec ap.Capability, dec policy.Decision, g *sessions.Goal, picked *act.Choice) (outcome, error) {
	pending := g.Pending
	value, ok := plainValue(picked.Value)
	if !ok {
		return t.failure(contracts.ErrInternal, g.CapabilityID), nil
	}
	origin := sessions.OriginChoice
	if pending.EntityType != "" {
		origin = sessions.OriginEntityResolution
	}
	if _, err := g.SetSlot(pending.Field, value, origin, sessions.SlotMeta{Type: fieldType(spec, pending.Field), TurnID: t.id}); err != nil {
		return t.failure(contracts.ErrInternal, g.CapabilityID), nil
	}
	g.Entities = dropEntity(g.Entities, pending.Field)
	if pending.EntityType != "" {
		g.Entities = append(g.Entities, sessions.ResolvedEntity{
			Field: pending.Field, EntityType: pending.EntityType, EntityID: string(picked.ChoiceID), Version: pending.Version,
		})
	}
	g.Pending = nil
	return t.advance(ctx, spec, dec, g, nil)
}

// decisionFor returns the policy gateway's exposure decision for a capability: the
// one the router gave this turn if it has it, else a fresh one. A denial is an error.
func (t *turn) decisionFor(ctx context.Context, capID string, g *sessions.Goal) (policy.Decision, error) {
	for _, c := range t.route.Candidates {
		if c.CapabilityID == capID {
			return c.Decision, nil
		}
	}
	d := t.e.policy.Decide(ctx, policy.Request{
		Scope: t.scope, Package: t.pkg, CapabilityID: capID, Binding: policy.BindingOf(t.sess.Scope),
		Phase: policy.PhaseExpose, TaskFields: pendingFields(g),
	})
	if !d.Allowed {
		return d, d.Err()
	}
	return d, nil
}

// help lists what the application can do for this caller, from the policy-exposed
// capabilities and their package titles.
func (t *turn) help(ctx context.Context) (outcome, error) {
	exp := t.e.policy.Expose(ctx, policy.ExposeRequest{
		Scope: t.scope, Package: t.pkg, Binding: policy.BindingOf(t.sess.Scope),
	})
	var cs []act.Choice
	for _, d := range exp.Visible {
		if len(cs) == t.e.cfg.MaxChoices {
			break
		}
		id := d.Record.CapabilityID
		spec, ok := t.pkg.Spec(id)
		if !ok || !validIdentifier(id) {
			continue
		}
		c := act.Choice{ChoiceID: common.Identifier(id), Label: clipRunes(sanitize(string(spec.Title)), 80), Value: identifierValue(id)}
		if p := firstSentence(string(spec.Purpose), 100); p != "" {
			c.Description = strPtr(p)
		}
		cs = append(cs, c)
	}
	return t.offerHelp(cs), nil
}
