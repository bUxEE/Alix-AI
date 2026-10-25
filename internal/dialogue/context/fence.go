package dctx

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The fence tags. They are the labels the system frame tells the model to treat as
// untrusted data (the same labels the dialogue prompt builder uses, so one frame text
// serves both). Text that contains one of them, in any case or spacing, is neutralised
// so data cannot close its own fence early.
const (
	tagUser    = "user_message"
	tagEarlier = "earlier_message"
	tagReply   = "earlier_reply"
	tagState   = "session_state"
	// tagClient wraps what the client's screen supplied (readable context, the descriptions of its UI tools): data that
	// the user's browser wrote, placed by this package.
	tagClient = "client_context"
	// tagDirective and tagAttachment are not placed by this package; they stay in the list so
	// no fenced text of any consumer can forge them: the directive fence of the response prompt
	// and the fence of an attached file (the proposal prompt of a turn that carries attachments).
	tagDirective  = "directive_data"
	tagAttachment = "attachment_data"
)

// fenceTags is every fence tag of the dialogue prompts, the list the dialogue package keeps in
// step with (FenceTags). A tag one of the two forgets is a fence the text it holds can close.
var fenceTags = []string{tagUser, tagEarlier, tagReply, tagState, tagDirective, tagAttachment, tagClient}

// FenceTags returns the fence tags this package neutralises inside fenced text. The dialogue
// package builds its own fence from the same names, and a test compares the two lists.
func FenceTags() []string { return slices.Clone(fenceTags) }

var fenceBreak = regexp.MustCompile(`(?i)<\s*/?\s*(` + strings.Join(fenceTags, "|") + `)`)

// sanitize removes control and format characters (zero-width characters,
// bidirectional overrides, control codes) other than newline and tab, so what a reader
// sees is what the model reads.
func sanitize(s string) string {
	clean := true
	for _, r := range s {
		if r != '\n' && r != '\t' && (r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			clean = false
			break
		}
	}
	if clean {
		return s // the usual case: nothing to remove, nothing to allocate
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fence wraps untrusted text as labelled data. A fence tag inside the text is
// neutralised.
func fence(tag, text string) string {
	text = sanitize(text)
	if strings.IndexByte(text, '<') >= 0 { // a fence tag needs a '<'; most text has none
		text = fenceBreak.ReplaceAllStringFunc(text, func(m string) string {
			return "&lt;" + strings.TrimLeft(m, "< \t\r\n")
		})
	}
	return "<" + tag + ">\n" + text + "\n</" + tag + ">"
}

// clipRunes shortens s to at most n runes and marks the cut.
func clipRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n]), " ") + "..."
}

// oneLine collapses all white space, so a value cannot add lines to a list.
func oneLine(s string) string {
	return strings.Join(strings.Fields(sanitize(s)), " ")
}
