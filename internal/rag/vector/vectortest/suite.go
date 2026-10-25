// Package vectortest is the reusable contract suite for
// vector.VectorIndexProvider (master plan 6, ADR 0011). An implementation, the
// embedded one or a future external one, proves it is interchangeable by
// passing RunProviderSuite from its own test:
//
//	func TestProviderContractSuite(t *testing.T) {
//		vectortest.RunProviderSuite(t, func(t *testing.T) vector.VectorIndexProvider {
//			return newMyProvider(t) // a fresh, empty index
//		})
//	}
//
// The suite checks behaviour a caller depends on for SECURITY (scope is applied
// before similarity, nothing crosses a scope, a deleted vector is gone, reuse
// never finds another scope's embedding) and for CORRECTNESS (ordering, bounds,
// recall against an independent brute force). It does not test speed.
package vectortest

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Factory returns a fresh, empty provider for one subtest.
type Factory func(t *testing.T) vector.VectorIndexProvider

type settings struct {
	dim       int
	corpus    int
	minRecall float64
}

// Option adjusts the suite.
type Option func(*settings)

// WithDim sets the vector dimension (default 32), for a provider fixed to one
// dimension such as the 384 of Granite Embedding 97M.
func WithDim(dim int) Option { return func(s *settings) { s.dim = dim } }

// WithCorpus sets the corpus size of the recall test (default 1500).
func WithCorpus(n int) Option { return func(s *settings) { s.corpus = n } }

// WithRecallFloor sets the recall@10 an exact provider must reach against brute
// force on clustered data (default 0.95, the bar of ADR 0011).
func WithRecallFloor(r float64) Option { return func(s *settings) { s.minRecall = r } }

var bg = context.Background()

// RunProviderSuite runs every contract subtest against providers from newProvider.
func RunProviderSuite(t *testing.T, newProvider Factory, opts ...Option) {
	t.Helper()
	cfg := settings{dim: 32, corpus: 1500, minRecall: 0.95}
	for _, o := range opts {
		o(&cfg)
	}
	run := func(name string, fn func(t *testing.T, p vector.VectorIndexProvider, cfg settings)) {
		t.Run(name, func(t *testing.T) { fn(t, newProvider(t), cfg) })
	}
	run("ScopeRequired", testScopeRequired)
	run("ScopePrefilterBeforeSimilarity", testScopePrefilter)
	run("DelimiterCollisionsStayApart", testDelimiterCollisions)
	run("GenerationsAreSeparate", testGenerations)
	run("ModelAndDimensionAreEnforced", testModelAndDimension)
	run("UpsertReplacesAndIsIdempotent", testUpsert)
	run("DeleteRemovesVectors", testDeletes)
	run("ReuseNeverCrossesScopes", testReuse)
	run("InputsAreBounded", testBounds)
	run("OrderingIsDeterministic", testOrdering)
	run("StatsAreScoped", testStats)
	run("RecallAgainstBruteForce", testRecall)
}

// ---------------------------------------------------------------- helpers

func codeIs(t *testing.T, err error, codes ...contracts.ErrCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want one of %v", codes)
	}
	got := contracts.CodeOf(err)
	for _, c := range codes {
		if got == c {
			return
		}
	}
	t.Fatalf("error code = %q (%v), want one of %v", got, err, codes)
}

func fatalIf(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func statsOf(t *testing.T, p vector.VectorIndexProvider, scope contracts.Scope) vector.Stats {
	t.Helper()
	st, err := p.Stats(bg, scope)
	fatalIf(t, err)
	return st
}

func searchQ(t *testing.T, p vector.VectorIndexProvider, scope contracts.Scope, q vector.Query) vector.SearchResult {
	t.Helper()
	res, err := p.Search(bg, scope, q)
	fatalIf(t, err)
	return res
}

func delChunks(t *testing.T, p vector.VectorIndexProvider, scope contracts.Scope, gen string, ids []string) int {
	t.Helper()
	n, err := p.DeleteChunks(bg, scope, gen, ids)
	fatalIf(t, err)
	return n
}

func delGen(t *testing.T, p vector.VectorIndexProvider, scope contracts.Scope, gen string) int {
	t.Helper()
	n, err := p.DeleteGeneration(bg, scope, gen)
	fatalIf(t, err)
	return n
}

func delSource(t *testing.T, p vector.VectorIndexProvider, scope contracts.Scope, source string) int {
	t.Helper()
	n, err := p.DeleteSource(bg, scope, source)
	fatalIf(t, err)
	return n
}

func delScope(t *testing.T, p vector.VectorIndexProvider, scope contracts.Scope) int {
	t.Helper()
	n, err := p.DeleteScope(bg, scope)
	fatalIf(t, err)
	return n
}

func reuseOf(t *testing.T, p vector.VectorIndexProvider, scope contracts.Scope, r vector.ReuseRequest) []string {
	t.Helper()
	res, err := p.Reuse(bg, scope, r)
	fatalIf(t, err)
	return res.Reused
}

func upsert(t *testing.T, p vector.VectorIndexProvider, scope contracts.Scope, generation, source string, items []vector.Item) vector.UpsertResult {
	t.Helper()
	res, err := mustUpsert(p, scope, generation, source, items)
	fatalIf(t, err)
	return res
}

func mustUpsert(p vector.VectorIndexProvider, scope contracts.Scope, generation, source string, items []vector.Item) (vector.UpsertResult, error) {
	return p.Upsert(bg, scope, vector.UpsertBatch{Generation: generation, SourceID: source, Model: Model, Items: items})
}

func search(t *testing.T, p vector.VectorIndexProvider, scope contracts.Scope, q []float32, k int, gens ...string) vector.SearchResult {
	t.Helper()
	return searchQ(t, p, scope, vector.Query{Vector: q, Model: Model, K: k, Generations: gens})
}

func hitIDs(r vector.SearchResult) []string {
	out := make([]string, len(r.Hits))
	for i, h := range r.Hits {
		out[i] = h.ChunkID
	}
	return out
}

// ---------------------------------------------------------------- subtests

// SEC-021: every method refuses a missing or malformed scope, and a refused
// call changes nothing.
func testScopeRequired(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	good := Scope("app", "tenA", "ws1")
	r := Rand(1)
	items := Items(good, "c", Clustered(r, 10, cfg.dim, 3, 0.4))
	upsert(t, p, good, "g1", "s1", items)

	noTenant, noApp, wildWS := good, good, good
	noTenant.TenantID = ""
	noApp.ApplicationID = ""
	wildWS.WorkspaceID = "*"
	for name, bad := range map[string]contracts.Scope{"zero": {}, "no tenant": noTenant, "no application": noApp, "wildcard workspace": wildWS} {
		t.Run(name, func(t *testing.T) {
			_, err := mustUpsert(p, bad, "g1", "s1", items)
			codeIs(t, err, contracts.ErrScopeMissing, contracts.ErrScopeViolation)
			_, err = p.Reuse(bg, bad, vector.ReuseRequest{Generation: "g2", SourceID: "s1", Model: Model, Refs: []vector.ReuseRef{{ChunkID: items[0].ChunkID, Key: items[0].Key}}})
			codeIs(t, err, contracts.ErrScopeMissing, contracts.ErrScopeViolation)
			_, err = p.Search(bg, bad, vector.Query{Vector: items[0].Vector, Model: Model, K: 3, Generations: []string{"g1"}})
			codeIs(t, err, contracts.ErrScopeMissing, contracts.ErrScopeViolation)
			_, err = p.DeleteChunks(bg, bad, "g1", []string{items[0].ChunkID})
			codeIs(t, err, contracts.ErrScopeMissing, contracts.ErrScopeViolation)
			_, err = p.DeleteGeneration(bg, bad, "g1")
			codeIs(t, err, contracts.ErrScopeMissing, contracts.ErrScopeViolation)
			_, err = p.DeleteSource(bg, bad, "s1")
			codeIs(t, err, contracts.ErrScopeMissing, contracts.ErrScopeViolation)
			_, err = p.DeleteScope(bg, bad)
			codeIs(t, err, contracts.ErrScopeMissing, contracts.ErrScopeViolation)
			_, err = p.Stats(bg, bad)
			codeIs(t, err, contracts.ErrScopeMissing, contracts.ErrScopeViolation)
		})
	}
	st := statsOf(t, p, good)
	if st.Vectors != len(items) {
		t.Fatalf("refused calls changed the index: %d vectors, want %d", st.Vectors, len(items))
	}
	if res := search(t, p, good, items[0].Vector, 3, "g1"); len(res.Hits) != 3 {
		t.Fatalf("index no longer answers after refused calls: %+v", res)
	}
}

// SEC-021: the scope is applied before similarity. Foreign scopes hold, under
// the same generation id and some of the same chunk ids, vectors that are
// closer to the query than anything in the caller's scope, one of them
// identical to it. A search that ranked a global candidate set and filtered
// afterwards would return nothing for K=10; the exact result must be 10 hits
// from the caller's scope, ranked as if the foreign scopes did not exist.
func testScopePrefilter(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	r := Rand(2)
	base := Scope("app", "tenA", "ws1")
	foreign := map[string]contracts.Scope{
		"other application": Scope("appX", "tenA", "ws1"),
		"other tenant":      Scope("app", "tenB", "ws1"),
		"other workspace":   Scope("app", "tenA", "ws2"),
	}
	q := RandomUnit(r, cfg.dim)

	own := Items(base, "own", Clustered(r, 40, cfg.dim, 5, 0.6))
	upsert(t, p, base, "g1", "s1", own)

	// Foreign vectors: the query itself and 39 near-copies. The "other
	// workspace" scope reuses the caller's chunk ids on purpose.
	for name, fs := range foreign {
		prefix := "foreign-" + strings.ReplaceAll(name, " ", "-") + "-"
		if name == "other workspace" {
			prefix = "own"
		}
		vecs := [][]float32{append([]float32(nil), q...)}
		for i := 0; i < 39; i++ {
			vecs = append(vecs, Perturb(r, q, 0.05))
		}
		upsert(t, p, fs, "g1", "s1", Items(fs, prefix, vecs))
	}

	res := search(t, p, base, q, 10, "g1")
	if len(res.Hits) != 10 {
		t.Fatalf("got %d hits, want 10 from the caller's scope: the scope was not applied before similarity", len(res.Hits))
	}
	ownIDs := map[string]bool{}
	for _, it := range own {
		ownIDs[it.ChunkID] = true
	}
	want := BruteForce(IDs(own), Vectors(own), q, 10)
	for _, h := range res.Hits {
		if !ownIDs[h.ChunkID] {
			t.Fatalf("hit %q is not a chunk of the caller's scope", h.ChunkID)
		}
		if h.Score > 0.99 {
			t.Fatalf("hit %q scores %.3f: an identical foreign vector leaked into the candidates", h.ChunkID, h.Score)
		}
	}
	if rec := Recall(want, hitIDs(res)); rec < 0.9 {
		t.Fatalf("recall against the caller's own brute force is %.2f; the foreign vectors disturbed the ranking", rec)
	}
	if res.Scanned > len(own) {
		t.Fatalf("scanned %d vectors for a scope that holds %d", res.Scanned, len(own))
	}

	// The reverse direction: a foreign caller never sees the base scope.
	for name, fs := range foreign {
		r2 := search(t, p, fs, own[0].Vector, 5, "g1")
		for _, h := range r2.Hits {
			if strings.HasPrefix(h.ChunkID, "own") && name != "other workspace" {
				t.Fatalf("%s: saw the caller's chunk %q", name, h.ChunkID)
			}
		}
	}
}

// Scopes whose identifiers contain delimiter characters must not collide when
// an implementation joins the levels into one key: ("a|b","c","d") and
// ("a","b|c","d") join to the same string with "|", yet are different scopes.
func testDelimiterCollisions(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	r := Rand(3)
	for _, d := range []string{"|", ":", "/", ",", ".", "-", "_", "#"} {
		base := Scope("app"+d+"one", "two", "three")
		others := []contracts.Scope{
			Scope("app", "one"+d+"two", "three"),
			Scope("app", "one", "two"+d+"three"),
			Scope("app"+d+"one"+d+"two", "three", "four"),
		}
		q := RandomUnit(r, cfg.dim)
		gen := "g" + d
		upsert(t, p, base, gen, "s1", Items(base, "base", Clustered(r, 6, cfg.dim, 2, 0.5)))
		for i, o := range others {
			upsert(t, p, o, gen, "s1", Items(o, fmt.Sprintf("other%d-", i), [][]float32{append([]float32(nil), q...)}))
		}
		res := search(t, p, base, q, 10, gen)
		for _, h := range res.Hits {
			if strings.HasPrefix(h.ChunkID, "other") {
				t.Fatalf("delimiter %q: scope %v leaked chunk %q into %v", d, others, h.ChunkID, base)
			}
		}
		if len(res.Hits) != 6 {
			t.Fatalf("delimiter %q: got %d hits, want the scope's own 6", d, len(res.Hits))
		}
		for i, o := range others {
			if got := search(t, p, o, q, 10, gen); len(got.Hits) != 1 || got.Hits[0].ChunkID != fmt.Sprintf("other%d-0000", i) {
				t.Fatalf("delimiter %q: scope %v sees %v, want only its own chunk", d, o, hitIDs(got))
			}
		}
	}
}

func testGenerations(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	r := Rand(4)
	scope := Scope("app", "tenA", "ws1")
	a := Items(scope, "a", Clustered(r, 20, cfg.dim, 3, 0.5))
	b := Items(scope, "b", Clustered(r, 20, cfg.dim, 3, 0.5))
	upsert(t, p, scope, "gen-a", "src-a", a)
	upsert(t, p, scope, "gen-b", "src-b", b)

	only := search(t, p, scope, a[0].Vector, 40, "gen-a")
	if len(only.Hits) != 20 {
		t.Fatalf("searching gen-a returned %d hits, want 20", len(only.Hits))
	}
	for _, h := range only.Hits {
		if h.GenerationID != "gen-a" || h.SourceID != "src-a" || !strings.HasPrefix(h.ChunkID, "a") {
			t.Fatalf("hit %+v does not belong to gen-a", h)
		}
	}
	both := search(t, p, scope, a[0].Vector, 40, "gen-a", "gen-b", "gen-a")
	if len(both.Hits) != 40 {
		t.Fatalf("searching both returned %d hits, want 40 (a repeated generation counts once)", len(both.Hits))
	}
	if both.Hits[0].ChunkID != a[0].ChunkID {
		t.Fatalf("best hit is %q, want the query's own chunk %q", both.Hits[0].ChunkID, a[0].ChunkID)
	}
	for _, h := range both.Hits {
		wantSrc := map[string]string{"gen-a": "src-a", "gen-b": "src-b"}[h.GenerationID]
		if h.SourceID != wantSrc {
			t.Fatalf("hit %+v carries the wrong source", h)
		}
	}

	// An unknown generation contributes nothing and is reported.
	res := search(t, p, scope, a[0].Vector, 5, "gen-a", "gen-nope")
	if len(res.Skipped) != 1 || res.Skipped[0].Generation != "gen-nope" || res.Skipped[0].Reason != vector.SkipUnknownGeneration {
		t.Fatalf("skipped = %+v, want gen-nope reported as unknown", res.Skipped)
	}
	// No generation named: nothing is returned, never everything.
	none := searchQ(t, p, scope, vector.Query{Vector: a[0].Vector, Model: Model, K: 5})
	if len(none.Hits) != 0 {
		t.Fatalf("an empty generation list returned %d hits, want none", len(none.Hits))
	}
}

func testModelAndDimension(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	r := Rand(5)
	scope := Scope("app", "tenA", "ws1")
	items := Items(scope, "c", Clustered(r, 10, cfg.dim, 2, 0.5))
	upsert(t, p, scope, "g1", "s1", items)

	// A query from another model is never scored against these vectors.
	res := searchQ(t, p, scope, vector.Query{Vector: items[0].Vector, Model: "other-model", K: 3, Generations: []string{"g1"}})
	if len(res.Hits) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Reason != vector.SkipModelMismatch {
		t.Fatalf("another model's query: %+v, want no hits and a model_mismatch skip", res)
	}
	// A query of another dimension likewise.
	short := items[0].Vector[:cfg.dim-1]
	res = searchQ(t, p, scope, vector.Query{Vector: short, Model: Model, K: 3, Generations: []string{"g1"}})
	if len(res.Hits) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Reason != vector.SkipDimMismatch {
		t.Fatalf("a shorter query: %+v, want no hits and a dim_mismatch skip", res)
	}
	// A generation is fixed to its source, model and dimension.
	_, err := p.Upsert(bg, scope, vector.UpsertBatch{Generation: "g1", SourceID: "s1", Model: "other-model", Items: Items(scope, "m", [][]float32{items[0].Vector})})
	codeIs(t, err, contracts.ErrConflict)
	_, err = p.Upsert(bg, scope, vector.UpsertBatch{Generation: "g1", SourceID: "other-source", Model: Model, Items: Items(scope, "m", [][]float32{items[0].Vector})})
	codeIs(t, err, contracts.ErrConflict)
	_, err = p.Upsert(bg, scope, vector.UpsertBatch{Generation: "g1", SourceID: "s1", Model: Model, Items: Items(scope, "m", [][]float32{items[0].Vector[:cfg.dim-1]})})
	codeIs(t, err, contracts.ErrConflict, contracts.ErrValidationFailed)
	if st := statsOf(t, p, scope); st.Vectors != len(items) {
		t.Fatalf("a refused upsert changed the index: %d vectors", st.Vectors)
	}
}

func testUpsert(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	r := Rand(6)
	scope := Scope("app", "tenA", "ws1")
	items := Items(scope, "c", Clustered(r, 30, cfg.dim, 4, 0.5))
	if res := upsert(t, p, scope, "g1", "s1", items); res.Inserted != 30 || res.Replaced != 0 {
		t.Fatalf("first upsert = %+v, want 30 inserted", res)
	}
	if res := upsert(t, p, scope, "g1", "s1", items); res.Inserted != 0 || res.Replaced != 30 {
		t.Fatalf("repeated upsert = %+v, want 30 replaced (idempotent)", res)
	}
	if st := statsOf(t, p, scope); st.Vectors != 30 {
		t.Fatalf("stats after a repeated upsert: %d vectors, want 30", st.Vectors)
	}

	// Replace one chunk's vector: the old direction no longer finds it, the new one does.
	target := items[3]
	oldVec := target.Vector
	newVec := RandomUnit(r, cfg.dim)
	upsert(t, p, scope, "g1", "s1", []vector.Item{{ChunkID: target.ChunkID, Key: target.Key, Vector: newVec}})
	if got := search(t, p, scope, newVec, 1, "g1"); len(got.Hits) != 1 || got.Hits[0].ChunkID != target.ChunkID {
		t.Fatalf("after replacing, the new vector's best hit is %v, want %q", hitIDs(got), target.ChunkID)
	}
	got := search(t, p, scope, oldVec, 5, "g1")
	for _, h := range got.Hits {
		if h.ChunkID == target.ChunkID && h.Score > 0.9 {
			t.Fatalf("the replaced vector is still served: %+v", h)
		}
	}
	if st := statsOf(t, p, scope); st.Vectors != 30 {
		t.Fatalf("replace changed the count: %d", st.Vectors)
	}
}

// A deleted vector is gone from every later search and from the stats, in every
// way it can be deleted, and a delete never touches another scope.
func testDeletes(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	r := Rand(7)
	scope := Scope("app", "tenA", "ws1")
	other := Scope("app", "tenB", "ws1")
	mk := func(sc contracts.Scope, prefix string, n int) []vector.Item {
		return Items(sc, prefix, Clustered(r, n, cfg.dim, 3, 0.5))
	}
	a1 := mk(scope, "a", 20)
	a2 := mk(scope, "b", 10)
	other1 := mk(other, "a", 20)
	upsert(t, p, scope, "g1", "s1", a1)
	upsert(t, p, scope, "g2", "s1", a2) // a second generation of the same source
	upsert(t, p, scope, "g3", "s2", mk(scope, "c", 5))
	upsert(t, p, other, "g1", "s1", other1)

	// Chunks.
	gone := []string{a1[0].ChunkID, a1[1].ChunkID, "never-existed"}
	if n := delChunks(t, p, scope, "g1", gone); n != 2 {
		t.Fatalf("DeleteChunks removed %d, want 2", n)
	}
	res := search(t, p, scope, a1[0].Vector, 30, "g1")
	for _, h := range res.Hits {
		if h.ChunkID == a1[0].ChunkID || h.ChunkID == a1[1].ChunkID {
			t.Fatalf("deleted chunk %q is still served", h.ChunkID)
		}
	}
	if len(res.Hits) != 18 {
		t.Fatalf("after deleting 2 of 20, %d are served, want 18", len(res.Hits))
	}
	if n := delChunks(t, p, scope, "g1", gone); n != 0 {
		t.Fatalf("repeating DeleteChunks removed %d, want 0", n)
	}
	if got := search(t, p, other, other1[0].Vector, 1, "g1"); len(got.Hits) != 1 || got.Hits[0].ChunkID != other1[0].ChunkID {
		t.Fatalf("deleting in one scope disturbed another: %+v", got)
	}

	// A generation.
	if n := delGen(t, p, scope, "g2"); n != 10 {
		t.Fatalf("DeleteGeneration removed %d, want 10", n)
	}
	if got := search(t, p, scope, a2[0].Vector, 10, "g2"); len(got.Hits) != 0 || len(got.Skipped) != 1 {
		t.Fatalf("a deleted generation is still served: %+v", got)
	}
	if n := delGen(t, p, scope, "g2"); n != 0 {
		t.Fatalf("deleting a missing generation removed %d", n)
	}

	// A source: every generation of it.
	if n := delSource(t, p, scope, "s1"); n != 18 {
		t.Fatalf("DeleteSource removed %d, want the 18 left in g1", n)
	}
	if got := search(t, p, scope, a1[5].Vector, 10, "g1", "g3"); len(got.Hits) != 5 {
		t.Fatalf("after DeleteSource(s1) %d hits remain, want only s2's 5: %+v", len(got.Hits), got)
	}

	// The whole workspace.
	if n := delScope(t, p, scope); n != 5 {
		t.Fatalf("DeleteScope removed %d, want 5", n)
	}
	if st := statsOf(t, p, scope); st.Vectors != 0 || len(st.Generations) != 0 {
		t.Fatalf("the workspace still holds %+v", st)
	}
	if got := search(t, p, other, other1[2].Vector, 3, "g1"); len(got.Hits) != 3 {
		t.Fatalf("DeleteScope removed another scope's vectors: %+v", got)
	}
	// The slate is clean: the same chunk ids can be stored again.
	upsert(t, p, scope, "g1", "s1", a1[:3])
	if st := statsOf(t, p, scope); st.Vectors != 3 {
		t.Fatalf("re-adding after a purge: %d vectors", st.Vectors)
	}
}

// SEC-023: reuse is keyed by scope, model and the exact input, and only ever
// finds the same scope's vectors, so identical content in another tenant causes
// no observable difference.
func testReuse(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	r := Rand(8)
	a := Scope("app", "tenA", "ws1")
	b := Scope("app", "tenB", "ws1")
	vecs := Clustered(r, 12, cfg.dim, 3, 0.5)
	items := Items(a, "c", vecs)
	upsert(t, p, a, "g1", "s1", items)

	refs := func(items []vector.Item) []vector.ReuseRef {
		out := make([]vector.ReuseRef, len(items))
		for i, it := range items {
			out[i] = vector.ReuseRef{ChunkID: it.ChunkID, Key: it.Key}
		}
		return out
	}
	reuse := func(sc contracts.Scope, gen, model string, refs []vector.ReuseRef) []string {
		t.Helper()
		return reuseOf(t, p, sc, vector.ReuseRequest{Generation: gen, SourceID: "s1", Model: model, Refs: refs})
	}

	// Another scope, even handed the exact keys scope A computed, reuses nothing.
	if got := reuse(b, "g1", Model, refs(items)); len(got) != 0 {
		t.Fatalf("scope B reused %d of scope A's vectors by guessing their keys", len(got))
	}
	// ...and neither does it with its own keys for identical content.
	if got := reuse(b, "g1", Model, refs(Items(b, "c", vecs))); len(got) != 0 {
		t.Fatalf("scope B reused %d vectors for identical content: reuse crossed a tenant", len(got))
	}
	if st := statsOf(t, p, b); st.Vectors != 0 {
		t.Fatalf("a failed reuse left %d vectors in scope B", st.Vectors)
	}
	if got := search(t, p, b, vecs[0], 5, "g1"); len(got.Hits) != 0 {
		t.Fatalf("scope B can see %d hits after trying to reuse: %+v", len(got.Hits), got)
	}

	if !p.Info().Reuses {
		return // reuse is optional; everything above held for a provider that reuses nothing
	}
	// Same scope, same model and input: the vectors are copied into the new generation.
	if got := reuse(a, "g2", Model, refs(items)); len(got) != len(items) {
		t.Fatalf("same-scope reuse returned %d of %d", len(got), len(items))
	}
	if res := search(t, p, a, vecs[4], 1, "g2"); len(res.Hits) != 1 || res.Hits[0].ChunkID != items[4].ChunkID {
		t.Fatalf("a reused vector is not searchable in the new generation: %+v", res)
	}
	// Another model never reuses.
	if got := reuse(a, "g3", "another-model", refs(items)); len(got) != 0 {
		t.Fatalf("a different model reused %d vectors", len(got))
	}
	// A key that matches nothing reuses nothing, and says so.
	unknown, _ := vector.NewInputKey(a, Model, "text nobody embedded")
	if got := reuse(a, "g2", Model, []vector.ReuseRef{{ChunkID: "zzz", Key: unknown}}); len(got) != 0 {
		t.Fatalf("an unknown key reused %v", got)
	}
	// A deleted chunk leaves no reusable embedding behind (retention follows the row).
	delGen(t, p, a, "g1")
	delGen(t, p, a, "g2")
	if got := reuse(a, "g4", Model, refs(items)); len(got) != 0 {
		t.Fatalf("%d vectors were reused after every generation holding them was deleted", len(got))
	}
}

func testBounds(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	r := Rand(9)
	scope := Scope("app", "tenA", "ws1")
	items := Items(scope, "c", Clustered(r, 5, cfg.dim, 2, 0.5))
	upsert(t, p, scope, "g1", "s1", items)
	q := items[0].Vector
	nan := append([]float32(nil), q...)
	nan[1] = float32(math.NaN())
	inf := append([]float32(nil), q...)
	inf[2] = float32(math.Inf(1))

	bad := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		codeIs(t, err, contracts.ErrValidationFailed)
	}
	srch := func(v []float32, k int, gens ...string) error {
		_, err := p.Search(bg, scope, vector.Query{Vector: v, Model: Model, K: k, Generations: gens})
		return err
	}
	bad("k = 0", srch(q, 0, "g1"))
	bad("k above the maximum", srch(q, vector.MaxK+1, "g1"))
	bad("empty vector", srch(nil, 3, "g1"))
	bad("NaN query", srch(nan, 3, "g1"))
	bad("Inf query", srch(inf, 3, "g1"))
	bad("zero query", srch(make([]float32, cfg.dim), 3, "g1"))
	tooMany := make([]string, vector.MaxGenerations+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("g%d", i)
	}
	bad("too many generations", srch(q, 3, tooMany...))
	_, err := p.Search(bg, scope, vector.Query{Vector: q, K: 3, Generations: []string{"g1"}})
	bad("no model", err)

	up := func(name string, mutate func(*vector.UpsertBatch)) {
		t.Helper()
		b := vector.UpsertBatch{Generation: "g9", SourceID: "s9", Model: Model, Items: Items(scope, "x", [][]float32{q})}
		mutate(&b)
		_, err := p.Upsert(bg, scope, b)
		bad(name, err)
	}
	up("no items", func(b *vector.UpsertBatch) { b.Items = nil })
	up("NaN vector", func(b *vector.UpsertBatch) { b.Items[0].Vector = nan })
	up("zero vector", func(b *vector.UpsertBatch) { b.Items[0].Vector = make([]float32, cfg.dim) })
	up("missing input key", func(b *vector.UpsertBatch) { b.Items[0].Key = vector.InputKey{} })
	up("empty chunk id", func(b *vector.UpsertBatch) { b.Items[0].ChunkID = "" })
	up("empty generation", func(b *vector.UpsertBatch) { b.Generation = "" })
	up("empty source", func(b *vector.UpsertBatch) { b.SourceID = "" })
	up("duplicate chunk ids", func(b *vector.UpsertBatch) { b.Items = append(b.Items, b.Items[0]) })
	up("oversized batch", func(b *vector.UpsertBatch) {
		for len(b.Items) <= vector.MaxBatch {
			it := b.Items[0]
			it.ChunkID = fmt.Sprintf("x%d", len(b.Items))
			b.Items = append(b.Items, it)
		}
	})
	up("mixed dimensions", func(b *vector.UpsertBatch) {
		b.Items = append(b.Items, vector.Item{ChunkID: "y", Key: b.Items[0].Key, Vector: q[:cfg.dim-1]})
	})
	_, err = p.DeleteChunks(bg, scope, "g1", nil)
	bad("delete without ids", err)
	if st := statsOf(t, p, scope); st.Vectors != len(items) || len(st.Generations) != 1 {
		t.Fatalf("a refused request changed the index: %+v", st)
	}
}

func testOrdering(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	r := Rand(10)
	scope := Scope("app", "tenA", "ws1")
	v := RandomUnit(r, cfg.dim)
	// Identical vectors tie exactly; chunk ids decide.
	var vecs [][]float32
	for i := 0; i < 8; i++ {
		vecs = append(vecs, append([]float32(nil), v...))
	}
	items := Items(scope, "t", vecs)
	// Store them in a scrambled order.
	scr := append([]vector.Item(nil), items[4:]...)
	scr = append(scr, items[:4]...)
	upsert(t, p, scope, "g1", "s1", scr)
	first := hitIDs(search(t, p, scope, v, 5, "g1"))
	want := IDs(items[:5])
	if p.Info().StableTies {
		if fmt.Sprint(first) != fmt.Sprint(want) {
			t.Fatalf("tied hits = %v, want %v (chunk id ascending)", first, want)
		}
	} else if len(first) != 5 {
		t.Fatalf("got %d hits, want 5 of the 8 tied chunks", len(first))
	}
	for i := 0; i < 3; i++ {
		if again := hitIDs(search(t, p, scope, v, 5, "g1")); fmt.Sprint(again) != fmt.Sprint(first) {
			t.Fatalf("a repeated search changed its answer: %v then %v", first, again)
		}
	}
	res := search(t, p, scope, v, 8, "g1")
	for i := 1; i < len(res.Hits); i++ {
		if res.Hits[i].Score > res.Hits[i-1].Score {
			t.Fatalf("hits are not ordered best first: %+v", res.Hits)
		}
	}
}

func testStats(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	r := Rand(11)
	a := Scope("app", "tenA", "ws1")
	b := Scope("app", "tenB", "ws1")
	upsert(t, p, a, "g1", "s1", Items(a, "a", Clustered(r, 7, cfg.dim, 2, 0.5)))
	upsert(t, p, a, "g2", "s2", Items(a, "b", Clustered(r, 3, cfg.dim, 2, 0.5)))
	upsert(t, p, b, "g1", "s1", Items(b, "a", Clustered(r, 11, cfg.dim, 2, 0.5)))

	st := statsOf(t, p, a)
	if st.Vectors != 10 || len(st.Generations) != 2 {
		t.Fatalf("stats for A = %+v, want 10 vectors in 2 generations and nothing of B", st)
	}
	byGen := map[string]vector.GenerationStats{}
	for _, g := range st.Generations {
		byGen[g.Generation] = g
	}
	if g := byGen["g1"]; g.Vectors != 7 || g.SourceID != "s1" || g.Model != Model || g.Dim != cfg.dim {
		t.Fatalf("generation g1 stats = %+v", g)
	}
	if st.Name == "" || st.Metric == "" {
		t.Fatalf("stats carry no implementation info: %+v", st.Info)
	}
	if empty := statsOf(t, p, Scope("app", "tenC", "ws1")); empty.Vectors != 0 || len(empty.Generations) != 0 {
		t.Fatalf("an empty scope reports %+v", empty)
	}
}

// Recall@10 against an independent float64 brute force on clustered vectors,
// the shape real embeddings have. An exact provider must clear the floor; an
// approximate one reports lower and the suite records what it got.
func testRecall(t *testing.T, p vector.VectorIndexProvider, cfg settings) {
	r := Rand(12)
	scope := Scope("app", "tenA", "ws1")
	vecs := Clustered(r, cfg.corpus, cfg.dim, 24, 0.55)
	items := Items(scope, "c", vecs)
	for i := 0; i < len(items); i += vector.MaxBatch {
		upsert(t, p, scope, "g1", "s1", items[i:min(i+vector.MaxBatch, len(items))])
	}
	ids := IDs(items)
	var sum float64
	const queries = 60
	for i := 0; i < queries; i++ {
		q := Perturb(r, vecs[r.IntN(len(vecs))], 0.3)
		got := hitIDs(search(t, p, scope, q, 10, "g1"))
		sum += Recall(BruteForce(ids, vecs, q, 10), got)
	}
	recall := sum / queries
	t.Logf("%s: recall@10 = %.4f over %d queries on %d clustered vectors of dimension %d", p.Info().Name, recall, queries, cfg.corpus, cfg.dim)
	if p.Info().Exact && recall < cfg.minRecall {
		t.Fatalf("recall@10 = %.4f, below the %.2f an exact provider must reach", recall, cfg.minRecall)
	}
	if recall < 0.5 {
		t.Fatalf("recall@10 = %.4f: even an approximate index must do far better than this", recall)
	}
}
