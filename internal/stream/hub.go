package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// MaxSeq is the largest event sequence number (the schema's seq maximum).
const MaxSeq int64 = 4294967295

// FromStart is the cursor of a subscriber that has received nothing: it asks
// for the turn from its first event.
const FromStart int64 = -1

// Config bounds a Hub. Every bound has a default; a zero field takes it. Memory
// is bounded three times over: per event, per turn (a ring that forgets its
// oldest events) and per hub (finished turns are forgotten oldest first).
type Config struct {
	// MaxEvents is how many events of one turn are kept for resume. Default 512.
	MaxEvents int
	// MaxBytes is how many bytes of events of one turn are kept. Default 1 MiB.
	MaxBytes int
	// MaxEventBytes is the largest single event. Default 128 KiB.
	MaxEventBytes int
	// MaxLag is how many events a live subscriber may be behind the newest
	// before it is cut off (and must resume). It is the length of its queue.
	// Default 128; never more than half of MaxEvents, so a cut-off subscriber
	// can still resume from what the ring retains.
	MaxLag int
	// MaxSubscribers is how many subscribers one turn serves at once. Default 8.
	MaxSubscribers int
	// MaxTurns is how many turns (open and retained) the hub holds. Default 256.
	MaxTurns int
	// MaxTotalBytes is the most event bytes the hub holds across turns.
	// Default 32 MiB.
	MaxTotalBytes int64
	// MaxTurnsPerTenant and MaxBytesPerTenant are one tenant's share of
	// MaxTurns and MaxTotalBytes (a tenant is an application and a tenant id).
	// A tenant at its share is refused (ErrCapacity) or has its own finished
	// turns forgotten; it never takes room another tenant is entitled to, so a
	// tenant that floods the stream cannot starve the rest. Default: a quarter
	// of the hub's limits, and never more than the limit itself.
	MaxTurnsPerTenant int
	MaxBytesPerTenant int64
	// Retention is how long a finished turn stays resumable. Default 5 minutes.
	Retention time.Duration
	// IdleTimeout is how long an open turn may go without an event before it is
	// abandoned (its subscribers get ErrAborted). Default 10 minutes.
	IdleTimeout time.Duration
	// Clock is the time source for retention. Default clock.Real.
	Clock clock.Clock
	// Validator checks and sanitises every event before it is buffered, so a
	// live stream and a resumed one carry the same bytes. Default blocks.Default().
	Validator *blocks.Validator
	// Logger receives operational events (counts and ids only, never event
	// text). Default: discard.
	Logger *slog.Logger
}

func (c Config) withDefaults() Config {
	pickI := func(v, d int) int {
		if v <= 0 {
			return d
		}
		return v
	}
	c.MaxEvents = pickI(c.MaxEvents, 512)
	c.MaxBytes = pickI(c.MaxBytes, 1<<20)
	c.MaxEventBytes = pickI(c.MaxEventBytes, 128<<10)
	if c.MaxEventBytes > c.MaxBytes {
		c.MaxEventBytes = c.MaxBytes
	}
	c.MaxLag = pickI(c.MaxLag, 128)
	if half := c.MaxEvents / 2; c.MaxLag > half {
		c.MaxLag = max(half, 1)
	}
	c.MaxSubscribers = pickI(c.MaxSubscribers, 8)
	c.MaxTurns = pickI(c.MaxTurns, 256)
	if c.MaxTotalBytes <= 0 {
		c.MaxTotalBytes = 32 << 20
	}
	if c.MaxTurnsPerTenant <= 0 {
		c.MaxTurnsPerTenant = max(c.MaxTurns/4, 1)
	}
	c.MaxTurnsPerTenant = min(c.MaxTurnsPerTenant, c.MaxTurns)
	if c.MaxBytesPerTenant <= 0 {
		c.MaxBytesPerTenant = c.MaxTotalBytes / 4
	}
	c.MaxBytesPerTenant = min(c.MaxBytesPerTenant, c.MaxTotalBytes)
	if c.Retention <= 0 {
		c.Retention = 5 * time.Minute
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 10 * time.Minute
	}
	if c.Clock == nil {
		c.Clock = clock.Real{}
	}
	if c.Validator == nil {
		c.Validator = blocks.Default()
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	return c
}

// turnKey identifies a turn: the whole verified scope that opened it, and the
// turn id. A stream is visible to exactly that scope.
type turnKey struct {
	deployment, application, tenant, workspace, principal, session string
	turn                                                           string
}

func keyOf(scope contracts.Scope, turnID string) turnKey {
	return turnKey{
		deployment: scope.DeploymentID, application: scope.ApplicationID, tenant: scope.TenantID,
		workspace: scope.WorkspaceID, principal: scope.PrincipalID, session: scope.SessionID,
		turn: turnID,
	}
}

// Hub keeps the events of turns so that a client can resume a stream from a
// cursor without the turn being run again. A producer opens a turn (Begin or
// Start) and publishes events; any number of subscribers, in the same scope,
// read them live or from a cursor. The hub never blocks a producer: a slow
// subscriber is cut off, the retained window is a ring, and what no longer fits
// is refused with a typed error, never dropped silently.
type Hub struct {
	cfg Config

	mu      sync.Mutex
	turns   map[turnKey]*Turn
	tenants map[tenantKey]*tenantAcct
	opened  int64        // turns opened so far; orders turns that finish in the same instant
	total   atomic.Int64 // event bytes held across turns
}

// tenantKey is the unit of fairness: one tenant of one application.
type tenantKey struct{ application, tenant string }

// tenantAcct is what one tenant holds. turns is guarded by Hub.mu.
type tenantAcct struct {
	turns int
	bytes atomic.Int64
}

// NewHub returns a Hub.
func NewHub(cfg Config) *Hub {
	return &Hub{cfg: cfg.withDefaults(), turns: map[turnKey]*Turn{}, tenants: map[tenantKey]*tenantAcct{}}
}

// Config returns the effective configuration (defaults filled in).
func (h *Hub) Config() Config { return h.cfg }

// HubStats is a snapshot of the hub's holdings.
type HubStats struct {
	Turns int
	Bytes int64
}

// Stats returns what the hub holds.
func (h *Hub) Stats() HubStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return HubStats{Turns: len(h.turns), Bytes: h.total.Load()}
}

func (h *Hub) now() time.Time { return h.cfg.Clock.Now() }

// validTurnID bounds a turn id taken from a path: it is a key, never text that
// is echoed or logged unbounded.
func validTurnID(id string) bool {
	if id == "" || len(id) > contracts.MaxScopeValueLen || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if r <= 0x20 || r == 0x7f || r == '*' || r > 0x7e {
			return false
		}
	}
	return true
}

// Begin opens a turn for the scope and returns its producer handle. The scope
// must be a valid verified scope; a turn id already in use in that scope is a
// conflict (an event stream is the account of one run, never restarted).
func (h *Hub) Begin(scope contracts.Scope, turnID string) (*Turn, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !validTurnID(turnID) {
		return nil, contracts.New(contracts.ErrValidationFailed, "the turn id is malformed")
	}
	h.sweep()
	key := keyOf(scope, turnID)

	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.turns[key]; exists {
		return nil, contracts.New(contracts.ErrConflict, "the turn already has a stream")
	}
	tk := tenantKey{application: scope.ApplicationID, tenant: scope.TenantID}
	acct := h.tenants[tk]
	if acct == nil {
		acct = &tenantAcct{}
		h.tenants[tk] = acct
	}
	for acct.turns >= h.cfg.MaxTurnsPerTenant {
		if !h.evictOldestFinishedLocked(acct) {
			h.dropIdleAcctLocked(tk, acct)
			return nil, ErrCapacity
		}
	}
	for len(h.turns) >= h.cfg.MaxTurns {
		if !h.evictOldestFinishedLocked(nil) {
			h.dropIdleAcctLocked(tk, acct)
			return nil, ErrCapacity
		}
	}
	now := h.now()
	h.tenants[tk] = acct // eviction above may have dropped an emptied account
	acct.turns++
	h.opened++
	t := &Turn{
		hub: h, key: key, id: turnID, tenant: tk, acct: acct, order: h.opened,
		notify: make(chan struct{}),
		subs:   map[*Subscription]struct{}{},
		facts:  blocks.NewFactSet(nil),
		born:   now,
	}
	t.lastEvent.Store(now.UnixNano())
	h.turns[key] = t
	return t, nil
}

// Lookup returns the turn of the scope, for a subscriber. An unknown turn, a
// turn of another scope and a turn past its retention are the same answer
// (ErrNotVisible).
func (h *Hub) Lookup(scope contracts.Scope, turnID string) (*Turn, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if !validTurnID(turnID) {
		return nil, ErrNotVisible
	}
	h.sweep()
	h.mu.Lock()
	t := h.turns[keyOf(scope, turnID)]
	h.mu.Unlock()
	if t == nil {
		return nil, ErrNotVisible
	}
	return t, nil
}

// Sweep forgets finished turns past their retention and abandons open turns
// idle for too long. Begin and Lookup call it; a host may call it on a timer.
func (h *Hub) Sweep() { h.sweep() }

func (h *Hub) sweep() {
	now := h.now()
	var idle []*Turn
	h.mu.Lock()
	for key, t := range h.turns {
		if fin := t.finishedAt.Load(); fin != 0 {
			if now.Sub(time.Unix(0, fin)) >= h.cfg.Retention {
				h.removeLocked(key, t)
			}
			continue
		}
		if now.Sub(time.Unix(0, t.lastEvent.Load())) >= h.cfg.IdleTimeout {
			idle = append(idle, t)
		}
	}
	h.mu.Unlock()
	for _, t := range idle {
		t.abort(ErrAborted)
	}
}

func (h *Hub) removeLocked(key turnKey, t *Turn) {
	if h.turns[key] != t {
		return
	}
	delete(h.turns, key)
	held := t.size.Load()
	h.total.Add(-held)
	t.acct.bytes.Add(-held)
	t.acct.turns--
	h.dropIdleAcctLocked(t.tenant, t.acct)
}

// dropIdleAcctLocked forgets a tenant's account when it holds nothing, so the
// map does not grow with every tenant the hub has ever seen.
func (h *Hub) dropIdleAcctLocked(tk tenantKey, acct *tenantAcct) {
	if acct.turns <= 0 && acct.bytes.Load() <= 0 && h.tenants[tk] == acct {
		delete(h.tenants, tk)
	}
}

// evictOldestFinishedLocked drops the finished turn that finished first (of the
// tenant of acct, or of any tenant when acct is nil). It reports whether one was
// dropped. A turn that is still open is never dropped: its producer is working
// and its subscribers are reading.
func (h *Hub) evictOldestFinishedLocked(acct *tenantAcct) bool {
	var (
		oldest    *Turn
		oldestKey turnKey
	)
	for key, t := range h.turns {
		fin := t.finishedAt.Load()
		if fin == 0 || (acct != nil && t.acct != acct) {
			continue
		}
		if oldest == nil || fin < oldest.finishedAt.Load() || (fin == oldest.finishedAt.Load() && t.order < oldest.order) {
			oldest, oldestKey = t, key
		}
	}
	if oldest == nil {
		return false
	}
	h.removeLocked(oldestKey, oldest)
	return true
}

// reserve accounts n more bytes held by a turn, forgetting finished turns (the
// tenant's own first, then any) to make room. It fails, and holds nothing, when
// the tenant's share or the hub is full of open turns.
func (h *Hub) reserve(t *Turn, n int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for t.acct.bytes.Load()+n > h.cfg.MaxBytesPerTenant {
		if !h.evictOldestFinishedLocked(t.acct) {
			return ErrCapacity
		}
	}
	for h.total.Load()+n > h.cfg.MaxTotalBytes {
		if !h.evictOldestFinishedLocked(nil) {
			return ErrCapacity
		}
	}
	h.total.Add(n)
	t.acct.bytes.Add(n)
	return nil
}

// release gives back n bytes a turn forgot from its ring.
func (t *Turn) release(n int64) {
	t.size.Add(-n)
	t.hub.total.Add(-n)
	t.acct.bytes.Add(-n)
}

// Start opens a turn and runs produce on its own goroutine, detached from any
// request: a client that disconnects does not cancel the turn, and a client that
// resumes never runs it again. produce publishes the events (ending with a done
// event); if it returns an error, or returns without finishing the turn, or
// panics, the turn is ended with an error event and a failed done event, so a
// subscriber always sees a well-formed end.
func (h *Hub) Start(ctx context.Context, scope contracts.Scope, turnID string, produce func(ctx context.Context, t *Turn) error) (*Turn, error) {
	t, err := h.Begin(scope, turnID)
	if err != nil {
		return nil, err
	}
	ctx = context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.cfg.Logger.Error("turn producer panicked", "turn_id", turnID)
				_ = t.Fail(contracts.New(contracts.ErrInternal, "internal error"))
			}
		}()
		err := produce(ctx, t)
		switch {
		case err != nil && !t.Finished():
			_ = t.Fail(err)
		case err == nil && !t.Finished():
			_ = t.Fail(contracts.New(contracts.ErrInternal, "the turn ended without a result"))
		}
	}()
	return t, nil
}

// ---- turn ----

// Frame is one encoded event: its sequence number (the resume cursor), its type
// and its JSON, which is one line and is exactly what both transports send.
type Frame struct {
	Seq  int64
	Type string
	Data []byte
}

// Turn is the stream of one turn. Its producer methods (Publish, Fail) are for
// the one goroutine that runs the turn; Subscribe is for transports.
type Turn struct {
	hub    *Hub
	key    turnKey
	id     string
	tenant tenantKey
	acct   *tenantAcct
	order  int64 // position in the order turns were opened
	// born is when the turn was opened.
	born time.Time

	pub sync.Mutex // serialises producers; guards facts

	mu       sync.Mutex
	frames   []Frame
	first    int64 // seq of frames[0]
	next     int64 // seq of the next event
	bytes    int
	finished bool
	aborted  error
	notify   chan struct{}
	subs     map[*Subscription]struct{}

	facts *blocks.FactSet
	// caps is what the client announced for this turn (ADR 0047). Zero = 1.4.
	// Set with SetClientCapabilities before the first Publish.
	caps ClientCapabilities

	size       atomic.Int64 // bytes held, for the hub's accounting
	finishedAt atomic.Int64 // unix nanos, 0 while open
	lastEvent  atomic.Int64 // unix nanos
}

// SetClientCapabilities records what the client of this turn can parse (ADR 0047).
// Call before the first Publish. Absent / zero maps every event to the frozen 1.4
// form. The dialogue loop (or gateway) persists the announcement from the turn
// body / AG-UI forwardedProps onto the hub turn via this method.
func (t *Turn) SetClientCapabilities(caps ClientCapabilities) {
	t.pub.Lock()
	defer t.pub.Unlock()
	t.caps = caps
}

// ClientCapabilities returns the announcement recorded for this turn.
func (t *Turn) ClientCapabilities() ClientCapabilities {
	t.pub.Lock()
	defer t.pub.Unlock()
	return t.caps
}

// ID returns the turn id.
func (t *Turn) ID() string { return t.id }

// NextSeq is the sequence number the next event must carry.
func (t *Turn) NextSeq() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.next
}

// Finished reports whether the turn has ended (a done event was published, or
// the turn was abandoned).
func (t *Turn) Finished() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished || t.aborted != nil
}

// TurnStats is a snapshot of one turn's holdings.
type TurnStats struct {
	Retained    int   // events in the ring
	Bytes       int   // bytes in the ring
	FirstSeq    int64 // oldest retained seq
	NextSeq     int64 // next seq to be published
	Subscribers int
	Finished    bool
}

// Stats returns the turn's holdings.
func (t *Turn) Stats() TurnStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TurnStats{Retained: len(t.frames), Bytes: t.bytes, FirstSeq: t.first, NextSeq: t.next,
		Subscribers: len(t.subs), Finished: t.finished || t.aborted != nil}
}

// Publish validates, encodes and buffers the next event of the turn, then
// wakes the subscribers. It never waits for a subscriber. The event must carry
// this turn's id and the next sequence number; it is checked by the block
// validator (rich blocks are validated and their Markdown sanitised; a chart
// may show only numbers in the directive's facts, which the turn learns from
// its directive and receipt events), and refused if it exceeds the size bound
// or carries a credential-shaped member.
//
// A refused event is not buffered and the sequence number is not used up: the
// producer may publish a different event (an error event, say) at the same
// sequence number. A done event ends the turn.
func (t *Turn) Publish(ev conversation.TurnEvent) error {
	t.pub.Lock()
	defer t.pub.Unlock()

	t.mu.Lock()
	switch {
	case t.finished:
		t.mu.Unlock()
		return ErrTurnClosed
	case t.aborted != nil:
		err := t.aborted
		t.mu.Unlock()
		return err
	}
	want := t.next
	t.mu.Unlock()
	if want > MaxSeq {
		return refuse(errors.New("the turn has no sequence numbers left"), "the event was refused")
	}

	clean, err := t.hub.cfg.Validator.Event(ev, blocks.Context{Facts: t.facts})
	if err != nil {
		return refuse(err, "the event was refused by block validation")
	}
	// Map 1.5-only members to the frozen 1.4 form when the client did not announce them
	// (ADR 0047). Facts are learned from the pre-map directive/receipt so verification
	// still sees the authoritative payload.
	mapped := MapEvent(clean, t.caps)
	data, head, tree, err := encodeEvent(mapped)
	if err != nil {
		return refuse(err, "the event could not be encoded")
	}
	if len(data) > t.hub.cfg.MaxEventBytes {
		return refuse(errors.New("event too large"), "the event is larger than the stream allows")
	}
	if head.turnID != t.id {
		return ErrOutOfOrder
	}
	if head.seq != want {
		return ErrOutOfOrder
	}
	if key := sensitiveKey(tree); key != "" {
		// A credential has no place in a stream a proxy may log: refuse rather
		// than send it.
		return refuse(errors.New("credential-shaped member"), "the event carries a member that may hold a credential")
	}
	if err := t.hub.reserve(t, int64(len(data))); err != nil {
		return err
	}
	// From here the event is accepted. Learn facts from the validated (pre-map) event.
	switch e := clean.Value.(type) {
	case conversation.DirectiveEvent:
		t.facts.AddFacts(e.Directive.Facts)
	case conversation.ReceiptEvent:
		t.facts.AddFacts(e.Receipt.Facts)
	}
	_, isDone := mapped.Value.(conversation.DoneEvent)

	t.mu.Lock()
	t.frames = append(t.frames, Frame{Seq: head.seq, Type: head.typ, Data: data})
	t.bytes += len(data)
	t.size.Add(int64(len(data)))
	t.next = want + 1
	t.lastEvent.Store(t.hub.now().UnixNano())
	t.trimLocked()
	t.cutSlowLocked()
	if isDone {
		t.finished = true
		t.finishedAt.Store(t.hub.now().UnixNano())
	}
	t.wakeLocked()
	t.mu.Unlock()
	return nil
}

// trimLocked forgets the oldest events until the ring is within its bounds. The
// newest event is always kept.
func (t *Turn) trimLocked() {
	cfg := t.hub.cfg
	for len(t.frames) > 1 && (len(t.frames) > cfg.MaxEvents || t.bytes > cfg.MaxBytes) {
		n := len(t.frames[0].Data)
		t.frames[0] = Frame{}
		t.frames = t.frames[1:]
		t.first++
		t.bytes -= n
		t.release(int64(n))
	}
}

// cutSlowLocked cuts off every live subscriber that is more than MaxLag events
// behind. Its queue is the ring itself (it costs nothing to keep), so what is
// bounded is how far behind it may fall while the events it still needs are
// retained: the cut-off subscriber can resume without a gap.
func (t *Turn) cutSlowLocked() {
	lag := int64(t.hub.cfg.MaxLag)
	for s := range t.subs {
		if !s.live {
			continue
		}
		behind := t.next - s.next
		if behind > lag {
			s.err = ErrSlowConsumer
			delete(t.subs, s)
			continue
		}
		if behind > s.peak {
			s.peak = behind
		}
	}
}

func (t *Turn) wakeLocked() {
	close(t.notify)
	t.notify = make(chan struct{})
}

// abort ends the turn without a done event: subscribers get err and a later
// resume gets it too.
func (t *Turn) abort(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished || t.aborted != nil {
		return
	}
	t.aborted = err
	t.finishedAt.Store(t.hub.now().UnixNano())
	t.wakeLocked()
}

// Fail ends the turn with an error event followed by a done event (finish
// reason failed), at the next sequence numbers. The error carries only its code,
// message and retryable flag: the details of an error may hold anything, and a
// stream is not the place for them. Fail returns the publish error, if any; the
// turn is abandoned (ErrAborted) when even that cannot be published.
func (t *Turn) Fail(cause error) error {
	code, msg := contracts.CodeOf(cause), "the turn failed"
	if code == "" {
		code = contracts.ErrInternal
	}
	var ce *contracts.Error
	if code != contracts.ErrInternal && errors.As(cause, &ce) && ce.Message != "" {
		msg = ce.Message
		if len(msg) > 2048 {
			msg = msg[:2048]
			for !utf8.ValidString(msg) {
				msg = msg[:len(msg)-1]
			}
		}
	} else if code == contracts.ErrInternal {
		msg = "internal error"
	}
	if t.Finished() {
		return nil
	}
	turn := actions.TurnID(t.id)
	err := t.Publish(conversation.TurnEvent{Value: conversation.ErrorEvent{
		TurnID: turn, Seq: conversation.Seq(t.NextSeq()),
		Error: common.Error{Code: common.ErrorCode(code), Message: msg, Retryable: code.Retryable()},
	}})
	if err == nil {
		err = t.Publish(conversation.TurnEvent{Value: conversation.DoneEvent{
			TurnID: turn, Seq: conversation.Seq(t.NextSeq()), FinishReason: conversation.DoneEventFinishReasonFailed,
		}})
	}
	if err != nil {
		t.hub.cfg.Logger.Error("turn could not be failed cleanly", "turn_id", t.id, "reason", string(ReasonOf(err)))
		t.abort(ErrAborted)
	}
	return err
}

// ---- encoding ----

type eventHead struct {
	typ    string
	turnID string
	seq    int64
}

// encodeEvent encodes an event as one line of JSON and reads its envelope. The
// generated encoder writes the schema's members only; the line has no raw
// newline (JSON escapes them), which is checked, because a frame is one SSE data
// line and one WebSocket message. The line is decoded once: the envelope is read
// from that tree, and the tree is returned for the credential check.
func encodeEvent(ev conversation.TurnEvent) ([]byte, eventHead, any, error) {
	data, err := json.Marshal(ev)
	if err != nil {
		return nil, eventHead{}, nil, err
	}
	if bytes.ContainsAny(data, "\n\r") {
		return nil, eventHead{}, nil, errors.New("the encoded event is not one line")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, eventHead{}, nil, err
	}
	m, isObject := tree.(map[string]any)
	if !isObject && tree != nil {
		return nil, eventHead{}, nil, errors.New("the encoded event is not an object")
	}
	// member is one envelope member as JSON (nil when absent), read below exactly
	// as a member of the line itself.
	member := func(name string) json.RawMessage {
		v, ok := m[name]
		if !ok {
			return nil
		}
		raw, _ := json.Marshal(v) // a decoded value always encodes
		return raw
	}
	var head eventHead
	if err := json.Unmarshal(member("type"), &head.typ); err != nil || !validEventType(head.typ) {
		return nil, eventHead{}, nil, errors.New("the event has no valid type")
	}
	if err := json.Unmarshal(member("turn_id"), &head.turnID); err != nil {
		return nil, eventHead{}, nil, errors.New("the event has no turn id")
	}
	var num json.Number
	if err := json.Unmarshal(member("seq"), &num); err != nil {
		return nil, eventHead{}, nil, errors.New("the event has no sequence number")
	}
	n, err := strconv.ParseInt(string(num), 10, 64)
	if err != nil || n < 0 || n > MaxSeq {
		return nil, eventHead{}, nil, errors.New("the event's sequence number is out of range")
	}
	head.seq = n
	return data, head, tree, nil
}

// validEventType accepts the schema's event names (lower-case words joined by
// underscores): the type becomes an SSE field and must not carry a newline.
func validEventType(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if (s[i] < 'a' || s[i] > 'z') && s[i] != '_' {
			return false
		}
	}
	return true
}

// credentialKeys are JSON member names that must never appear in an event, at
// any depth. The comparison ignores case, "_" and "-". The published schemas
// have no such member; the check is for the open maps (error details, chart
// records) a hostile or careless producer could put one in. It is an exact-name
// list, not the log redactor's substring rule, so a field that merely mentions
// a token count is not refused.
var credentialKeys = map[string]bool{
	"token": true, "confirmationtoken": true, "confirmationsecret": true, "contexttoken": true,
	"accesstoken": true, "refreshtoken": true, "idtoken": true, "bearer": true, "secret": true,
	"clientsecret": true, "password": true, "passwd": true, "authorization": true, "apikey": true,
	"cookie": true, "setcookie": true, "privatekey": true, "credential": true, "credentials": true,
}

// sensitiveKey returns the first credential-shaped member name in the decoded
// JSON document, or "".
func sensitiveKey(root any) string {
	var walk func(v any, depth int) string
	walk = func(v any, depth int) string {
		if depth > 64 {
			return "too_deep"
		}
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if credentialKeys[normalizeKey(k)] {
					return k
				}
				if bad := walk(e, depth+1); bad != "" {
					return bad
				}
			}
		case []any:
			for _, e := range x {
				if bad := walk(e, depth+1); bad != "" {
					return bad
				}
			}
		}
		return ""
	}
	return walk(root, 0)
}

func normalizeKey(k string) string {
	var sb []byte
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'A' && c <= 'Z':
			sb = append(sb, c+32)
		case c == '_' || c == '-':
		default:
			sb = append(sb, c)
		}
	}
	return string(sb)
}

// ---- subscriptions ----

// Subscription is one reader of a turn, positioned after a cursor. Next hands
// out the events in order, exactly once.
type Subscription struct {
	t *Turn
	// next is the seq of the next event to hand out.
	next int64
	// live is true once the subscriber has been within MaxLag of the newest
	// event; only a live subscriber is cut off for lagging.
	live bool
	err  error
	peak int64
	// complete is true when the cursor was at the end of a finished turn: the
	// subscriber has everything.
	complete bool
	closed   bool
}

// Subscribe positions a reader after the event with sequence number after
// (FromStart for the whole turn). It fails, and never returns an empty stream
// that looks like success, when the cursor is malformed, ahead of the stream or
// behind the retained window.
func (t *Turn) Subscribe(after int64) (*Subscription, error) {
	if after < FromStart || after > MaxSeq {
		return nil, ErrCursorMalformed
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.aborted != nil {
		return nil, t.aborted
	}
	if after >= t.next {
		return nil, ErrCursorAhead
	}
	if after+1 < t.first {
		return nil, ErrCursorExpired
	}
	if len(t.subs) >= t.hub.cfg.MaxSubscribers {
		return nil, ErrSubscribers
	}
	s := &Subscription{t: t, next: after + 1}
	if t.finished && s.next >= t.next {
		s.complete = true
		return s, nil
	}
	s.live = t.next-s.next <= int64(t.hub.cfg.MaxLag)
	t.subs[s] = struct{}{}
	return s, nil
}

// Complete reports whether the cursor was at the end of a finished turn: there
// is nothing more to send, and nothing was missed.
func (s *Subscription) Complete() bool { return s.complete }

// PeakLag is the most events this subscriber has been behind the newest while it
// was live. It never exceeds Config.MaxLag: past that it is cut off.
func (s *Subscription) PeakLag() int64 {
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	return s.peak
}

// Close releases the subscription. It is safe to call more than once.
func (s *Subscription) Close() {
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	if !s.closed {
		s.closed = true
		delete(s.t.subs, s)
	}
}

// Next returns the next event, waiting for it if the subscriber is caught up.
// It returns io.EOF after the done event, ErrSlowConsumer when the subscriber
// was cut off (or fell out of the retained window), the turn's abort error if
// the turn was abandoned, and ctx.Err() when ctx ends.
func (s *Subscription) Next(ctx context.Context) (Frame, error) {
	t := s.t
	for {
		t.mu.Lock()
		switch {
		case s.closed:
			t.mu.Unlock()
			return Frame{}, io.EOF
		case s.err != nil:
			err := s.err
			t.mu.Unlock()
			return Frame{}, err
		case s.next < t.first:
			// The events this subscriber needs were forgotten while it waited.
			s.err = ErrSlowConsumer
			delete(t.subs, s)
			t.mu.Unlock()
			return Frame{}, s.err
		case s.next < t.next:
			f := t.frames[s.next-t.first]
			s.next++
			if !s.live && t.next-s.next <= int64(t.hub.cfg.MaxLag) {
				s.live = true
			}
			t.mu.Unlock()
			return f, nil
		}
		// Caught up.
		s.live = true
		if t.finished {
			t.mu.Unlock()
			return Frame{}, io.EOF
		}
		if t.aborted != nil {
			err := t.aborted
			t.mu.Unlock()
			return Frame{}, err
		}
		ch := t.notify
		t.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		}
	}
}

// String describes a frame for logs: counts only.
func (f Frame) String() string {
	return fmt.Sprintf("frame seq=%d type=%s bytes=%d", f.Seq, f.Type, len(f.Data))
}
