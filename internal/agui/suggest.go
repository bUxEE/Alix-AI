package agui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Follow-up suggestions (CopilotKit's chat suggestions). They are computed, never generated: no model is called and the
// same directive, catalog and language always give the same list in the same order.
//
// What may be suggested is exactly what the directive already allows, which the core built from the host's data and
// intersected with policy, scope and the goal's state: the host's own AllowedSuggestions, and the capabilities named by
// PermittedNextActions. A capability the directive does not name is not suggested, so a suggestion can never reveal that
// a capability exists for a caller who may not use it. The capability catalog only supplies display strings for those
// capabilities (a title and the examples the application package ships); an example is shown, never used: it does not
// route, rank, authorise or prompt anything, and what the user does with a suggestion (sends it as a message) goes
// through the same pipeline as anything typed.

// Suggestion is one follow-up the user may pick. Picking one sends Message as a user message.
type Suggestion struct {
	Title   string `json:"title"`
	Message string `json:"message"`
	// CapabilityID is set for a suggestion that starts a capability.
	CapabilityID string `json:"capabilityId,omitempty"`
}

// CapabilityHint is what the caller of Suggest knows about a capability the caller may use: its title and the examples
// of its application package, as display strings.
type CapabilityHint struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Examples []ExampleHint `json:"examples,omitempty"`
}

// ExampleHint is one example utterance of a capability and its language tag.
type ExampleHint struct {
	Language  string `json:"language"`
	Utterance string `json:"utterance"`
}

const (
	// DefaultMaxSuggestions is how many suggestions a turn offers at most; MaxSuggestionsCeiling is the hard limit.
	DefaultMaxSuggestions = 4
	MaxSuggestionsCeiling = 6
	suggestionMaxRunes    = 160
)

// SuggestionsName is the name of the CUSTOM event that carries the suggestions: {"suggestions": [...]}.
const SuggestionsName = "alixai.suggestions"

// Suggest returns the follow-up suggestions for the turn whose directive is d, in the language lang (a BCP 47 tag),
// at most max (zero means DefaultMaxSuggestions). Only these directive acts offer any: a confirmation, a step-up, a
// refusal or a failure leaves the user with the one thing the turn asked of them.
func Suggest(d *conversation.DialogueDirective, lang string, catalog []CapabilityHint, max int) []Suggestion {
	if d == nil {
		return nil
	}
	switch d.Act {
	case conversation.DialogueDirectiveActAskMissing, conversation.DialogueDirectiveActAskInvalid,
		conversation.DialogueDirectiveActDisambiguate, conversation.DialogueDirectiveActPresentResult,
		conversation.DialogueDirectiveActReportOutcome, conversation.DialogueDirectiveActOfferHelp,
		conversation.DialogueDirectiveActAcknowledgeCancel:
	default:
		return nil
	}
	if max <= 0 {
		max = DefaultMaxSuggestions
	}
	max = min(max, MaxSuggestionsCeiling)

	byID := make(map[string]CapabilityHint, len(catalog))
	for _, c := range catalog {
		if _, dup := byID[c.ID]; !dup {
			byID[c.ID] = c
		}
	}
	var out []Suggestion
	seen := map[string]bool{}
	add := func(s Suggestion) {
		s.Title, s.Message = suggestionText(s.Title), suggestionText(s.Message)
		if s.Title == "" || s.Message == "" || seen[s.Message] || len(out) >= max {
			return
		}
		seen[s.Message] = true
		out = append(out, s)
	}
	// What the host offered: its own choices, relayed verbatim.
	for _, c := range d.AllowedSuggestions {
		add(Suggestion{Title: c.Label, Message: c.Label})
	}
	// What the core permits next: the capabilities it named, in its order.
	for _, a := range d.PermittedNextActions {
		if a.Action != actions.PermittedNextActionActionInvokeCapability || a.CapabilityID == nil {
			continue
		}
		hint, ok := byID[string(*a.CapabilityID)]
		if !ok {
			continue // not in the caller's catalog: nothing is suggested for it
		}
		title := hint.Title
		if title == "" && a.Label != nil {
			title = *a.Label
		}
		add(Suggestion{Title: title, Message: exampleFor(hint, lang, title), CapabilityID: hint.ID})
	}
	return out
}

// exampleFor picks the display message of a capability: the first example in the language of the turn, else the first
// example of the package, else the title.
func exampleFor(h CapabilityHint, lang, fallback string) string {
	want := baseLanguage(lang)
	for _, e := range h.Examples {
		if want != "" && baseLanguage(e.Language) == want && strings.TrimSpace(e.Utterance) != "" {
			return e.Utterance
		}
	}
	for _, e := range h.Examples {
		if strings.TrimSpace(e.Utterance) != "" {
			return e.Utterance
		}
	}
	return fallback
}

func baseLanguage(tag string) string {
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(tag)), "-")
	return base
}

// suggestionText makes one clean line of at most suggestionMaxRunes runes: control characters and line breaks become
// spaces, runs of white space collapse.
func suggestionText(s string) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if utf8.RuneCountInString(s) > suggestionMaxRunes {
		s = strings.TrimSpace(string([]rune(s)[:suggestionMaxRunes-1])) + "…"
	}
	return s
}
