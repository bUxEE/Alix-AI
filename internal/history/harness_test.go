package history

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Identifiers of the synthetic deployment the tests run against.
const (
	testApp   = "app_demo"
	otherApp  = "app_other"
	tenA      = "ten_a"
	tenB      = "ten_b"
	ws1       = "wsp_1"
	ws2       = "wsp_2"
	alice     = "usr_alice"
	bob       = "usr_bob"
	testDepID = "dep_test"
)

var t0 = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)

// userCaps are the capabilities of an ordinary signed-in user of a host that
// enabled the history API.
var userCaps = []string{CapRead, CapSearch, CapClear, CapExport}

// adminCaps are what the host grants to the person who manages history.
var adminCaps = []string{CapRead, CapSearch, CapClear, CapExport, CapCleanup, CapRetention}

// scopeOf returns a valid scope.
func scopeOf(app, tenant, ws, principal, session string, caps ...string) contracts.Scope {
	if len(caps) == 0 {
		caps = userCaps
	}
	return contracts.Scope{
		DeploymentID:     testDepID,
		ApplicationID:    app,
		TenantID:         tenant,
		WorkspaceID:      ws,
		PrincipalID:      principal,
		Roles:            []string{"member"},
		CapabilityScopes: append([]string(nil), caps...),
		SessionID:        session,
	}
}

// with returns sc with other capability scopes.
func with(sc contracts.Scope, caps ...string) contracts.Scope {
	sc = sc.Clone()
	sc.CapabilityScopes = append([]string(nil), caps...)
	return sc
}

// env is a throwaway store with the real session store, the real audit log and a
// history service on them.
type env struct {
	t    *testing.T
	db   *store.DB
	clk  *clock.Fake
	sess *sessions.Store
	alog *audit.Log
	svc  *Service
	q    *jobs.Queue
	n    int
}

type envOpts struct {
	deps func(*Deps)
	path string
}

type envOpt func(*envOpts)

// withDeps adjusts the dependencies the service is built with.
func withDeps(f func(*Deps)) envOpt { return func(o *envOpts) { o.deps = f } }

// withPath opens (and migrates) the database file at path instead of a new one.
func withPath(path string) envOpt { return func(o *envOpts) { o.path = path } }

func newEnv(t *testing.T, opts ...envOpt) *env {
	t.Helper()
	var o envOpts
	for _, f := range opts {
		f(&o)
	}
	e := &env{t: t, clk: clock.NewFake(t0)}
	e.db = storetest.Open(t, nil, func(so *store.Options) {
		if o.path != "" {
			so.Path = o.path
		}
	})
	var err error
	if e.alog, err = audit.New(e.db, audit.Options{Clock: e.clk}); err != nil {
		t.Fatal(err)
	}
	if e.sess, err = sessions.New(e.db, e.clk, sessions.Config{}); err != nil {
		t.Fatal(err)
	}
	if e.q, err = jobs.NewQueue(e.db, jobs.Options{Clock: e.clk}); err != nil {
		t.Fatal(err)
	}
	d := Deps{DB: e.db, Transcripts: e.sess, Audit: e.alog, Clock: e.clk, Queue: e.q, DeploymentID: testDepID}
	if o.deps != nil {
		o.deps(&d)
	}
	if e.svc, err = New(d); err != nil {
		t.Fatal(err)
	}
	return e
}

// say stores one turn of the session of sc through the session store, the way
// the dialogue engine does: the transcript is written with the state. An empty
// user or assistant text is a turn with one row only.
func (e *env) say(sc contracts.Scope, user, assistant string) string {
	e.t.Helper()
	ctx := context.Background()
	sess, err := e.sess.Open(ctx, sc)
	if err != nil {
		e.t.Fatalf("Open: %v", err)
	}
	e.n++
	id := fmt.Sprintf("trn_%s_%s_%d", sc.TenantID, sc.SessionID, e.n)
	if _, err := e.sess.Save(ctx, sess, &sessions.TurnRecord{TurnID: id, User: user, Act: "answer", Assistant: assistant}); err != nil {
		e.t.Fatalf("Save: %v", err)
	}
	return id
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// transcriptRows is how many transcript rows the whole database holds.
func (e *env) transcriptRows() int { return e.count(`SELECT COUNT(*) FROM transcript_turns`) }

// ftsRows is how many entries the search index resolves to a transcript row for
// the given word, across every scope.
func (e *env) ftsMatches(word string) int {
	e.t.Helper()
	return e.count(`SELECT COUNT(*) FROM history_fts WHERE history_fts MATCH ?`, `"`+word+`"`)
}

// checkIndex asks FTS5 to verify its index against the transcript table.
func (e *env) checkIndex() {
	e.t.Helper()
	if _, err := e.db.ExecContext(context.Background(), `INSERT INTO history_fts (history_fts, rank) VALUES ('integrity-check', 1)`); err != nil {
		e.t.Fatalf("the search index is out of step with the transcript: %v", err)
	}
}

func wantCode(t testing.TB, err error, code contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", code)
	}
	if got := contracts.CodeOf(err); got != code {
		t.Fatalf("error code = %s (%v), want %s", got, err, code)
	}
}

func noErr(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---- doubles ----

// failingAudit refuses every append: the audit log is down.
type failingAudit struct{ audit.Appender }

var errAuditDown = errors.New("audit is down")

func (failingAudit) AppendAt(context.Context, audit.Locator, audit.NewEvent) (audit.Event, error) {
	return audit.Event{}, contracts.Wrap(contracts.ErrUnavailable, errAuditDown, "audit down")
}

func (failingAudit) AppendTx(context.Context, *store.Tx, audit.Locator, audit.NewEvent) (audit.Event, error) {
	return audit.Event{}, contracts.Wrap(contracts.ErrUnavailable, errAuditDown, "audit down")
}

// recordingAudit delegates to a real appender and remembers the order of what it
// was asked to append.
type recordingAudit struct {
	audit.Appender
	mu    sync.Mutex
	types []audit.EventType
}

func (r *recordingAudit) AppendAt(ctx context.Context, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	r.mu.Lock()
	r.types = append(r.types, ev.Type)
	r.mu.Unlock()
	return r.Appender.AppendAt(ctx, loc, ev)
}

func (r *recordingAudit) AppendTx(ctx context.Context, tx *store.Tx, loc audit.Locator, ev audit.NewEvent) (audit.Event, error) {
	r.mu.Lock()
	r.types = append(r.types, ev.Type)
	r.mu.Unlock()
	return r.Appender.AppendTx(ctx, tx, loc, ev)
}

func (r *recordingAudit) seen() []audit.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.EventType(nil), r.types...)
}

// spyTranscripts counts the calls of the session store that delete, so a test
// can prove the caller was refused before any of them was made.
type spyTranscripts struct {
	TranscriptStore
	mu     sync.Mutex
	clears int
	prunes int
	order  *[]string
}

func (s *spyTranscripts) ClearTranscript(ctx context.Context, sc contracts.Scope) (int64, error) {
	s.mu.Lock()
	s.clears++
	if s.order != nil {
		*s.order = append(*s.order, "clear")
	}
	s.mu.Unlock()
	return s.TranscriptStore.ClearTranscript(ctx, sc)
}

func (s *spyTranscripts) PruneTranscript(ctx context.Context, sc contracts.Scope, cutoff time.Time) (int64, error) {
	s.mu.Lock()
	s.prunes++
	if s.order != nil {
		*s.order = append(*s.order, "prune")
	}
	s.mu.Unlock()
	return s.TranscriptStore.PruneTranscript(ctx, sc, cutoff)
}

func (s *spyTranscripts) calls() (clears, prunes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clears, s.prunes
}
