package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/config"
	"github.com/buxee/alix-ai/internal/gateway"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/ingest"
	"github.com/buxee/alix-ai/internal/rag/parse"
	"github.com/buxee/alix-ai/internal/rag/parse/wire"
	fsprovider "github.com/buxee/alix-ai/internal/rag/providers/fs"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/internal/rag/source"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/internal/trust"
	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// ---- stand-ins ----------------------------------------------------------------

const (
	ragApp  = "demo-library"
	ragHash = "pkg-hash-1"
)

var (
	ragStart   = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ragOldTime = time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)
)

func ragScope(tenant, workspace, principal string, roles ...string) contracts.Scope {
	if len(roles) == 0 {
		roles = []string{"member"}
	}
	return contracts.Scope{
		DeploymentID: "dep_test", ApplicationID: ragApp, TenantID: tenant, WorkspaceID: workspace,
		PrincipalID: principal, Roles: roles, CapabilityScopes: []string{"rag.read"}, SessionID: "ses_test",
	}
}

func ragCodeOf(t *testing.T, err error, want contracts.ErrCode) {
	t.Helper()
	if got := contracts.CodeOf(err); err == nil || got != want {
		t.Fatalf("error = %v (code %q), want code %q", err, got, want)
	}
}

// ragFakePkg is an active package, as far as the registry asks.
type ragFakePkg struct {
	app, hash string
	status    policy.PackageStatus
}

func (p ragFakePkg) ApplicationID() string        { return p.app }
func (p ragFakePkg) Status() policy.PackageStatus { return p.status }
func (p ragFakePkg) ContentHash() string          { return p.hash }

// ragFakePackages is the package source of a test: per application, active with
// a content hash, not active, or failing.
type ragFakePackages struct {
	mu     sync.Mutex
	hash   map[string]string
	off    map[string]bool
	err    error
	status policy.PackageStatus
	calls  int
}

func newRagFakePackages(app, hash string) *ragFakePackages {
	return &ragFakePackages{hash: map[string]string{app: hash}, off: map[string]bool{}, status: policy.StatusActive}
}

func (p *ragFakePackages) set(app, hash string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hash[app], p.off[app] = hash, false
}

func (p *ragFakePackages) deactivate(app string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.off[app] = true
}

func (p *ragFakePackages) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *ragFakePackages) ActivePackage(_ context.Context, app string) (ragActivePackage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	switch {
	case p.err != nil:
		return nil, p.err
	case p.off[app] || p.hash[app] == "":
		return nil, contracts.New(contracts.ErrQuarantined, "the application has no active package")
	}
	return ragFakePkg{app: app, hash: p.hash[app], status: p.status}, nil
}

// ragFakeDisk is the disk-pressure hook of a test.
type ragFakeDisk struct {
	mu    sync.Mutex
	under bool
	err   error
	asked int
}

func (d *ragFakeDisk) UnderPressure(context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked++
	return d.under, d.err
}

func (d *ragFakeDisk) set(under bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.under, d.err = under, err
}

// ragStubParser is a deterministic ingest.Parser that starts no process: it
// splits text into paragraphs and "# " headings, can be told to fail its probe,
// counts its calls and records that it was closed.
type ragStubParser struct {
	mu     sync.Mutex
	probe  func() (parse.SandboxReport, error)
	probes atomic.Int64
	parses atomic.Int64
	closed atomic.Bool
}

func newRagStubParser() *ragStubParser { return &ragStubParser{} }

func (p *ragStubParser) Component() rag.ComponentVersion {
	return rag.ComponentVersion{Name: "stubparse", Version: actions.OpaqueVersion("1.0.0")}
}

func (p *ragStubParser) Probe(context.Context) (parse.SandboxReport, error) {
	p.probes.Add(1)
	p.mu.Lock()
	fn := p.probe
	p.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return parse.SandboxReport{Level: parse.LevelL2, Platform: "test"}, nil
}

func (p *ragStubParser) setProbe(fn func() (parse.SandboxReport, error)) {
	p.mu.Lock()
	p.probe = fn
	p.mu.Unlock()
}

func (p *ragStubParser) Close() error { p.closed.Store(true); return nil }

func (p *ragStubParser) Parse(ctx context.Context, f wire.Format, doc []byte) (*parse.Result, error) {
	p.parses.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
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

func ragTestDecl(root string) ap.RAGSource {
	title := ap.Label("Handbook")
	return ap.RAGSource{
		ID:    "docs",
		Type:  ap.RAGSourceTypeLocalFolder,
		Title: &title,
		Owner: ap.RAGSourceOwner{Name: "Test owner"},
		Scope: ap.RAGSourceScope{
			ApplicationID: ragApp,
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

// ragTestDef is a registration built the way FromPackage builds it, with the
// content hash of the fake package.
func ragTestDef(t *testing.T, root, tenant, workspace, hash string, mutate ...func(*ap.RAGSource)) source.Definition {
	t.Helper()
	d := ragTestDecl(root)
	for _, m := range mutate {
		m(&d)
	}
	def, err := source.FromDeclaration(d, source.Binding{SourceID: ids.New(source.IDPrefix), TenantID: tenant, WorkspaceID: workspace})
	if err != nil {
		t.Fatal(err)
	}
	def.ManifestHash = hash
	return def
}

// ---- the fixture ----------------------------------------------------------------

type ragFixtureOpts struct {
	embedding bool
	// embedderOnly registers the embedder role but names no model in the section.
	embedderOnly bool
	// embedFunc replaces the fake embedder's vectors.
	embedFunc func(providers.EmbedRequest) ([][]float32, error)
	operators []ragOperator
	pressure  bool
	jobs      func(*jobs.Config)
	nopool    bool
	// resources builds the resource governor over the fixture's clock and fixed probes
	// (resourcesTestWiring) and composes the stack with it, as serve does.
	resources bool
	// quarantine is the set of isolated derived indexes the stack consults (quarantineOf in serve); nil isolates nothing.
	quarantine *upgrade.Quarantine
	// reaudit is the re-audit trigger's package source; nil turns it off.
	reaudit ragReauditSource
	// over starts the process again over the store, clock, source root and
	// registration of an earlier fixture: a restart. Everything in memory is new.
	over *ragFixture
}

// ragFixture is the composed RAG stack over a throwaway store, a temp directory
// as the source root, a fake clock, the stub parser and the fake embedder.
type ragFixture struct {
	t      *testing.T
	clk    *clock.Fake
	db     *store.DB
	aud    *audit.Log
	pkgs   *ragFakePackages
	disk   *ragFakeDisk
	parser *ragStubParser
	stack  *ragStack
	res    *resourcesWiring
	root   string
	scope  contracts.Scope
	def    source.Definition
	syncs  int
}

func newRagFixture(t *testing.T, o ...func(*ragFixtureOpts)) *ragFixture {
	t.Helper()
	var opts ragFixtureOpts
	for _, m := range o {
		m(&opts)
	}
	var f *ragFixture
	var err error
	if o := opts.over; o != nil {
		f = &ragFixture{t: t, clk: o.clk, root: o.root, scope: o.scope, db: o.db}
	} else {
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		f = &ragFixture{t: t, clk: clock.NewFake(ragStart), root: filepath.Join(base, "root"), scope: ragScope("t1", "w1", "usr-1")}
		if err := os.MkdirAll(f.root, 0o755); err != nil {
			t.Fatal(err)
		}
		f.db = storetest.Open(t, nil)
	}
	if f.aud, err = audit.New(f.db, audit.Options{Clock: f.clk}); err != nil {
		t.Fatal(err)
	}
	f.pkgs, f.disk, f.parser = newRagFakePackages(ragApp, ragHash), &ragFakeDisk{under: opts.pressure}, newRagStubParser()

	cfg := ragConfig{Operators: opts.operators}
	var reg *providers.Registry
	if opts.embedding || opts.embedderOnly {
		reg = providers.NewRegistry(f.clk)
		var eopts []fake.Option
		eopts = append(eopts, fake.WithEmbedDim(8))
		if opts.embedFunc != nil {
			eopts = append(eopts, fake.WithEmbedFunc(opts.embedFunc))
		}
		if err := reg.Register(providers.RoleEmbedder, fake.New(eopts...)); err != nil {
			t.Fatal(err)
		}
		if opts.embedding {
			cfg.Embedding = ragEmbeddingConf{Model: "fake-model", ComponentVersion: "1"}
		}
	}
	jc := jobs.DefaultConfig()
	if opts.jobs != nil {
		opts.jobs(&jc)
	}
	vc := vector.DefaultConfig()
	vc.CacheMiB = 0
	in := ragInput{
		DB: f.db, Clock: f.clk, Audit: f.aud, Providers: reg, Packages: f.pkgs, Config: cfg,
		Ingest: ingest.DefaultConfig(), Jobs: jc, Query: query.DefaultConfig(), Vector: vc,
		Parser: f.parser, Disk: f.disk,
		SourceProviders: []source.Provider{fsprovider.New(fsprovider.Config{Clock: f.clk})},
	}
	in.Quarantine = opts.quarantine
	if opts.reaudit != nil {
		in.Reaudit = opts.reaudit
	}
	if opts.resources {
		f.res = resourcesTestWiring(t, f.clk)
		in.Resources = f.res
	}
	f.stack, err = ragCompose(in)
	if err != nil {
		t.Fatalf("ragCompose: %v", err)
	}
	f.def = ragTestDef(t, f.root, "t1", "w1", ragHash)
	if opts.over != nil {
		f.def = opts.over.def
	}
	if !opts.nopool {
		t.Cleanup(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := f.stack.pool.Shutdown(sctx); err != nil {
				t.Errorf("pool shutdown: %v", err)
			}
		})
	}
	return f
}

func (f *ragFixture) register() {
	f.t.Helper()
	if err := f.stack.sources.Register(context.Background(), f.scope, f.def); err != nil {
		f.t.Fatalf("Register: %v", err)
	}
}

// write creates a file under the root with an old modification time, so that a
// record of it is trusted for the size-and-time shortcut.
func (f *ragFixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(p, ragOldTime, ragOldTime); err != nil {
		f.t.Fatal(err)
	}
}

func (f *ragFixture) emptyRoot() {
	f.t.Helper()
	entries, err := os.ReadDir(f.root)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(f.root, e.Name())); err != nil {
			f.t.Fatal(err)
		}
	}
}

// requestSync queues a sync of the fixture's source the way the system does.
func (f *ragFixture) requestSync() jobs.Job {
	f.t.Helper()
	f.syncs++
	res, err := f.stack.svc.RequestSync(context.Background(), f.scope, f.def.SourceID,
		ingest.SyncOptions{Key: fmt.Sprintf("test:%d", f.syncs), ServiceIdentity: ragServiceIdentity})
	if err != nil {
		f.t.Fatalf("RequestSync: %v", err)
	}
	return res.Job
}

func (f *ragFixture) running() int {
	n := 0
	for _, c := range jobs.Classes() {
		n += f.stack.pool.Running(c)
	}
	return n
}

func (f *ragFixture) countRows(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// pending counts the jobs that are running, ready, or waiting out a retry
// backoff; a job scheduled for later does not count.
func (f *ragFixture) pending() int {
	return f.countRows(`SELECT count(*) FROM jobs WHERE state = 'running' OR (state = 'pending' AND (attempts > 0 OR run_at <= ?))`,
		f.clk.Now().UnixMilli())
}

func (f *ragFixture) waitIdle() {
	f.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for f.running() > 0 {
		if time.Now().After(deadline) {
			f.t.Fatal("handlers did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *ragFixture) poll() int {
	f.t.Helper()
	n, err := f.stack.pool.Poll(context.Background())
	if err != nil {
		f.t.Fatalf("Poll: %v", err)
	}
	f.waitIdle()
	return n
}

// drain polls until the queue is empty, waiting out retry backoffs by moving
// the fake clock.
func (f *ragFixture) drain() {
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

func (f *ragFixture) dumpJobs() string {
	rows, err := f.db.QueryContext(context.Background(), `SELECT type, state, attempts, coalesce(error_code, ''), coalesce(error_message, '') FROM jobs ORDER BY seq`)
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

func (f *ragFixture) jobState(id string) jobs.Job {
	f.t.Helper()
	j, err := f.stack.queue.Get(context.Background(), f.scope, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return j
}

func (f *ragFixture) ask(text string) query.Result {
	f.t.Helper()
	res, err := f.stack.pipeline.Query(context.Background(), f.scope, query.Request{Text: text})
	if err != nil {
		f.t.Fatalf("Query(%q): %v", text, err)
	}
	return res
}

func ragTexts(res query.Result) []string {
	var out []string
	for _, it := range res.Package.Items {
		out = append(out, it.Text)
	}
	return out
}

// ---- the three acceptance tests -------------------------------------------------

// The composition is built in the order the pieces need: the pause before the
// queue (the queue takes its policy when it is created), the same pause in the
// service, the handlers attached before the pool runs, the parser probed before
// the first job, and the embedding job registered once, by ingest.New.
func TestRAGServeWiringOrder(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.embedding, o.pressure, o.nopool = true, true, true })
	st := f.stack

	want := []string{"pause", "queue", "index", "ingest", "pool", "handle", "query"}
	if got := st.stepsDone(); !slices.Equal(got, want) {
		t.Fatalf("built in the order %v, want %v", got, want)
	}
	if st.svc.Pause() != st.pause {
		t.Error("the ingestion service holds another Pause than the one the queue's policy asks")
	}
	if got := st.queue.Config().InteractiveUpTo; got != 4 {
		t.Errorf("interactive_up_to_class = %d, want 4 (ingestion yields to classes 1 to 4)", got)
	}
	if st.sources == nil || st.pipeline == nil || st.ix == nil {
		t.Fatalf("the stack is missing a part: %+v", st)
	}

	// The embedding job is registered once: by ingest.New. Registering it, or
	// attaching its handler, again is refused, which is what calling
	// vector.Pipeline.Register after ingest.New would hit.
	ragCodeOf(t, st.queue.Register(vector.EmbedJobSpec()), contracts.ErrConflict)
	ragCodeOf(t, st.pool.Handle(vector.EmbedJobType, func(context.Context, jobs.Job) error { return nil }), contracts.ErrConflict)

	// Start comes after Handle, and the parser is probed before the pool runs.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := st.start(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(st.stepsDone(), "start") {
		if time.Now().After(deadline) {
			t.Fatal("ingestion did not start")
		}
		time.Sleep(time.Millisecond)
	}
	steps := st.stepsDone()
	if slices.Index(steps, "start") < slices.Index(steps, "handle") || steps[len(steps)-1] != "start" {
		t.Errorf("steps = %v: start must come after handle", steps)
	}
	if f.parser.probes.Load() != 1 {
		t.Errorf("the parser was probed %d times at start, want 1", f.parser.probes.Load())
	}
	if rd, ok := st.svc.Readiness(); !ok || !rd.Ready {
		t.Errorf("readiness after start = %+v, %v", rd, ok)
	}

	// The queue's policy is the pause: with the disk under pressure an ingestion
	// job is not leased by the running pool, and it is leased once the pressure is
	// gone (the verdict is reused for the recheck interval, so the clock moves).
	f.register()
	var jobID string
	if err := f.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE type = ?`, ingest.JobSync).Scan(&jobID); err != nil {
		t.Fatalf("registering the source queued no sync: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if j := f.jobState(jobID); j.State != jobs.StatePending || j.Attempts != 0 {
		t.Fatalf("under disk pressure the sync job is %s after %d attempts, want it left pending", j.State, j.Attempts)
	}
	if !st.svc.Paused(ctx).Paused {
		t.Error("the service does not report the pause")
	}
	f.disk.set(false, nil)
	f.clk.Advance(2 * time.Minute)
	st.pool.Wake()
	deadline = time.Now().Add(10 * time.Second)
	for f.jobState(jobID).State == jobs.StatePending {
		if time.Now().After(deadline) {
			t.Fatal("the job was not leased after the pressure ended")
		}
		st.pool.Wake()
		time.Sleep(5 * time.Millisecond)
	}

	stop()
	if !f.parser.closed.Load() {
		t.Error("stopping the stack did not close the parser")
	}

	// Without an embedder there is no vector index and no embedding job.
	g := newRagFixture(t, func(o *ragFixtureOpts) { o.nopool = true })
	if g.stack.vectors != nil || g.stack.model != "" {
		t.Error("a vector index was built with no embedder")
	}
	_, err := g.stack.queue.Enqueue(context.Background(), g.scope, jobs.EnqueueRequest{Type: vector.EmbedJobType, Payload: json.RawMessage(`{}`)})
	ragCodeOf(t, err, contracts.ErrValidationFailed)
}

// What /readyz says about ingestion is the parser worker's readiness, and only
// once a source is registered: a deployment that never ingests is not made
// unready by a worker it does not use. The reason is a fixed phrase; the probe's
// own error names the worker's path and stays in the operator's log.
func TestRAGReadinessReachesReadyz(t *testing.T) {
	const secretPath = `C:\Users\someone\secret-install\alixai-parser.exe`
	cases := []struct {
		name      string
		probeErr  error
		register  bool
		wantReady bool
	}{
		{"no source and no worker", errors.New("the worker is missing: " + secretPath), false, true},
		{"a source and a ready worker", nil, true, true},
		{"a source and a worker that is not ready", contracts.New(contracts.ErrUnavailable, "parse: worker \""+secretPath+"\" is not an executable file"), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parser := newRagStubParser()
			parser.setProbe(func() (parse.SandboxReport, error) {
				if tc.probeErr != nil {
					return parse.SandboxReport{}, tc.probeErr
				}
				return parse.SandboxReport{Level: parse.LevelL1, Platform: "test"}, nil
			})
			oldParser, oldHook := ragNewParser, ragOnCompose
			stacks := make(chan *ragStack, 1)
			ragNewParser = func(ingest.Config, *slog.Logger) ingest.Parser { return parser }
			ragOnCompose = func(s *ragStack) { stacks <- s }
			t.Cleanup(func() { ragNewParser, ragOnCompose = oldParser, oldHook })

			f := newFixture(t)
			t.Setenv("ALIXAI_TEST_LOCALAI_KEY", testKey(t))
			cfg, _ := serveConfig(t, f, "")
			srv := startServe(t, cfg)
			var st *ragStack
			select {
			case st = <-stacks:
			case <-time.After(10 * time.Second):
				t.Fatal("serve did not build the RAG stack")
			}
			pollUntil(t, 10*time.Second, "the parser probe", func() bool { _, probed := st.svc.Readiness(); return probed })

			if tc.register {
				def := ragTestDef(t, t.TempDir(), "t1", "w1", ragHash)
				st.sources.mu.Lock()
				st.sources.entries[ragKeyOf(ragScope("t1", "w1", "usr-1"), def.SourceID)] = &ragEntry{def: def}
				st.sources.mu.Unlock()
			}
			code, _, body := srv.do(t, http.MethodGet, "/readyz", "")
			var rep gateway.ReadyReport
			if err := json.Unmarshal(body, &rep); err != nil {
				t.Fatalf("/readyz = %d: %s", code, body)
			}
			var check *gateway.CheckReport
			for i := range rep.Checks {
				if rep.Checks[i].Name == ragReadyCheck {
					check = &rep.Checks[i]
				}
			}
			if check == nil {
				t.Fatalf("/readyz has no %q check: %s", ragReadyCheck, body)
			}
			if check.OK != tc.wantReady || rep.Ready != tc.wantReady || (code == http.StatusOK) != tc.wantReady {
				t.Errorf("/readyz = %d, check %+v, ready %v; want ready %v\n%s", code, check, rep.Ready, tc.wantReady, body)
			}
			if !tc.wantReady && check.Reason == "" {
				t.Error("a failing check has no reason")
			}
			if strings.Contains(string(body), "secret-install") || strings.Contains(string(body), "alixai-parser") {
				t.Errorf("/readyz names the worker's path: %s", body)
			}
			if err := srv.stop(); err != nil {
				t.Fatal(err)
			}
			if !parser.closed.Load() {
				t.Error("stopping serve did not close the parser")
			}
		})
	}
}

// A held mass deletion and a tenant deletion are operator actions: only a
// principal the deployment configured as an operator, named by its verified
// application and principal ids, may take them. A role the token asserts, or any
// capability scope, grants nothing. The test runs a real hold through the whole
// stack: the pipeline indexes a folder, the folder is emptied (an unmounted share
// looks like this), the next scan is refused, and only the operator's
// confirmation lets the deletion through.
func TestRAGMassDeletionNeedsOperatorAuthority(t *testing.T) {
	ops := []ragOperator{{ApplicationID: ragApp, PrincipalID: "ops-lead", Actions: []string{string(ingest.ActionConfirmMassDeletion)}}}
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.operators = ops })
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		f.write(fmt.Sprintf("handbook-%02d.md", i), fmt.Sprintf("# Handbook %d\n\nThe harbour dock schedule %d lists the tide tables.", i, i))
	}
	f.register()
	f.drain()
	if got := f.ask("harbour dock schedule"); len(got.Package.Items) == 0 {
		t.Fatalf("nothing was indexed through the stack:\n%s", f.dumpJobs())
	}

	// The share is unmounted: every file is gone and the root looks empty.
	f.emptyRoot()
	j := f.requestSync()
	f.drain()
	if got := f.jobState(j.ID); got.State != jobs.StateFailed || got.ErrorCode != contracts.ErrUnavailable {
		t.Fatalf("the sync of an emptied source ended %s (%s), want it held as a mass deletion\n%s", got.State, got.ErrorCode, f.dumpJobs())
	}
	info, err := f.stack.svc.Status(ctx, f.scope, f.def.SourceID)
	if err != nil || info.Hold == nil || info.Hold.Digest == "" {
		t.Fatalf("no hold in the status: %+v, %v", info, err)
	}
	if got := f.ask("harbour dock schedule"); len(got.Package.Items) == 0 {
		t.Error("a held deletion removed what the source still serves")
	}

	// Who asks decides. The same call, five callers.
	confirm := func(scope contracts.Scope) error {
		_, err := f.stack.svc.ConfirmMassDeletion(ctx, scope, f.def.SourceID, info.Hold.Digest)
		return err
	}
	registered := ragScope("t1", "w1", "usr-1", "admin", "operator", "rag.operator", "owner")
	registered.CapabilityScopes = []string{"rag.confirm_mass_deletion", "rag.admin"}
	otherApp := ragScope("t1", "w1", "ops-lead")
	otherApp.ApplicationID = "another-app"
	for name, scope := range map[string]contracts.Scope{
		"a user whose token claims every administrative role": registered,
		"the operator's principal id in another application":  otherApp,
		"an operator with no scope":                           {},
	} {
		if err := confirm(scope); err == nil || contracts.CodeOf(err) == contracts.ErrConflict {
			t.Errorf("%s: ConfirmMassDeletion = %v, want a refusal", name, err)
		}
	}
	// The configured operator may confirm deletions, but was not given tenant deletion.
	opScope := ragScope("t1", "w1", "ops-lead")
	ragCodeOf(t, f.stack.svc.RemoveTenant(ctx, opScope), contracts.ErrPermissionDenied)
	for _, who := range []contracts.Scope{registered, ragScope("t1", "w1", "usr-1")} {
		ragCodeOf(t, f.stack.svc.RemoveTenant(ctx, who), contracts.ErrPermissionDenied)
	}
	if info, _ := f.stack.svc.Status(ctx, f.scope, f.def.SourceID); info.Hold == nil || info.Hold.Confirmed {
		t.Fatalf("a refused caller changed the hold: %+v", info.Hold)
	}

	if err := confirm(opScope); err != nil {
		t.Fatalf("the configured operator was refused: %v", err)
	}
	if info, _ := f.stack.svc.Status(ctx, f.scope, f.def.SourceID); info.Hold == nil || !info.Hold.Confirmed || info.Hold.ConfirmedBy != "ops-lead" {
		t.Fatalf("the confirmation is not recorded: %+v", info.Hold)
	}
	f.drain()
	if got := f.ask("harbour dock schedule"); len(got.Package.Items) != 0 {
		t.Errorf("the confirmed deletion left %q behind", ragTexts(got))
	}
}

// ---- the stack end to end ---------------------------------------------------------

// A folder goes in through the pool under the service identity, and a question
// comes out through the catalog: the evidence names the source as the catalog
// does, a chunk that only the vector index found is read back by reference
// (index.Hits), and a package that is revoked, replaced or unreadable stops the
// source from being served.
func TestRAGIngestAndQueryThroughTheStack(t *testing.T) {
	target := fake.EmbedVector("tide tables", 8)
	f := newRagFixture(t, func(o *ragFixtureOpts) {
		o.embedding = true
		// Every text, a chunk or a question, embeds to the same vector: a question
		// with no word in common with the documents is found by the vector index alone.
		o.embedFunc = func(req providers.EmbedRequest) ([][]float32, error) {
			out := make([][]float32, len(req.Inputs))
			for i := range out {
				out[i] = target
			}
			return out, nil
		}
	})
	f.write("tides.md", "# Tides\n\nThe harbour dock schedule lists the tide tables.")
	f.write("moorings.md", "# Moorings\n\nChains and shackles for the moorings.")
	f.register()
	f.drain()

	res := f.ask("tide tables")
	if len(res.Package.Items) == 0 || res.Diagnostics.Mode != query.ModeHybrid {
		t.Fatalf("a full-text question found %q in mode %q\n%s", ragTexts(res), res.Diagnostics.Mode, f.dumpJobs())
	}
	for _, it := range res.Package.Items {
		if string(it.SourceID) != f.def.SourceID || it.DisplayName != "Handbook" || it.Availability != conversation.CitationAvailabilityAvailable {
			t.Errorf("evidence %+v does not name the source as the catalog does", it)
		}
		if it.SourceEgress == nil || *it.SourceEgress != rag.SourceEgressLocalOnly || it.SourceClassification == nil || *it.SourceClassification != rag.SourceClassificationRestricted {
			t.Errorf("a source that declares no egress or classification must carry the most restrictive values, got %v / %v", it.SourceEgress, it.SourceClassification)
		}
	}

	vec := f.ask("zzqxv")
	d := vec.Diagnostics
	if d.FTSCandidates != 0 || d.VectorCandidates == 0 || len(vec.Package.Items) == 0 {
		t.Fatalf("a vector-only question: fts %d vector %d items %d degraded %v", d.FTSCandidates, d.VectorCandidates, len(vec.Package.Items), d.Degraded)
	}
	if slices.Contains(d.Degraded, query.DegradedNoHitReader) || slices.Contains(d.Degraded, query.DegradedHydrateFailed) || d.DroppedUnhydrated != 0 {
		t.Errorf("the vector-only candidates were not read by reference: %+v", d)
	}

	// Another tenant's principal reads nothing of it.
	other := ragScope("t2", "w1", "usr-2")
	if got, err := f.stack.pipeline.Query(context.Background(), other, query.Request{Text: "tide tables"}); err != nil || len(got.Package.Items) != 0 {
		t.Errorf("another tenant: %d items, %v", len(got.Package.Items), err)
	}

	// The package is revoked: nothing is served, and that is an empty answer.
	f.pkgs.deactivate(ragApp)
	if got := f.ask("tide tables"); len(got.Package.Items) != 0 {
		t.Errorf("a source of a quarantined package was served: %q", ragTexts(got))
	}
	// The package comes back as another content: the registration was built from
	// the old one and serves nothing until it is rebound.
	f.pkgs.set(ragApp, "pkg-hash-2")
	if got := f.ask("tide tables"); len(got.Package.Items) != 0 {
		t.Errorf("a registration built from another package content was served: %q", ragTexts(got))
	}
	rep, err := f.stack.sources.Reaudit(context.Background(), ragApp, func(old source.Definition) (source.Definition, bool, error) {
		def := ragTestDef(t, f.root, "t1", "w1", "pkg-hash-2")
		def.SourceID = old.SourceID
		return def, true, nil
	})
	if err != nil || rep.Rebound != 1 || rep.Failed != 0 {
		t.Fatalf("Reaudit = %+v, %v", rep, err)
	}
	if got := f.ask("tide tables"); len(got.Package.Items) == 0 {
		t.Error("a rebound source is not served")
	}
	// The package state cannot be read: the query fails, it does not answer empty.
	f.pkgs.fail(errors.New("the package store is down"))
	if _, err := f.stack.pipeline.Query(context.Background(), f.scope, query.Request{Text: "tide tables"}); err == nil {
		t.Error("a query answered while the package state could not be read")
	}
}

// The catalog answers from the registration, the active package and the
// ingestion status, and every doubt is a source that is not there.
func TestRAGCatalogFailsClosed(t *testing.T) {
	f := newRagFixture(t)
	ctx := context.Background()
	f.write("a.md", "# A\n\nAlpha.")
	f.register()
	id := f.def.SourceID

	foreignApp := ragScope("t1", "w1", "usr-1")
	foreignApp.ApplicationID = "another-app"
	cases := []struct {
		name  string
		scope contracts.Scope
		prep  func()
		want  bool
	}{
		{"the registered scope", f.scope, func() {}, true},
		{"another tenant", ragScope("t2", "w1", "usr-1"), func() {}, false},
		{"another workspace", ragScope("t1", "w2", "usr-1"), func() {}, false},
		{"another application", foreignApp, func() {}, false},
		{"a package that is not active", f.scope, func() { f.pkgs.deactivate(ragApp) }, false},
		{"a package with other content", f.scope, func() { f.pkgs.set(ragApp, "pkg-hash-9") }, false},
	}
	for _, tc := range cases {
		f.pkgs.set(ragApp, ragHash)
		tc.prep()
		got, err := f.stack.sources.Sources(ctx, tc.scope, []string{id, "src_nothere"})
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		st, ok := got[id]
		if ok != tc.want || (ok && !st.Authorised) || len(got) > 1 {
			t.Errorf("%s: got %+v, want served %v", tc.name, got, tc.want)
		}
	}
	f.pkgs.set(ragApp, ragHash)

	// What cannot be established is an error, never a grant.
	f.pkgs.fail(errors.New("the package store is down"))
	if got, err := f.stack.sources.Sources(ctx, f.scope, []string{id}); err == nil || got != nil {
		t.Errorf("an unreadable package state: %+v, %v", got, err)
	}
	if _, err := f.stack.sources.Resolve(ctx, f.scope, id); err == nil || errors.Is(err, ingest.ErrSourceGone) {
		t.Errorf("an unreadable package state was read as a removal: %v", err)
	}
	f.pkgs.fail(nil)
	if _, err := f.stack.sources.Sources(ctx, contracts.Scope{}, []string{id}); err == nil {
		t.Error("an empty scope was answered")
	}

	// Not scanned yet (the queued sync has not run): authorised, and nothing is
	// claimed as checked.
	got, err := f.stack.sources.Sources(ctx, f.scope, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if st := got[id]; !st.Authorised || st.DisplayName != "Handbook" || st.Availability == conversation.CitationAvailabilityAvailable || st.Staleness != "" {
		t.Errorf("a source never scanned: %+v", st)
	}

	// Scanned: available, and "current" only while the declared schedule is kept.
	hourly := ragTestDef(t, f.root, "t1", "w1", ragHash, func(d *ap.RAGSource) {
		d.ID = "hourly"
		d.Sync = ap.RAGSourceSync{Method: ap.RAGSourceSyncMethodPoll, IntervalSeconds: ptrTo(int64(3600))}
	})
	if err := f.stack.sources.Register(ctx, f.scope, hourly); err != nil {
		t.Fatal(err)
	}
	f.drain()
	state := func(id string) query.SourceState {
		t.Helper()
		got, err := f.stack.sources.Sources(ctx, f.scope, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		return got[id]
	}
	if st := state(hourly.SourceID); st.Availability != conversation.CitationAvailabilityAvailable || st.Staleness != conversation.CitationStalenessCurrent {
		t.Errorf("a scanned source on schedule: %+v", st)
	}
	if st := state(id); st.Availability != conversation.CitationAvailabilityAvailable || st.Staleness != conversation.CitationStalenessUnknown {
		t.Errorf("a scanned source with no schedule cannot be called current: %+v", st)
	}
	f.clk.Advance(3 * time.Hour)
	if st := state(hourly.SourceID); st.Staleness != conversation.CitationStalenessStale {
		t.Errorf("a source that missed its schedule: %+v", st)
	}

	// The root disappears: the next scan cannot reach it, and the source says so.
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatal(err)
	}
	f.requestSync()
	f.drain()
	if st := state(id); !st.Authorised || st.Availability != conversation.CitationAvailabilityUnavailable || st.Staleness == conversation.CitationStalenessCurrent {
		t.Errorf("an unreachable source: %+v", st)
	}

	// Removal stops it being served at once, whatever RemoveSource then does.
	if err := f.stack.sources.Remove(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.stack.sources.Sources(ctx, f.scope, []string{id}); len(got) != 0 {
		t.Errorf("a removed source is still listed: %+v", got)
	}
	if _, err := f.stack.sources.Resolve(ctx, f.scope, id); !errors.Is(err, ingest.ErrSourceGone) {
		t.Errorf("a removed source resolves: %v", err)
	}
	ragCodeOf(t, f.stack.sources.Remove(ctx, f.scope, id), contracts.ErrValidationFailed)
	if f.stack.sources.count() != 1 {
		t.Errorf("%d registrations left, want the other one", f.stack.sources.count())
	}
}

func ptrTo[T any](v T) *T { return &v }

// Registration is refused for anything that is not a source of the application's
// active package, in the caller's scope, with a provider.
func TestRAGRegisterRefusals(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.nopool = true })
	ctx := context.Background()
	good := f.def

	noProvider := good
	noProvider.Type = ap.RAGSourceTypeHTTPAPI
	badID := good
	badID.SourceID = "src_nope"
	otherHash := good
	otherHash.ManifestHash = "pkg-hash-9"
	noHash := good
	noHash.ManifestHash = ""
	home := good
	home.Root.Path = os.Getenv(map[bool]string{true: "USERPROFILE", false: "HOME"}[runtime.GOOS == "windows"])
	system := good
	system.Root.Path = map[bool]string{true: os.Getenv("SystemRoot"), false: "/etc"}[runtime.GOOS == "windows"]
	for name, tc := range map[string]struct {
		scope contracts.Scope
		def   source.Definition
		prep  func()
		want  contracts.ErrCode
	}{
		"another tenant's scope":              {ragScope("t2", "w1", "usr-1"), good, func() {}, contracts.ErrScopeViolation},
		"another workspace's scope":           {ragScope("t1", "w2", "usr-1"), good, func() {}, contracts.ErrScopeViolation},
		"a type no provider serves":           {f.scope, noProvider, func() {}, contracts.ErrUnavailable},
		"a malformed id":                      {f.scope, badID, func() {}, contracts.ErrValidationFailed},
		"a definition of another package":     {f.scope, otherHash, func() {}, contracts.ErrConflict},
		"a definition with no package hash":   {f.scope, noHash, func() {}, contracts.ErrConflict},
		"an application that is quarantined":  {f.scope, good, func() { f.pkgs.deactivate(ragApp) }, contracts.ErrQuarantined},
		"a folder that is the home directory": {f.scope, home, func() {}, contracts.ErrValidationFailed},
		"a folder of the operating system":    {f.scope, system, func() {}, contracts.ErrValidationFailed},
	} {
		if tc.def.Root.Path == "" {
			continue // the variable that names the directory is not set on this host
		}
		f.pkgs.set(ragApp, ragHash)
		tc.prep()
		err := f.stack.sources.Register(ctx, tc.scope, tc.def)
		if err == nil || contracts.CodeOf(err) != tc.want {
			t.Errorf("%s: Register = %v, want code %s", name, err, tc.want)
		}
		if n := f.stack.sources.count(); n != 0 {
			t.Fatalf("%s: a refused registration was kept (%d)", name, n)
		}
	}
	f.pkgs.set(ragApp, ragHash)
	var logged bytes.Buffer
	f.stack.sources.log = slog.New(slog.NewJSONHandler(&logged, nil))
	if err := f.stack.sources.Register(ctx, f.scope, good); err != nil {
		t.Fatal(err)
	}
	// The operator's log shows where the folder resolves; the status a member of the scope reads does not.
	if want, _ := json.Marshal(f.root); !strings.Contains(logged.String(), `"resolved_root":`+string(want)) {
		t.Errorf("the registration did not log the resolved root %s: %s", want, logged.String())
	}
	// Idempotent: the same source again replaces itself and queues nothing new.
	if err := f.stack.sources.Register(ctx, f.scope, good); err != nil {
		t.Fatal(err)
	}
	if n := f.stack.sources.count(); n != 1 {
		t.Errorf("%d registrations after registering one source twice", n)
	}
	if n := f.countRows(`SELECT count(*) FROM jobs WHERE type = ?`, ingest.JobSync); n != 1 {
		t.Errorf("%d sync jobs after registering one source twice, want 1", n)
	}
	// The sync was queued under the service identity.
	var identity string
	if err := f.db.QueryRowContext(ctx, `SELECT coalesce(service_identity, '') FROM jobs WHERE type = ?`, ingest.JobSync).Scan(&identity); err != nil || identity != ragServiceIdentity {
		t.Errorf("service identity of the registration's sync = %q, %v", identity, err)
	}
}

// After a re-audit every registered source of the application is rebuilt from the
// new package and bound to it, and ingestion compares the new rules with the last
// scan's: a changed definition queues a sync, an unchanged one does not, and a
// source the package no longer declares is removed.
func TestRAGReauditRebindsAndQueues(t *testing.T) {
	f := newRagFixture(t)
	ctx := context.Background()
	f.write("a.md", "# A\n\nAlpha dock.")
	f.register()
	f.drain()
	syncs := func() int { return f.countRows(`SELECT count(*) FROM jobs WHERE type = ?`, ingest.JobSync) }
	if syncs() != 1 {
		t.Fatalf("%d syncs after registering", syncs())
	}

	rebuild := func(hash string, mutate ...func(*ap.RAGSource)) ragRebuild {
		return func(old source.Definition) (source.Definition, bool, error) {
			d := ragTestDecl(f.root)
			for _, m := range mutate {
				m(&d)
			}
			def, err := source.FromDeclaration(d, source.Binding{SourceID: old.SourceID, TenantID: old.Tenant.ID, WorkspaceID: old.Workspace.ID})
			def.ManifestHash = hash
			return def, true, err
		}
	}

	// A new package version with the same source declaration: rebound, no rescan.
	f.pkgs.set(ragApp, "pkg-hash-2")
	rep, err := f.stack.sources.Reaudit(ctx, ragApp, rebuild("pkg-hash-2"))
	if err != nil || rep != (ragReauditReport{Checked: 1, Rebound: 1}) {
		t.Fatalf("an unchanged declaration: %+v, %v", rep, err)
	}
	if syncs() != 1 {
		t.Errorf("an unchanged declaration queued a sync (%d)", syncs())
	}
	if got, _ := f.stack.sources.Sources(ctx, f.scope, []string{f.def.SourceID}); len(got) != 1 {
		t.Error("the rebound source is not served")
	}

	// The rules changed: ingestion sees another fingerprint and queues a sync.
	f.pkgs.set(ragApp, "pkg-hash-3")
	rep, err = f.stack.sources.Reaudit(ctx, ragApp, rebuild("pkg-hash-3", func(d *ap.RAGSource) { d.Exclude = []ap.GlobPattern{"drafts/**"} }))
	if err != nil || rep != (ragReauditReport{Checked: 1, Rebound: 1, Queued: 1}) {
		t.Fatalf("a changed declaration: %+v, %v", rep, err)
	}
	if syncs() != 2 {
		t.Errorf("a changed declaration queued %d syncs in all, want 2", syncs())
	}

	// A rebuild that fails, or that returns something that is not the registered
	// source from the active package, leaves the source unserved and is counted.
	f.pkgs.set(ragApp, "pkg-hash-4")
	for name, rb := range map[string]ragRebuild{
		"an error": func(source.Definition) (source.Definition, bool, error) {
			return source.Definition{}, true, errors.New("the declaration is invalid")
		},
		"another source's id": func(old source.Definition) (source.Definition, bool, error) {
			def, _, err := rebuild("pkg-hash-4")(old)
			def.SourceID = ids.New(source.IDPrefix)
			return def, true, err
		},
		"the old package's hash": rebuild("pkg-hash-3"),
		"another tenant": func(old source.Definition) (source.Definition, bool, error) {
			def, _, err := rebuild("pkg-hash-4")(old)
			def.Tenant.ID = "t9"
			return def, true, err
		},
	} {
		rep, err := f.stack.sources.Reaudit(ctx, ragApp, rb)
		if err == nil || rep.Failed != 1 || rep.Rebound != 0 {
			t.Errorf("%s: %+v, %v", name, rep, err)
		}
		if got, _ := f.stack.sources.Sources(ctx, f.scope, []string{f.def.SourceID}); len(got) != 0 {
			t.Errorf("%s: a source that could not be rebound is served", name)
		}
	}
	if _, err := f.stack.sources.Reaudit(ctx, "another-app", rebuild("x")); err == nil {
		t.Error("a re-audit of an application with no active package was accepted")
	}

	// The package no longer declares the source: it is removed, purging its data.
	rep, err = f.stack.sources.Reaudit(ctx, ragApp, func(source.Definition) (source.Definition, bool, error) { return source.Definition{}, false, nil })
	if err != nil || rep != (ragReauditReport{Checked: 1, Removed: 1}) {
		t.Fatalf("a source the package dropped: %+v, %v", rep, err)
	}
	if f.stack.sources.count() != 0 {
		t.Error("the dropped source is still registered")
	}
	if info, err := f.stack.svc.Status(ctx, f.scope, f.def.SourceID); err != nil || info.Available || info.Reason != ingest.ReasonRemoved {
		t.Errorf("status of the dropped source: %+v, %v", info, err)
	}
}

// The rebuild of a loaded package binds each registered source as it was
// registered and reports a source the package does not declare.
func TestRAGRebuildFromALoadedPackage(t *testing.T) {
	res, err := apppkg.Load(context.Background(), apppkg.FromDir(filepath.Join(fixtures, "valid-minimal")), apppkg.Options{CoreVersion: apppkg.DevCoreVersion})
	if err != nil || res.Package == nil {
		t.Fatalf("loading the fixture package: %v, %+v", err, res)
	}
	old, err := source.FromPackage(res.Package, "loan-policies", source.Binding{SourceID: ids.New(source.IDPrefix)})
	if err != nil {
		t.Fatal(err)
	}
	rebuild := ragRebuildFrom(res.Package, nil)
	def, declared, err := rebuild(old)
	if err != nil || !declared || def.SourceID != old.SourceID || def.PackageSourceID != "loan-policies" || def.ManifestHash != res.Package.ManifestHash || def.ManifestHash == "" {
		t.Errorf("rebuild = %+v, %v, %v", def, declared, err)
	}
	gone := old
	gone.PackageSourceID = "dropped-source"
	if _, declared, err := rebuild(gone); err != nil || declared {
		t.Errorf("a source the package does not declare: declared %v, %v", declared, err)
	}
}

// ---- the parts ------------------------------------------------------------------------

func TestRAGOperatorsAuthorize(t *testing.T) {
	o := ragOperators{entries: []ragOperator{
		{ApplicationID: ragApp, PrincipalID: "ops-lead", Actions: []string{"confirm_mass_deletion", "delete_tenant"}},
		{ApplicationID: ragApp, PrincipalID: "ops-confirm", Actions: []string{"confirm_mass_deletion"}},
	}}
	ctx := context.Background()
	other := ragScope("t1", "w1", "ops-lead")
	other.ApplicationID = "another-app"
	admin := ragScope("t1", "w1", "usr-1", "admin", "operator", "owner")
	admin.CapabilityScopes = []string{"rag.operator", "rag.admin"}
	for name, tc := range map[string]struct {
		scope  contracts.Scope
		action ingest.OperatorAction
		ok     bool
	}{
		"the operator confirms":                 {ragScope("t1", "w1", "ops-lead"), ingest.ActionConfirmMassDeletion, true},
		"the operator deletes a tenant":         {ragScope("t9", "w9", "ops-lead"), ingest.ActionDeleteTenant, true},
		"an operator limited to confirmation":   {ragScope("t1", "w1", "ops-confirm"), ingest.ActionConfirmMassDeletion, true},
		"that operator may not delete a tenant": {ragScope("t1", "w1", "ops-confirm"), ingest.ActionDeleteTenant, false},
		"an unknown action":                     {ragScope("t1", "w1", "ops-lead"), "drop_database", false},
		"the same principal id elsewhere":       {other, ingest.ActionConfirmMassDeletion, false},
		"every role a token can assert":         {admin, ingest.ActionConfirmMassDeletion, false},
		"no scope at all":                       {contracts.Scope{}, ingest.ActionConfirmMassDeletion, false},
	} {
		err := o.AuthorizeOperator(ctx, tc.scope, tc.action)
		switch {
		case tc.ok && err != nil:
			t.Errorf("%s: %v", name, err)
		case !tc.ok && contracts.CodeOf(err) != contracts.ErrPermissionDenied:
			t.Errorf("%s: %v, want permission_denied", name, err)
		}
	}
	if err := (ragOperators{}).AuthorizeOperator(ctx, ragScope("t1", "w1", "ops-lead"), ingest.ActionConfirmMassDeletion); err == nil {
		t.Error("no operators configured and an operator action was allowed")
	}
}

func TestRAGJobAuthority(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.nopool = true })
	ctx := context.Background()
	f.register()
	a := ragAuthorizer{packages: f.pkgs, sources: f.stack.sources}
	sync := func(scope contracts.Scope, source string) jobs.Job {
		return jobs.Job{Scope: scope, Type: ingest.JobSync, Payload: json.RawMessage(fmt.Sprintf(`{"source_id":%q}`, source))}
	}

	t.Run("the scope check", func(t *testing.T) {
		if err := a.CheckScope(ctx, f.scope); err != nil {
			t.Errorf("an active application: %v", err)
		}
		if err := a.CheckScope(ctx, contracts.Scope{}); err == nil {
			t.Error("an empty scope passed")
		}
		f.pkgs.deactivate(ragApp)
		ragCodeOf(t, a.CheckScope(ctx, f.scope), contracts.ErrPermissionDenied)
		f.pkgs.set(ragApp, ragHash)
		f.pkgs.fail(contracts.New(contracts.ErrUnavailable, "the store is busy"))
		if code := contracts.CodeOf(a.CheckScope(ctx, f.scope)); !code.Retryable() {
			t.Errorf("a package state that could not be read is %q, want a retryable code so the attempt is retried", code)
		}
		f.pkgs.fail(errors.New("an untyped failure"))
		ragCodeOf(t, a.CheckScope(ctx, f.scope), contracts.ErrPermissionDenied)
		f.pkgs.fail(nil)
	})

	t.Run("the service identity", func(t *testing.T) {
		id := f.def.SourceID
		for name, tc := range map[string]struct {
			identity string
			job      jobs.Job
			ok       bool
		}{
			"a sync of a registered source":       {ragServiceIdentity, sync(f.scope, id), true},
			"another identity":                    {"somebody-else", sync(f.scope, id), false},
			"an empty identity":                   {"", sync(f.scope, id), false},
			"a source nobody registered":          {ragServiceIdentity, sync(f.scope, "src_01ARZ3NDEKTSV4RRFFQ69G5FAV"), false},
			"the source in another tenant":        {ragServiceIdentity, sync(ragScope("t2", "w1", "usr-1"), id), false},
			"a job type of another module":        {ragServiceIdentity, jobs.Job{Scope: f.scope, Type: "billing.charge", Payload: json.RawMessage(`{"source_id":"` + id + `"}`)}, false},
			"a payload with no source":            {ragServiceIdentity, jobs.Job{Scope: f.scope, Type: ingest.JobParse, Payload: json.RawMessage(`{}`)}, false},
			"a payload that is not an object":     {ragServiceIdentity, jobs.Job{Scope: f.scope, Type: ingest.JobParse, Payload: json.RawMessage(`[1]`)}, false},
			"maintenance, which names no source":  {ragServiceIdentity, jobs.Job{Scope: f.scope, Type: ingest.JobMaintain, Payload: json.RawMessage(`{}`)}, true},
			"an embedding job of a source":        {ragServiceIdentity, jobs.Job{Scope: f.scope, Type: vector.EmbedJobType, Payload: json.RawMessage(`{"generation":"gen_x","source_id":"` + id + `","chunk_ids":["c"]}`)}, true},
			"a reindex stage of a registered one": {ragServiceIdentity, jobs.Job{Scope: f.scope, Type: ingest.JobReindexIndex, Payload: json.RawMessage(`{"source_id":"` + id + `","version_id":"v"}`)}, true},
		} {
			err := a.AuthorizeService(ctx, tc.identity, tc.job)
			switch {
			case tc.ok && err != nil:
				t.Errorf("%s: %v", name, err)
			case !tc.ok && err == nil:
				t.Errorf("%s: authorised", name)
			}
		}
		f.pkgs.deactivate(ragApp)
		if err := a.AuthorizeService(ctx, ragServiceIdentity, sync(f.scope, id)); err == nil {
			t.Error("a quarantined application's source was synced under the service identity")
		}
		f.pkgs.set(ragApp, ragHash)
	})
}

// The stack queues the maintenance of every scope that holds a source on its own,
// under the ingestion service identity, and the pool runs it: deleted text does
// not wait for a source removal to be purged.
func TestRAGMaintenanceIsScheduled(t *testing.T) {
	f := newRagFixture(t)
	f.stack.input.DeploymentID = "dep-1"
	f.register()
	f.write("a.md", "# A\n\nalpha text")
	f.requestSync()
	f.drain()

	res, err := f.stack.maintainOnce(context.Background())
	if err != nil || res.Scopes != 1 || res.Enqueued != 1 {
		t.Fatalf("scheduled %+v, %v; want one job for the one scope", res, err)
	}
	f.drain()
	if n := f.countRows(`SELECT count(*) FROM jobs WHERE type = ? AND state = 'succeeded' AND service_identity = ?`, ingest.JobMaintain, ragServiceIdentity); n != 1 {
		t.Fatalf("%d maintenance jobs succeeded under the service identity\n%s", n, f.dumpJobs())
	}
}

// The production hook compares the free space of the data directory with the
// floor, and a volume that cannot be measured is pressure (the Pause reads an
// error that way): ingestion fails closed.
func TestRAGDiskPressureHook(t *testing.T) {
	ctx := context.Background()
	free := uint64(10 << 20)
	hook := func(floor int64, err error) ragDiskPressure {
		return ragDiskPressure{dir: "data", floor: floor, free: func(string) (uint64, error) { return free, err }}
	}
	for name, tc := range map[string]struct {
		h     ragDiskPressure
		under bool
		err   bool
	}{
		"plenty of room":           {hook(5<<20, nil), false, false},
		"exactly the floor":        {hook(10<<20, nil), false, false},
		"a byte under the floor":   {hook(10<<20+1, nil), true, false},
		"no floor":                 {hook(0, nil), false, false},
		"a volume that won't read": {hook(5<<20, errors.New("access denied")), false, true},
	} {
		under, err := tc.h.UnderPressure(ctx)
		if under != tc.under || (err != nil) != tc.err {
			t.Errorf("%s: under %v err %v", name, under, err)
		}
	}
	pause := ingest.NewPause(hook(5<<20, errors.New("access denied")), clock.NewFake(ragStart), time.Minute, nil)
	if v := pause.Verdict(ctx); !v.Paused || v.Reason != "disk_pressure_unknown" {
		t.Errorf("an unmeasurable volume did not pause ingestion: %+v", v)
	}

	// The measurement itself, on this platform.
	dir := t.TempDir()
	got, err := ragFreeBytes(dir)
	if err != nil || got == 0 {
		t.Fatalf("ragFreeBytes(%s) = %d, %v", dir, got, err)
	}
	if _, err := ragFreeBytes(filepath.Join(dir, "missing")); err == nil {
		t.Error("the free space of a directory that does not exist was read")
	}
	if under, err := ragNewDiskPressure(dir, 1).UnderPressure(ctx); err != nil || under {
		t.Errorf("one byte of floor on a temp directory: under %v, %v", under, err)
	}
	if under, err := ragNewDiskPressure(dir, 1<<62).UnderPressure(ctx); err != nil || !under {
		t.Errorf("a floor of 4 EiB: under %v, %v", under, err)
	}
}

func TestRAGConfigValidate(t *testing.T) {
	good := ragConfig{
		DiskFloorBytes: 1 << 30,
		Mounts:         map[string]string{"nas-1": filepath.Join(t.TempDir(), "nas")},
		Operators:      []ragOperator{{ApplicationID: "app", PrincipalID: "ops", Actions: []string{"confirm_mass_deletion"}}},
		Embedding:      ragEmbeddingConf{Model: "granite-embedding-97m", ComponentVersion: "97m-q8"},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a valid section: %v", err)
	}
	if err := (ragConfig{}).Validate(); err != nil {
		t.Fatalf("the empty section must be valid: %v", err)
	}
	for name, mutate := range map[string]func(*ragConfig){
		"a negative floor":                   func(c *ragConfig) { c.DiskFloorBytes = -1 },
		"a relative mount path":              func(c *ragConfig) { c.Mounts = map[string]string{"nas-1": "mnt/nas"} },
		"a mount id with a slash":            func(c *ragConfig) { c.Mounts = map[string]string{"a/b": "/x"} },
		"an operator with no principal":      func(c *ragConfig) { c.Operators[0].PrincipalID = "" },
		"an operator with no application":    func(c *ragConfig) { c.Operators[0].ApplicationID = "" },
		"an operator with no actions":        func(c *ragConfig) { c.Operators[0].Actions = nil },
		"an operator with an unknown action": func(c *ragConfig) { c.Operators[0].Actions = []string{"everything"} },
		"an operator listed twice": func(c *ragConfig) {
			c.Operators = append(c.Operators, c.Operators[0])
		},
		"an embedding with no version":          func(c *ragConfig) { c.Embedding.ComponentVersion = "" },
		"an embedding component with a capital": func(c *ragConfig) { c.Embedding.ComponentName = "Granite" },
		"an embedding with a negative batch":    func(c *ragConfig) { c.Embedding.BatchSize = -1 },
		"an embedding section with no model":    func(c *ragConfig) { c.Embedding = ragEmbeddingConf{ComponentVersion: "1"} },
	} {
		c := good
		c.Operators = slices.Clone(good.Operators)
		c.Operators[0].Actions = slices.Clone(good.Operators[0].Actions)
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The section reads as documented, strictly, through the config registry.
func TestRAGSectionLoadsFromYAML(t *testing.T) {
	read := func(doc string) (ragConfig, error) {
		sec := ragConfig{DiskFloorBytes: ragDefaultDiskFloor}
		reg := config.NewRegistry()
		reg.Register(ragSectionName, &sec)
		path := filepath.Join(t.TempDir(), "alixai.yaml")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		err := reg.LoadFrom(path, func(string) (string, bool) { return "", false })
		return sec, err
	}
	nas := filepath.ToSlash(filepath.Join(t.TempDir(), "nas"))
	sec, err := read(fmt.Sprintf(`rag:
  disk_floor_bytes: 2147483648
  mounts:
    nas-1: %q
  operators:
    - application_id: acme-clinic
      principal_id: ops-lead
      actions: [confirm_mass_deletion, delete_tenant]
  embedding:
    model: granite-embedding-97m
    component_version: 97m-q8
`, nas))
	if err != nil {
		t.Fatal(err)
	}
	if sec.DiskFloorBytes != 2<<30 || len(sec.Operators) != 1 || sec.Operators[0].PrincipalID != "ops-lead" || len(sec.Operators[0].Actions) != 2 ||
		sec.Mounts["nas-1"] != nas || sec.Embedding.Model != "granite-embedding-97m" {
		t.Errorf("section = %+v", sec)
	}
	if sec, err := read("rag:\n  embedding: {}\n"); err != nil || sec.DiskFloorBytes != ragDefaultDiskFloor || sec.floor() != ragDefaultDiskFloor {
		t.Errorf("the defaults: %+v, %v", sec, err)
	}
	for name, doc := range map[string]string{
		"an unknown key":  "rag:\n  operators_group: ops\n",
		"an invalid list": "rag:\n  operators:\n    - application_id: a\n      principal_id: p\n",
	} {
		if _, err := read(doc); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if !slices.Contains(config.Default.Sections(), ragSectionName) {
		t.Error("the rag section is not registered on the default registry")
	}
	if names, _ := registeredServeProviders(); !slices.Contains(names, "rag") {
		t.Errorf("the rag serve provider is not registered (have %v)", names)
	}
}

func TestRAGEmbeddingConfiguration(t *testing.T) {
	clk := clock.NewFake(ragStart)
	withEmbedder := func() *providers.Registry {
		reg := providers.NewRegistry(clk)
		if err := reg.Register(providers.RoleEmbedder, fake.New(fake.WithEmbedDim(8))); err != nil {
			t.Fatal(err)
		}
		return reg
	}
	conf := ragEmbeddingConf{Model: "Granite Embedding/97M", ComponentVersion: "q8"}
	in := func(reg *providers.Registry, c ragEmbeddingConf) ragInput {
		return ragInput{Providers: reg, Config: ragConfig{Embedding: c}, Log: slog.New(slog.DiscardHandler)}
	}
	if e, err := ragEmbedding(in(nil, conf)); e != nil || err != nil {
		t.Errorf("no registry: %+v, %v", e, err)
	}
	if e, err := ragEmbedding(in(providers.NewRegistry(clk), conf)); e != nil || err != nil {
		t.Errorf("no embedder role: %+v, %v", e, err)
	}
	if e, err := ragEmbedding(in(withEmbedder(), ragEmbeddingConf{})); e != nil || err != nil {
		t.Errorf("an embedder and no model configured must stay text-only: %+v, %v", e, err)
	}
	e, err := ragEmbedding(in(withEmbedder(), conf))
	if err != nil || e == nil {
		t.Fatalf("a configured embedder: %+v, %v", e, err)
	}
	if e.Model != "Granite Embedding/97M" || e.Component.Name != "granite-embedding-97m" || string(e.Component.Version) != "q8" {
		t.Errorf("embedding = %+v", e)
	}
	if !ragComponentRE.MatchString(e.Component.Name) {
		t.Errorf("component name %q is not valid", e.Component.Name)
	}
	conf.ComponentName = "my-embedder"
	if e, _ := ragEmbedding(in(withEmbedder(), conf)); e == nil || e.Component.Name != "my-embedder" {
		t.Errorf("an explicit component name was not used: %+v", e)
	}
	for in, want := range map[string]string{"": "embedder", "/-./": "embedder", "Ab C": "ab-c", strings.Repeat("x", 100): strings.Repeat("x", 64)} {
		if got := ragComponentName(in); got != want {
			t.Errorf("ragComponentName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Without the worker installed the parser is not an error at construction, only
// a probe that fails and parses that are refused.
func TestRAGLazyParserFailsClosedWithoutTheWorker(t *testing.T) {
	cfg := ingest.DefaultConfig()
	cfg.Parser.WorkerPath = filepath.Join(t.TempDir(), "alixai-parser-missing")
	p := newRagLazyParser(cfg, slog.New(slog.DiscardHandler))
	if p.Component() != parse.Component() {
		t.Errorf("component = %+v", p.Component())
	}
	if _, err := p.Probe(context.Background()); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("Probe without a worker = %v, want unavailable", err)
	}
	if _, err := p.Parse(context.Background(), wire.FormatMD, []byte("# a")); contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Errorf("Parse without a worker = %v, want unavailable", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close with no runner: %v", err)
	}
}

// start never blocks serve on the parser probe, and stop waits for what it
// started.
func TestRAGStartDoesNotWaitForTheProbe(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.nopool = true })
	release := make(chan struct{})
	f.parser.setProbe(func() (parse.SandboxReport, error) {
		<-release
		return parse.SandboxReport{}, errors.New("the worker is missing")
	})
	begin := time.Now()
	stop := f.stack.start(context.Background())
	if time.Since(begin) > 2*time.Second {
		t.Fatal("start waited for the probe")
	}
	if _, probed := f.stack.svc.Readiness(); probed {
		t.Fatal("the probe cannot have finished")
	}
	// A source registered while the probe runs makes /readyz report that the
	// parser has not been checked yet, never "ready".
	f.register()
	err := f.stack.readiness().Fn(context.Background())
	ragCodeOf(t, err, contracts.ErrUnavailable)
	if !strings.Contains(err.Error(), "not been checked") {
		t.Errorf("reason = %v", err)
	}
	close(release)
	stop()
	if !f.parser.closed.Load() {
		t.Error("stop did not close the parser")
	}
	if rd, probed := f.stack.svc.Readiness(); !probed || rd.Ready {
		t.Errorf("readiness after a failed probe = %+v, %v", rd, probed)
	}
	if err := f.stack.readiness().Fn(context.Background()); err == nil || strings.Contains(err.Error(), "missing") {
		t.Errorf("the check after a failed probe = %v: it must fail with a fixed phrase", err)
	}
	f.stack.sources.mu.Lock()
	f.stack.sources.entries = map[ragKey]*ragEntry{}
	f.stack.sources.mu.Unlock()
	if err := f.stack.readiness().Fn(context.Background()); err != nil {
		t.Errorf("with no source registered the check must pass: %v", err)
	}
}

// The provider hands serve what it promised: the two services in the gateway's
// registry, one readiness check and one service to start, and it refuses to
// start without the application service.
func TestRAGServeProvideAddsServicesChecksAndAStart(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.nopool = true })
	oldParser, oldDisk, oldHook := ragNewParser, ragNewDiskPressure, ragOnCompose
	t.Cleanup(func() { ragNewParser, ragNewDiskPressure, ragOnCompose = oldParser, oldDisk, oldHook })
	parser := newRagStubParser()
	var disk struct {
		dir   string
		floor int64
	}
	ragNewParser = func(ingest.Config, *slog.Logger) ingest.Parser { return parser }
	ragNewDiskPressure = func(dir string, floor int64) ingest.DiskPressure {
		disk.dir, disk.floor = dir, floor
		return &ragFakeDisk{}
	}
	var composed *ragStack
	ragOnCompose = func(s *ragStack) { composed = s }

	var checks []gateway.Check
	var starts []func(context.Context) func()
	services := gateway.NewServices()
	deps := serveDeps{
		Log: slog.New(slog.DiscardHandler), Clock: f.clk, DB: f.db, DataDir: "the-data-dir", Audit: f.aud,
		Services:   services,
		AddCheck:   func(c gateway.Check) { checks = append(checks, c) },
		AddService: func(s func(context.Context) func()) { starts = append(starts, s) },
	}
	if err := ragServeProvide(context.Background(), deps); err == nil || !strings.Contains(err.Error(), "application service") {
		t.Fatalf("without the application service: %v", err)
	}
	if err := gateway.Provide[gateway.ApplicationService](services, ragFakeApps{}); err != nil {
		t.Fatal(err)
	}
	if err := ragServeProvide(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	if _, ok := gateway.Lookup[*ingest.Service](services); !ok {
		t.Error("the ingestion service is not in the registry")
	}
	if _, ok := gateway.Lookup[*query.Pipeline](services); !ok {
		t.Error("the query pipeline is not in the registry")
	}
	if len(checks) != 1 || checks[0].Name != ragReadyCheck || len(starts) != 1 || composed == nil {
		t.Fatalf("checks %d, starts %d, composed %v", len(checks), len(starts), composed != nil)
	}
	if disk.dir != "the-data-dir" || disk.floor != ragDefaultDiskFloor {
		t.Errorf("the disk hook was built for %+v: it must watch the data directory against the configured floor", disk)
	}
	// An application with no active package is served nothing.
	if got, err := composed.sources.Sources(context.Background(), f.scope, []string{"src_x"}); err != nil || len(got) != 0 {
		t.Errorf("Sources = %+v, %v", got, err)
	}
	stop := starts[0](context.Background())
	stop()
	if !parser.closed.Load() {
		t.Error("the stop function did not close the parser")
	}
}

// ragFakeApps is a gateway.ApplicationService with no application installed.
type ragFakeApps struct{}

func (ragFakeApps) Package(context.Context, string) (capabilities.Package, error) {
	return nil, contracts.New(contracts.ErrQuarantined, "the application has no active package")
}
func (ragFakeApps) Records(context.Context, string) ([]apppkg.Record, error) { return nil, nil }
func (ragFakeApps) Register(context.Context, contracts.Scope, []byte) (*apppkg.Installed, error) {
	return nil, errors.New("not in this test")
}

// ---- the re-audit trigger -----------------------------------------------------------

// ragFakeReaudit is the active package's declarations, as a test controls them.
type ragFakeReaudit struct {
	mu      sync.Mutex
	calls   int
	hash    string
	rebuild ragRebuild
	err     error
}

func (r *ragFakeReaudit) Active(context.Context, string) (string, ragRebuild, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.hash, r.rebuild, r.err
}

// Declare is not what these tests exercise: the registration of a declared
// source has its own fake in cmd_rag_sources_test.go.
func (r *ragFakeReaudit) Declare(context.Context, contracts.Scope, string, string) (source.Definition, bool, error) {
	return source.Definition{}, false, contracts.New(contracts.ErrUnavailable, "the fake package reader declares nothing")
}

func (r *ragFakeReaudit) set(hash string, rebuild ragRebuild, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hash, r.rebuild, r.err = hash, rebuild, err
}

func (r *ragFakeReaudit) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// ragRebuildTo is a rebuild that binds the registered source to a package
// content, with the rules the mutations give.
func ragRebuildTo(f *ragFixture, hash string, mutate ...func(*ap.RAGSource)) ragRebuild {
	return func(old source.Definition) (source.Definition, bool, error) {
		d := ragTestDecl(f.root)
		for _, m := range mutate {
			m(&d)
		}
		def, err := source.FromDeclaration(d, source.Binding{SourceID: old.SourceID, TenantID: old.Tenant.ID, WorkspaceID: old.Workspace.ID})
		def.ManifestHash = hash
		return def, true, err
	}
}

// The process re-binds the registrations of an application whose package was
// replaced, by itself and only when it must: nothing is read while the content
// is the one the registrations were built from, a failure is not retried at
// once, and until it succeeds the sources are not served.
func TestRAGReconcileRebindsAReplacedPackage(t *testing.T) {
	re := &ragFakeReaudit{}
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.reaudit = re })
	ctx := context.Background()
	f.write("a.md", "# A\n\nAlpha dock.")
	f.register()
	f.drain()
	served := func() bool {
		got, err := f.stack.sources.Sources(ctx, f.scope, []string{f.def.SourceID})
		if err != nil {
			t.Fatal(err)
		}
		return len(got) == 1
	}
	syncs := func() int { return f.countRows(`SELECT count(*) FROM jobs WHERE type = ?`, ingest.JobSync) }

	// Nothing changed: one package lookup, and nothing read.
	if err := f.stack.sources.Reconcile(ctx); err != nil || re.count() != 0 || !served() {
		t.Fatalf("an unchanged package: err %v, %d reads, served %v", err, re.count(), served())
	}

	// The package is replaced and the rules changed.
	f.pkgs.set(ragApp, "pkg-hash-2")
	if served() {
		t.Fatal("a registration of the old package content is served")
	}
	re.set("pkg-hash-2", ragRebuildTo(f, "pkg-hash-2", func(d *ap.RAGSource) { d.Exclude = []ap.GlobPattern{"drafts/**"} }), nil)
	if err := f.stack.sources.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if re.count() != 1 || !served() || syncs() != 2 {
		t.Fatalf("after the re-audit: %d reads, served %v, %d syncs (want 1, true, 2)", re.count(), served(), syncs())
	}
	if err := f.stack.sources.Reconcile(ctx); err != nil || re.count() != 1 {
		t.Errorf("a rebound registration was read again: %v, %d reads", err, re.count())
	}

	// A package whose declarations cannot be read: the sources stay unserved, the
	// failure is not retried at once, and it is retried after the backoff.
	f.pkgs.set(ragApp, "pkg-hash-3")
	re.set("", nil, errors.New("the package no longer verifies"))
	if err := f.stack.sources.Reconcile(ctx); err == nil || re.count() != 2 || served() {
		t.Fatalf("a package that cannot be read: err %v, %d reads, served %v", err, re.count(), served())
	}
	if err := f.stack.sources.Reconcile(ctx); err != nil || re.count() != 2 {
		t.Errorf("a failed re-audit was retried inside the backoff: %v, %d reads", err, re.count())
	}
	f.clk.Advance(ragReauditBackoff + time.Second)
	if err := f.stack.sources.Reconcile(ctx); err == nil || re.count() != 3 {
		t.Errorf("after the backoff: err %v, %d reads", err, re.count())
	}
	// The package changed under the re-audit: not bound, and tried again later.
	f.clk.Advance(ragReauditBackoff + time.Second)
	re.set("pkg-hash-9", ragRebuildTo(f, "pkg-hash-9"), nil)
	if err := f.stack.sources.Reconcile(ctx); contracts.CodeOf(err) != contracts.ErrConflict || served() {
		t.Errorf("a package that changed while it was read: %v, served %v", err, served())
	}
	// A rebuild that fails for the source counts as a failed re-audit.
	f.clk.Advance(ragReauditBackoff + time.Second)
	re.set("pkg-hash-3", func(source.Definition) (source.Definition, bool, error) {
		return source.Definition{}, true, errors.New("the declaration is invalid")
	}, nil)
	if err := f.stack.sources.Reconcile(ctx); err == nil || served() {
		t.Errorf("a source that cannot be rebuilt: %v, served %v", err, served())
	}
	// Fixed: it is served again.
	f.clk.Advance(ragReauditBackoff + time.Second)
	re.set("pkg-hash-3", ragRebuildTo(f, "pkg-hash-3"), nil)
	if err := f.stack.sources.Reconcile(ctx); err != nil || !served() {
		t.Fatalf("after the fix: %v, served %v", err, served())
	}

	// An application that is not active is skipped (it serves nothing anyway), an
	// unreadable package state is an error, and with no re-audit source it is off.
	reads := re.count()
	f.pkgs.deactivate(ragApp)
	if err := f.stack.sources.Reconcile(ctx); err != nil || re.count() != reads {
		t.Errorf("an inactive application: %v, %d reads", err, re.count()-reads)
	}
	f.pkgs.set(ragApp, "pkg-hash-3")
	f.pkgs.fail(errors.New("the package store is down"))
	if err := f.stack.sources.Reconcile(ctx); err == nil {
		t.Error("an unreadable package state was not reported")
	}
	f.pkgs.fail(nil)
	f.stack.sources.reaudit = nil
	if err := f.stack.sources.Reconcile(ctx); err != nil {
		t.Errorf("with no re-audit source: %v", err)
	}
}

// When ingestion cannot record the comparison of a re-audited definition (here a
// full queue), the registration keeps its old package content: the source stays
// unserved and the next re-audit tries again, instead of being bound to a
// definition no sync was ever queued for.
func TestRAGReauditRestoresTheRegistrationWhenIngestionRefuses(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) {
		o.jobs = func(c *jobs.Config) { c.Class5 = jobs.ClassConfig{Workers: 1, MaxDepth: 1} }
	})
	ctx := context.Background()
	f.register() // its sync is queued and fills the class
	f.pkgs.set(ragApp, "pkg-hash-2")
	changed := ragRebuildTo(f, "pkg-hash-2", func(d *ap.RAGSource) { d.Exclude = []ap.GlobPattern{"drafts/**"} })
	rep, err := f.stack.sources.Reaudit(ctx, ragApp, changed)
	if contracts.CodeOf(err) != contracts.ErrResourceExhausted || rep.Failed != 1 || rep.Rebound != 0 {
		t.Fatalf("Reaudit with a full queue = %+v, %v", rep, err)
	}
	if e := f.stack.sources.lookup(f.scope, f.def.SourceID); e == nil || e.def.ManifestHash != ragHash {
		t.Fatalf("the registration was not put back: %+v", e)
	}
	if got, _ := f.stack.sources.Sources(ctx, f.scope, []string{f.def.SourceID}); len(got) != 0 {
		t.Error("a source bound to a definition nobody queued a sync for is served")
	}
	f.drain()
	if rep, err := f.stack.sources.Reaudit(ctx, ragApp, changed); err != nil || rep.Rebound != 1 || rep.Queued != 1 {
		t.Fatalf("Reaudit once the queue has room = %+v, %v", rep, err)
	}
}

// The installer-backed source reports an application with no active package as
// quarantined, which is what Reconcile reads as "nothing to bind".
func TestRAGInstallerSourceWithoutAnActivePackage(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.nopool = true })
	inst, err := apppkg.NewInstaller(apppkg.InstallerOptions{
		DB: f.db, Clock: f.clk, DataDir: t.TempDir(),
		Verifier: trust.NewVerifier(trust.NewStore(f.db, f.clk), f.clk), CoreVersion: apppkg.DevCoreVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	var src ragReauditSource = ragInstallerSource{inst: inst}
	if _, _, err := src.Active(context.Background(), ragApp); contracts.CodeOf(err) != contracts.ErrQuarantined {
		t.Errorf("Active with nothing installed = %v, want quarantined", err)
	}
}

// The mounts the operator registered are what source definitions resolve.
func TestRAGMountsOfTheConfiguration(t *testing.T) {
	if ragMountsOf(ragConfig{}) != nil {
		t.Error("no mounts must give no resolver: a mount root is then refused")
	}
	nas := filepath.Join(t.TempDir(), "nas")
	cfg := ragConfig{Mounts: map[string]string{"nas-1": nas}}
	resolve := ragMountsOf(cfg)
	if p, ok := resolve("nas-1"); !ok || p != nas {
		t.Errorf("nas-1 = %q, %v", p, ok)
	}
	if _, ok := resolve("nas-2"); ok {
		t.Error("an unregistered mount resolved")
	}
	cfg.Mounts["nas-2"] = "/elsewhere"
	if _, ok := resolve("nas-2"); ok {
		t.Error("the resolver follows later changes to the configuration map")
	}
}

// An embedder role with no model named in the section keeps ingestion text-only, and
// composing without a logger must not fail on the way there.
func TestRAGEmbedderWithoutAModelStaysTextOnly(t *testing.T) {
	f := newRagFixture(t, func(o *ragFixtureOpts) { o.embedderOnly, o.nopool = true, true })
	if f.stack.vectors != nil || f.stack.model != "" {
		t.Errorf("a vector index or model was configured without rag.embedding: %v %q", f.stack.vectors, f.stack.model)
	}
}

// TestBenchRAGCatalog times what the query pipeline pays the catalog: it calls
// Sources twice per query (before retrieval and again just before the package is
// built), and each call asks the ingestion service for the status of each source.
// It is skipped unless ALIXAI_RAG_CATALOG_BENCH lists corpus sizes (documents in
// the one source):
//
//	ALIXAI_RAG_CATALOG_BENCH=500,2000 go test -run TestBenchRAGCatalog -v ./internal/cli/
func TestBenchRAGCatalog(t *testing.T) {
	spec := os.Getenv("ALIXAI_RAG_CATALOG_BENCH")
	if spec == "" {
		t.Skip("set ALIXAI_RAG_CATALOG_BENCH=500,2000 to run the catalog measurement")
	}
	for _, field := range strings.Split(spec, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < 1 {
			t.Fatalf("ALIXAI_RAG_CATALOG_BENCH: %q is not a corpus size", field)
		}
		t.Run(fmt.Sprintf("%d docs", n), func(t *testing.T) {
			f := newRagFixture(t, func(o *ragFixtureOpts) {
				o.jobs = func(c *jobs.Config) { c.Class5 = jobs.ClassConfig{Workers: 4, MaxDepth: 5000} }
			})
			for i := 0; i < n; i++ {
				f.write(fmt.Sprintf("d%05d.md", i), fmt.Sprintf("# Doc %d\n\nThe harbour dock schedule %d lists the tide tables.", i, i))
			}
			f.register()
			begin := time.Now()
			f.drain()
			t.Logf("ingested %d documents in %.1fs", n, time.Since(begin).Seconds())
			ctx := context.Background()
			ids := []string{f.def.SourceID}
			timeIt := func(rounds int, run func()) (p50, p95 time.Duration) {
				lat := make([]time.Duration, 0, rounds)
				for i := 0; i < rounds; i++ {
					t0 := time.Now()
					run()
					lat = append(lat, time.Since(t0))
				}
				sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })
				return lat[len(lat)/2], lat[len(lat)*95/100]
			}
			ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
			measure := func(label string, ttl time.Duration) {
				f.stack.sources.statusTTL = ttl
				f.stack.sources.states = map[ragKey]ragCachedState{}
				s50, s95 := timeIt(100, func() {
					if got, err := f.stack.sources.Sources(ctx, f.scope, ids); err != nil || len(got) != 1 {
						t.Fatalf("Sources = %+v, %v", got, err)
					}
				})
				q50, q95 := timeIt(30, func() {
					if _, err := f.stack.pipeline.Query(ctx, f.scope, query.Request{Text: "harbour dock schedule"}); err != nil {
						t.Fatal(err)
					}
				})
				t.Logf("%d documents, %s: Sources (one source) p50 / p95 %.2f / %.2f ms; whole query (catalog called twice) %.2f / %.2f ms",
					n, label, ms(s50), ms(s95), ms(q50), ms(q95))
			}
			measure("label read every call", 0)
			measure("label reused for the default time to live", ragStatusTTL)
		})
	}
}

// The availability label of a source is reused for a few seconds, because the
// status behind it costs more the more documents the source holds and the
// pipeline asks twice per query; an authorisation is never reused: the package,
// the registration and the scope are checked afresh on every call.
func TestRAGCatalogReusesOnlyTheLabel(t *testing.T) {
	f := newRagFixture(t)
	ctx := context.Background()
	f.write("a.md", "# A\n\nAlpha.")
	f.register()
	f.drain()
	id := f.def.SourceID
	src := f.stack.sources
	src.statusTTL = time.Hour
	state := func(scope contracts.Scope) (query.SourceState, bool) {
		t.Helper()
		got, err := src.Sources(ctx, scope, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		st, ok := got[id]
		return st, ok
	}
	if st, ok := state(f.scope); !ok || st.Availability != conversation.CitationAvailabilityAvailable {
		t.Fatalf("before the root went away: %+v, %v", st, ok)
	}

	// The root goes away and the next scan says so, but the label is reused.
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatal(err)
	}
	f.requestSync()
	f.drain()
	if info, err := f.stack.svc.Status(ctx, f.scope, id); err != nil || info.Available {
		t.Fatalf("the scan did not mark the source unavailable: %+v, %v", info, err)
	}
	if st, _ := state(f.scope); st.Availability != conversation.CitationAvailabilityAvailable {
		t.Errorf("the label was read again inside its time to live: %+v", st)
	}
	// ...but nothing about who may read it is reused.
	f.pkgs.deactivate(ragApp)
	if _, ok := state(f.scope); ok {
		t.Error("a cached label served a source of a quarantined package")
	}
	f.pkgs.set(ragApp, "pkg-hash-9")
	if _, ok := state(f.scope); ok {
		t.Error("a cached label served a registration of another package content")
	}
	f.pkgs.set(ragApp, ragHash)
	if _, ok := state(ragScope("t2", "w1", "usr-1")); ok {
		t.Error("a cached label served another tenant")
	}
	if st, ok := state(f.scope); !ok || st.Availability != conversation.CitationAvailabilityAvailable {
		t.Errorf("the label after the package came back: %+v, %v", st, ok)
	}
	// After its time to live the label is read again, and now it is the true one.
	f.clk.Advance(2 * time.Hour)
	if st, _ := state(f.scope); st.Availability != conversation.CitationAvailabilityUnavailable {
		t.Errorf("the label after its time to live: %+v", st)
	}
	// No caching at all when the time to live is zero.
	src.statusTTL = 0
	src.states = map[ragKey]ragCachedState{}
	state(f.scope)
	if len(src.states) != 0 {
		t.Error("a label was kept with a zero time to live")
	}
	// Removing and registering again never serve a label of the earlier registration.
	src.statusTTL = time.Hour
	state(f.scope)
	if len(src.states) != 1 {
		t.Fatalf("%d labels cached, want 1", len(src.states))
	}
	if err := src.Remove(ctx, f.scope, id); err != nil {
		t.Fatal(err)
	}
	if len(src.states) != 0 {
		t.Error("removing a source left its label behind")
	}
}
