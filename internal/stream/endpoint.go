package stream

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Options configure the SSE and WebSocket endpoints of a Hub.
type Options struct {
	// Scope returns the verified scope of the request. It is required and is
	// the only source of the caller's identity: a tenant, principal or
	// session in a path, a query or a header does not exist for the stream. A
	// credential never goes in the URL; the host's authentication decides
	// where it travels (the Authorization header, or for a browser WebSocket
	// the Sec-WebSocket-Protocol header or a cookie) and Scope reads it from
	// there. The gateway passes a function that calls identity.RequireScope.
	Scope func(r *http.Request) (contracts.Scope, error)
	// TurnID returns the turn id of the request. Default: the "turn_id" path
	// value.
	TurnID func(r *http.Request) string
	// WriteError writes a refusal that happens before the stream starts. The
	// status is the transport's suggestion. Default: a typed JSON error with
	// that status. The gateway passes its own writer so that refusals look like
	// every other refusal and carry the request id.
	WriteError func(w http.ResponseWriter, r *http.Request, status int, err error)
	// WriteTimeout bounds one frame write. A client that does not read for this
	// long is disconnected. Default 10 seconds.
	WriteTimeout time.Duration
	// Heartbeat is how long the stream may be idle before a keep-alive is sent
	// (an SSE comment, a WebSocket ping). Default 15 seconds.
	Heartbeat time.Duration
	// RetryAfter is the reconnection delay an SSE client is told to use.
	// Default 2 seconds.
	RetryAfter time.Duration
	// AllowedOrigins are the origins (host patterns, as in
	// websocket.AcceptOptions.OriginPatterns) a browser may open a WebSocket
	// from, besides the request's own host. Default: none. A request without an
	// Origin header (not a browser) is not subject to the check; the host's
	// authentication is what protects it.
	AllowedOrigins []string
	// Subprotocols are the WebSocket subprotocols offered. Default
	// "alixai.turn-events.v1".
	Subprotocols []string
}

// WebSocketSubprotocol is the subprotocol of the WebSocket transport.
const WebSocketSubprotocol = "alixai.turn-events.v1"

func (o Options) withDefaults() Options {
	if o.TurnID == nil {
		o.TurnID = func(r *http.Request) string { return r.PathValue("turn_id") }
	}
	if o.WriteError == nil {
		o.WriteError = defaultWriteError
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 10 * time.Second
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 15 * time.Second
	}
	if o.RetryAfter <= 0 {
		o.RetryAfter = 2 * time.Second
	}
	if len(o.Subprotocols) == 0 {
		o.Subprotocols = []string{WebSocketSubprotocol}
	}
	return o
}

// ParseCursor reads the resume cursor of a request: the Last-Event-ID header
// (what an EventSource sends on its own when it reconnects) and the "cursor"
// query parameter (for a fetch-based reader and for a WebSocket, where the
// browser cannot set headers). Both name the sequence number of the last event
// the client received; the stream resumes with the next one. When both are
// present the later one wins, since neither can claim an event the client did
// not get. With neither, the result is FromStart. A value that is not a plain
// sequence number, or a parameter given twice, is ErrCursorMalformed.
func ParseCursor(r *http.Request) (int64, error) {
	after := FromStart
	raws := make([]string, 0, 2)
	if h := r.Header.Values("Last-Event-ID"); len(h) > 1 {
		return 0, ErrCursorMalformed
	} else if len(h) == 1 && h[0] != "" {
		raws = append(raws, h[0])
	}
	if q := r.URL.Query()["cursor"]; len(q) > 1 {
		return 0, ErrCursorMalformed
	} else if len(q) == 1 && q[0] != "" {
		raws = append(raws, q[0])
	}
	for _, raw := range raws {
		n, ok := parseSeq(raw)
		if !ok {
			return 0, ErrCursorMalformed
		}
		after = max(after, n)
	}
	return after, nil
}

// parseSeq reads a sequence number: digits only, at most the schema's maximum.
func parseSeq(s string) (int64, bool) {
	if s == "" || len(s) > 10 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n > MaxSeq {
		return 0, false
	}
	return n, true
}

// attach does everything both transports do before the first byte of a stream:
// the method, the scope, the cursor, the turn and the subscription. On any
// failure it has already written the refusal and returns nil. A cursor that is
// invalid is refused before the turn is looked up, so a malformed request never
// reveals whether a turn exists.
func (h *Hub) attach(w http.ResponseWriter, r *http.Request, o Options) (*Subscription, *Turn) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		o.WriteError(w, r, http.StatusMethodNotAllowed, contracts.New(contracts.ErrValidationFailed, "a stream is read with GET"))
		return nil, nil
	}
	if o.Scope == nil {
		o.WriteError(w, r, http.StatusInternalServerError, contracts.New(contracts.ErrInternal, "stream endpoint has no scope source"))
		return nil, nil
	}
	scope, err := o.Scope(r)
	if err == nil {
		err = scope.Validate()
	}
	if err != nil {
		o.WriteError(w, r, 0, err)
		return nil, nil
	}
	after, err := ParseCursor(r)
	if err != nil {
		o.WriteError(w, r, http.StatusBadRequest, err)
		return nil, nil
	}
	t, err := h.Lookup(scope, o.TurnID(r))
	if err != nil {
		o.WriteError(w, r, statusOf(err), err)
		return nil, nil
	}
	sub, err := t.Subscribe(after)
	if err != nil {
		o.WriteError(w, r, statusOf(err), err)
		return nil, nil
	}
	return sub, t
}

// statusOf is the HTTP status of a refusal before the stream starts.
func statusOf(err error) int {
	switch ReasonOf(err) {
	case ReasonCursorExpired:
		return http.StatusGone
	case ReasonCursorAhead, ReasonCursorMalformed:
		return http.StatusBadRequest
	case ReasonNotVisible:
		return http.StatusForbidden
	case ReasonSubscribers:
		return http.StatusTooManyRequests
	case ReasonAborted, ReasonCapacity, ReasonSlowConsumer:
		return http.StatusServiceUnavailable
	}
	return 0 // let the writer map the code
}

// defaultWriteError writes a typed JSON error (schemas/common/error). Only the
// code, the message, the retryable flag and scalar details leave: the cause of
// an untyped error is never sent.
func defaultWriteError(w http.ResponseWriter, r *http.Request, status int, err error) {
	var body contracts.Error
	var ce *contracts.Error
	switch {
	case err == nil:
		body = *contracts.New(contracts.ErrInternal, "internal error")
	case contracts.CodeOf(err) != contracts.ErrInternal && errors.As(err, &ce) && ce != nil:
		body = contracts.Error{Code: ce.Code, Message: ce.Message, Retryable: ce.Code.Retryable(), Details: scalarDetails(ce.Details)}
	default:
		body = *contracts.New(contracts.ErrInternal, "internal error")
	}
	if status == 0 {
		status = statusForCode(body.Code)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if status == http.StatusUnauthorized {
		h.Set("WWW-Authenticate", "Bearer")
	}
	if body.Retryable {
		h.Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// scalarDetails keeps the scalar details of an error: nothing nested.
func scalarDetails(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch v.(type) {
		case string, bool, int, int64, float64:
			out[k] = v
		}
	}
	return out
}

// statusForCode mirrors the gateway's default mapping for the codes a stream
// endpoint can produce.
func statusForCode(code contracts.ErrCode) int {
	switch code {
	case contracts.ErrUnauthenticated, contracts.ErrExpired, contracts.ErrReplayed, contracts.ErrScopeMissing:
		return http.StatusUnauthorized
	case contracts.ErrScopeViolation, contracts.ErrPermissionDenied, contracts.ErrStepUpRequired, contracts.ErrQuarantined:
		return http.StatusForbidden
	case contracts.ErrValidationFailed:
		return http.StatusBadRequest
	case contracts.ErrConflict:
		return http.StatusConflict
	case contracts.ErrRateLimited:
		return http.StatusTooManyRequests
	case contracts.ErrUnavailable, contracts.ErrDegraded, contracts.ErrResourceExhausted:
		return http.StatusServiceUnavailable
	case contracts.ErrTimeout:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}
