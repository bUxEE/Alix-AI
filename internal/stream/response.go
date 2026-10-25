package stream

import (
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// MaxTokenRunes is the schema's maxLength of a token event's text.
const MaxTokenRunes = 4096

// PublishResponse publishes the text of a response as token events, after
// sanitising it as a whole. This is the only way model Markdown should reach a
// stream: the whole text goes through blocks.Validator.SanitizeResponse first
// (raw HTML and images removed, links limited to the allowed schemes, every
// construct escaped), and only the sanitised text is cut into fragments of at
// most MaxTokenRunes runes, so no fragment boundary can split a construct into
// two harmless-looking halves that become active when a client joins them. The
// fragments, concatenated, are exactly the sanitised text.
//
// The events take the next sequence numbers of the turn. Text with nothing to
// show (empty or white space) publishes nothing and is not an error. A response
// the sanitiser refuses (invalid encoding, invisible characters, too large,
// nested too deeply) publishes nothing and returns the refusal; the caller
// decides what the user is told, for example with Fail.
func (t *Turn) PublishResponse(text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	clean, _, err := t.hub.cfg.Validator.SanitizeResponse(text)
	if err != nil {
		return refuse(err, "the response was refused by sanitisation")
	}
	turn := actions.TurnID(t.id)
	for _, chunk := range chunkRunes(clean, MaxTokenRunes) {
		ev := conversation.TurnEvent{Value: conversation.TokenEvent{
			TurnID: turn, Seq: conversation.Seq(t.NextSeq()), Text: chunk,
		}}
		if err := t.Publish(ev); err != nil {
			return err
		}
	}
	return nil
}

// chunkRunes splits s into pieces of at most max runes, preferring to cut after
// a newline or a space and never inside a rune. The pieces concatenate to s.
func chunkRunes(s string, max int) []string {
	var out []string
	for utf8.RuneCountInString(s) > max {
		cut := runeOffset(s, max)
		if i := strings.LastIndexAny(s[:cut], "\n "); i > 0 {
			cut = i + 1
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// runeOffset is the byte offset of the n-th rune of s.
func runeOffset(s string, n int) int {
	i := 0
	for ; n > 0 && i < len(s); n-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return i
}
