package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/rag/answer"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// corpusDir is the corpus of the repository, which the tests use as it is.
const corpusDir = "../../tests/performance/rag"

func loadTestCorpus(t testing.TB) *Corpus {
	t.Helper()
	c, err := LoadCorpus(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testEvalConfig(t testing.TB, mode string) EvalConfig {
	t.Helper()
	cfg := EvalConfig{
		Mode: mode, K: []int{1, 3, 5, 8}, Query: query.DefaultConfig(), Ans: answer.DefaultConfig(),
		Gen: newExtractiveGenerator(), Warmup: 1, TempDir: t.TempDir(),
	}
	if mode != ModeFTS {
		cfg.Embedder, cfg.Model = newHashEmbedder(), fakeModel
	}
	return cfg
}

func byID(recs []QuestionRecord) map[string]QuestionRecord {
	m := map[string]QuestionRecord{}
	for _, r := range recs {
		m[r.ID] = r
	}
	return m
}

// testRunEnv is what the command line would have decided for a fake run.
func testRunEnv(t testing.TB, mode string, gen *extractiveGenerator) *runEnv {
	t.Helper()
	m, err := ResolveModel("fake", fakeModelID, "")
	if err != nil {
		t.Fatal(err)
	}
	env := &runEnv{
		opts: runOptions{mode: mode, warmup: 1}, profile: models.ProfileLite, ks: []int{1, 3, 5, 8},
		qcfg: query.DefaultConfig(), acfg: answer.DefaultConfig(), ccfg: chunk.DefaultConfig(),
		embedderKey: "fake", modelID: fakeModel, provider: "fake", model: m, gen: gen,
	}
	env.backend = backendFor("fake", nil, "")
	return env
}

func benchSchemaForTest(t testing.TB) (schemas string) {
	t.Helper()
	dir, err := FindSchemasDir("")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRunWithFakeEmbedder runs the whole tool in process over the repository's
// corpus with the fake embedder and checks what the run did, not only that it
// ran: the embedder took part in every query, the hard cases behaved (deleted,
// denied, other tenant, superseded version, unavailable, stale), nothing leaked,
// the result is reproducible, and the two result files pass their own checks.
func TestRunWithFakeEmbedder(t *testing.T) {
	ctx := context.Background()
	corpus := loadTestCorpus(t)
	cfg := testEvalConfig(t, ModeHybrid)
	emb := cfg.Embedder.(*hashEmbedder)

	ev, err := RunEval(ctx, corpus, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.Records) != len(corpus.Questions) {
		t.Fatalf("%d records for %d questions", len(ev.Records), len(corpus.Questions))
	}

	// The embedder took part: it embedded the chunks (through the vector
	// pipeline) and the questions, every query was served hybrid and none fell
	// back to full-text only.
	if emb.calls.Load() == 0 || ev.Ingest.Embedded == 0 || ev.Vectors == 0 {
		t.Errorf("the fake embedder did not take part: calls %d, embedded %d, vectors %d", emb.calls.Load(), ev.Ingest.Embedded, ev.Vectors)
	}
	if ev.ModeCounts[string(query.ModeHybrid)] != len(corpus.Questions) || len(ev.DegradedCount) != 0 {
		t.Errorf("served %v, degraded %v: every question should be hybrid and undegraded", ev.ModeCounts, ev.DegradedCount)
	}
	if ev.Ingest.Docs < corpus.Documents() || ev.Ingest.Children == 0 || ev.Builds != len(corpus.Manifest.Sources)+2 {
		t.Errorf("ingestion built %d documents / %d chunks in %d generations; the corpus has %d sources, two of them in two versions",
			ev.Ingest.Docs, ev.Ingest.Children, ev.Builds, len(corpus.Manifest.Sources))
	}
	if ev.Store.Used <= 0 || ev.WarmupFirstMS == nil {
		t.Errorf("store size %+v, warm-up %v", ev.Store, ev.WarmupFirstMS)
	}

	recs := byID(ev.Records)
	m := computeMetrics(ev.Records, cfg.K)

	// Nothing leaked, anywhere, and no question failed.
	if m.Leaks != 0 || m.Errors != 0 {
		t.Errorf("leaks %d, errors %d: both must be 0", m.Leaks, m.Errors)
	}
	// The questions that must be declined because a source is deleted, denied or
	// another tenant's are all declined, with nothing of the source anywhere.
	for _, kind := range []string{KindDeleted, KindPermission, KindCrossTenant} {
		for _, r := range ev.Records {
			if r.Kind != kind || r.Outcome != OutcomeDecline {
				continue
			}
			if !r.Declined || r.Leak || !r.Correct {
				t.Errorf("%s (%s): declined %v, leak %v, correct %v", r.ID, kind, r.Declined, r.Leak, r.Correct)
			}
		}
	}
	// The principal who may read the restricted source gets the answer the other did not.
	if r := recs["en-perm-03"]; r.Declined || !r.Correct {
		t.Errorf("en-perm-03: the permitted principal got declined=%v correct=%v", r.Declined, r.Correct)
	}
	// Exact identifiers and tables are found: every labelled entry is in the
	// package, in the first positions.
	for _, r := range ev.Records {
		if (r.Kind == KindIdentifier || r.Kind == KindTable) && r.Outcome == OutcomeAnswer {
			if novel := novelHits(r.Ranked); recallAt(novel, r.Expected, 3) != 1 {
				t.Errorf("%s (%s): the labelled evidence is not in the first three items: %v", r.ID, r.Kind, r.Ranked)
			}
		}
	}
	// Conflicting versions: only the current version is searchable and cited,
	// and the superseded value is nowhere.
	for _, id := range []string{"en-conf-01", "en-conf-02", "en-conf-03", "it-conf-01", "it-conf-02"} {
		r := recs[id]
		if r.ForbiddenHits != 0 || r.LabelOK == nil || !*r.LabelOK || !r.Correct {
			t.Errorf("%s: forbidden hits %d, label %v, correct %v", id, r.ForbiddenHits, r.LabelOK, r.Correct)
		}
		for _, c := range r.Citations {
			if c.Version != "v2" {
				t.Errorf("%s cites version %s of %s, want v2", id, c.Version, c.Source)
			}
		}
	}
	// Unavailable and stale sources are answered from, and cited as what they are.
	for id, check := range map[string]func(CitationRecord) bool{
		"en-unav-01":  func(c CitationRecord) bool { return c.Availability == "unavailable" },
		"en-stale-01": func(c CitationRecord) bool { return c.Staleness == "stale" },
	} {
		r := recs[id]
		if r.Declined || len(r.Citations) == 0 || !r.Correct {
			t.Errorf("%s: declined %v, citations %d, correct %v", id, r.Declined, len(r.Citations), r.Correct)
		}
		for _, c := range r.Citations {
			if !check(c) {
				t.Errorf("%s: citation %+v does not carry the state of its source", id, c)
			}
		}
	}
	// The rates the report is built from exist.
	if m.Retrieval.Questions == 0 || m.Retrieval.MRR == nil || *m.Retrieval.MRR < 0.5 {
		t.Errorf("retrieval metrics %+v", m.Retrieval)
	}

	// Reproducible: a second run gives the same metrics (latency aside).
	ev2, err := RunEval(ctx, corpus, testEvalConfig(t, ModeHybrid), nil)
	if err != nil {
		t.Fatal(err)
	}
	m2 := computeMetrics(ev2.Records, cfg.K)
	b1, _ := json.Marshal(m)
	b2, _ := json.Marshal(m2)
	if string(b1) != string(b2) {
		t.Errorf("two runs differ:\n%s\n%s", b1, b2)
	}

	// The full-text-only pipeline is the same tool with the vector stage off:
	// every query says so.
	fts, err := RunEval(ctx, corpus, testEvalConfig(t, ModeFTS), nil)
	if err != nil {
		t.Fatal(err)
	}
	if fts.ModeCounts[string(query.ModeFTSOnly)] != len(corpus.Questions) || fts.DegradedCount[query.DegradedNoEmbedder] != len(corpus.Questions) || fts.Vectors != 0 {
		t.Errorf("fts run: served %v, degraded %v, vectors %d", fts.ModeCounts, fts.DegradedCount, fts.Vectors)
	}
	if got := computeMetrics(fts.Records, cfg.K); got.Leaks != 0 || got.Errors != 0 {
		t.Errorf("fts run: leaks %d, errors %d", got.Leaks, got.Errors)
	}

	// The scale sweep, in process and tiny: the shape is exact and every
	// statistic is re-derivable from its samples.
	scale, err := RunScale(ctx, ScaleConfig{
		Sizes: []int{40, 120}, SmallScopes: 2, Queries: 4, Pooled: 6, Mode: ModeHybrid, Seed: 3,
		Query: query.DefaultConfig(), InProcess: true, TempDir: t.TempDir(),
		Embedder: embedderSpec{Kind: "fake", Model: fakeModel, Dim: fakeDim},
	})
	if err != nil {
		t.Fatal(err)
	}
	if scale.Shape.Scopes != 4 || scale.Shape.CorpusChunks != 40+120+40+40 || scale.Build.Chunks != 240 || scale.Build.Vectors != 240 {
		t.Errorf("shape %+v build %+v", scale.Shape, scale.Build)
	}
	if len(scale.Buckets) != 2 || *scale.Buckets[0].ScopeChunks != 40 || *scale.Buckets[1].ScopeChunks != 120 {
		t.Fatalf("buckets %+v", scale.Buckets)
	}
	if scale.Buckets[0].Scopes != 3 || scale.Buckets[1].Scopes != 1 || scale.Pooled.Scopes != 4 || scale.Pooled.ScopeChunks != nil {
		t.Errorf("scopes per row: %d, %d, pooled %d", scale.Buckets[0].Scopes, scale.Buckets[1].Scopes, scale.Pooled.Scopes)
	}
	for _, b := range append(append([]ScaleQuery{}, scale.Buckets...), scale.Pooled) {
		if b.LatencyMS.N == 0 || b.Errors != 0 || len(b.Degraded) != 0 || b.AvgItems == 0 {
			t.Errorf("row %s: n %d, errors %d, degraded %v, avg items %v", b.Name, b.LatencyMS.N, b.Errors, b.Degraded, b.AvgItems)
		}
	}
	if ps := verifyScale(scale); len(ps) != 0 {
		t.Errorf("scale does not verify: %v", ps)
	}

	// Both result files are written and pass their own checks.
	env := testRunEnv(t, ModeHybrid, cfg.Gen.(*extractiveGenerator))
	res, companion, err := buildResults(resultInput{
		env: env, corpus: corpus, eval: ev, scale: scale, mem: RAGMemory{Source: "os-process", Covers: memoryCovers},
		runID: "run_01M406HNDCV7GSJY7W9DS7PDZ4", started: testTime(), duration: testDuration(),
		git: GitInfo{}, host: CollectHost(),
	})
	if err != nil {
		t.Fatal(err)
	}
	benchSchema, err := LoadBenchSchema(benchSchemaForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	files, err := encodeResults(res, companion, benchSchema)
	if err != nil {
		t.Fatal(err)
	}
	if problems := checkRAGBytes(files.rag, files.companion, benchSchema); len(problems) != 0 {
		t.Errorf("the result does not pass its own check: %v", problems)
	}
	if !res.Emulated || !res.Synthetic || res.Provider.Name != "fake" || res.Profile != "lite" || res.Isolation.CgroupLimitsApplied {
		t.Errorf("labels: emulated %v synthetic %v provider %s profile %s isolation %+v", res.Emulated, res.Synthetic, res.Provider.Name, res.Profile, res.Isolation)
	}
	if companion.Corpus.CasesTotal != res.Corpus.ToDecline || companion.Metrics.RefusalCorrectness.Total != res.Corpus.ToDecline {
		t.Errorf("the companion holds %d cases, the corpus has %d questions to decline", companion.Corpus.CasesTotal, res.Corpus.ToDecline)
	}
}

// TestJudgeDetectsAnAuthorisationLeak is the negative control of the judge: it
// reads its ground truth from the manifest, so a source catalog that grants
// what it should not (a principal reading a source restricted to another) shows
// up as a leak and as a wrong answer, while the honest catalog does not.
func TestJudgeDetectsAnAuthorisationLeak(t *testing.T) {
	corpus := loadTestCorpus(t)
	honest, err := RunEval(context.Background(), corpus, testEvalConfig(t, ModeHybrid), nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := testEvalConfig(t, ModeHybrid)
	cfg.wrapCatalog = func(inner query.SourceCatalog) query.SourceCatalog { return leakyCatalog{inner: inner} }
	leaky, err := RunEval(context.Background(), corpus, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := byID(honest.Records)["en-perm-01"]; r.Leak || !r.Correct {
		t.Fatalf("the honest catalog leaked: %+v", r)
	}
	r := byID(leaky.Records)["en-perm-01"]
	if !r.Leak || r.ForbiddenSourceHits == 0 || r.Correct {
		t.Errorf("a catalog that grants bob the restricted source must be seen as a leak: leak %v, forbidden source hits %d, correct %v", r.Leak, r.ForbiddenSourceHits, r.Correct)
	}
	if m := computeMetrics(leaky.Records, cfg.K); m.Leaks == 0 {
		t.Error("the leaky run reports no leak")
	}
}

// leakyCatalog answers every query as the first principal of the tenant, which
// is the one source readers are listed for.
type leakyCatalog struct{ inner query.SourceCatalog }

func (l leakyCatalog) Sources(ctx context.Context, scope contracts.Scope, ids []string) (map[string]query.SourceState, error) {
	if scope.TenantID == "tn_acme" {
		scope.PrincipalID = "usr_alice"
	}
	return l.inner.Sources(ctx, scope, ids)
}

// TestEnvelopePassesBenchCheck runs the real `go run ./tools/bench check` over
// the companion of a real run, so that drift between this tool's copy of the
// bench envelope (its types, its aggregation, its rounding) and tools/bench's is
// caught here and not by the next person who runs both. It needs the Go
// toolchain and builds tools/bench, so -short skips it.
func TestEnvelopePassesBenchCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("builds tools/bench")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	root, err := FindRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	var stdout, stderr strings.Builder
	if code := run([]string{"run", "--embedder", "fake", "--corpus", corpusDir, "--out", out, "--scale-sizes", "none", "--warmup", "0"}, &stdout, &stderr); code != 0 {
		t.Fatalf("rageval run exited %d: %s", code, stderr.String())
	}
	cmd := exec.Command(goBin, "run", "./tools/bench", "check", out)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bench check refused the companion: %v\n%s", err, b)
	}
	if !strings.Contains(string(b), "ok - 1 result file") {
		t.Errorf("bench check output: %s", b)
	}
	// And the two files are what the directory holds.
	rags, _ := filepath.Glob(filepath.Join(out, "*"+RAGExt))
	jsons, _ := filepath.Glob(filepath.Join(out, "*.json"))
	if len(rags) != 1 || len(jsons) != 1 {
		t.Errorf("the run wrote %d .rag and %d .json files, want one of each", len(rags), len(jsons))
	}
}

func testTime() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) }

func testDuration() time.Duration { return 1500 * time.Millisecond }

// TestScaleInChildProcesses runs the sweep the way the command does, each phase
// in its own process (the test binary serves as the child, see TestMain), and
// checks what only isolation gives: a peak memory per phase.
func TestScaleInChildProcesses(t *testing.T) {
	scale, err := RunScale(context.Background(), ScaleConfig{
		Sizes: []int{30, 90}, SmallScopes: 1, Queries: 3, Pooled: 4, Mode: ModeFTS, Seed: 5,
		Query: query.DefaultConfig(), TempDir: t.TempDir(), Embedder: embedderSpec{Kind: "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !scale.Isolated || scale.Build.Chunks != 30+90+30 || scale.Build.Vectors != 0 {
		t.Errorf("isolated %v, build %+v", scale.Isolated, scale.Build)
	}
	if ps := verifyScale(scale); len(ps) != 0 {
		t.Errorf("scale does not verify: %v", ps)
	}
	if _, _, ok := processMemory(); ok {
		if !scale.Build.RSSMeasured || scale.Build.PeakRSSMB == nil || *scale.Build.PeakRSSMB <= 0 {
			t.Errorf("build memory: %+v", scale.Build)
		}
		for _, b := range append(append([]ScaleQuery{}, scale.Buckets...), scale.Pooled) {
			if !b.RSSMeasured || b.PeakRSSMB == nil || *b.PeakRSSMB <= 0 || *b.RSSAfterMB > *b.PeakRSSMB {
				t.Errorf("row %s memory: start %v after %v peak %v", b.Name, b.RSSStartMB, b.RSSAfterMB, b.PeakRSSMB)
			}
		}
	}
}

func TestScenarioRefusesABadSpec(t *testing.T) {
	var out, errb strings.Builder
	for _, tc := range []struct {
		args  []string
		stdin string
	}{
		{nil, ""},
		{[]string{"build"}, "not json"},
		{[]string{"build"}, `{"oops":1}`},
		{[]string{"nothing"}, `{}`},
	} {
		out.Reset()
		errb.Reset()
		if code := scenarioMain(tc.args, strings.NewReader(tc.stdin), &out, &errb); code == 0 {
			t.Errorf("scenario %v with %q exited 0", tc.args, tc.stdin)
		}
	}
}

// TestScalePinAndReaderCache: the two knobs of the reader-cache experiment. A
// pinned bucket asks the first N scopes of its size only (the corpus-wide row
// still rotates over every scope), the cache is what the result says it was, and
// the checker knows what a pinned bucket should hold.
func TestScalePinAndReaderCache(t *testing.T) {
	cfg := ScaleConfig{
		Sizes: []int{30}, SmallScopes: 3, Queries: 3, Pooled: 4, Mode: ModeHybrid, Seed: 7, Query: query.DefaultConfig(),
		InProcess: true, TempDir: t.TempDir(), Embedder: embedderSpec{Kind: "fake", Model: fakeModel, Dim: fakeDim},
		PinScopes: 1, ReaderCacheKiB: 64,
	}
	scale, err := RunScale(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if scale.PinScopes != 1 || scale.ReaderCacheKiB != 64 || scale.Embedder != "fake" {
		t.Errorf("pin %d cache %d embedder %s", scale.PinScopes, scale.ReaderCacheKiB, scale.Embedder)
	}
	if scale.Shape.Scopes != 4 || scale.Buckets[0].Scopes != 1 || scale.Pooled.Scopes != 4 {
		t.Errorf("scopes asked: bucket %d, corpus-wide %d of %d", scale.Buckets[0].Scopes, scale.Pooled.Scopes, scale.Shape.Scopes)
	}
	if ps := verifyScale(scale); len(ps) != 0 {
		t.Errorf("a pinned sweep does not verify: %v", ps)
	}
	scale.PinScopes = 0
	if ps := verifyScale(scale); len(ps) == 0 || !strings.Contains(ps[0], "expected 4") {
		t.Errorf("an unpinned sweep whose bucket asked one scope of four verified: %v", ps)
	}
	if effectiveReaderCacheKiB(0) != 2000 || effectiveReaderCacheKiB(64) != 64 {
		t.Error("the default reader cache is the Lite configuration's 2000 KiB")
	}

	for name, mut := range map[string]func(*ScaleConfig){
		"a negative pin":              func(c *ScaleConfig) { c.PinScopes = -1 },
		"a reader cache over the max": func(c *ScaleConfig) { c.ReaderCacheKiB = 1<<18 + 1 },
		"no embedder":                 func(c *ScaleConfig) { c.Embedder = embedderSpec{} },
		"the fake in a full-text one": func(c *ScaleConfig) { c.Mode = ModeFTS },
		"an embedder with no dim":     func(c *ScaleConfig) { c.Embedder.Dim = 0 },
	} {
		c := cfg
		mut(&c)
		if _, err := RunScale(context.Background(), c); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestStageTimingsAreRecorded: every question carries the pipeline's own timing
// per stage, the result derives p50/p95 per stage from them, and a stage that did
// not run (no reranker here) is 0, not missing.
func TestStageTimingsAreRecorded(t *testing.T) {
	ev, err := RunEval(context.Background(), loadTestCorpus(t), testEvalConfig(t, ModeHybrid), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ev.Records {
		if len(r.StagesMS) != len(stageNames) {
			t.Fatalf("question %s: %d stages, want %d: %v", r.ID, len(r.StagesMS), len(stageNames), r.StagesMS)
		}
		if r.StagesMS["rerank"] != 0 {
			t.Errorf("question %s: a rerank stage of %v ms with reranking off", r.ID, r.StagesMS["rerank"])
		}
	}
	l := deriveLatency(ev.Records, ev.WarmupFirstMS)
	for _, name := range stageNames {
		if l.Stages[name].N != len(ev.Records) {
			t.Errorf("stage %s over %d of %d questions", name, l.Stages[name].N, len(ev.Records))
		}
	}
	if l.Stages["fts"].P50 == nil || *l.Stages["fts"].P50 <= 0 {
		t.Errorf("the full-text stage has no p50: %+v", l.Stages["fts"])
	}
}
