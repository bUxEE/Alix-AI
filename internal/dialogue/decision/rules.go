package decision

import (
	"strings"
	"unicode"
)

// phrase normalises text for an exact lookup: lower case, letters and digits only, one
// space between words, no leading or trailing courtesy word. It is deliberately the same
// shape as the core's control phrases so "Book an appointment!" equals "book an
// appointment" and "appointments.book" equals "appointments book".
func phrase(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			space = false
		case r == '\'' || r == '’':
			// "don't" stays one word
		default:
			if !space {
				b.WriteByte(' ')
			}
			space = true
		}
	}
	out := strings.TrimSpace(b.String())
	for _, p := range []string{"please ", "per favore "} {
		if rest, ok := strings.CutPrefix(out, p); ok && rest != "" {
			out = rest
		}
	}
	for _, p := range []string{" please", " per favore", " grazie", " thanks"} {
		if rest, ok := strings.CutSuffix(out, p); ok && rest != "" {
			out = rest
		}
	}
	return out
}

// Named is a shortlisted capability with the exact names a user may say for it.
type Named struct {
	ID    string
	Names []string
}

// ExactCapability is the exact deterministic rule for capability routing: it returns the
// one candidate whose id or title the whole utterance equals (case, spacing and
// punctuation aside). It never matches inside a longer sentence and never matches when two
// candidates share the phrase. It is a function of its arguments: no model, no state.
func ExactCapability(utterance string, cands []Named) (string, bool) {
	p := phrase(utterance)
	if p == "" {
		return "", false
	}
	found := ""
	for _, c := range cands {
		for _, n := range c.Names {
			if phrase(n) != p {
				continue
			}
			if found != "" && found != c.ID {
				return "", false // ambiguous: not exact
			}
			found = c.ID
		}
	}
	return found, found != ""
}
