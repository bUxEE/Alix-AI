package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// protocolVersion is the MCP protocol revision the adapter asks for. It is the
// stable revision before 2026-07-28, which makes the SDK serve tool lists from a
// cache and add per-request metadata: the adapter wants every tool list to be
// the server's answer of this moment, because the pins are checked against it.
const protocolVersion = "2025-11-25"

// resultMetaKey is the optional member of a tool result's _meta in which a
// server echoes the idempotency key of the call it answers. When present it must
// equal the key that was sent; a result for another key is a result for another
// request.
const resultMetaKey = "alixai/idempotency_key"

// errAdapterClosed is the cause of the unavailable error of a closed adapter.
var errAdapterClosed = errors.New("the adapter is closed")

// liveTool is one tool as the last listing showed it.
type liveTool struct {
	hash  string
	input any
}

// listing is the result of one tools/list walk.
type listing struct {
	// tools holds the tools whose names are valid tool names, by name.
	tools map[string]liveTool
	// ambiguous names were listed twice with different definitions.
	ambiguous map[string]bool
	// odd describes the tools whose names are not valid tool names (control
	// characters, spaces, homoglyphs...), in a form safe to report.
	odd []string
}

// session is one open MCP session.
type session struct {
	ep    *endpointConn
	cs    *mcp.ClientSession
	extra io.Closer // the stdio process, if any
	dead  atomic.Bool

	mu       sync.Mutex
	users    int
	retired  bool
	closed   bool
	forced   bool
	calls    map[int]context.CancelCauseFunc
	nextCall int
}

// errSessionClosed is the cause of a call that ended because its session was closed
// under it (the adapter was closed, or superseded).
var errSessionClosed = errors.New("the session was closed")

// bind derives the context of one use of the session: it ends when ctx does, and
// when the session is closed by force, which the SDK would not otherwise tell a
// call that waits for an answer. It carries the use's own redirect flag (see
// redirected).
func (s *session) bind(ctx context.Context) (context.Context, func()) {
	bound, cancel := context.WithCancelCause(context.WithValue(ctx, redirectKey{}, new(atomic.Bool)))
	s.mu.Lock()
	if s.forced {
		s.mu.Unlock()
		cancel(errSessionClosed)
		return bound, func() {}
	}
	if s.calls == nil {
		s.calls = map[int]context.CancelCauseFunc{}
	}
	s.nextCall++
	id := s.nextCall
	s.calls[id] = cancel
	s.mu.Unlock()
	return bound, func() {
		s.mu.Lock()
		delete(s.calls, id)
		s.mu.Unlock()
		cancel(nil)
	}
}

// use registers one more user of the session; it fails for a session that is
// being retired.
func (s *session) use() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retired {
		return false
	}
	s.users++
	return true
}

// release ends one use. A retired session closes when its last user is done.
func (s *session) release() {
	s.mu.Lock()
	s.users--
	last := s.retired && s.users == 0
	s.mu.Unlock()
	if last {
		go s.shutdown()
	}
}

// retire stops new users and closes the session once the current ones are done.
func (s *session) retire() {
	s.mu.Lock()
	s.retired = true
	idle := s.users == 0
	s.mu.Unlock()
	if idle {
		go s.shutdown()
	}
}

// force closes the session now, failing the calls still in flight.
func (s *session) force() {
	s.mu.Lock()
	s.retired, s.forced = true, true
	cancels := s.calls
	s.calls = nil
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel(errSessionClosed)
	}
	s.shutdown()
}

func (s *session) shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.cs.Close()
	if s.extra != nil {
		_ = s.extra.Close()
	}
}

// endpointConn is one MCP server: its session, opened lazily and replaced when
// it breaks, and what is known about its tools.
type endpointConn struct {
	a      *Adapter
	cfg    Endpoint
	http   *http.Client
	client *mcp.Client
	trips  atomic.Int64

	// mapped is the set of tools a capability maps on this endpoint, managed the
	// set of management tools, approved the tools the adapter configuration
	// lists (nil: no list).
	mapped   map[string]bool
	managed  map[string]bool
	approved map[string]bool

	connSlot    chan struct{}
	refreshSlot chan struct{}

	mu     sync.Mutex
	sess   *session
	closed bool

	smu       sync.Mutex
	listed    bool
	listedAt  time.Time
	stale     bool
	live      listing
	pins      map[string]string
	drifts    map[string]Drift
	serverVer string
}

func (e *endpointConn) markStale() {
	e.smu.Lock()
	e.stale = true
	e.smu.Unlock()
}

// acquire returns a session in use, opening one when there is none or the last
// one broke. A failure to open is always unavailable: nothing of the caller's
// request has been sent.
func (e *endpointConn) acquire(ctx context.Context) (*session, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, contracts.Wrap(contracts.ErrUnavailable, errAdapterClosed, "the application's adapter was closed")
	}
	if s := e.sess; s != nil && !s.dead.Load() && s.use() {
		e.mu.Unlock()
		return s, nil
	}
	e.mu.Unlock()
	return e.connect(ctx)
}

// connect opens a session. Opens are serialised: callers that arrive while one is
// in progress wait for it and share its session.
func (e *endpointConn) connect(ctx context.Context) (*session, error) {
	select {
	case e.connSlot <- struct{}{}:
		defer func() { <-e.connSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, contracts.Wrap(contracts.ErrUnavailable, errAdapterClosed, "the application's adapter was closed")
	}
	if s := e.sess; s != nil && !s.dead.Load() && s.use() {
		e.mu.Unlock()
		return s, nil // another caller opened it while this one waited
	}
	e.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, e.a.cfg.ConnectTimeout)
	defer cancel()

	var (
		tr    mcp.Transport
		extra io.Closer
		err   error
	)
	switch {
	case e.a.cfg.connect != nil:
		tr, err = e.a.cfg.connect(cctx, e)
	case e.cfg.URL != "":
		tr = e.a.httpTransport(e)
	default:
		var proc *stdioProc
		tr, proc, err = startStdio(e.cfg.Command, transportCap(e.a.cfg.MaxResponseBytes), &e.trips)
		if proc != nil {
			extra = proc
		}
	}
	if err != nil {
		return nil, e.a.unavailable(ctx, err, "the application could not be reached")
	}
	cs, err := e.client.Connect(cctx, tr, &mcp.ClientSessionOptions{ProtocolVersion: protocolVersion})
	if err != nil {
		if extra != nil {
			_ = extra.Close()
		}
		return nil, e.a.unavailable(ctx, err, "the application did not accept a session")
	}
	s := &session{ep: e, cs: cs, extra: extra, users: 1}
	go func() {
		_ = cs.Wait()
		s.dead.Store(true)
	}()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		s.force()
		return nil, contracts.Wrap(contracts.ErrUnavailable, errAdapterClosed, "the application's adapter was closed")
	}
	old := e.sess
	e.sess = s
	e.mu.Unlock()
	if old != nil {
		old.retire()
	}
	if res := cs.InitializeResult(); res != nil && res.ServerInfo != nil {
		e.smu.Lock()
		e.serverVer = res.ServerInfo.Version
		e.smu.Unlock()
	}
	return s, nil
}

// discard drops a session that failed, so the next call opens a new one. The
// session closes once its other users are done.
func (e *endpointConn) discard(s *session) {
	e.mu.Lock()
	if e.sess == s {
		e.sess = nil
	}
	e.mu.Unlock()
	s.retire()
}

// close ends the endpoint's session now.
func (e *endpointConn) close() {
	e.mu.Lock()
	e.closed = true
	s := e.sess
	e.sess = nil
	e.mu.Unlock()
	if s != nil {
		s.force()
	}
	if e.http != nil {
		e.http.CloseIdleConnections()
	}
}

// ---- the surface ------------------------------------------------------------

// surfaceStale reports whether the listing must be refreshed before a call.
func (e *endpointConn) surfaceStale() bool {
	e.smu.Lock()
	defer e.smu.Unlock()
	if !e.listed || e.stale {
		return true
	}
	if e.a.cfg.SurfaceRecheck < 0 {
		return true
	}
	return e.a.cfg.Clock.Now().Sub(e.listedAt) >= e.a.cfg.SurfaceRecheck
}

// ensureSurface refreshes the listing when it is stale.
func (e *endpointConn) ensureSurface(ctx context.Context, s *session) error {
	if !e.surfaceStale() {
		return nil
	}
	return e.refresh(ctx, s, false)
}

// refresh lists the server's tools and checks them against the pins. One
// refresh runs at a time; a caller that waited for another one uses its answer,
// unless force is set (Describe asks for the server's answer of this moment).
func (e *endpointConn) refresh(ctx context.Context, s *session, force bool) error {
	select {
	case e.refreshSlot <- struct{}{}:
		defer func() { <-e.refreshSlot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if !force && !e.surfaceStale() {
		return nil
	}
	tripsBefore := e.trips.Load()
	bound, done := s.bind(ctx)
	defer done()
	l, err := listTools(bound, s.cs)
	if err != nil {
		return e.a.listFailure(ctx, s, err, tripsBefore)
	}
	for _, d := range e.apply(l) {
		e.a.raiseDrift(d)
	}
	return nil
}

var errTooManyTools = errors.New("the server lists an unreasonable number of tools")

// listTools walks the server's tool list.
func listTools(ctx context.Context, cs *mcp.ClientSession) (listing, error) {
	l := listing{tools: map[string]liveTool{}, ambiguous: map[string]bool{}}
	cursor, count := "", 0
	for page := 0; ; page++ {
		if page >= maxListPages {
			return listing{}, errTooManyTools
		}
		res, err := cs.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return listing{}, err
		}
		for _, t := range res.Tools {
			if t == nil {
				continue
			}
			if count++; count > maxListedTools {
				return listing{}, errTooManyTools
			}
			h, err := toolHash(t.Name, t.InputSchema, t.OutputSchema)
			if err != nil {
				return listing{}, fmt.Errorf("a tool's schema cannot be encoded: %w", err)
			}
			if !toolNameRE.MatchString(t.Name) {
				l.odd = append(l.odd, oddName(t.Name, h))
				continue
			}
			if prev, dup := l.tools[t.Name]; dup {
				if prev.hash != h {
					l.ambiguous[t.Name] = true
				}
				continue
			}
			l.tools[t.Name] = liveTool{hash: h, input: t.InputSchema}
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			break
		}
		cursor = res.NextCursor
	}
	sort.Strings(l.odd)
	return l, nil
}

// oddName is how a tool whose name is not a valid tool name is reported: its
// length and a prefix of its hash, never the name itself.
func oddName(name, hash string) string {
	return fmt.Sprintf("(a tool with an unusable name: %d bytes, hash %s)", len(name), hash[:12])
}

// apply installs a listing and returns the drifts it revealed.
func (e *endpointConn) apply(l listing) []Drift {
	now := e.a.cfg.Clock.Now()
	e.smu.Lock()
	defer e.smu.Unlock()
	e.live, e.listed, e.listedAt, e.stale = l, true, now, false

	var found []Drift
	record := func(d Drift) {
		if _, dup := e.drifts[d.Tool]; dup {
			return
		}
		d.Endpoint, d.At = e.cfg.ID, now
		e.drifts[d.Tool] = d
		found = append(found, d)
	}
	names := make([]string, 0, len(e.mapped)+len(e.managed))
	for n := range e.mapped {
		names = append(names, n)
	}
	for n := range e.managed {
		if !e.mapped[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		lt, present := l.tools[n]
		pin, pinned := e.pins[n]
		switch {
		case l.ambiguous[n]:
			record(Drift{Tool: n, Kind: DriftAmbiguousName, Pinned: pin})
		case !present:
			if pinned {
				record(Drift{Tool: n, Kind: DriftToolMissing, Pinned: pin})
			}
		case !pinned:
			e.pins[n] = lt.hash
		case pin != lt.hash:
			record(Drift{Tool: n, Kind: DriftSchemaChanged, Pinned: pin, Observed: lt.hash})
		}
	}
	if e.cfg.ContractSHA256 != "" {
		var tools []ContractTool
		for n := range e.mapped {
			if lt, ok := l.tools[n]; ok && !l.ambiguous[n] {
				tools = append(tools, ContractTool{Name: n, InputSchema: lt.input})
			}
		}
		got, err := ContractSHA256(tools)
		if err != nil || got != e.cfg.ContractSHA256 {
			record(Drift{Kind: DriftContractHash, Pinned: e.cfg.ContractSHA256, Observed: got})
		}
	}
	return found
}

// toolState is what a call needs to know about a tool before it is sent.
type toolState int

const (
	toolOK toolState = iota
	toolDrifted
	toolAbsent
)

// state reports whether a tool may be called now.
func (e *endpointConn) state(tool string) (toolState, Drift) {
	e.smu.Lock()
	defer e.smu.Unlock()
	if d, ok := e.drifts[tool]; ok {
		return toolDrifted, d
	}
	if d, ok := e.drifts[""]; ok && e.mapped[tool] {
		return toolDrifted, d
	}
	if _, ok := e.live.tools[tool]; !ok || e.live.ambiguous[tool] {
		return toolAbsent, Drift{}
	}
	return toolOK, Drift{}
}

// unmappedNames lists what the server offers that nobody maps or approved: the
// names the integration audit reports as undocumented or ambiguous.
func (e *endpointConn) unmappedNames() []string {
	e.smu.Lock()
	defer e.smu.Unlock()
	var out []string
	for n := range e.live.tools {
		if e.mapped[n] || e.managed[n] || e.approved[n] {
			continue
		}
		out = append(out, n)
	}
	out = append(out, e.live.odd...)
	return out
}

// applicationVersion is the version label of the server's MCP handshake, if it
// is plain text.
func (e *endpointConn) applicationVersion() string {
	e.smu.Lock()
	defer e.smu.Unlock()
	if v := e.serverVer; v != "" && shortPrintable(v, 256) {
		return v
	}
	return ""
}

// shortPrintable reports whether s is printable text of at most max characters.
func shortPrintable(s string, max int) bool {
	if utf8.RuneCountInString(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// snapshotPins copies the pins.
func (e *endpointConn) snapshotPins() map[string]string {
	e.smu.Lock()
	defer e.smu.Unlock()
	out := make(map[string]string, len(e.pins))
	for k, v := range e.pins {
		out[k] = v
	}
	return out
}
