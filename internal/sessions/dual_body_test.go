package sessions

import (
	"context"
	"strings"
	"testing"
)

// TestDualAssistantBodiesKeepsTokensOutOfPromptWindow proves ADR 0049: the
// model window (RecentTurns / session_turns) keeps the token form; the
// transcript (history) shows the resolved display form.
func TestDualAssistantBodiesKeepsTokensOutOfPromptWindow(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newStore(t, Config{})
	sc := scopeOf("ten-a", "alice", "ses_dual")
	sess, err := s.Open(ctx, sc)
	if err != nil {
		t.Fatal(err)
	}
	const (
		tokens  = "Attended by {{ref:ds_att_001:0:attendee_name}}."
		display = "Attended by Maria Rossi."
	)
	sess, err = s.Save(ctx, sess, &TurnRecord{
		TurnID: "trn_1", User: "Who attended?", Act: "present_result",
		Assistant: tokens, AssistantDisplay: display,
	})
	if err != nil {
		t.Fatal(err)
	}

	recent, err := s.RecentTurns(ctx, sess, 10)
	if err != nil {
		t.Fatal(err)
	}
	var sawAssistant bool
	for _, row := range recent {
		if row.Role != "assistant" {
			continue
		}
		sawAssistant = true
		if row.Body != tokens {
			t.Fatalf("prompt window body = %q, want token form %q", row.Body, tokens)
		}
		if strings.Contains(row.Body, "Maria Rossi") {
			t.Fatal("resolved display text must never appear in RecentTurns (prompt path)")
		}
	}
	if !sawAssistant {
		t.Fatal("expected an assistant row in the recent window")
	}

	tr, err := s.Transcript(ctx, sc, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	sawAssistant = false
	for _, row := range tr {
		if row.Role != "assistant" {
			continue
		}
		sawAssistant = true
		if row.Body != display {
			t.Fatalf("transcript body = %q, want display form %q", row.Body, display)
		}
		if strings.Contains(row.Body, "{{ref:") {
			t.Fatal("token form must not appear in the user-facing transcript")
		}
	}
	if !sawAssistant {
		t.Fatal("expected an assistant row in the transcript")
	}
}
