package dialogue

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Directive construction. A DialogueDirective is built here, by deterministic
// code, from validated state and typed host results: its act, codes, question and
// confirmation are the core's, its facts and suggestions are the host's, and no
// property of it has a path from model output (docs/integration/conversation-objects.md,
// section 4). The model only phrases it; internal/verify checks the phrasing.

// outcome is what a pipeline stage decided for the turn: the directive, and the
// objects the turn produced.
type outcome struct {
	dir      *conversation.DialogueDirective
	receipt  *contracts.ActionReceipt
	prepared *contracts.PreparedAction
	// token is the confirmation secret of prepared. It goes to the Result and
	// nowhere else.
	token actions.ConfirmationToken
	code  contracts.ErrCode
	// capID is the capability whose field labels the response uses.
	capID string
	// ui is the validated UI action the turn carries to the client (Result.UIAction).
	ui *UIAction
	// resultIDs and resultValues are the host records of a present_result read, kept so the
	// deterministic template can emit a validated table (and optional chart) of those rows.
	resultIDs    []act.RecordRef
	resultValues []act.Fact
	// analystReply is a verifier-accepted analyst answer (ADR 0047). When set, respond
	// uses it instead of a second model call.
	analystReply string
}

// newDirective returns an empty directive for an act, with every required array
// present and the confirmation requirement none.
func (t *turn) newDirective(a conversation.DialogueDirectiveAct, capID string) *conversation.DialogueDirective {
	d := &conversation.DialogueDirective{
		TurnID:               act.TurnID(t.id),
		Act:                  a,
		Facts:                []act.Fact{},
		MustState:            []common.FieldName{},
		MissingFields:        []common.FieldName{},
		InvalidFields:        []act.Issue{},
		AllowedSuggestions:   []act.Choice{},
		Confirmation:         act.ConfirmationRequirement{Mode: act.ConfirmationRequirementModeNone, Bindings: []act.ConfirmationRequirementBindingsItem{}},
		PermittedNextActions: []act.PermittedNextAction{},
		Sources:              []conversation.Citation{},
	}
	if capID != "" {
		c := common.CapabilityID(capID)
		d.CapabilityID = &c
	}
	return d
}

// refusalCodes are the codes that are a refusal to act rather than a failure to
// act: the request is not one this caller may make or the application does not do.
func isRefusal(c contracts.ErrCode) bool {
	switch c {
	case contracts.ErrUnknownCapability, contracts.ErrPermissionDenied, contracts.ErrScopeViolation,
		contracts.ErrScopeMissing, contracts.ErrQuarantined:
		return true
	}
	return false
}

// refuse is the refusal directive: out of the application, or not allowed.
func (t *turn) refuse(code contracts.ErrCode, capID string) outcome {
	d := t.newDirective(conversation.DialogueDirectiveActRefuse, capID)
	d.ReasonCode = errorCode(code)
	return outcome{dir: d, code: code, capID: capID}
}

// failure is the failure directive: the request was fine and could not be done.
// Nothing was executed (or, for an uncertain outcome, nothing is claimed).
func (t *turn) failure(code contracts.ErrCode, capID string) outcome {
	if isRefusal(code) {
		return t.refuse(code, capID)
	}
	if !code.Valid() {
		code = contracts.ErrInternal
	}
	d := t.newDirective(conversation.DialogueDirectiveActReportFailure, capID)
	d.ReasonCode = errorCode(code)
	return outcome{dir: d, code: code, capID: capID}
}

// acknowledgeCancel confirms that the goal was dropped.
func (t *turn) acknowledgeCancel(capID string) outcome {
	return outcome{dir: t.newDirective(conversation.DialogueDirectiveActAcknowledgeCancel, capID), capID: capID}
}

// offerHelp lists what the application can do for this caller: the capabilities
// the policy gateway exposed, by their package titles.
func (t *turn) offerHelp(suggestions []act.Choice) outcome {
	d := t.newDirective(conversation.DialogueDirectiveActOfferHelp, "")
	d.AllowedSuggestions = suggestions
	return outcome{dir: d}
}

// choiceID makes a choice ID from a value: the value itself when it is a valid
// identifier, otherwise opt_<n>.
func choiceID(value string, n int) string {
	if validIdentifier(value) {
		return value
	}
	return "opt_" + strconv.Itoa(n)
}

// validIdentifier is the grammar of schemas/common/ids: 1 to 128 characters, no
// whitespace, no control characters and no "*".
func validIdentifier(s string) bool {
	if s == "" || len([]rune(s)) > 128 {
		return false
	}
	for _, r := range s {
		if r == '*' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// questionFor builds the question that asks for a field: free text, one of the
// package's enum values, or an entity to be resolved by the host.
func (t *turn) questionFor(spec ap.Capability, field string) (conversation.DialogueDirectiveQuestion, *sessions.Clarification) {
	q := conversation.DialogueDirectiveQuestion{Field: common.FieldName(field), AnswerKind: conversation.DialogueDirectiveQuestionAnswerKindText}
	cl := &sessions.Clarification{Field: field, Kind: sessions.ClarifyText}
	for _, f := range spec.Fields {
		if string(f.Name) != field {
			continue
		}
		switch f.Type {
		case ap.CapabilityFieldTypeEnum:
			if len(f.Values) >= 2 {
				var cs []act.Choice
				for i, v := range f.Values {
					if len(cs) == t.e.cfg.MaxChoices {
						break
					}
					cs = append(cs, act.Choice{ChoiceID: common.Identifier(choiceID(v, i+1)), Label: v, Value: textValue(v)})
				}
				q.AnswerKind = conversation.DialogueDirectiveQuestionAnswerKindChoice
				q.Choices = cs
				cl.Kind, cl.Choices = sessions.ClarifyChoice, cs
			}
		case ap.CapabilityFieldTypeEntityRef:
			q.AnswerKind = conversation.DialogueDirectiveQuestionAnswerKindEntity
			cl.Kind = sessions.ClarifyEntity
		}
	}
	return q, cl
}

// askMissing asks for the first missing required field. The directive lists every
// missing field; the question is one, in the order of the capability's fields.
func (t *turn) askMissing(spec ap.Capability, g *sessions.Goal, missing []string) outcome {
	d := t.newDirective(conversation.DialogueDirectiveActAskMissing, string(spec.ID))
	d.MissingFields = fieldNames(missing)
	q, cl := t.questionFor(spec, missing[0])
	d.NextQuestion = &q
	g.Pending = cl
	return outcome{dir: d, capID: string(spec.ID)}
}

// askInvalid reports values that failed validation and asks for the first again.
func (t *turn) askInvalid(spec ap.Capability, g *sessions.Goal, issues []act.Issue) outcome {
	d := t.newDirective(conversation.DialogueDirectiveActAskInvalid, string(spec.ID))
	d.InvalidFields = issues
	field := ""
	for _, is := range issues {
		if is.Field != nil {
			field = string(*is.Field)
			break
		}
	}
	if field != "" {
		q, cl := t.questionFor(spec, field)
		d.NextQuestion = &q
		g.Pending = cl
	}
	return outcome{dir: d, capID: string(spec.ID)}
}

// disambiguate lists the host's candidates for a mention. At least two choices.
func (t *turn) disambiguate(spec ap.Capability, g *sessions.Goal, field, entityType, version string, cands []EntityCandidate) outcome {
	d := t.newDirective(conversation.DialogueDirectiveActDisambiguate, string(spec.ID))
	cs := make([]act.Choice, 0, len(cands))
	for _, c := range cands {
		if len(cs) == t.e.cfg.MaxChoices {
			break
		}
		label := c.Label
		if strings.TrimSpace(label) == "" {
			label = c.ID
		}
		cs = append(cs, act.Choice{ChoiceID: common.Identifier(c.ID), Label: label, Value: identifierValue(c.ID)})
	}
	d.NextQuestion = &conversation.DialogueDirectiveQuestion{
		Field:      common.FieldName(field),
		AnswerKind: conversation.DialogueDirectiveQuestionAnswerKindChoice,
		Choices:    cs,
	}
	g.Pending = &sessions.Clarification{Field: field, Kind: sessions.ClarifyChoice, Choices: cs, EntityType: entityType, Version: version}
	return outcome{dir: d, capID: string(spec.ID)}
}

// summarise reduces a directive to what the next turn needs of it.
func summarise(d *conversation.DialogueDirective) *sessions.DirectiveSummary {
	s := &sessions.DirectiveSummary{TurnID: string(d.TurnID), Act: string(d.Act)}
	if d.CapabilityID != nil {
		s.CapabilityID = string(*d.CapabilityID)
	}
	for _, f := range d.MissingFields {
		s.MissingFields = append(s.MissingFields, string(f))
	}
	for _, is := range d.InvalidFields {
		if is.Field != nil {
			s.InvalidFields = append(s.InvalidFields, string(*is.Field))
		}
	}
	if q := d.NextQuestion; q != nil {
		sq := &sessions.Question{Field: string(q.Field), AnswerKind: string(q.AnswerKind)}
		for _, c := range q.Choices {
			sq.Choices = append(sq.Choices, sessions.ChoiceSummary{ID: string(c.ChoiceID), Label: c.Label})
		}
		s.Question = sq
	}
	if d.ReasonCode != nil {
		s.ReasonCode = contracts.ErrCode(*d.ReasonCode)
	}
	return s
}
