package convstage

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The checks a replay is held to, on hand-made results: the failure paths of the stage,
// which the clinic reference application (which has none) cannot exercise.

func entityField(name, entityType string) ap.CapabilityField {
	et := ap.Slug(entityType)
	return ap.CapabilityField{Name: common.FieldName(name), Type: ap.CapabilityFieldTypeEntityRef, EntityType: &et}
}

// bookSpec is a consequential capability with two entities, as appointments.book is.
func bookSpec(mode ap.CapabilityConfirmationMode) ap.Capability {
	return ap.Capability{
		Fields:        []ap.CapabilityField{entityField("patient", "patient"), entityField("slot", "slot"), {Name: "reason", Type: ap.CapabilityFieldTypeString}},
		RiskLevel:     2,
		Confirmation:  ap.CapabilityConfirmation{Mode: mode},
		FailureStates: []ap.CapabilityFailureState{{Code: "conflict"}, {Code: "scope_violation"}},
	}
}

func bookExample() exampleRef {
	return exampleRef{capability: "appointments.book", example: ap.CapabilityExample{
		ID: "en-book", Language: "en", Utterance: "Book Maria Rossi the next available appointment",
		ExpectedFields: ap.ExampleFields{"patient": "Maria Rossi", "slot": "next available"},
	}}
}

func resolve(entityType, mention string) recordedCall {
	return recordedCall{Op: adapters.OpRead, CapabilityID: "appointments.book", Fields: map[string]any{"entity_type": entityType, "mention": mention}, Status: "ok"}
}

func call(op adapters.Operation) recordedCall {
	return recordedCall{Op: op, CapabilityID: "appointments.book", Status: "ok"}
}

func result(act conversation.DialogueDirectiveAct, code contracts.ErrCode, lang string, shortlist ...string) dialogue.Result {
	res := dialogue.Result{Code: code, Language: verify.Language(lang)}
	res.Directive.Act = act
	res.Response.Text = "a response"
	res.Trace.Shortlist = shortlist
	return res
}

func TestJudgeAcceptsAConformingReplay(t *testing.T) {
	calls := []recordedCall{resolve("patient", "Maria Rossi"), resolve("slot", "next available"), call(adapters.OpPrepare)}
	o := judge("reception-a1", bookSpec(ap.CapabilityConfirmationModeExplicit), bookExample(), calls,
		result(conversation.DialogueDirectiveActRequestConfirmation, "", "en", "appointments.book"), "appointments.book")
	if len(o.problems) != 0 || !o.reached || o.unrouted {
		t.Fatalf("%+v", o)
	}
}

func TestJudgeFindsWhatIsWrong(t *testing.T) {
	explicit := bookSpec(ap.CapabilityConfirmationModeExplicit)
	allMentions := []recordedCall{resolve("patient", "Maria Rossi"), resolve("slot", "next available")}
	cases := []struct {
		name   string
		spec   ap.Capability
		calls  []recordedCall
		res    dialogue.Result
		goal   string
		want   string // a fragment of the problem
		reach  bool
		routed bool
	}{
		{
			name: "a commit before the user confirmed", spec: explicit, calls: append(append([]recordedCall{}, allMentions...), call(adapters.OpPrepare), call(adapters.OpCommit)),
			res: result(conversation.DialogueDirectiveActRequestConfirmation, "", "en", "appointments.book"), want: "commit before the user confirmed", reach: true, routed: true,
		},
		{
			name: "a receipt without the confirmation", spec: explicit, calls: allMentions,
			res: func() dialogue.Result {
				r := result(conversation.DialogueDirectiveActReportOutcome, "", "en", "appointments.book")
				r.Receipt = &contracts.ActionReceipt{}
				return r
			}(),
			want: "receipt without the confirmation", routed: true,
		},
		{
			name: "a failure the capability does not declare", spec: explicit, calls: allMentions,
			res: result(conversation.DialogueDirectiveActReportFailure, "internal", "en", "appointments.book"), want: `"internal", which the capability does not declare`, routed: true,
		},
		{
			name: "the first entity never reaches the resolver", spec: explicit, calls: []recordedCall{call(adapters.OpPrepare)},
			res: result(conversation.DialogueDirectiveActAskInvalid, "", "en", "appointments.book"), want: `the patient "Maria Rossi" the example names never reached`, routed: true,
		},
		{
			name: "an entity is lost on the way to a confirmation", spec: explicit, calls: []recordedCall{resolve("patient", "Maria Rossi"), call(adapters.OpPrepare)},
			res: result(conversation.DialogueDirectiveActRequestConfirmation, "", "en", "appointments.book"), want: `the slot "next available" the example names never reached`, reach: true, routed: true,
		},
		{
			name: "a wrong mention", spec: explicit, calls: []recordedCall{resolve("patient", "Mario Rossi")},
			res: result(conversation.DialogueDirectiveActAskInvalid, "", "en", "appointments.book"), want: "never reached the application's resolver", routed: true,
		},
		{
			name: "the response is in another language", spec: explicit, calls: allMentions,
			res: result(conversation.DialogueDirectiveActRequestConfirmation, "", "it", "appointments.book"), want: `not the example's language "en"`, reach: true, routed: true,
		},
		{
			name: "no response at all", spec: explicit, calls: allMentions,
			res: func() dialogue.Result {
				r := result(conversation.DialogueDirectiveActRequestConfirmation, "", "en", "appointments.book")
				r.Response.Text = "  "
				return r
			}(),
			want: "no response", reach: true, routed: true,
		},
		{
			name: "the capability was shortlisted and then not taken", spec: explicit, calls: nil,
			res: result(conversation.DialogueDirectiveActOfferHelp, "", "en", "appointments.book"), want: "did not select the capability", routed: true,
		},
		{
			name: "a step-up capability commits", spec: bookSpec(ap.CapabilityConfirmationModeStepUp), calls: append(append([]recordedCall{}, allMentions...), call(adapters.OpCommit)),
			res: result(conversation.DialogueDirectiveActRequestStepUp, "", "en", "appointments.book"), want: "commit before the user confirmed", reach: true, routed: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := judge("p", c.spec, bookExample(), c.calls, c.res, c.goal)
			if got := strings.Join(o.problems, "; "); !strings.Contains(got, c.want) {
				t.Fatalf("problems = %q, want one with %q", got, c.want)
			}
			if o.reached != c.reach || o.unrouted == c.routed {
				t.Errorf("reached = %v (want %v), unrouted = %v", o.reached, c.reach, o.unrouted)
			}
		})
	}
}

func TestJudgeHoldsNothingAgainstAnEngineThatWasNeverAsked(t *testing.T) {
	// the router did not shortlist the capability: no proposal was made, so no mention,
	// capability or failure code is expected (and the refusal's code is the router's)
	o := judge("p", bookSpec(ap.CapabilityConfirmationModeExplicit), bookExample(), nil,
		result(conversation.DialogueDirectiveActRefuse, contracts.ErrUnknownCapability, "en"), "")
	if !o.unrouted || len(o.problems) != 0 || o.reached {
		t.Fatalf("%+v", o)
	}
	// a stopped turn does not owe the entities after the one it stopped on
	o = judge("p", bookSpec(ap.CapabilityConfirmationModeExplicit), bookExample(), []recordedCall{resolve("patient", "Maria Rossi")},
		result(conversation.DialogueDirectiveActDisambiguate, "", "en", "appointments.book"), "")
	if len(o.problems) != 0 {
		t.Fatalf("%+v", o.problems)
	}
	// a declared failure is a typed outcome, not a problem
	o = judge("p", bookSpec(ap.CapabilityConfirmationModeExplicit), bookExample(), []recordedCall{resolve("patient", "Maria Rossi")},
		result(conversation.DialogueDirectiveActReportFailure, "scope_violation", "en", "appointments.book"), "")
	if len(o.problems) != 0 || o.reached {
		t.Fatalf("%+v", o)
	}
}

func TestNaturalActs(t *testing.T) {
	for _, c := range []struct {
		name string
		spec ap.Capability
		want string
	}{
		{"a read answers", ap.Capability{RiskLevel: 0, Confirmation: ap.CapabilityConfirmation{Mode: ap.CapabilityConfirmationModeNone}}, actPresentResult},
		{"an explicit write asks", ap.Capability{RiskLevel: 2, Confirmation: ap.CapabilityConfirmation{Mode: ap.CapabilityConfirmationModeExplicit}}, actRequestConfirmation},
		{"a step-up write asks for the step-up", ap.Capability{RiskLevel: 3, Confirmation: ap.CapabilityConfirmation{Mode: ap.CapabilityConfirmationModeStepUp}}, actRequestStepUp},
		{"a low-impact write reports", ap.Capability{RiskLevel: 1, Confirmation: ap.CapabilityConfirmation{Mode: ap.CapabilityConfirmationModeNone}}, actReportOutcome},
	} {
		if got := naturalActs(c.spec); len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: %v, want %s", c.name, got, c.want)
		}
	}
}
