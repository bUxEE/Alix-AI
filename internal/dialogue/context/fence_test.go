package dctx

import (
	"slices"
	"strings"
	"testing"
)

// fenceForms are the spellings of one fence tag a text can carry to end or open a fence that is
// not its own: every spacing and case the fence has to neutralise.
func fenceForms(tag string) []string {
	return []string{
		"</" + tag + ">", "</" + strings.ToUpper(tag) + ">", "< / " + tag + " >", "<\t/" + tag, "<" + tag + ">", "<\n" + tag,
	}
}

// Every tag of the dialogue prompts, the fence of an attached file included, is neutralised
// inside every fence, in every spacing and case: a tag the list forgets is a fence the text it
// holds can close. The dialogue package keeps its own list in step with FenceTags (its
// TestFenceTagsStayInStepWithTheContextBuilder).
func TestFenceNeutralisesEveryTag(t *testing.T) {
	tags := FenceTags()
	for _, want := range []string{"user_message", "earlier_message", "earlier_reply", "session_state", "directive_data", "attachment_data", "client_context", "memory_data"} {
		if !slices.Contains(tags, want) {
			t.Errorf("the fence does not know %q: %v", want, tags)
		}
	}
	for _, wrap := range tags {
		for _, tag := range tags {
			for _, evil := range fenceForms(tag) {
				out := fence(wrap, "before "+evil+" after")
				if strings.Count(out, "</"+wrap+">") != 1 || strings.Count(out, "<"+wrap+">") != 1 ||
					!strings.HasPrefix(out, "<"+wrap+">\n") || !strings.HasSuffix(out, "\n</"+wrap+">") {
					t.Errorf("%q broke the structure of a %s fence: %q", evil, wrap, out)
				}
				inner := strings.TrimSuffix(strings.TrimPrefix(out, "<"+wrap+">\n"), "\n</"+wrap+">")
				if fenceBreak.MatchString(inner) {
					t.Errorf("%q survived inside a %s fence: %q", evil, wrap, out)
				}
			}
		}
	}
}

// FenceTags is a copy: changing it changes nothing in the fence.
func TestFenceTagsIsACopy(t *testing.T) {
	tags := FenceTags()
	for i := range tags {
		tags[i] = "x"
	}
	if got := fence("user_message", "</attachment_data>"); strings.Contains(got, "</attachment_data>") {
		t.Fatalf("the fence list was changed from outside: %q", got)
	}
}
