package agui

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The tests of M3-34 for what the capabilities document claims, the run that hands back a UI tool result, and the empty
// state CopilotKit sends with every run.

func TestCapabilitiesClaimOnlyWhatIsWired(t *testing.T) {
	cases := []struct {
		name         string
		w            Wiring
		notClaimed   []string
		claimed      []string
		clientTools  bool
		clientClaims bool
	}{
		{"nothing wired", Wiring{}, []string{FeatureClientContext, FeatureClientToolCalls}, nil, false, false},
		{"client context only", Wiring{ClientContext: true}, []string{FeatureClientToolCalls}, []string{FeatureClientContext}, false, false},
		{"a registered UI tool without context", Wiring{ClientTools: true}, []string{FeatureClientContext}, []string{FeatureClientToolCalls}, true, false},
		{"both", Wiring{ClientContext: true, ClientTools: true}, nil, []string{FeatureClientContext, FeatureClientToolCalls}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := CapabilitiesFor("0.0.0-test", tc.w)
			for _, f := range tc.notClaimed {
				if !slices.Contains(doc.NotClaimed, f) {
					t.Errorf("%s is claimed: %v", f, doc.NotClaimed)
				}
			}
			for _, f := range tc.claimed {
				if slices.Contains(doc.NotClaimed, f) {
					t.Errorf("%s is wired and still listed as not claimed: %v", f, doc.NotClaimed)
				}
			}
			if got := doc.Agent["tools"].(map[string]any)["clientProvided"]; got != tc.clientTools {
				t.Errorf("tools.clientProvided = %v, want %v", got, tc.clientTools)
			}
			agent, _ := json.Marshal(doc.Agent)
			if err := vendored(t).Validate("AgentCapabilities", agent); err != nil {
				t.Errorf("the document does not validate: %v", err)
			}
			for _, f := range []string{"reasoning_events", "subagent_events", "raw_events", "voice_full_duplex"} {
				if !slices.Contains(doc.NotClaimed, f) {
					t.Errorf("%s is claimed whatever the wiring", f)
				}
			}
		})
	}
	// The package-level list is the baseline and is never edited by building a document.
	_ = CapabilitiesFor("v", Wiring{ClientContext: true, ClientTools: true})
	if !slices.Contains(NotClaimed, FeatureClientContext) || !slices.Contains(NotClaimed, FeatureClientToolCalls) {
		t.Errorf("NotClaimed was edited: %v", NotClaimed)
	}
}

func TestUIToolNameAllowedIsTheNameRuleOfIntake(t *testing.T) {
	isCap := func(id string) bool { return id == "items.create" }
	for name, want := range map[string]bool{
		"highlightRow": true, "open.panel:1-x_y": true, "a": true, strings.Repeat("a", 64): true,
		"": false, "1st": false, "has space": false, strings.Repeat("a", 65): false,
		"alixai_confirm_action": false, "AliXAI.x": false, "alixaiTool": false,
		"items.create": false, "ITEMS.CREATE": false,
	} {
		if got := UIToolNameAllowed(name, isCap); got != want {
			t.Errorf("UIToolNameAllowed(%q) = %v, want %v", name, got, want)
		}
	}
	if !UIToolNameAllowed("items.create", nil) {
		t.Error("with no capability check the name is a plain tool name")
	}
}

// The run that hands back a UI tool result is a start, the state and a finish: no history snapshot (which would replace the
// tool call and its result the client holds), no turn, nothing looked up.
func TestUIResultRunHasNoHistoryAndNoTurn(t *testing.T) {
	for _, mode := range []StateMode{StateSnapshotMode, StatePatchMode} {
		rc := newRC()
		rc.Kind, rc.StateMode = RunKindConnect, mode
		before := ClientState{}
		rc.StateBefore = &before
		em, err := UIResultRun(rc)
		if err != nil {
			t.Fatal(err)
		}
		evs := eventsOf(em)
		if err := verifyLifecycle(evs); err != nil {
			t.Fatalf("lifecycle: %v", err)
		}
		var types []string
		for i, e := range em {
			if err := vendored(t).ValidateEvent(e.Data); err != nil {
				t.Fatalf("event %d does not validate: %v\n%s", i, err, e.Data)
			}
			if e.Seq != 0 {
				t.Errorf("event %d has seq %d, want 0 (no TurnEvents)", i, e.Seq)
			}
			p, _ := ParseEvent(e.Data)
			types = append(types, string(p.Type))
		}
		want := []string{"RUN_STARTED", "STATE_SNAPSHOT", "RUN_FINISHED"}
		if mode == StatePatchMode {
			want[1] = "STATE_DELTA"
		}
		if strings.Join(types, " ") != strings.Join(want, " ") {
			t.Errorf("mode %s: events = %v, want %v", mode, types, want)
		}
	}
}

// CopilotKit sends `state: {}` with every run that shares none. An empty object is no client data: it must not put a
// client block into the prompt of every run.
func TestAnEmptyStateIsNotClientData(t *testing.T) {
	env, _ := testEnv(t)
	for name, state := range map[string]string{"empty object": `{}`, "null": `null`, "empty array": `[]`, "spaced": ` { } `} {
		doc := `{"threadId":"ses1","runId":"r1","state":` + state + `,"context":[],"messages":[{"id":"m1","role":"user","content":"hello"}]}`
		req, err := intake(t, doc, "", env)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(req.Context.Lines) != 0 || req.Context.Text != "" {
			t.Errorf("%s became client context: %q", name, req.Context.Lines)
		}
	}
	doc := `{"threadId":"ses1","runId":"r1","state":{"page":"items"},"messages":[{"id":"m1","role":"user","content":"hello"}]}`
	if req, err := intake(t, doc, "", env); err != nil || len(req.Context.Lines) != 1 {
		t.Errorf("a state with a member is client context: %v, %v", req.Context.Lines, err)
	}
}
