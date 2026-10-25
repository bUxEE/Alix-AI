package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/parse"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	fsprov "github.com/buxee/alix-ai/internal/rag/providers/fs"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

var bg = context.Background()

var (
	clockStart = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	oldTime    = time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)
)

// ---------------------------------------------------------------- stand-ins

// stubParser is a deterministic Parser that starts no process. It splits text
// into paragraphs (blank-line separated) and "# " headings, counts its calls
// and lets a test change the parser version it reports.
type stubParser struct {
	calls   atomic.Int64
	mu      sync.Mutex
	version string
	fail    func(doc []byte) error
	probe   func() (parse.SandboxReport, error)
}

func newStubParser() *stubParser { return &stubParser{version: "1.0.0"} }

func (p *stubParser) setVersion(v string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.version = v
}

func (p *stubParser) Component() rag.ComponentVersion {
	p.mu.Lock()
	defer p.mu.Unlock()
	return rag.ComponentVersion{Name: "stubparse", Version: actions.OpaqueVersion(p.version)}
}

func (p *stubParser) Probe(context.Context) (parse.SandboxReport, error) {
	if p.probe != nil {
		return p.probe()
	}
	return parse.SandboxReport{Level: parse.LevelL2, Platform: "test"}, nil
}

func (p *stubParser) Parse(ctx context.Context, f wire.Format, doc []byte) (*parse.Result, error) {
	p.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.fail != nil {
		if err := p.fail(doc); err != nil {
			return nil, err
		}
	}
	text := string(doc)
	var blocks []wire.Block
	pos := 0
	for _, para := range strings.Split(text, "\n\n") {
		start, end := pos, pos+len(para)
		pos = end + 2
		if strings.TrimSpace(para) == "" {
			continue
		}
		b := wire.Block{Kind: wire.KindParagraph, Start: start, End: end}
		if title, ok := strings.CutPrefix(para, "# "); ok && !strings.Contains(para, "\n") {
			b = wire.Block{Kind: wire.KindHeading, Level: 1, Title: title, Start: start, End: end}
		}
		blocks = append(blocks, b)
	}
	return &parse.Result{Parser: p.Component(), Format: f, Text: text, Blocks: blocks}, nil
}

// gateEmbedder wraps the fake embedding runtime: it counts the inputs it embeds
// and can be told to be down. A hook runs inside every call, as if the model
// were still working on it.
type gateEmbedder struct {
	inner  *fake.Provider
	down   atomic.Bool
	calls  atomic.Int64
	inputs atomic.Int64
	hook   atomic.Pointer[func(providers.EmbedRequest)]
}

func (g *gateEmbedder) Embed(ctx context.Context, scope contracts.Scope, req providers.EmbedRequest) (providers.EmbedResult, error) {
	g.calls.Add(1)
	if h := g.hook.Load(); h != nil {
		(*h)(req)
	}
	if g.down.Load() {
		return providers.EmbedResult{}, contracts.New(contracts.ErrUnavailable, "the embedding runtime is down")
	}
	g.inputs.Add(int64(len(req.Inputs)))
	return g.inner.Embed(ctx, scope, req)
}

// testRegistry resolves sources to a definition and the fs provider.
type testRegistry struct {
	mu   sync.Mutex
	prov source.Provider
	defs map[string]source.Definition
}

func (r *testRegistry) Resolve(_ context.Context, _ contracts.Scope, id string) (Resolved, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.defs[id]
	if !ok {
		return Resolved{}, ErrSourceGone
	}
	return Resolved{Def: d, Provider: r.prov}, nil
}

func (r *testRegistry) set(d source.Definition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defs[d.SourceID] = d
}

func (r *testRegistry) drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.defs, id)
}

type pressure struct {
	mu    sync.Mutex
	under bool
	err   error
	asked int
}

func (p *pressure) UnderPressure(context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked++
	return p.under, p.err
}

func (p *pressure) set(under bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.under, p.err = under, err
}

type scopeGate struct {
	mu  sync.Mutex
	err error
}

func (g *scopeGate) CheckScope(context.Context, contracts.Scope) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

func (g *scopeGate) set(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.err = err
}

type operatorGate struct {
	mu      sync.Mutex
	allowed bool
	asked   []OperatorAction
}

func (o *operatorGate) AuthorizeOperator(_ context.Context, _ contracts.Scope, a OperatorAction) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.asked = append(o.asked, a)
	if !o.allowed {
		return contracts.New(contracts.ErrPermissionDenied, "not an operator")
	}
	return nil
}

func (o *operatorGate) allow(v bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.allowed = v
}

// ---------------------------------------------------------------- the fixture

type fixtureOpts struct {
	noEmbedding bool
	config      func(*Config)
	jobs        func(*jobs.Config)
}

// fixture is the whole pipeline over a throwaway database, a temp directory as
// the source root, a fake clock, the stand-in parser and the fake embedder.
type fixture struct {
	t      *testing.T
	clk    *clock.Fake
	db     *store.DB
	q      *jobs.Queue
	pool   *jobs.Pool
	ix     *index.Index
	vec    *vector.Flat
	aud    *audit.Log
	svc    *Service
	parser *stubParser
	emb    *gateEmbedder
	reg    *testRegistry
	prov   *fsprov.Provider
	pause  *Pause
	press  *pressure
	gate   *scopeGate
	oper   *operatorGate
	root   string
	scope  contracts.Scope
	def    source.Definition
	srcID  string
	cfg    Config
	syncs  int
}

func testScope(workspace string) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-1", ApplicationID: "demo-library", TenantID: "tenant-1", WorkspaceID: workspace,
		PrincipalID: "usr-1", Roles: []string{"sync"}, CapabilityScopes: []string{"rag.sync"}, SessionID: "sess-1",
	}
}

func baseDecl(root string) ap.RAGSource {
	return ap.RAGSource{
		ID:    "docs",
		Type:  ap.RAGSourceTypeLocalFolder,
		Owner: ap.RAGSourceOwner{Name: "Test owner"},
		Scope: ap.RAGSourceScope{
			ApplicationID: "demo-library",
			Tenant:        ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
			Workspace:     ap.RAGSourceScopeBinding{Mode: ap.RAGSourceScopeBindingModeRequest},
		},
		Root:     ap.RAGSourceRoot{Value: ap.RAGSourceRootFolder{Kind: "folder", Path: root}},
		Include:  []ap.GlobPattern{"**"},
		Exclude:  []ap.GlobPattern{},
		Formats:  []ap.MimeType{source.MIMEText, source.MIMEMarkdown},
		Limits:   ap.RAGSourceLimits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 30, MaxFiles: 10000},
		Sync:     ap.RAGSourceSync{Method: ap.RAGSourceSyncMethodManual},
		Deletion: ap.RAGSourceDeletion{PurgeAfterHours: 24, OnSourceRemoved: ap.RAGSourceDeletionOnSourceRemovedPurge},
	}
}

func newFixture(t *testing.T, o ...func(*fixtureOpts)) *fixture {
	t.Helper()
	var opts fixtureOpts
	for _, m := range o {
		m(&opts)
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, clk: clock.NewFake(clockStart), root: filepath.Join(base, "root"), scope: testScope("ws-1")}
	if err := os.MkdirAll(f.root, 0o755); err != nil {
		t.Fatal(err)
	}
	f.db = storetest.Open(t, nil)
	f.press, f.gate, f.oper = &pressure{}, &scopeGate{}, &operatorGate{}
	f.pause = NewPause(f.press, f.clk, 30*time.Second, nil)

	jc := jobs.DefaultConfig()
	if opts.jobs != nil {
		opts.jobs(&jc)
	}
	f.q, err = jobs.NewQueue(f.db, jobs.Options{Config: jc, Clock: f.clk, Policy: f.pause.Policy(nil), Jitter: func() float64 { return 0 }})
	if err != nil {
		t.Fatal(err)
	}
	f.ix, err = index.New(f.db, f.clk, index.Config{})
	if err != nil {
		t.Fatal(err)
	}
	vc := vector.DefaultConfig()
	vc.CacheMiB = 0
	f.vec, err = vector.NewFlat(f.db, vector.FlatOptions{Config: vc})
	if err != nil {
		t.Fatal(err)
	}
	f.aud, err = audit.New(f.db, audit.Options{Clock: f.clk})
	if err != nil {
		t.Fatal(err)
	}
	f.parser = newStubParser()
	f.emb = &gateEmbedder{inner: fake.New(fake.WithEmbedDim(8))}
	f.prov = fsprov.New(fsprov.Config{Clock: f.clk})
	f.reg = &testRegistry{prov: f.prov, defs: map[string]source.Definition{}}
	f.srcID = ids.New(source.IDPrefix)
	f.def = f.mkDef(f.srcID, "tenant-1", "ws-1", f.root)
	f.reg.set(f.def)

	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = DefaultConfig()
	if opts.config != nil {
		opts.config(&f.cfg)
	}
	deps := Deps{
		DB: f.db, Clock: f.clk, Queue: f.q, Index: f.ix, Vectors: f.vec, Registry: f.reg, Parser: f.parser, Chunker: chk,
		Audit: f.aud, Pause: f.pause, Operator: f.oper, Config: f.cfg,
	}
	if !opts.noEmbedding {
		deps.Embedding = &Embedding{Embedder: f.emb, Model: "fake-model", Component: rag.ComponentVersion{Name: "fake-embed", Version: "1"}}
	}
	f.svc, err = New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.pool, err = jobs.NewPool(f.q, jobs.PoolOptions{ScopeChecker: f.gate})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Handle(f.pool); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := f.pool.Shutdown(sctx); err != nil {
			t.Errorf("pool shutdown: %v", err)
		}
	})
	return f
}

func (f *fixture) mkDef(srcID, tenant, ws, root string, mutate ...func(*ap.RAGSource)) source.Definition {
	f.t.Helper()
	d := baseDecl(root)
	for _, m := range mutate {
		m(&d)
	}
	def, err := source.FromDeclaration(d, source.Binding{SourceID: srcID, TenantID: tenant, WorkspaceID: ws})
	if err != nil {
		f.t.Fatal(err)
	}
	return def
}

// write creates a file under the root with an old modification time, so that a
// record of it is trusted for the size-and-time shortcut.
func (f *fixture) write(rel, content string) {
	f.t.Helper()
	f.writeIn(f.root, rel, content)
}

func (f *fixture) writeIn(root, rel, content string) {
	f.t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(p, oldTime, oldTime); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) remove(rel string) {
	f.t.Helper()
	if err := os.RemoveAll(filepath.Join(f.root, filepath.FromSlash(rel))); err != nil {
		f.t.Fatal(err)
	}
}

// requestSync queues a sync of the fixture's source.
func (f *fixture) requestSync() jobs.Job {
	f.t.Helper()
	f.syncs++
	res, err := f.svc.RequestSync(bg, f.scope, f.srcID, SyncOptions{Key: fmt.Sprintf("test:%d", f.syncs)})
	if err != nil {
		f.t.Fatalf("RequestSync: %v", err)
	}
	return res.Job
}

// sync requests a sync and drains the queue until the pipeline is idle.
func (f *fixture) sync() jobs.Job {
	f.t.Helper()
	j := f.requestSync()
	f.drain()
	got, err := f.q.Get(bg, f.scope, j.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func (f *fixture) running() int {
	n := 0
	for _, c := range jobs.Classes() {
		n += f.pool.Running(c)
	}
	return n
}

// pending counts the jobs that are running, ready, or waiting out a retry
// backoff; a job scheduled for later (never attempted, run_at ahead) does not
// count, because nothing but the passing of time will run it.
func (f *fixture) pending() int {
	return f.countRows(`SELECT count(*) FROM jobs WHERE state = 'running' OR (state = 'pending' AND (attempts > 0 OR run_at <= ?))`,
		f.clk.Now().UnixMilli())
}

// waitIdle waits until no handler is running.
func (f *fixture) waitIdle() {
	f.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for f.running() > 0 {
		if time.Now().After(deadline) {
			f.t.Fatal("handlers did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

// poll does one scheduling step and waits for what it started.
func (f *fixture) poll() int {
	f.t.Helper()
	n, err := f.pool.Poll(bg)
	if err != nil {
		f.t.Fatalf("Poll: %v", err)
	}
	f.waitIdle()
	return n
}

// drain polls until the queue is empty. A job in its retry backoff is waited
// out by moving the fake clock; a queue that cannot drain (everything left is
// held back) is a test failure, so a pause is tested with poll instead.
func (f *fixture) drain() {
	f.t.Helper()
	stuck := 0
	for i := 0; i < 5000; i++ {
		n := f.poll()
		if f.pending() == 0 {
			return
		}
		if n == 0 {
			stuck++
			if stuck > 40 {
				f.t.Fatalf("the queue did not drain: %d jobs left\n%s", f.pending(), f.dumpJobs())
			}
			f.clk.Advance(30 * time.Second)
			continue
		}
		stuck = 0
	}
	f.t.Fatal("drain did not finish")
}

func (f *fixture) dumpJobs() string {
	rows, err := f.db.QueryContext(bg, `SELECT type, state, attempts, coalesce(error_code, ''), coalesce(error_message, '') FROM jobs ORDER BY seq`)
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

// jobsOf returns the jobs of a type, oldest first.
func (f *fixture) jobsOf(typ string) []jobs.Job {
	f.t.Helper()
	rows, err := f.db.QueryContext(bg, `SELECT id FROM jobs WHERE type = ? ORDER BY seq`, typ)
	if err != nil {
		f.t.Fatal(err)
	}
	var idList []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			f.t.Fatal(err)
		}
		idList = append(idList, id)
	}
	rows.Close()
	var out []jobs.Job
	for _, id := range idList {
		j, err := f.q.Get(bg, f.scope, id)
		if err != nil {
			f.t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

func (f *fixture) search(text string) []index.Hit {
	f.t.Helper()
	return f.searchIn(f.scope, text)
}

func (f *fixture) searchIn(scope contracts.Scope, text string) []index.Hit {
	f.t.Helper()
	res, err := f.ix.Search(bg, scope, index.Query{Text: text, Limit: 50})
	if err != nil {
		f.t.Fatalf("Search(%q): %v", text, err)
	}
	return res.Hits
}

func (f *fixture) vectors() int {
	f.t.Helper()
	st, err := f.vec.Stats(bg, f.scope)
	if err != nil {
		f.t.Fatal(err)
	}
	return st.Vectors
}

// versionStates counts the versions of the fixture's source by state.
func (f *fixture) versionStates() map[string]int {
	f.t.Helper()
	rows, err := f.db.QueryContext(bg, `SELECT state, count(*) FROM ragingest_version WHERE source_id = ? GROUP BY state`, f.srcID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			f.t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

func (f *fixture) generations() []index.Generation {
	f.t.Helper()
	gens, err := f.ix.ListGenerations(bg, f.scope, f.srcID)
	if err != nil {
		f.t.Fatal(err)
	}
	return gens
}

func (f *fixture) activeGen() index.Generation {
	f.t.Helper()
	g, ok, err := f.ix.ActiveGeneration(bg, f.scope, f.srcID)
	if err != nil || !ok {
		f.t.Fatalf("no active generation: %v", err)
	}
	return g
}

func (f *fixture) status() SourceInfo {
	f.t.Helper()
	st, err := f.svc.Status(bg, f.scope, f.srcID)
	if err != nil {
		f.t.Fatalf("Status: %v", err)
	}
	return st
}

func (f *fixture) auditTypes() []string {
	f.t.Helper()
	rows, err := f.db.QueryContext(bg, `SELECT event_type FROM audit_events WHERE tenant_id = ? ORDER BY chain_seq`, f.scope.TenantID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func docHits(hits []index.Hit) []string {
	set := map[string]bool{}
	for _, h := range hits {
		set[h.DocumentID] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func codeOf(t *testing.T, err error, want contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want code %s", want)
	}
	if got := contracts.CodeOf(err); got != want {
		t.Fatalf("error code = %s (%v), want %s", got, err, want)
	}
}

func mustEqual[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}
