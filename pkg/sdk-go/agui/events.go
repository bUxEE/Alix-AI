package agui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ProtocolVersion is the AG-UI protocol version of the pinned release, PinnedPackage the release it was taken from.
const (
	ProtocolVersion = "1.0"
	PinnedPackage   = "@ag-ui/core 1.0.1"
)

// EventType is the "type" member of an AG-UI event.
type EventType string

// The event types of the pinned release. The gateway emits only some of them (docs/integration/agui.md section 11); the
// rest are accepted when read.
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

// AllTypes lists the event types of the pinned release.
func AllTypes() []EventType {
	return []EventType{
		RunStarted, RunFinished, RunError, StepStarted, StepFinished, TextMessageStart, TextMessageContent, TextMessageEnd,
		TextMessageChunk, ToolCallStart, ToolCallArgs, ToolCallEnd, ToolCallResult, ToolCallChunk, StateSnapshot,
		StateDelta, MessagesSnapshot, ActivitySnapshot, ActivityDelta, Raw, Custom, ReasoningStart, ReasoningMessageStart,
		ReasoningMessageContent, ReasoningMessageEnd, ReasoningMessageChunk, ReasoningEnd, ReasoningEncryptedValue,
		SubagentStarted, SubagentFinished, SubagentError,
	}
}

// Known reports whether t is an event type of the pinned release.
func Known(t EventType) bool {
	for _, k := range AllTypes() {
		if k == t {
			return true
		}
	}
	return false
}

// Terminal reports whether t ends a run: nothing may follow it.
func Terminal(t EventType) bool { return t == RunFinished || t == RunError }

// Event is one AG-UI event. The typed events below implement it; UnknownEvent carries any other.
type Event interface {
	Kind() EventType
}

type base struct {
	Type EventType `json:"type"`
}

// Kind returns the event's type.
func (b base) Kind() EventType { return b.Type }

// RunStartedEvent opens a run.
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

// StepStartedEvent opens a pipeline stage.
type StepStartedEvent struct {
	base
	StepName string `json:"stepName"`
}

// StepFinishedEvent closes a pipeline stage.
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

// TextMessageContentEvent carries one fragment of a message.
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

// UnmarshalJSON reads the members of a JSON Patch operation.
func (p *PatchOp) UnmarshalJSON(data []byte) error {
	var raw struct {
		Op    string `json:"op"`
		Path  string `json:"path"`
		From  string `json:"from"`
		Value any    `json:"value"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = PatchOp{Op: raw.Op, Path: raw.Path, From: raw.From, Value: raw.Value}
	return nil
}

// StateDeltaEvent changes the shared state by a JSON Patch.
type StateDeltaEvent struct {
	base
	Delta []PatchOp `json:"delta"`
}

// Message is one message of a MessagesSnapshot.
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

// UnknownEvent is an event of a type this package has no struct for (a chunk form, a reasoning event, a type of a newer
// protocol version). Its bytes are kept as they came.
type UnknownEvent struct {
	Type EventType
	Data json.RawMessage
}

// Kind returns the event's type.
func (u UnknownEvent) Kind() EventType { return u.Type }

// MarshalJSON returns the event as it was read.
func (u UnknownEvent) MarshalJSON() ([]byte, error) { return u.Data, nil }

// ErrNotAnEvent is returned by ParseEvent for data that is not a JSON object with a non-empty string "type".
var ErrNotAnEvent = errors.New("agui: not an AG-UI event")

// ParseEvent reads one AG-UI event. Types with a struct here are decoded into it; every other type, including types of a
// newer protocol version, is returned as an UnknownEvent. Data that is not an event is ErrNotAnEvent.
func ParseEvent(data []byte) (Event, error) {
	typ, err := eventType(data)
	if err != nil {
		return nil, err
	}
	var dst any
	switch typ {
	case RunStarted:
		dst = &RunStartedEvent{}
	case RunFinished:
		dst = &RunFinishedEvent{}
	case RunError:
		dst = &RunErrorEvent{}
	case StepStarted:
		dst = &StepStartedEvent{}
	case StepFinished:
		dst = &StepFinishedEvent{}
	case TextMessageStart:
		dst = &TextMessageStartEvent{}
	case TextMessageContent:
		dst = &TextMessageContentEvent{}
	case TextMessageEnd:
		dst = &TextMessageEndEvent{}
	case ToolCallStart:
		dst = &ToolCallStartEvent{}
	case ToolCallArgs:
		dst = &ToolCallArgsEvent{}
	case ToolCallEnd:
		dst = &ToolCallEndEvent{}
	case ToolCallResult:
		dst = &ToolCallResultEvent{}
	case StateSnapshot:
		dst = &StateSnapshotEvent{}
	case StateDelta:
		dst = &StateDeltaEvent{}
	case MessagesSnapshot:
		dst = &MessagesSnapshotEvent{}
	case ActivitySnapshot:
		dst = &ActivitySnapshotEvent{}
	case ActivityDelta:
		dst = &ActivityDeltaEvent{}
	case Custom:
		dst = &CustomEvent{}
	default:
		return UnknownEvent{Type: typ, Data: append(json.RawMessage(nil), data...)}, nil
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return nil, fmt.Errorf("agui: %s event is malformed: %w", typ, err)
	}
	return derefEvent(dst), nil
}

func eventType(data []byte) (EventType, error) {
	var head struct {
		Type *string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil || head.Type == nil || *head.Type == "" {
		return "", ErrNotAnEvent
	}
	return EventType(*head.Type), nil
}

// derefEvent turns the pointer ParseEvent decoded into back into the value type of the event.
func derefEvent(p any) Event {
	switch v := p.(type) {
	case *RunStartedEvent:
		return *v
	case *RunFinishedEvent:
		return *v
	case *RunErrorEvent:
		return *v
	case *StepStartedEvent:
		return *v
	case *StepFinishedEvent:
		return *v
	case *TextMessageStartEvent:
		return *v
	case *TextMessageContentEvent:
		return *v
	case *TextMessageEndEvent:
		return *v
	case *ToolCallStartEvent:
		return *v
	case *ToolCallArgsEvent:
		return *v
	case *ToolCallEndEvent:
		return *v
	case *ToolCallResultEvent:
		return *v
	case *StateSnapshotEvent:
		return *v
	case *StateDeltaEvent:
		return *v
	case *MessagesSnapshotEvent:
		return *v
	case *ActivitySnapshotEvent:
		return *v
	case *ActivityDeltaEvent:
		return *v
	case *CustomEvent:
		return *v
	}
	return nil
}

// NewRunError returns a RUN_ERROR event.
func NewRunError(message, code string) RunErrorEvent {
	return RunErrorEvent{base: base{RunError}, Message: message, Code: code}
}

// MarshalEvent returns the event as the single line of JSON a `data:` field carries.
func MarshalEvent(e Event) ([]byte, error) {
	if e == nil {
		return nil, ErrNotAnEvent
	}
	return json.Marshal(e)
}
