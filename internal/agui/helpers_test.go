package agui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

const (
	turnExamples  = "../../schemas/conversation/examples/turn-event/valid"
	blockExamples = "../../schemas/conversation/examples/rich-block/valid"

	// testTurn, testPrep and testSession are the ids of the published examples.
	testTurn    = "trn_01J0000000000000000000000A"
	testPrep    = "prep_01J0000000000000000000000A"
	testSession = "ses1"
	testThread  = "ses1"
	testRun     = "run_1"
)

var (
	schemasOnce sync.Once
	schemasVal  *Schemas
	schemasErr  error
)

// vendored loads the vendored AG-UI schemas once.
func vendored(t testing.TB) *Schemas {
	t.Helper()
	schemasOnce.Do(func() {
		dir, err := FindSchemaDir()
		if err != nil {
			schemasErr = err
			return
		}
		schemasVal, schemasErr = LoadSchemas(dir)
	})
	if schemasErr != nil {
		t.Fatalf("the vendored AG-UI schemas do not load: %v", schemasErr)
	}
	return schemasVal
}

func readExample(t testing.TB, dir, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func decodeTurnEvent(t testing.TB, doc map[string]any) conversation.TurnEvent {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var ev conversation.TurnEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("a turn event does not decode: %v\n%s", err, raw)
	}
	return ev
}

// tev builds a TurnEvent from a published example of the turn-event schema, at a seq, optionally changed.
func tev(t testing.TB, name string, seq int64, mutate ...func(map[string]any)) conversation.TurnEvent {
	t.Helper()
	doc := readExample(t, turnExamples, name+".json")
	doc["turn_id"], doc["seq"] = testTurn, seq
	for _, m := range mutate {
		m(doc)
	}
	return decodeTurnEvent(t, doc)
}

// bev wraps a published rich-block example as a block event.
func bev(t testing.TB, example string, seq int64) conversation.TurnEvent {
	t.Helper()
	return decodeTurnEvent(t, map[string]any{
		"type": "block", "turn_id": testTurn, "seq": seq, "block": readExample(t, blockExamples, example+".json"),
	})
}

func token(t testing.TB, seq int64, text string) conversation.TurnEvent {
	return tev(t, "token", seq, func(d map[string]any) { d["text"] = text })
}

func done(t testing.TB, seq int64, reason string, prep string) conversation.TurnEvent {
	return tev(t, "done-completed", seq, func(d map[string]any) {
		d["finish_reason"] = reason
		if prep != "" {
			d["preparation_id"] = prep
		}
	})
}

func progress(t testing.TB, seq int64, stage, state string, extra map[string]any) conversation.TurnEvent {
	return tev(t, "progress-queued", seq, func(d map[string]any) {
		delete(d, "percent")
		delete(d, "job_id")
		d["stage"], d["state"] = stage, state
		for k, v := range extra {
			d[k] = v
		}
	})
}

// seqed numbers a list of events 0..n-1, so a scenario is written as a list.
func seqed(t testing.TB, build ...func(seq int64) conversation.TurnEvent) []conversation.TurnEvent {
	t.Helper()
	out := make([]conversation.TurnEvent, len(build))
	for i, b := range build {
		out[i] = b(int64(i))
	}
	return out
}

// scenario names the TurnEvents of one turn the way the gateway publishes them.
type scenario struct {
	name   string
	events []conversation.TurnEvent
	rc     RunContext
}

func newRC() RunContext {
	return RunContext{ThreadID: testThread, RunID: testRun, Kind: RunKindTurn, Language: "en"}
}

// runAll maps every event of a turn and returns what was emitted.
func runAll(t testing.TB, rc RunContext, events []conversation.TurnEvent) []Emitted {
	t.Helper()
	m, err := NewMapper(rc)
	if err != nil {
		t.Fatal(err)
	}
	var out []Emitted
	for _, ev := range events {
		got, err := m.Map(ev)
		if err != nil {
			t.Fatalf("Map: %v", err)
		}
		out = append(out, got...)
	}
	return out
}

func types(em []Emitted) []EventType {
	out := make([]EventType, len(em))
	for i, e := range em {
		out[i] = e.Event.Kind()
	}
	return out
}

// verifyLifecycle checks the ordering rules of the AG-UI client's event verification (the pinned @ag-ui/client's
// verifyEvents): RUN_STARTED first; nothing after RUN_FINISHED or RUN_ERROR; steps, text messages and tool calls are
// opened before use, closed before the run finishes and never opened twice; a step is finished by name.
func verifyLifecycle(events []Event) error {
	var (
		started, ended bool
		steps          = map[string]bool{}
		texts          = map[string]bool{}
		calls          = map[string]bool{}
		activities     = map[string]bool{}
	)
	for i, ev := range events {
		k := ev.Kind()
		if ended {
			return fmt.Errorf("event %d (%s) follows the end of the run", i, k)
		}
		if !started && k != RunStarted && k != RunError {
			return fmt.Errorf("event %d (%s): the first event must be RUN_STARTED", i, k)
		}
		switch e := ev.(type) {
		case RunStartedEvent:
			if started {
				return fmt.Errorf("event %d: RUN_STARTED twice", i)
			}
			started = true
		case RunFinishedEvent:
			if len(steps)+len(texts)+len(calls) > 0 {
				return fmt.Errorf("event %d: RUN_FINISHED with steps %v, texts %v or calls %v still open", i, steps, texts, calls)
			}
			ended = true
		case RunErrorEvent:
			ended = true
		case StepStartedEvent:
			if steps[e.StepName] {
				return fmt.Errorf("event %d: step %q is already active", i, e.StepName)
			}
			steps[e.StepName] = true
		case StepFinishedEvent:
			if !steps[e.StepName] {
				return fmt.Errorf("event %d: step %q was not started", i, e.StepName)
			}
			delete(steps, e.StepName)
		case TextMessageStartEvent:
			if texts[e.MessageID] {
				return fmt.Errorf("event %d: message %q is already in progress", i, e.MessageID)
			}
			texts[e.MessageID] = true
		case TextMessageContentEvent:
			if !texts[e.MessageID] {
				return fmt.Errorf("event %d: content for message %q that was not started", i, e.MessageID)
			}
			if e.Delta == "" {
				return fmt.Errorf("event %d: empty delta", i)
			}
		case TextMessageEndEvent:
			if !texts[e.MessageID] {
				return fmt.Errorf("event %d: end of message %q that was not started", i, e.MessageID)
			}
			delete(texts, e.MessageID)
		case ToolCallStartEvent:
			if calls[e.ToolCallID] {
				return fmt.Errorf("event %d: tool call %q is already in progress", i, e.ToolCallID)
			}
			calls[e.ToolCallID] = true
		case ToolCallArgsEvent:
			if !calls[e.ToolCallID] {
				return fmt.Errorf("event %d: args for tool call %q that was not started", i, e.ToolCallID)
			}
		case ToolCallEndEvent:
			if !calls[e.ToolCallID] {
				return fmt.Errorf("event %d: end of tool call %q that was not started", i, e.ToolCallID)
			}
			delete(calls, e.ToolCallID)
		case ActivitySnapshotEvent:
			activities[e.MessageID] = true
		case ActivityDeltaEvent:
			if !activities[e.MessageID] {
				return fmt.Errorf("event %d: delta for activity %q that has no snapshot", i, e.MessageID)
			}
		}
	}
	if !ended {
		return fmt.Errorf("the run never ended")
	}
	return nil
}

func eventsOf(em []Emitted) []Event {
	out := make([]Event, len(em))
	for i, e := range em {
		out[i] = e.Event
	}
	return out
}

// validateAll checks every emitted event against the vendored schemas and the lifecycle rules.
func validateAll(t testing.TB, em []Emitted) {
	t.Helper()
	s := vendored(t)
	for i, e := range em {
		if err := s.ValidateEvent(e.Data); err != nil {
			t.Fatalf("event %d (%s) does not validate: %v\n%s", i, e.Event.Kind(), err, e.Data)
		}
	}
	if err := verifyLifecycle(eventsOf(em)); err != nil {
		t.Fatalf("lifecycle: %v\n%v", err, types(em))
	}
}

// testCtx is a context carrying the verified scope of the test principal.
func testCtx(t testing.TB, mod ...func(*contracts.Scope)) (context.Context, contracts.Scope) {
	t.Helper()
	sc := contracts.Scope{
		DeploymentID: "dep1", ApplicationID: "app1", TenantID: "ten1", WorkspaceID: "ws1",
		PrincipalID: "usr1", Roles: []string{"user"}, CapabilityScopes: []string{"appointments.book"}, SessionID: testSession,
	}
	for _, m := range mod {
		m(&sc)
	}
	ctx, err := identity.WithScope(context.Background(), sc)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, sc
}

// samplePrepared decodes the prepared action of the published example.
func samplePrepared(t testing.TB) act.PreparedAction {
	t.Helper()
	ev := tev(t, "prepared-action", 0)
	return ev.Value.(conversation.PreparedActionEvent).PreparedAction
}

// fakePreps is a Preparations that knows one preparation of one principal and session, as the coordinator does: any
// other scope gets scope_violation.
type fakePreps struct {
	mu        sync.Mutex
	prep      act.PreparedAction
	principal string
	session   string
	err       error
	calls     int
	// reply, when set, is what a lookup in scope answers whatever id was asked for: a confused coordinator.
	reply *act.PreparedAction
}

func (f *fakePreps) Get(_ context.Context, scope contracts.Scope, id string) (contracts.PreparedAction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return contracts.PreparedAction{}, f.err
	}
	if scope.PrincipalID != f.principal || scope.SessionID != f.session || (f.reply == nil && id != string(f.prep.PreparationID)) {
		return contracts.PreparedAction{}, contracts.New(contracts.ErrScopeViolation, "actions: no such preparation in this scope")
	}
	if f.reply != nil {
		return *f.reply, nil
	}
	return f.prep, nil
}

func newPreps(t testing.TB) *fakePreps {
	return &fakePreps{prep: samplePrepared(t), principal: "usr1", session: testSession}
}

// mustJSON marshals v or fails.
func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rawInput builds a RunAgentInput from a JSON literal.
func rawInput(t testing.TB, doc string) RunAgentInput {
	t.Helper()
	in, err := ParseRunAgentInput([]byte(doc))
	if err != nil {
		t.Fatalf("ParseRunAgentInput: %v\n%s", err, doc)
	}
	return in
}

func codeOf(err error) contracts.ErrCode { return contracts.CodeOf(err) }

// reasonOf returns the "reason" detail of a typed error, "" when there is none.
func reasonOf(err error) string {
	var ce *contracts.Error
	if errors.As(err, &ce) {
		if r, ok := ce.Details["reason"].(string); ok {
			return r
		}
	}
	return ""
}
