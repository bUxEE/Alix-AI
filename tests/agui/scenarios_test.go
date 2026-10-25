package aguigold

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buxee/alix-ai/internal/agui"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/identity/identitytest"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// secretHeader is the header that carries the confirmation secret, on the response that prepared an action and on the resume.
const secretHeader = gateway.HeaderConfirmationToken

// recorded is one recorded exchange.
type recorded struct {
	// source says where the events come from: the engine, or the hub (see doc.go).
	source string
	method string
	path   string
	body   map[string]any
	// secretRequest is true when the request carried the confirmation secret in its header.
	secretRequest bool
	status        int
	contentType   string
	// secretResponse is true when the response carried a confirmation secret in its header.
	secretResponse bool
	sse            string
	// secrets are the values that must appear in no recorded file.
	secrets []string
}

// scenario is one golden stream.
type scenario struct {
	name        string
	description string
	// expect are the event keys the stream must contain (eventKeys): what the scenario exists to show.
	expect []string
	run    func(t *testing.T) *recorded
}

func (r *rig) record(source string, body map[string]any, headers ...string) *recorded {
	r.t.Helper()
	rec := r.post(body, headers...)
	out := &recorded{
		source: source, method: http.MethodPost, path: "/v1/agui", body: body, secretRequest: len(headers) > 0,
		status: rec.Code, contentType: rec.Header().Get("Content-Type"), sse: rec.Body.String(),
	}
	if s := rec.Header().Get(secretHeader); s != "" {
		out.secretResponse, out.secrets = true, append(out.secrets, s)
	}
	for i := 1; i < len(headers); i += 2 {
		out.secrets = append(out.secrets, headers[i])
	}
	return out
}

// prepare runs the turn that prepares items.create and returns the confirmation secret and the preparation id.
func (r *rig) prepare(run string) (secret, prep string) {
	r.t.Helper()
	r.onProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3, "size": "small"})
	r.onResponse("Please confirm: create an item named widget.")
	rec := r.post(runInput(run, nil, userMessage("m1", "create an item named widget with quantity 3")))
	secret = rec.Header().Get(secretHeader)
	if rec.Code != http.StatusOK || secret == "" {
		r.t.Fatalf("the preparing run = %d, secret %q: %s", rec.Code, secret, rec.Body.String())
	}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Outcome struct {
				Type       string `json:"type"`
				Interrupts []struct {
					ID string `json:"id"`
				} `json:"interrupts"`
			} `json:"outcome"`
		}
		if json.Unmarshal([]byte(data), &ev) == nil && ev.Type == "RUN_FINISHED" && ev.Outcome.Type == "interrupt" && len(ev.Outcome.Interrupts) == 1 {
			prep = ev.Outcome.Interrupts[0].ID
		}
	}
	if prep == "" {
		r.t.Fatalf("the preparing run did not end in an interrupt: %s", rec.Body.String())
	}
	return secret, prep
}

// knowledge is the dialogue.Knowledge of the citations scenario.
type knowledge struct {
	res   rag.AnswerResult
	asked atomic.Int32
}

func (k *knowledge) Answer(context.Context, contracts.Scope, rag.AnswerRequest) (rag.AnswerResult, error) {
	k.asked.Add(1)
	return k.res, nil
}

func citation(name, section string, claims ...string) conversation.Citation {
	return conversation.Citation{
		SourceID: act.SourceID(ids.New("src")), DisplayName: name, Version: "v7",
		Locator:      &conversation.CitationLocator{Section: &section},
		Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent,
		OpenRef: conversation.CitationOpenRef{Value: conversation.CitationOpenRefFile{Kind: "file", FileID: act.FileID(ids.New("fil"))}},
		Claims:  claims,
	}
}

const (
	warrantyQuestion = "what is the warranty on the Model X kettle"
	warrantyClaim1   = "The Model X kettle has a warranty of 24 months from the date of purchase."
	warrantyClaim2   = "Keep the receipt as proof of purchase."
)

func warrantyAnswer() rag.GroundedAnswer {
	return rag.GroundedAnswer{
		Claims: []rag.GroundedAnswerClaim{
			{Text: warrantyClaim1, CitationIndexes: []int64{0}},
			{Text: warrantyClaim2, CitationIndexes: []int64{1}},
		},
		Citations: []conversation.Citation{
			citation("Warranty terms", "Section 2", warrantyClaim1),
			citation("Customer handbook", "Section 5", warrantyClaim2),
		},
		Notices: rag.RetrievalNotices{},
	}
}

// ---- hub-recorded streams ----

const examples = "../../schemas/conversation/examples"

func readExample(t *testing.T, rel string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(examples, rel))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// publish puts one TurnEvent document into the turn, numbered by the turn.
func publish(t *testing.T, turn *stream.Turn, doc map[string]any) {
	t.Helper()
	doc["turn_id"], doc["seq"] = turn.ID(), turn.NextSeq()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var ev conversation.TurnEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("a turn event does not decode: %v\n%s", err, raw)
	}
	if err := turn.Publish(ev); err != nil {
		t.Fatalf("the hub refused an event: %v\n%s", err, raw)
	}
}

// hubRun records a stream through the real hub, validator and mapper: the TurnEvents are a present_result directive whose
// facts carry the numbers of the block, the block, a response text and the done event.
func hubRun(t *testing.T, run string, facts []map[string]any, block map[string]any, text string) *recorded {
	t.Helper()
	r := newRig(t)
	scope := identitytest.ExpectedScope()
	scope.ApplicationID, scope.CapabilityScopes = rigApp, rigCaps
	const turnID = "trn_01J0000000000000000000H001"
	turn, err := r.hub.Begin(scope, turnID)
	if err != nil {
		t.Fatal(err)
	}
	dir := readExample(t, "turn-event/valid/directive.json")
	d := dir["directive"].(map[string]any)
	d["turn_id"], d["act"], d["facts"] = turnID, "present_result", facts
	d["missing_fields"], d["permitted_next_actions"] = []any{}, []any{}
	delete(d, "next_question")
	publish(t, turn, dir)
	publish(t, turn, map[string]any{"type": "block", "block": block})
	if err := turn.PublishResponse(text); err != nil {
		t.Fatal(err)
	}
	publish(t, turn, readExample(t, "turn-event/valid/done-completed.json"))
	rc := agui.RunContext{ThreadID: rigSession, RunID: run, Kind: agui.RunKindTurn, Language: "en", StateMode: agui.StateSnapshotMode}
	if err := r.runs.Record(scope, rc, turnID, r.hub.Config().Retention); err != nil {
		t.Fatal(err)
	}
	path := "/v1/agui/threads/" + rigSession + "/runs/" + run + "/events"
	rec := r.call(http.MethodGet, path, "", nil)
	return &recorded{source: "hub", method: http.MethodGet, path: path, status: rec.Code, contentType: rec.Header().Get("Content-Type"), sse: rec.Body.String()}
}

func integerFact(field string, v int) map[string]any {
	return map[string]any{"field": field, "value": map[string]any{"kind": "integer", "value": v}}
}

// ---- the scenarios ----

var scenarios = []scenario{
	{
		name:        "plain_answer",
		description: "A read of an item the host holds: the verified answer as one assistant message, the directive, the closing state and the follow-up suggestion the host's next action gives. The client sent an empty object as its state, so the run only adds /alixai.",
		expect:      []string{"RUN_STARTED", "STATE_DELTA", "STEP_STARTED", "STEP_FINISHED", "CUSTOM:alixai.directive", "TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_END", "CUSTOM:alixai.suggestions", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.seedItem()
			r.onProposal(capRead, "task", map[string]any{"item_id": "item-1"})
			r.onResponse("The item is named gadget.")
			return r.record("engine", runInput("run_plain", nil, userMessage("m1", "show item item-1")))
		},
	},
	{
		name:        "clarification",
		description: "A request with a missing number: the core asks for it as plain text and the run ends successfully, awaiting the answer. The pending question is in the shared state.",
		expect:      []string{"RUN_STARTED", "CUSTOM:alixai.directive", "TEXT_MESSAGE_CONTENT", "STATE_DELTA", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.onProposal(capCreate, "task", map[string]any{"name": "widget", "size": "small"})
			r.onResponse("How many?")
			return r.record("engine", runInput("run_clarify", nil, userMessage("m1", "create an item named widget, small")))
		},
	},
	{
		name:        "forms",
		description: "A request with a missing choice: the question and its choices are a clarification_choices block (the form a client renders as buttons, with optional free text), shown as a generative-UI activity beside the text.",
		expect:      []string{"ACTIVITY_SNAPSHOT:alixai.block.clarification_choices", "TEXT_MESSAGE_CONTENT", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.onProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3})
			r.onResponse("Which size?")
			return r.record("engine", runInput("run_form", nil, userMessage("m1", "create an item named widget with quantity 3")))
		},
	},
	{
		name:        "choice_answer",
		description: "The answer to the form above: the client sends the id of the chosen choice in forwardedProps.alixai.choiceId (structured input, no model reads it) and the run ends in the confirmation interrupt.",
		expect:      []string{"TOOL_CALL_START:alixai_confirm_action", "TOOL_CALL_ARGS", "TOOL_CALL_END", "RUN_FINISHED:interrupt"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.onProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3})
			r.onResponse("Which size?")
			if rec := r.post(runInput("run_form", nil, userMessage("m1", "create an item named widget with quantity 3"))); rec.Code != http.StatusOK {
				t.Fatalf("the form run = %d: %s", rec.Code, rec.Body.String())
			}
			r.model.Reset()
			r.onResponse("Please confirm: create an item named widget.")
			return r.record("engine", runInput("run_choice", map[string]any{"forwardedProps": map[string]any{"alixai": map[string]any{"choiceId": "small"}}},
				userMessage("m1", "create an item named widget with quantity 3")))
		},
	},
	{
		name:        "read_with_citations",
		description: "A question answered from the application's knowledge sources: a retrieval step, the citations as one activity (the first) and activity deltas (each further one), then the verified text built from the claims. The citations are opaque, scope-checked references.",
		expect:      []string{"STEP_STARTED", "ACTIVITY_SNAPSHOT:alixai.citations", "ACTIVITY_DELTA:alixai.citations", "TEXT_MESSAGE_CONTENT", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.eng.SetKnowledge(&knowledge{res: rag.AnswerResult{Value: warrantyAnswer()}})
			return r.record("engine", runInput("run_cite", nil, userMessage("m1", warrantyQuestion)))
		},
	},
	{
		name:        "prepare_interrupt",
		description: "A consequential request: the action is prepared, its exact effects are shown as the arguments of the confirmation tool call, and the run ends with an interrupt outcome. The confirmation secret is the X-AlixAI-Confirmation-Token response header (a placeholder here); it is in no event.",
		expect:      []string{"TOOL_CALL_START:alixai_confirm_action", "TOOL_CALL_ARGS", "TOOL_CALL_END", "TEXT_MESSAGE_CONTENT", "RUN_FINISHED:interrupt"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.onProposal(capCreate, "task", map[string]any{"name": "widget", "quantity": 3, "size": "small"})
			r.onResponse("Please confirm: create an item named widget.")
			return r.record("engine", runInput("run_prepare", nil, userMessage("m1", "create an item named widget with quantity 3")))
		},
	},
	{
		name:        "resume_approve",
		description: "The user approves: resume with {approved: true} and the confirmation secret in the request header. The action commits, its receipt (the application's facts) is the result of the confirmation tool call, and the run finishes.",
		expect:      []string{"TOOL_CALL_RESULT", "TEXT_MESSAGE_CONTENT", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			secret, prep := r.prepare("run_prepare")
			r.model.Reset()
			r.onResponse("Done. The item widget was created.")
			rec := r.record("engine", runInput("run_approve", resumeInput(prep, true)), secretHeader, secret)
			if r.host.Effects() != 1 {
				t.Fatalf("the approved action had %d effects, want 1", r.host.Effects())
			}
			return rec
		},
	},
	{
		name:        "resume_reject",
		description: "The user declines: resume with {approved: false}, no secret needed. Nothing is sent to the application, the confirmation tool call is resolved as rejected and the run finishes with the cancelled outcome (the core acknowledged a cancellation).",
		expect:      []string{"TOOL_CALL_RESULT", "RUN_FINISHED:cancelled"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			_, prep := r.prepare("run_prepare")
			r.model.Reset()
			r.onResponse("Okay, I will not create it.")
			rec := r.record("engine", runInput("run_reject", resumeInput(prep, false)))
			if r.host.Effects() != 0 {
				t.Fatalf("a declined action had %d effects", r.host.Effects())
			}
			return rec
		},
	},
	{
		name:        "stale",
		description: "The data the action was prepared against changed before the user approved. The application refuses the commit as stale, nothing is created, the run ends with RUN_ERROR (never a success-looking finish) and the confirmation call is resolved as failed.",
		expect:      []string{"TOOL_CALL_RESULT", "RUN_ERROR"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			secret, prep := r.prepare("run_prepare")
			r.host.Seed(rigTenant, rigWsp, "item-9", "someone else's item")
			r.model.Reset()
			r.onResponse("The data changed, so the item was not created.")
			rec := r.record("engine", runInput("run_stale", resumeInput(prep, true)), secretHeader, secret)
			if r.host.Effects() != 0 {
				t.Fatalf("a stale action had %d effects", r.host.Effects())
			}
			return rec
		},
	},
	{
		name:        "error",
		description: "A request nothing in the application handles: the core refuses it with a typed code, the text that explains the refusal is shown, and the run ends with RUN_ERROR carrying that code.",
		expect:      []string{"TEXT_MESSAGE_CONTENT", "RUN_ERROR"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.onProposal("", "chat", nil)
			r.onResponse("I can only help with items.")
			return r.record("engine", runInput("run_error", nil, userMessage("m1", "what is the weather like in Rome tomorrow")))
		},
	},
	{
		name:        "state_snapshot",
		description: "The client sent no state (null): the run sends the shared state as a STATE_SNAPSHOT of {alixai: ...} first, and a STATE_DELTA with the change at the end (the pending question).",
		expect:      []string{"STATE_SNAPSHOT", "STATE_DELTA", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.onProposal(capCreate, "task", map[string]any{"name": "widget", "size": "small"})
			r.onResponse("How many?")
			return r.record("engine", runInput("run_snapshot", map[string]any{"state": nil}, userMessage("m1", "create an item named widget, small")))
		},
	},
	{
		name:        "state_delta",
		description: "The client sent its own state object: the run only adds /alixai with a STATE_DELTA, so the members the client keeps for itself are never replaced, and a second STATE_DELTA carries the change at the end.",
		expect:      []string{"STATE_DELTA", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.onProposal(capCreate, "task", map[string]any{"name": "widget", "size": "small"})
			r.onResponse("How many?")
			state := map[string]any{"page": "inbox", "draft": map[string]any{"title": "x"}}
			return r.record("engine", runInput("run_delta", map[string]any{"state": state}, userMessage("m1", "create an item named widget, small")))
		},
	},
	{
		name:        "suggestions",
		description: "A request for help: the follow-up suggestions (CUSTOM alixai.suggestions) are computed from the capabilities the policy exposes to the caller, with no extra model call.",
		expect:      []string{"CUSTOM:alixai.suggestions", "TEXT_MESSAGE_CONTENT", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.onResponse("Here is what I can do.")
			return r.record("engine", runInput("run_help", nil, userMessage("m1", "what can you do")))
		},
	},
	{
		name:        "attachments_preview",
		description: "A message with a pre-uploaded image: the file is a file reference in the user message (never inline bytes), resolved under the caller's scope, and its size, type and previewable flag are an activity at the start of the run.",
		expect:      []string{"ACTIVITY_SNAPSHOT:alixai.attachments", "TEXT_MESSAGE_CONTENT", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			r := newRig(t)
			r.seedItem()
			fileID := r.upload("photo.png", "image/png", rigPNG())
			r.onProposal(capRead, "task", map[string]any{"item_id": "item-1"})
			r.onResponse("The item is named gadget.")
			msg := map[string]any{"id": "m1", "role": "user", "content": []any{
				map[string]any{"type": "text", "text": "show item item-1"},
				map[string]any{"type": "image", "source": map[string]any{"type": "file", "value": fileID}},
			}}
			return r.record("engine", runInput("run_attach", nil, msg))
		},
	},
	{
		name:        "chart",
		description: "A declarative chart block (data, not executable code; its numbers are in the directive's facts) as a generative-UI activity of type alixai.block.chart, beside the text. Recorded through the hub and read back from the run's events route.",
		expect:      []string{"ACTIVITY_SNAPSHOT:alixai.block.chart", "TEXT_MESSAGE_CONTENT", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			facts := []map[string]any{integerFact("booked_monday", 12), integerFact("booked_tuesday", 18), integerFact("booked_wednesday", 9)}
			return hubRun(t, "run_chart", facts, readExample(t, "rich-block/valid/chart-bar.json"), "Tuesday is the busiest day.")
		},
	},
	{
		name:        "table",
		description: "A table block (columns and rows as data) as a generative-UI activity of type alixai.block.table, beside the text. Recorded through the hub and read back from the run's events route.",
		expect:      []string{"ACTIVITY_SNAPSHOT:alixai.block.table", "TEXT_MESSAGE_CONTENT", "RUN_FINISHED:success"},
		run: func(t *testing.T) *recorded {
			facts := []map[string]any{integerFact("free_slots", 3)}
			return hubRun(t, "run_table", facts, readExample(t, "rich-block/valid/table.json"), "These are the free slots.")
		},
	},
}
