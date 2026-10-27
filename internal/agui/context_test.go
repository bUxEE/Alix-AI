package agui

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildContextDropsAssistantClientMessages(t *testing.T) {
	const secretName = "Maria Rossi Confidential"
	msgs := []InputMessage{
		{ID: "u1", Role: "user", Content: json.RawMessage(`"Who attended?"`)},
		{ID: "a1", Role: "assistant", Content: mustRaw(t, "Attended by "+secretName+".")},
		{ID: "u2", Role: "user", Content: json.RawMessage(`"Thanks"`)},
	}
	// hintsEnd excludes the last user message (the utterance).
	ctx := buildContext(nil, nil, msgs, 2)
	if strings.Contains(ctx.Text, secretName) {
		t.Fatalf("resolved assistant text must not enter client context: %q", ctx.Text)
	}
	if strings.Contains(ctx.Text, "earlier assistant") {
		t.Fatalf("assistant hints must be dropped: %q", ctx.Text)
	}
	if !strings.Contains(ctx.Text, "earlier user message") || !strings.Contains(ctx.Text, "Who attended?") {
		t.Fatalf("earlier user hints must remain: %q", ctx.Text)
	}
}

func mustRaw(t *testing.T, s string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
