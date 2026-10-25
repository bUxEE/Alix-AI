package dialogue

import (
	"context"
	"strings"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The deterministic rules of a text turn (pipeline step "deterministic rules for exact
// cases"). They run before routing, so a turn they decide calls neither the router's
// embedder, the decision stage nor the controller. Each is a detection (control.go,
// replaceable) followed by a transition that checks the state itself:
//
//	help      what the application can do                       (any state)
//	cancel    drop the goal; "cancel all" drops every goal      (needs a goal)
//	restart   begin the goal again from nothing                 (needs a goal)
//	resume    bring a set-aside goal back                       (needs a suspended goal)
//	reject    decline the pending preparation                   (needs a live preparation)
//	confirm   re-present the pending preparation, commit nothing (needs a live preparation)
//	ordinal   "the second one" answers the question just asked  (needs offered choices)
//	reference a turn that is only a relative reference          (needs a goal)
//
// A detection whose precondition does not hold is ignored: the utterance goes on
// through the pipeline as if no rule had matched.

// controlState describes the session for a detector.
func (t *turn) controlState() ControlState {
	g := t.sess.State.Goal
	cs := ControlState{HasGoal: g != nil, Suspended: len(t.sess.State.Suspended)}
	if g != nil {
		cs.Finished = finished(g)
		cs.Prepared = g.Prepared(t.now)
		cs.Pending = g.Pending != nil
		cs.Choices = g.Pending != nil && len(g.Pending.Choices) > 0
	}
	return cs
}

func (t *turn) controlRequest() ControlRequest {
	return ControlRequest{Text: t.userText, Language: t.lang, State: t.controlState()}
}

// rules applies the deterministic rules. done reports that the turn ended here.
func (t *turn) rules(ctx context.Context) (out outcome, done bool, err error) {
	req := t.controlRequest()
	det := t.e.detect
	st := req.State

	if safeDetect(ctx, det.Help, req).Matched {
		out, err = t.help(ctx)
		return out, true, err
	}
	if st.HasGoal || st.Suspended > 0 {
		if d := safeDetect(ctx, det.Cancel, req); d.Matched && (st.HasGoal || d.All) {
			out, err = t.cancelGoals(ctx, d.All)
			return out, true, err
		}
	}
	if st.HasGoal && safeDetect(ctx, det.Restart, req).Matched {
		return t.restart(ctx)
	}
	if st.Suspended > 0 {
		if d := safeDetect(ctx, det.Resume, req); d.Matched {
			if out, ok, err := t.resumeBy(ctx, d, st); ok || err != nil {
				return out, true, err
			}
		}
	}
	if st.Prepared {
		if safeDetect(ctx, det.Reject, req).Matched {
			out, err = t.cancelGoals(ctx, false)
			return out, true, err
		}
		if safeDetect(ctx, det.Confirm, req).Matched {
			out, err = t.reconfirm(ctx)
			return out, true, err
		}
	}
	if st.HasGoal {
		return t.referenceOnly(ctx)
	}
	return outcome{}, false, nil
}

// ---- cancel ----

// cancelGoals drops the active goal: its preparation is rejected through the
// coordinator (so it can never be confirmed), its question is forgotten with it, and it
// leaves a capsule that names what was cancelled and holds no value. A nested goal
// returns control to its parent. With all, every goal of the session goes, the set-aside
// ones included. Nothing is committed and nothing is sent to the application.
func (t *turn) cancelGoals(ctx context.Context, all bool) (outcome, error) {
	st := &t.sess.State
	g := st.Goal
	capID := goalCap(g)
	if g != nil {
		if err := t.rejectPrepared(ctx, g); err != nil {
			if isCanceled(err) {
				return outcome{}, providers.ContextError(err)
			}
			// The preparation could not be rejected: nothing was dropped, and nothing is
			// claimed.
			return t.failure(failureCode(err, contracts.ErrUnavailable), capID), nil
		}
	}
	if all {
		for _, s := range st.Suspended {
			if err := t.rejectPrepared(ctx, s); err != nil { // suspended goals hold none; fail closed anyway
				return t.failure(failureCode(err, contracts.ErrUnavailable), capID), nil
			}
		}
		st.Suspended = nil // the active goal then has no parent to return to
	}
	if g != nil {
		if err := t.completeGoal(ctx, sessions.OutcomeCancelled, ""); err != nil {
			return outcome{}, err
		}
	}
	t.transition(TransitionCancelled)
	return t.acknowledgeCancel(capID), nil
}

// reject is the client's rejection of the pending prepared action.
func (t *turn) reject(ctx context.Context) (outcome, error) {
	g := t.sess.State.Goal
	if lp, ok := t.lapsedFor(t.in.Reject); ok && g != nil && g.CapabilityID == lp.capID {
		// Already final (it expired): dropping the goal is the right outcome.
		return t.cancelGoals(ctx, false)
	}
	if g == nil || g.PreparedID == "" || g.PreparedID != t.in.Reject {
		return t.failure(contracts.ErrConflict, goalCap(g)), nil
	}
	return t.cancelGoals(ctx, false)
}

// ---- reconfirm ----

// reconfirm answers a "yes" typed while a preparation awaits its confirmation. A
// confirmation is a structured input carrying the single-use token the client received
// with the preparation; text can never confirm. The pending request is presented again
// (the same preparation, no new token) and nothing is committed.
func (t *turn) reconfirm(ctx context.Context) (outcome, error) {
	g := t.sess.State.Goal
	pa, err := t.e.actions.Get(ctx, t.scope, g.PreparedID)
	if err != nil {
		if isCanceled(err) {
			return outcome{}, providers.ContextError(err)
		}
		code := failureCode(err, contracts.ErrUnavailable)
		if code == contracts.ErrExpired || code == contracts.ErrStale || code == contracts.ErrConflict {
			g.ClearPrepared()
			g.Phase = capabilities.PhaseFailure
		}
		return t.failure(code, g.CapabilityID), nil
	}
	// A confirmed preparation whose commit is still to be retried is shown too: its
	// confirmation was used, and the client retries the commit with it.
	if pa.State != act.PreparedActionStatePrepared && pa.State != act.PreparedActionStateConfirmed {
		g.ClearPrepared()
		g.Phase = capabilities.PhaseFailure
		return t.failure(contracts.ErrConflict, g.CapabilityID), nil
	}
	if pa.Confirmation.Mode == act.ConfirmationRequirementModeStepUp {
		return t.requestStepUp(g.CapabilityID, pa), nil
	}
	return t.requestConfirmation(g.CapabilityID, pa, actions.ConfirmationToken{}), nil
}

// ---- restart ----

// restart begins the active goal again from nothing: same capability, no collected
// slots, no resolved entities, no question, and its preparation (if any) voided. The
// goal then asks for what the capability needs, as a new request would. A nested goal
// stays the child of its parent.
func (t *turn) restart(ctx context.Context) (outcome, bool, error) {
	g := t.sess.State.Goal
	spec, ok := t.pkg.Spec(g.CapabilityID)
	if !ok {
		return outcome{}, false, nil
	}
	dec, derr := t.decisionFor(ctx, g.CapabilityID, g)
	if derr != nil {
		// The caller may no longer use the capability: the goal ends, with a refusal.
		if err := t.rejectPrepared(ctx, g); err != nil {
			return t.failure(failureCode(err, contracts.ErrUnavailable), g.CapabilityID), true, nil
		}
		code := failureCode(derr, contracts.ErrPermissionDenied)
		if err := t.completeGoal(ctx, sessions.OutcomeCancelled, ""); err != nil {
			return outcome{}, true, err
		}
		return t.failure(code, g.CapabilityID), true, nil
	}
	if err := t.voidPrepared(ctx, g, act.PreparedActionInvalidationConditionsItemGoalChanged); err != nil {
		return outcome{}, true, err
	}
	g.ID = "" // a fresh goal: Save names it
	g.Slots, g.Entities, g.Pending = map[string]sessions.Slot{}, nil, nil
	g.Phase, g.StartedAt = capabilities.PhaseOpen, t.now
	t.sess.State.Last = nil
	t.transition(TransitionRestarted)
	out, err := t.advance(ctx, spec, dec, g, nil)
	return out, true, err
}

// ---- resume ----

// resumeBy resumes the goal a resume detection names: the one whose capability the
// words after the phrase name, else the most recently set aside. A weak resume
// ("continue") only takes the place of a finished or absent goal; a strong one
// ("go back") replaces a goal still being worked on, which is set aside in turn.
func (t *turn) resumeBy(ctx context.Context, d ControlDetection, st ControlState) (outcome, bool, error) {
	if st.HasGoal && !st.Finished && !d.Strong {
		return outcome{}, false, nil // "continue" with a goal in progress continues that goal
	}
	target := ""
	if d.Target != "" {
		var ok bool
		if target, ok = t.suspendedMatching(d.Target); !ok {
			return outcome{}, false, nil // the words name nothing set aside: not a resume
		}
	}
	// A finished goal is not set aside: it becomes a capsule (and a nested one returns
	// to its parent, which a strong resume then sets aside in turn).
	for i := 0; i < 4 && finished(t.sess.State.Goal); i++ {
		if err := t.completeGoal(ctx, "", ""); err != nil {
			return outcome{}, true, err
		}
	}
	imp, err := t.sess.State.Resume(target, t.now, t.e.sessions.Config())
	if err != nil {
		return outcome{}, false, nil // nothing to resume
	}
	if imp.Resumed != "" {
		t.transition(TransitionResumed)
	}
	if err := t.applyImpact(ctx, imp); err != nil {
		return outcome{}, true, err
	}
	out, err := t.continueGoal(ctx)
	return out, true, err
}

// suspendedMatching finds the set-aside goal the words name: those whose capability
// title or terminology the words contain; the most recently set aside among them.
func (t *turn) suspendedMatching(words string) (string, bool) {
	toks := map[string]bool{}
	for _, w := range strings.Fields(words) {
		toks[w] = true
	}
	susp := t.sess.State.Suspended
	for i := len(susp) - 1; i >= 0; i-- {
		spec, ok := t.pkg.Spec(susp[i].CapabilityID)
		if !ok {
			continue
		}
		for _, w := range t.capabilityWords(spec) {
			if toks[w] {
				return susp[i].ID, susp[i].ID != ""
			}
		}
	}
	return "", false
}

// capabilityWords are the words that name a capability to a user: its title and the
// terminology of its conversation contract, in the language of the turn. Short function
// words are not names.
func (t *turn) capabilityWords(spec ap.Capability) []string {
	var out []string
	add := func(s string) {
		for _, w := range strings.Fields(phraseOf(s)) {
			if len([]rune(w)) >= 4 {
				out = append(out, w)
			}
		}
	}
	add(string(spec.Title))
	lang := string(t.lang)
	if ct, ok := t.pkg.ContractFor(string(spec.ID), lang); ok {
		terms := map[string]bool{}
		for _, slug := range ct.Terminology {
			terms[string(slug)] = true
		}
		for _, term := range t.pkg.Terminology() {
			if !terms[string(term.ID)] {
				continue
			}
			if l, ok := term.Labels[lang]; ok {
				add(string(l.Preferred))
				for _, syn := range l.Synonyms {
					add(string(syn))
				}
			}
		}
	}
	return out
}

// continueGoal presents the active goal where it was left: the question it was waiting
// on again, or else what it still needs; a goal that has everything is read or prepared
// again (a resumed goal never keeps a preparation: it needs a fresh confirmation). The
// policy gateway is asked again whether the caller may still use the capability.
func (t *turn) continueGoal(ctx context.Context) (outcome, error) {
	g := t.sess.State.Goal
	spec, ok := t.pkg.Spec(g.CapabilityID)
	if !ok {
		return t.failure(contracts.ErrUnknownCapability, g.CapabilityID), nil
	}
	dec, derr := t.decisionFor(ctx, g.CapabilityID, g)
	if derr != nil {
		code := failureCode(derr, contracts.ErrPermissionDenied)
		if err := t.completeGoal(ctx, sessions.OutcomeCancelled, ""); err != nil {
			return outcome{}, err
		}
		return t.failure(code, g.CapabilityID), nil
	}
	if p := g.Pending; p != nil {
		return t.reask(spec, g, p), nil
	}
	return t.advance(ctx, spec, dec, g, nil)
}

// reask presents the question a goal was waiting on again, with the same choices it
// offered, and keeps it pending.
func (t *turn) reask(spec ap.Capability, g *sessions.Goal, p *sessions.Clarification) outcome {
	if p.EntityType != "" && len(p.Choices) >= 2 {
		cands := make([]EntityCandidate, 0, len(p.Choices))
		for _, c := range p.Choices {
			cands = append(cands, EntityCandidate{ID: string(c.ChoiceID), Label: c.Label})
		}
		return t.disambiguate(spec, g, p.Field, p.EntityType, p.Version, cands)
	}
	return t.askMissing(spec, g, []string{p.Field})
}

// ---- references ----

// referenceOnly handles a turn that is nothing but a relative reference ("the second
// one", "same time tomorrow", "that patient"): resolved from the state, it is applied
// to the active goal with no model involved. An ordinal over offered choices selects
// the choice, like a choice input does.
func (t *turn) referenceOnly(ctx context.Context) (outcome, bool, error) {
	refs, pure := parseRefs(t.userText, t.lang)
	if !pure || len(refs) == 0 {
		return outcome{}, false, nil
	}
	g := t.sess.State.Goal
	spec, ok := t.pkg.Spec(g.CapabilityID)
	if !ok {
		return outcome{}, false, nil
	}
	// An ordinal over the choices just offered answers that question.
	if p := g.Pending; p != nil && len(p.Choices) > 0 && len(refs) == 1 && refs[0].kind == refOrdinal {
		out, err := t.pickOrdinalChoice(ctx, spec, g, refs[0].ordinal)
		return out, true, err
	}
	dec, derr := t.decisionFor(ctx, g.CapabilityID, g)
	if derr != nil {
		return outcome{}, false, nil // the ordinary path says why
	}
	if finished(g) && !isReadDecision(dec) {
		// A finished action is not reopened by a reference alone: the ordinary path makes it
		// a capsule, which "same time tomorrow" still resolves from, and a new request.
		return outcome{}, false, nil
	}
	asg := t.stateRefs(spec, dec.VisibleFields, g.Slots, g.Pending, nil)
	if len(asg) == 0 {
		return outcome{}, false, nil
	}
	var writes []slotWrite
	for _, name := range sortedAssignNames(asg) {
		a := asg[name]
		writes = append(writes, slotWrite{name: name, value: a.value, origin: a.origin, typ: fieldType(spec, name), entity: a.entity})
	}
	issues, err := t.writeSlots(ctx, g, writes, false)
	if err != nil {
		return outcome{}, true, err
	}
	t.transition(TransitionReference)
	out, err := t.advance(ctx, spec, dec, g, issues)
	return out, true, err
}

func sortedAssignNames(m map[string]refAssign) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sortStrings(names)
	return names
}

// pickOrdinalChoice selects the n-th (1-based; -1 is the last) of the choices the
// pending question offered. A position the list does not have is not one of the choices:
// the question stands.
func (t *turn) pickOrdinalChoice(ctx context.Context, spec ap.Capability, g *sessions.Goal, n int) (outcome, error) {
	choices := g.Pending.Choices
	i := n - 1
	if n < 0 {
		i = len(choices) - 1
	}
	if i < 0 || i >= len(choices) {
		return t.notAChoice(spec, g), nil
	}
	dec, err := t.decisionFor(ctx, g.CapabilityID, g)
	if err != nil {
		return t.failure(failureCode(err, contracts.ErrPermissionDenied), g.CapabilityID), nil
	}
	t.transition(TransitionReference)
	return t.pickChoice(ctx, spec, dec, g, &choices[i])
}

// notAChoice repeats the pending question with the host's choices, saying the answer
// is not one of them.
func (t *turn) notAChoice(spec ap.Capability, g *sessions.Goal) outcome {
	m := messagesFor(t.lang)
	pending := g.Pending
	out := t.askInvalid(spec, g, []act.Issue{issue(contracts.ErrValidationFailed, pending.Field, m.notAChoice)})
	out.dir.NextQuestion = &conversation.DialogueDirectiveQuestion{
		Field: common.FieldName(pending.Field), AnswerKind: conversation.DialogueDirectiveQuestionAnswerKindChoice, Choices: pending.Choices,
	}
	g.Pending = pending
	return out
}

// ---- nested goals ----

// settle runs at the end of a turn, before the state is saved: a nested goal that has
// produced its result (or failed) completes, and control returns to its parent, whose
// own question is still pending. A goal that is not nested stays as it is until the
// user's next request replaces it.
func (t *turn) settle(ctx context.Context, out outcome) error {
	g := t.sess.State.Goal
	if g == nil || g.ParentID == "" || !finished(g) {
		return nil
	}
	receipt := ""
	if out.receipt != nil {
		receipt = string(out.receipt.ReceiptID)
	}
	return t.completeGoal(ctx, "", receipt)
}
