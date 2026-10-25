package integrationaudit

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func TestConversationActNamesOneVocabulary(t *testing.T) {
	for _, a := range []conversation.DialogueDirectiveAct{
		conversation.DialogueDirectiveActAskMissing, conversation.DialogueDirectiveActAskInvalid, conversation.DialogueDirectiveActDisambiguate,
		conversation.DialogueDirectiveActRequestConfirmation, conversation.DialogueDirectiveActRequestStepUp, conversation.DialogueDirectiveActPresentResult,
		conversation.DialogueDirectiveActReportOutcome, conversation.DialogueDirectiveActReportFailure, conversation.DialogueDirectiveActRefuse,
		conversation.DialogueDirectiveActOfferHelp, conversation.DialogueDirectiveActAcknowledgeCancel, conversation.DialogueDirectiveActReportNoEvidence,
	} {
		if got, ok := ConversationAct(string(a)); !ok || got != a {
			t.Errorf("ConversationAct(%q) = %q, %v", a, got, ok)
		}
	}
	if got, ok := ConversationAct("ask_missing_field"); !ok || got != conversation.DialogueDirectiveActAskMissing {
		t.Errorf("ask_missing_field is the alias of ask_missing, got %q, %v", got, ok)
	}
	for _, name := range []string{"", "ask_clarification", "report_success", "Ask_Missing", "request_step_up "} {
		if got, ok := ConversationAct(name); ok {
			t.Errorf("ConversationAct(%q) = %q: not an act", name, got)
		}
	}
}

// Every act the conversation cases of the reference applications name resolves, so the
// harness never meets a spelling it does not know.
func TestConversationCasesOfTheReferencePackagesNameKnownActs(t *testing.T) {
	var turns int
	for _, dir := range []string{
		"../../examples/reference-apps/clinic-booking/package",
		"../../examples/reference-apps/facility-maintenance/package",
		"../../examples/reference-apps/ledger/package",
	} {
		res, err := apppkg.Load(context.Background(), apppkg.FromDir(dir), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
		if err != nil || res.Package == nil {
			t.Fatalf("%s: %v %v", dir, err, res.Findings)
		}
		for _, suite := range res.Package.Tests {
			for _, c := range suite.ConversationCases {
				for _, turn := range c.Turns {
					turns++
					if _, ok := ConversationAct(string(turn.Expect.Act)); !ok {
						t.Errorf("%s: conversation case %s expects act %q, which is not a dialogue act", dir, c.ID, turn.Expect.Act)
					}
				}
			}
		}
	}
	if turns < 10 {
		t.Fatalf("only %d conversation turns were read", turns)
	}
}
