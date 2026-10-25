package upgrade

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/rag/vector"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// vectorEnv is a database with the vector module's tables, three real vectors in
// one generation, and a quarantine beside it.
type vectorEnv struct {
	t     *testing.T
	db    *store.DB
	flat  *vector.Flat
	q     *Quarantine
	clk   *clock.Fake
	index DerivedIndex
}

func newVectorEnv(t *testing.T, rebuild VectorRebuild) *vectorEnv {
	t.Helper()
	e := &vectorEnv{t: t, clk: clock.NewFake(t0)}
	e.db = storetest.Open(t, nil) // store.Default holds the migrations of every imported module, vector's included
	var err error
	if e.flat, err = vector.NewFlat(e.db, vector.FlatOptions{Clock: e.clk}); err != nil {
		t.Fatal(err)
	}
	e.q = NewQuarantine(t.TempDir(), e.clk)
	e.index = VectorDerived(rebuild)(e.db)[0]
	e.embed()
	return e
}

// embed stores three vectors in generation gen1: what the embedding job does.
func (e *vectorEnv) embed() {
	e.t.Helper()
	sc := storetest.Scope("app", "tenant", "ws")
	var items []vector.Item
	for i := 0; i < 3; i++ {
		key, err := vector.NewInputKey(sc, "model-4", fmt.Sprintf("chunk %d", i))
		if err != nil {
			e.t.Fatal(err)
		}
		items = append(items, vector.Item{ChunkID: fmt.Sprintf("chk_%d", i), Key: key, Vector: []float32{1, float32(i), 2, 3}})
	}
	if _, err := e.flat.Upsert(context.Background(), sc, vector.UpsertBatch{Generation: "gen1", SourceID: "src1", Model: "model-4", Items: items}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *vectorEnv) exec(q string) {
	e.t.Helper()
	if _, err := e.db.ExecContext(ctx, q); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
}

func (e *vectorEnv) count(table string) int { return count(e.t, e.db, `SELECT count(*) FROM `+table) }

// The vector index is derived data: damage to it is isolated (recorded and
// emptied, so nothing is served from it) and then waits for the chunks to be
// embedded again, because the upgrade has no embedder. The record is what tells
// retrieval to label its results; it must not go while the index is empty.
func TestVectorIndexIsolatedAndHeldUntilReEmbedded(t *testing.T) {
	for name, damage := range map[string]string{
		"a generation that counts a vector it does not have": `UPDATE vector_generations SET vectors = vectors + 1`,
		"a vector shorter than its dimension":                `UPDATE vector_rows SET code = substr(code, 1, 2) WHERE chunk_id = 'chk_1'`,
		"a vector with no norm":                              `UPDATE vector_rows SET inv_norm = 0 WHERE chunk_id = 'chk_2'`,
	} {
		t.Run(name, func(t *testing.T) {
			e := newVectorEnv(t, nil)
			if err := e.index.Check(ctx); err != nil {
				t.Fatalf("a healthy vector index: %v", err)
			}
			if d, err := IsolateDamaged(ctx, e.q, []DerivedIndex{e.index}); err != nil || len(d) != 0 || e.q.Has(VectorIndexName) {
				t.Fatalf("a healthy vector index was isolated: %v, %v", d, err)
			}
			var revBefore int
			if err := e.db.QueryRowContext(ctx, `SELECT rev FROM vector_generations`).Scan(&revBefore); err != nil {
				t.Fatal(err)
			}

			e.exec(damage)
			if err := e.index.Check(ctx); !errors.Is(err, ErrDamaged) {
				t.Fatalf("Check on %s = %v, want ErrDamaged", name, err)
			}
			rep, err := RepairDerived(ctx, e.q, []DerivedIndex{e.index})
			if err != nil {
				t.Fatalf("RepairDerived: %v", err)
			}
			// Isolated, and held: nothing re-embedded, so nothing released.
			if len(rep.Damaged) != 1 || rep.Damaged[0] != VectorIndexName || len(rep.Rebuilt) != 0 || len(rep.Pending) != 1 || rep.Pending[0] != VectorIndexName {
				t.Errorf("report = %+v, want %s damaged and pending, nothing rebuilt", rep, VectorIndexName)
			}
			if !e.q.Has(VectorIndexName) {
				t.Fatal("the vector index left the quarantine although it is empty: retrieval would find nothing without saying so")
			}
			if n := e.count("vector_rows"); n != 0 {
				t.Errorf("%d vectors are still served from an isolated index", n)
			}
			var vectors, rev int
			if err := e.db.QueryRowContext(ctx, `SELECT vectors, rev FROM vector_generations`).Scan(&vectors, &rev); err != nil {
				t.Fatal(err)
			}
			if vectors != 0 || rev <= revBefore {
				t.Errorf("generation: %d vectors, rev %d (was %d): the count must be zero and the revision bumped so no cache or reuse hands out dropped vectors", vectors, rev, revBefore)
			}
			// Isolating is idempotent and the record survives repeated repairs.
			if rep, err := RepairDerived(ctx, e.q, []DerivedIndex{e.index}); err != nil || len(rep.Rebuilt) != 0 || !e.q.Has(VectorIndexName) {
				t.Errorf("a second repair: %+v, %v, held %v", rep, err, e.q.Has(VectorIndexName))
			}

			// Whoever re-embeds releases it.
			e.embedAgain()
			if err := e.q.Clear(VectorIndexName); err != nil {
				t.Fatal(err)
			}
			if err := e.index.Check(ctx); err != nil {
				t.Errorf("the re-embedded index fails its check: %v", err)
			}
		})
	}
}

// embedAgain is what the re-embedding job does after an isolation: the generation
// still exists, with its vectors dropped, and the chunks are embedded again.
func (e *vectorEnv) embedAgain() { e.embed() }

// With a VectorRebuild that really re-embeds, the repair finishes by itself; one
// that fails keeps the record and says the damage is structural.
func TestVectorRebuildHookReleasesTheRecordOnlyWhenItSucceeds(t *testing.T) {
	var e *vectorEnv
	calls := 0
	e = newVectorEnv(t, func(context.Context, *store.DB) error { calls++; e.embed(); return nil })
	e.exec(`UPDATE vector_generations SET vectors = vectors + 5`)
	rep, err := RepairDerived(ctx, e.q, []DerivedIndex{e.index})
	if err != nil || len(rep.Rebuilt) != 1 || len(rep.Pending) != 0 || calls != 1 {
		t.Fatalf("RepairDerived = %+v, %v (rebuild called %d times)", rep, err, calls)
	}
	if e.q.Has(VectorIndexName) || e.count("vector_rows") != 3 {
		t.Errorf("after the rebuild: held %v, %d vectors", e.q.Has(VectorIndexName), e.count("vector_rows"))
	}

	failing := newVectorEnv(t, func(context.Context, *store.DB) error { return errors.New("embedder is down") })
	failing.exec(`UPDATE vector_generations SET vectors = vectors + 5`)
	rep, err = RepairDerived(ctx, failing.q, []DerivedIndex{failing.index})
	if err == nil || len(rep.Rebuilt) != 0 || !failing.q.Has(VectorIndexName) {
		t.Errorf("a failed re-embedding: %+v, %v, held %v", rep, err, failing.q.Has(VectorIndexName))
	}
}

// A database without the vector module has no vector index: the upgrade's health
// probe over such a database (every fixture of this package) finds nothing to do,
// and a stale record is cleared like the one of a dropped FTS table.
func TestVectorIndexAbsentIsNothingToRepair(t *testing.T) {
	e := newNotesEnv(t)
	ix := VectorDerived(nil)(e.db)[0]
	if err := ix.Check(ctx); err != nil {
		t.Errorf("Check without vector tables: %v", err)
	}
	if err := ix.Isolate(ctx); err != nil {
		t.Errorf("Isolate without vector tables: %v", err)
	}
	q := NewQuarantine(e.dir, e.clk)
	if err := q.Add(VectorIndexName, "stale"); err != nil {
		t.Fatal(err)
	}
	rep, err := RepairDerived(ctx, q, []DerivedIndex{ix})
	if err != nil || len(rep.Rebuilt) != 1 || q.Has(VectorIndexName) {
		t.Errorf("a stale record: %+v, %v, held %v", rep, err, q.Has(VectorIndexName))
	}
}

// The vector index is wired into the upgrader without the caller naming it, and a
// caller's own index of the same name replaces it.
func TestVectorIndexIsWiredIntoExtraDerived(t *testing.T) {
	f := newFixture(t)
	u := f.upgrader(regV1(t))
	db := f.openWith(regV1(t))
	defer db.Close()
	idx, err := u.derived(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	got := names(idx)
	if len(got) != 2 || got[0] != "notes_fts" || got[1] != VectorIndexName {
		t.Errorf("derived indexes = %v, want notes_fts and %s", got, VectorIndexName)
	}

	own := &namedIndex{name: VectorIndexName}
	u2 := f.upgrader(regV1(t), func(c *Config) {
		c.ExtraDerived = func(*store.DB) []DerivedIndex { return []DerivedIndex{own, &namedIndex{name: "extra_idx"}} }
	})
	idx, err = u2.derived(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(idx); len(got) != 3 || got[0] != "notes_fts" || got[1] != VectorIndexName || got[2] != "extra_idx" {
		t.Errorf("derived indexes = %v", got)
	}
	var found bool
	for _, ix := range idx {
		if ix == DerivedIndex(own) {
			found = true
		}
	}
	if !found {
		t.Error("the caller's index did not replace the built-in vector index of the same name")
	}
}

type namedIndex struct{ name string }

func (n *namedIndex) Name() string                { return n.name }
func (n *namedIndex) Check(context.Context) error { return nil }
func (n *namedIndex) Isolate(context.Context) error {
	return nil
}
func (n *namedIndex) Rebuild(context.Context) error { return nil }

// Has fails closed: a record that cannot be looked up is reported as isolated,
// because an index that cannot be shown to be healthy must be labelled, not
// served from in silence.
func TestQuarantineHasFailsClosed(t *testing.T) {
	dir := t.TempDir()
	q := NewQuarantine(dir, clock.NewFake(t0))
	if q.Has("notes_fts") {
		t.Error("an index with no record is reported isolated")
	}
	if err := q.Add("notes_fts", "x"); err != nil {
		t.Fatal(err)
	}
	if !q.Has("notes_fts") {
		t.Error("a recorded index is not reported isolated")
	}
	if q.Has("bad name!") {
		t.Error("a name that cannot be recorded is reported isolated")
	}
	// A lookup that fails for any reason other than "no such record" (here: a path the operating system
	// refuses outright) is not evidence of health.
	if bq := NewQuarantine("bad\x00dir", clock.NewFake(t0)); !bq.Has("notes_fts") {
		t.Error("a quarantine that cannot be read reports an index as healthy")
	}
}

// A check that cries wolf is worse than none: a failed Check isolates the index,
// which drops every vector of every scope. So the check must stay silent through
// everything the flat provider legitimately does to its tables: first upserts,
// replacements, reuse across generations, chunk deletes, a generation deleted
// halfway, and the deletes of a whole source and a whole scope.
func TestVectorCheckHasNoFalseAlarmsOnLegitimateOperations(t *testing.T) {
	e := newVectorEnv(t, nil)
	sc := storetest.Scope("app", "tenant", "ws")
	other := storetest.Scope("app", "tenant2", "ws")
	step := func(what string) {
		t.Helper()
		if err := e.index.Check(ctx); err != nil {
			t.Fatalf("after %s the check reports damage: %v", what, err)
		}
	}
	key := func(s contracts.Scope, text string) vector.InputKey {
		k, err := vector.NewInputKey(s, "model-4", text)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	item := func(s contracts.Scope, i int, v float32) vector.Item {
		return vector.Item{ChunkID: fmt.Sprintf("chk_%d", i), Key: key(s, fmt.Sprintf("chunk %d", i)), Vector: []float32{1, v, 2, 3}}
	}
	step("the first upsert (newVectorEnv embedded three chunks into gen1)")

	// Replacement: the same chunks with new vectors.
	if res, err := e.flat.Upsert(ctx, sc, vector.UpsertBatch{Generation: "gen1", SourceID: "src1", Model: "model-4", Items: []vector.Item{item(sc, 0, 9), item(sc, 1, 8)}}); err != nil || res.Replaced != 2 {
		t.Fatalf("replace: %+v, %v", res, err)
	}
	step("re-upserting two chunks")

	// A second generation of the same source, one new chunk, two reused from gen1.
	if _, err := e.flat.Upsert(ctx, sc, vector.UpsertBatch{Generation: "gen2", SourceID: "src1", Model: "model-4", Items: []vector.Item{item(sc, 9, 4)}}); err != nil {
		t.Fatal(err)
	}
	step("a second generation")
	rr, err := e.flat.Reuse(ctx, sc, vector.ReuseRequest{Generation: "gen2", SourceID: "src1", Model: "model-4",
		Refs: []vector.ReuseRef{{ChunkID: "chk_1", Key: key(sc, "chunk 1")}, {ChunkID: "chk_2", Key: key(sc, "chunk 2")}}})
	if err != nil || len(rr.Reused) != 2 {
		t.Fatalf("reuse: %+v, %v", rr, err)
	}
	step("reusing two vectors in the second generation")

	// Another scope in the same table.
	if _, err := e.flat.Upsert(ctx, other, vector.UpsertBatch{Generation: "genB", SourceID: "srcB", Model: "model-4", Items: []vector.Item{item(other, 0, 3), item(other, 1, 5)}}); err != nil {
		t.Fatal(err)
	}
	step("another scope's upsert")

	if n, err := e.flat.DeleteChunks(ctx, sc, "gen1", []string{"chk_0", "chk_missing"}); err != nil || n != 1 {
		t.Fatalf("DeleteChunks: %d, %v", n, err)
	}
	step("deleting a chunk")

	// A deletion interrupted between its two phases: the generation is hidden, part of its rows are gone.
	e.exec(`UPDATE vector_generations SET state = 'dropping', rev = rev + 1 WHERE generation_id = 'gen2'`)
	e.exec(`DELETE FROM vector_rows WHERE generation_id = 'gen2' AND chunk_id = 'chk_9'`)
	step("a generation deleted halfway")
	if n, err := e.flat.ResumeDrops(ctx); err != nil || n != 1 {
		t.Fatalf("ResumeDrops: %d, %v", n, err)
	}
	step("finishing the interrupted delete")

	if _, err := e.flat.DeleteSource(ctx, sc, "src1"); err != nil {
		t.Fatal(err)
	}
	step("deleting a source")
	if _, err := e.flat.DeleteScope(ctx, other); err != nil {
		t.Fatal(err)
	}
	step("deleting a scope")
	if n := e.count("vector_rows") + e.count("vector_generations"); n != 0 {
		t.Errorf("%d rows left after every delete: the scenario did not exercise what it says", n)
	}
}
