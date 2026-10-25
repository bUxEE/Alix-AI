// Package stream carries the TurnEvents of a turn to a client over server-sent
// events or a WebSocket, and lets a client that lost its connection resume from
// a cursor (master plan 14.2: "Streaming uses SSE, WebSocket or gRPC according
// to the client and deployment profile").
//
// # Model
//
// A Hub keeps the events of turns. The producer of a turn (the code that runs
// the dialogue engine) opens it with Begin or Start and publishes TurnEvents;
// each is validated and sanitised by internal/blocks, encoded once, and buffered
// in a bounded ring. Subscribers read from the ring. A live stream and a resumed
// one therefore carry the same bytes, and a resume reads: it never reaches the
// producer, so a turn is never run twice because a client reconnected.
//
// # Wire protocol
//
// Both transports are a GET on a path that names the turn. The caller's
// identity comes from Options.Scope (the verified context token), never from the
// URL; a credential is never put in a URL.
//
// SSE: the response is text/event-stream. Each event is
//
//	id: <seq>
//	event: <type>
//	data: <the TurnEvent JSON on one line>
//
// followed by a blank line; the first line of the response is a retry hint and
// idle periods carry ": keep-alive" comments. The id is the event's seq, the
// resume cursor. The stream ends after the done event; a connection that ends
// without one is to be resumed.
//
// WebSocket: one text message per event, the same JSON. The subprotocol is
// "alixai.turn-events.v1" (optional). The transport is server to client only: a
// message from the client closes the connection with 1008. After the done event
// the server closes with 1000 "done"; 1013 "slow_consumer" asks the client to
// reconnect with its cursor; 1011 "aborted" says the turn was abandoned.
//
// Resume: the cursor is the seq of the last event the client received; the
// stream continues with the next one, each exactly once and in order. SSE reads
// it from the Last-Event-ID header (an EventSource sends it on its own), either
// transport from the "cursor" query parameter; with both, the later wins. No
// cursor is the start of the turn.
//
// Refusals before the stream starts are HTTP statuses with a typed error body
// (schemas/common/error): 400 for a malformed cursor or one ahead of the stream,
// 410 for a cursor behind the retained window (the stream is never resumed with
// a gap), 403 for a turn that is not visible in the caller's scope (an unknown
// turn, another principal's turn and an expired turn are indistinguishable),
// 429 when the turn has its most subscribers, 204 on SSE when the cursor is
// already at the end of a finished turn (the signal to stop reconnecting). The
// error's details carry a stable "reason" (see Reason).
//
// # Model text
//
// Model Markdown reaches a stream through Turn.PublishResponse: the whole
// response is sanitised first, and only the sanitised text is cut into token
// events (at most 4096 runes each), so a construct split across two fragments
// cannot become active when a client joins them. The fragments concatenate to
// exactly the sanitised text.
//
// # Backpressure
//
// The producer never waits for a reader. Memory is bounded per event
// (MaxEventBytes), per turn (a ring of MaxEvents events and MaxBytes bytes that
// forgets its oldest) and per hub (MaxTurns, MaxTotalBytes; finished turns are
// forgotten oldest first, and an open turn is never dropped). A live subscriber
// may be at most MaxLag events behind the newest; past that it is cut off with
// ErrSlowConsumer (SSE: the connection is closed after a comment; WebSocket:
// close 1013) and resumes from its last cursor, which the ring still holds
// because MaxLag is at most half of MaxEvents. A connection that stops reading
// is dropped when a frame write does not finish within Options.WriteTimeout.
// The hub's limits are shared out per tenant (an application and a tenant id):
// a tenant at its share (MaxTurnsPerTenant, MaxBytesPerTenant) is refused with
// resource_exhausted, or has its own finished turns forgotten, and never takes
// room another tenant is entitled to.
//
// # What a stream never carries
//
// The single-use confirmation secret of a prepared action is not a TurnEvent
// member and is never sent here; it is shown once, to the client that made the
// turn, by that turn's own response. The buffer holds TurnEvents only and a
// resume does not repeat it. As a last line of defence an event with a
// credential-shaped member name (token, secret, password, authorization, and
// similar) is refused rather than sent.
//
// # Mounting
//
// Hub.SSEHandler and Hub.WebSocketHandler are plain http.Handlers. The gateway
// mounts them under its /v1 turn routes with a Scope function that calls
// identity.RequireScope and its own error writer; this package does not import
// the gateway.
package stream
