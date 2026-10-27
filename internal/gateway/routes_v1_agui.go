package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/buxee/alix-ai/internal/agui"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dialogue"
	ddec "github.com/buxee/alix-ai/internal/dialogue/decision"
	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/internal/version"
	"github.com/buxee/alix-ai/pkg/contracts"
	actgen "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// The AG-UI face of the streaming API (docs/integration/agui.md, ADR 0035, master plan 15.6): the same turn, the same
// authority rules, expressed as AG-UI events by internal/agui. The contract is schemas/api/paths/agui.yaml;
// TestRoutesMatchOpenAPISpec fails on any difference in either direction.
//
//	POST /v1/agui                                           a run: RunAgentInput in, AG-UI events out as server-sent events
//	GET  /v1/agui/capabilities                              the protocol version, the events emitted, the features not claimed
//	GET  /v1/agui/threads/{thread_id}/runs/{run_id}/events  the events of a run again, over SSE or a WebSocket, from a cursor
//	POST /v1/agui/threads/{thread_id}/runs/{run_id}/cancel  stop a run that is still in flight
//
// # Identity
//
// Every handler starts from identity.RequireScope and an active application, exactly like /v1/sessions: the scope is
// the verified context token and nothing a body, a path or a header says. The thread of a run IS the session of the
// token (the host mints the token with session = thread); any other thread is the one scope_violation that
// agui.Intake gives. A context token is single use, so a reconnect needs a fresh one.
//
// # A run
//
// The body is the AG-UI RunAgentInput. It is read as raw bytes (the protocol's input carries members this API ignores,
// so the strict decoding of the other routes does not apply) and agui.Intake turns it into what to run: a user
// message, a choice, a resume (approve or reject a prepared action) or a connect (no turn). The turn runs on the
// dialogue engine exactly as POST /v1/sessions/{id}/turns runs it, its TurnEvents are published into the stream hub
// (so they are validated, their Markdown sanitised, their numbers ordered), and the AG-UI events are mapped from what
// the hub kept. The engine runs a turn to completion before anything exists to send, so the whole run is mapped
// before the first byte is written: a mapping that is refused (a credential-shaped member, the confirmation secret in
// a data member) ends the run with RUN_ERROR and the secret header is not sent.
//
// # Client data and UI tools
//
// What the page hands the agent (readable context, its state, earlier messages, the tools it declares) is untrusted
// data. The route puts it into the context of the engine call (Request.EngineContext), where the engine places it in
// one fenced block of the proposal prompt and offers a declared tool to the model only if the host registered it for
// the application (dialogue.Deps.UITools, the dialogue.UIToolRegistry service the composition root provides). A UI
// action the engine validated (Result.UIAction) is put into the RunContext, so the mapping sends it as one tool call
// after the reply and a reconnect sends the same one; it is an instruction to the client's screen, never to the
// application. The result the client sends back (a tool message for that call) is a run with no turn: it is answered
// with a start, the state and a finish (agui.UIResultRun), needs no history, calls no model and approves nothing, and
// reaches the next turn as one more fenced line of client context.
//
// # The confirmation secret
//
// It is the response header X-AlixAI-Confirmation-Token of the run that prepared an action, sent only when the
// account of that run was shown in full and ended in its interrupt, and it comes back in the same header on the
// resume. It is in no event, no body and no log. The retry of a run (same runId) and every reconnect replay the events
// and never the secret again; a client that lost it declines the preparation or starts a new run.
//
// # Resume, retry and reconnect
//
// A run is identified by (thread, runId). Its events are kept by the stream hub, its RunContext (everything besides
// the TurnEvents the mapping depends on, suggestion catalog included) is kept as JSON beside it in AGUIRuns, and the
// mapping is a pure function of the two: a reconnect maps the buffered TurnEvents again and skips what the client has,
// so it never runs the turn twice. The cursor is the seq of the last TurnEvent the client received. An SSE event
// carries `id: <seq>` only on the LAST AG-UI event of each TurnEvent (SSE keeps the last id across events without
// one), so a connection cut in the middle of a TurnEvent's events resumes at the start of that TurnEvent instead of
// losing its tail. A cursor behind the retained window is 410, one ahead of the stream 400, a run that is unknown,
// foreign or expired is the identical 403 of the turn-events route, and a cursor at the end of a finished run is 204
// on SSE. A POST with a runId that is already recorded replays the recorded run, one still in flight is a conflict.
// A WebSocket message is exactly the data line of the SSE event, one per message; the socket carries no ids, so a
// WebSocket reader that must resume passes the cursor it learned over SSE, or starts again from the beginning.
//
// # Cancel
//
// Cancelling a run in flight cancels the context of the request that runs it: the engine stops, no confirmation is
// presented and nothing is committed (a run that is a resume has already been confirmed by the user, and a commit
// that has begun is not undone). The run's own response then ends with RUN_FINISHED and the cancelled outcome. After
// a run has finished there is nothing to stop: the receipt that exists stands, and a prepared action it left pending
// is declined by a resume with status cancelled.

func init() { RegisterArea("v1_agui", registerV1AGUI) }

// aguiRunsDefaultMax bounds the runs the index holds; the stream hub bounds the events behind them.
const aguiRunsDefaultMax = 512

// aguiWebSocketSubprotocol is the subprotocol of the WebSocket transport of AG-UI events.
const aguiWebSocketSubprotocol = "alixai.agui.v1"

// registerV1AGUI needs exactly what v1_sessions and v1_turnevents need: the session store, the dialogue engine, the
// action service, the application service and the stream hub. The run index (AGUIRuns), the policy gateway (for the
// suggestion catalog), the files service (for attachment previews) and the history service (for a connect run) are
// optional; without the index the area keeps a private one of its own.
func registerV1AGUI(rt *Router, d *Deps) {
	n := d.Needs()
	h := &aguiHandler{
		sessions: Need[*sessions.Store](n),
		engine:   Need[*dialogue.Engine](n),
		actions:  Need[ActionService](n),
		apps:     Need[ApplicationService](n),
		hub:      Need[*stream.Hub](n),
		d:        d,
	}
	if !n.Ready() {
		return
	}
	h.turner = turnerFor(n, h.engine)
	h.sess = &sessionsHandler{attach: attachWant(n)}
	h.policy, _ = Want[*policy.Gateway](n)
	h.uiTools, _ = Want[dialogue.UIToolRegistry](n)
	h.files, _ = Want[FilesService](n)
	h.history, _ = Want[*history.Service](n)
	h.archive, _ = Want[TurnBlocksArchive](n)
	if h.runs, _ = Want[*AGUIRuns](n); h.runs == nil {
		h.runs = NewAGUIRuns(d.Clock)
	}
	attachSetRouteLimit(rt, http.MethodPost, "/v1/agui", func() int64 { return agui.MaxInputBytes + 4<<10 })
	v1 := rt.V1()
	v1.HandleFunc("POST /agui", h.run)
	v1.HandleFunc("GET /agui/capabilities", h.capabilities)
	v1.HandleFunc("GET /agui/threads/{thread_id}/runs/{run_id}/events", h.events)
	v1.HandleFunc("POST /agui/threads/{thread_id}/runs/{run_id}/cancel", h.cancel)
}

type aguiHandler struct {
	sessions *sessions.Store
	engine   *dialogue.Engine
	turner   ddec.Turner // the decision wrapper when provided, else the engine (turnerFor)
	actions  ActionService
	apps     ApplicationService
	hub      *stream.Hub
	// sess resolves the attachments of a message with the code of the sessions route (one scope_violation for every
	// reference that does not resolve).
	sess   *sessionsHandler
	policy *policy.Gateway
	// uiTools is the host's allow-list of UI tools, the same value the engine was built with. It is read only to say
	// honestly in the capabilities document whether a client tool can be offered; the engine does the enforcing.
	uiTools dialogue.UIToolRegistry
	files   FilesService
	history *history.Service
	archive TurnBlocksArchive
	runs    *AGUIRuns
	d       *Deps
}

// aguiErrNotResolved is the one refusal for a thread that is not the session of the token. It is built the same way as
// agui.Intake's, and a test compares the two byte for byte.
func aguiErrNotResolved() error {
	return contracts.New(contracts.ErrScopeViolation, "the thread or the interrupt does not resolve for this caller")
}

// ---- the run index ----

// aguiRunKey identifies a run: the whole verified scope that started it (the thread is its session) and the run id.
type aguiRunKey struct {
	deployment, application, tenant, workspace, principal, thread, run string
}

func aguiKeyOf(scope contracts.Scope, runID string) aguiRunKey {
	return aguiRunKey{
		deployment: scope.DeploymentID, application: scope.ApplicationID, tenant: scope.TenantID,
		workspace: scope.WorkspaceID, principal: scope.PrincipalID, thread: scope.SessionID, run: runID,
	}
}

// AGUIRuns is the index of the AG-UI runs of this process: which turn of the stream hub a run is, the whole
// agui.RunContext of the run as JSON (what the mapping needs besides the TurnEvents, so a reconnect maps them again
// to the same bytes), whether the run is still in flight, and how to cancel it. It lives exactly as long as the hub
// keeps the events (the retention is passed in when a run finishes), is bounded, and is keyed by the whole scope, so a
// run is visible to the principal and the session that made it and to no one else.
//
// The values the mapping must never send (the confirmation secret) are kept beside the JSON, in memory only; they are
// not part of the persisted context.
type AGUIRuns struct {
	clk clock.Clock
	max int

	mu    sync.Mutex
	runs  map[aguiRunKey]*aguiRun
	order int64
}

type aguiRun struct {
	key     aguiRunKey
	order   int64
	running bool
	cancel  context.CancelFunc
	// cancelled is set when the cancel route stopped the run while it was in flight.
	cancelled bool

	turnID      string
	contextJSON []byte
	withhold    []string
	expires     time.Time
}

// aguiRunInfo is a copy of what the index knows of a run.
type aguiRunInfo struct {
	running     bool
	turnID      string
	contextJSON []byte
	withhold    []string
}

// NewAGUIRuns returns an empty run index on clk (nil is the real clock).
func NewAGUIRuns(clk clock.Clock) *AGUIRuns {
	if clk == nil {
		clk = clock.Real{}
	}
	return &AGUIRuns{clk: clk, max: aguiRunsDefaultMax, runs: map[aguiRunKey]*aguiRun{}}
}

func (a *AGUIRuns) sweepLocked(now time.Time) {
	for k, r := range a.runs {
		if !r.running && !now.Before(r.expires) {
			delete(a.runs, k)
		}
	}
}

func (r *aguiRun) info() aguiRunInfo {
	return aguiRunInfo{running: r.running, turnID: r.turnID, contextJSON: slices.Clone(r.contextJSON), withhold: slices.Clone(r.withhold)}
}

// begin reserves a run. The reservation is atomic: of two requests with the same run id exactly one gets the run, and
// the other gets what is recorded (a run that is running or finished), so one run id never starts two turns.
func (a *AGUIRuns) begin(scope contracts.Scope, runID string, cancel context.CancelFunc) (*aguiRun, *aguiRunInfo, error) {
	key := aguiKeyOf(scope, runID)
	now := a.clk.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(now)
	if prior, ok := a.runs[key]; ok {
		info := prior.info()
		return nil, &info, nil
	}
	for len(a.runs) >= a.max {
		var oldest *aguiRun
		for _, r := range a.runs {
			if !r.running && (oldest == nil || r.order < oldest.order) {
				oldest = r
			}
		}
		if oldest == nil {
			return nil, nil, stream.ErrCapacity
		}
		delete(a.runs, oldest.key)
	}
	a.order++
	r := &aguiRun{key: key, order: a.order, running: true, cancel: cancel}
	a.runs[key] = r
	return r, nil, nil
}

// finish records a run that produced a turn in the hub.
func (a *AGUIRuns) finish(r *aguiRun, turnID string, contextJSON []byte, withhold []string, retention time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r.running, r.cancel = false, nil
	r.turnID, r.contextJSON, r.withhold = turnID, contextJSON, withhold
	r.expires = a.clk.Now().Add(retention)
}

// Record registers a run whose events were published into the stream hub by something other than POST /v1/agui (a
// composition root's own producer, a test): it can then be read again, over SSE or a WebSocket, and cancelled like any
// run of the route. The RunContext is persisted as JSON exactly as the route persists its own, and its Withhold values
// are kept in memory beside it. The run id must be new in the scope.
func (a *AGUIRuns) Record(scope contracts.Scope, rc agui.RunContext, turnID string, retention time.Duration) error {
	raw, err := json.Marshal(rc)
	if err != nil {
		return contracts.Wrap(contracts.ErrInternal, err, "the run context cannot be kept")
	}
	run, prior, err := a.begin(scope, rc.RunID, nil)
	if err != nil {
		return err
	}
	if prior != nil {
		return contracts.New(contracts.ErrConflict, "the run id is already in use")
	}
	a.finish(run, turnID, raw, slices.Clone(rc.Withhold), retention)
	return nil
}

// release forgets a run that produced no turn: its id may be used again.
func (a *AGUIRuns) release(r *aguiRun) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runs[r.key] == r {
		delete(a.runs, r.key)
	}
}

func (a *AGUIRuns) wasCancelled(r *aguiRun) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return r.cancelled
}

// lookup returns a recorded run that has not expired.
func (a *AGUIRuns) lookup(scope contracts.Scope, runID string) (aguiRunInfo, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(a.clk.Now())
	r, ok := a.runs[aguiKeyOf(scope, runID)]
	if !ok {
		return aguiRunInfo{}, false
	}
	return r.info(), true
}

// requestCancel stops a run that is in flight. It reports "cancelling" for a run it signalled and "finished" for a run
// that has already ended; ok is false for a run that is not this caller's or does not exist.
func (a *AGUIRuns) requestCancel(scope contracts.Scope, runID string) (status string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sweepLocked(a.clk.Now())
	r, found := a.runs[aguiKeyOf(scope, runID)]
	if !found {
		return "", false
	}
	if !r.running {
		return "finished", true
	}
	r.cancelled = true
	if r.cancel != nil {
		r.cancel()
	}
	return "cancelling", true
}

// ---- POST /v1/agui ----

func aguiReadBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		WriteErrorStatus(w, r, http.StatusUnsupportedMediaType, contracts.New(contracts.ErrValidationFailed, "Content-Type must be application/json"))
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, agui.MaxInputBytes+1))
	if err != nil {
		writeDecodeError(w, r, err)
		return nil, false
	}
	return body, true
}

func (h *aguiHandler) run(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	pkg, ok := activePackage(w, r, h.apps, scope)
	if !ok {
		return
	}
	body, ok := aguiReadBody(w, r)
	if !ok {
		return
	}
	in, err := agui.ParseRunAgentInput(body)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if in.ThreadID != scope.SessionID {
		WriteError(w, r, aguiErrNotResolved())
		return
	}

	// One run id, one turn: the reservation is atomic, and a recorded run is replayed, never run again.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	run, prior, err := h.runs.begin(scope, in.RunID, cancel)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if prior != nil {
		if prior.running {
			WriteError(w, r, contracts.New(contracts.ErrConflict, "the run is already in progress").WithDetails("reason", "run_in_progress"))
			return
		}
		h.serveRecorded(w, r, scope, *prior, stream.FromStart, false)
		return
	}
	recorded := false
	defer func() {
		if !recorded {
			h.runs.release(run)
		}
	}()

	secret := r.Header.Get(HeaderConfirmationToken)
	req, err := agui.Intake(ctx, in, secret, agui.Env{
		Preparations: h.actions,
		IsCapability: func(id string) bool { _, ok := pkg.Capability(id); return ok },
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}

	// The state the client holds when the run begins. Opening the session is idempotent, and an ended session is Gone.
	sess, err := h.sessions.Open(ctx, scope)
	if err != nil {
		sessionError(w, r, err)
		return
	}
	rc := agui.RunContext{
		ThreadID: req.ThreadID, RunID: req.RunID, Kind: req.Kind, Language: req.Input.Language,
		StateMode: req.StateMode, Resume: req.Resume,
	}
	before := agui.ProjectState(sess.State)
	rc.StateBefore = &before

	if req.UIResult {
		h.uiResult(w, r, rc)
		return
	}
	if req.Kind == agui.RunKindConnect {
		h.connect(w, r, ctx, scope, rc)
		return
	}

	// The files of the message are resolved before the engine runs, under the verified scope, with the sessions route's
	// one answer for every reference that does not resolve. Their references (size, type, preview) go to the client as
	// the attachments activity.
	req.Input.Attachments, err = h.sess.attachments(ctx, scope, req.AttachmentIDs)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if h.files != nil {
		for _, id := range req.AttachmentIDs {
			ref, err := h.files.Stat(ctx, scope, id)
			if err != nil {
				WriteError(w, r, contracts.New(contracts.ErrScopeViolation, "the file reference does not resolve for this caller"))
				return
			}
			rc.Attachments = append(rc.Attachments, ref)
		}
	}

	extendWriteDeadline(w)
	// The client's context and the tools it declared reach the engine as data, on the context of the call and not in the
	// turn input: the engine fences the first and offers a tool only if the host registered it.
	res, terr := h.turner.Turn(req.EngineContext(ctx), scope, req.Input)
	// A run the cancel route stopped ends as cancelled, whether or not the engine had already finished: unless a commit
	// happened (the user had confirmed), nothing it prepared is presented, so no secret leaves and nothing can be
	// confirmed from it. Its turn is not published and its run id is free again.
	if h.runs.wasCancelled(run) && (res.TurnID == "" || res.Receipt == nil) {
		h.writeCancelled(w, r, rc.ThreadID, rc.RunID)
		return
	}
	if res.TurnID == "" {
		if r.Context().Err() == nil { // else the client went away: there is no one to answer
			if terr == nil {
				terr = contracts.New(contracts.ErrInternal, "the turn produced no result")
			}
			sessionError(w, r, terr)
		}
		return
	}
	log := loggerFrom(r.Context())

	turn, err := h.hub.Lookup(scope, res.TurnID)
	if err != nil {
		turn, err = h.hub.Begin(scope, res.TurnID)
	}
	if err != nil {
		log.Error("the run's turn ran but its event stream could not be opened", "turn_id", res.TurnID,
			"reason", string(stream.ReasonOf(err)), "code", string(contracts.CodeOf(err)))
		WriteError(w, r, err)
		return
	}
	turn.SetClientCapabilities(req.ClientCapabilities)
	plan := buildTurnEvents(res, terr)
	perr := plan.publish(turn, res.Response.Streamed)
	if perr != nil {
		log.Error("a turn event of the run was refused; the turn was failed", "turn_id", res.TurnID,
			"reason", string(stream.ReasonOf(perr)), "code", string(contracts.CodeOf(perr)))
		if ferr := turn.Fail(perr); ferr != nil {
			log.Error("the turn could not be failed cleanly", "turn_id", res.TurnID, "reason", string(stream.ReasonOf(ferr)))
		}
	} else {
		archiveTurnBlocks(ctx, h.archive, scope, res.TurnID, plan.export, log)
	}
	frames, err := turnEventsFrames(ctx, turn)
	if err != nil {
		log.Error("the events of the run could not be read back", "turn_id", res.TurnID,
			"reason", string(stream.ReasonOf(err)), "code", string(contracts.CodeOf(err)))
		WriteError(w, r, contracts.New(contracts.ErrInternal, "the run events could not be delivered"))
		return
	}

	// What the mapping depends on besides the TurnEvents is persisted as JSON, and the mapping runs from that JSON and the
	// values to withhold, through the one path every reconnect takes (aguiMapRun): what a reconnect maps is exactly what
	// this run mapped, and a member that does not survive the round trip shows in the first stream and not only in a later one.
	if after, err := h.sessions.Open(ctx, scope); err == nil {
		st := agui.ProjectState(after.State)
		rc.StateAfter = &st
	} else {
		log.Warn("the session state after the run could not be read; the run sends no closing state delta", "code", string(contracts.CodeOf(err)))
	}
	if rc.Language == "" {
		rc.Language = string(res.Language)
	}
	rc.Catalog = h.catalog(ctx, scope, pkg, res.Directive)
	rc.UIAction = aguiUIActionOf(req, res)
	ctxJSON, err := json.Marshal(rc)
	if err != nil {
		WriteError(w, r, contracts.New(contracts.ErrInternal, "the run context could not be kept"))
		return
	}
	var withhold []string
	if !res.Confirmation.IsZero() {
		withhold = append(withhold, res.Confirmation.Reveal())
	}
	if secret != "" {
		withhold = append(withhold, secret)
	}

	events, mapErr := aguiMapRun(ctxJSON, withhold, frames)
	if len(events) == 0 {
		WriteError(w, r, contracts.New(contracts.ErrInternal, "the run events could not be delivered"))
		return
	}
	if mapErr != nil {
		log.Error("an event of the run was withheld and the run was failed", "turn_id", res.TurnID, "code", string(contracts.CodeOf(mapErr)))
	}
	grant := aguiGrant(res, perr, mapErr, events)
	h.runs.finish(run, res.TurnID, ctxJSON, withhold, h.hub.Config().Retention)
	recorded = true

	if err := aguiWriteSSE(w, events, stream.FromStart, true, grant); err != nil {
		log.Warn("the run's event stream was interrupted", "turn_id", res.TurnID)
	}
}

// aguiGrant is the confirmation secret the response may carry: the one of a run whose account was shown in full. Every
// condition fails closed: a refused publish, a refused mapping, a turn that prepared nothing, or a stream that did not
// end in the interrupt that asks for the confirmation leaves the header out, because the user would be confirming
// something the stream did not show.
func aguiGrant(res dialogue.Result, publishErr, mapErr error, events []agui.Emitted) string {
	if publishErr != nil || mapErr != nil || res.Prepared == nil || res.Confirmation.IsZero() || len(events) == 0 {
		return ""
	}
	fin, ok := events[len(events)-1].Event.(agui.RunFinishedEvent)
	if !ok || fin.Outcome == nil || fin.Outcome.Type != "interrupt" {
		return ""
	}
	for _, it := range fin.Outcome.Interrupts {
		if it.ID == string(res.Prepared.PreparationID) {
			return res.Confirmation.Reveal()
		}
	}
	return ""
}

// catalog is the suggestion catalog of the run: the title and the examples of the capabilities the policy gateway
// exposes to the caller and that the turn's directive names as permitted next actions. Examples are display strings;
// nothing routes on them.
func (h *aguiHandler) catalog(ctx context.Context, scope contracts.Scope, pkg capabilities.Package, d conversation.DialogueDirective) []agui.CapabilityHint {
	if h.policy == nil {
		return nil
	}
	named := map[string]bool{}
	for _, a := range d.PermittedNextActions {
		if a.Action == actgen.PermittedNextActionActionInvokeCapability && a.CapabilityID != nil {
			named[string(*a.CapabilityID)] = true
		}
	}
	if len(named) == 0 {
		return nil
	}
	exp := h.policy.Expose(ctx, policy.ExposeRequest{Scope: scope, Package: pkg, Binding: policy.BindingOf(scope)})
	var out []agui.CapabilityHint
	for _, dec := range exp.Visible {
		id := dec.Record.CapabilityID
		if !named[id] {
			continue
		}
		hint := agui.CapabilityHint{ID: id}
		if spec, ok := pkg.Spec(id); ok {
			hint.Title = string(spec.Title)
			for _, ex := range spec.Examples {
				if len(hint.Examples) == 4 {
					break
				}
				hint.Examples = append(hint.Examples, agui.ExampleHint{Language: string(ex.Language), Utterance: ex.Utterance})
			}
		}
		out = append(out, hint)
	}
	return out
}

// aguiMapRun maps the buffered TurnEvents of a run from its persisted RunContext (JSON) and the values that must never be
// sent (the confirmation secrets, kept in memory beside the JSON). It is the only way a run is mapped, the first time and
// on every reconnect, so the secret guard is the same in both.
func aguiMapRun(contextJSON []byte, withhold []string, frames []stream.Frame) ([]agui.Emitted, error) {
	var rc agui.RunContext
	if err := json.Unmarshal(contextJSON, &rc); err != nil {
		return nil, contracts.New(contracts.ErrInternal, "the run context does not decode")
	}
	rc.Withhold = withhold
	return aguiMapFrames(rc, frames)
}

// aguiMapFrames maps the buffered TurnEvents of a run to AG-UI events. An error means an event had to be withheld: the
// events returned with it end the run with RUN_ERROR and nothing follows.
func aguiMapFrames(rc agui.RunContext, frames []stream.Frame) ([]agui.Emitted, error) {
	m, err := agui.NewMapper(rc)
	if err != nil {
		return nil, err
	}
	var out []agui.Emitted
	for _, f := range frames {
		var ev conversation.TurnEvent
		if err := json.Unmarshal(f.Data, &ev); err != nil {
			out = append(out, m.Abort(f.Seq, "internal", "the event stream was stopped")...)
			return out, contracts.New(contracts.ErrInternal, "a buffered turn event does not decode")
		}
		em, err := m.Map(ev)
		out = append(out, em...)
		if err != nil {
			return out, err
		}
	}
	if !m.Finished() && len(frames) > 0 {
		out = append(out, m.Abort(frames[len(frames)-1].Seq, "aborted", "the run ended without its closing event")...)
	}
	return out, nil
}

// aguiWriteSSE sends the events as server-sent events: `data:` is the AG-UI event on one line, and `id:` is the seq of
// the TurnEvent the event was produced for, on the last event of each TurnEvent only (see the package comment above).
// Events with a seq up to the cursor are skipped: the client has them.
func aguiWriteSSE(w http.ResponseWriter, events []agui.Emitted, cursor int64, withIDs bool, grant string) error {
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Accel-Buffering", "no")
	if grant != "" {
		hd.Set(HeaderConfirmationToken, grant)
	}
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	for i, e := range events {
		if withIDs && e.Seq <= cursor {
			continue
		}
		if bytes.ContainsAny(e.Data, "\r\n") {
			return errors.New("an encoded event is not one line")
		}
		id := ""
		if withIDs && (i == len(events)-1 || events[i+1].Seq != e.Seq) {
			id = fmt.Sprintf("id: %d\n", e.Seq)
		}
		if _, err := fmt.Fprintf(w, "%sdata: %s\n\n", id, e.Data); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
	}
	return nil
}

// writeCancelled ends a run that was cancelled before its turn existed: RUN_STARTED, then RUN_FINISHED with the
// cancelled outcome.
func (h *aguiHandler) writeCancelled(w http.ResponseWriter, r *http.Request, threadID, runID string) {
	var events []agui.Emitted
	for _, ev := range []agui.Event{
		agui.NewRunStarted(threadID, runID),
		agui.NewRunFinished(threadID, runID, map[string]any{"cancelled": true}, &agui.Outcome{Type: "cancelled"}),
	} {
		data, err := agui.Encode(ev)
		if err != nil {
			WriteError(w, r, contracts.New(contracts.ErrInternal, "the run could not be ended"))
			return
		}
		events = append(events, agui.Emitted{Event: ev, Data: data})
	}
	_ = aguiWriteSSE(w, events, stream.FromStart, false, "")
}

// connect answers a run with no message: the history of the thread and its state, and no turn.
func (h *aguiHandler) connect(w http.ResponseWriter, r *http.Request, ctx context.Context, scope contracts.Scope, rc agui.RunContext) {
	if h.history == nil {
		WriteError(w, r, contracts.New(contracts.ErrUnavailable, "this deployment has no history to connect to").WithDetails("reason", "history_unavailable"))
		return
	}
	var turns []agui.HistoryTurn
	cursor := int64(0)
	for range 50 {
		page, err := h.history.Segments(ctx, scope, scope.SessionID, cursor, history.MaxPage)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		for _, t := range page.Turns {
			if t.Withheld || t.Body == "" {
				continue
			}
			turns = append(turns, agui.HistoryTurn{TurnID: t.TurnID, Role: t.Role, Text: t.Body})
		}
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
	}
	events, err := agui.Connect(rc, turns)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	_ = aguiWriteSSE(w, events, stream.FromStart, false, "")
}

// aguiUIActionOf is the UI action the run sends to the client's screen: the one the engine validated (declared by the
// client for this run, registered by the host, arguments valid against the declared schema), and only if its name is
// still one this request declared. The engine's check is the authority; this one only keeps a result that could never
// be the answer to this request from reaching the mapping.
func aguiUIActionOf(req agui.Request, res dialogue.Result) *agui.UIActionCall {
	ua := res.UIAction
	if ua == nil || !slices.Contains(req.ClientTools, ua.Name) {
		return nil
	}
	return &agui.UIActionCall{Name: ua.Name, Arguments: append(json.RawMessage(nil), ua.Arguments...)}
}

// uiResult answers the run a client starts to hand back the result of a UI tool call: no turn, no model, no history.
// The result is data; it travels with the thread's messages and reaches the next turn as a line of client context.
func (h *aguiHandler) uiResult(w http.ResponseWriter, r *http.Request, rc agui.RunContext) {
	events, err := agui.UIResultRun(rc)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	_ = aguiWriteSSE(w, events, stream.FromStart, false, "")
}

// ---- GET /v1/agui/threads/{thread_id}/runs/{run_id}/events ----

// aguiStreamStatus is the HTTP status of a refusal before the stream starts, the stream hub's own mapping.
func aguiStreamStatus(err error) int {
	switch stream.ReasonOf(err) {
	case stream.ReasonCursorExpired:
		return http.StatusGone
	case stream.ReasonCursorAhead, stream.ReasonCursorMalformed:
		return http.StatusBadRequest
	case stream.ReasonNotVisible:
		return http.StatusForbidden
	case stream.ReasonSubscribers:
		return http.StatusTooManyRequests
	case stream.ReasonAborted, stream.ReasonCapacity, stream.ReasonSlowConsumer:
		return http.StatusServiceUnavailable
	}
	return 0
}

func aguiStreamError(w http.ResponseWriter, r *http.Request, err error) {
	WriteErrorStatus(w, r, aguiStreamStatus(err), err)
}

// aguiRunPath checks the path of a run route: the thread is the token's session and the run id is well-formed. A run
// id that cannot exist is not visible, like one that does not.
func aguiRunPath(w http.ResponseWriter, r *http.Request, scope contracts.Scope) (string, bool) {
	if r.PathValue("thread_id") != scope.SessionID {
		WriteError(w, r, aguiErrNotResolved())
		return "", false
	}
	runID := r.PathValue("run_id")
	if !aguiValidRunID(runID) {
		aguiStreamError(w, r, stream.ErrNotVisible)
		return "", false
	}
	return runID, true
}

// aguiValidRunID accepts the run ids the run request accepts (internal/agui): letters, digits, '_', '-', '.' and ':', 1 to
// 128 of them. A run that was started can always be read again and cancelled.
func aguiValidRunID(s string) bool {
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

func (h *aguiHandler) events(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	// Nothing about the run is looked at before these: they depend on the token and the application, never on the id.
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	runID, ok := aguiRunPath(w, r, scope)
	if !ok {
		return
	}
	cursor, err := stream.ParseCursor(r)
	if err != nil {
		aguiStreamError(w, r, err)
		return
	}
	info, found := h.runs.lookup(scope, runID)
	if !found {
		aguiStreamError(w, r, stream.ErrNotVisible)
		return
	}
	if info.running {
		WriteError(w, r, contracts.New(contracts.ErrConflict, "the run has not produced its events yet").WithDetails("reason", "run_in_progress"))
		return
	}
	if aguiWantsWebSocket(r) {
		h.serveWebSocket(w, r, scope, info, cursor)
		return
	}
	h.serveRecorded(w, r, scope, info, cursor, true)
}

func aguiWantsWebSocket(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}

// aguiReplay maps the buffered TurnEvents of a recorded run again, from the first one, and says whether the cursor was
// already at its end. The cursor is validated by the hub's own rules.
func (h *aguiHandler) aguiReplay(ctx context.Context, scope contracts.Scope, info aguiRunInfo, cursor int64) (events []agui.Emitted, complete bool, err error) {
	turn, err := h.hub.Lookup(scope, info.turnID)
	if err != nil {
		return nil, false, err
	}
	sub, err := turn.Subscribe(cursor)
	if err != nil {
		return nil, false, err
	}
	complete = sub.Complete()
	sub.Close()
	frames, err := turnEventsFrames(ctx, turn)
	if err != nil {
		return nil, false, err
	}
	events, mapErr := aguiMapRun(info.contextJSON, info.withhold, frames)
	if len(events) == 0 {
		return nil, false, contracts.New(contracts.ErrInternal, "the run events could not be delivered")
	}
	if mapErr != nil {
		loggerFrom(ctx).Error("an event of a replayed run was withheld", "turn_id", info.turnID, "code", string(contracts.CodeOf(mapErr)))
	}
	return events, complete, nil
}

// serveRecorded sends a recorded run as server-sent events. The secret is never sent again.
func (h *aguiHandler) serveRecorded(w http.ResponseWriter, r *http.Request, scope contracts.Scope, info aguiRunInfo, cursor int64, get bool) {
	events, complete, err := h.aguiReplay(r.Context(), scope, info, cursor)
	if err != nil {
		aguiStreamError(w, r, err)
		return
	}
	if get && complete {
		// The client has everything: 204 is the signal to stop reconnecting.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := aguiWriteSSE(w, events, cursor, true, ""); err != nil {
		loggerFrom(r.Context()).Warn("a replayed run stream was interrupted", "turn_id", info.turnID)
	}
}

// serveWebSocket sends a recorded run over a WebSocket: one text message per event, exactly the SSE data line. The
// transport is server to client only (a message from the client closes the connection), every refusal that can happen
// before the upgrade is an HTTP status, and a browser's Origin must be the request's own host.
func (h *aguiHandler) serveWebSocket(w http.ResponseWriter, r *http.Request, scope contracts.Scope, info aguiRunInfo, cursor int64) {
	events, complete, err := h.aguiReplay(r.Context(), scope, info, cursor)
	if err != nil {
		aguiStreamError(w, r, err)
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{aguiWebSocketSubprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept has written the refusal
	}
	c.SetReadLimit(256)
	if complete {
		_ = c.Close(websocket.StatusNormalClosure, "complete")
		return
	}
	ctx := c.CloseRead(r.Context())
	for _, e := range events {
		if e.Seq <= cursor {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := c.Write(wctx, websocket.MessageText, e.Data)
		cancel()
		if err != nil {
			return // the library has closed the connection
		}
	}
	_ = c.Close(websocket.StatusNormalClosure, "done")
}

// ---- POST /v1/agui/threads/{thread_id}/runs/{run_id}/cancel ----

// AGUICancelBody is the body of the cancel route.
type AGUICancelBody struct {
	ThreadID string `json:"thread_id"`
	RunID    string `json:"run_id"`
	// Status is cancelling (the run was in flight and has been told to stop; its own response ends with the cancelled
	// outcome) or finished (the run had already ended: its receipt, if it has one, stands).
	Status string `json:"status"`
}

func (h *aguiHandler) cancel(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	runID, ok := aguiRunPath(w, r, scope)
	if !ok {
		return
	}
	status, found := h.runs.requestCancel(scope, runID)
	if !found {
		aguiStreamError(w, r, stream.ErrNotVisible)
		return
	}
	writeJSON(w, http.StatusOK, AGUICancelBody{ThreadID: scope.SessionID, RunID: runID, Status: status})
}

// ---- GET /v1/agui/capabilities ----

// AGUIPendingFeature is a feature the endpoint accepts input for and does not yet act on. No feature is pending any more
// (client context and UI tools act since M3-34); the type stays because the published contract of the capabilities
// document still has the member.
type AGUIPendingFeature struct {
	Feature   string `json:"feature"`
	Behaviour string `json:"behaviour"`
	// SupportedBy is the task of the plan that makes the feature work.
	SupportedBy string `json:"supported_by"`
}

// AGUICapabilitiesBody is the body of GET /v1/agui/capabilities: the capabilities document of internal/agui.
type AGUICapabilitiesBody struct {
	agui.CapabilitiesDocument
	// Pending is always the empty list: the member is kept only because the published contract requires it.
	Pending []AGUIPendingFeature `json:"pending"`
}

// aguiWiring says what this deployment has wired for the caller's application. Client context always is: the route
// puts it into the context of every engine call. A client tool can be called only if the host registered one for the
// application that the model could be offered (a usable name: well-formed, not reserved, not a capability's). No
// registry, no name or a registry that fails is "not wired": the document never claims what it cannot back.
func (h *aguiHandler) aguiWiring(ctx context.Context, scope contracts.Scope, pkg capabilities.Package) agui.Wiring {
	w := agui.Wiring{ClientContext: true}
	if h.uiTools == nil {
		return w
	}
	names, err := h.uiTools.UITools(ctx, scope.ApplicationID)
	if err != nil {
		return w
	}
	isCapability := func(id string) bool { _, ok := pkg.Capability(id); return ok }
	for _, n := range names {
		if agui.UIToolNameAllowed(n, isCapability) {
			w.ClientTools = true
			break
		}
	}
	return w
}

func (h *aguiHandler) capabilities(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	pkg, ok := activePackage(w, r, h.apps, scope)
	if !ok {
		return
	}
	doc := agui.CapabilitiesFor(version.Get().Version, h.aguiWiring(r.Context(), scope, pkg))
	writeJSON(w, http.StatusOK, AGUICapabilitiesBody{CapabilitiesDocument: doc, Pending: []AGUIPendingFeature{}})
}
