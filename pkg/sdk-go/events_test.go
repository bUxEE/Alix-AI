package sdkgo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

func TestTurnTextResetsOnTextReplaced(t *testing.T) {
	tid := actions.TurnID("trn_01J0000000000000000000000A")
	events := []conversation.TurnEvent{
		{Value: conversation.TokenEvent{TurnID: tid, Seq: 0, Text: "shown "}},
		{Value: conversation.TokenEvent{TurnID: tid, Seq: 1, Text: "text"}},
		{Value: conversation.TextReplacedEvent{TurnID: tid, Seq: 2, Reason: "verification_failed"}},
		{Value: conversation.TokenEvent{TurnID: tid, Seq: 3, Text: "template"}},
		{Value: conversation.DoneEvent{TurnID: tid, Seq: 4, FinishReason: conversation.DoneEventFinishReasonCompleted}},
	}
	if got := TurnText(events); got != "template" {
		t.Fatalf("TurnText = %q, want template only", got)
	}
}

func TestTurnEventTypesMatchSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "conversation", "turn-event.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		OneOf []struct {
			Ref string `json:"$ref"`
		} `json:"oneOf"`
		Defs map[string]struct {
			Properties struct {
				Type struct {
					Const string `json:"const"`
				} `json:"type"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, item := range schema.OneOf {
		name := strings.TrimPrefix(item.Ref, "#/$defs/")
		def, ok := schema.Defs[name]
		if !ok || def.Properties.Type.Const == "" {
			t.Fatalf("oneOf ref %q has no type const", item.Ref)
		}
		want = append(want, def.Properties.Type.Const)
	}
	if !slices.Equal(TurnEventTypes, want) {
		t.Fatalf("TurnEventTypes = %v, schema = %v", TurnEventTypes, want)
	}
}

// SEC-097: parseTurnEvent refuses what schemas/conversation/turn-event refuses.
// Each reject case is an input the typed decoder alone delivers as an event.
func TestParseTurnEventRejectsWhatTheSchemaRejects(t *testing.T) {
	const trn = "trn_01J0000000000000000000000A"
	head := `"type":"token","turn_id":"` + trn + `"`
	rejects := map[string]string{
		"a member name in another case":     `{"tYpe":"token","turn_id":"` + trn + `","seq":0,"text":"hi"}`,
		"the type missing":                  `{"turn_id":"` + trn + `","seq":0,"text":"hi"}`,
		"seq missing (zero-filled)":         `{` + head + `,"text":"hi"}`,
		"text missing (zero-filled)":        `{` + head + `,"seq":0}`,
		"seq and text missing":              `{` + head + `}`,
		"turn_id in another case":           `{"type":"token","Turn_ID":"` + trn + `","seq":0,"text":"hi"}`,
		"turn_id missing":                   `{"type":"token","seq":0,"text":"hi"}`,
		"turn_id not a turn id":             `{"type":"token","turn_id":"trn_unknown","seq":0,"text":"hi"}`,
		"seq negative":                      `{` + head + `,"seq":-1,"text":"hi"}`,
		"seq over the maximum":              `{` + head + `,"seq":4294967296,"text":"hi"}`,
		"empty text":                        `{` + head + `,"seq":0,"text":""}`,
		"text over 4096 characters":         `{` + head + `,"seq":0,"text":"` + strings.Repeat("a", 4097) + `"}`,
		"an unknown member":                 `{` + head + `,"seq":0,"text":"hi","extra":1}`,
		"null for a required member":        `{` + head + `,"seq":0,"text":null}`,
		"done without finish_reason":        `{"type":"done","turn_id":"` + trn + `","seq":1}`,
		"an unknown type":                   `{"type":"mystery","turn_id":"` + trn + `","seq":0}`,
		"not an object":                     `[1]`,
		"a null document":                   `null`,
		"a duplicate-case twin of a member": `{` + head + `,"seq":0,"text":"hi","TEXT":"x"}`,
	}
	for name, data := range rejects {
		t.Run("rejects "+name, func(t *testing.T) {
			ev, err := parseTurnEvent([]byte(data))
			if err == nil {
				t.Fatalf("delivered %+v for %s", ev.Value, data)
			}
			if !contracts.HasCode(err, contracts.ErrInternal) {
				t.Errorf("err = %v, want an internal malformed_event error", err)
			}
		})
	}

	accepts := map[string]string{
		"a token":                  `{` + head + `,"seq":0,"text":"hi"}`,
		"the largest seq":          `{` + head + `,"seq":4294967295,"text":"hi"}`,
		"a 4096-character text":    `{` + head + `,"seq":3,"text":"` + strings.Repeat("a", 4096) + `"}`,
		"a text with multibyte":    `{` + head + `,"seq":3,"text":"` + strings.Repeat("é", 4096) + `"}`,
		"members in another order": `{"text":"hi","seq":0,"turn_id":"` + trn + `","type":"token"}`,
		"done":                     `{"type":"done","turn_id":"` + trn + `","seq":1,"finish_reason":"completed"}`,
	}
	for name, data := range accepts {
		t.Run("accepts "+name, func(t *testing.T) {
			ev, err := parseTurnEvent([]byte(data))
			if err != nil {
				t.Fatalf("refused %s: %v", data, err)
			}
			if _, ok := Envelope(ev); !ok {
				t.Fatalf("no envelope for %+v", ev.Value)
			}
		})
	}
}

// The schema's own examples are the corpus: every valid one is delivered, every invalid one is refused.
// (The generated decoder is shape-only, so the invalid ones are what shows whether the checks in
// parseTurnEvent cover the whole schema and not only the cases SEC-097 first named.)
func TestParseTurnEventAgreesWithTheSchemaExamples(t *testing.T) {
	root := filepath.Join("..", "..", "schemas", "conversation", "examples", "turn-event")
	for _, kind := range []string{"valid", "invalid"} {
		files, err := filepath.Glob(filepath.Join(root, kind, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no %s examples under %s (%v)", kind, root, err)
		}
		for _, f := range files {
			t.Run(kind+"/"+filepath.Base(f), func(t *testing.T) {
				raw, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				ev, err := parseTurnEvent(raw)
				switch {
				case kind == "valid" && err != nil:
					t.Errorf("a valid example was refused: %v", err)
				case kind == "invalid" && err == nil:
					t.Errorf("an invalid example was delivered as %+v", ev.Value)
				}
			})
		}
	}
}
