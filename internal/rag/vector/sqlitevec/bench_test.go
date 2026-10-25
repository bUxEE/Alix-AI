package sqlitevec_test

// The head-to-head benchmark of ADR 0011: the exact flat scan (option A,
// streamed from SQLite and from the resident cache) against sqlite-vec (option
// B, cosine and L2 int8), on synthetic 384-dimensional vectors (the dimension
// of Granite Embedding 97M) at 10,000 and 100,000 chunks, with a small scope
// next to a large deployment.
//
// It does nothing in a normal `go test`. To reproduce the figures of
// docs/decisions/0011-embedded-vector-index.md and docs/efficiency/M2-07.md:
//
//	ALIXAI_VECTOR_BENCH=1 go test -run TestBenchMatrix -v -timeout 60m ./internal/rag/vector/sqlitevec/
//
// Knobs: ALIXAI_VECTOR_BENCH_ONLY (substring of a scenario name), ALIXAI_VECTOR_BENCH_OUT
// (append JSON lines to this file), ALIXAI_VECTOR_BENCH_QUERIES (timed queries per
// scenario, default 100).
//
// Every scenario runs in a child process (this test binary, re-executed) so the
// RSS figures are that configuration's own, read from the OS, and so the OS peak
// is meaningful. Vectors are regenerated from a seed rather than stored, both
// for ingestion and for the brute-force ground truth, so the harness itself
// holds almost nothing and does not inflate the RSS it measures.

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

	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/rag/vector/sqlitevec"
	"github.com/buxee/alix-ai/internal/rag/vector/vectortest"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/vec"
	"github.com/buxee/alix-ai/pkg/contracts"
)

const (
	benchDim   = vec.Dim
	benchK     = 10
	benchModel = "bench-model"
	// l2Scale maps a unit vector's components (|x| < 0.4 for 384 dimensions) to
	// int8 for the L2 variant, which needs one corpus-wide scale.
	l2Scale = 127 / 0.4
)

type scenario struct {
	Provider    string `json:"provider"` // flat-streamed | flat-resident (default 64 MiB cache) | flat-resident-128 | sqlitevec-cosine | sqlitevec-l2
	Kind        string `json:"kind"`     // clustered | uniform
	Target      int    `json:"target"`   // vectors in the scope searched
	Others      int    `json:"others"`   // vectors in nine other scopes
	Queries     int    `json:"queries"`
	RecallQ     int    `json:"recall_queries"`
	Concurrency int    `json:"concurrency"`
	// HotScopes is how many scopes of Target vectors each are searched in
	// rotation (default 1): two or three hot 100k workspaces against a 64 MiB cache.
	HotScopes int `json:"hot_scopes,omitempty"`
}

func (s scenario) name() string {
	n := fmt.Sprintf("%s/%s/%dk", s.Provider, s.Kind, s.Target/1000)
	if s.Others > 0 {
		n += fmt.Sprintf("+%dk-other", s.Others/1000)
	}
	if s.Concurrency > 1 {
		n += fmt.Sprintf("/x%d", s.Concurrency)
	}
	if s.HotScopes > 1 {
		n += fmt.Sprintf("/%d-hot-scopes", s.HotScopes)
	}
	return n
}

type result struct {
	Scenario scenario `json:"scenario"`
	Name     string   `json:"name"`

	IngestSec    float64 `json:"ingest_sec"`
	IngestPerSec float64 `json:"ingest_per_sec"`
	DeleteMs     float64 `json:"delete_generation_ms"`
	// DeleteStallMs is the longest another writer of the process (a tiny insert
	// every 2 ms) waited while the generation was being deleted: what the delete
	// costs everything else that writes to the store.
	DeleteStallMs  float64 `json:"delete_stall_max_ms"`
	DeleteStallP99 float64 `json:"delete_stall_p99_ms"`

	FirstQueryMs float64 `json:"first_query_ms"`
	MeanMs       float64 `json:"mean_ms"`
	P50Ms        float64 `json:"p50_ms"`
	P95Ms        float64 `json:"p95_ms"`
	P99Ms        float64 `json:"p99_ms"`
	QPS          float64 `json:"qps"`
	Recall10     float64 `json:"recall_at_10"`

	BaselineMiB     float64 `json:"rss_baseline_mib"`
	AfterIngestMiB  float64 `json:"rss_after_ingest_mib"`
	AfterQueriesMiB float64 `json:"rss_after_queries_mib"`
	PeakMiB         float64 `json:"rss_peak_mib"`
	DBMiB           float64 `json:"db_mib"`
	WALMiB          float64 `json:"wal_mib"`
	RSSMeasured     bool    `json:"rss_measured"`
}

// ------------------------------------------------------------ data generation

type generator struct {
	kind    string
	centres [][]float32
	seed    uint64
}

func newGenerator(kind string, n int) *generator {
	g := &generator{kind: kind, seed: 20261002}
	if kind == "clustered" {
		r := vectortest.Rand(g.seed)
		k := max(50, n/400) // about 400 vectors per topic
		for i := 0; i < k; i++ {
			g.centres = append(g.centres, vectortest.RandomUnit(r, benchDim))
		}
	}
	return g
}

// vec returns vector i of stream; the same arguments always give the same vector.
func (g *generator) vec(stream, i uint64) []float32 {
	r := rand.New(rand.NewPCG(g.seed^stream*0x9e3779b97f4a7c15, i+1))
	if g.kind == "uniform" {
		return vectortest.RandomUnit(r, benchDim)
	}
	return vectortest.Perturb(r, g.centres[r.IntN(len(g.centres))], 0.55)
}

func (g *generator) query(i int) []float32 {
	r := rand.New(rand.NewPCG(g.seed+77, uint64(i)+1))
	if g.kind == "uniform" {
		return vectortest.RandomUnit(r, benchDim)
	}
	return vectortest.Perturb(r, g.centres[r.IntN(len(g.centres))], 0.5)
}

func benchScope(tenant int) contracts.Scope {
	return vectortest.Scope("bench", fmt.Sprintf("tenant-%d", tenant), "ws-0")
}

func chunkID(i int) string { return fmt.Sprintf("c%07d", i) }

// ------------------------------------------------------------ providers

// index is what a scenario needs from a provider.
type index struct {
	upsert func(scope contracts.Scope, gen string, ids []string, vecs [][]float32) error
	search func(scope contracts.Scope, gen string, q []float32) ([]string, error)
	delete func(scope contracts.Scope, gen string) error
}

func openIndex(ctx context.Context, name string, dir string) (*index, *store.DB, error) {
	// The flat provider's tables are in the default registry (importing the
	// vector package registers them); the sqlite-vec schema never is.
	var reg *store.Registry
	if !strings.HasPrefix(name, "flat") {
		reg = store.NewRegistry()
		if err := vec.Register(reg); err != nil {
			return nil, nil, err
		}
	}
	db, err := store.Open(ctx, store.Options{Path: filepath.Join(dir, "bench.db"), Migrations: reg})
	if err != nil {
		return nil, nil, err
	}
	toItems := func(scope contracts.Scope, ids []string, vecs [][]float32) []vector.Item {
		items := make([]vector.Item, len(ids))
		for i := range ids {
			k, _ := vector.NewInputKey(scope, benchModel, ids[i])
			items[i] = vector.Item{ChunkID: ids[i], Key: k, Vector: vecs[i]}
		}
		return items
	}
	ceilings := vector.Config{MaxVectorsPerWorkspace: 1 << 22, MaxVectorsPerDeployment: 1 << 23}
	switch name {
	case "flat-streamed", "flat-resident", "flat-resident-128":
		cfg := ceilings
		switch name {
		case "flat-resident":
			cfg.CacheMiB = vector.DefaultCacheMiB // what ships
		case "flat-resident-128":
			cfg.CacheMiB = 128
		}
		f, err := vector.NewFlat(db, vector.FlatOptions{Config: cfg})
		if err != nil {
			return nil, nil, err
		}
		return providerIndex(f, toItems), db, nil
	case "sqlitevec-cosine":
		p, err := sqlitevec.New(ctx, db, sqlitevec.Options{Config: ceilings})
		if err != nil {
			return nil, nil, err
		}
		return providerIndex(p, toItems), db, nil
	case "sqlitevec-l2":
		return l2Index(ctx, db)
	}
	return nil, nil, fmt.Errorf("unknown provider %q", name)
}

func providerIndex(p vector.VectorIndexProvider, toItems func(contracts.Scope, []string, [][]float32) []vector.Item) *index {
	bg := context.Background()
	return &index{
		upsert: func(scope contracts.Scope, gen string, ids []string, vecs [][]float32) error {
			_, err := p.Upsert(bg, scope, vector.UpsertBatch{Generation: gen, SourceID: "src", Model: benchModel, Items: toItems(scope, ids, vecs)})
			return err
		},
		search: func(scope contracts.Scope, gen string, q []float32) ([]string, error) {
			res, err := p.Search(bg, scope, vector.Query{Vector: q, Model: benchModel, K: benchK, Generations: []string{gen}})
			if err != nil {
				return nil, err
			}
			ids := make([]string, len(res.Hits))
			for i, h := range res.Hits {
				ids[i] = h.ChunkID
			}
			return ids, nil
		},
		delete: func(scope contracts.Scope, gen string) error {
			_, err := p.DeleteGeneration(bg, scope, gen)
			return err
		},
	}
}

// l2Index is sqlite-vec with an L2 int8 table, bypassing the provider: it exists
// only to put a number on the cheaper metric ("benchmark B with L2 before calling
// it slower"). L2 ranks like cosine only for equal-length vectors, and needs one
// corpus-wide quantisation scale, so it is not a drop-in; the figures are an upper
// bound on what B could do.
func l2Index(ctx context.Context, db *store.DB) (*index, *store.DB, error) {
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE VIRTUAL TABLE bench_l2 USING vec0(
		application_id text partition key, tenant_id text partition key, workspace_id text partition key, generation_id text partition key,
		embedding int8[%d] distance_metric=l2, +chunk_id text)`, benchDim)); err != nil {
		return nil, nil, err
	}
	q8 := func(v []float32) []byte {
		out := make([]byte, len(v))
		for i, x := range v {
			c := x * l2Scale
			if c > 127 {
				c = 127
			} else if c < -127 {
				c = -127
			}
			out[i] = byte(int8(c))
		}
		return out
	}
	return &index{
		upsert: func(scope contracts.Scope, gen string, ids []string, vecs [][]float32) error {
			return db.Tx(ctx, func(tx *store.Tx) error {
				st, err := tx.PrepareContext(ctx, `INSERT INTO bench_l2 (application_id, tenant_id, workspace_id, generation_id, embedding, chunk_id) VALUES (?, ?, ?, ?, vec_int8(?), ?)`)
				if err != nil {
					return err
				}
				defer st.Close()
				for i := range ids {
					if _, err := st.ExecContext(ctx, scope.ApplicationID, scope.TenantID, scope.WorkspaceID, gen, q8(vecs[i]), ids[i]); err != nil {
						return err
					}
				}
				return nil
			})
		},
		search: func(scope contracts.Scope, gen string, q []float32) ([]string, error) {
			rows, err := db.QueryContext(ctx, `SELECT chunk_id FROM bench_l2 WHERE embedding MATCH vec_int8(?) AND k = ?
				AND application_id = ? AND tenant_id = ? AND workspace_id = ? AND generation_id = ?`,
				q8(q), benchK, scope.ApplicationID, scope.TenantID, scope.WorkspaceID, gen)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var ids []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return nil, err
				}
				ids = append(ids, id)
			}
			return ids, rows.Err()
		},
		delete: func(scope contracts.Scope, gen string) error {
			return db.Tx(ctx, func(tx *store.Tx) error {
				rows, err := tx.QueryContext(ctx, `SELECT rowid FROM bench_l2 WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND generation_id = ?`,
					scope.ApplicationID, scope.TenantID, scope.WorkspaceID, gen)
				if err != nil {
					return err
				}
				var ids []int64
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err != nil {
						rows.Close()
						return err
					}
					ids = append(ids, id)
				}
				rows.Close()
				for _, id := range ids {
					if _, err := tx.ExecContext(ctx, `DELETE FROM bench_l2 WHERE rowid = ?`, id); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}, db, nil
}

// ------------------------------------------------------------ the child

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

func fileMiB(path string) float64 {
	if st, err := os.Stat(path); err == nil {
		return float64(st.Size()) / (1 << 20)
	}
	return 0
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

func runScenario(s scenario) (result, error) {
	ctx := context.Background()
	res := result{Scenario: s, Name: s.name()}
	rss0, _, _, ok := processMemory()
	res.RSSMeasured = ok
	res.BaselineMiB = mib(rss0)

	dir, err := os.MkdirTemp("", "alixai-vector-bench-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(dir)
	idx, db, err := openIndex(ctx, s.Provider, dir)
	if err != nil {
		return res, err
	}
	defer db.Close()

	gen := newGenerator(s.Kind, s.Target)
	hot := max(1, s.HotScopes)
	target := benchScope(0)
	const batch = vector.MaxBatch

	ingest := func(scope contracts.Scope, stream uint64, n int) error {
		for start := 0; start < n; start += batch {
			end := min(start+batch, n)
			ids := make([]string, 0, end-start)
			vecs := make([][]float32, 0, end-start)
			for i := start; i < end; i++ {
				ids = append(ids, chunkID(i))
				vecs = append(vecs, gen.vec(stream, uint64(i)))
			}
			if err := idx.upsert(scope, "g1", ids, vecs); err != nil {
				return err
			}
		}
		return nil
	}
	t0 := time.Now()
	if err := ingest(target, 0, s.Target); err != nil {
		return res, err
	}
	targetDur := time.Since(t0)
	res.IngestSec = targetDur.Seconds()
	res.IngestPerSec = float64(s.Target) / targetDur.Seconds()
	for h := 1; h < hot; h++ { // further hot scopes, searched in rotation
		if err := ingest(benchScope(h), uint64(h), s.Target); err != nil {
			return res, err
		}
	}
	if s.Others > 0 {
		per := s.Others / 9
		for tnt := 1; tnt <= 9; tnt++ {
			if err := ingest(benchScope(100+tnt), uint64(100+tnt), per); err != nil {
				return res, err
			}
		}
	}
	rssIngest, _, _, _ := processMemory()
	res.AfterIngestMiB = mib(rssIngest)

	// Timed queries.
	queries := make([][]float32, s.Queries)
	for i := range queries {
		queries[i] = gen.query(i)
	}
	answers := make([][]string, s.Queries)
	lat := make([]float64, s.Queries)
	t1 := time.Now()
	ids, err := idx.search(benchScope(0), "g1", queries[0])
	if err != nil {
		return res, err
	}
	res.FirstQueryMs = float64(time.Since(t1)) / 1e6
	answers[0], lat[0] = ids, res.FirstQueryMs
	conc := max(1, s.Concurrency)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	next := 1
	wall := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				i := next
				next++
				mu.Unlock()
				if i >= s.Queries {
					return
				}
				start := time.Now()
				got, err := idx.search(benchScope(i%hot), "g1", queries[i])
				d := float64(time.Since(start)) / 1e6
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				answers[i], lat[i] = got, d
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return res, firstErr
	}
	res.QPS = float64(s.Queries-1) / time.Since(wall).Seconds()
	warm := append([]float64(nil), lat[1:]...)
	sort.Float64s(warm)
	var sum float64
	for _, d := range warm {
		sum += d
	}
	res.MeanMs = sum / float64(len(warm))
	res.P50Ms, res.P95Ms, res.P99Ms = percentile(warm, 0.50), percentile(warm, 0.95), percentile(warm, 0.99)

	rssQ, peak, _, _ := processMemory()
	res.AfterQueriesMiB, res.PeakMiB = mib(rssQ), mib(peak)
	res.DBMiB = fileMiB(filepath.Join(dir, "bench.db"))
	res.WALMiB = fileMiB(filepath.Join(dir, "bench.db-wal"))

	// Recall against an exact float64 brute force over the regenerated corpus.
	nq := min(s.RecallQ*hot, s.Queries) // only the queries that went to scope 0 are checked
	type top struct {
		score float64
		id    int
	}
	var checked []int
	for qi := 0; qi < nq; qi++ {
		if qi%hot == 0 {
			checked = append(checked, qi)
		}
	}
	tops := make([][]top, len(checked))
	for i := 0; i < s.Target; i++ {
		v := gen.vec(0, uint64(i))
		for ci, qi := range checked {
			var d float64
			for j, x := range v {
				d += float64(x) * float64(queries[qi][j])
			}
			t := tops[ci]
			if len(t) < benchK {
				tops[ci] = append(t, top{d, i})
				if len(tops[ci]) == benchK {
					sort.Slice(tops[ci], func(a, b int) bool { return tops[ci][a].score < tops[ci][b].score })
				}
			} else if d > t[0].score {
				t[0] = top{d, i}
				sort.Slice(t, func(a, b int) bool { return t[a].score < t[b].score })
			}
		}
	}
	var rsum float64
	for ci, qi := range checked {
		want := make([]string, 0, benchK)
		for _, t := range tops[ci] {
			want = append(want, chunkID(t.id))
		}
		rsum += vectortest.Recall(want, answers[qi])
	}
	res.Recall10 = rsum / float64(len(checked))

	// Delete the searched generation while a second writer probes the store.
	if _, err := db.ExecContext(ctx, `CREATE TABLE bench_probe (x INTEGER)`); err != nil {
		return res, err
	}
	var (
		stalls []float64
		stop   = make(chan struct{})
		done   = make(chan struct{})
	)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			start := time.Now()
			if _, err := db.ExecContext(ctx, `INSERT INTO bench_probe VALUES (1)`); err == nil {
				stalls = append(stalls, float64(time.Since(start))/1e6)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	t2 := time.Now()
	err = idx.delete(target, "g1")
	res.DeleteMs = float64(time.Since(t2)) / 1e6
	close(stop)
	<-done
	if err != nil {
		return res, err
	}
	sort.Float64s(stalls)
	if n := len(stalls); n > 0 {
		res.DeleteStallMs, res.DeleteStallP99 = stalls[n-1], percentile(stalls, 0.99)
	}
	return res, nil
}

// TestBenchChild is the measuring half; it does nothing unless the parent sets
// ALIXAI_VECTOR_BENCH_SCENARIO.
func TestBenchChild(t *testing.T) {
	raw := os.Getenv("ALIXAI_VECTOR_BENCH_SCENARIO")
	if raw == "" {
		t.Skip("child of TestBenchMatrix")
	}
	var s scenario
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	res, err := runScenario(s)
	if err != nil {
		t.Fatalf("%s: %v", s.name(), err)
	}
	out, _ := json.Marshal(res)
	fmt.Printf("BENCHRESULT %s\n", out)
}

// ------------------------------------------------------------ the parent

func benchMatrix(queries int) []scenario {
	var out []scenario
	providers := []string{"flat-streamed", "flat-resident", "sqlitevec-cosine", "sqlitevec-l2"}
	add := func(kind string, target, others, conc int) {
		for _, p := range providers {
			out = append(out, scenario{Provider: p, Kind: kind, Target: target, Others: others, Queries: queries, RecallQ: 50, Concurrency: conc})
		}
	}
	add("clustered", 10_000, 0, 1)
	add("clustered", 100_000, 0, 1)
	add("clustered", 10_000, 90_000, 1)
	add("uniform", 10_000, 0, 1)
	add("uniform", 100_000, 0, 1)
	// The sizes between, for the streamed path's ceiling (which has no cache to hide behind).
	for _, n := range []int{25_000, 50_000} {
		for _, p := range []string{"flat-streamed", "flat-resident", "sqlitevec-cosine"} {
			out = append(out, scenario{Provider: p, Kind: "clustered", Target: n, Queries: queries, RecallQ: 50, Concurrency: 1})
		}
	}
	// The supported deployment ceiling: the largest workspace (100k) beside 150k
	// more vectors in nine other workspaces, 250k active in all.
	for _, p := range []string{"flat-streamed", "flat-resident", "sqlitevec-cosine"} {
		out = append(out, scenario{Provider: p, Kind: "clustered", Target: 100_000, Others: 150_000, Queries: queries, RecallQ: 50, Concurrency: 1})
	}
	// Two hot 100k workspaces against the default 64 MiB cache (it holds one), and
	// against 128 MiB (it holds both).
	for _, p := range []string{"flat-resident", "flat-resident-128"} {
		out = append(out, scenario{Provider: p, Kind: "clustered", Target: 100_000, Queries: queries, RecallQ: 25, Concurrency: 1, HotScopes: 2})
	}
	// Three hot 100k workspaces against the default 64 MiB cache: one stays resident
	// and two stream, which must be no worse than the cache off.
	out = append(out, scenario{Provider: "flat-resident", Kind: "clustered", Target: 100_000, Queries: queries, RecallQ: 25, Concurrency: 1, HotScopes: 3})
	// Four concurrent searchers on the full ceiling: the reader pool is four wide.
	for _, p := range []string{"flat-streamed", "flat-resident", "sqlitevec-cosine"} {
		out = append(out, scenario{Provider: p, Kind: "clustered", Target: 100_000, Queries: queries, RecallQ: 20, Concurrency: 4})
	}
	return out
}

func TestBenchMatrix(t *testing.T) {
	if os.Getenv("ALIXAI_VECTOR_BENCH") == "" {
		t.Skip("set ALIXAI_VECTOR_BENCH=1 to run the head-to-head benchmark (several minutes)")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	queries := 100
	if v, err := strconv.Atoi(os.Getenv("ALIXAI_VECTOR_BENCH_QUERIES")); err == nil && v > 1 {
		queries = v
	}
	only := os.Getenv("ALIXAI_VECTOR_BENCH_ONLY")
	var results []result
	for _, s := range benchMatrix(queries) {
		if only != "" && !strings.Contains(s.name(), only) {
			continue
		}
		raw, _ := json.Marshal(s)
		cmd := exec.Command(exe, "-test.run", "^TestBenchChild$", "-test.v", "-test.timeout=30m")
		cmd.Env = append(os.Environ(), "ALIXAI_VECTOR_BENCH_SCENARIO="+string(raw))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Errorf("%s failed: %v\n%s", s.name(), err, out)
			continue
		}
		var r result
		found := false
		for _, line := range strings.Split(string(out), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "BENCHRESULT "); ok {
				if err := json.Unmarshal([]byte(rest), &r); err != nil {
					t.Fatalf("%s: unreadable result: %v", s.name(), err)
				}
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no result in the child's output:\n%s", s.name(), out)
			continue
		}
		results = append(results, r)
		t.Logf("%-52s p50 %7.2f ms  p95 %7.2f ms  recall@10 %.3f  RSS peak %6.1f MiB  ingest %6.0f/s  db %6.1f MiB  delete %7.0f ms (writer stall max %6.0f ms)",
			r.Name, r.P50Ms, r.P95Ms, r.Recall10, r.PeakMiB, r.IngestPerSec, r.DBMiB, r.DeleteMs, r.DeleteStallMs)
		if outPath := os.Getenv("ALIXAI_VECTOR_BENCH_OUT"); outPath != "" {
			f, err := os.OpenFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err == nil {
				b, _ := json.Marshal(r)
				fmt.Fprintf(f, "%s\n", b)
				f.Close()
			}
		}
	}
	t.Logf("%d scenarios measured", len(results))
}
