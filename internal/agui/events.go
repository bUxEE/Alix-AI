package agui

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the AG-UI protocol version of the pinned release (PROTOCOL_VERSION of @ag-ui/core, recorded in
// schemas/agui/VERSION and checked against it by a test). PinnedPackage is the release the schemas were vendored from.
const (
	ProtocolVersion = "1.0"
	PinnedPackage   = "@ag-ui/core 1.0.1"
)

// EventType is the "type" member of an AG-UI event.
type EventType string

// The event types of the pinned release. Only the ones in Emitted are ever sent by this package; the rest are
// accepted when read (ParseEvent) and are never produced (docs/integration/agui.md, "What is not emitted").
const (
	RunStarted              EventType = "RUN_STARTED"
	RunFinished             EventType = "RUN_FINISHED"
	RunError                EventType = "RUN_ERROR"
	StepStarted             EventType = "STEP_STARTED"
	StepFinished            EventType = "STEP_FINISHED"
	TextMessageStart        EventType = "TEXT_MESSAGE_START"
	TextMessageContent      EventType = "TEXT_MESSAGE_CONTENT"
	TextMessageEnd          EventType = "TEXT_MESSAGE_END"
	TextMessageChunk        EventType = "TEXT_MESSAGE_CHUNK"
	ToolCallStart           EventType = "TOOL_CALL_START"
	ToolCallArgs            EventType = "TOOL_CALL_ARGS"
	ToolCallEnd             EventType = "TOOL_CALL_END"
	ToolCallResult          EventType = "TOOL_CALL_RESULT"
	ToolCallChunk           EventType = "TOOL_CALL_CHUNK"
	StateSnapshot           EventType = "STATE_SNAPSHOT"
	StateDelta              EventType = "STATE_DELTA"
	MessagesSnapshot        EventType = "MESSAGES_SNAPSHOT"
	ActivitySnapshot        EventType = "ACTIVITY_SNAPSHOT"
	ActivityDelta           EventType = "ACTIVITY_DELTA"
	Raw                     EventType = "RAW"
	Custom                  EventType = "CUSTOM"
	ReasoningStart          EventType = "REASONING_START"
	ReasoningMessageStart   EventType = "REASONING_MESSAGE_START"
	ReasoningMessageContent EventType = "REASONING_MESSAGE_CONTENT"
	ReasoningMessageEnd     EventType = "REASONING_MESSAGE_END"
	ReasoningMessageChunk   EventType = "REASONING_MESSAGE_CHUNK"
	ReasoningEnd            EventType = "REASONING_END"
	ReasoningEncryptedValue EventType = "REASONING_ENCRYPTED_VALUE"
	SubagentStarted         EventType = "SUBAGENT_STARTED"
	SubagentFinished        EventType = "SUBAGENT_FINISHED"
	SubagentError           EventType = "SUBAGENT_ERROR"
)

// knownTypes are the event types of the pinned release.
var knownTypes = map[EventType]bool{
	RunStarted: true, RunFinished: true, RunError: true, StepStarted: true, StepFinished: true,
	TextMessageStart: true, TextMessageContent: true, TextMessageEnd: true, TextMessageChunk: true,
	ToolCallStart: true, ToolCallArgs: true, ToolCallEnd: true, ToolCallResult: true, ToolCallChunk: true,
	StateSnapshot: true, StateDelta: true, MessagesSnapshot: true, ActivitySnapshot: true, ActivityDelta: true,
	Raw: true, Custom: true,
	ReasoningStart: true, ReasoningMessageStart: true, ReasoningMessageContent: true, ReasoningMessageEnd: true,
	ReasoningMessageChunk: true, ReasoningEnd: true, ReasoningEncryptedValue: true,
	SubagentStarted: true, SubagentFinished: true, SubagentError: true,
}

// emitted is the whole set of event types this package produces. Chunk forms (the compact forms of a message or a
// tool call, which a client expands), Raw, the reasoning events and the sub-agent events are not in it, by design.
var emitted = map[EventType]bool{
	RunStarted: true, RunFinished: true, RunError: true, StepStarted: true, StepFinished: true,
	TextMessageStart: true, TextMessageContent: true, TextMessageEnd: true,
	ToolCallStart: true, ToolCallArgs: true, ToolCallEnd: true, ToolCallResult: true,
	StateSnapshot: true, StateDelta: true, MessagesSnapshot: true, ActivitySnapshot: true, ActivityDelta: true,
	Custom: true,
}

// EmittedTypes returns the event types this package produces, in protocol order.
func EmittedTypes() []EventType {
	order := []EventType{
		RunStarted, RunFinished, RunError, StepStarted, StepFinished,
		TextMessageStart, TextMessageContent, TextMessageEnd,
		ToolCallStart, ToolCallArgs, ToolCallEnd, ToolCallResult,
		StateSnapshot, StateDelta, MessagesSnapshot, ActivitySnapshot, ActivityDelta, Custom,
	}
	return append([]EventType(nil), order...)
}

// NotEmittedTypes returns the event types of the pinned release this package never produces.
func NotEmittedTypes() []EventType {
	var out []EventType
	for _, t := range allTypesInOrder {
		if !emitted[t] {
			out = append(out, t)
		}
	}
	return out
}

var allTypesInOrder = []EventType{
	RunStarted, RunFinished, RunError, StepStarted, StepFinished, TextMessageStart, TextMessageContent, TextMessageEnd,
	TextMessageChunk, ToolCallStart, ToolCallArgs, ToolCallEnd, ToolCallResult, ToolCallChunk, StateSnapshot,
	StateDelta, MessagesSnapshot, ActivitySnapshot, ActivityDelta, Raw, Custom, ReasoningStart, ReasoningMessageStart,
	ReasoningMessageContent, ReasoningMessageEnd, ReasoningMessageChunk, ReasoningEnd, ReasoningEncryptedValue,
	SubagentStarted, SubagentFinished, SubagentError,
}

// Known reports whether t is an event type of the pinned release.
func Known(t EventType) bool { return knownTypes[t] }

// Emittable reports whether this package may produce an event of type t.
func Emittable(t EventType) bool { return emitted[t] }

// Event is one AG-UI event this package produces. Every concrete event below is one; a type that is not one of them
// cannot be sent, because the only way out is Encode, which refuses anything that is not Emittable.
type Event interface {
	Kind() EventType
}

type base struct {
	Type EventType `json:"type"`
}

// Kind returns the event's type.
func (b base) Kind() EventType { return b.Type }

// RunStartedEvent opens a run. It never echoes the run's input: client state and forwarded properties are not reflected.
type RunStartedEvent struct {
	base
	ThreadID string `json:"threadId"`
	RunID    string `json:"runId"`
}

// Outcome is the outcome of a finished run: success, interrupt (with its interrupts) or cancelled.
type Outcome struct {
	Type       string      `json:"type"`
	Interrupts []Interrupt `json:"interrupts,omitempty"`
}

// Interrupt is one reason a run paused for the user. Its ID is the preparation id of the action awaiting confirmation.
type Interrupt struct {
	ID             string `json:"id"`
	Reason         string `json:"reason"`
	Message        string `json:"message,omitempty"`
	ToolCallID     string `json:"toolCallId,omitempty"`
	ResponseSchema any    `json:"responseSchema,omitempty"`
	ExpiresAt      string `json:"expiresAt,omitempty"`
	Metadata       any    `json:"metadata,omitempty"`
}

// RunFinishedEvent ends a run that did not fail.
type RunFinishedEvent struct {
	base
	ThreadID string   `json:"threadId"`
	RunID    string   `json:"runId"`
	Result   any      `json:"result,omitempty"`
	Outcome  *Outcome `json:"outcome,omitempty"`
}

// RunErrorEvent ends a run that failed. Nothing follows it.
type RunErrorEvent struct {
	base
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// StepStartedEvent and StepFinishedEvent bracket a pipeline stage.
type StepStartedEvent struct {
	base
	StepName string `json:"stepName"`
}

// StepFinishedEvent closes a step.
type StepFinishedEvent struct {
	base
	StepName string `json:"stepName"`
}

// TextMessageStartEvent opens an assistant message.
type TextMessageStartEvent struct {
	base
	MessageID string `json:"messageId"`
	Role      string `json:"role"`
}

// TextMessageContentEvent carries one fragment of a message. Delta is never empty.
type TextMessageContentEvent struct {
	base
	MessageID string `json:"messageId"`
	Delta     string `json:"delta"`
}

// TextMessageEndEvent closes a message.
type TextMessageEndEvent struct {
	base
	MessageID string `json:"messageId"`
}

// ToolCallStartEvent opens a call of a UI tool (the confirmation card, a hand-off to the application).
type ToolCallStartEvent struct {
	base
	ToolCallID      string `json:"toolCallId"`
	ToolCallName    string `json:"toolCallName"`
	ParentMessageID string `json:"parentMessageId,omitempty"`
}

// ToolCallArgsEvent carries the arguments of a call as a JSON text.
type ToolCallArgsEvent struct {
	base
	ToolCallID string `json:"toolCallId"`
	Delta      string `json:"delta"`
}

// ToolCallEndEvent closes a call.
type ToolCallEndEvent struct {
	base
	ToolCallID string `json:"toolCallId"`
}

// ToolCallResultEvent is the result of a call: the application's deterministic facts as a JSON text.
type ToolCallResultEvent struct {
	base
	MessageID  string `json:"messageId"`
	ToolCallID string `json:"toolCallId"`
	Content    string `json:"content"`
	Role       string `json:"role,omitempty"`
}

// StateSnapshotEvent replaces the shared state.
type StateSnapshotEvent struct {
	base
	Snapshot any `json:"snapshot"`
}

// PatchOp is one RFC 6902 operation (add, remove, replace, move, copy, test).
type PatchOp struct {
	Op    string
	Path  string
	From  string
	Value any
}

// MarshalJSON writes the members the operation has: value for add, replace and test (even when it is null), from for
// move and copy.
func (p PatchOp) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	op, _ := json.Marshal(p.Op)
	path, _ := json.Marshal(p.Path)
	fmt.Fprintf(&buf, `{"op":%s`, op)
	switch p.Op {
	case "add", "replace", "test":
		val, err := json.Marshal(p.Value)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&buf, `,"path":%s,"value":%s}`, path, val)
	case "remove":
		fmt.Fprintf(&buf, `,"path":%s}`, path)
	case "move", "copy":
		from, _ := json.Marshal(p.From)
		fmt.Fprintf(&buf, `,"from":%s,"path":%s}`, from, path)
	default:
		return nil, fmt.Errorf("agui: %q is not a JSON Patch operation", p.Op)
	}
	return buf.Bytes(), nil
}

// StateDeltaEvent changes the shared state by a JSON Patch.
type StateDeltaEvent struct {
	base
	Delta []PatchOp `json:"delta"`
}

// Message is one message of a MessagesSnapshot: the user's and the assistant's text of the thread.
type Message struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

// MessagesSnapshotEvent replaces the client's messages with the thread's history.
type MessagesSnapshotEvent struct {
	base
	Messages []Message `json:"messages"`
}

// ActivitySnapshotEvent creates or replaces an activity message: structured, renderable content that is not text.
type ActivitySnapshotEvent struct {
	base
	MessageID    string `json:"messageId"`
	ActivityType string `json:"activityType"`
	Content      any    `json:"content"`
	Replace      *bool  `json:"replace,omitempty"`
}

// ActivityDeltaEvent changes an activity message by a JSON Patch.
type ActivityDeltaEvent struct {
	base
	MessageID    string    `json:"messageId"`
	ActivityType string    `json:"activityType"`
	Patch        []PatchOp `json:"patch"`
}

// CustomEvent carries an application-defined event: the dialogue directive and the suggestions.
type CustomEvent struct {
	base
	Name  string `json:"name"`
	Value any    `json:"value"`
}

// Constructors set the type, so an event can never be built with the wrong one.

// NewRunStarted returns a RUN_STARTED event.
func NewRunStarted(threadID, runID string) RunStartedEvent {
	return RunStartedEvent{base: base{RunStarted}, ThreadID: threadID, RunID: runID}
}

// NewRunFinished returns a RUN_FINISHED event.
func NewRunFinished(threadID, runID string, result any, outcome *Outcome) RunFinishedEvent {
	return RunFinishedEvent{base: base{RunFinished}, ThreadID: threadID, RunID: runID, Result: result, Outcome: outcome}
}

// NewRunError returns a RUN_ERROR event.
func NewRunError(message, code string) RunErrorEvent {
	return RunErrorEvent{base: base{RunError}, Message: message, Code: code}
}

// NewStepStarted returns a STEP_STARTED event.
func NewStepStarted(name string) StepStartedEvent {
	return StepStartedEvent{base: base{StepStarted}, StepName: name}
}

// NewStepFinished returns a STEP_FINISHED event.
func NewStepFinished(name string) StepFinishedEvent {
	return StepFinishedEvent{base: base{StepFinished}, StepName: name}
}

// NewTextMessageStart returns a TEXT_MESSAGE_START event for an assistant message.
func NewTextMessageStart(id string) TextMessageStartEvent {
	return TextMessageStartEvent{base: base{TextMessageStart}, MessageID: id, Role: RoleAssistant}
}

// NewTextMessageContent returns a TEXT_MESSAGE_CONTENT event.
func NewTextMessageContent(id, delta string) TextMessageContentEvent {
	return TextMessageContentEvent{base: base{TextMessageContent}, MessageID: id, Delta: delta}
}

// NewTextMessageEnd returns a TEXT_MESSAGE_END event.
func NewTextMessageEnd(id string) TextMessageEndEvent {
	return TextMessageEndEvent{base: base{TextMessageEnd}, MessageID: id}
}

// NewToolCallStart returns a TOOL_CALL_START event.
func NewToolCallStart(id, name string) ToolCallStartEvent {
	return ToolCallStartEvent{base: base{ToolCallStart}, ToolCallID: id, ToolCallName: name}
}

// NewToolCallArgs returns a TOOL_CALL_ARGS event.
func NewToolCallArgs(id, delta string) ToolCallArgsEvent {
	return ToolCallArgsEvent{base: base{ToolCallArgs}, ToolCallID: id, Delta: delta}
}

// NewToolCallEnd returns a TOOL_CALL_END event.
func NewToolCallEnd(id string) ToolCallEndEvent {
	return ToolCallEndEvent{base: base{ToolCallEnd}, ToolCallID: id}
}

// NewToolCallResult returns a TOOL_CALL_RESULT event.
func NewToolCallResult(messageID, toolCallID, content string) ToolCallResultEvent {
	return ToolCallResultEvent{base: base{ToolCallResult}, MessageID: messageID, ToolCallID: toolCallID, Content: content, Role: RoleTool}
}

// NewStateSnapshot returns a STATE_SNAPSHOT event.
func NewStateSnapshot(snapshot any) StateSnapshotEvent {
	return StateSnapshotEvent{base: base{StateSnapshot}, Snapshot: snapshot}
}

// NewStateDelta returns a STATE_DELTA event.
func NewStateDelta(ops []PatchOp) StateDeltaEvent {
	return StateDeltaEvent{base: base{StateDelta}, Delta: ops}
}

// NewMessagesSnapshot returns a MESSAGES_SNAPSHOT event.
func NewMessagesSnapshot(msgs []Message) MessagesSnapshotEvent {
	if msgs == nil {
		msgs = []Message{}
	}
	return MessagesSnapshotEvent{base: base{MessagesSnapshot}, Messages: msgs}
}

// NewActivitySnapshot returns an ACTIVITY_SNAPSHOT event.
func NewActivitySnapshot(messageID, activityType string, content any) ActivitySnapshotEvent {
	return ActivitySnapshotEvent{base: base{ActivitySnapshot}, MessageID: messageID, ActivityType: activityType, Content: content}
}

// NewActivityDelta returns an ACTIVITY_DELTA event.
func NewActivityDelta(messageID, activityType string, patch []PatchOp) ActivityDeltaEvent {
	return ActivityDeltaEvent{base: base{ActivityDelta}, MessageID: messageID, ActivityType: activityType, Patch: patch}
}

// NewCustom returns a CUSTOM event.
func NewCustom(name string, value any) CustomEvent {
	return CustomEvent{base: base{Custom}, Name: name, Value: value}
}

// Roles of the pinned release's messages. Only the first three appear in output.
const (
	RoleAssistant = "assistant"
	RoleUser      = "user"
	RoleTool      = "tool"
	RoleSystem    = "system"
	RoleDeveloper = "developer"
)
