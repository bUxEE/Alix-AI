package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/buxee/alix-ai/internal/history"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The history area of the public API (master plan 14.2: "conversation history,
// search, segment cleanup and export under authorization", and "list paginated
// transcript segments under host authorization"). The contract is
// schemas/api/paths/history.yaml; TestRoutesMatchOpenAPISpec fails on any
// difference in either direction.
//
//	GET    /v1/history/sessions/{session_id}/segments   a page of the transcript
//	DELETE /v1/history/sessions/{session_id}            clear that session's transcript
//	POST   /v1/history/search                           full-text search of the caller's history
//	POST   /v1/history/export                           export the caller's history (JSON lines)
//	POST   /v1/history/cleanup                          delete the workspace's history older than a time
//
// # Identity and authorisation
//
// Every handler starts from identity.RequireScope: the scope is the verified
// context token and nothing the URL or a body says. The host authorises each
// operation by putting its capability in the token (history.read, history.search,
// history.clear, history.export, history.cleanup: internal/history), the check is
// an exact match, and a missing capability is 403 permission_denied before
// anything is read or deleted. A session id in the path must be the token's (as
// everywhere in this API). Reading, searching, clearing and exporting are about
// the caller's OWN history: another principal's rows are never visible, and a
// session that belongs to another principal is scope_violation. Cleanup reaches
// the whole workspace and has a capability of its own. A quarantined application
// serves no history either.
//
// # What the text is
//
// A user row is the user's own text, verbatim, plain text. An assistant row is
// model Markdown, and the transcript keeps it as the engine verified it, while the
// stream hub sanitises a response before the client sees it. So history runs the
// same sanitiser over assistant text on its way out (segments, the snippet of a
// search hit, the export): what a host renders from history, with the renderer it
// uses for live token events, is never more than it would have got live. A text
// the sanitiser refuses is not shown and the row says withheld.
//
// # What a request does not carry
//
// The text of a search is in the body of a POST, never in the URL: a URL is
// logged by every proxy on the way and the words a person searches their
// conversations for are theirs. Responses are never cached. No response carries a
// contracts.Scope, and the audit events a deletion or an export leaves hold ids,
// counts and times, never transcript text.
//
// # The export
//
// The export is one JSON object per line, one per transcript row, then a last line
// {"complete": true, "rows": N}. A status line is sent before the rows, so a
// failure half way cannot change it: the client knows the export is whole only if
// it ends with that line.

func init() { RegisterArea("v1_history", registerV1History) }

// registerV1History needs the history service and the application service (a
// quarantined application serves no traffic).
func registerV1History(rt *Router, d *Deps) {
	n := d.Needs()
	h := &historyHandler{svc: Need[*history.Service](n), apps: Need[ApplicationService](n)}
	if !n.Ready() {
		return
	}
	v1 := rt.V1()
	v1.HandleFunc("GET /history/sessions/{session_id}/segments", h.segments)
	v1.HandleFunc("DELETE /history/sessions/{session_id}", h.clearSession)
	v1.HandleFunc("POST /history/search", h.search)
	v1.HandleFunc("POST /history/export", h.export)
	v1.HandleFunc("POST /history/cleanup", h.cleanup)
}

type historyHandler struct {
	svc  *history.Service
	apps ApplicationService
}

// HistoryTurnBody is one transcript row.
type HistoryTurnBody struct {
	TurnID string `json:"turn_id"`
	// N is the turn number inside the session: the user's row and the assistant's
	// row of one turn share it.
	N    int64  `json:"n"`
	Role string `json:"role"`
	// Act is the directive act of an assistant row.
	Act string `json:"act,omitempty"`
	// Body is a user row's own text, verbatim, or an assistant row's Markdown
	// sanitised exactly as the token events of a live turn are.
	Body      string    `json:"body"`
	Clipped   bool      `json:"clipped"`
	CreatedAt time.Time `json:"created_at"`
	// Withheld is true when Body is empty because the sanitiser refused the text.
	Withheld bool `json:"withheld,omitempty"`
}

// HistorySegmentsBody is a page of a session's transcript: whole turns, oldest
// first.
type HistorySegmentsBody struct {
	SessionID string            `json:"session_id"`
	Turns     []HistoryTurnBody `json:"turns"`
	// NextCursor is the cursor of the next page, or null at the end.
	NextCursor *string `json:"next_cursor"`
	// Total is how many rows the session's transcript holds.
	Total int64 `json:"total"`
}

// HistorySearchRequest is the body of POST /v1/history/search.
type HistorySearchRequest struct {
	// Text is the words to find; every one must occur in the row.
	Text string `json:"text"`
	// SessionID restricts the search to one of the caller's sessions.
	SessionID string `json:"session_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Offset    int    `json:"offset,omitempty"`
}

// HistoryHitBody is one transcript row that matches a search.
type HistoryHitBody struct {
	SessionID string    `json:"session_id"`
	TurnID    string    `json:"turn_id"`
	N         int64     `json:"n"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	// Snippet is a short plain-text excerpt around the match.
	Snippet string `json:"snippet"`
}

// HistorySearchBody is a page of hits, best match first.
type HistorySearchBody struct {
	Hits []HistoryHitBody `json:"hits"`
	// NextOffset is the offset of the next page, or null at the end.
	NextOffset *int `json:"next_offset"`
}

// HistorySessionClearedBody is the body of DELETE /v1/history/sessions/{session_id}.
type HistorySessionClearedBody struct {
	SessionID   string `json:"session_id"`
	RowsDeleted int64  `json:"rows_deleted"`
}

// HistoryCleanupRequest is the body of POST /v1/history/cleanup.
type HistoryCleanupRequest struct {
	// Before is the cutoff: every transcript turn of the workspace created before
	// it is deleted. It cannot be in the future.
	Before time.Time `json:"before"`
}

// HistoryCleanupBody is the body of a cleanup.
type HistoryCleanupBody struct {
	RowsDeleted int64 `json:"rows_deleted"`
}

// HistoryExportRequest is the body of POST /v1/history/export. Every member is
// optional.
type HistoryExportRequest struct {
	SessionID string    `json:"session_id,omitempty"`
	Since     time.Time `json:"since,omitzero"`
	Until     time.Time `json:"until,omitzero"`
}

// HistoryExportTrailer is the last line of a complete export.
type HistoryExportTrailer struct {
	Complete bool `json:"complete"`
	Rows     int  `json:"rows"`
}

// Bounds of the query parameters of the segments route.
const (
	historyMaxCursorDigits = 18
	historyMaxLimitDigits  = 3
)

// historyQueryInt reads an optional decimal query parameter of at most maxDigits
// digits. ok is false (and the refusal written) for a malformed value.
func historyQueryInt(w http.ResponseWriter, r *http.Request, name string, maxDigits int) (int64, bool) {
	vals := r.URL.Query()[name]
	switch len(vals) {
	case 0:
		return 0, true
	case 1:
	default:
		WriteError(w, r, contracts.Newf(contracts.ErrValidationFailed, "the %s parameter is given more than once", name))
		return 0, false
	}
	v := vals[0]
	if v == "" || len(v) > maxDigits {
		WriteError(w, r, contracts.Newf(contracts.ErrValidationFailed, "the %s parameter is not a number", name))
		return 0, false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			WriteError(w, r, contracts.Newf(contracts.ErrValidationFailed, "the %s parameter is not a number", name))
			return 0, false
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		WriteError(w, r, contracts.Newf(contracts.ErrValidationFailed, "the %s parameter is not a number", name))
		return 0, false
	}
	return n, true
}

// scope returns the verified scope after the checks every history route shares:
// the application is active (a quarantined application serves no traffic).
func (h *historyHandler) scope(w http.ResponseWriter, r *http.Request) (contracts.Scope, bool) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return contracts.Scope{}, false
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return contracts.Scope{}, false
	}
	return scope, true
}

func (h *historyHandler) segments(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r)
	if !ok || !sessionPath(w, r, scope) {
		return
	}
	cursor, ok := historyQueryInt(w, r, "cursor", historyMaxCursorDigits)
	if !ok {
		return
	}
	limit, ok := historyQueryInt(w, r, "limit", historyMaxLimitDigits)
	if !ok {
		return
	}
	// A limit of 0 would mean "the default" to the service; the API has no such
	// value, so a present zero is refused.
	if _, given := r.URL.Query()["limit"]; given && limit == 0 {
		WriteError(w, r, contracts.New(contracts.ErrValidationFailed, "the limit must be at least 1"))
		return
	}
	page, err := h.svc.Segments(r.Context(), scope, scope.SessionID, cursor, int(limit))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	body := HistorySegmentsBody{SessionID: page.SessionID, Turns: make([]HistoryTurnBody, 0, len(page.Turns)), Total: page.Total}
	for _, t := range page.Turns {
		body.Turns = append(body.Turns, HistoryTurnBody{
			TurnID: t.TurnID, N: t.N, Role: t.Role, Act: t.Act, Body: t.Body, Clipped: t.Clipped, CreatedAt: t.CreatedAt, Withheld: t.Withheld,
		})
	}
	if page.NextCursor != 0 {
		next := strconv.FormatInt(page.NextCursor, 10)
		body.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *historyHandler) clearSession(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r)
	if !ok || !sessionPath(w, r, scope) {
		return
	}
	res, err := h.svc.ClearSession(r.Context(), scope, scope.SessionID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, HistorySessionClearedBody{SessionID: scope.SessionID, RowsDeleted: res.Rows})
}

func (h *historyHandler) search(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req HistorySearchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	page, err := h.svc.Search(r.Context(), scope, history.SearchQuery{
		Text: req.Text, SessionID: req.SessionID, Limit: req.Limit, Offset: req.Offset,
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	body := HistorySearchBody{Hits: make([]HistoryHitBody, 0, len(page.Hits))}
	for _, hit := range page.Hits {
		body.Hits = append(body.Hits, HistoryHitBody{
			SessionID: hit.SessionID, TurnID: hit.TurnID, N: hit.N, Role: hit.Role, CreatedAt: hit.CreatedAt, Snippet: hit.Snippet,
		})
	}
	if page.NextOffset != 0 {
		next := page.NextOffset
		body.NextOffset = &next
	}
	writeJSON(w, http.StatusOK, body)
}

func (h *historyHandler) cleanup(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req HistoryCleanupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := h.svc.Cleanup(r.Context(), scope, req.Before)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, HistoryCleanupBody{RowsDeleted: res.Rows})
}

// historyExportWriter starts the response on the first byte, so a refusal that
// comes before any row is a normal error response and not a half-written stream.
type historyExportWriter struct {
	w       http.ResponseWriter
	started bool
}

func (e *historyExportWriter) Write(p []byte) (int, error) {
	if !e.started {
		e.started = true
		h := e.w.Header()
		h.Set("Content-Type", "application/x-ndjson")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		e.w.WriteHeader(http.StatusOK)
	}
	return e.w.Write(p)
}

func (h *historyHandler) export(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.scope(w, r)
	if !ok {
		return
	}
	var req HistoryExportRequest
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	extendWriteDeadline(w)
	out := &historyExportWriter{w: w}
	res, err := h.svc.Export(r.Context(), scope, out, history.ExportOptions{SessionID: req.SessionID, Since: req.Since, Until: req.Until})
	if err != nil {
		if !out.started {
			WriteError(w, r, err)
			return
		}
		// Rows are on the wire: the status cannot change. The missing trailer is
		// how the client knows the export is not whole.
		loggerFrom(r.Context()).Error("the history export failed after it started", "code", string(contracts.CodeOf(err)), "rows", res.Rows)
		return
	}
	// A whole export with no rows still starts the response with its trailer.
	enc := json.NewEncoder(out)
	if err := enc.Encode(HistoryExportTrailer{Complete: true, Rows: res.Rows}); err != nil {
		loggerFrom(r.Context()).Error("the history export trailer could not be written", "rows", res.Rows)
	}
}
