package injectionsec

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	dctx "github.com/buxee/alix-ai/internal/dialogue/context"
	"github.com/buxee/alix-ai/internal/providers"
)

// fenceTag matches an opening or closing fence tag of the dialogue prompts, in any case and spacing:
// the exact pattern the prompt builder neutralises inside data.
var fenceTag = regexp.MustCompile(`(?i)<\s*/?\s*(` + strings.Join(dctx.FenceTags(), "|") + `)`)

// fenceOpen is the opening of a fenced message: the tag on a line of its own.
var fenceOpen = regexp.MustCompile(`^<(` + strings.Join(dctx.FenceTags(), "|") + `)>\n`)

// unfenced says why a message that carries untrusted text is not fenced as data, or "" when it is:
// it is not in a system message, it is not marked trusted, it opens with one fence tag and ends with
// the matching closing tag, and no other fence tag, in any case or spacing, stands between them (so
// the data could not close its own fence, nor open another).
func unfenced(m providers.Message) string {
	switch {
	case m.Role == providers.MsgSystem:
		return "is in a system message"
	case m.Trusted:
		return "is in a message marked trusted"
	}
	open := fenceOpen.FindStringSubmatch(m.Content)
	if open == nil {
		return "does not open with a fence tag"
	}
	if !strings.HasSuffix(m.Content, "\n</"+open[1]+">") {
		return "does not end with the closing tag of its fence"
	}
	if n := len(fenceTag.FindAllString(m.Content, -1)); n != 2 {
		return fmt.Sprintf("holds %d fence tags, want exactly its own opening and closing tag", n)
	}
	return ""
}

// carrying lists the messages of the captured calls that contain text, with the call they were sent in.
type carrier struct {
	Call sent
	Msg  providers.Message
}

func carrying(calls []sent, text string) []carrier {
	var out []carrier
	for _, c := range calls {
		for _, m := range c.Prompt.Chat.Messages {
			if strings.Contains(m.Content, text) {
				out = append(out, carrier{c, m})
			}
		}
	}
	return out
}

// requireFenced fails the test unless the text was sent at least once and every message that carries
// it is fenced as data. It returns the carriers so a caller can say where it was seen.
func requireFenced(t testing.TB, calls []sent, text, what string) []carrier {
	t.Helper()
	cs := carrying(calls, text)
	if len(cs) == 0 {
		t.Fatalf("%s never reached a prompt: the test did not exercise the fence", what)
	}
	for _, c := range cs {
		if why := unfenced(c.Msg); why != "" {
			t.Errorf("%s, sent in %s call %d, %s:\n%s", what, c.Call.Kind, c.Call.Seq, why, c.Msg.Content)
		}
	}
	return cs
}

// kinds counts, among carriers, how many were sent in calls of a kind.
func inKind(cs []carrier, kind string) int {
	n := 0
	for _, c := range cs {
		if c.Call.Kind == kind {
			n++
		}
	}
	return n
}
