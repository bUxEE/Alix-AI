package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/actions"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/dialogue"
	ddec "github.com/buxee/alix-ai/internal/dialogue/decision"
	"github.com/buxee/alix-ai/internal/files"
	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The application-facing API of master plan 14.2: sessions and conversation
// (this file), actions (routes_v1_actions.go) and applications
// (routes_v1_apps.go). The contract is schemas/api/openapi.yaml; every route
// registered here is on it and TestRoutesMatchOpenAPISpec fails on any
// difference in either direction.
//
// # How the routes are mounted
//
// Each of the three areas of this API (v1_sessions here, v1_actions in
// routes_v1_actions.go, v1_apps in routes_v1_apps.go) registers itself with
// RegisterArea and takes the services it uses from Deps.Services by type
// (Need[T], see services.go): there is no struct that lists them and no
// injection through Options.ExtraAreas. A server built without those services
// (every M0 test) mounts none of the three and has no /v1 route at all; with
// only some of them it is not built (Needs.Ready); and a /v1 route without
// Options.V1Auth refuses every request (Router.V1). No handler reads a package
// variable.
//
// # Identity
//
// Every handler starts from identity.RequireScope: the scope comes from the
// verified context token and from nothing else. A tenant, workspace, principal
// or application in a path, a query or a body does not exist for these
// handlers; a session or application id in the path must be the one the token
// is bound to or the request is refused with scope_violation. Context tokens
// are single use: every request, and every stream open, needs a fresh one (the
// middleware consumes its jti). The deployment level of the scope is
// configuration, and no response ever carries a contracts.Scope.
//
// # Sessions
//
// A session is keyed by the session id the token carries (internal/sessions):
// the core never mints one. POST /v1/sessions opens the session the token
// names (creating it on first sight), GET resumes it, DELETE ends it. A later
// token for the same session goes through identity.CheckContinuity inside the
// store, so the principal, tenant and workspace cannot change.
//
// # Turns and the confirmation secret
//
// POST /v1/sessions/{id}/turns takes exactly one of text, a choice, a
// confirmation or a rejection. A confirmation is a structured input,
// {preparation_id, token}: the model never reads "yes" as consent. The turn
// answers with the TurnEvents of schemas/conversation, as a JSON document or,
// when the client asks for text/event-stream, as server-sent events. The engine
// runs the turn to completion before anything is sent, so the text of a token
// event has already passed the response verifier; the events are the core's
// account of that turn.
//
// # The stream hub
//
// The events do not go from the engine to the client directly: the handler
// opens the turn in the stream hub (internal/stream, Hub.Begin) and publishes
// them into it, and what the client receives, as JSON or as server-sent events,
// is read back from the hub. Three things follow. The hub validates every event
// before it is buffered, and the response text goes through Turn.PublishResponse,
// which sanitises the whole text before it cuts it into token events, so
// Markdown in a token event is never raw. A client that loses the connection
// resumes with GET /v1/sessions/{session_id}/turns/{turn_id}/events
// (routes_v1_turnevents.go), which only reads the hub and never runs the turn
// again. And the sequence number of an event is assigned when it is published,
// from the turn's NextSeq: an event the hub refuses uses up no number, so the
// numbers of the events that follow it never skip. A refusal fails the turn
// (Turn.Fail: an error event, then a done event with finish_reason failed) and
// withholds the confirmation secret, because the user would be confirming
// something the stream did not show.
//
// # Attachments
//
// A text turn may name files the user uploaded (TurnRequest.Attachments, file ids of
// POST /v1/files, at most dialogue.AttachmentMaxCount). Before anything runs they are
// resolved together through the files service (AttachModelFiles, files.Service.ForModel)
// under the turn's verified scope, so every use re-checks scope, principal, session, expiry
// and permission. A reference that does not resolve in that scope (another tenant's,
// another principal's, another session's, deleted, expired, never existed) fails the whole
// turn with ONE answer, scope_violation with no detail, built here from either code: the
// engine is not run, the session does not change and no model is called. What the files
// service lets a model see (the text of a text file, never a path, an id or image bytes)
// goes to the engine as TurnInput.Attachments, where the context builder places it in
// the proposal prompt fenced as untrusted data, and nowhere else: it is not the user's
// message, so it is not routed on, not recorded in the transcript and not replayed, and it
// is part of the prompt the turn is budgeted and audited on (ModelCall.PromptToks). A
// deployment that has no files service refuses a turn that carries them (unavailable): never
// a turn the user believes the model read a file in.
//
// The single-use confirmation secret of a prepared action is not part of any
// TurnEvent (the schema is closed, and a secret has no place in a stream a
// proxy may log). It leaves in three places, each to the client's own confirm
// control: the X-AlixAI-Confirmation-Token response header of an event-stream
// turn, the confirmation member of the JSON turn response, and the confirmation
// member of the turn.completed message of the voice socket (GET
// /v1/sessions/{session_id}/voice, routes_v1_voice.go; a WebSocket message, not
// a TurnEvent). It is shown to the client UI and never to the model.

func init() { RegisterArea("v1_sessions", registerV1Sessions) }

// turnerFor is the entry point of the HTTP turn routes (sessions, AG-UI, voice): the decision layer's
// wrapper (ddec.Turner, provided by serve only while the layer is built) when there is one, else the
// engine itself, so a deployment or a rig that provides only the engine is unchanged. A nil engine
// gives a nil Turner, never a typed nil. The wrapper adds no authority: it binds the verified scope
// of the turn for the scorer-backed detectors and counts what the turn avoided.
func turnerFor(n *Needs, e *dialogue.Engine) ddec.Turner {
	if e == nil {
		return nil
	}
	if t, ok := Want[ddec.Turner](n); ok && t != nil {
		return t
	}
	return e
}

// registerV1Sessions needs the session store, the dialogue engine, the action
// service (as ActionService: without the cross-tenant sweeps), the application
// service and the stream hub the events of a turn are published into.
func registerV1Sessions(rt *Router, d *Deps) {
	n := d.Needs()
	h := &sessionsHandler{
		sessions: Need[*sessions.Store](n),
		engine:   Need[*dialogue.Engine](n),
		actions:  Need[ActionService](n),
		apps:     Need[ApplicationService](n),
		hub:      Need[*stream.Hub](n),
		d:        d,
		// Optional: without it a turn that names files is refused, and the rest is unchanged.
		attach: attachWant(n),
	}
	if !n.Ready() {
		return
	}
	h.turner = turnerFor(n, h.engine)
	v1 := rt.V1()
	v1.HandleFunc("POST /sessions", h.create)
	v1.HandleFunc("GET /sessions/{session_id}", h.get)
	v1.HandleFunc("DELETE /sessions/{session_id}", h.end)
	v1.HandleFunc("GET /sessions/{session_id}/pending", h.pending)
	v1.HandleFunc("POST /sessions/{session_id}/turns", h.turn)
}

// v1Scope returns the verified scope of the request, or writes the refusal.
func v1Scope(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, err := identity.RequireScope(r.Context())
	if err != nil {
		WriteError(w, r, err)
		return contracts.Scope{}, false
	}
	return scope, true
}

// activePackage returns the application's active package, or writes the
// refusal: a quarantined application serves no traffic (SEC-054, SEC-060).
func activePackage(w http.ResponseWriter, r *http.Request, apps ApplicationService, scope contracts.Scope) (capabilities.Package, bool) {
	pkg, err := apps.Package(r.Context(), scope.ApplicationID)
	if err != nil {
		WriteError(w, r, err)
		return nil, false
	}
	return pkg, true
}

// sessionPath checks the session id of the path against the token's.
func sessionPath(w http.ResponseWriter, r *http.Request, scope contracts.Scope) bool {
	id := r.PathValue("session_id")
	if !validPathID(id) {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the session id is malformed"))
		return false
	}
	if id != scope.SessionID {
		WriteError(w, r, scopeLevelError(contracts.LevelSession))
		return false
	}
	return true
}

// sessionError writes the error of a session operation. An ended session is
// Gone, not a credential problem.
func sessionError(w http.ResponseWriter, r *http.Request, err error) {
	if contracts.CodeOf(err) == contracts.ErrExpired {
		WriteErrorStatus(w, r, http.StatusGone, err)
		return
	}
	WriteError(w, r, err)
}

// decodeOptionalJSON reads a JSON object body that may be absent.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 && r.Header.Get("Content-Type") == "" {
		return true
	}
	return decodeJSON(w, r, dst)
}

type sessionsHandler struct {
	sessions *sessions.Store
	engine   *dialogue.Engine
	// turner is what a turn runs through: the decision wrapper when the deployment provided one, else the engine.
	turner  ddec.Turner
	actions ActionService
	apps    ApplicationService
	hub     *stream.Hub
	// attach resolves the files a turn names; nil when no files service is provided.
	attach AttachModelFiles
	d      *Deps
}

// SessionBody is a session as the API reports it. It never carries the scope.
type SessionBody struct {
	SessionID string `json:"session_id"`
	// Status is active or ended.
	Status    string    `json:"status"`
	TurnCount int64     `json:"turn_count"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Pending is what the session waits for, or null.
	Pending *PendingState `json:"pending"`
}

// PendingState is the structured pending state of a session (master plan
// 14.2): what the active goal is waiting for. It holds identifiers, field
// names and the host's own choices and preparation; collected values stay in
// the session.
type PendingState struct {
	// Kind is confirmation (a prepared action awaits the user), clarification
	// (a question awaits an answer) or goal (an open goal with nothing blocking).
	Kind         string `json:"kind"`
	CapabilityID string `json:"capability_id"`
	Phase        string `json:"phase"`
	// CollectedFields are the fields the goal has a value for, with where
	// each came from.
	CollectedFields []PendingField `json:"collected_fields"`
	MissingFields   []string       `json:"missing_fields,omitempty"`
	// Question is the clarification the core asked.
	Question *PendingQuestion `json:"question,omitempty"`
	// Preparation is the prepared action that awaits confirmation, with its
	// current state. The confirmation secret is never part of it.
	Preparation *contracts.PreparedAction `json:"preparation,omitempty"`
	ExpiresAt   *time.Time                `json:"expires_at,omitempty"`
}

// PendingField is one collected field.
type PendingField struct {
	Name   string `json:"name"`
	Origin string `json:"origin"`
}

// PendingQuestion is the pending clarification.
type PendingQuestion struct {
	Field string `json:"field"`
	// AnswerKind is text, choice or entity.
	AnswerKind string       `json:"answer_kind"`
	Choices    []act.Choice `json:"choices,omitempty"`
}

// PendingBody is the body of GET /v1/sessions/{session_id}/pending.
type PendingBody struct {
	Pending *PendingState `json:"pending"`
}

func (h *sessionsHandler) sessionBody(ctx context.Context, scope contracts.Scope, sess sessions.Session) (SessionBody, error) {
	pending, err := h.pendingState(ctx, scope, sess)
	if err != nil {
		return SessionBody{}, err
	}
	return SessionBody{
		SessionID: sess.ID, Status: "active", TurnCount: sess.TurnCount,
		CreatedAt: sess.CreatedAt.UTC(), UpdatedAt: sess.UpdatedAt.UTC(), Pending: pending,
	}, nil
}

func (h *sessionsHandler) pendingState(ctx context.Context, scope contracts.Scope, sess sessions.Session) (*PendingState, error) {
	g := sess.State.Goal
	if g == nil {
		return nil, nil
	}
	p := &PendingState{CapabilityID: g.CapabilityID, Phase: g.Phase, Kind: "goal", CollectedFields: make([]PendingField, 0, len(g.Slots))}
	for name, slot := range g.Slots {
		p.CollectedFields = append(p.CollectedFields, PendingField{Name: name, Origin: string(slot.Origin)})
	}
	sort.Slice(p.CollectedFields, func(i, j int) bool { return p.CollectedFields[i].Name < p.CollectedFields[j].Name })
	if !g.ExpiresAt.IsZero() {
		t := g.ExpiresAt.UTC()
		p.ExpiresAt = &t
	}
	if last := sess.State.Last; last != nil && len(last.MissingFields) > 0 {
		p.MissingFields = append([]string(nil), last.MissingFields...)
	}
	if c := g.Pending; c != nil {
		p.Kind = "clarification"
		p.Question = &PendingQuestion{Field: c.Field, AnswerKind: string(c.Kind), Choices: append([]act.Choice(nil), c.Choices...)}
	}
	if g.PreparedID != "" {
		pa, err := h.actions.Get(ctx, scope, g.PreparedID)
		switch code := contracts.CodeOf(err); {
		case err == nil:
			p.Kind, p.Preparation = "confirmation", &pa
		case code == contracts.ErrScopeViolation:
			// The reference points at nothing this scope may see: it is not pending.
		default:
			return nil, err
		}
	}
	return p, nil
}

func (h *sessionsHandler) create(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	var req struct{}
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	sess, err := h.sessions.Open(r.Context(), scope)
	if err != nil {
		sessionError(w, r, err)
		return
	}
	body, err := h.sessionBody(r.Context(), scope, sess)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The store gives no way to tell a session it just created from one that
	// already existed untouched, and the difference changes nothing for the
	// caller: opening is idempotent, so it is always 200.
	writeJSON(w, http.StatusOK, body)
}

func (h *sessionsHandler) get(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok || !sessionPath(w, r, scope) {
		return
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	sess, err := h.sessions.Open(r.Context(), scope)
	if err != nil {
		sessionError(w, r, err)
		return
	}
	body, err := h.sessionBody(r.Context(), scope, sess)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *sessionsHandler) pending(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok || !sessionPath(w, r, scope) {
		return
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	sess, err := h.sessions.Open(r.Context(), scope)
	if err != nil {
		sessionError(w, r, err)
		return
	}
	p, err := h.pendingState(r.Context(), scope, sess)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, PendingBody{Pending: p})
}

// EndedBody is the body of DELETE /v1/sessions/{session_id}.
type EndedBody struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

func (h *sessionsHandler) end(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok || !sessionPath(w, r, scope) {
		return
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	// Ending invalidates the prepared action the session waits on first, and
	// never commits anything. Ending twice is the same as ending once.
	if err := h.sessions.End(r.Context(), scope, h.actions); err != nil && contracts.CodeOf(err) != contracts.ErrExpired {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, EndedBody{SessionID: scope.SessionID, Status: "ended"})
}

// ---- turns ----

// TurnRequest is the body of POST /v1/sessions/{session_id}/turns. Exactly one
// of Text, ChoiceID, Confirmation and Reject is set.
type TurnRequest struct {
	// Text is the user's message (or a finalised transcript).
	Text string `json:"text,omitempty"`
	// Language is the user's BCP 47 tag ("it", "en-GB"); empty lets the core choose.
	Language string `json:"language,omitempty"`
	// ChoiceID selects one of the choices of the pending clarification.
	ChoiceID string `json:"choice_id,omitempty"`
	// Confirmation presents the confirmation of the pending prepared action. It
	// is a structured input from the client UI: nothing the model writes, and no
	// text of the user's, can stand in for it.
	Confirmation *TurnConfirmation `json:"confirmation,omitempty"`
	// Reject declines the pending prepared action.
	Reject *TurnRejection `json:"reject,omitempty"`
	// Attachments are the ids of files the user uploaded (POST /v1/files) for this message: at
	// most dialogue.AttachmentMaxCount, with Text only. Each is resolved under the turn's scope.
	Attachments []string `json:"attachments,omitempty"`
}

// TurnConfirmation is Confirmation{PreparationID, Token} on the wire.
type TurnConfirmation struct {
	PreparationID string `json:"preparation_id"`
	Token         string `json:"token"`
}

// TurnRejection names the preparation to decline.
type TurnRejection struct {
	PreparationID string `json:"preparation_id"`
}

func (q TurnRequest) input() (dialogue.TurnInput, error) {
	in := dialogue.TurnInput{Text: q.Text, Language: q.Language, ChoiceID: q.ChoiceID}
	if c := q.Confirmation; c != nil {
		if !validPathID(c.PreparationID) {
			return in, contracts.New(contracts.ErrValidationFailed, "the preparation id is malformed")
		}
		tok, err := actions.ParseConfirmationToken(c.Token)
		if err != nil {
			return in, err
		}
		in.Confirm = &dialogue.Confirmation{PreparationID: c.PreparationID, Token: tok}
	}
	if rj := q.Reject; rj != nil {
		if !validPathID(rj.PreparationID) {
			return in, contracts.New(contracts.ErrValidationFailed, "the preparation id is malformed")
		}
		in.Reject = rj.PreparationID
	}
	return in, nil
}

// TurnBody is the JSON response of a turn.
type TurnBody struct {
	TurnID string                   `json:"turn_id"`
	Events []conversation.TurnEvent `json:"events"`
	// Confirmation is present when the turn prepared an action that needs the
	// user's explicit confirmation: the preparation and its single-use secret,
	// for the client UI.
	Confirmation *ConfirmationGrant `json:"confirmation,omitempty"`
}

// ConfirmationGrant is the single-use secret of one preparation.
type ConfirmationGrant struct {
	PreparationID string `json:"preparation_id"`
	Token         string `json:"token"`
}

// HeaderConfirmationToken carries the confirmation secret of an event-stream turn.
const HeaderConfirmationToken = "X-AlixAI-Confirmation-Token"

func wantsEventStream(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept") {
		for _, part := range strings.Split(v, ",") {
			mt, _, _ := strings.Cut(strings.TrimSpace(part), ";")
			if strings.EqualFold(strings.TrimSpace(mt), "text/event-stream") {
				return true
			}
		}
	}
	return false
}

func (h *sessionsHandler) turn(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok || !sessionPath(w, r, scope) {
		return
	}
	var req TurnRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	in, err := req.input()
	if err != nil {
		WriteError(w, r, err)
		return
	}
	attachIDs, err := req.attachIDs()
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// A quarantined application serves no traffic: nothing below runs, no
	// model is called and no adapter is reached.
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	// The files are resolved before the engine runs: a reference that does not
	// resolve fails the turn as a whole, with no model call and no turn recorded.
	in.Attachments, err = h.attachments(r.Context(), scope, attachIDs)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	extendWriteDeadline(w)
	res, terr := h.turner.Turn(r.Context(), scope, in)
	if res.TurnID == "" {
		// The turn could not be run (no valid scope, a limit, a session that is
		// not this context, a store that failed before anything was decided):
		// that is an error response, never a stream that ends well.
		if terr == nil {
			terr = contracts.New(contracts.ErrInternal, "the turn produced no result")
		}
		sessionError(w, r, terr)
		return
	}
	log := loggerFrom(r.Context())

	// The engine mints the turn id, so the stream is opened once the turn has
	// run, and the hub is where its account is kept: what the client receives
	// below is read back from it, which is also what a resume reads.
	turn, err := h.hub.Begin(scope, res.TurnID)
	if err != nil {
		// The turn ran and cannot be shown. Its receipt, if it has one, is
		// still the actions route's to give.
		log.Error("the turn ran but its event stream could not be opened", "turn_id", res.TurnID,
			"reason", string(stream.ReasonOf(err)), "code", string(contracts.CodeOf(err)))
		WriteError(w, r, err)
		return
	}
	perr := buildTurnEvents(res, terr).publish(turn)
	if perr != nil {
		// An event the hub refused is not sent and not skipped over: the turn
		// is failed with an error event and a failed done, at the next numbers.
		log.Error("a turn event was refused; the turn was failed", "turn_id", res.TurnID,
			"reason", string(stream.ReasonOf(perr)), "code", string(contracts.CodeOf(perr)))
		if ferr := turn.Fail(perr); ferr != nil {
			log.Error("the turn could not be failed cleanly", "turn_id", res.TurnID, "reason", string(stream.ReasonOf(ferr)))
		}
	}
	frames, err := turnEventsFrames(r.Context(), turn)
	if err != nil {
		log.Error("the events of the turn could not be read back", "turn_id", res.TurnID,
			"reason", string(stream.ReasonOf(err)), "code", string(contracts.CodeOf(err)))
		WriteError(w, r, contracts.New(contracts.ErrInternal, "the turn events could not be delivered"))
		return
	}

	// The secret leaves with a turn whose account was shown in full: with a
	// refusal in the middle the user may not have seen what they would confirm.
	var grant *ConfirmationGrant
	if perr == nil && res.Prepared != nil && !res.Confirmation.IsZero() {
		grant = &ConfirmationGrant{PreparationID: string(res.Prepared.PreparationID), Token: res.Confirmation.Reveal()}
	}
	if wantsEventStream(r) {
		if err := writeEventStream(w, frames, grant); err != nil {
			log.Warn("turn stream interrupted", "turn_id", res.TurnID)
		}
		return
	}
	events, err := turnEventsDecode(frames)
	if err != nil {
		log.Error("a buffered turn event does not decode", "turn_id", res.TurnID)
		WriteError(w, r, contracts.New(contracts.ErrInternal, "the turn events could not be delivered"))
		return
	}
	writeJSON(w, http.StatusOK, TurnBody{TurnID: res.TurnID, Events: events, Confirmation: grant})
}

// turnEventsFrames reads every event of a finished turn back from the hub, in
// order: the exact bytes a resume of the same turn would send.
func turnEventsFrames(ctx context.Context, t *stream.Turn) ([]stream.Frame, error) {
	if !t.Finished() {
		return nil, contracts.New(contracts.ErrInternal, "the turn is not finished")
	}
	sub, err := t.Subscribe(stream.FromStart)
	if err != nil {
		return nil, err
	}
	defer sub.Close()
	var frames []stream.Frame
	for {
		f, err := sub.Next(ctx)
		switch {
		case err == nil:
			frames = append(frames, f)
		case errors.Is(err, io.EOF):
			return frames, nil
		default:
			return nil, err
		}
	}
}

// turnEventsDecode decodes buffered frames as TurnEvents, for the JSON document.
func turnEventsDecode(frames []stream.Frame) ([]conversation.TurnEvent, error) {
	events := make([]conversation.TurnEvent, len(frames))
	for i, f := range frames {
		if err := json.Unmarshal(f.Data, &events[i]); err != nil {
			return nil, err
		}
	}
	return events, nil
}

// writeEventStream sends the buffered events as server-sent events: id is the
// event's seq (the resume cursor), event its type and data the TurnEvent JSON
// on one line, the very frames of GET .../events. The confirmation secret, if
// any, is a header: it is not a TurnEvent.
func writeEventStream(w http.ResponseWriter, frames []stream.Frame, grant *ConfirmationGrant) error {
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	if grant != nil {
		hd.Set(HeaderConfirmationToken, grant.Token)
	}
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	for _, f := range frames {
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", f.Seq, f.Type, f.Data); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
	}
	return nil
}

// ---- events ----

// turnEventsDraft is one event of a turn that has no sequence number yet: the
// number is whatever the stream says is next at the moment the event is
// published. Numbering the events up front would let an event the hub refuses
// leave a hole (a refused event does not use up its number), and the events
// after it would then arrive out of order.
type turnEventsDraft func(seq conversation.Seq) conversation.TurnEventValue

// turnEventsPlan is the account of one turn, in the order it is published: the
// events before the text, the response text (which Turn.PublishResponse
// sanitises as a whole and cuts into token events), and the done event.
type turnEventsPlan struct {
	leading []turnEventsDraft
	text    string
	done    turnEventsDraft
}

// publish puts the plan into the turn, numbering each event from the turn's
// NextSeq as it goes. It stops at the first refusal and returns it; the caller
// fails the turn.
func (p turnEventsPlan) publish(t *stream.Turn) error {
	for _, d := range p.leading {
		if err := turnEventsPublishDraft(t, d); err != nil {
			return err
		}
	}
	if err := t.PublishResponse(p.text); err != nil {
		return err
	}
	return turnEventsPublishDraft(t, p.done)
}

func turnEventsPublishDraft(t *stream.Turn, d turnEventsDraft) error {
	return t.Publish(conversation.TurnEvent{Value: d(conversation.Seq(t.NextSeq()))})
}

// buildTurnEvents is the account of one turn as TurnEvents. Everything in it is
// the core's or the host's: the only text is the response the verifier
// accepted (or the deterministic template). A turn that ended in a refusal or a
// failure carries a typed error event before its done event, and a turn that
// returned an error beside its result (a commit may have happened but could not
// be audited) says so, with the receipt first.
func buildTurnEvents(res dialogue.Result, turnErr error) turnEventsPlan {
	turnID := act.TurnID(res.TurnID)
	d := res.Directive
	var p turnEventsPlan
	add := func(f turnEventsDraft) { p.leading = append(p.leading, f) }

	for _, st := range progressStages(res) {
		stage := st
		add(func(seq conversation.Seq) conversation.TurnEventValue {
			return conversation.ProgressEvent{
				TurnID: turnID, Seq: seq, Stage: stage.stage, State: conversation.ProgressEventStateCompleted,
				CapabilityID: stage.capability(d),
			}
		})
	}
	add(func(seq conversation.Seq) conversation.TurnEventValue {
		return conversation.DirectiveEvent{TurnID: turnID, Seq: seq, Directive: d}
	})
	// The citations of a knowledge answer follow the directive that carries them, one event per
	// source, in order, before the response text: the core's own data (the directive's sources),
	// never taken from model output. A turn that did not answer from sources has none.
	for _, c := range dialogue.KnowledgeCitationEvents(res) {
		add(turnEventsDraft(c))
	}

	if pa := res.Prepared; pa != nil {
		add(func(seq conversation.Seq) conversation.TurnEventValue {
			return conversation.PreparedActionEvent{TurnID: turnID, Seq: seq, PreparedAction: *pa}
		})
		if pa.Confirmation.Mode != act.ConfirmationRequirementModeNone && len(pa.Effects) > 0 {
			add(func(seq conversation.Seq) conversation.TurnEventValue {
				return conversation.BlockEvent{TurnID: turnID, Seq: seq, Block: conversation.RichBlock{Value: conversation.ConfirmationPanelBlock{
					PreparationID: pa.PreparationID, Summary: pa.Summary, Effects: pa.Effects, RiskLevel: pa.RiskLevel,
					ExpiresAt: pa.ExpiresAt, Confirmation: pa.Confirmation,
				}}}
			})
		}
	}
	if q := d.NextQuestion; q != nil && len(q.Choices) > 0 &&
		(q.AnswerKind == conversation.DialogueDirectiveQuestionAnswerKindChoice || q.AnswerKind == conversation.DialogueDirectiveQuestionAnswerKindEntity) {
		kind := conversation.ClarificationChoicesBlockAnswerKindChoice
		if q.AnswerKind == conversation.DialogueDirectiveQuestionAnswerKindEntity {
			kind = conversation.ClarificationChoicesBlockAnswerKindEntity
		}
		add(func(seq conversation.Seq) conversation.TurnEventValue {
			return conversation.ClarificationEvent{TurnID: turnID, Seq: seq, Clarification: conversation.ClarificationChoicesBlock{
				Field: q.Field, AnswerKind: kind, AllowFreeText: kind == conversation.ClarificationChoicesBlockAnswerKindChoice,
				Choices: q.Choices,
			}}
		})
	}
	// The receipt comes before the text: a response may claim completion only
	// after a receipt event with status committed.
	if rc := res.Receipt; rc != nil {
		add(func(seq conversation.Seq) conversation.TurnEventValue {
			return conversation.ReceiptEvent{TurnID: turnID, Seq: seq, Receipt: *rc}
		})
	}

	failed := res.Failed() || turnErr != nil
	if failed {
		var e contracts.Error
		if turnErr != nil {
			e = problemFor(turnErr).body
		} else {
			msg := strings.TrimSpace(res.Response.Text)
			if msg == "" {
				msg = "the request could not be completed"
			}
			e = problemFor(contracts.New(res.Code, msg)).body
		}
		add(func(seq conversation.Seq) conversation.TurnEventValue {
			return conversation.ErrorEvent{TurnID: turnID, Seq: seq, Error: common.Error{
				Code: common.ErrorCode(e.Code), Message: e.Message, Retryable: e.Retryable, Details: e.Details,
			}}
		})
	}
	p.text = res.Response.Text

	reason := finishReason(res, failed)
	p.done = func(seq conversation.Seq) conversation.TurnEventValue {
		done := conversation.DoneEvent{TurnID: turnID, Seq: seq, FinishReason: reason}
		if reason == conversation.DoneEventFinishReasonAwaitingConfirmation {
			id := res.Prepared.PreparationID
			done.PreparationID = &id
		}
		return done
	}
	return p
}

// finishReason says why the turn stopped. A failed turn is failed whatever else
// it carries; awaiting_confirmation only for an action that the user can
// confirm here (a step-up action is finished as far as this API goes: the user
// continues in the application's own interface).
func finishReason(res dialogue.Result, failed bool) conversation.DoneEventFinishReason {
	d := res.Directive
	switch {
	case failed:
		return conversation.DoneEventFinishReasonFailed
	case d.Act == conversation.DialogueDirectiveActRequestConfirmation && res.Prepared != nil:
		return conversation.DoneEventFinishReasonAwaitingConfirmation
	case d.Act == conversation.DialogueDirectiveActAcknowledgeCancel:
		return conversation.DoneEventFinishReasonCancelled
	case d.Act == conversation.DialogueDirectiveActAskMissing, d.Act == conversation.DialogueDirectiveActAskInvalid,
		d.Act == conversation.DialogueDirectiveActDisambiguate:
		return conversation.DoneEventFinishReasonAwaitingInput
	}
	return conversation.DoneEventFinishReasonCompleted
}

// stageProgress is one progress event derived from the turn's trace.
type stageProgress struct {
	stage conversation.ProgressEventStage
	tool  bool
}

func (s stageProgress) capability(d conversation.DialogueDirective) *common.CapabilityID {
	if s.tool && d.CapabilityID != nil {
		id := *d.CapabilityID
		return &id
	}
	return nil
}

// progressStages maps the pipeline stages the turn actually ran to the progress
// stages of the event schema, in order, one event per run of equal stages.
func progressStages(res dialogue.Result) []stageProgress {
	var out []stageProgress
	push := func(s stageProgress) {
		if n := len(out); n > 0 && out[n-1].stage == s.stage {
			return
		}
		out = append(out, s)
	}
	for _, name := range res.Trace.Stages {
		switch name {
		case dialogue.StageShortlist, dialogue.StageDecision, dialogue.StageContext, dialogue.StageProposal:
			push(stageProgress{stage: conversation.ProgressEventStageInterpreting})
		case dialogue.StageKnowledge:
			push(stageProgress{stage: conversation.ProgressEventStageRetrieving})
		case dialogue.StageValidate, dialogue.StageResolve:
			push(stageProgress{stage: conversation.ProgressEventStageValidating})
		case dialogue.StageHost:
			push(stageProgress{stage: conversation.ProgressEventStageToolCall, tool: true})
		case dialogue.StageConfirm, dialogue.StageCommit:
			push(stageProgress{stage: conversation.ProgressEventStageCommitting})
		case dialogue.StageVerify:
			for _, c := range res.Trace.ModelCalls {
				if c.Purpose == "response" {
					push(stageProgress{stage: conversation.ProgressEventStageGenerating})
					break
				}
			}
			push(stageProgress{stage: conversation.ProgressEventStageVerifying})
		}
	}
	return out
}

// ---- attachments ----

// AttachModelFiles is the part of the files service a turn needs: what a model may be given of
// the files a user attached (files.Service.ForModel). Every call resolves the references from
// scratch under the verified scope. It is provided separately from FilesService, which a
// tenant's upload and download requests use, so each area is given only what it uses.
type AttachModelFiles interface {
	ForModel(ctx context.Context, scope contracts.Scope, fileIDs []string) ([]files.ModelFile, error)
}

var _ AttachModelFiles = (*files.Service)(nil)

func attachWant(n *Needs) AttachModelFiles {
	f, _ := Want[AttachModelFiles](n)
	return f
}

// attachIDs checks the file ids of a turn and returns them once each, in order. Attachments
// belong to a text turn: a choice, a confirmation or a rejection has no message to attach to.
func (q TurnRequest) attachIDs() ([]string, error) {
	if len(q.Attachments) == 0 {
		return nil, nil
	}
	if q.Text == "" {
		return nil, contracts.New(contracts.ErrValidationFailed, "attachments go with a text message")
	}
	if len(q.Attachments) > dialogue.AttachmentMaxCount {
		return nil, contracts.Newf(contracts.ErrValidationFailed, "a turn carries at most %d attachments", dialogue.AttachmentMaxCount).
			WithDetails("reason", "too_many_attachments").WithDetails("limit", dialogue.AttachmentMaxCount)
	}
	ids := make([]string, 0, len(q.Attachments))
	seen := map[string]bool{}
	for _, id := range q.Attachments {
		if !files.ValidID(id) {
			return nil, contracts.New(contracts.ErrValidationFailed, "an attachment is not a file id")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// attachments resolves the files of a turn and returns them as the attachments of its TurnInput.
// Every reference that does not resolve for the caller, whatever the reason (another
// tenant's, another principal's or another session's file, a deleted one, one that never
// existed, an expired one), is the same scope_violation with no detail, so a reference cannot
// be used to find out which files exist. Anything else the files service refuses (the policy
// does not let a model read that file) is passed on: the caller has shown the file is theirs.
func (h *sessionsHandler) attachments(ctx context.Context, scope contracts.Scope, ids []string) ([]dialogue.Attachment, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if h.attach == nil {
		return nil, contracts.New(contracts.ErrUnavailable, "this deployment does not take attachments in a turn").
			WithDetails("reason", "attachments_unavailable")
	}
	mfs, err := h.attach.ForModel(ctx, scope, ids)
	if err != nil {
		switch contracts.CodeOf(err) {
		case contracts.ErrScopeViolation, contracts.ErrExpired:
			return nil, contracts.New(contracts.ErrScopeViolation, "the file reference does not resolve for this caller")
		}
		return nil, err
	}
	atts := make([]dialogue.Attachment, len(mfs))
	for i, mf := range mfs {
		// The image bytes of a ModelFile are never read here: a prompt message is text.
		atts[i] = dialogue.Attachment{
			Name: mf.Name, MIMEType: mf.MIMEType, Kind: mf.Kind, Text: mf.Text, TextTruncated: mf.TextTruncated, Withheld: mf.Withheld,
		}
	}
	return atts, nil
}
