package sdkgo

import (
	"context"
	"errors"
	"io"
	"iter"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/coder/websocket"
)

// ConfirmationTokenHeader is the response header that carries the single-use confirmation secret of an
// event-stream turn.
const ConfirmationTokenHeader = "X-AlixAI-Confirmation-Token"

// WebSocketSubprotocol is the subprotocol of the WebSocket form of the event stream.
const WebSocketSubprotocol = "alixai.turn-events.v1"

// TurnStream reads the events of one turn.
//
// Sending happens when SendTurn returns: the response head has arrived and the events are read on demand.
// Iterate with Next, Event and Err, or with All, or call Result to read it all. Events are decoded strictly
// and the sequence is enforced (one turn_id, seq from 0 without gaps, `done` last): a stream that ends before
// `done`, a gap and an event of an unknown type each stop the stream with a typed error rather than ending the
// turn quietly. A connection that drops after the first event is resumed from the last seq with a fresh
// context token (the turn is never run again), up to ClientOptions.MaxResumes times. A TurnStream is not safe
// for concurrent use.
type TurnStream struct {
	c         *Client
	sessionID string
	scope     *callScope
	webSocket bool

	src   frameSource
	queue []conversation.TurnEvent // the events of a JSON-form answer
	seq   turnSequence

	events []conversation.TurnEvent
	cur    conversation.TurnEvent
	err    error

	closed        bool
	finished      bool
	resumes       int
	secret        string // the confirmation secret of the response header
	jsonGrant     *ConfirmationGrant
	preparationID string
	grant         *ConfirmationGrant
}

// SendTurn sends a turn and returns its events as a stream (server-sent events over a POST). A turn that
// could not be run at all (a limit, an ended session, a scope that is not this session's) is an error here,
// never a stream.
func (c *Client) SendTurn(ctx context.Context, sessionID string, input TurnInput, options ...CallOption) (*TurnStream, error) {
	if err := pathID("sessionID", sessionID); err != nil {
		return nil, err
	}
	body, err := c.turnBody(input)
	if err != nil {
		return nil, err
	}
	o := applyOptions(options)
	scope := newScope(ctx, o.deadline(c.turnTimeout), opSendTurn)
	resp, err := c.t.send(scope, request{
		op: opSendTurn, params: map[string]string{"session_id": sessionID},
		accept: "text/event-stream", contentType: "application/json", body: body,
	})
	if err != nil {
		scope.release()
		return nil, err
	}
	s := &TurnStream{c: c, sessionID: sessionID, scope: scope, webSocket: o.webSocket}
	if err := s.open(resp); err != nil {
		resp.Body.Close()
		s.closed = true
		scope.release()
		return nil, err
	}
	return s, nil
}

// StreamTurnEvents reads the events of a turn again, from a cursor, so that a client that lost its connection
// continues without the turn being run again. WithCursor names the seq of the last event received; without it
// the stream starts at the beginning of the turn. WithWebSocket reads over a WebSocket. Every call needs a
// fresh context token. A cursor behind the retained window is `expired` (details.reason cursor_expired); a
// turn that is not the caller's is `scope_violation`. A cursor at the end of a finished turn gives a stream
// with no events. The confirmation secret is never sent here: Confirmation is always nil.
func (c *Client) StreamTurnEvents(ctx context.Context, sessionID, turnID string, options ...CallOption) (*TurnStream, error) {
	if err := pathID("sessionID", sessionID); err != nil {
		return nil, err
	}
	if turnID == "" || len(turnID) > 128 || strings.ContainsAny(turnID, "/\\?#") {
		return nil, invalidArgument("turnID", "it is not a valid identifier")
	}
	o := applyOptions(options)
	if o.cursor != nil && *o.cursor < 0 {
		return nil, invalidArgument("cursor", "it must be a non-negative sequence number")
	}
	scope := newScope(ctx, o.deadline(c.turnTimeout), opStreamTurnEvent)
	src, err := c.openEvents(scope, sessionID, turnID, o.cursor, o.webSocket)
	if err != nil {
		scope.release()
		return nil, err
	}
	s := &TurnStream{c: c, sessionID: sessionID, scope: scope, webSocket: o.webSocket, src: src}
	s.seq.turnID = turnID
	if o.cursor != nil {
		s.seq.nextSeq = *o.cursor + 1
	}
	if src == nil { // 204: the client has every event
		s.finished = true
		s.closed = true
		scope.release()
	}
	return s, nil
}

// open reads the head of the answer to a turn request.
func (s *TurnStream) open(resp *http.Response) error {
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch media {
	case "text/event-stream":
		if secret := resp.Header.Get(ConfirmationTokenHeader); secret != "" {
			if !confirmationPattern.MatchString(secret) {
				return malformedTurn("the confirmation token header is malformed")
			}
			s.secret = secret
		}
		s.src = newSSEReader(resp.Body)
	case "application/json":
		// The server answered with the JSON form of the turn instead of a stream: same events, same checks.
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		if err != nil {
			return s.scope.failure(err)
		}
		result, err := decodeTurnResult(body, opSendTurn)
		if err != nil {
			return err
		}
		s.queue = result.Events
		s.jsonGrant = result.Confirmation
		s.seq.turnID = result.TurnID
		s.src = emptySource{}
	default:
		return malformedTurn("the content type is neither text/event-stream nor application/json")
	}
	return nil
}

// Next advances to the next event and reports whether there is one. It returns false at the end of the turn
// (after `done`), after Close, and on a failure: read Err to tell them apart.
func (s *TurnStream) Next() bool {
	if s.err != nil || s.closed || s.finished {
		return false
	}
	ev, err := s.pull()
	if err != nil {
		s.fail(err)
		return false
	}
	s.cur = ev
	s.events = append(s.events, ev)
	switch v := ev.Value.(type) {
	case conversation.PreparedActionEvent:
		s.preparationID = string(v.PreparedAction.PreparationID)
	case conversation.DoneEvent:
		if err := s.complete(v); err != nil {
			s.fail(err)
			return false
		}
		s.finished = true
		s.Close()
	}
	return true
}

// Event is the event Next advanced to.
func (s *TurnStream) Event() conversation.TurnEvent { return s.cur }

// Err is the failure that stopped the stream: nil after a clean end or a Close.
func (s *TurnStream) Err() error { return s.err }

// Events returns the events read so far.
func (s *TurnStream) Events() []conversation.TurnEvent { return s.events }

// TurnID is the id of the turn, known once the first event has arrived.
func (s *TurnStream) TurnID() string { return s.seq.turnID }

// Close abandons the turn: the connection is released. It is safe to call twice. Closing before `done` ends
// iteration without an error; Confirmation then reports the unknown outcome.
func (s *TurnStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	if s.src != nil {
		s.src.close()
	}
	s.scope.release()
	return nil
}

// All iterates the events: for event, err := range stream.All(). A failure is the last element, with its
// error. The stream is closed when the loop ends.
func (s *TurnStream) All() iter.Seq2[conversation.TurnEvent, error] {
	return func(yield func(conversation.TurnEvent, error) bool) {
		defer s.Close()
		for s.Next() {
			if !yield(s.Event(), nil) {
				return
			}
		}
		if s.err != nil {
			yield(conversation.TurnEvent{}, s.err)
		}
	}
}

// Result reads the stream to its end and returns every event with the confirmation grant.
func (s *TurnStream) Result() (*TurnResult, error) {
	for s.Next() {
	}
	if s.err != nil {
		return nil, s.err
	}
	if !s.finished {
		return nil, s.seq.finish()
	}
	grant, err := s.Confirmation()
	if err != nil {
		return nil, err
	}
	return &TurnResult{TurnID: s.seq.turnID, Events: append([]conversation.TurnEvent(nil), s.events...), Confirmation: grant}, nil
}

// Confirmation is the single-use confirmation secret and the preparation it belongs to, when the turn
// prepared an action that needs the user's explicit confirmation; nil otherwise (also for a level 3 action,
// which returns a `request_step_up` directive and cannot be confirmed here). It is available once the turn
// has been read to its `done` event; before that, or after a failure or an early Close, it is an error.
func (s *TurnStream) Confirmation() (*ConfirmationGrant, error) {
	if s.err != nil {
		return nil, s.err
	}
	if !s.finished {
		return nil, localError(contracts.ErrUncertainOutcome,
			"the turn stream was not read to its done event", "reason", "closed_early")
	}
	return s.grant, nil
}

func (s *TurnStream) fail(err error) {
	if s.err == nil {
		s.err = err
	}
	s.Close()
}

// complete pairs the header's secret with the preparation the turn prepared.
func (s *TurnStream) complete(done conversation.DoneEvent) error {
	id := s.preparationID
	if done.PreparationID != nil {
		if id != "" && id != string(*done.PreparationID) {
			return malformedTurn("done names another preparation than the prepared_action event")
		}
		id = string(*done.PreparationID)
	}
	switch {
	case s.secret != "":
		if id == "" {
			return malformedTurn("a confirmation token arrived without a prepared action")
		}
		s.grant = &ConfirmationGrant{PreparationID: id, Token: s.secret}
	case s.jsonGrant != nil:
		if id != "" && id != s.jsonGrant.PreparationID {
			return malformedTurn("the confirmation names another preparation than the prepared_action event")
		}
		s.grant = s.jsonGrant
	}
	return nil
}

// pull reads the next event, resuming the stream when the connection ends before `done`.
func (s *TurnStream) pull() (conversation.TurnEvent, error) {
	for {
		// A cancelled or expired context ends the stream at once, even when events are still buffered.
		if cerr := s.scope.ctx.Err(); cerr != nil {
			return conversation.TurnEvent{}, s.scope.failure(cerr)
		}
		if s.queue != nil {
			if len(s.queue) == 0 {
				return conversation.TurnEvent{}, s.seq.finish()
			}
			ev := s.queue[0]
			s.queue = s.queue[1:]
			if err := s.seq.accept(ev, nil); err != nil {
				return conversation.TurnEvent{}, err
			}
			return ev, nil
		}
		fr, err := s.src.next()
		if err == nil {
			ev, err := parseTurnEvent(fr.data)
			if err != nil {
				return ev, err
			}
			return ev, s.seq.accept(ev, &fr)
		}
		if cerr := s.scope.ctx.Err(); cerr != nil {
			return conversation.TurnEvent{}, s.scope.failure(cerr)
		}
		if errors.Is(err, errEventTooLarge) {
			return conversation.TurnEvent{}, localError(contracts.ErrInternal, err.Error(), "reason", "event_too_large")
		}
		if ended := (wsEnded{}); errors.As(err, &ended) {
			return conversation.TurnEvent{}, ended.err
		}
		if err := s.resume(err); err != nil {
			return conversation.TurnEvent{}, err
		}
	}
}

// maxResumeWait bounds the linear pause before a resume (5 seconds, as the TypeScript SDK), unless ResumeBackoff itself is longer.
const maxResumeWait = 5 * time.Second

// resume opens the event stream again after the last event received. A drop before the first event leaves
// the turn id unknown: the turn cannot be resumed and its outcome is unknown.
func (s *TurnStream) resume(cause error) error {
	for {
		if s.seq.turnID == "" || s.c.maxResumes < 0 || s.resumes >= s.c.maxResumes {
			return contracts.Wrap(contracts.ErrUncertainOutcome, cause,
				"the turn stream ended before its done event and could not be resumed: what the turn did is unknown; read the session's pending state before sending anything again").
				WithDetails("reason", "stream_ended_early").WithDetails("received", s.seq.nextSeq)
		}
		s.resumes++
		wait := s.c.backoff * time.Duration(s.resumes)
		if limit := max(s.c.backoff, maxResumeWait); wait > limit {
			wait = limit
		}
		select {
		case <-time.After(wait):
		case <-s.scope.ctx.Done():
			return s.scope.failure(s.scope.ctx.Err())
		}
		s.src.close()
		last := s.seq.nextSeq - 1
		// The resume is a read of the turn's events, whatever call opened the stream: its transport failure is classified as
		// that read (unavailable, retryable), never as the uncertain outcome of the SendTurn that started it.
		read := s.scope.as(opStreamTurnEvent)
		src, err := s.c.openEvents(read, s.sessionID, s.seq.turnID, &last, s.webSocket)
		if err == nil {
			if src == nil { // the server says the turn is complete, yet its done event never arrived
				return malformedTurn("the server reports the turn complete but its done event was not received")
			}
			s.src = src
			return nil
		}
		if !IsRetryable(err) {
			return err
		}
		cause = err
	}
}

// openEvents opens the resume route of a turn, as server-sent events or as a WebSocket. It returns a nil
// source for a 204: the cursor is at the end of a finished turn.
func (c *Client) openEvents(scope *callScope, sessionID, turnID string, cursor *int64, webSocket bool) (frameSource, error) {
	r := request{
		op:     opStreamTurnEvent,
		params: map[string]string{"session_id": sessionID, "turn_id": turnID},
		accept: "text/event-stream",
	}
	if webSocket {
		return c.openWebSocket(scope, r, cursor)
	}
	if cursor != nil {
		r.header = http.Header{"Last-Event-ID": {itoa(*cursor)}}
	}
	resp, err := c.t.send(scope, r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent {
		resp.Body.Close()
		return nil, nil
	}
	if media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); media != "text/event-stream" {
		resp.Body.Close()
		return nil, malformedTurn("the resume route did not answer with an event stream")
	}
	return newSSEReader(resp.Body), nil
}

func (c *Client) openWebSocket(scope *callScope, r request, cursor *int64) (frameSource, error) {
	if cursor != nil {
		r.query = map[string][]string{"cursor": {itoa(*cursor)}}
	}
	if err := scope.ctx.Err(); err != nil {
		return nil, scope.failure(err)
	}
	tok, err := c.t.authorize(scope, r)
	if err != nil {
		return nil, err
	}
	hc := *c.t.hc
	hc.Timeout = 0 // the deadline is the scope's
	conn, resp, err := websocket.Dial(scope.ctx, c.t.url(r), &websocket.DialOptions{
		HTTPClient:   &hc,
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + tok}},
		Subprotocols: []string{WebSocketSubprotocol},
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				return nil, nil
			}
			return nil, errorFromResponse(resp)
		}
		return nil, scope.failure(err)
	}
	conn.SetReadLimit(maxEventBytes)
	return &wsSource{conn: conn, ctx: scope.ctx}, nil
}

// wsSource reads one TurnEvent per text message. The transport is server to client only.
type wsSource struct {
	conn *websocket.Conn
	ctx  context.Context
}

// wsEnded is a close by the server that ends the stream: it is not a lost connection, so it is never resumed.
type wsEnded struct{ err *contracts.Error }

func (e wsEnded) Error() string { return e.err.Error() }
func (e wsEnded) Unwrap() error { return e.err }

// next reads one message. A close is read as the TypeScript SDK reads it: 1000 is the clean end (after `done`; without it the
// stream resumes like after any lost connection); 1011 (the turn was abandoned) and 1008 (a protocol violation, since the
// client never sends) end the stream with a typed error; anything else (1013 slow consumer, 1006, 1001) is a lost connection.
func (w *wsSource) next() (frame, error) {
	typ, data, err := w.conn.Read(w.ctx)
	if err != nil {
		switch websocket.CloseStatus(err) {
		case websocket.StatusNormalClosure:
			return frame{}, io.EOF
		case websocket.StatusInternalError:
			e := localError(contracts.ErrUnavailable, "the turn was abandoned by the server", "reason", "aborted", "code", int(websocket.StatusInternalError))
			e.Retryable = false
			return frame{}, wsEnded{e}
		case websocket.StatusPolicyViolation:
			return frame{}, wsEnded{localError(contracts.ErrInternal, "the server closed the event stream for a protocol violation",
				"reason", "protocol", "code", int(websocket.StatusPolicyViolation))}
		}
		return frame{}, err
	}
	if typ != websocket.MessageText {
		return frame{}, localError(contracts.ErrInternal, "the event stream sent a binary message", "reason", "malformed_event")
	}
	return frame{data: data}, nil
}

func (w *wsSource) close() { _ = w.conn.CloseNow() }

// emptySource is the source of a JSON-form turn, whose events are queued.
type emptySource struct{}

func (emptySource) next() (frame, error) { return frame{}, io.EOF }
func (emptySource) close()               {}
