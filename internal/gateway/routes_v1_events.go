package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/stream"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Application events reach sessions (master plan 9.3 item 4, 11.3). Two routes:
//
//	POST /v1/events                          a host application delivers a signed event
//	GET  /v1/sessions/{session_id}/events    a client learns that its session's state changed
//
// # Delivering an event
//
// The body is the delivery of adapters/events (see that package for the wire
// format and the order of its checks): {event, signatures}, one detached
// Ed25519 signature by a key registered for the application. This package does
// not import the adapter (module-map rule 7); the composition root provides
// EventsIntakes, which gives the handler of the application's intake.
//
// The request carries a context token like every /v1 request, and the token is
// used for one thing only: its application id picks the intake (and an
// application that is not active serves nothing, a quarantined one included).
// It does not authenticate the event and it does not scope it: the tenant and
// workspace an event is about, and the sessions it reaches, come only from the
// signed payload; the token's tenant, workspace, principal and session, the
// path, the query and the headers never name or widen a scope
// (TestEventsRouteTakesTheScopeOnlyFromThePayload). The signature is the
// authority: an event without one, or with one that does not verify, touches
// nothing.
//
// # How a client learns of it (the session-level announcement)
//
// ApplyEvent invalidates the sessions an event changed and tells each one with a
// notice: a failed turn of its own (a stale error event and a failed done, both
// core data, "Nothing was committed"), opened in the stream hub under the
// session's own scope. The hub is keyed per turn and a client never learns the
// id of a turn it did not start, so the notice alone reaches nobody. The
// announcement closes that gap:
//
//   - the notifier (internal/cli/cmd_events.go) opens the notice turn in the hub
//     and then records one EventsAnnouncement for the session in an EventsFeed:
//     {seq, type "state_changed", event_id, event_type, kind, turn_id, at}. It
//     carries identifiers only, no text and no state;
//   - GET /v1/sessions/{session_id}/events is a server-sent event stream of the
//     session's announcements (id = seq, event = state_changed, data = the
//     announcement as one line of JSON). It replays what is retained after the
//     cursor (Last-Event-ID header or cursor query parameter), then follows live,
//     with keep-alive comments. The scope is the verified token's, whole (the
//     application, tenant, workspace, principal and session it is bound to), and
//     the session in the path must be the token's;
//   - the client then reads the notice with the existing
//     GET /v1/sessions/{session_id}/turns/{turn_id}/events, under a fresh token.
//
// An announcement is a hint, never the truth. It is kept in memory for as long
// as the notice turn is (the hub's retention, five minutes by default), bounded
// per session and in total, and lost with the process; the authority on what is
// pending is always GET /v1/sessions/{session_id}/pending. Nothing about
// announcing can commit, prepare or confirm anything: the feed holds
// identifiers, and an event itself can only invalidate (sessions.ApplyEvent).
//
// Context tokens are single use, so every open of the stream needs a fresh one:
// a browser EventSource cannot be used under header authentication, a fetch
// based reader (the SDK's) can.
//
// # AG-UI (ADR 0035)
//
// A CopilotKit client does not read this route; it is on the AG-UI face. What it
// sees is the state itself: every run and every connect run of POST /v1/agui
// starts from the client-safe projection of the session (agui.ProjectState), and
// after an event the projection no longer shows the voided confirmation, so the
// next run (or a reconnect) emits a STATE_SNAPSHOT or a STATE_DELTA on /alixai
// that removes it (TestEventsChangeTheStateAGUIProjects). The reason ("the
// application reported a change") is the announcement above; carrying it into
// AG-UI as a CUSTOM event "alixai.state_changed" right after RUN_STARTED is a
// proposal for internal/agui and routes_v1_agui.go, which are not part of this
// task.

func init() { RegisterArea("v1_events", registerV1Events) }

// EventsIntakes gives the handler that accepts the events of one application.
// The composition root implements it with the intake of adapters/events bound
// to that application, which refuses the platform application and verifies every
// signature against the keys registered for it. An error means there is no
// intake for the application (not configured, refused, not available): the
// route answers it and delivers nothing.
type EventsIntakes interface {
	EventsHandler(ctx context.Context, applicationID string) (http.Handler, error)
}

// registerV1Events needs the intakes, the feed the announcements go through and
// the application service (a quarantined application serves no traffic).
func registerV1Events(rt *Router, d *Deps) {
	n := d.Needs()
	h := &eventsHandler{
		intakes: Need[EventsIntakes](n),
		feed:    Need[*EventsFeed](n),
		apps:    Need[ApplicationService](n),
	}
	if !n.Ready() {
		return
	}
	v1 := rt.V1()
	v1.HandleFunc("POST /events", h.deliver)
	v1.HandleFunc("GET /sessions/{session_id}/events", h.listen)
}

type eventsHandler struct {
	intakes EventsIntakes
	feed    *EventsFeed
	apps    ApplicationService
}

// deliver hands the request to the application's intake. The token's application
// id is the only thing read from the verified scope.
func (h *eventsHandler) deliver(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok {
		return
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	intake, err := h.intakes.EventsHandler(r.Context(), scope.ApplicationID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	intake.ServeHTTP(w, r)
}

// listen serves the announcements of the token's session.
func (h *eventsHandler) listen(w http.ResponseWriter, r *http.Request) {
	scope, ok := v1Scope(w, r)
	if !ok || !sessionPath(w, r, scope) {
		return
	}
	if _, ok := activePackage(w, r, h.apps, scope); !ok {
		return
	}
	cursor, err := stream.ParseCursor(r)
	if err != nil {
		WriteErrorStatus(w, r, http.StatusBadRequest, err)
		return
	}
	sub, err := h.feed.Subscribe(scope, cursor)
	if err != nil {
		status := 0
		if errors.Is(err, stream.ErrCursorAhead) {
			status = http.StatusBadRequest
		} else if errors.Is(err, stream.ErrSubscribers) {
			status = http.StatusTooManyRequests
		}
		WriteErrorStatus(w, r, status, err)
		return
	}
	defer sub.Close()

	hd := w.Header()
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	cfg := h.feed.cfg
	write := func(b []byte) error {
		_ = rc.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout))
		if _, err := w.Write(b); err != nil {
			return err
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		return nil
	}
	w.WriteHeader(http.StatusOK)
	if write([]byte("retry: "+strconv.FormatInt(cfg.RetryAfter.Milliseconds(), 10)+"\n\n")) != nil {
		return
	}
	ctx, stop := serverStreamContext(r)
	defer stop()
	for {
		wctx, cancel := context.WithTimeout(ctx, cfg.Heartbeat)
		a, err := sub.Next(wctx)
		cancel()
		switch {
		case err == nil:
			frame, merr := eventsFrame(a)
			if merr != nil || write(frame) != nil {
				return
			}
		case ctx.Err() != nil:
			return
		case errors.Is(err, context.DeadlineExceeded):
			if write([]byte(": keep-alive\n\n")) != nil {
				return
			}
		default:
			return
		}
	}
}

// eventsFrame renders an announcement as one server-sent event.
func eventsFrame(a EventsAnnouncement) ([]byte, error) {
	data, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 0, len(data)+64)
	b = append(b, "id: "...)
	b = strconv.AppendInt(b, a.Seq, 10)
	b = append(b, "\nevent: "...)
	b = append(b, EventsAnnouncementType...)
	b = append(b, "\ndata: "...)
	b = append(b, data...)
	return append(b, '\n', '\n'), nil
}

// ---- the feed ----

// EventsAnnouncementType is the type of an announcement: the state of the session
// changed, and a notice turn tells how.
const EventsAnnouncementType = "state_changed"

// EventsAnnouncement tells a connected client that an application event changed
// its session. It carries identifiers only; the notice itself is the turn TurnID
// in the stream hub, and the session's pending state is the authority.
type EventsAnnouncement struct {
	// Seq numbers the announcements of one session; it is the SSE id and the
	// resume cursor. The feed assigns it: it starts at a time-based value (the Unix
	// time in seconds when the session's record in the feed was made) and only
	// increases.
	Seq int64 `json:"seq"`
	// Type is always EventsAnnouncementType.
	Type string `json:"type"`
	// EventID, EventType and Kind identify the application event.
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	Kind      string `json:"kind"`
	// TurnID is the notice turn, readable with
	// GET /v1/sessions/{session_id}/turns/{turn_id}/events until it is forgotten.
	TurnID string `json:"turn_id"`
	// At is when the session was told; the feed assigns it.
	At time.Time `json:"at"`

	expires time.Time
}

// EventsFeedConfig bounds an EventsFeed. A zero field takes its default.
type EventsFeedConfig struct {
	// Clock is the time source. Default: the real clock.
	Clock clock.Clock
	// MaxPerSession is how many announcements of one session are kept. Default 16.
	MaxPerSession int
	// MaxSessions is how many sessions have announcements at once. Default 4096.
	MaxSessions int
	// MaxSubscribers is how many streams one session may have open. Default 4.
	MaxSubscribers int
	// Heartbeat is how long an idle stream waits before a keep-alive comment.
	// Default 15 seconds. WriteTimeout bounds one frame write (default 10
	// seconds); RetryAfter is the reconnection delay an SSE client is told to
	// use (default 2 seconds).
	Heartbeat, WriteTimeout, RetryAfter time.Duration
}

func (c EventsFeedConfig) withDefaults() EventsFeedConfig {
	if c.Clock == nil {
		c.Clock = clock.Real{}
	}
	if c.MaxPerSession <= 0 {
		c.MaxPerSession = 16
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = 4096
	}
	if c.MaxSubscribers <= 0 {
		c.MaxSubscribers = 4
	}
	if c.Heartbeat <= 0 {
		c.Heartbeat = 15 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 10 * time.Second
	}
	if c.RetryAfter <= 0 {
		c.RetryAfter = 2 * time.Second
	}
	return c
}

// eventsKey is the whole scope a session is bound to: an announcement is visible
// to exactly that scope, like a turn of the hub.
type eventsKey struct {
	deployment, application, tenant, workspace, principal, session string
}

func eventsKeyOf(s contracts.Scope) eventsKey {
	return eventsKey{s.DeploymentID, s.ApplicationID, s.TenantID, s.WorkspaceID, s.PrincipalID, s.SessionID}
}

// eventsSession is the record of one session in the feed. Its first announcement
// is numbered base and every next one last+1.
type eventsSession struct {
	base   int64
	last   int64
	items  []EventsAnnouncement
	subs   int
	notify chan struct{}
}

// EventsFeed holds the announcements of sessions, in memory, bounded per session
// (MaxPerSession), in total (MaxSessions) and in time (each lives as long as the
// notice turn it points to). It is safe for concurrent use.
//
// A session's announcements are numbered from the clock's Unix time in seconds
// when its record is made, so a record made again (after a restart of the
// process, or after the feed forgot it) does not reuse the numbers a client may
// still hold as its cursor; within one process a record made again also starts
// past the last number it gave (forgot).
type EventsFeed struct {
	cfg      EventsFeedConfig
	mu       sync.Mutex
	sessions map[eventsKey]*eventsSession
	// forgot keeps the last number of a forgotten record while it is not yet
	// behind the clock, which is the only time the clock alone could reuse it.
	forgot map[eventsKey]int64
}

// NewEventsFeed returns an empty feed.
func NewEventsFeed(cfg EventsFeedConfig) *EventsFeed {
	return &EventsFeed{cfg: cfg.withDefaults(), sessions: map[eventsKey]*eventsSession{}, forgot: map[eventsKey]int64{}}
}

// eventsBaseLocked is the first number of a record made now for key: the clock's
// Unix seconds, past the last number of the record it replaces, and at least 1.
func (f *EventsFeed) eventsBaseLocked(key eventsKey, now time.Time) int64 {
	return max(1, now.Unix(), f.forgot[key]+1)
}

// eventsOpenLocked makes the record of key, numbered from base.
func (f *EventsFeed) eventsOpenLocked(key eventsKey, base int64) *eventsSession {
	delete(f.forgot, key)
	s := &eventsSession{base: base, last: base - 1, notify: make(chan struct{})}
	f.sessions[key] = s
	return s
}

func eventsIdent(s string) bool { return s != "" && len(s) <= contracts.MaxScopeValueLen }

// Announce records that the session bound to scope was changed by an event, and
// wakes the streams open on it. The announcement is kept for retain, which is
// how long the notice turn it names stays readable. It returns the sequence
// number it was given. A full feed refuses (resource_exhausted) rather than
// forgetting another session's announcements.
func (f *EventsFeed) Announce(scope contracts.Scope, a EventsAnnouncement, retain time.Duration) (int64, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if !eventsIdent(a.EventID) || !eventsIdent(a.EventType) || !eventsIdent(a.Kind) || !eventsIdent(a.TurnID) || retain <= 0 {
		return 0, contracts.New(contracts.ErrValidationFailed, "events: an announcement names the event, its kind and the notice turn")
	}
	key := eventsKeyOf(scope)
	now := f.cfg.Clock.Now()

	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[key]
	if s == nil {
		if len(f.sessions) >= f.cfg.MaxSessions {
			f.pruneLocked(now)
		}
		if len(f.sessions) >= f.cfg.MaxSessions {
			return 0, contracts.New(contracts.ErrResourceExhausted, "events: the announcement feed is full")
		}
		s = f.eventsOpenLocked(key, f.eventsBaseLocked(key, now))
	}
	if s.last >= stream.MaxSeq {
		return 0, contracts.New(contracts.ErrResourceExhausted, "events: the session's announcement numbers are used up")
	}
	s.last++
	a.Seq, a.Type, a.At, a.expires = s.last, EventsAnnouncementType, now.UTC(), now.Add(retain)
	s.items = append(s.items, a)
	if over := len(s.items) - f.cfg.MaxPerSession; over > 0 {
		s.items = append([]EventsAnnouncement(nil), s.items[over:]...)
	}
	close(s.notify)
	s.notify = make(chan struct{})
	return a.Seq, nil
}

// pruneLocked forgets the expired announcements and the sessions left with none
// and nobody listening. The last number of a forgotten session is kept only
// while the clock has not passed it.
func (f *EventsFeed) pruneLocked(now time.Time) {
	sec := now.Unix()
	for k, last := range f.forgot {
		if last < sec {
			delete(f.forgot, k)
		}
	}
	for k, s := range f.sessions {
		s.items = eventsLive(s.items, now)
		if len(s.items) == 0 && s.subs == 0 {
			delete(f.sessions, k)
			if s.last >= sec {
				f.forgot[k] = s.last
			}
		}
	}
}

func eventsLive(items []EventsAnnouncement, now time.Time) []EventsAnnouncement {
	i := 0
	for i < len(items) && !items[i].expires.After(now) {
		i++
	}
	return items[i:]
}

// EventsSubscription follows the announcements of one session.
type EventsSubscription struct {
	f      *EventsFeed
	key    eventsKey
	after  int64
	closed bool
}

// Subscribe follows the announcements of the session bound to scope after the
// cursor (stream.FromStart for all that are retained). A cursor older than the
// session's record (one a client kept from before a restart, or from a record
// the feed forgot) follows the record from its start. A cursor past the newest
// announcement is refused (stream.ErrCursorAhead), as a stream's is; so is a
// session that already has its most subscribers (stream.ErrSubscribers).
func (f *EventsFeed) Subscribe(scope contracts.Scope, after int64) (*EventsSubscription, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	key := eventsKeyOf(scope)
	now := f.cfg.Clock.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[key]
	if s == nil {
		base := f.eventsBaseLocked(key, now)
		if after >= base {
			return nil, stream.ErrCursorAhead
		}
		if len(f.sessions) >= f.cfg.MaxSessions {
			f.pruneLocked(now)
		}
		if len(f.sessions) >= f.cfg.MaxSessions {
			return nil, stream.ErrCapacity
		}
		s = f.eventsOpenLocked(key, base)
	}
	if after > s.last {
		return nil, stream.ErrCursorAhead
	}
	if after < s.base {
		after = stream.FromStart
	}
	if s.subs >= f.cfg.MaxSubscribers {
		return nil, stream.ErrSubscribers
	}
	s.subs++
	return &EventsSubscription{f: f, key: key, after: after}, nil
}

// Next returns the next announcement after the cursor, waiting for one until ctx
// ends (it then returns the context's error). Each announcement is returned once,
// in order.
func (s *EventsSubscription) Next(ctx context.Context) (EventsAnnouncement, error) {
	for {
		s.f.mu.Lock()
		sess := s.f.sessions[s.key]
		if sess == nil || s.closed {
			s.f.mu.Unlock()
			return EventsAnnouncement{}, stream.ErrAborted
		}
		sess.items = eventsLive(sess.items, s.f.cfg.Clock.Now())
		for _, a := range sess.items {
			if a.Seq > s.after {
				s.after = a.Seq
				s.f.mu.Unlock()
				return a, nil
			}
		}
		wait := sess.notify
		s.f.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return EventsAnnouncement{}, ctx.Err()
		}
	}
}

// Close ends the subscription; the session's slot is freed.
func (s *EventsSubscription) Close() {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if sess := s.f.sessions[s.key]; sess != nil && sess.subs > 0 {
		sess.subs--
	}
}
