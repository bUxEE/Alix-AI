package vector_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/rag/vector/vectortest"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

var bg = context.Background()

// newFlat opens a throwaway store and a flat provider over it.
func newFlat(t testing.TB, cfg vector.Config) (*vector.Flat, *store.DB) {
	t.Helper()
	db := storetest.Open(t, nil)
	f, err := vector.NewFlat(db, vector.FlatOptions{Config: cfg})
	if err != nil {
		t.Fatalf("NewFlat: %v", err)
	}
	return f, db
}

func noCache() vector.Config {
	c := vector.DefaultConfig()
	c.CacheMiB = 0
	return c
}

func withCache() vector.Config {
	c := vector.DefaultConfig()
	c.CacheMiB = 8
	return c
}

func upsertAll(t testing.TB, p vector.VectorIndexProvider, scope contracts.Scope, gen, source string, items []vector.Item) {
	t.Helper()
	for i := 0; i < len(items); i += vector.MaxBatch {
		_, err := p.Upsert(bg, scope, vector.UpsertBatch{Generation: gen, SourceID: source, Model: vectortest.Model, Items: items[i:min(i+vector.MaxBatch, len(items))]})
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
}

func searchIDs(t testing.TB, p vector.VectorIndexProvider, scope contracts.Scope, q []float32, k int, gens ...string) (ids []string, res vector.SearchResult) {
	t.Helper()
	res, err := p.Search(bg, scope, vector.Query{Vector: q, Model: vectortest.Model, K: k, Generations: gens})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range res.Hits {
		ids = append(ids, h.ChunkID)
	}
	return ids, res
}

func rawCount(t testing.TB, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(bg, query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// SEC-021: the scope is applied before similarity. Three foreign scopes hold
// thousands of vectors closer to the query than anything in the caller's scope,
// one identical to it, under the same generation id; the caller must still get
// ten of its own hits, and the scan must have read only its own range.
func TestScopePrefilterBeforeSimilarity(t *testing.T) {
	for name, cfg := range map[string]vector.Config{"streamed": noCache(), "resident": withCache()} {
		t.Run(name, func(t *testing.T) {
			f, _ := newFlat(t, cfg)
			r := vectortest.Rand(31)
			const dim = 64
			base := vectortest.Scope("app", "tenA", "ws1")
			q := vectortest.RandomUnit(r, dim)

			own := vectortest.Items(base, "own", vectortest.Clustered(r, 200, dim, 6, 0.7))
			upsertAll(t, f, base, "g1", "s1", own)
			for i, fs := range []contracts.Scope{
				vectortest.Scope("app", "tenB", "ws1"),
				vectortest.Scope("app", "tenA", "ws2"),
				vectortest.Scope("appX", "tenA", "ws1"),
			} {
				vecs := [][]float32{append([]float32(nil), q...)}
				for j := 0; j < 1500; j++ {
					vecs = append(vecs, vectortest.Perturb(r, q, 0.05))
				}
				upsertAll(t, f, fs, "g1", "s1", vectortest.Items(fs, fmt.Sprintf("foreign%d-", i), vecs))
			}

			ids, res := searchIDs(t, f, base, q, 10, "g1")
			if len(ids) != 10 {
				t.Fatalf("got %d hits, want 10: the caller's candidates were crowded out by foreign vectors", len(ids))
			}
			for _, id := range ids {
				if !strings.HasPrefix(id, "own") {
					t.Fatalf("hit %q is not the caller's", id)
				}
			}
			if res.Scanned != len(own) {
				t.Fatalf("scanned %d vectors, want exactly the caller's %d: the scan was not confined to the scope", res.Scanned, len(own))
			}
			want := vectortest.BruteForce(vectortest.IDs(own), vectortest.Vectors(own), q, 10)
			if rec := vectortest.Recall(want, ids); rec < 0.9 {
				t.Fatalf("recall against the caller's own brute force = %.2f", rec)
			}
		})
	}
}

// The plan that makes the prefilter true by construction: a primary-key range
// over the scope and generation, never a scan of the whole table.
func TestScanReadsOnlyTheScopeRange(t *testing.T) {
	_, db := newFlat(t, noCache())
	scope := vectortest.Scope("app", "tenA", "ws1")
	query, args, err := vector.RowsStmt(scope, "g1")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.QueryContext(bg, "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	got := strings.Join(plan, "; ")
	want := "SEARCH vector_rows USING PRIMARY KEY (application_id=? AND tenant_id=? AND workspace_id=? AND generation_id=?)"
	if got != want {
		t.Fatalf("query plan = %q, want %q", got, want)
	}
}

// A deleted vector is gone, from the streamed path, from the resident cache
// (including a cache another handle on the same database would have to notice),
// and from the tables themselves.
func TestDeleteRemovesVectors(t *testing.T) {
	for name, cfg := range map[string]vector.Config{"streamed": noCache(), "resident": withCache()} {
		t.Run(name, func(t *testing.T) {
			f, db := newFlat(t, cfg)
			r := vectortest.Rand(41)
			const dim = 48
			a := vectortest.Scope("app", "tenA", "ws1")
			b := vectortest.Scope("app", "tenB", "ws1")
			ia := vectortest.Items(a, "a", vectortest.Clustered(r, 120, dim, 5, 0.6))
			ib := vectortest.Items(b, "a", vectortest.Clustered(r, 30, dim, 5, 0.6))
			upsertAll(t, f, a, "g1", "s1", ia)
			upsertAll(t, f, b, "g1", "s1", ib)

			// Warm the cache, then delete the chunk we are about to ask for.
			target := ia[17]
			if ids, _ := searchIDs(t, f, a, target.Vector, 1, "g1"); len(ids) != 1 || ids[0] != target.ChunkID {
				t.Fatalf("setup: best hit = %v, want %s", ids, target.ChunkID)
			}
			if n, err := f.DeleteChunks(bg, a, "g1", []string{target.ChunkID}); err != nil || n != 1 {
				t.Fatalf("DeleteChunks = %d, %v", n, err)
			}
			ids, _ := searchIDs(t, f, a, target.Vector, 120, "g1")
			for _, id := range ids {
				if id == target.ChunkID {
					t.Fatal("the deleted chunk is still served (stale cache?)")
				}
			}
			if len(ids) != 119 {
				t.Fatalf("%d chunks served, want 119", len(ids))
			}
			if n := rawCount(t, db, `SELECT count(*) FROM vector_rows WHERE application_id = 'app' AND tenant_id = 'tenA' AND chunk_id = ?`, target.ChunkID); n != 0 {
				t.Fatalf("the row of a deleted chunk is still in vector_rows (%d)", n)
			}
			if n := rawCount(t, db, `SELECT vectors FROM vector_generations WHERE application_id = 'app' AND tenant_id = 'tenA' AND generation_id = 'g1'`); n != 119 {
				t.Fatalf("generation count = %d, want 119", n)
			}

			// A second handle on the same database (another process, a rebuild
			// tool) deletes; this handle's cache must notice.
			f2, err := vector.NewFlat(db, vector.FlatOptions{Config: noCache()})
			if err != nil {
				t.Fatal(err)
			}
			victim := ia[40]
			if ids, _ := searchIDs(t, f, a, victim.Vector, 1, "g1"); ids[0] != victim.ChunkID {
				t.Fatalf("setup: best hit = %v", ids)
			}
			if n, err := f2.DeleteChunks(bg, a, "g1", []string{victim.ChunkID}); err != nil || n != 1 {
				t.Fatalf("second handle DeleteChunks = %d, %v", n, err)
			}
			ids, _ = searchIDs(t, f, a, victim.Vector, 120, "g1")
			for _, id := range ids {
				if id == victim.ChunkID {
					t.Fatal("a chunk deleted through another handle is still served from this handle's cache")
				}
			}

			// Whole-generation and whole-scope deletes leave no rows at all.
			if _, err := f.DeleteGeneration(bg, a, "g1"); err != nil {
				t.Fatal(err)
			}
			if n := rawCount(t, db, `SELECT count(*) FROM vector_rows WHERE tenant_id = 'tenA'`); n != 0 {
				t.Fatalf("%d rows of tenant A survive DeleteGeneration", n)
			}
			if _, err := f.DeleteScope(bg, b); err != nil {
				t.Fatal(err)
			}
			if n := rawCount(t, db, `SELECT count(*) FROM vector_rows`); n != 0 {
				t.Fatalf("%d rows survive deleting everything", n)
			}
			if n := rawCount(t, db, `SELECT count(*) FROM vector_generations`); n != 0 {
				t.Fatalf("%d generation rows survive deleting everything", n)
			}
			if held, used := vector.CacheStats(f); held != 0 || used != 0 {
				t.Fatalf("the cache still holds %d entries (%d bytes) of deleted generations", held, used)
			}
		})
	}
}

// Deleting a generation is immediate (the generation is hidden before any row
// goes) and bounded (rows go in batches), and an interrupted delete is finished
// later without anything of it ever being served again.
func TestDeleteGenerationIsHiddenAtOnceAndResumable(t *testing.T) {
	for name, cfg := range map[string]vector.Config{"streamed": noCache(), "resident": withCache()} {
		t.Run(name, func(t *testing.T) {
			f, db := newFlat(t, cfg)
			r := vectortest.Rand(91)
			const dim = 16
			a := vectortest.Scope("app", "tenA", "ws1")
			big := vectortest.Items(a, "big", vectortest.Clustered(r, 5000, dim, 8, 0.5)) // more than two delete batches
			keep := vectortest.Items(a, "keep", vectortest.Clustered(r, 40, dim, 3, 0.5))
			upsertAll(t, f, a, "gdrop", "s1", big)
			upsertAll(t, f, a, "gkeep", "s2", keep)
			if ids, _ := searchIDs(t, f, a, big[10].Vector, 1, "gdrop"); ids[0] != big[10].ChunkID {
				t.Fatalf("setup: %v", ids)
			}

			// Simulate a crash after phase one: the generation is marked, no row is gone.
			if _, err := db.ExecContext(bg, `UPDATE vector_generations SET state = 'dropping', rev = rev + 1 WHERE generation_id = 'gdrop'`); err != nil {
				t.Fatal(err)
			}
			ids, res := searchIDs(t, f, a, big[10].Vector, 5, "gdrop")
			if len(ids) != 0 || len(res.Skipped) != 1 {
				t.Fatalf("a dropping generation is still served: %v %+v", ids, res)
			}
			if st, _ := f.Stats(bg, a); len(st.Generations) != 1 || st.Generations[0].Generation != "gkeep" {
				t.Fatalf("a dropping generation is still listed: %+v", st)
			}
			if _, err := f.Upsert(bg, a, vector.UpsertBatch{Generation: "gdrop", SourceID: "s1", Model: vectortest.Model, Items: big[:1]}); contracts.CodeOf(err) != contracts.ErrConflict {
				t.Fatalf("upsert into a dropping generation: %v, want conflict", err)
			}
			// Nothing reusable is left in it either.
			reused, err := f.Reuse(bg, a, vector.ReuseRequest{Generation: "gnew", SourceID: "s1", Model: vectortest.Model,
				Refs: []vector.ReuseRef{{ChunkID: big[10].ChunkID, Key: big[10].Key}}})
			if err != nil || len(reused.Reused) != 0 {
				t.Fatalf("reuse from a dropping generation: %+v, %v", reused, err)
			}

			// The janitor finishes it.
			if n, err := f.ResumeDrops(bg); err != nil || n != 1 {
				t.Fatalf("ResumeDrops = %d, %v, want 1", n, err)
			}
			if n := rawCount(t, db, `SELECT count(*) FROM vector_rows WHERE generation_id = 'gdrop'`); n != 0 {
				t.Fatalf("%d rows of the dropped generation remain", n)
			}
			if n := rawCount(t, db, `SELECT count(*) FROM vector_generations WHERE generation_id = 'gdrop'`); n != 0 {
				t.Fatal("the generation row remains")
			}
			if n, err := f.ResumeDrops(bg); err != nil || n != 0 {
				t.Fatalf("a second ResumeDrops = %d, %v, want nothing to do", n, err)
			}
			if got, _ := searchIDs(t, f, a, keep[3].Vector, 1, "gkeep"); len(got) != 1 || got[0] != keep[3].ChunkID {
				t.Fatalf("the other generation was disturbed: %v", got)
			}

			// A normal DeleteGeneration of many rows reports the count and leaves nothing.
			upsertAll(t, f, a, "gbig2", "s3", big)
			if n, err := f.DeleteGeneration(bg, a, "gbig2"); err != nil || n != len(big) {
				t.Fatalf("DeleteGeneration = %d, %v, want %d", n, err, len(big))
			}
			if n := rawCount(t, db, `SELECT count(*) FROM vector_rows WHERE generation_id = 'gbig2'`); n != 0 {
				t.Fatalf("%d rows remain after DeleteGeneration", n)
			}
			if n := rawCount(t, db, `SELECT count(*) FROM vector_rows`); n != len(keep) {
				t.Fatalf("%d rows remain in total, want the %d kept", n, len(keep))
			}
		})
	}
}

// Recall@10 of the int8 flat scan against an independent float64 brute force,
// at the real embedding dimension (384, Granite Embedding 97M). The floor is the
// 0.95 bar of ADR 0011; the larger 10k and 100k measurements are in the
// benchmark harness (bench_test.go).
func TestRecallAgainstBruteForce(t *testing.T) {
	const (
		dim     = 384
		n       = 5000
		queries = 50
	)
	for _, kind := range []string{"clustered", "uniform"} {
		t.Run(kind, func(t *testing.T) {
			f, _ := newFlat(t, withCache())
			r := vectortest.Rand(51)
			scope := vectortest.Scope("app", "tenA", "ws1")
			var vecs [][]float32
			if kind == "clustered" {
				vecs = vectortest.Clustered(r, n, dim, 50, 0.55)
			} else {
				for i := 0; i < n; i++ {
					vecs = append(vecs, vectortest.RandomUnit(r, dim))
				}
			}
			items := vectortest.Items(scope, "c", vecs)
			upsertAll(t, f, scope, "g1", "s1", items)
			ids := vectortest.IDs(items)
			var sum float64
			for i := 0; i < queries; i++ {
				q := vectortest.RandomUnit(r, dim)
				if kind == "clustered" {
					q = vectortest.Perturb(r, vecs[r.IntN(n)], 0.3)
				}
				got, _ := searchIDs(t, f, scope, q, 10, "g1")
				sum += vectortest.Recall(vectortest.BruteForce(ids, vecs, q, 10), got)
			}
			recall := sum / queries
			t.Logf("%s: recall@10 = %.4f (%d queries, %d vectors, dimension %d)", kind, recall, queries, n, dim)
			if recall < 0.95 {
				t.Fatalf("recall@10 = %.4f, below the 0.95 bar of ADR 0011", recall)
			}
		})
	}
}

func TestUpsertCeilingsAreEnforced(t *testing.T) {
	cfg := noCache()
	cfg.MaxVectorsPerWorkspace = 10
	cfg.MaxVectorsPerDeployment = 16
	f, db := newFlat(t, cfg)
	r := vectortest.Rand(61)
	a := vectortest.Scope("app", "tenA", "ws1")
	b := vectortest.Scope("app", "tenB", "ws1")
	mk := func(sc contracts.Scope, prefix string, n int) []vector.Item {
		return vectortest.Items(sc, prefix, vectortest.Clustered(r, n, 16, 2, 0.5))
	}
	up := func(sc contracts.Scope, gen string, items []vector.Item) error {
		_, err := f.Upsert(bg, sc, vector.UpsertBatch{Generation: gen, SourceID: "s1", Model: vectortest.Model, Items: items})
		return err
	}

	first := mk(a, "a", 8)
	if err := up(a, "g1", first); err != nil {
		t.Fatal(err)
	}
	// Eight more would make 16 > 10 in the workspace: refused as a whole.
	err := up(a, "g2", mk(a, "b", 8))
	if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
		t.Fatalf("err = %v, want resource_exhausted", err)
	}
	if n := rawCount(t, db, `SELECT count(*) FROM vector_rows WHERE tenant_id = 'tenA'`); n != 8 {
		t.Fatalf("a refused batch left %d rows, want the original 8 (the batch is atomic)", n)
	}
	if st, _ := f.Stats(bg, a); len(st.Generations) != 1 {
		t.Fatalf("a refused batch left its generation behind: %+v", st)
	}
	// Replacing existing chunks at the ceiling is not growth.
	if err := up(a, "g1", first); err != nil {
		t.Fatalf("replacing at the ceiling: %v", err)
	}
	// Up to the limit is fine.
	if err := up(a, "g1", mk(a, "c", 2)); err != nil {
		t.Fatalf("filling to the limit: %v", err)
	}
	if err := up(a, "g1", mk(a, "d", 1)); contracts.CodeOf(err) != contracts.ErrResourceExhausted {
		t.Fatalf("one past the workspace limit: %v", err)
	}
	// Another workspace is bounded by the deployment ceiling: 10 held, 16 allowed.
	if err := up(b, "g1", mk(b, "e", 6)); err != nil {
		t.Fatalf("within the deployment ceiling: %v", err)
	}
	err = up(b, "g1", mk(b, "f", 1))
	if contracts.CodeOf(err) != contracts.ErrResourceExhausted {
		t.Fatalf("one past the deployment ceiling: %v", err)
	}
	// The error never reveals another tenant's holdings.
	if msg := fmt.Sprint(err); strings.Contains(msg, "tenA") || strings.Contains(msg, "workspace") {
		t.Fatalf("the deployment-limit error leaks another scope's data: %v", err)
	}
	// Making room makes writing possible again.
	if _, err := f.DeleteGeneration(bg, a, "g1"); err != nil {
		t.Fatal(err)
	}
	if err := up(b, "g1", mk(b, "g", 4)); err != nil {
		t.Fatalf("after freeing space: %v", err)
	}
}

func TestResidentCacheStaysCurrentAndBounded(t *testing.T) {
	f, db := newFlat(t, vector.Config{CacheMiB: 1}) // zero ceilings take their defaults
	r := vectortest.Rand(71)
	const dim = 384
	a := vectortest.Scope("app", "tenA", "ws1")
	mk := func(prefix string, n int) []vector.Item {
		return vectortest.Items(a, prefix, vectortest.Clustered(r, n, dim, 4, 0.5))
	}
	small := mk("s", 2000) // about 0.8 MiB resident: one fits, two do not
	other := mk("o", 2000)
	huge := mk("h", 3000) // over the 1 MiB budget on its own: always streamed
	upsertAll(t, f, a, "small", "s1", small)
	upsertAll(t, f, a, "other", "s2", other)
	upsertAll(t, f, a, "huge", "s3", huge)

	held := func() (int, int64) { return vector.CacheStats(f) }
	if ids, _ := searchIDs(t, f, a, small[5].Vector, 1, "small"); ids[0] != small[5].ChunkID {
		t.Fatalf("best hit = %v", ids)
	}
	if n, _ := held(); n != 1 {
		t.Fatalf("%d entries cached after one search, want 1", n)
	}

	// A second generation does not fit beside the first: the older is evicted
	// and the budget is never exceeded.
	if ids, _ := searchIDs(t, f, a, other[9].Vector, 1, "other"); ids[0] != other[9].ChunkID {
		t.Fatalf("best hit = %v", ids)
	}
	if n, used := held(); n != 1 || used > 1<<20 {
		t.Fatalf("cache holds %d entries, %d bytes, with a 1 MiB budget", n, used)
	}
	// A generation larger than the whole budget is streamed, and correct.
	ids, res := searchIDs(t, f, a, huge[100].Vector, 1, "huge")
	if ids[0] != huge[100].ChunkID || res.Scanned != len(huge) {
		t.Fatalf("streamed fallback: %v, scanned %d", ids, res.Scanned)
	}
	if _, used := held(); used > 1<<20 {
		t.Fatalf("an oversized generation was cached: %d bytes", used)
	}

	// A write is visible to the very next search: the rev changed, the old
	// entry is unreachable.
	fresh := vectortest.RandomUnit(r, dim)
	extra := vectortest.Items(a, "x", [][]float32{fresh})
	upsertAll(t, f, a, "other", "s2", extra)
	ids, _ = searchIDs(t, f, a, fresh, 1, "other")
	if ids[0] != extra[0].ChunkID {
		t.Fatalf("a vector written after the cache was filled is not found: %v", ids)
	}
	// Same through a second handle writing behind this one's back.
	f2, err := vector.NewFlat(db, vector.FlatOptions{Config: noCache()})
	if err != nil {
		t.Fatal(err)
	}
	later := vectortest.RandomUnit(r, dim)
	upsertAll(t, f2, a, "other", "s2", vectortest.Items(a, "y", [][]float32{later}))
	ids, _ = searchIDs(t, f, a, later, 1, "other")
	if ids[0] != "y0000" {
		t.Fatalf("a vector written through another handle is not found: %v", ids)
	}
}

// Hot generations that do not all fit must not evict each other on every
// search: loading costs about a streamed scan on top of the scan, so plain LRU
// made the cache slower than no cache (measured at two 100,000-vector
// workspaces in a 64 MiB budget, ADR 0011). Whichever generation got in first
// stays resident while it keeps being searched, however many rivals rotate
// through, and the rivals are streamed (and correct). A rival that keeps asking
// replaces an incumbent that has gone cold.
func TestResidentCacheNeverThrashesUnderRotation(t *testing.T) {
	const dim = 384
	for _, rivals := range []int{1, 2, 3, 5} { // 2, 3, 4 and 6 hot generations for a budget of one
		t.Run(fmt.Sprintf("%d_hot", rivals+1), func(t *testing.T) {
			f, _ := newFlat(t, vector.Config{CacheMiB: 1}) // holds one 2000-vector generation, not two
			r := vectortest.Rand(72)
			type hot struct {
				scope contracts.Scope
				items []vector.Item
			}
			var all []hot
			for i := 0; i <= rivals; i++ {
				sc := vectortest.Scope("app", "tenA", fmt.Sprintf("ws%d", i+1))
				items := vectortest.Items(sc, fmt.Sprintf("c%d_", i), vectortest.Clustered(r, 2000, dim, 4, 0.5))
				upsertAll(t, f, sc, "g1", "s1", items)
				all = append(all, hot{sc, items})
			}
			resident := func() (which []int) {
				for i, h := range all {
					if vector.CacheBytesFor(f, h.scope.ApplicationID, h.scope.TenantID, h.scope.WorkspaceID) > 0 {
						which = append(which, i)
					}
				}
				return which
			}
			search := func(i, probe int) {
				t.Helper()
				h := all[i]
				ids, res := searchIDs(t, f, h.scope, h.items[probe].Vector, 1, "g1")
				if len(ids) != 1 || ids[0] != h.items[probe].ChunkID || res.Scanned != len(h.items) {
					t.Fatalf("search in %s: %v, scanned %d", h.scope.WorkspaceID, ids, res.Scanned)
				}
			}

			// Rotate through every generation: the first to get in stays, so no
			// search after the first ever evicts anything (no repeated loads).
			search(0, 0)
			if got := resident(); len(got) != 1 || got[0] != 0 {
				t.Fatalf("after the first search resident = %v, want [0]", got)
			}
			for round := 0; round < 10; round++ {
				for i := range all {
					search(i, (round*7+i)%len(all[i].items))
					if got := resident(); len(got) != 1 || got[0] != 0 {
						t.Fatalf("round %d, after searching generation %d: resident = %v, want [0] throughout (the cache thrashes)", round, i, got)
					}
				}
			}
			// Generation 0 goes cold; generation 1 keeps asking and takes the room.
			took := -1
			for i := 0; i < 6 && took < 0; i++ {
				search(1, i)
				if got := resident(); len(got) == 1 && got[0] == 1 {
					took = i
				}
			}
			if took < 0 {
				t.Fatalf("generation 1 never replaced the idle generation 0 (resident %v)", resident())
			}
			if n, used := vector.CacheStats(f); n != 1 || used > 1<<20 {
				t.Fatalf("cache holds %d entries, %d bytes, with a 1 MiB budget", n, used)
			}
		})
	}
}

// Flat finds a reusable vector under the same chunk id in the scope's other live
// generations, and only when the input key is equal: a different chunk id or a
// different key reuses nothing (an unchanged chunk keeps its id; the key says
// its embedding input did not change either).
func TestReuseIsAnchoredToTheChunkAndTheKey(t *testing.T) {
	f, db := newFlat(t, withCache())
	r := vectortest.Rand(95)
	a := vectortest.Scope("app", "tenA", "ws1")
	items := vectortest.Items(a, "c", vectortest.Clustered(r, 4, 32, 2, 0.5))
	upsertAll(t, f, a, "g1", "s1", items)
	otherKey, err := vector.NewInputKey(a, vectortest.Model, "some other input")
	if err != nil {
		t.Fatal(err)
	}
	req := func(refs ...vector.ReuseRef) []string {
		t.Helper()
		res, err := f.Reuse(bg, a, vector.ReuseRequest{Generation: "g2", SourceID: "s1", Model: vectortest.Model, Refs: refs})
		if err != nil {
			t.Fatal(err)
		}
		return res.Reused
	}
	if got := req(vector.ReuseRef{ChunkID: "renamed", Key: items[0].Key}); len(got) != 0 {
		t.Fatalf("a vector was reused under another chunk id: %v", got)
	}
	if got := req(vector.ReuseRef{ChunkID: items[0].ChunkID, Key: otherKey}); len(got) != 0 {
		t.Fatalf("a vector was reused for a changed input: %v", got)
	}
	if n := rawCount(t, db, `SELECT count(*) FROM vector_generations WHERE generation_id = 'g2'`); n != 0 {
		t.Fatal("a reuse that found nothing created a generation")
	}
	if got := req(vector.ReuseRef{ChunkID: items[1].ChunkID, Key: items[1].Key}, vector.ReuseRef{ChunkID: items[0].ChunkID, Key: otherKey}); len(got) != 1 || got[0] != items[1].ChunkID {
		t.Fatalf("reused %v, want only %s", got, items[1].ChunkID)
	}
}

func TestVectorsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	r := vectortest.Rand(81)
	a := vectortest.Scope("app", "tenA", "ws1")
	items := vectortest.Items(a, "c", vectortest.Clustered(r, 50, 32, 3, 0.5))

	db1, err := store.Open(bg, store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	f1, err := vector.NewFlat(db1, vector.FlatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	upsertAll(t, f1, a, "g1", "s1", items)
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := store.Open(bg, store.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	f2, err := vector.NewFlat(db2, vector.FlatOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := searchIDs(t, f2, a, items[12].Vector, 1, "g1")
	if len(ids) != 1 || ids[0] != items[12].ChunkID {
		t.Fatalf("after reopening, best hit = %v, want %s", ids, items[12].ChunkID)
	}
	if err := db2.IntegrityCheck(bg); err != nil {
		t.Fatalf("integrity check after reopen: %v", err)
	}
}

// Every table of the module carries the three scope columns (master plan 10.2).
func TestSchemaCarriesScopeColumns(t *testing.T) {
	_, db := newFlat(t, noCache())
	storetest.AssertScopedTables(t, db)
	for _, table := range []string{"vector_generations", "vector_rows"} {
		if n := rawCount(t, db, `SELECT count(*) FROM pragma_table_info(?) WHERE name IN ('application_id','tenant_id','workspace_id')`, table); n != 3 {
			t.Fatalf("%s has %d of the 3 scope columns", table, n)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	if err := vector.DefaultConfig().Validate(); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	for name, c := range map[string]vector.Config{
		"negative cache":     {CacheMiB: -1},
		"huge cache":         {CacheMiB: vector.MaxCacheMiB + 1},
		"negative workspace": {MaxVectorsPerWorkspace: -1},
		"huge deployment":    {MaxVectorsPerDeployment: vector.MaxVectors + 1},
	} {
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	db := storetest.Open(t, nil)
	if _, err := vector.NewFlat(db, vector.FlatOptions{Config: vector.Config{CacheMiB: -5}}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("vector.NewFlat with a bad config: %v", err)
	}
	if _, err := vector.NewFlat(nil, vector.FlatOptions{}); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("vector.NewFlat without a store: %v", err)
	}
	if c := vector.ResolveConfig(vector.Config{}); c.MaxVectorsPerWorkspace != vector.DefaultMaxVectorsPerWorkspace || c.CacheMiB != 0 {
		t.Fatalf("zero config resolves to %+v: ceilings take defaults, a zero cache stays off", c)
	}
}
