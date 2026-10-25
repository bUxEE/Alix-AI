package verify

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Render writes the deterministic response for a directive (and, for an outcome, the receipt it
// names) in opts.Language. It states exactly the facts the directive says must be stated, from
// the canonical values, and nothing else. Its phrasing is plain, and no model is involved, so it
// is the response used when a model's text fails Verify and the whole response when no model is
// available (degraded mode).
//
// Render returns the same errors as Verify: a directive it cannot read is never papered over.
func Render(d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options) (string, error) {
	v, err := prepare(d, r, opts)
	if err != nil {
		return "", err
	}
	return v.render(), nil
}

// fieldLabel returns the label of a capability field: the application's own, else the field name
// with underscores turned into spaces and the first letter capitalised.
func fieldLabel(opts Options, field string) string {
	if l := strings.TrimSpace(opts.FieldLabels[field]); l != "" {
		return l
	}
	s := strings.ReplaceAll(field, "_", " ")
	r, w := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[w:]
}

// lines accumulates the lines of a response.
type lines struct{ ls []string }

func (l *lines) add(s string)       { l.ls = append(l.ls, s) }
func (l *lines) addAll(ss []string) { l.ls = append(l.ls, ss...) }
func (l *lines) String() string     { return strings.Join(l.ls, "\n") }
func bullet(s string) string        { return "- " + s }

func (v *verification) render() string {
	m := messagesFor(v.lang)
	var out lines
	d := v.d
	switch d.Act {
	case conversation.DialogueDirectiveActAskMissing:
		out.add(m.needMore)
		if q := d.NextQuestion; q != nil {
			out.addAll(v.questionLines(m, q))
		} else if len(d.MissingFields) > 0 {
			names := make([]string, len(d.MissingFields))
			for i, f := range d.MissingFields {
				names[i] = v.label(string(f))
			}
			out.add(sprintf(m.stillNeeded, strings.Join(names, ", ")))
		}
		out.addAll(v.factLines(v.shownFacts(false)))
	case conversation.DialogueDirectiveActAskInvalid:
		out.add(m.invalid)
		for _, is := range d.InvalidFields {
			if is.Field != nil {
				out.add(bullet(v.label(string(*is.Field)) + ": " + is.Message))
			} else {
				out.add(bullet(is.Message))
			}
		}
		if q := d.NextQuestion; q != nil {
			out.addAll(v.questionLines(m, q))
		}
		out.addAll(v.factLines(v.shownFacts(false)))
	case conversation.DialogueDirectiveActDisambiguate:
		out.add(m.disambig)
		if q := d.NextQuestion; q != nil {
			out.addAll(v.choiceLines(q.Choices))
		}
		out.addAll(v.factLines(v.shownFacts(false)))
	case conversation.DialogueDirectiveActRequestConfirmation:
		out.add(m.confirm)
		out.addAll(v.factLines(v.shownFacts(true)))
		if line := v.confirmationLine(m); line != "" {
			out.add(line)
		}
	case conversation.DialogueDirectiveActRequestStepUp:
		out.add(m.stepUp)
		out.addAll(v.factLines(v.shownFacts(true)))
		if v.permitted(actions.PermittedNextActionActionContinueInApp) {
			out.add(m.continueHere)
		}
	case conversation.DialogueDirectiveActPresentResult:
		facts := v.shownFacts(true)
		sources := v.sourceLines(m)
		if len(facts) == 0 && len(sources) == 0 {
			out.add(m.presentNothing)
		} else {
			out.add(m.present)
			out.addAll(v.factLines(facts))
			out.addAll(sources)
		}
		out.addAll(v.alsoLines(m))
	case conversation.DialogueDirectiveActReportOutcome:
		out.addAll(v.outcomeLines(m))
		out.addAll(v.alsoLines(m))
	case conversation.DialogueDirectiveActReportFailure:
		out.add(m.failure)
		out.add(v.reasonSentence(m))
		out.addAll(v.factLines(v.shownFacts(false)))
		out.addAll(v.alsoLines(m))
	case conversation.DialogueDirectiveActRefuse:
		out.add(m.refuse)
		out.add(v.reasonSentence(m))
		out.addAll(v.alsoLines(m))
	case conversation.DialogueDirectiveActOfferHelp:
		if len(d.AllowedSuggestions) > 0 {
			out.add(m.helpIntro)
			out.addAll(v.choiceLines(d.AllowedSuggestions))
		} else {
			out.add(m.helpNone)
		}
	case conversation.DialogueDirectiveActAcknowledgeCancel:
		out.add(m.cancelled)
	case conversation.DialogueDirectiveActReportNoEvidence:
		out.add(m.noEvidence)
	}
	return out.String()
}

func sprintf(format string, a ...string) string {
	args := make([]any, len(a))
	for i, s := range a {
		args[i] = s
	}
	return fmt.Sprintf(format, args...)
}

// shownFacts returns the facts the response states: those named in must_state, in that order; and
// when none is named and allFallback is set, every fact.
func (v *verification) shownFacts(allFallback bool) []typedFact {
	if len(v.d.MustState) == 0 {
		if allFallback {
			return v.facts
		}
		return nil
	}
	var out []typedFact
	used := map[int]bool{}
	for _, name := range v.d.MustState {
		for i, tf := range v.facts {
			if tf.field == string(name) && !used[i] {
				used[i] = true
				out = append(out, tf)
			}
		}
	}
	return out
}

// statedFacts returns the facts the template for this directive's act states, which are the
// facts a response states as "<label>: <value>". It is shownFacts as render asks for it, act by
// act, so what the verifier treats as stated and what the template writes cannot drift apart.
func (v *verification) statedFacts() []typedFact {
	switch v.d.Act {
	case conversation.DialogueDirectiveActRequestConfirmation, conversation.DialogueDirectiveActRequestStepUp,
		conversation.DialogueDirectiveActPresentResult, conversation.DialogueDirectiveActReportOutcome:
		return v.shownFacts(true)
	}
	return v.shownFacts(false)
}

func (v *verification) factLines(facts []typedFact) []string {
	out := make([]string, 0, len(facts))
	for _, tf := range facts {
		out = append(out, bullet(v.label(tf.field)+": "+v.formatFact(tf)))
	}
	return out
}

func (v *verification) choiceLines(cs []actions.Choice) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		s := c.Label
		if c.Description != nil && *c.Description != "" {
			s += " (" + *c.Description + ")"
		}
		out = append(out, bullet(s))
	}
	return out
}

func (v *verification) questionLines(m *messages, q *conversation.DialogueDirectiveQuestion) []string {
	label := v.label(string(q.Field))
	switch q.AnswerKind {
	case conversation.DialogueDirectiveQuestionAnswerKindChoice:
		return append([]string{sprintf(m.chooseOne, label)}, v.choiceLines(q.Choices)...)
	case conversation.DialogueDirectiveQuestionAnswerKindEntity:
		return []string{sprintf(m.whichOne, label)}
	case conversation.DialogueDirectiveQuestionAnswerKindConfirmation:
		return []string{m.yesNo}
	}
	return []string{sprintf(m.provide, label)}
}

func (v *verification) permitted(a actions.PermittedNextActionAction) bool {
	for _, na := range v.d.PermittedNextActions {
		if na.Action == a {
			return true
		}
	}
	return false
}

// confirmationLine asks for the confirmation and names only the next actions the directive
// permits.
func (v *verification) confirmationLine(m *messages) string {
	type opt struct {
		action actions.PermittedNextActionAction
		verb   string
	}
	var verbs []string
	for _, o := range []opt{
		{actions.PermittedNextActionActionConfirm, m.verbConfirm},
		{actions.PermittedNextActionActionModify, m.verbModify},
		{actions.PermittedNextActionActionReject, m.verbReject},
		{actions.PermittedNextActionActionCancel, m.verbCancel},
	} {
		if v.permitted(o.action) {
			verbs = append(verbs, o.verb)
		}
	}
	var line string
	if v.permitted(actions.PermittedNextActionActionConfirm) {
		line = m.confirmAsk
	}
	if len(verbs) > 0 {
		list := verbs[0]
		switch {
		case len(verbs) == 2:
			list = verbs[0] + " " + m.and + " " + verbs[1]
		case len(verbs) > 2:
			list = strings.Join(verbs[:len(verbs)-1], ", ") + " " + m.and + " " + verbs[len(verbs)-1]
		}
		if line != "" {
			line += " "
		}
		line += sprintf(m.confirmOptions, list)
	}
	return line
}

// alsoLines names the next actions the host gave a label.
func (v *verification) alsoLines(m *messages) []string {
	var labels []string
	for _, na := range v.d.PermittedNextActions {
		if na.Label != nil && *na.Label != "" {
			labels = append(labels, *na.Label)
		}
	}
	if len(labels) == 0 {
		return nil
	}
	return []string{sprintf(m.alsoYouCan, strings.Join(labels, ", "))}
}

func (v *verification) reasonSentence(m *messages) string {
	if v.d.ReasonCode == nil {
		return m.generic
	}
	if s, ok := m.reasons[contracts.ErrCode(*v.d.ReasonCode)]; ok {
		return s
	}
	return m.generic
}

// sourceLines states what the sources say, with their citation markers.
func (v *verification) sourceLines(m *messages) []string {
	var claims, list []string
	for i, s := range v.d.Sources {
		if !v.citable[i] {
			continue
		}
		marker := "[" + strconv.Itoa(i+1) + "]"
		for _, c := range s.Claims {
			claims = append(claims, bullet(c+" "+marker))
		}
		entry := marker + " " + s.DisplayName
		var where []string
		if s.Locator != nil {
			if s.Locator.Page != nil {
				where = append(where, pageWord(v.lang)+" "+strconv.FormatInt(*s.Locator.Page, 10))
			}
			if s.Locator.Section != nil {
				where = append(where, *s.Locator.Section)
			}
		}
		if len(where) > 0 {
			entry += " (" + strings.Join(where, ", ") + ")"
		}
		list = append(list, entry)
	}
	if len(list) == 0 {
		return nil
	}
	return append(append(claims, m.sources), list...)
}

func pageWord(l Language) string {
	if l == LangIT {
		return "pagina"
	}
	return "page"
}

// outcomeLines states the outcome of a commit from its receipt. Completion is stated only for a
// committed receipt; without a receipt the outcome is stated as unconfirmed.
func (v *verification) outcomeLines(m *messages) []string {
	var out lines
	r := v.r
	committed := r != nil && r.Status == actions.ActionReceiptStatusCommitted
	switch {
	case r == nil:
		out.add(m.uncertain)
	case r.Status == actions.ActionReceiptStatusCommitted:
		out.add(m.committed)
	case r.Status == actions.ActionReceiptStatusRejected:
		out.add(m.rejected)
	case r.Status == actions.ActionReceiptStatusQueued:
		out.add(m.queued)
	case r.Status == actions.ActionReceiptStatusPartial:
		out.add(m.partial)
	default: // uncertain
		out.add(m.uncertain)
	}
	// The facts the directive says to state. Of a request that did not complete they are the
	// details of the request, and are labelled as such, never as a result.
	if facts := v.factLines(v.shownFacts(true)); len(facts) > 0 {
		if !committed {
			out.add(m.requested)
		}
		out.addAll(facts)
	}
	if r != nil && !committed && len(r.Errors) > 0 {
		out.add(m.details)
		for _, is := range r.Errors {
			out.add(bullet(is.Message))
		}
	}
	if r != nil && r.FollowUp != nil {
		out.add(sprintf(m.followUp, followState(m, r.FollowUp.State)))
	}
	return out.ls
}

func followState(m *messages, s actions.FollowUpState) string {
	switch s {
	case actions.FollowUpStateQueued:
		return m.followQueued
	case actions.FollowUpStateRunning:
		return m.followRunning
	case actions.FollowUpStateSucceeded:
		return m.followSucceeded
	case actions.FollowUpStateFailed:
		return m.followFailed
	}
	return m.followUnknown
}

// formatFact writes a fact's value in the conventions of the response's language. A host's
// display form wins when there is one.
func (v *verification) formatFact(tf typedFact) string {
	m := messagesFor(v.lang)
	if tf.hasDisplay && tf.display != "" {
		return tf.display
	}
	switch tf.kind {
	case fkText, fkIdentifier:
		return tf.text
	case fkInteger:
		return tf.raw
	case fkDecimal:
		return localizeDecimal(tf.raw, v.lang)
	case fkBoolean:
		if tf.boolean {
			return m.yes
		}
		return m.no
	case fkDate:
		return formatDate(m, tf.date)
	case fkTime:
		return formatTimeAtom(tf.tm)
	case fkDatetime:
		s := formatDate(m, tf.localDate) + ", " + formatTimeAtom(tf.localTime)
		if tf.localTime.n3 == 0 {
			s = formatDate(m, tf.localDate) + ", " + pad(tf.localTime.n1, 2) + ":" + pad(tf.localTime.n2, 2)
		}
		if !tf.zoneNamed {
			s += " " + m.utc
		}
		return s
	case fkMoney:
		return localizeDecimal(tf.raw, v.lang) + " " + tf.cur
	}
	return ""
}

func formatDate(m *messages, a atom) string {
	return strconv.Itoa(a.n3) + " " + m.months[a.n2-1] + " " + strconv.Itoa(a.n1)
}

func localizeDecimal(raw string, l Language) string {
	if l == LangIT {
		return strings.Replace(raw, ".", ",", 1)
	}
	return raw
}
