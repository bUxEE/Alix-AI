package gateway

import (
	"net/http"
	"strings"

	"github.com/buxee/alix-ai/internal/identity"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// GET /v1/sessions/{session_id}/turns/{turn_id}/events is how a client reads the
// events of a turn again: it resumes a stream it lost, from the cursor of the
// last event it received, without the turn being run twice. It is the stream hub
// of internal/stream (see that package's documentation for the wire protocol)
// mounted on a /v1 route; the hub holds the events the POST turn handler
// published (routes_v1_sessions.go), so a resume only reads.
//
// # Transport
//
// One route serves both transports: a request that asks to upgrade to a
// WebSocket (Upgrade: websocket) is answered by Hub.WebSocketHandler and every
// other request by Hub.SSEHandler (server-sent events: id is the event's seq,
// event its type, data the TurnEvent on one line). The WebSocket is server to
// client only, and a browser's Origin must be the request's own host: the
// gateway sends no CORS headers and configures no other origin.
//
// # Resume
//
// The cursor is the seq of the last event the client received: the Last-Event-ID
// header or the cursor query parameter (the later of the two wins). A malformed
// cursor, or one ahead of the stream, is 400; one behind the retained window is
// 410 (never a stream with a gap); one at the end of a finished turn is 204 on
// SSE, the signal to stop reconnecting.
//
// # Identity
//
// The scope is the verified context token (identity.RequireScope) and nothing
// the URL says: the turn is looked up under the token's whole scope, so a turn
// of another principal, a turn that does not exist and a turn whose retention
// ran out are one and the same 403 (scope_violation, turn_not_visible), and the
// route is no oracle for turn ids. A session id in the path must be the token's.
// The context token is single use, so every reconnect needs a fresh one: a
// browser EventSource, which reconnects on its own and cannot set the
// Authorization header, cannot be used under header authentication. A client
// reads the stream with a fetch-based reader (the SDK's) and mints a token per
// open, or uses the WebSocket from a non-browser client.
//
// The confirmation secret of a prepared action is not in any event and is not
// sent here: it is the response of the turn that prepared the action, once.

func init() { RegisterArea("v1_turnevents", registerV1TurnEvents) }

// registerV1TurnEvents needs the stream hub and the application service (a
// quarantined application serves no traffic, a stream included).
func registerV1TurnEvents(rt *Router, d *Deps) {
	n := d.Needs()
	hub := Need[*stream.Hub](n)
	apps := Need[ApplicationService](n)
	if !n.Ready() {
		return
	}
	// The hub names the status of a refusal it knows (410 for an expired cursor:
	// the code alone, expired, would be a 401) and passes 0 for the rest, which is
	// the gateway's own mapping: WriteErrorStatus is both. No origin is allowed
	// besides the request's own host: the gateway is not a CORS surface (SEC-092),
	// and a host application that wants a browser page on another origin to
	// connect puts that origin in front of the gateway.
	opts := stream.Options{
		Scope:      func(r *http.Request) (contracts.Scope, error) { return identity.RequireScope(r.Context()) },
		WriteError: WriteErrorStatus,
	}
	h := &turnEventsHandler{apps: apps, sse: hub.SSEHandler(opts), ws: hub.WebSocketHandler(opts)}
	rt.V1().HandleFunc("GET /sessions/{session_id}/turns/{turn_id}/events", h.serve)
}

type turnEventsHandler struct {
	apps ApplicationService
	sse  http.Handler
	ws   http.Handler
}

func (h *turnEventsHandler) serve(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok || !sessionPath(w, r, scope) {
		return
	}
	// Nothing about the turn is looked at before these: they depend on the
	// token and the application, never on the turn id, so they cannot tell an
	// existing turn from a missing one.
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	if turnEventsWantsWebSocket(r) {
		h.ws.ServeHTTP(w, r)
		return
	}
	ctx, stop := serverStreamContext(r)
	defer stop()
	h.sse.ServeHTTP(w, r.WithContext(ctx))
}

// turnEventsWantsWebSocket reports whether the request asks to upgrade to a
// WebSocket.
func turnEventsWantsWebSocket(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Upgrade")), "websocket")
}
