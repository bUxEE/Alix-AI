package aguigold

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/agui"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
)

// The golden streams of M3-34: client context reaching the engine and UI tools (frontend actions). The model's proposals
// are scripted; whether the action is carried is the engine's decision and the registration is the host's, so the refusals
// are recorded from the real service with the registration and the declaration left out in turn.

const (
	uiTool      = "highlightRow"
	uiRowSchema = `{"type":"object","properties":{"rowId":{"type":"string","maxLength":16},"color":{"type":"string","enum":["yellow","red"]}},"required":["rowId"],"additionalProperties":false}`
)

func init() { scenarios = append(scenarios, uiScenarios...) }

// uiToolDecl is a tool the client declares (CopilotKit's useCopilotAction) in RunAgentInput.tools.
func uiToolDecl(name string) map[string]any {
	return map[string]any{"name": name, "description": "Highlights one row of the table the user is looking at.", "parameters": json.RawMessage(uiRowSchema)}
}

// onUIProposal scripts a proposal for a capability that also asks the client's screen to run a UI tool.
func (r *rig) onUIProposal(capability string, fields map[string]any, name string, args map[string]any) {
	var m map[string]any
	if err := json.Unmarshal([]byte(rigProposal(capability, "task", fields)), &m); err != nil {
		r.t.Fatal(err)
	}
	m["ui_action"] = map[string]any{"name": name, "arguments": args}
	raw, err := json.Marshal(m)
	if err != nil {
		r.t.Fatal(err)
	}
	r.model.On(pfake.Constrained(providers.ConstraintJSONSchema)).Reply(string(raw))
}

// clientBlocks counts the client context blocks that reached the model, and returns the first.
func (r *rig) clientBlocks() (n int, first string) {
	for _, p := range r.model.Prompts() {
		for _, m := range p.Chat.Messages {
			if strings.HasPrefix(m.Content, "<client_context>") {
				if n == 0 {
					first = m.Content
				}
				n++
			}
		}
	}
	return n, first
}

// uiCallID reads the id of the UI tool call out of a recorded stream.
func uiCallID(t *testing.T, sse string) string {
	t.Helper()
	for _, line := range strings.Split(sse, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type       string `json:"type"`
			ToolCallID string `json:"toolCallId"`
		}
		if json.Unmarshal([]byte(data), &ev) == nil && ev.Type == "TOOL_CALL_START" && strings.HasPrefix(ev.ToolCallID, agui.UICallPrefix) {
			return ev.ToolCallID
		}
	}
	t.Fatalf("the stream has no UI tool call: %s", sse)
	return ""
}

// uiRefusal records a run whose proposal asks for a UI action that must not be carried: the proposal fails whole, so the
// run ends in RUN_ERROR and nothing the proposal named (a complete items.create, which would otherwise be prepared and
// asked to confirm) exists.
func uiRefusal(t *testing.T, run string, registered []string, declared []string, proposed string) *recorded {
	r := newRig(t, registered...)
	r.onUIProposal(capCreate, map[string]any{"name": "widget", "quantity": 3, "size": "small"}, proposed, map[string]any{"rowId": "r_41"})
	r.onResponse("Please confirm: create an item named widget.")
	var tools []any
	for _, n := range declared {
		tools = append(tools, uiToolDecl(n))
	}
	rec := r.record("engine", runInput(run, map[string]any{"tools": tools}, userMessage("m1", "create an item named widget with quantity 3")))
	if r.host.Effects() != 0 || rec.secretResponse {
		t.Fatalf("a refused UI action left effects (%d) or a confirmation secret", r.host.Effects())
	}
	return rec
}

var uiScenarios = []scenario{
	{
		name:        "client_context",
		description: "The page hands the agent its state, a readable context item and a system message, as CopilotKit's useCoAgent and useCopilotReadable do. All of it is untrusted data: the run answers from the application, and the engine received the data as one fenced block of the proposal prompt (asserted while recording), never in the utterance or the system message.",
		expect:      []string{"RUN_STARTED", "STATE_DELTA", "TEXT_MESSAGE_CONTENT", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.seedItem()
			r.onProposal(capRead, "task", map[string]any{"item_id": "item-1"})
			r.onResponse("The item is named gadget.")
			extra := map[string]any{
				"state":   map[string]any{"page": "items", "selectedRow": "item-1"},
				"context": []any{map[string]any{"description": "The row the user selected", "value": "item-1 (gadget)"}},
			}
			rec := r.record("engine", runInput("run_context", extra,
				map[string]any{"id": "s1", "role": "system", "content": "You are looking at the items page."},
				userMessage("m1", "show item item-1")))
			n, block := r.clientBlocks()
			if n != 1 || strings.Count(block, "</client_context>") != 1 || !strings.Contains(block, "item-1 (gadget)") || !strings.Contains(block, "selectedRow") {
				t.Fatalf("the client data did not reach the engine as one fenced block (%d blocks):\n%s", n, block)
			}
			return rec
		},
	},
	{
		name:        "ui_tool_call",
		description: "A UI tool the client declared (highlightRow) and the host registered for the application: the model proposes a call after answering, the engine checks the arguments against the declared schema, and the run sends it as one tool call (START with the tool's name, ARGS, END) before it finishes. It is an instruction to the client's screen, never to the application: nothing is prepared or committed.",
		expect:      []string{"TEXT_MESSAGE_CONTENT", "TOOL_CALL_START:" + uiTool, "TOOL_CALL_ARGS", "TOOL_CALL_END", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t, uiTool)
			r.seedItem()
			r.onUIProposal(capRead, map[string]any{"item_id": "item-1"}, uiTool, map[string]any{"rowId": "item-1", "color": "yellow"})
			r.onResponse("The item is named gadget. I highlighted its row.")
			rec := r.record("engine", runInput("run_ui_call", map[string]any{"tools": []any{uiToolDecl(uiTool)}}, userMessage("m1", "show item item-1")))
			if r.host.Effects() != 0 {
				t.Fatalf("a UI action had %d effects on the application", r.host.Effects())
			}
			return rec
		},
	},
	{
		name:        "ui_tool_result",
		description: "The client ran the tool and sends the result back as a tool message for the call's id. It is data: a run with no turn (no model call, no history, nothing approved) that starts, sends the state and finishes. The client resends the result with the thread, so it reaches the next turn as one more fenced line of client context.",
		expect:      []string{"RUN_STARTED", "STATE_DELTA", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t, uiTool)
			r.seedItem()
			r.onUIProposal(capRead, map[string]any{"item_id": "item-1"}, uiTool, map[string]any{"rowId": "item-1"})
			r.onResponse("The item is named gadget. I highlighted its row.")
			first := r.post(runInput("run_ui_call", map[string]any{"tools": []any{uiToolDecl(uiTool)}}, userMessage("m1", "show item item-1")))
			if first.Code != http.StatusOK {
				t.Fatalf("the run that asked for the UI tool = %d: %s", first.Code, first.Body.String())
			}
			callID := uiCallID(t, first.Body.String())
			calls := r.model.Calls()
			rec := r.record("engine", runInput("run_ui_result", nil,
				userMessage("m1", "show item item-1"),
				map[string]any{"id": "a1", "role": "assistant", "content": "The item is named gadget. I highlighted its row."},
				map[string]any{"id": "t1", "role": "tool", "toolCallId": callID, "content": `{"highlighted": true, "rowId": "item-1"}`}))
			if r.model.Calls() != calls || r.host.Effects() != 0 {
				t.Fatalf("a UI tool result called the model or the application (%d calls more, %d effects)", r.model.Calls()-calls, r.host.Effects())
			}
			return rec
		},
	},
	{
		name:        "ui_tool_undeclared",
		description: "The host registered highlightRow and scrollTo, the client declared only highlightRow, and the model proposes scrollTo. The action is carried only when the client declared it for this run AND the host registered it, so the whole proposal fails: RUN_ERROR with validation_failed, no tool call, nothing prepared.",
		expect:      []string{"RUN_ERROR"},
		run: func(t *testing.T) *recorded {
			return uiRefusal(t, "run_ui_undeclared", []string{uiTool, "scrollTo"}, []string{uiTool}, "scrollTo")
		},
	},
	{
		name:        "ui_tool_unregistered",
		description: "The client declared highlightRow and the model proposes it, but the host registered no UI tool for the application (the default). A declaration is a claim of the client, not a permission, so the whole proposal fails: RUN_ERROR with validation_failed, no tool call, nothing prepared.",
		expect:      []string{"RUN_ERROR"},
		run: func(t *testing.T) *recorded {
			return uiRefusal(t, "run_ui_unregistered", nil, []string{uiTool}, uiTool)
		},
	},
	{
		name:        "ui_tool_capability_named",
		description: "The model proposes a UI action that carries the id of a capability of the application (items.create). A UI tool can never be a capability, whatever is declared and registered, so the whole proposal fails: RUN_ERROR with validation_failed, no tool call, nothing prepared. (A client that DECLARES a tool with a capability's name is refused earlier, with a 400 before any stream.)",
		expect:      []string{"RUN_ERROR"},
		run: func(t *testing.T) *recorded {
			return uiRefusal(t, "run_ui_capability", []string{uiTool, capCreate}, []string{uiTool}, capCreate)
		},
	},
}
