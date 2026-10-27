package agui

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
)

// Bounds of a RunAgentInput. A request over a bound is refused whole (validation_failed): nothing is cut silently
// except the client context, which is bounded by design and says what it left out.
const (
	MaxInputBytes   = 1 << 20
	MaxMessages     = 200
	MaxTools        = 32
	MaxContextItems = 32
	// MaxUtteranceBytes bounds the user's message; the engine clips it again to its profile's utterance limit.
	MaxUtteranceBytes = 32 << 10
	maxToolBytes      = 16 << 10
)

// RunAgentInput is the input of an AG-UI run, as much of it as AlixAI reads. Members it does not read (a newer
// protocol version's, forwardedProps beyond the "alixai" object, parentRunId) are accepted and ignored. Everything in
// it is client data: nothing here is a capability, a permission, a scope or an instruction.
type RunAgentInput struct {
	ThreadID       string          `json:"threadId"`
	RunID          string          `json:"runId"`
	State          json.RawMessage `json:"state"`
	Messages       []InputMessage  `json:"messages"`
	Tools          []InputTool     `json:"tools"`
	Context        []InputContext  `json:"context"`
	ForwardedProps json.RawMessage `json:"forwardedProps"`
	Resume         []ResumeEntry   `json:"resume"`
}

// InputMessage is one message of the client's history. Content is a string, or an array of content parts.
type InputMessage struct {
	ID         string          `json:"id"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
}

// InputTool is a tool the client declares: a UI-only declaration of something the client executes itself.
type InputTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// InputContext is one readable-context item of the client (CopilotKit's useCopilotReadable).
type InputContext struct {
	Description string `json:"description"`
	Value       string `json:"value"`
}

// ResumeEntry answers an interrupt: status resolved or cancelled, and a payload.
type ResumeEntry struct {
	InterruptID string          `json:"interruptId"`
	Status      string          `json:"status"`
	Payload     json.RawMessage `json:"payload"`
}

func invalid(reason, msg string) error {
	return contracts.New(contracts.ErrValidationFailed, msg).WithDetails("reason", reason)
}

// ParseRunAgentInput decodes the body of a run request. It checks the shape and the bounds, not the meaning: Intake does
// that, with the verified scope.
func ParseRunAgentInput(data []byte) (RunAgentInput, error) {
	if len(data) > MaxInputBytes {
		return RunAgentInput{}, invalid("input_too_large", "the run input is too large")
	}
	var in RunAgentInput
	if err := json.Unmarshal(data, &in); err != nil {
		return RunAgentInput{}, invalid("malformed_input", "the run input is not a valid RunAgentInput")
	}
	if !validID(in.ThreadID) || !validID(in.RunID) {
		return RunAgentInput{}, invalid("ids", "threadId and runId are required and must be well-formed")
	}
	if in.Messages == nil {
		return RunAgentInput{}, invalid("messages", "messages is required")
	}
	if len(in.Messages) > MaxMessages || len(in.Tools) > MaxTools || len(in.Context) > MaxContextItems || len(in.Resume) > 1 {
		return RunAgentInput{}, invalid("too_many", "the run input has more messages, tools, context items or resume entries than allowed")
	}
	return in, nil
}

// Preparations is the part of the action coordinator an AG-UI resume needs: reading a preparation under the verified
// scope. *actions.Coordinator implements it, as does the gateway's ActionService.
type Preparations interface {
	Get(ctx context.Context, scope contracts.Scope, preparationID string) (contracts.PreparedAction, error)
}

// Env is what Intake needs besides the input.
type Env struct {
	// Preparations resolves the preparation of a resume. Without it a resume is refused (unavailable).
	Preparations Preparations
	// IsCapability reports whether an id names a capability of the application. A client tool may not carry one's name.
	IsCapability func(id string) bool
}

// Request is a run request after Intake: what to run, and nothing the client could not be trusted with.
type Request struct {
	Kind     RunKind
	ThreadID string
	RunID    string
	// Input is the turn the engine runs: the last user message as text, a choice, a confirmation or a rejection. It is
	// the zero value for a connect run. It is built from the user's message and nothing else the client sent.
	Input dialogue.TurnInput
	// AttachmentIDs are the pre-uploaded files the user's message names (file ids of POST /v1/files), which the caller
	// resolves under the verified scope before the engine runs. Inline bytes and URLs are refused.
	AttachmentIDs []string
	// UIResult is set on a connect run that was caused by the result of a UI tool call the client ran (a tool message whose
	// call id starts with UICallPrefix). No turn runs and no model is called: the result is data, one more line of client
	// context for the NEXT turn (the client resends the thread's messages with it), never an answer to anything, so the
	// gateway answers it with UIResultRun (no history is read).
	UIResult bool
	// Resume is set for a resume run.
	Resume *ResumeRef
	// StateMode is how the client's shared state is to be updated.
	StateMode StateMode
	// ClientTools are the names of the tools the client declared for this run. They are declarations of UI the client
	// renders or executes; they are never added to the capabilities of the turn.
	ClientTools []string
	// UITools are the same declarations with their descriptions and parameter schemas, bounded, for the engine
	// (dialogue.WithUITools, see EngineContext): the model may be offered one only if the host also registered its name for
	// the application. Client data: the engine shows the description to the model only inside its fenced client block.
	UITools []dialogue.UIToolDeclaration
	// Context is the client's state, readable context, earlier messages and system messages, as bounded, fenced,
	// untrusted data. It is not part of Input.
	Context ClientContext
	// ClientCapabilities is what the client announced it can parse of the 1.5 surface
	// (forwardedProps.alixai.clientCapabilities, ADR 0047). Meaningful only when
	// HasClientCapabilities is true; absent means 1.4: the stream hub maps every event
	// to the frozen form before emit.
	ClientCapabilities    analyst.ClientCapabilities
	HasClientCapabilities bool
}

// errNotResolved is the one refusal for everything that does not resolve for the caller: a thread that is not the
// session of the token, an interrupt of another thread, of another principal, of another tenant or that never existed.
// It is built the same way every time, so the answers cannot be told apart.
func errNotResolved() error {
	return contracts.New(contracts.ErrScopeViolation, "the thread or the interrupt does not resolve for this caller")
}

// Intake turns a run request into a Request. The scope is the verified one of the context (identity.RequireScope); the
// thread of the run is the session of the token, never something the client names.
//
// confirmation is the single-use confirmation secret as the SDK agent of the host backend presented it (the
// X-AlixAI-Confirmation-Token header). It is a separate argument on purpose: the secret is not read from the body, so
// a payload that carries one is refused, and it is used only to approve a preparation.
//
// The rules (docs/integration/agui.md):
//   - the thread id must be the session id of the scope;
//   - exactly one of resume, a choice, or a user message is answered; no messages at all is a connect run;
//   - the utterance is the last user message's text and nothing else; earlier user messages are hints;
//     earlier assistant messages from the client are dropped (session store is the record; ADR 0049);
//   - tools are declarations: valid names, not reserved, not a capability's name;
//   - attachments are file references only;
//   - a resume approves only on an explicit {"approved": true}, with the secret, for a preparation of this thread that
//     the coordinator resolves in this scope.
func Intake(ctx context.Context, in RunAgentInput, confirmation string, env Env) (Request, error) {
	scope, err := identity.RequireScope(ctx)
	if err != nil {
		return Request{}, err
	}
	if !validID(in.ThreadID) || !validID(in.RunID) {
		return Request{}, invalid("ids", "threadId and runId are required and must be well-formed")
	}
	if in.ThreadID != scope.SessionID {
		return Request{}, errNotResolved()
	}
	tools, decls, err := checkTools(in.Tools, env)
	if err != nil {
		return Request{}, err
	}
	props, err := readProps(in.ForwardedProps)
	if err != nil {
		return Request{}, err
	}
	req := Request{
		ThreadID: in.ThreadID, RunID: in.RunID, ClientTools: tools, UITools: decls, StateMode: StateSnapshotMode,
		ClientCapabilities: props.ClientCapabilities, HasClientCapabilities: props.HasClientCapabilities,
	}
	if trimmed := bytes.TrimSpace(in.State); len(trimmed) > 0 && trimmed[0] == '{' {
		req.StateMode = StatePatchMode
	}

	// Which run is it? Resume first: the explicit answer to an interrupt.
	var (
		approval *ResumeRef
		hintsEnd = len(in.Messages)
	)
	switch {
	case len(in.Resume) > 0:
		if props.ChoiceID != "" {
			return Request{}, invalid("ambiguous_input", "a run answers an interrupt or a choice, not both")
		}
		approval, err = approvalFromResume(in.Resume[0])
	case props.ChoiceID != "":
		req.Kind = RunKindChoice
		req.Input = dialogue.TurnInput{ChoiceID: props.ChoiceID, Language: props.Language}
	case len(in.Messages) == 0:
		req.Kind = RunKindConnect
	default:
		last := in.Messages[len(in.Messages)-1]
		switch last.Role {
		case "user":
			hintsEnd = len(in.Messages) - 1
			err = intakeUtterance(&req, last, props.Language)
		case "tool":
			if strings.HasPrefix(last.ToolCallID, UICallPrefix) {
				// The client ran a UI tool the model asked for and sends the result back. It is data: no turn runs, nothing is
				// approved or looked up, and the result reaches the next turn as a line of client context. A tool result that is
				// not for a call of this agent (a client's own tool, a made-up id) is refused below, as ever.
				if !validID(last.ToolCallID) {
					err = invalid("tool_result", "the tool call id is malformed")
					break
				}
				req.Kind, req.UIResult = RunKindConnect, true
				hintsEnd = len(in.Messages)
				break
			}
			approval, err = approvalFromToolMessage(last)
		default:
			err = invalid("no_user_message", "the run has no user message to answer")
		}
	}
	if err != nil {
		return Request{}, err
	}
	if approval != nil {
		req.Kind, req.Resume = RunKindResume, approval
		if err := resolveApproval(ctx, scope, approval, confirmation, env, &req); err != nil {
			return Request{}, err
		}
		req.Input.Language = props.Language
	}
	req.Context = buildContext(in.State, in.Context, in.Messages, hintsEnd)
	return req, nil
}

// EngineContext returns ctx carrying what the engine reads of the run besides the turn input: the client context
// (dialogue.WithClientContext) and the tools the client declared (dialogue.WithUITools). Both are client data. The engine
// offers a UI tool only if the host registered it for the application (dialogue.Deps.UITools), and shows the client
// context to the model only as one fenced, bounded, untrusted block of the proposal prompt. A caller that does not call
// this keeps both away from the engine, which is the behaviour of a run without them.
func (r Request) EngineContext(ctx context.Context) context.Context {
	ctx = dialogue.WithClientContext(ctx, r.Context.Dialogue())
	return dialogue.WithUITools(ctx, r.UITools)
}

// props are the members of forwardedProps.alixai that AlixAI reads. Anything else in forwardedProps is ignored.
type props struct {
	ChoiceID              string
	Language              string
	ClientCapabilities    analyst.ClientCapabilities
	HasClientCapabilities bool
}

var languageRE = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`)

func readProps(raw json.RawMessage) (props, error) {
	var p props
	if len(bytes.TrimSpace(raw)) == 0 {
		return p, nil
	}
	var outer struct {
		AlixAI *struct {
			ChoiceID           string          `json:"choiceId"`
			Language           string          `json:"language"`
			ClientCapabilities json.RawMessage `json:"clientCapabilities"`
		} `json:"alixai"`
	}
	if json.Unmarshal(raw, &outer) != nil || outer.AlixAI == nil {
		return p, nil // not an object, or no alixai member: nothing for us in it
	}
	if c := outer.AlixAI.ChoiceID; c != "" {
		if !validID(c) {
			return p, invalid("choice", "the choice id is malformed")
		}
		p.ChoiceID = c
	}
	if l := outer.AlixAI.Language; l != "" {
		if !languageRE.MatchString(l) {
			return p, invalid("language", "the language is not a BCP 47 tag")
		}
		p.Language = l
	}
	if caps := bytes.TrimSpace(outer.AlixAI.ClientCapabilities); len(caps) > 0 && string(caps) != "null" {
		var cc analyst.ClientCapabilities
		if err := json.Unmarshal(caps, &cc); err != nil {
			return p, invalid("client_capabilities", "clientCapabilities is not a valid ClientCapabilities")
		}
		p.ClientCapabilities = cc
		p.HasClientCapabilities = true
	}
	return p, nil
}

// ---- tools ----

var toolNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,63}$`)

// UIToolNameAllowed reports whether a tool name could ever be a UI tool of the application: well-formed, outside the
// reserved alixai namespace and not the name of a capability (isCapability may be nil). It is the name rule of
// Intake, for a caller that has the host's list of registered names and wants to know whether any of them is usable.
func UIToolNameAllowed(name string, isCapability func(id string) bool) bool {
	if !toolNameRE.MatchString(name) || strings.HasPrefix(strings.ToLower(name), "alixai") {
		return false
	}
	return isCapability == nil || !(isCapability(name) || isCapability(strings.ToLower(name)))
}

// checkTools validates the client's tool declarations and returns their names and the bounded declarations. A declaration
// is accepted only as a UI declaration: the name must be well-formed, must not be in the reserved alixai namespace and
// must not be the name of a capability of the application, so no tool of the client can pass for something AlixAI does or
// be mistaken for it. The declaration is bounded and kept as data for the engine: it is not read here, it is never added
// to the capabilities of the turn, and the engine offers it to the model only if the host registered the name.
func checkTools(in []InputTool, env Env) ([]string, []dialogue.UIToolDeclaration, error) {
	var names []string
	var decls []dialogue.UIToolDeclaration
	seen := map[string]bool{}
	for _, t := range in {
		if !toolNameRE.MatchString(t.Name) {
			return nil, nil, invalid("tool_name", "a tool name is malformed")
		}
		if strings.HasPrefix(strings.ToLower(t.Name), "alixai") || (env.IsCapability != nil && (env.IsCapability(t.Name) || env.IsCapability(strings.ToLower(t.Name)))) {
			return nil, nil, invalid("tool_name_reserved", "a tool name is not allowed")
		}
		if seen[t.Name] {
			return nil, nil, invalid("tool_duplicate", "a tool is declared twice")
		}
		if len(t.Description)+len(t.Parameters) > maxToolBytes {
			return nil, nil, invalid("tool_too_large", "a tool declaration is too large")
		}
		seen[t.Name] = true
		names = append(names, t.Name)
		decls = append(decls, dialogue.UIToolDeclaration{Name: t.Name, Description: t.Description, Parameters: append(json.RawMessage(nil), t.Parameters...)})
	}
	return names, decls, nil
}

// ---- the user's message ----

func intakeUtterance(req *Request, m InputMessage, language string) error {
	text, fileIDs, err := userContent(m.Content)
	if err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return invalid("empty_message", "the last user message has no text")
	}
	if len(text) > MaxUtteranceBytes {
		return invalid("message_too_large", "the user message is too large")
	}
	if len(fileIDs) > dialogue.AttachmentMaxCount {
		return invalid("too_many_attachments", "a message carries too many attachments")
	}
	req.Kind = RunKindTurn
	req.Input = dialogue.TurnInput{Text: text, Language: language}
	req.AttachmentIDs = fileIDs
	return nil
}

// userContent reads the content of a user message: its text and the pre-uploaded files it names. A file part is
// accepted only as a reference to a file the user uploaded to AlixAI (source type "file", no other provider);
// inline bytes and URLs are refused, so no byte and no address a client names is ever fetched or read.
func userContent(raw json.RawMessage) (string, []string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", nil, invalid("malformed_content", "the message content is malformed")
		}
		return s, nil, nil
	}
	var parts []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source *struct {
			Type     string `json:"type"`
			Value    string `json:"value"`
			Provider string `json:"provider"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, invalid("malformed_content", "the message content is malformed")
	}
	var texts, ids []string
	seen := map[string]bool{}
	for _, p := range parts {
		switch p.Type {
		case "text":
			texts = append(texts, p.Text)
		case "image", "audio", "video", "document":
			if p.Source == nil {
				return "", nil, invalid("malformed_content", "a content part has no source")
			}
			switch {
			case p.Source.Type == "data":
				return "", nil, invalid("inline_attachment", "attachments are pre-uploaded files, not inline bytes: upload the file first")
			case p.Source.Type == "url":
				return "", nil, invalid("url_attachment", "attachments are pre-uploaded files, not addresses: upload the file first")
			case p.Source.Type != "file" || (p.Source.Provider != "" && p.Source.Provider != "alixai") || !files.ValidID(p.Source.Value):
				return "", nil, invalid("attachment_reference", "an attachment must name a file uploaded to AlixAI")
			}
			if !seen[p.Source.Value] {
				seen[p.Source.Value] = true
				ids = append(ids, p.Source.Value)
			}
		default:
			return "", nil, invalid("content_part", "a content part of this type is not accepted")
		}
	}
	return strings.Join(texts, "\n"), ids, nil
}

// ---- interrupts and resume ----

// approvalFromResume reads the answer to an interrupt. Only an explicit {"approved": true} approves; {"approved": false}
// and a cancelled interrupt reject; anything else is refused. A confirm is never inferred.
func approvalFromResume(e ResumeEntry) (*ResumeRef, error) {
	if !validID(e.InterruptID) {
		return nil, invalid("interrupt_id", "the interrupt id is malformed")
	}
	switch e.Status {
	case "cancelled":
		return &ResumeRef{PreparationID: e.InterruptID, Approved: false}, nil
	case "resolved":
		approved, err := approvedPayload(e.Payload)
		if err != nil {
			return nil, err
		}
		return &ResumeRef{PreparationID: e.InterruptID, Approved: approved}, nil
	}
	return nil, invalid("resume_status", "the resume status is not resolved or cancelled")
}

// approvalFromToolMessage reads the answer to the confirmation tool call given as a tool message (the human-in-the-loop
// form of clients that answer a tool call instead of resuming an interrupt). The call id names the preparation; the
// content is the same {"approved": bool} payload.
func approvalFromToolMessage(m InputMessage) (*ResumeRef, error) {
	id, ok := strings.CutPrefix(m.ToolCallID, ConfirmCallPrefix)
	if !ok || !validID(id) {
		return nil, invalid("tool_result", "the tool result does not answer a confirmation of this agent")
	}
	var content string
	if err := json.Unmarshal(bytes.TrimSpace(m.Content), &content); err != nil {
		return nil, invalid("tool_result", "the tool result is not an approval")
	}
	approved, err := approvedPayload(json.RawMessage(content))
	if err != nil {
		return nil, err
	}
	return &ResumeRef{PreparationID: id, Approved: approved}, nil
}

// approvedPayload parses the one payload a resume or a tool result may carry: an object with exactly one member,
// "approved", a boolean. A missing payload, another type or another member (a secret, say) is refused, never read as a
// yes.
func approvedPayload(raw json.RawMessage) (bool, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &members); err != nil || len(members) != 1 {
		return false, invalid("approval_payload", `the answer must be {"approved": true} or {"approved": false}`)
	}
	v, ok := members["approved"]
	if !ok {
		return false, invalid("approval_payload", `the answer must be {"approved": true} or {"approved": false}`)
	}
	switch string(bytes.TrimSpace(v)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, invalid("approval_payload", `the answer must be {"approved": true} or {"approved": false}`)
}

// resolveApproval checks that the preparation is one this caller may answer and builds the engine input: a
// confirmation with the secret, or a rejection. The thread was checked against the scope first, so a preparation of
// another thread never reaches the coordinator; the coordinator then finds it only through the verified scope and only
// for the principal and the session that made it, and a miss of any kind is the same refusal as a foreign thread.
func resolveApproval(ctx context.Context, scope contracts.Scope, a *ResumeRef, confirmation string, env Env, req *Request) error {
	if env.Preparations == nil {
		return contracts.New(contracts.ErrUnavailable, "agui: no action service is configured to resolve an interrupt")
	}
	pa, err := env.Preparations.Get(ctx, scope, a.PreparationID)
	switch code := contracts.CodeOf(err); {
	case err == nil:
	case code == contracts.ErrScopeViolation || code == contracts.ErrPermissionDenied:
		return errNotResolved()
	default:
		return err
	}
	if string(pa.PreparationID) != a.PreparationID {
		return errNotResolved()
	}
	switch {
	case pa.State == act.PreparedActionStatePrepared:
	case !a.Approved && (pa.State == act.PreparedActionStateConfirmed || pa.State == act.PreparedActionStateRejected):
		// Declining an action that is already declined, or that was confirmed and has not committed, is idempotent:
		// the coordinator answers it.
	case pa.State == act.PreparedActionStateExpired:
		return contracts.New(contracts.ErrExpired, "the preparation has expired")
	default:
		return contracts.New(contracts.ErrConflict, "the preparation is no longer awaiting a confirmation")
	}
	if !a.Approved {
		req.Input = dialogue.TurnInput{Reject: a.PreparationID}
		return nil
	}
	if strings.TrimSpace(confirmation) == "" {
		return contracts.New(contracts.ErrConfirmationRequired, "an approval needs the confirmation secret of the preparation")
	}
	tok, err := actions.ParseConfirmationToken(strings.TrimSpace(confirmation))
	if err != nil {
		return err
	}
	req.Input = dialogue.TurnInput{Confirm: &dialogue.Confirmation{PreparationID: a.PreparationID, Token: tok}}
	return nil
}
