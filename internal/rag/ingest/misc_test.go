package ingest

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/jobs"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/parse"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

func TestTablesCarryTheScopeColumns(t *testing.T) {
	f := newFixture(t)
	// The tables of other modules in the shared database are theirs to assert.
	storetest.AssertScopedTables(t, f.db, "apppkg_packages", "trust_publisher_keys")
	for _, table := range []string{"ragingest_source", "ragingest_item", "ragingest_generation", "ragingest_version", "ragingest_embed", "ragingest_tombstone", "ragingest_stage"} {
		if f.countRows(`SELECT count(*) FROM pragma_table_info(?) WHERE name IN ('application_id', 'tenant_id', 'workspace_id')`, table) != 3 {
			t.Errorf("%s lacks a scope column", table)
		}
	}
}

func TestConfig(t *testing.T) {
	d := DefaultConfig()
	if d.Parser.StartupTimeout <= parse.DefaultConfig().StartupTimeout {
		t.Errorf("the worker's startup limit (%s) is not wider than the parser's own (%s): an antivirus scan of a fresh worker took 26.5 s",
			d.Parser.StartupTimeout, parse.DefaultConfig().StartupTimeout)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("the defaults do not validate: %v", err)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("the zero value does not validate: %v", err)
	}
	// The parser configuration an operator's settings produce is the parser's own
	// defaults with the startup limit, worker path and limits laid over them.
	c := Config{Parser: ParserConfig{WorkerPath: filepathAbs("alixai-parser"), StartupTimeout: 2 * time.Minute, WallTimeout: 45 * time.Second, MaxConcurrent: 2}}
	pc := c.ParseConfig()
	if err := pc.Validate(); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	mustEqual(t, "startup timeout", pc.StartupTimeout, 2*time.Minute)
	mustEqual(t, "wall timeout", pc.WallTimeout, 45*time.Second)
	mustEqual(t, "concurrency", pc.MaxConcurrent, 2)
	mustEqual(t, "memory", pc.MemoryBytes, parse.DefaultConfig().MemoryBytes)
	if pc.OCR.Enabled {
		t.Error("OCR is enabled by default")
	}
	for _, bad := range []Config{
		{Guard: GuardConfig{MaxFraction: 1.5}},
		{Guard: GuardConfig{MaxFraction: -0.1}},
		{Guard: GuardConfig{MaxDeleted: -1}},
		{Guard: GuardConfig{MinPrevious: -1}},
		{Parser: ParserConfig{StartupTimeout: time.Millisecond}},
		{Parser: ParserConfig{WallTimeout: -time.Second}},
		{Parser: ParserConfig{MaxConcurrent: 65}},
		{Parser: ParserConfig{ProbeTimeout: -time.Second}},
		{EnqueueBatch: -1},
		{EnqueueBatch: 200_000},
		{PressureRecheck: -time.Second},
		{ConfirmTTL: -time.Second},
		{CompactGrace: -time.Hour},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v validated", bad)
		}
	}
	if FromConfig().Guard.MaxFraction == 0 {
		t.Error("the registered section has no defaults")
	}
}

func filepathAbs(name string) string {
	if runtime.GOOS == "windows" {
		return `C:\opt\alixai\` + name + ".exe"
	}
	return "/opt/alixai/" + name
}

func TestNewRefusesAnIncompleteSetup(t *testing.T) {
	f := newFixture(t)
	base := Deps{
		DB: f.db, Clock: f.clk, Queue: f.q, Index: f.ix, Registry: f.reg, Parser: f.parser, Audit: f.aud,
	}
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	base.Chunker = chk
	for _, c := range []struct {
		name   string
		mutate func(*Deps)
	}{
		{"no store", func(d *Deps) { d.DB = nil }},
		{"no queue", func(d *Deps) { d.Queue = nil }},
		{"no index", func(d *Deps) { d.Index = nil }},
		{"no registry", func(d *Deps) { d.Registry = nil }},
		{"no parser", func(d *Deps) { d.Parser = nil }},
		{"no chunker", func(d *Deps) { d.Chunker = nil }},
		{"no audit log: a deletion that cannot be recorded does not proceed", func(d *Deps) { d.Audit = nil }},
		{"embedding without a vector index", func(d *Deps) {
			d.Embedding = &Embedding{Embedder: f.emb, Model: "m", Component: rag.ComponentVersion{Name: "emb", Version: "1"}}
		}},
		{"embedding without a model", func(d *Deps) {
			d.Vectors = f.vec
			d.Embedding = &Embedding{Embedder: f.emb, Component: rag.ComponentVersion{Name: "emb", Version: "1"}}
		}},
		{"embedder component name that is not one", func(d *Deps) {
			d.Vectors = f.vec
			d.Embedding = &Embedding{Embedder: f.emb, Model: "m", Component: rag.ComponentVersion{Name: "Bad Name", Version: "1"}}
		}},
		{"invalid configuration", func(d *Deps) { d.Config.Guard.MaxFraction = 3 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := base
			c.mutate(&d)
			_, err := New(d)
			codeOf(t, err, contracts.ErrValidationFailed)
		})
	}
}

func TestPausedJobTypes(t *testing.T) {
	types := PausedJobTypes()
	has := func(n string) bool {
		for _, x := range types {
			if x == n {
				return true
			}
		}
		return false
	}
	for _, want := range []string{JobSync, JobParse, JobIndex, JobFinalize, JobReindexParse, JobReindexIndex, JobReindexFinalize, vector.EmbedJobType} {
		if !has(want) {
			t.Errorf("%s is not held back by disk pressure", want)
		}
	}
	if has(JobMaintain) {
		t.Error("maintenance only removes data and must not be held back")
	}
}

func TestPausePolicyDefersToTheInnerPolicy(t *testing.T) {
	under := false
	clk := clock.NewFake(clockStart)
	p := NewPause(DiskPressureFunc(func(context.Context) (bool, error) { return under, nil }), clk, time.Second, nil)
	ready := []jobs.Candidate{
		{ID: "a", Type: JobParse, Class: jobs.ClassIngest},
		{ID: "b", Type: "other.job", Class: jobs.ClassIngest},
	}
	last := jobs.DequeuePolicyFunc(func(r []jobs.Candidate) int { return len(r) - 1 })
	pol := p.Policy(last)
	mustEqual(t, "without pressure the inner policy picks", pol.Pick(ready), 1)
	under = true
	clk.Advance(time.Minute)
	mustEqual(t, "under pressure only the other work is offered", pol.Pick(ready), 1)
	mustEqual(t, "under pressure nothing of ingestion", pol.Pick(ready[:1]), -1)
	mustEqual(t, "nothing ready", pol.Pick(nil), -1)
	// With no inner policy the oldest eligible job is taken.
	under = false
	clk.Advance(time.Minute)
	mustEqual(t, "fifo", p.Policy(nil).Pick(ready), 0)
	mustEqual(t, "fifo, nothing ready", p.Policy(nil).Pick(nil), -1)
	// A nil pause never pauses.
	var none *Pause
	mustEqual(t, "a nil pause", none.Verdict(context.Background()).Paused, false)
}

func TestReadinessReportsTheSandboxLevel(t *testing.T) {
	t.Run("L2", func(t *testing.T) {
		f := newFixture(t)
		rd, err := f.svc.Start(bg)
		if err != nil || !rd.Ready || rd.Level != parse.LevelL2 {
			t.Fatalf("Start = %+v, %v", rd, err)
		}
		for _, w := range rd.Warnings {
			if strings.Contains(w, "import-graph") {
				t.Errorf("an L2 worker is told its confinement rests on the import graph: %s", w)
			}
		}
		got, ok := f.svc.Readiness()
		if !ok || got.Level != parse.LevelL2 {
			t.Fatalf("Readiness = %+v, %v", got, ok)
		}
		joined := strings.Join(rd.Warnings, "\n")
		if !strings.Contains(joined, "one worker process is started per document") {
			t.Errorf("the throughput budget is not reported: %s", joined)
		}
		if runtime.GOOS == "windows" && !strings.Contains(joined, "antivirus") {
			t.Errorf("no antivirus advice on Windows: %s", joined)
		}
	})

	t.Run("L1 says what 'no network' rests on", func(t *testing.T) {
		f := newFixture(t)
		f.parser.probe = func() (parse.SandboxReport, error) {
			return parse.SandboxReport{Level: parse.LevelL1, Platform: "linux", Missing: []string{"seccomp: blocked by the container's profile"}}, nil
		}
		rd, err := f.svc.Start(bg)
		if err != nil || rd.Level != parse.LevelL1 {
			t.Fatalf("Start = %+v, %v", rd, err)
		}
		joined := strings.Join(rd.Warnings, "\n")
		for _, want := range []string{"L1", "import-graph", "seccomp: blocked by the container's profile"} {
			if !strings.Contains(joined, want) {
				t.Errorf("the warnings do not mention %q:\n%s", want, joined)
			}
		}
	})

	t.Run("a slow first start is reported", func(t *testing.T) {
		f := newFixture(t)
		f.parser.probe = func() (parse.SandboxReport, error) {
			f.clk.Advance(60 * time.Second) // 26.5 s took a 30 s limit to the edge under Avast
			return parse.SandboxReport{Level: parse.LevelL2}, nil
		}
		rd, err := f.svc.Start(bg)
		if err != nil {
			t.Fatal(err)
		}
		mustEqual(t, "probe time", rd.ProbeTime, 60*time.Second)
		mustEqual(t, "startup limit", rd.StartupTimeout, DefaultStartupTimeout)
		if !strings.Contains(strings.Join(rd.Warnings, "\n"), "exclude the executable from antivirus scanning") {
			t.Errorf("a slow start is not explained: %v", rd.Warnings)
		}
	})

	t.Run("a worker that cannot start fails parse jobs closed, and a fixed one is picked up", func(t *testing.T) {
		f := newFixture(t)
		corpus(f, 1)
		broken := true
		f.parser.probe = func() (parse.SandboxReport, error) {
			if broken {
				return parse.SandboxReport{}, contracts.New(contracts.ErrUnavailable, "the sandbox could not be installed")
			}
			return parse.SandboxReport{Level: parse.LevelL2}, nil
		}
		rd, err := f.svc.Start(bg)
		codeOf(t, err, contracts.ErrUnavailable)
		if rd.Ready || rd.Err == "" {
			t.Fatalf("readiness = %+v", rd)
		}
		f.sync()
		mustEqual(t, "documents parsed by a worker that never started", f.parser.calls.Load(), 0)
		mustEqual(t, "documents searchable", len(f.search("alpha")), 0)

		// The worker is fixed: a minute later a parse job probes again and goes on.
		broken = false
		f.clk.Advance(2 * time.Minute)
		f.sync()
		mustEqual(t, "documents searchable after the fix", len(docHits(f.search("alpha"))), 1)
		got, _ := f.svc.Readiness()
		mustEqual(t, "ready", got.Ready, true)
	})

	t.Run("the probe is bounded", func(t *testing.T) {
		f := newFixture(t, func(o *fixtureOpts) { o.config = func(c *Config) { c.Parser.ProbeTimeout = 30 * time.Millisecond } })
		hang := &hangingParser{stubParser: f.parser}
		f.svc.parser = hang
		start := time.Now()
		_, err := f.svc.Start(bg)
		if err == nil {
			t.Fatal("a probe that never returned was accepted")
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("the probe was not bounded: %s", time.Since(start))
		}
	})
}

type hangingParser struct{ *stubParser }

func (h *hangingParser) Probe(ctx context.Context) (parse.SandboxReport, error) {
	<-ctx.Done()
	return parse.SandboxReport{}, contracts.Wrap(contracts.ErrTimeout, ctx.Err(), "the worker did not start")
}

func TestRemovalFailsClosedWithoutAnAuditTrail(t *testing.T) {
	f := newFixture(t)
	corpus(f, 2)
	f.sync()
	f.svc.aud = failingAudit{}
	if err := f.svc.RemoveSource(bg, f.scope, f.srcID); err == nil {
		t.Fatal("a source was deleted although the deletion could not be recorded")
	}
	mustEqual(t, "documents searchable", len(docHits(f.search("alpha"))), 2)
	mustEqual(t, "snapshot rows", f.countRows(`SELECT count(*) FROM ragingest_item`), 2)
	mustEqual(t, "availability", f.status().Available, true)
	f.oper.allow(true)
	if err := f.svc.RemoveTenant(bg, f.scope); err == nil {
		t.Fatal("a tenant was deleted although the deletion could not be recorded")
	}
	mustEqual(t, "documents searchable after the tenant attempt", len(docHits(f.search("alpha"))), 2)
}

func TestMaintenanceSweepsWhatNothingServes(t *testing.T) {
	f := newFixture(t)
	corpus(f, 2)
	f.sync()

	// Two rebuilds leave two superseded generations; the index keeps one of them
	// for a rollback and compacts the other away, and the pipeline forgets it.
	f.parser.setVersion("2.0.0")
	f.sync()
	f.parser.setVersion("3.0.0")
	f.sync()
	mustEqual(t, "generation records", f.countRows(`SELECT count(*) FROM ragingest_generation`), 3)
	// Vectors that belong to no generation that can serve (a batch stored while a
	// generation was being withdrawn, say) are swept.
	uvec := make([]float32, 8)
	uvec[0] = 1
	if _, err := f.vec.Upsert(bg, f.scope, vector.UpsertBatch{
		Generation: "gen_orphaned", SourceID: f.srcID, Model: "fake-model",
		Items: []vector.Item{{ChunkID: "chk_orphan", Key: mustKey(t, f.scope, "fake-model", "x"), Vector: uvec}},
	}); err != nil {
		t.Fatal(err)
	}
	withOrphan := f.vectors()

	f.clk.Advance(f.cfg.withDefaults().CompactGrace + time.Hour)
	if _, err := f.svc.RequestMaintenance(bg, f.scope, time.Time{}); err != nil {
		t.Fatal(err)
	}
	f.drain()
	mustEqual(t, "generation records after maintenance", f.countRows(`SELECT count(*) FROM ragingest_generation`), 2)
	mustEqual(t, "generations of the index", len(f.generations()), 2)
	mustEqual(t, "version records follow their generations", f.countRows(`SELECT count(DISTINCT generation_id) FROM ragingest_version`), 2)
	mustEqual(t, "orphan vector", f.vectors(), withOrphan-1)
	mustEqual(t, "documents searchable", len(docHits(f.search("alpha"))), 2)
}

// One scope's maintenance never fails another scope's build: a build that began
// long ago and is still being written is not abandoned, and a removal in one
// tenant does not reach into another.
func TestMaintenanceOfOneScopeLeavesAnotherScopesBuildAlone(t *testing.T) {
	f := newFixture(t)
	corpus(f, 2)
	f.sync()
	other := f.addSource("tenant-2", "ws-1", "slow", 0)
	build, err := f.ix.BeginGeneration(bg, other.scope, other.def.SourceID, f.svc.components())
	if err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(30 * time.Hour)
	if _, err := f.ix.PutDocument(bg, other.scope, string(build.GenerationID), index.Document{ID: "doc-late", Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(time.Hour)

	if err := f.svc.RemoveSource(bg, f.scope, f.srcID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RequestMaintenance(bg, f.scope, time.Time{}); err != nil {
		t.Fatal(err)
	}
	f.drain()
	g, err := f.ix.GetGeneration(bg, other.scope, string(build.GenerationID))
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "the other tenant's build", g.State, rag.IndexGenerationStateBuilding)
}

// The text of a deleted document leaves the database once its purge window and
// the compaction grace have passed, with no source removal to trigger it: the
// maintenance pass is scheduled for every scope that holds a source.
func TestScheduledMaintenancePurgesADeletedDocument(t *testing.T) {
	f := newFixture(t)
	corpus(f, 2)
	f.sync()
	f.remove("doc1.md")
	f.sync()
	gone := documentID(f.srcID, "doc1.md")
	if f.countRows(`SELECT count(*) FROM ragindex_chunk WHERE document_id = ?`, gone) == 0 {
		t.Fatal("precondition: a deleted document is hidden at once and purged later")
	}
	other := f.addSource("tenant-2", "ws-1", "kept", 1)
	f.syncOther(other)

	f.clk.Advance(max(f.def.Deletion.PurgeAfter, f.cfg.withDefaults().CompactGrace) + time.Hour)
	res, err := f.svc.ScheduleMaintenance(bg, "dep-1", "")
	if err != nil {
		t.Fatal(err)
	}
	mustEqual(t, "scopes", res.Scopes, 2)
	mustEqual(t, "queued", res.Enqueued, 2)
	again, err := f.svc.ScheduleMaintenance(bg, "dep-1", "")
	if err != nil || again.Enqueued != 0 || again.Existing != 2 {
		t.Fatalf("a second pass in the same period queued again: %+v, %v", again, err)
	}
	f.drain()
	mustEqual(t, "chunk rows of the deleted document", f.countRows(`SELECT count(*) FROM ragindex_chunk WHERE document_id = ?`, gone), 0)
	mustEqual(t, "its tombstone", f.countRows(`SELECT count(*) FROM ragingest_tombstone WHERE document_id = ?`, gone), 0)
	mustEqual(t, "the document that stays", len(docHits(f.search("p0a"))), 1)
	mustEqual(t, "the other scope's document", len(docHits(f.searchIn(other.scope, "kept0a"))), 1)
}

// A vector deletion interrupted between its two phases (the generation hidden,
// its rows not yet removed) is finished by maintenance, so the rows stop
// counting against the vector ceilings.
func TestMaintenanceFinishesAnInterruptedVectorDrop(t *testing.T) {
	f := newFixture(t)
	corpus(f, 2)
	f.sync()
	uvec := make([]float32, 8)
	uvec[0] = 1
	if _, err := f.vec.Upsert(bg, f.scope, vector.UpsertBatch{
		Generation: "gen_dropping", SourceID: f.srcID, Model: "fake-model",
		Items: []vector.Item{{ChunkID: "chk_dropping", Key: mustKey(t, f.scope, "fake-model", "x"), Vector: uvec}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(bg, `UPDATE vector_generations SET state = 'dropping' WHERE generation_id = 'gen_dropping'`); err != nil {
		t.Fatal(err)
	}
	vecs := f.vectors()

	if _, err := f.svc.RequestMaintenance(bg, f.scope, time.Time{}); err != nil {
		t.Fatal(err)
	}
	f.drain()
	mustEqual(t, "rows of the interrupted drop", f.countRows(`SELECT count(*) FROM vector_rows WHERE generation_id = 'gen_dropping'`), 0)
	mustEqual(t, "the interrupted drop's generation", f.countRows(`SELECT count(*) FROM vector_generations WHERE generation_id = 'gen_dropping'`), 0)
	mustEqual(t, "serving vectors", f.vectors(), vecs)
}

func mustKey(t *testing.T, scope contracts.Scope, model, input string) vector.InputKey {
	t.Helper()
	k, err := vector.NewInputKey(scope, model, input)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// SEC-023: embeddings are never reused across tenants.
func TestEmbeddingsAreNeverReusedAcrossTenants(t *testing.T) {
	// SEC-023: identical content in another tenant is embedded afresh; reuse must
	// not be an existence oracle.
	f := newFixture(t)
	corpus(f, 2)
	f.sync()
	before := f.counters()

	other := f.addSource("tenant-2", "ws-1", "same", 0)
	for i := 0; i < 2; i++ {
		name := "doc" + string(rune('0'+i)) + ".md"
		f.writeIn(other.root, name, doc("Title "+string(rune('0'+i)), para("p"+string(rune('0'+i))+"a"), para("p"+string(rune('0'+i))+"b"), para("p"+string(rune('0'+i))+"c")))
	}
	f.syncOther(other)
	after := f.counters()
	mustEqual(t, "chunks embedded for the second tenant", after.embedInputs-before.embedInputs, before.embedInputs)
	mustEqual(t, "vectors of the second tenant", f.vectorsIn(other.scope), f.vectors())
}

func TestStatusOfASourceThatWasNeverScanned(t *testing.T) {
	f := newFixture(t)
	_, err := f.svc.Status(bg, f.scope, f.srcID)
	codeOf(t, err, contracts.ErrValidationFailed)
}
