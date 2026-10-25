package agui

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// Names of the application-defined parts of the stream. Tool names and activity types start with "alixai" so a client
// can tell them from its own; a client tool may not use them (see Intake).
const (
	// ConfirmToolName is the UI tool whose call carries the exact effects of a prepared action. Its call id is
	// ConfirmCallID(preparation id) and the matching interrupt names it in toolCallId.
	ConfirmToolName = "alixai_confirm_action"
	// ContinueToolName hands the user to the application's own authenticated interface (a step-up action).
	ContinueToolName = "alixai_continue_in_app"

	// ActivityProgress, ActivityCitations, ActivityAttachments and ActivityReceipt are activity types; the blocks use
	// ActivityBlockPrefix + the block type (alixai.block.chart, alixai.block.table, alixai.block.clarification_choices, ...).
	ActivityProgress    = "alixai.progress"
	ActivityCitations   = "alixai.citations"
	ActivityAttachments = "alixai.attachments"
	ActivityReceipt     = "alixai.receipt"
	ActivityBlockPrefix = "alixai.block."

	// DirectiveName is the name of the CUSTOM event that carries the dialogue directive.
	DirectiveName = "alixai.directive"

	// ReasonConfirmation is the reason of the interrupt of a prepared action.
	ReasonConfirmation = "confirmation_required"
)

// UICallPrefix starts the id of every call of a UI tool the model asked the client's screen to run (frontend actions,
// generative UI). Like ConfirmCallPrefix it is not "call_" and it is not ConfirmCallPrefix: the result the client sends
// back for such a call is client data and can never be taken for an approval.
const UICallPrefix = "alixai_ui_"

// UICallID is the id of the UI tool call of a turn: one per turn, derived from the turn id, so the same TurnEvents and
// the same RunContext always give the same id.
func UICallID(turnID string) string { return UICallPrefix + turnID }

// ConfirmCallPrefix starts the id of every confirmation tool call. It is not "call_", the prefix many model clients mint
// for their own tool calls, so a result a client sends for one of its own tools can never be taken for an approval.
const ConfirmCallPrefix = "alixai_confirm_"

// ConfirmCallID is the tool call id of the confirmation of a preparation: stable, so the run that resumes it can answer
// the same call.
func ConfirmCallID(preparationID string) string { return ConfirmCallPrefix + preparationID }

// RunKind says what a run does.
type RunKind string

const (
	// RunKindTurn answers a user message.
	RunKindTurn RunKind = "turn"
	// RunKindChoice answers the pending clarification by choosing one of its choices.
	RunKindChoice RunKind = "choice"
	// RunKindResume approves or rejects the prepared action an earlier run ended on.
	RunKindResume RunKind = "resume"
	// RunKindConnect replays the thread: history and state, no turn.
	RunKindConnect RunKind = "connect"
)

// UIActionCall is a validated call of a UI tool for the client's screen (dialogue.UIAction): the tool the client
// declared and the host registered, and arguments that satisfy the declared schema. The mapping emits it as the events
// of one tool call; the client runs it. It is an instruction to the client's UI, never to the application.
type UIActionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ResumeRef is the preparation a resume run answers and the answer.
type ResumeRef struct {
	PreparationID string `json:"preparation_id"`
	Approved      bool   `json:"approved"`
}

// RunContext is everything besides the TurnEvents that a run's events depend on. It is plain data, so the caller keeps
// it with the turn's stream: the mapping is a pure function of (RunContext, TurnEvents), and a reconnecting client is
// served by mapping the buffered TurnEvents again and skipping what it has (see Emitted.Seq), which gives the same
// bytes the live stream had. It holds no secret.
type RunContext struct {
	ThreadID string  `json:"thread_id"`
	RunID    string  `json:"run_id"`
	Kind     RunKind `json:"kind"`
	// Language is the language of the response (a BCP 47 tag), for the suggestions' examples.
	Language string `json:"language,omitempty"`
	// StateMode, StateBefore and StateAfter: the client-safe state when the run began and when it ended. Either may be
	// nil (no state events).
	StateMode   StateMode    `json:"state_mode,omitempty"`
	StateBefore *ClientState `json:"state_before,omitempty"`
	StateAfter  *ClientState `json:"state_after,omitempty"`
	// Attachments are the files the user attached to the message of this run, as the files service resolved them.
	Attachments []conversation.FileRef `json:"attachments,omitempty"`
	// Resume is set for a resume run.
	Resume *ResumeRef `json:"resume,omitempty"`
	// UIAction is the UI tool call the turn asked the client to run (dialogue.Result.UIAction), if any. The mapping
	// re-checks its shape and emits TOOL_CALL_START, TOOL_CALL_ARGS and TOOL_CALL_END before the run finishes, and only
	// when the run finishes as a success: a turn that awaits a confirmation or ends in an error asks the screen for nothing.
	UIAction *UIActionCall `json:"ui_action,omitempty"`
	// Catalog and MaxSuggestions feed the follow-up suggestions (Suggest).
	Catalog        []CapabilityHint `json:"catalog,omitempty"`
	MaxSuggestions int              `json:"max_suggestions,omitempty"`
	// Withhold lists values that must never appear in an event, whatever member they sit in: the caller puts the
	// confirmation secret of the turn here as a last line of defence. It is never serialised.
	Withhold []string `json:"-"`
}

// Emitted is one event ready to send. Seq is the seq of the TurnEvent the event was produced for: it is the SSE id of
// the event and the resume cursor. A client that has everything up to cursor c is sent the events with Seq > c.
type Emitted struct {
	Seq   int64
	Event Event
	// Data is the encoded event (Encode).
	Data []byte
}

// errWithheld is the error of Map when an event of the turn had to be held back (a credential-shaped member, a
// withheld value, an oversized event). The events returned with it close the run cleanly with a RUN_ERROR.
func errWithheld(cause error) error {
	return contracts.Wrap(contracts.ErrInternal, cause, "agui: an event of the turn was withheld and the run was failed")
}

// wire tracks what is open on the wire, so the tail of a failed or aborted run can close it and so no lifecycle rule
// of the protocol is left to the cases of Map.
type wire struct {
	started, finished bool
	text              string
	calls             []string
	steps             []string
}

func (w *wire) observe(ev Event) {
	switch e := ev.(type) {
	case RunStartedEvent:
		w.started = true
	case RunFinishedEvent, RunErrorEvent:
		w.finished = true
	case TextMessageStartEvent:
		w.text = e.MessageID
	case TextMessageEndEvent:
		w.text = ""
	case ToolCallStartEvent:
		w.calls = append(w.calls, e.ToolCallID)
	case ToolCallEndEvent:
		w.calls = removeString(w.calls, e.ToolCallID)
	case StepStartedEvent:
		w.steps = append(w.steps, e.StepName)
	case StepFinishedEvent:
		w.steps = removeString(w.steps, e.StepName)
	}
}

func removeString(list []string, s string) []string {
	for i, v := range list {
		if v == s {
			return append(list[:i:i], list[i+1:]...)
		}
	}
	return list
}

// Mapper turns the TurnEvents of one turn into the AG-UI events of one run. It is not safe for concurrent use. Its input
// is the stream hub's account of the turn (internal/stream): events the block validator has accepted and whose Markdown
// has been sanitised; it neither validates nor sanitises again. The confirmation secret is not a TurnEvent member, so it
// cannot reach an event by construction, and Encode checks the output for credential-shaped members and the values in
// RunContext.Withhold besides.
type Mapper struct {
	rc RunContext
	w  wire

	turnID string
	// counters make every id deterministic: the same TurnEvents always give the same ids.
	texts, blocks, results int

	progress  map[string]*activityState
	citations *activityState
	prepared  *confirmView
	directive *conversation.DialogueDirective
	pendErr   *common.Error
	resultOut bool
}

type activityState struct {
	id   string
	vals map[string]any
}

// NewMapper returns the mapper of one run.
func NewMapper(rc RunContext) (*Mapper, error) {
	if !validID(rc.ThreadID) || !validID(rc.RunID) {
		return nil, contracts.New(contracts.ErrValidationFailed, "agui: the thread id and the run id must be set and well-formed")
	}
	if rc.StateMode == "" {
		rc.StateMode = StateSnapshotMode
	}
	return &Mapper{rc: rc, progress: map[string]*activityState{}}, nil
}

// Finished reports whether the run has ended (RUN_FINISHED or RUN_ERROR was produced).
func (m *Mapper) Finished() bool { return m.w.finished }

// batch collects the events produced for one TurnEvent.
type batch struct {
	m   *Mapper
	seq int64
	out []Emitted
	err error
}

func (b *batch) emit(ev Event) {
	if b.err != nil {
		return
	}
	data, err := Encode(ev, b.m.rc.Withhold...)
	if err != nil {
		b.err = err
		return
	}
	b.m.w.observe(ev)
	b.out = append(b.out, Emitted{Seq: b.seq, Event: ev, Data: data})
}

func (b *batch) closeText() {
	if b.m.w.text != "" {
		b.emit(NewTextMessageEnd(b.m.w.text))
	}
}

func (b *batch) text(delta string) {
	if delta == "" {
		return
	}
	if b.m.w.text == "" {
		b.m.texts++
		b.emit(NewTextMessageStart("msg_" + b.m.turnID + "_" + strconv.Itoa(b.m.texts)))
	}
	if b.m.w.text != "" {
		b.emit(NewTextMessageContent(b.m.w.text, delta))
	}
}

// fail ends a batch that could not be encoded: nothing of the batch after the refused event is sent, what is open on the
// wire is closed, and the run ends with a RUN_ERROR that says nothing about the event.
func (b *batch) fail(cause error) ([]Emitted, error) {
	b.err = nil
	for _, t := range b.m.tail("internal", "the event stream was stopped") {
		b.emit(t)
	}
	return b.out, errWithheld(cause)
}

func (m *Mapper) tail(code, message string) []Event {
	var out []Event
	if m.w.text != "" {
		out = append(out, NewTextMessageEnd(m.w.text))
	}
	for _, c := range m.w.calls {
		out = append(out, NewToolCallEnd(c))
	}
	for i := len(m.w.steps) - 1; i >= 0; i-- {
		out = append(out, NewStepFinished(m.w.steps[i]))
	}
	return append(out, NewRunError(message, code))
}

// Abort ends a run whose turn stream ended without its done event (the producer was abandoned, the stream was cut off
// for good): it closes what is open and sends a RUN_ERROR. A run that has finished is left alone. seq is the cursor
// the events carry (the last TurnEvent seq seen, or 0).
func (m *Mapper) Abort(seq int64, code, message string) []Emitted {
	if m.w.finished {
		return nil
	}
	b := &batch{m: m, seq: seq}
	if !m.w.started {
		b.emit(NewRunStarted(m.rc.ThreadID, m.rc.RunID))
	}
	for _, t := range m.tail(code, message) {
		b.emit(t)
	}
	return b.out
}

func identify(ev conversation.TurnEvent) (turnID string, seq int64, ok bool) {
	switch v := ev.Value.(type) {
	case conversation.TokenEvent:
		return string(v.TurnID), int64(v.Seq), true
	case conversation.DirectiveEvent:
		return string(v.TurnID), int64(v.Seq), true
	case conversation.ClarificationEvent:
		return string(v.TurnID), int64(v.Seq), true
	case conversation.PreparedActionEvent:
		return string(v.TurnID), int64(v.Seq), true
	case conversation.ReceiptEvent:
		return string(v.TurnID), int64(v.Seq), true
	case conversation.CitationEvent:
		return string(v.TurnID), int64(v.Seq), true
	case conversation.ProgressEvent:
		return string(v.TurnID), int64(v.Seq), true
	case conversation.ErrorEvent:
		return string(v.TurnID), int64(v.Seq), true
	case conversation.DoneEvent:
		return string(v.TurnID), int64(v.Seq), true
	case conversation.BlockEvent:
		return string(v.TurnID), int64(v.Seq), true
	}
	return "", 0, false
}

// Map returns the AG-UI events for one TurnEvent, in order. TurnEvents must be given in seq order, once each. The first
// call also produces the opening of the run (RUN_STARTED, the attachments, the initial state). When it returns an error
// the events returned with it end the run (RUN_ERROR) and the mapper is finished; send them and stop.
func (m *Mapper) Map(ev conversation.TurnEvent) ([]Emitted, error) {
	if m.w.finished {
		return nil, contracts.New(contracts.ErrConflict, "agui: the run has ended; no event follows it")
	}
	turnID, seq, ok := identify(ev)
	if !ok {
		return nil, contracts.New(contracts.ErrValidationFailed, "agui: the turn event has no variant")
	}
	if m.turnID == "" {
		m.turnID = turnID
	} else if turnID != m.turnID {
		return nil, contracts.New(contracts.ErrValidationFailed, "agui: the turn event belongs to another turn")
	}
	b := &batch{m: m, seq: seq}
	if !m.w.started {
		m.open(b)
	}
	switch v := ev.Value.(type) {
	case conversation.TokenEvent:
		b.text(v.Text)
	case conversation.DirectiveEvent:
		b.closeText()
		d := v.Directive
		m.directive = &d
		b.emit(NewCustom(DirectiveName, rawJSON(d)))
	case conversation.ClarificationEvent:
		b.closeText()
		m.block(b, conversation.RichBlock{Value: v.Clarification})
	case conversation.BlockEvent:
		b.closeText()
		m.block(b, v.Block)
	case conversation.PreparedActionEvent:
		b.closeText()
		m.preparedAction(b, v.PreparedAction)
	case conversation.ReceiptEvent:
		b.closeText()
		m.receipt(b, v.Receipt)
	case conversation.CitationEvent:
		b.closeText()
		m.citation(b, v.Citation)
	case conversation.ProgressEvent:
		b.closeText()
		m.progressEvent(b, v)
	case conversation.ErrorEvent:
		e := v.Error
		m.pendErr = &e // the run ends with it at the done event: a RUN_ERROR is terminal, and the text that explains it is still to come
	case conversation.DoneEvent:
		m.done(b, v)
	}
	if b.err != nil {
		return b.fail(b.err)
	}
	return b.out, nil
}

// open produces the start of the run.
func (m *Mapper) open(b *batch) {
	b.emit(NewRunStarted(m.rc.ThreadID, m.rc.RunID))
	if len(m.rc.Attachments) > 0 {
		files := make([]map[string]any, 0, len(m.rc.Attachments))
		for _, f := range m.rc.Attachments {
			files = append(files, map[string]any{
				"fileId": string(f.FileID), "name": f.Name, "mimeType": f.MimeType, "sizeBytes": f.SizeBytes,
				"previewable": f.Previewable, "expiresAt": string(f.ExpiresAt),
			})
		}
		b.emit(NewActivitySnapshot("act_"+m.turnID+"_attachments", ActivityAttachments, map[string]any{"files": files}))
	}
	if m.rc.StateBefore != nil {
		if ev, err := InitialState(m.rc.StateMode, *m.rc.StateBefore); err == nil {
			b.emit(ev)
		}
	}
}

func rawJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return data
}

// ---- progress ----

func (m *Mapper) progressEvent(b *batch, v conversation.ProgressEvent) {
	stage := string(v.Stage)
	active := false
	for _, s := range m.w.steps {
		active = active || s == stage
	}
	if !active {
		b.emit(NewStepStarted(stage))
	}
	// An activity carries what the step name does not say: a percentage, the capability or the job.
	if _, has := m.progress[stage]; has || v.State == conversation.ProgressEventStateProgress ||
		v.Percent != nil || v.JobID != nil || v.CapabilityID != nil {
		m.progressActivity(b, stage, v)
	}
	if v.State == conversation.ProgressEventStateCompleted || v.State == conversation.ProgressEventStateFailed {
		b.emit(NewStepFinished(stage))
	}
}

func (m *Mapper) progressActivity(b *batch, stage string, v conversation.ProgressEvent) {
	content := map[string]any{"stage": stage, "state": string(v.State)}
	if v.Percent != nil {
		content["percent"] = *v.Percent
	}
	if v.CapabilityID != nil {
		content["capabilityId"] = string(*v.CapabilityID)
	}
	if v.JobID != nil {
		content["jobId"] = string(*v.JobID)
	}
	st, seen := m.progress[stage]
	if !seen {
		st = &activityState{id: "act_" + m.turnID + "_" + stage, vals: content}
		m.progress[stage] = st
		b.emit(NewActivitySnapshot(st.id, ActivityProgress, content))
		return
	}
	var ops []PatchOp
	for _, k := range []string{"state", "percent", "capabilityId", "jobId"} {
		nv, has := content[k]
		if !has {
			continue
		}
		if old, had := st.vals[k]; !had || old != nv {
			op := "add"
			if had {
				op = "replace"
			}
			ops = append(ops, PatchOp{Op: op, Path: "/" + k, Value: nv})
		}
	}
	st.vals = content
	if len(ops) > 0 {
		b.emit(NewActivityDelta(st.id, ActivityProgress, ops))
	}
}

// ---- citations and blocks ----

func (m *Mapper) citation(b *batch, c conversation.Citation) {
	val := rawJSON(c)
	if m.citations == nil {
		m.citations = &activityState{id: "act_" + m.turnID + "_citations"}
		b.emit(NewActivitySnapshot(m.citations.id, ActivityCitations, map[string]any{"citations": []json.RawMessage{val}}))
		return
	}
	b.emit(NewActivityDelta(m.citations.id, ActivityCitations, []PatchOp{{Op: "add", Path: "/citations/-", Value: val}}))
}

// block maps one rich block. Markdown and code are text, so a plain chat component shows them; the structured blocks
// (table, chart, diagram, citation list, action card, clarification choices, confirmation panel) are activities that a
// renderer registered for their type shows, and that carry the validated block as it is.
func (m *Mapper) block(b *batch, blk conversation.RichBlock) {
	switch v := blk.Value.(type) {
	case conversation.MarkdownBlock:
		m.wholeMessage(b, v.Text)
		return
	case conversation.CodeBlock:
		lang := ""
		if v.Language != nil {
			lang = *v.Language
		}
		m.wholeMessage(b, codeFence(lang, v.Code))
		return
	}
	name := blockTypeName(blk.Value)
	if name == "" {
		return
	}
	m.blocks++
	b.emit(NewActivitySnapshot("act_"+m.turnID+"_b"+strconv.Itoa(m.blocks), ActivityBlockPrefix+name, rawJSON(blk)))
}

// wholeMessage sends a complete assistant message.
func (m *Mapper) wholeMessage(b *batch, text string) {
	if text == "" {
		return
	}
	b.text(text)
	b.closeText()
}

// codeFence writes code as a fenced Markdown block whose fence is longer than any run of backticks in the code, so the
// code cannot close it.
func codeFence(lang, code string) string {
	longest, run := 0, 0
	for _, r := range code {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	lang = strings.Map(func(r rune) rune {
		if r == '`' || r == '\n' || r == '\r' || r == ' ' {
			return -1
		}
		return r
	}, lang)
	return fence + lang + "\n" + strings.TrimRight(code, "\n") + "\n" + fence
}

func blockTypeName(v conversation.RichBlockValue) string {
	switch v.(type) {
	case conversation.MarkdownBlock:
		return "markdown"
	case conversation.CodeBlock:
		return "code"
	case conversation.TableBlock:
		return "table"
	case conversation.ChartBlock:
		return "chart"
	case conversation.DiagramBlock:
		return "diagram"
	case conversation.CitationListBlock:
		return "citation_list"
	case conversation.ActionCardBlock:
		return "action_card"
	case conversation.ClarificationChoicesBlock:
		return "clarification_choices"
	case conversation.ConfirmationPanelBlock:
		return "confirmation_panel"
	}
	return ""
}

// ---- prepared actions, receipts and interrupts ----

// confirmView is what a client is shown of a prepared action: exactly what the host validated and the core decided. It
// has no confirmation secret, no idempotency key and no data versions.
type confirmView struct {
	PreparationID string                          `json:"preparationId"`
	CapabilityID  string                          `json:"capabilityId"`
	Summary       string                          `json:"summary"`
	Effects       []actions.Effect                `json:"effects"`
	RiskLevel     actions.RiskLevel               `json:"riskLevel"`
	ExpiresAt     string                          `json:"expiresAt"`
	Confirmation  actions.ConfirmationRequirement `json:"confirmation"`
}

func viewOf(pa actions.PreparedAction) *confirmView {
	effects := pa.Effects
	if effects == nil {
		effects = []actions.Effect{}
	}
	return &confirmView{
		PreparationID: string(pa.PreparationID), CapabilityID: string(pa.CapabilityID), Summary: pa.Summary,
		Effects: effects, RiskLevel: pa.RiskLevel, ExpiresAt: string(pa.ExpiresAt), Confirmation: pa.Confirmation,
	}
}

func (m *Mapper) preparedAction(b *batch, pa actions.PreparedAction) {
	view := viewOf(pa)
	if pa.Confirmation.Mode == actions.ConfirmationRequirementModeStepUp {
		// The AI path cannot complete it: the call hands the user to the application's own interface.
		call := "alixai_continue_" + string(pa.PreparationID)
		b.emit(NewToolCallStart(call, ContinueToolName))
		b.emit(NewToolCallArgs(call, string(rawJSON(view))))
		b.emit(NewToolCallEnd(call))
		return
	}
	m.prepared = view
	call := ConfirmCallID(view.PreparationID)
	b.emit(NewToolCallStart(call, ConfirmToolName))
	b.emit(NewToolCallArgs(call, string(rawJSON(view))))
	b.emit(NewToolCallEnd(call))
}

// receipt maps an action receipt. In the run that resumes a confirmation it is the result of the confirmation's tool
// call: the application's deterministic facts. Anywhere else it is an activity.
func (m *Mapper) receipt(b *batch, rc actions.ActionReceipt) {
	if r := m.rc.Resume; r != nil && !m.resultOut && string(rc.PreparationID) == r.PreparationID {
		m.toolResult(b, r.PreparationID, rawJSON(rc))
		return
	}
	m.blocks++
	b.emit(NewActivitySnapshot("act_"+m.turnID+"_b"+strconv.Itoa(m.blocks), ActivityReceipt, rawJSON(rc)))
}

func (m *Mapper) toolResult(b *batch, preparationID string, content json.RawMessage) {
	m.resultOut = true
	m.results++
	b.emit(NewToolCallResult("msg_"+m.turnID+"_r"+strconv.Itoa(m.results), ConfirmCallID(preparationID), string(content)))
}

// resolveConfirmation closes the confirmation card of a resume run that produced no receipt (a rejection, a refusal, a
// failure), so the client never leaves it waiting.
func (m *Mapper) resolveConfirmation(b *batch) {
	r := m.rc.Resume
	if r == nil || m.resultOut {
		return
	}
	res := map[string]any{"preparationId": r.PreparationID, "status": "not_committed"}
	switch {
	case !r.Approved:
		res["status"] = "rejected"
	case m.pendErr != nil:
		res["status"] = "failed"
		res["code"] = string(m.pendErr.Code)
	}
	m.toolResult(b, r.PreparationID, rawJSON(res))
}

// ---- the end of the run ----

func (m *Mapper) done(b *batch, d conversation.DoneEvent) {
	b.closeText()
	for i := len(m.w.steps) - 1; i >= 0; i-- {
		b.emit(NewStepFinished(m.w.steps[i]))
	}
	m.resolveConfirmation(b)
	if m.rc.StateBefore != nil && m.rc.StateAfter != nil {
		if ev, err := StateChange(*m.rc.StateBefore, *m.rc.StateAfter); err == nil && ev != nil {
			b.emit(ev)
		}
	}
	result := map[string]any{"turnId": m.turnID, "finishReason": string(d.FinishReason)}

	// A typed error ends the run as an error whatever the done event says: the run never finishes as if it had worked.
	if e := m.pendErr; e != nil {
		b.emit(NewRunError(e.Message, string(e.Code)))
		return
	}
	switch d.FinishReason {
	case conversation.DoneEventFinishReasonFailed:
		b.emit(NewRunError("the turn failed", string(contracts.ErrInternal)))
	case conversation.DoneEventFinishReasonCancelled:
		b.emit(NewRunFinished(m.rc.ThreadID, m.rc.RunID, result, &Outcome{Type: "cancelled"}))
	case conversation.DoneEventFinishReasonAwaitingConfirmation:
		p := m.prepared
		if p == nil || d.PreparationID == nil || string(*d.PreparationID) != p.PreparationID {
			// The done event promises a confirmation the run never showed: finishing as if it had would leave the
			// user nothing to answer.
			b.emit(NewRunError("the turn awaits a confirmation that was not shown", string(contracts.ErrInternal)))
			return
		}
		b.emit(NewRunFinished(m.rc.ThreadID, m.rc.RunID, result, &Outcome{Type: "interrupt", Interrupts: []Interrupt{interruptOf(p)}}))
	default: // completed, awaiting_input
		m.uiAction(b)
		m.suggestions(b)
		b.emit(NewRunFinished(m.rc.ThreadID, m.rc.RunID, result, &Outcome{Type: "success"}))
	}
}

// uiAction emits the UI tool call of the turn: START with the tool's name, ARGS with the encoded arguments, END. The call
// is checked once more here, because this is where a string a model wrote becomes an event: a reserved name, a name that
// is not a tool name, arguments that are not one bounded JSON object fail the run closed (a producer bug), nothing is sent.
func (m *Mapper) uiAction(b *batch) {
	ua := m.rc.UIAction
	if ua == nil {
		return
	}
	if err := checkUIActionCall(*ua); err != nil {
		b.err = err
		return
	}
	call := UICallID(m.turnID)
	b.emit(NewToolCallStart(call, ua.Name))
	b.emit(NewToolCallArgs(call, string(ua.Arguments)))
	b.emit(NewToolCallEnd(call))
}

// MaxUIActionArgsBytes bounds the arguments of a UI tool call (dialogue.UIActionMaxArgsBytes).
const MaxUIActionArgsBytes = dialogue.UIActionMaxArgsBytes

func checkUIActionCall(ua UIActionCall) error {
	if !toolNameRE.MatchString(ua.Name) || strings.HasPrefix(strings.ToLower(ua.Name), "alixai") {
		return contracts.New(contracts.ErrValidationFailed, "agui: the UI action does not name a UI tool")
	}
	args := bytes.TrimSpace(ua.Arguments)
	var obj map[string]json.RawMessage
	if len(args) == 0 || len(args) > MaxUIActionArgsBytes || args[0] != '{' || json.Unmarshal(args, &obj) != nil {
		return contracts.New(contracts.ErrValidationFailed, "agui: the UI action arguments are not one bounded JSON object")
	}
	return nil
}

// suggestions sends the follow-up suggestions of the turn. They are computed from the directive the run already sent, so
// they offer nothing the directive does not allow, and no model is involved.
func (m *Mapper) suggestions(b *batch) {
	if m.directive == nil {
		return
	}
	if s := Suggest(m.directive, m.rc.Language, m.rc.Catalog, m.rc.MaxSuggestions); len(s) > 0 {
		b.emit(NewCustom(SuggestionsName, map[string]any{"suggestions": s}))
	}
}

// responseSchema is the JSON Schema of the payload of a resume: only an explicit boolean approves.
var responseSchema = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"approved": map[string]any{"type": "boolean"}},
	"required":             []string{"approved"},
	"additionalProperties": false,
}

func interruptOf(p *confirmView) Interrupt {
	return Interrupt{
		ID:             p.PreparationID,
		Reason:         ReasonConfirmation,
		Message:        clipRunes(p.Summary, 500),
		ToolCallID:     ConfirmCallID(p.PreparationID),
		ResponseSchema: responseSchema,
		ExpiresAt:      p.ExpiresAt,
		Metadata:       p,
	}
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n-1])) + "…"
}

func validID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}
