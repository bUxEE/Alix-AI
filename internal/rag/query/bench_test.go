package query

// The measurement behind docs/efficiency/M2-09.md: FTS-only against hybrid
// against hybrid plus rerank, per scope size, p50 and p95 latency and RSS.
//
// It does nothing in a normal `go test`. To reproduce the figures:
//
//	ALIXAI_QUERY_BENCH=1000,10000,50000 go test -run TestBenchQueryPipeline -v -timeout 90m ./internal/rag/query/
//
// Knobs: ALIXAI_QUERY_BENCH_SET=terms (only the term-cap comparison), ALIXAI_QUERY_BENCH_QUERIES (timed queries per kind per scenario, default
// 100) and ALIXAI_QUERY_BENCH_OUT (append JSON lines to this file).
//
// Each scope size is built once into a database file by a child process, then
// every scenario runs in its own child process (this test binary, re-executed)
// over that file, so the RSS figures are that scenario's own,
// read from the OS, and the build's memory is not in them. The corpus is the one
// of internal/rag/index's benchmark: 30 words per chunk drawn from a 3000-word
// Zipf vocabulary, which is the worst case for BM25 (a few terms occur in most
// chunks), with eight chunks to a parent section.
//
// The embedder and the reranker are the deterministic fake provider: they cost
// microseconds. What is measured is the pipeline (the index, the vector scan,
// fusion, parent reads, the final recheck) and its overhead around the models,
// never a model. A model's own latency and RSS are owed to M2-15.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

const (
	benchDim       = 384 // Granite Embedding 97M
	benchModel     = "bench-embed-384"
	benchPerParent = 8
	benchDocChunks = 400
)

type benchScenario struct {
	Name     string `json:"name"`
	DB       string `json:"db"`
	Size     int    `json:"size"`
	Mode     string `json:"mode"` // fts, hybrid, rerank
	Terms    int    `json:"terms"`
	Readers  int    `json:"readers"`
	CacheMiB int    `json:"cache_mib"`
	Queries  int    `json:"queries"`
}

type benchStats struct {
	N        int     `json:"n"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	MaxMs    float64 `json:"max_ms"`
	MeanMs   float64 `json:"mean_ms"`
	Timeouts int     `json:"fts_timeouts"`
	Errors   int     `json:"errors"`
	AvgItems float64 `json:"avg_items"`
}

type benchResult struct {
	Name        string             `json:"name"`
	Scenario    benchScenario      `json:"scenario"`
	Short       benchStats         `json:"short"`
	Long        benchStats         `json:"long"`
	FirstMs     float64            `json:"first_query_ms"`
	StageMeanMs map[string]float64 `json:"stage_mean_ms"`
	BaselineMiB float64            `json:"rss_baseline_mib"`
	AfterMiB    float64            `json:"rss_after_mib"`
	PeakMiB     float64            `json:"rss_peak_mib"`
	RSSMeasured bool               `json:"rss_measured"`
	DBMiB       float64            `json:"db_mib"`
}

func benchMs(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
func benchMiB(b uint64) float64       { return float64(b) / (1 << 20) }

func benchScope() contracts.Scope { return storetest.Scope("app", "bench", "w1") }

func benchOpen(t testing.TB, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{Path: path})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func benchPseudoWord(i int) string {
	const cons, vow = "bdfgklmnprstvz", "aeiou"
	var b strings.Builder
	for k := 0; k < 3; k++ {
		s := i % 70
		i /= 70
		b.WriteByte(cons[s%14])
		b.WriteByte(vow[s/14])
	}
	return b.String()
}

func benchVocab(seed1, seed2 uint64) ([]string, *rand.Zipf) {
	rng := rand.New(rand.NewPCG(seed1, seed2))
	vocab := make([]string, 3000)
	for i := range vocab {
		vocab[i] = benchPseudoWord(i)
	}
	return vocab, rand.NewZipf(rng, 1.2, 4, uint64(len(vocab)-1))
}

// benchDocument builds n children under parent sections of benchPerParent.
func benchDocument(zipf *rand.Zipf, vocab []string, doc string, n int) chunk.Result {
	var res chunk.Result
	words := func(k int) string {
		var b strings.Builder
		for i := 0; i < k; i++ {
			b.WriteString(vocab[zipf.Uint64()])
			b.WriteByte(' ')
		}
		return strings.TrimSpace(b.String())
	}
	off := 0
	for p := 0; p*benchPerParent < n; p++ {
		pid := fmt.Sprintf("chp_%s_%04d", doc, p)
		section := "Section " + vocab[zipf.Uint64()]
		start := off
		var texts, kids []string
		for j := 0; j < benchPerParent && p*benchPerParent+j < n; j++ {
			i := p*benchPerParent + j
			text := words(30)
			if i%500 == 0 {
				text += fmt.Sprintf(" PN-%s-%d", doc, i)
			}
			n := len([]rune(text))
			res.Children = append(res.Children, chunk.Chunk{
				ID: fmt.Sprintf("chk_%s_%05d", doc, i), ParentID: pid, Kind: chunk.KindParagraph, Ordinal: j,
				Text: text, Section: section, Path: []string{section}, Span: chunk.Span{Start: off, End: off + n},
				ContentHash: fmt.Sprintf("%064d", i), EstToks: chunk.EstimateTokens(text),
			})
			texts = append(texts, text)
			kids = append(kids, res.Children[len(res.Children)-1].ID)
			off += n + 2
		}
		ptext := strings.Join(texts, "\n\n")
		res.Parents = append(res.Parents, chunk.Chunk{
			ID: pid, Kind: chunk.KindSection, Ordinal: p, Text: ptext, Section: section, Path: []string{section},
			Span: chunk.Span{Start: start, End: off - 2}, EstToks: chunk.EstimateTokens(ptext), ChildIDs: kids,
		})
	}
	return res
}

// benchHits is the chunk read the pipeline needs from the index and the index
// does not export yet (see HitReader): one indexed point read per chunk under
// the visibility rules of a search (scope, serving generation, not tombstoned,
// not hidden). It is the statement a real reader should run, so the hydrate stage
// measured here is the cost to expect once the index has one. INDEXED BY is
// deliberate: without it SQLite picks the scope index and scans every row of the
// scope for each lookup (see the efficiency review, finding 1).
type benchHits struct {
	db *store.DB
}

func (b *benchHits) Hits(ctx context.Context, scope contracts.Scope, refs []index.Ref, includeHidden bool) ([]index.Hit, error) {
	var out []index.Hit
	err := b.db.View(ctx, func(tx *store.Tx) error {
		for _, r := range refs {
			h := index.Hit{ChunkID: r.ChunkID, GenerationID: r.GenerationID}
			err := tx.QueryRowContext(ctx, `SELECT c.source_id, c.document_id, d.version, c.parent_id, c.ordinal, c.span_start, c.span_end,
c.est_toks, c.content_hash, c.section, c.header, c.text
FROM ragindex_chunk AS c INDEXED BY ragindex_chunk_live
JOIN ragindex_generation AS g ON g.generation_id = c.generation_id
JOIN ragindex_document AS d ON d.generation_id = c.generation_id AND d.document_id = c.document_id AND d.tombstoned_at IS NULL
WHERE c.generation_id = ? AND c.chunk_id = ? AND c.tombstoned_at IS NULL AND c.hidden = 0
  AND c.application_id = ? AND c.tenant_id = ? AND c.workspace_id = ?
  AND g.application_id = ? AND g.tenant_id = ? AND g.workspace_id = ? AND g.state = 'active'
LIMIT 1`,
				r.GenerationID, r.ChunkID,
				scope.ApplicationID, scope.TenantID, scope.WorkspaceID,
				scope.ApplicationID, scope.TenantID, scope.WorkspaceID).Scan(
				&h.SourceID, &h.DocumentID, &h.DocumentVersion, &h.ParentID, &h.Ordinal, &h.Span.Start, &h.Span.End,
				&h.EstToks, &h.ContentHash, &h.Section, &h.Header, &h.Text)
			if err != nil {
				continue // not visible, or not there: omitted without a reason, as the contract says
			}
			h.Kind = chunk.KindParagraph
			out = append(out, h)
		}
		return nil
	})
	return out, err
}

type benchCatalog struct{}

func (benchCatalog) Sources(_ context.Context, _ contracts.Scope, sourceIDs []string) (map[string]SourceState, error) {
	out := make(map[string]SourceState, len(sourceIDs))
	for _, id := range sourceIDs {
		out[id] = SourceState{Authorised: true, Availability: conversation.CitationAvailabilityAvailable, Staleness: conversation.CitationStalenessCurrent}
	}
	return out, nil
}

type benchChildSpec struct {
	Role     string        `json:"role"` // build or run
	DB       string        `json:"db"`
	Size     int           `json:"size"`
	Scenario benchScenario `json:"scenario"`
}

// TestBenchChild is the child process of TestBenchQueryPipeline; it does nothing
// on its own.
func TestBenchChild(t *testing.T) {
	raw := os.Getenv("ALIXAI_QUERY_BENCH_CHILD")
	if raw == "" {
		t.Skip("child of TestBenchQueryPipeline")
	}
	var spec benchChildSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		t.Fatal(err)
	}
	switch spec.Role {
	case "build":
		benchBuild(t, spec.DB, spec.Size)
	case "run":
		res := benchRun(t, spec.Scenario)
		b, _ := json.Marshal(res)
		fmt.Printf("BENCHRESULT %s\n", b)
	default:
		t.Fatalf("role %q", spec.Role)
	}
}

func benchBuild(t *testing.T, path string, size int) {
	ctx := context.Background()
	db := benchOpen(t, path)
	ix, err := index.New(db, nil, index.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := vector.DefaultConfig()
	cfg.CacheMiB = 0
	flat, err := vector.NewFlat(db, vector.FlatOptions{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	scope := benchScope()
	src := ids.New("src")
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	g, err := ix.BeginGeneration(ctx, scope, src, index.Components{
		Parser:   rag.ComponentVersion{Name: "alixai-parser", Version: "1.0.0"},
		Chunker:  chk.Component(),
		Embedder: &rag.ComponentVersion{Name: "granite-embedding", Version: "97m"},
	})
	if err != nil {
		t.Fatal(err)
	}
	genID := string(g.GenerationID)
	vocab, zipf := benchVocab(1, 2)
	start := time.Now()
	for d, left := 0, size; left > 0; d++ {
		n := min(left, benchDocChunks)
		docID := fmt.Sprintf("doc-%04d", d)
		res := benchDocument(zipf, vocab, fmt.Sprintf("%04d", d), n)
		if _, err := ix.PutDocument(ctx, scope, genID, index.Document{ID: docID, Version: "v1", Chunks: res}); err != nil {
			t.Fatal(err)
		}
		items := make([]vector.Item, len(res.Children))
		for i, c := range res.Children {
			key, err := vector.NewInputKey(scope, benchModel, c.Text)
			if err != nil {
				t.Fatal(err)
			}
			items[i] = vector.Item{ChunkID: c.ID, Key: key, Vector: fake.EmbedVector(c.Text, benchDim)}
		}
		if _, err := flat.Upsert(ctx, scope, vector.UpsertBatch{Generation: genID, SourceID: src, Model: benchModel, Items: items}); err != nil {
			t.Fatal(err)
		}
		left -= n
	}
	if _, err := ix.ValidateGeneration(ctx, scope, genID, index.Expectation{}); err != nil {
		t.Fatal(err)
	}
	if err := ix.ActivateGeneration(ctx, scope, genID); err != nil {
		t.Fatal(err)
	}
	t.Logf("built %d chunks in %.1fs", size, time.Since(start).Seconds())
}

// benchQuestions are the seeded questions of one kind.
func benchQuestions(kind string, n int) []string {
	vocab, zipf := benchVocab(7, uint64(len(kind)))
	terms := 3
	if kind == "long" {
		terms = 12
	}
	out := make([]string, n)
	for i := range out {
		seen := map[string]bool{}
		var parts []string
		for len(parts) < terms {
			w := vocab[zipf.Uint64()]
			if !seen[w] {
				seen[w] = true
				parts = append(parts, w)
			}
		}
		out[i] = strings.Join(parts, " ")
	}
	return out
}

func benchRun(t *testing.T, sc benchScenario) benchResult {
	ctx := context.Background()
	db := benchOpen(t, sc.DB)
	ix, err := index.New(db, nil, index.Config{})
	if err != nil {
		t.Fatal(err)
	}
	vcfg := vector.DefaultConfig()
	vcfg.CacheMiB = sc.CacheMiB
	flat, err := vector.NewFlat(db, vector.FlatOptions{Config: vcfg})
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{Lexical: ix, Catalog: benchCatalog{}, Hits: &benchHits{db: db}, EmbeddingModel: benchModel}
	cfg := DefaultConfig()
	cfg.MaxQueryTerms = sc.Terms
	if sc.Mode != "fts" {
		reg := providers.NewRegistry(clock.Real{})
		if err := reg.Register(providers.RoleEmbedder, fake.New(fake.WithModel(benchModel), fake.WithEmbedDim(benchDim))); err != nil {
			t.Fatal(err)
		}
		if sc.Mode == "rerank" {
			if err := reg.Register(providers.RoleReranker, fake.New(fake.WithModel("bench-rerank"))); err != nil {
				t.Fatal(err)
			}
			cfg.Rerank = true
		}
		deps.Providers, deps.Vectors = reg, flat
	}
	p, err := New(deps, cfg)
	if err != nil {
		t.Fatal(err)
	}
	scope := benchScope()

	res := benchResult{Name: sc.Name, Scenario: sc, StageMeanMs: map[string]float64{}}
	if st, err := os.Stat(sc.DB); err == nil {
		res.DBMiB = float64(st.Size()) / (1 << 20)
	}
	rss0, _, ok := benchProcessMemory()
	res.RSSMeasured, res.BaselineMiB = ok, benchMiB(rss0)

	short, long := benchQuestions("short", sc.Queries+10), benchQuestions("long", sc.Queries+10)
	// The very first query pays for opening the readers and, in hybrid mode, for
	// loading the vector generation into the resident cache.
	t0 := time.Now()
	if _, err := p.Query(ctx, scope, Request{Text: short[0]}); err != nil {
		t.Fatalf("first query: %v", err)
	}
	res.FirstMs = benchMs(time.Since(t0))
	for _, q := range short[1:10] {
		if _, err := p.Query(ctx, scope, Request{Text: q}); err != nil {
			t.Fatalf("warm-up: %v", err)
		}
	}

	stageSum := map[string]time.Duration{}
	var stageN int
	var mu sync.Mutex
	run := func(questions []string) benchStats {
		lat := make([]time.Duration, 0, len(questions))
		var st benchStats
		var items int
		ch := make(chan string)
		var wg sync.WaitGroup
		for w := 0; w < sc.Readers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for q := range ch {
					t0 := time.Now()
					r, err := p.Query(ctx, scope, Request{Text: q})
					d := time.Since(t0)
					mu.Lock()
					lat = append(lat, d)
					switch {
					case err == nil:
						items += len(r.Package.Items)
						dg := r.Diagnostics
						for k, v := range map[string]time.Duration{"fts": dg.FTS, "embed": dg.Embed, "vector": dg.Vector, "hydrate": dg.Hydrate,
							"expand": dg.Expand, "rerank": dg.Rerank, "recheck": dg.Recheck, "total": dg.Total} {
							stageSum[k] += v
						}
						stageN++
					case contracts.HasCode(err, contracts.ErrTimeout):
						st.Timeouts++
					default:
						st.Errors++
						t.Logf("query error: %v", err)
					}
					mu.Unlock()
				}
			}()
		}
		for _, q := range questions {
			ch <- q
		}
		close(ch)
		wg.Wait()
		sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })
		var sum time.Duration
		for _, d := range lat {
			sum += d
		}
		st.N, st.P50Ms, st.P95Ms, st.MaxMs = len(lat), benchMs(lat[len(lat)/2]), benchMs(lat[len(lat)*95/100]), benchMs(lat[len(lat)-1])
		st.MeanMs = benchMs(sum) / float64(len(lat))
		if ok := len(lat) - st.Timeouts - st.Errors; ok > 0 {
			st.AvgItems = float64(items) / float64(ok)
		}
		return st
	}
	res.Short = run(short[10:])
	res.Long = run(long[10:])
	if stageN > 0 {
		for k, v := range stageSum {
			res.StageMeanMs[k] = benchMs(v) / float64(stageN)
		}
	}
	rss1, peak, ok := benchProcessMemory()
	res.RSSMeasured = res.RSSMeasured && ok
	res.AfterMiB, res.PeakMiB = benchMiB(rss1), benchMiB(peak)
	return res
}

func benchChild(t *testing.T, exe string, spec benchChildSpec) (string, error) {
	raw, _ := json.Marshal(spec)
	cmd := exec.Command(exe, "-test.run", "^TestBenchChild$", "-test.v", "-test.timeout=60m")
	cmd.Env = append(os.Environ(), "ALIXAI_QUERY_BENCH_CHILD="+string(raw))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestBenchQueryPipeline(t *testing.T) {
	env := os.Getenv("ALIXAI_QUERY_BENCH")
	if env == "" {
		t.Skip("set ALIXAI_QUERY_BENCH=1000,10000,50000 to run the pipeline measurement (several minutes)")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var sizes []int
	for _, s := range strings.Split(env, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			sizes = append(sizes, n)
		}
	}
	if len(sizes) == 0 {
		sizes = []int{1000, 10000, 50000}
	}
	queries := 100
	if v, err := strconv.Atoi(os.Getenv("ALIXAI_QUERY_BENCH_QUERIES")); err == nil && v > 1 {
		queries = v
	}
	for _, size := range sizes {
		dbPath := filepath.Join(t.TempDir(), "bench.db")
		if out, err := benchChild(t, exe, benchChildSpec{Role: "build", DB: dbPath, Size: size}); err != nil {
			t.Fatalf("build %d: %v\n%s", size, err, out)
		}
		mk := func(mode string, terms, readers, cache int) benchScenario {
			return benchScenario{
				Name: fmt.Sprintf("%dk %s terms=%d readers=%d cache=%d", size/1000, mode, terms, readers, cache),
				DB:   dbPath, Size: size, Mode: mode, Terms: terms, Readers: readers, CacheMiB: cache, Queries: queries,
			}
		}
		scenarios := []benchScenario{
			mk("fts", 8, 1, 0), mk("hybrid", 8, 1, 64), mk("rerank", 8, 1, 64),
			mk("fts", 16, 1, 0),
			mk("fts", 8, 4, 0), mk("hybrid", 8, 4, 64),
		}
		if size >= 50000 {
			scenarios = append(scenarios, mk("hybrid", 8, 1, 0)) // the vector cache off: the 4 GB profile's setting
		}
		if os.Getenv("ALIXAI_QUERY_BENCH_SET") == "terms" {
			// The term cap alone: full-text only, one reader, the caps interleaved twice so
			// that drift on a shared machine does not favour one of them.
			scenarios = nil
			for round := 0; round < 2; round++ {
				for _, terms := range []int{4, 8, 16} {
					scenarios = append(scenarios, mk("fts", terms, 1, 0))
				}
			}
		}
		for _, sc := range scenarios {
			out, err := benchChild(t, exe, benchChildSpec{Role: "run", DB: dbPath, Size: size, Scenario: sc})
			if err != nil {
				t.Errorf("%s: %v\n%s", sc.Name, err, out)
				continue
			}
			var r benchResult
			found := false
			for _, line := range strings.Split(out, "\n") {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "BENCHRESULT "); ok {
					if err := json.Unmarshal([]byte(rest), &r); err != nil {
						t.Fatalf("%s: %v", sc.Name, err)
					}
					found = true
				}
			}
			if !found {
				t.Errorf("%s: no result:\n%s", sc.Name, out)
				continue
			}
			t.Logf("%-38s short p50 %7.1f p95 %7.1f ms (timeouts %d) | long p50 %7.1f p95 %7.1f ms (timeouts %d) | first %7.1f ms | RSS base %5.1f after %5.1f peak %5.1f MiB | stages %v",
				r.Name, r.Short.P50Ms, r.Short.P95Ms, r.Short.Timeouts, r.Long.P50Ms, r.Long.P95Ms, r.Long.Timeouts, r.FirstMs,
				r.BaselineMiB, r.AfterMiB, r.PeakMiB, benchStageLine(r.StageMeanMs))
			if outPath := os.Getenv("ALIXAI_QUERY_BENCH_OUT"); outPath != "" {
				if f, err := os.OpenFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
					b, _ := json.Marshal(r)
					fmt.Fprintf(f, "%s\n", b)
					f.Close()
				}
			}
		}
	}
}

func benchStageLine(m map[string]float64) string {
	var parts []string
	for _, k := range []string{"fts", "embed", "vector", "hydrate", "expand", "rerank", "recheck", "total"} {
		parts = append(parts, fmt.Sprintf("%s=%.1f", k, m[k]))
	}
	return strings.Join(parts, " ")
}
