package ragsec

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/rag/parse"
	fsprov "github.com/buxee/alix-ai/internal/rag/providers/fs"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

var bg = context.Background()

var (
	clockStart = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	// oldTime is the modification time of every fixture file, far enough behind
	// the fake clock that the provider trusts its record for the size-and-time
	// shortcut (no racy window).
	oldTime = time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)
)

// embedModel is the embedding model of every world: the fake embedder reports
// it, the vector generations are built with it and the query pipeline names it.
// A mismatch anywhere silently turns a hybrid run into an FTS-only one, which is
// why the tests that mean "hybrid" assert query.Diagnostics.Mode.
const embedModel = "test-embed-32"

const appID = "demo-library"

// sentinel is generated per process, never a literal: the tests plant it where
// no prompt may ever carry it (session ids, the credential file outside the
// source root, an environment variable).
var sentinel = func() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return strings.ToUpper(hex.EncodeToString(b[:]))
}()

// scopeFor is the verified scope of a principal in a tenant and workspace. The
// session id carries the sentinel, so a prompt that echoes the scope is caught.
func scopeFor(tenant, workspace, principal string) contracts.Scope {
	return contracts.Scope{
		DeploymentID:     "dep-1",
		ApplicationID:    appID,
		TenantID:         tenant,
		WorkspaceID:      workspace,
		PrincipalID:      principal,
		Roles:            []string{"member"},
		CapabilityScopes: []string{"rag.sync", "rag.query"},
		SessionID:        "ses-" + sentinel,
	}
}

// ---------------------------------------------------------------- options

type worldOpts struct {
	noEmbedder bool
	rerank     bool
	rewrite    bool
	overGrant  bool
	parserCfg  func(*parse.Config)
	queryCfg   func(*query.Config)
}

type worldOpt func(*worldOpts)

// ftsOnly leaves the Embedder role unregistered: queries run full-text only and
// the evidence package carries the reduced_semantic_recall notice.
func ftsOnly() worldOpt { return func(o *worldOpts) { o.noEmbedder = true } }

// withModelRoles turns on every optional model role of the query pipeline (the
// reranker and the model rewriter), so their prompts are captured too.
func withModelRoles() worldOpt {
	return func(o *worldOpts) { o.rerank, o.rewrite = true, true }
}

// overGranting makes the source catalog authorise any source for any scope. The
// catalog is the deterministic authority on sources; with it deliberately
// broken, whatever still keeps another tenant's text out of the result is the
// index and the vector store applying the scope themselves.
func overGranting() worldOpt { return func(o *worldOpts) { o.overGrant = true } }

func withParser(f func(*parse.Config)) worldOpt { return func(o *worldOpts) { o.parserCfg = f } }

func withQueryConfig(f func(*query.Config)) worldOpt { return func(o *worldOpts) { o.queryCfg = f } }

// ---------------------------------------------------------------- the world

// world is the whole RAG stack of one test over a throwaway database: the real
// source provider, the real sandboxed parser worker, the chunker, the FTS5 and
// flat vector indexes, the job queue, the query pipeline and the grounded
// answerer. Only the model is scripted (the fake provider, which records every
// prompt).
type world struct {
	t     *testing.T
	clk   *clock.Fake
	db    *store.DB
	q     *jobs.Queue
	pool  *jobs.Pool
	ix    *index.Index
	flat  *vector.Flat
	svc   *ingest.Service
	prov  *fsprov.Provider
	reg   *sourceRegistry
	cat   *catalog
	fp    *fake.Provider
	model *model
	pipe  *query.Pipeline
	ans   *answer.Answerer
	runr  *parse.Runner
	base  string
	log   *scopeLog
	opts  worldOpts

	srcs  []*src
	syncs int
}

func newWorld(t *testing.T, o ...worldOpt) *world {
	t.Helper()
	var opts worldOpts
	for _, m := range o {
		m(&opts)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, clk: clock.NewFake(clockStart), base: base, opts: opts, log: &scopeLog{}}
	w.db = storetest.Open(t, nil) // store.Default holds the migrations of every imported module

	pause := ingest.NewPause(ingest.DiskPressureFunc(func(context.Context) (bool, error) { return false, nil }), w.clk, 30*time.Second, nil)
	w.q, err = jobs.NewQueue(w.db, jobs.Options{Config: jobs.DefaultConfig(), Clock: w.clk, Policy: pause.Policy(nil), Jitter: func() float64 { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	w.ix, err = index.New(w.db, w.clk, index.Config{})
	if err != nil {
		t.Fatal(err)
	}
	vc := vector.DefaultConfig()
	vc.CacheMiB = 0
	w.flat, err = vector.NewFlat(w.db, vector.FlatOptions{Config: vc, Clock: w.clk})
	if err != nil {
		t.Fatal(err)
	}
	aud, err := audit.New(w.db, audit.Options{Clock: w.clk})
	if err != nil {
		t.Fatal(err)
	}
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	// The real parser worker, one disposable process per document.
	pcfg := parse.DefaultConfig()
	pcfg.WorkerPath = parserWorker
	pcfg.StartupTimeout = 90 * time.Second
	pcfg.WallTimeout = 60 * time.Second
	if opts.parserCfg != nil {
		opts.parserCfg(&pcfg)
	}
	w.runr, err = parse.NewRunner(pcfg)
	if err != nil {
		t.Fatalf("parse.NewRunner: %v", err)
	}
	t.Cleanup(func() { w.runr.Close() })

	w.fp = fake.New(fake.WithModel(embedModel))
	w.model = newModel()
	w.model.install(w.fp)

	w.prov = fsprov.New(fsprov.Config{Clock: w.clk})
	w.reg = &sourceRegistry{prov: w.prov, defs: map[string]source.Definition{}}
	w.cat = &catalog{w: w}

	w.svc, err = ingest.New(ingest.Deps{
		DB: w.db, Clock: w.clk, Queue: w.q, Index: w.ix, Vectors: w.flat,
		Embedding: &ingest.Embedding{Embedder: w.fp, Model: embedModel, Component: rag.ComponentVersion{Name: "fake-embed", Version: "1"}},
		Registry:  w.reg, Parser: ingest.NewRunnerParser(w.runr), Chunker: chk, Audit: aud, Pause: pause,
		Config: ingest.DefaultConfig(),
	})
	if err != nil {
		t.Fatalf("ingest.New: %v", err)
	}
	w.pool, err = jobs.NewPool(w.q, jobs.PoolOptions{ScopeChecker: allowScopes{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.Handle(w.pool); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := w.pool.Shutdown(sctx); err != nil {
			t.Errorf("pool shutdown: %v", err)
		}
	})

	// The query side. Every collaborator that takes a scope is wrapped so the
	// scope of every call is recorded.
	roles := providers.NewRegistry(w.clk)
	if !opts.noEmbedder {
		must(t, roles.Register(providers.RoleEmbedder, w.fp))
	}
	if opts.rerank {
		must(t, roles.Register(providers.RoleReranker, w.fp))
	}
	deps := query.Deps{
		Lexical: spyLex{Index: w.ix, log: w.log}, Hits: spyLex{Index: w.ix, log: w.log},
		Catalog: spyCatalog{c: w.cat, log: w.log}, Providers: roles, EmbeddingModel: embedModel, Clock: w.clk,
	}
	if !opts.noEmbedder {
		deps.Vectors = spyVec{Flat: w.flat, log: w.log}
	}
	qcfg := query.DefaultConfig()
	qcfg.Rerank = opts.rerank
	if opts.rewrite {
		must(t, roles.Register(providers.RoleRouter, w.fp))
		qcfg.Rewrite = true
		qcfg.RewriteBelowHits = query.MaxShortlistCeiling // always "low confidence": the rewriter runs on every query
		deps.Rewriter = query.ModelRewriter{Registry: roles}
	}
	if opts.queryCfg != nil {
		opts.queryCfg(&qcfg)
	}
	w.pipe, err = query.New(deps, qcfg)
	if err != nil {
		t.Fatalf("query.New: %v", err)
	}
	w.ans, err = answer.New(answer.Deps{Generator: w.fp, Catalog: spyCatalog{c: w.cat, log: w.log}, Clock: w.clk}, answer.DefaultConfig())
	if err != nil {
		t.Fatalf("answer.New: %v", err)
	}
	return w
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type allowScopes struct{}

func (allowScopes) CheckScope(context.Context, contracts.Scope) error { return nil }

// ---------------------------------------------------------------- sources

// src is a registered source: a folder in the test's temp directory, the scope
// it serves and its definition.
type src struct {
	w     *world
	id    string
	name  string
	scope contracts.Scope
	root  string
	// outside is a sibling of root: a place no source may read.
	outside string
	def     source.Definition
}

// sourceRegistry is the ingest.Registry of the world: it resolves a source id to
// its definition and the production folder provider, and refuses (ErrSourceGone)
// a job whose scope is not the one the definition serves.
type sourceRegistry struct {
	mu   sync.Mutex
	prov source.Provider
	defs map[string]source.Definition
}

func (r *sourceRegistry) Resolve(_ context.Context, scope contracts.Scope, id string) (ingest.Resolved, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.defs[id]
	if !ok || d.CheckScope(scope) != nil {
		return ingest.Resolved{}, ingest.ErrSourceGone
	}
	return ingest.Resolved{Def: d, Provider: r.prov}, nil
}

func (r *sourceRegistry) set(d source.Definition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs[d.SourceID] = d
}

// declaration is the package declaration of a folder source that accepts every
// format the parser reads.
func declaration(root string) ap.RAGSource {
	return ap.RAGSource{
		ID:    "docs",
		Type:  ap.RAGSourceTypeLocalFolder,
		Owner: ap.RAGSourceOwner{Name: "Test owner"},
		Scope: ap.RAGSourceScope{
			ApplicationID: appID,
			Tenant:        ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
			Workspace:     ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
		},
		Root:    ap.RAGSourceRoot{Value: ap.RAGSourceRootFolder{Kind: "folder", Path: root}},
		Include: []ap.GlobPattern{"**"},
		Exclude: []ap.GlobPattern{},
		Formats: []ap.MimeType{
			source.MIMEText, source.MIMEMarkdown, source.MIMEHTML, source.MIMECSV, source.MIMEDOCX, source.MIMEPDF,
		},
		Limits:   ap.RAGSourceLimits{MaxFileBytes: 8 << 20, MaxTotalBytes: 1 << 30, MaxFiles: 10000},
		Sync:     ap.RAGSourceSync{Method: ap.RAGSourceSyncMethodManual},
		Deletion: ap.RAGSourceDeletion{PurgeAfterHours: 24, OnSourceRemoved: ap.RAGSourceDeletionOnSourceRemovedPurge},
	}
}

// addSource registers a folder source for the scope. The folder is created
// empty; the caller writes files and syncs.
func (w *world) addSource(scope contracts.Scope, name string, mutate ...func(*ap.RAGSource)) *src {
	w.t.Helper()
	dir := filepath.Join(w.base, name)
	root, outside := filepath.Join(dir, "root"), filepath.Join(dir, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			w.t.Fatal(err)
		}
	}
	decl := declaration(root)
	title := ap.Label(name)
	decl.Title = &title
	for _, m := range mutate {
		m(&decl)
	}
	s := &src{w: w, id: ids.New(source.IDPrefix), name: name, scope: scope, root: root, outside: outside}
	def, err := source.FromDeclaration(decl, source.Binding{SourceID: s.id, TenantID: scope.TenantID, WorkspaceID: scope.WorkspaceID})
	if err != nil {
		w.t.Fatal(err)
	}
	s.def = def
	w.reg.set(def)
	w.cat.add(s)
	w.srcs = append(w.srcs, s)
	return s
}

// write creates a file under the root with an old modification time.
func (s *src) write(rel string, data []byte) {
	s.w.t.Helper()
	writeFile(s.w.t, s.root, rel, data)
}

func (s *src) writeString(rel, content string) { s.write(rel, []byte(content)) }

// edit rewrites a file the way an editor does: new content and a later
// modification time (the provider skips a file whose size and time are unchanged).
func (s *src) edit(rel, content string) {
	s.w.t.Helper()
	p := filepath.Join(s.root, filepath.FromSlash(rel))
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		s.w.t.Fatal(err)
	}
	later := oldTime.Add(24 * time.Hour)
	if err := os.Chtimes(p, later, later); err != nil {
		s.w.t.Fatal(err)
	}
}

func writeFile(t testing.TB, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
}

// sync queues a sync of the source and drains the queue until the pipeline is
// idle (every document parsed, indexed and embedded). It returns the sync job.
func (w *world) sync(s *src) jobs.Job {
	w.t.Helper()
	w.syncs++
	res, err := w.svc.RequestSync(bg, s.scope, s.id, ingest.SyncOptions{Key: fmt.Sprintf("sec:%d", w.syncs)})
	if err != nil {
		w.t.Fatalf("RequestSync: %v", err)
	}
	w.drain()
	got, err := w.q.Get(bg, s.scope, res.Job.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	return got
}

// syncOK is sync that also requires the sync job to have succeeded.
func (w *world) syncOK(s *src) {
	w.t.Helper()
	if j := w.sync(s); j.State != jobs.StateSucceeded {
		w.t.Fatalf("sync of %s ended %s: %s\n%s", s.name, j.State, j.ErrorMessage, w.dumpJobs())
	}
}

func (w *world) running() int {
	n := 0
	for _, c := range jobs.Classes() {
		n += w.pool.Running(c)
	}
	return n
}

func (w *world) pending() int {
	var n int
	row := w.db.QueryRowContext(bg, `SELECT count(*) FROM jobs WHERE state = 'running' OR (state = 'pending' AND (attempts > 0 OR run_at <= ?))`,
		w.clk.Now().UnixMilli())
	if err := row.Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *world) waitIdle() {
	w.t.Helper()
	deadline := time.Now().Add(120 * time.Second)
	for w.running() > 0 {
		if time.Now().After(deadline) {
			w.t.Fatal("job handlers did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func (w *world) poll() int {
	w.t.Helper()
	n, err := w.pool.Poll(bg)
	if err != nil {
		w.t.Fatalf("Poll: %v", err)
	}
	w.waitIdle()
	return n
}

// drain polls until the queue is empty. A job in its retry backoff is waited out
// by moving the fake clock.
func (w *world) drain() {
	w.t.Helper()
	stuck := 0
	for i := 0; i < 5000; i++ {
		n := w.poll()
		if w.pending() == 0 {
			return
		}
		if n == 0 {
			stuck++
			if stuck > 40 {
				w.t.Fatalf("the queue did not drain: %d jobs left\n%s", w.pending(), w.dumpJobs())
			}
			w.clk.Advance(30 * time.Second)
			continue
		}
		stuck = 0
	}
	w.t.Fatal("drain did not finish")
}

func (w *world) dumpJobs() string {
	rows, err := w.db.QueryContext(bg, `SELECT type, state, attempts, coalesce(error_code, ''), coalesce(error_message, '') FROM jobs ORDER BY seq`)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, state, code, msg string
		var attempts int
		if err := rows.Scan(&typ, &state, &attempts, &code, &msg); err != nil {
			return err.Error()
		}
		fmt.Fprintf(&b, "  %-24s %-9s attempts=%d %s %s\n", typ, state, attempts, code, msg)
	}
	return b.String()
}

// versionRow is what ingestion recorded about one item.
type versionRow struct {
	Key, State, Code, Reason string
}

// versions lists the current (not superseded) versions of the source's items.
func (w *world) versions(s *src) map[string]versionRow {
	w.t.Helper()
	rows, err := w.db.QueryContext(bg,
		`SELECT item_key, state, error_code, error_reason FROM ragingest_version WHERE source_id = ? AND state <> 'superseded'`, s.id)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]versionRow{}
	for rows.Next() {
		var v versionRow
		if err := rows.Scan(&v.Key, &v.State, &v.Code, &v.Reason); err != nil {
			w.t.Fatal(err)
		}
		out[v.Key] = v
	}
	return out
}

func (w *world) status(s *src) ingest.SourceInfo {
	w.t.Helper()
	st, err := w.svc.Status(bg, s.scope, s.id)
	if err != nil {
		w.t.Fatalf("Status: %v", err)
	}
	return st
}

// search is a hidden-inclusive lexical search of the source's scope, to look at
// what the index holds regardless of what retrieval would show.
func (w *world) search(scope contracts.Scope, text string, includeHidden bool) []index.Hit {
	w.t.Helper()
	res, err := w.ix.Search(bg, scope, index.Query{Text: text, Limit: 50, IncludeHidden: includeHidden})
	if err != nil {
		w.t.Fatalf("Search(%q): %v", text, err)
	}
	return res.Hits
}

// ---------------------------------------------------------------- the catalog

// catalog is the deterministic authority on sources of the world. It answers
// like the composition root's (internal/cli ragSources): a source is readable
// only by the scope it was registered under, and its state is read from the
// ingestion status, so a source that could not be reached is never labelled
// checked. With overGrant it is deliberately broken and authorises everything.
type catalog struct {
	w *world

	mu       sync.Mutex
	sources  map[string]*src
	override map[string]func(query.SourceState) query.SourceState
	err      error
}

// fail makes every read of the catalog fail (nil makes it work again).
func (c *catalog) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func (c *catalog) add(s *src) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sources == nil {
		c.sources = map[string]*src{}
	}
	c.sources[s.id] = s
}

// force replaces the state the catalog reports for a source, after it computed
// it: a test's way to say "the catalog now knows this".
func (c *catalog) force(s *src, f func(query.SourceState) query.SourceState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.override == nil {
		c.override = map[string]func(query.SourceState) query.SourceState{}
	}
	c.override[s.id] = f
}

func sameLevels(a, b contracts.Scope) bool {
	return a.ApplicationID == b.ApplicationID && a.TenantID == b.TenantID && a.WorkspaceID == b.WorkspaceID
}

func (c *catalog) Sources(ctx context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	failure := c.err
	c.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	out := map[string]query.SourceState{}
	for _, id := range sourceIDs {
		c.mu.Lock()
		s := c.sources[id]
		force := c.override[id]
		c.mu.Unlock()
		if c.w.opts.overGrant {
			out[id] = query.SourceState{
				DisplayName: id, Authorised: true,
				Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent,
			}
			continue
		}
		if s == nil || !sameLevels(s.scope, scope) {
			continue // not readable by this scope: indistinguishable from not existing
		}
		info, err := c.w.svc.Status(ctx, scope, id)
		var st query.SourceState
		switch {
		case err == nil:
			st = c.stateOf(s.def, info)
		case contracts.CodeOf(err) == contracts.ErrValidationFailed:
			st = query.SourceState{DisplayName: s.def.Title, Authorised: true} // registered, never scanned: nothing is claimed as checked
		default:
			return nil, err
		}
		if force != nil {
			st = force(st)
		}
		out[id] = st
	}
	return out, nil
}

// stateOf maps an ingestion status to what a citation may say, as the
// composition root does: removed, unavailable, or available with a staleness
// read from the sync schedule (never "current" for a source with no schedule).
func (c *catalog) stateOf(def source.Definition, info ingest.SourceInfo) query.SourceState {
	st := query.SourceState{DisplayName: def.Title, Authorised: true}
	switch {
	case info.Reason == ingest.ReasonRemoved || info.Reason == ingest.ReasonTenantDeleted:
		st.Availability = conversation.CitationAvailabilityRemoved
	case !info.Available:
		st.Availability = conversation.CitationAvailabilityUnavailable
	default:
		st.Availability = conversation.CitationAvailabilityAvailable
		st.Staleness = conversation.CitationStalenessUnknown
		if def.Sync.Method == ap.RAGSourceSyncMethodPoll && def.Sync.Interval > 0 && !info.LastSuccess.IsZero() {
			if c.w.clk.Now().Sub(info.LastSuccess) <= 2*def.Sync.Interval {
				st.Staleness = conversation.CitationStalenessCurrent
			} else {
				st.Staleness = conversation.CitationStalenessStale
			}
		}
	}
	return st
}

// ---------------------------------------------------------------- scope recording

// scopeLog records the scope of every call the query pipeline and the answerer
// make into a collaborator.
type scopeLog struct {
	mu      sync.Mutex
	entries []scopeEntry
}

type scopeEntry struct {
	Where string
	Scope contracts.Scope
	// Detail is what was asked: the text of a lexical search.
	Detail string
}

func (l *scopeLog) add(where string, s contracts.Scope) { l.addDetail(where, s, "") }

func (l *scopeLog) addDetail(where string, s contracts.Scope, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, scopeEntry{Where: where, Scope: s.Clone(), Detail: detail})
}

// searched are the texts the lexical index was asked to search, in order.
func (l *scopeLog) searched() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.entries {
		if e.Where == "lexical.search" {
			out = append(out, e.Detail)
		}
	}
	return out
}

func (l *scopeLog) all() []scopeEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]scopeEntry(nil), l.entries...)
}

func (l *scopeLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = nil
}

type spyLex struct {
	*index.Index
	log *scopeLog
}

func (s spyLex) Search(ctx context.Context, scope contracts.Scope, q index.Query) (index.Result, error) {
	s.log.addDetail("lexical.search", scope, q.Text)
	return s.Index.Search(ctx, scope, q)
}

func (s spyLex) Version(ctx context.Context, scope contracts.Scope) (index.Version, error) {
	s.log.add("lexical.version", scope)
	return s.Index.Version(ctx, scope)
}

func (s spyLex) ParentOf(ctx context.Context, scope contracts.Scope, h index.Hit, includeHidden bool) (index.Parent, bool, error) {
	s.log.add("lexical.parent", scope)
	return s.Index.ParentOf(ctx, scope, h, includeHidden)
}

func (s spyLex) Recheck(ctx context.Context, scope contracts.Scope, refs []index.Ref, includeHidden bool) ([]index.Verdict, error) {
	s.log.add("lexical.recheck", scope)
	return s.Index.Recheck(ctx, scope, refs, includeHidden)
}

func (s spyLex) Hits(ctx context.Context, scope contracts.Scope, refs []index.Ref, includeHidden bool) ([]index.Hit, error) {
	s.log.add("lexical.hits", scope)
	return s.Index.Hits(ctx, scope, refs, includeHidden)
}

type spyVec struct {
	*vector.Flat
	log *scopeLog
}

func (s spyVec) Search(ctx context.Context, scope contracts.Scope, q vector.Query) (vector.SearchResult, error) {
	s.log.add("vector.search", scope)
	return s.Flat.Search(ctx, scope, q)
}

type spyCatalog struct {
	c   *catalog
	log *scopeLog
}

func (s spyCatalog) Sources(ctx context.Context, scope contracts.Scope, sourceIDs []string) (map[string]query.SourceState, error) {
	s.log.add("catalog.sources", scope)
	return s.c.Sources(ctx, scope, sourceIDs)
}

// ---------------------------------------------------------------- asking

// asked is the outcome of one question through retrieval and answering.
type asked struct {
	Question string
	Package  rag.EvidencePackage
	QDiag    query.Diagnostics
	QErr     error
	Answer   answer.Result
	AErr     error
}

// ask runs one question: the query pipeline, then the answerer on the package it
// built. A query that failed is not answered.
func (w *world) ask(scope contracts.Scope, question string, sourceIDs ...string) asked {
	w.t.Helper()
	a := asked{Question: question}
	res, err := w.pipe.Query(bg, scope, query.Request{Text: question, SourceIDs: sourceIDs})
	a.Package, a.QDiag, a.QErr = res.Package, res.Diagnostics, err
	if err != nil {
		return a
	}
	a.Answer, a.AErr = w.ans.Answer(bg, scope, res.Package)
	return a
}

// mustAsk is ask that requires both stages to have succeeded.
func (w *world) mustAsk(scope contracts.Scope, question string, sourceIDs ...string) asked {
	w.t.Helper()
	a := w.ask(scope, question, sourceIDs...)
	if a.QErr != nil {
		w.t.Fatalf("Query(%q): %v", question, a.QErr)
	}
	if a.AErr != nil {
		w.t.Fatalf("Answer(%q): %v", question, a.AErr)
	}
	return a
}

// texts of the package, in rank order.
func (a asked) evidenceText() string {
	var sb strings.Builder
	for _, it := range a.Package.Items {
		sb.WriteString(it.Text)
		sb.WriteString("\n")
	}
	return sb.String()
}

// everything is every string a client could have been shown for the question:
// the evidence package, the answer and its citations, as JSON.
func (a asked) everything(t testing.TB) string {
	t.Helper()
	var sb strings.Builder
	for _, v := range []any{a.Package, a.Answer.Answer, a.Answer.Citations} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(b)
		sb.WriteString("\n")
	}
	return sb.String()
}

func (a asked) grounded() (rag.GroundedAnswer, bool) {
	ga, ok := a.Answer.Answer.Value.(rag.GroundedAnswer)
	return ga, ok
}

func (a asked) noEvidence() (rag.NoSupportedEvidence, bool) {
	ns, ok := a.Answer.Answer.Value.(rag.NoSupportedEvidence)
	return ns, ok
}

func (a asked) claimTexts() []string {
	ga, ok := a.grounded()
	if !ok {
		return nil
	}
	out := make([]string, len(ga.Claims))
	for i, c := range ga.Claims {
		out[i] = c.Text
	}
	return out
}

// citedSources are the ids of the sources the answer's citations name.
func (a asked) citedSources() []string {
	ga, ok := a.grounded()
	if !ok {
		return nil
	}
	var out []string
	for _, c := range ga.Citations {
		out = append(out, string(c.SourceID))
	}
	sort.Strings(out)
	return out
}

func (a asked) sourcesInPackage() map[string]bool {
	out := map[string]bool{}
	for _, it := range a.Package.Items {
		out[string(it.SourceID)] = true
	}
	return out
}

// ---------------------------------------------------------------- prompts

// answerPrompts are the captured chat calls of the grounded answer (controller
// role).
func (w *world) answerPrompts() []fake.Prompt {
	var out []fake.Prompt
	for _, p := range w.fp.Prompts() {
		if p.Op == fake.OpChat && p.Chat.EffectiveRole() == providers.RoleController {
			out = append(out, p)
		}
	}
	return out
}

// system is the trusted system message of a chat prompt, and user the untrusted
// one.
func promptSystem(p fake.Prompt) string {
	for _, m := range p.Chat.Messages {
		if m.Role == providers.MsgSystem {
			return m.Content
		}
	}
	return ""
}

func promptUser(p fake.Prompt) string {
	for _, m := range p.Chat.Messages {
		if m.Role == providers.MsgUser {
			return m.Content
		}
	}
	return ""
}

// outsideFences is everything a chat prompt carries that is not the text of an
// <evidence> or <question> block: the system message, the output schema and the
// frame of the user message. It is the part no document may reach.
func outsideFences(p fake.Prompt) string {
	var sb strings.Builder
	for _, m := range p.Chat.Messages {
		if m.Role == providers.MsgSystem {
			sb.WriteString(m.Content)
			sb.WriteString("\n")
		}
	}
	sb.WriteString(string(p.Chat.Output.JSONSchema))
	sb.WriteString("\n")
	sb.WriteString(fenceRE.ReplaceAllString(promptUser(p), "<fence/>"))
	return sb.String()
}

// requireNoPromptContains fails when any captured call, of any kind (answer,
// embedding, rerank, rewrite, at ingestion or at query time) sent sub.
func (w *world) requireNoPromptContains(sub, what string) {
	w.t.Helper()
	for _, p := range w.fp.Prompts() {
		if p.Contains(sub) {
			w.t.Errorf("%s: %q reached the model in call #%d (%s, tenant %s)", what, sub, p.Seq, p.Op, p.Scope.TenantID)
		}
	}
}

// ---------------------------------------------------------------- misc

func itoa(n int) string { return strconv.Itoa(n) }

func containsAny(haystack string, needles ...string) (string, bool) {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return n, true
		}
	}
	return "", false
}
