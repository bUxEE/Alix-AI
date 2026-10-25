package integrationaudit

import (
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// ConversationAct resolves the dialogue act a conversation case of a package's tests/
// suite names (conversation_cases[].turns[].expect.act) to the one vocabulary there is: the
// acts of the DialogueDirective schema, whose generated enum is
// conversation.DialogueDirectiveAct (ask_missing, ask_invalid, disambiguate,
// request_confirmation, request_step_up, present_result, report_outcome, report_failure,
// refuse, offer_help, acknowledge_cancel, report_no_evidence).
//
// The package documentation used to give ask_missing_field as its example, and the first
// reference application still says it. The harness that replays conversation cases takes
// the generated enum as the truth and reads that spelling as the alias it is: ask_missing_field
// is ask_missing. Any other name that is not in the enum is not an act, and false is
// returned, so a case can never pass by naming a directive nobody emits.
func ConversationAct(name string) (conversation.DialogueDirectiveAct, bool) {
	if a, ok := conversationActAliases[name]; ok {
		return a, true
	}
	a := conversation.DialogueDirectiveAct(name)
	return a, a.Valid()
}

// conversationActAliases are the spellings a package may still use for an act.
var conversationActAliases = map[string]conversation.DialogueDirectiveAct{
	"ask_missing_field": conversation.DialogueDirectiveActAskMissing,
}
