package agui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func TestMapTextReplacedEmitsCustomThenAllowsNewTokens(t *testing.T) {
	m, err := NewMapper(newRC())
	if err != nil {
		t.Fatal(err)
	}
	tid := actions.TurnID("trn_01J0000000000000000000000A")
	steps := []conversation.TurnEvent{
		{Value: conversation.TokenEvent{TurnID: tid, Seq: 0, Text: "Hello. "}},
		{Value: conversation.TokenEvent{TurnID: tid, Seq: 1, Text: "World."}},
		{Value: conversation.TextReplacedEvent{TurnID: tid, Seq: 2, Reason: "verification_failed"}},
		{Value: conversation.TokenEvent{TurnID: tid, Seq: 3, Text: "Template reply."}},
		{Value: conversation.DoneEvent{TurnID: tid, Seq: 4, FinishReason: conversation.DoneEventFinishReasonCompleted}},
	}
	var types []string
	for _, ev := range steps {
		out, err := m.Map(ev)
		if err != nil {
			t.Fatalf("Map %v: %v", ev.Value, err)
		}
		for _, e := range out {
			types = append(types, string(e.Event.Kind()))
			if e.Event.Kind() == Custom {
				var raw map[string]any
				if err := json.Unmarshal(e.Data, &raw); err != nil {
					t.Fatal(err)
				}
				if raw["name"] != TextReplacedName {
					t.Fatalf("custom name=%v", raw["name"])
				}
			}
		}
	}
	joined := strings.Join(types, ",")
	if !strings.Contains(joined, "CUSTOM") || !strings.Contains(joined, "TEXT_MESSAGE_CONTENT") {
		t.Fatalf("types=%v", types)
	}
	// After replace, a new message must start (not append to the superseded one).
	starts := 0
	for _, ty := range types {
		if ty == "TEXT_MESSAGE_START" {
			starts++
		}
	}
	if starts < 2 {
		t.Fatalf("want ≥2 TEXT_MESSAGE_START after replace, got %v", types)
	}
}
