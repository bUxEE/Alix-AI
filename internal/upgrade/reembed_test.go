package upgrade

import (
	"context"
	"errors"
	"testing"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// isolate is what the repair does to a damaged vector index.
func (e *vectorEnv) isolate() {
	e.t.Helper()
	if err := e.q.Add(VectorIndexName, "test: damaged"); err != nil {
		e.t.Fatal(err)
	}
	if err := e.index.Isolate(ctx); err != nil {
		e.t.Fatal(err)
	}
}

// The record of the vector index goes only after a re-embedding that returned and an
// index that passes its check, and no other record goes with it.
func TestReleaseVectorReleasesOnlyAfterAVerifiedReEmbed(t *testing.T) {
	var e *vectorEnv
	calls := 0
	embed := func(context.Context, *store.DB) error { calls++; e.embed(); return nil }
	e = newVectorEnv(t, embed)
	for _, name := range []string{"ragindex_fts", "history_fts"} {
		if err := e.q.Add(name, "test: isolated"); err != nil {
			t.Fatal(err)
		}
	}
	held := func(want ...bool) {
		t.Helper()
		for i, name := range []string{VectorIndexName, "ragindex_fts", "history_fts"} {
			if got := e.q.Has(name); got != want[i] {
				t.Errorf("record %s held = %v, want %v", name, got, want[i])
			}
		}
	}

	// Not isolated: nothing to do, and the rebuild is not called.
	if released, err := ReleaseVector(ctx, e.q, e.db, embed); released || err != nil || calls != 0 {
		t.Fatalf("a vector index that is not isolated: released %v, %v, rebuild called %d times", released, err, calls)
	}

	e.isolate()
	held(true, true, true)

	// No embedder: the index stays isolated and the reason is typed.
	released, err := ReleaseVector(ctx, e.q, e.db, nil)
	if released || !errors.Is(err, ErrReembedRequired) || contracts.CodeOf(err) != contracts.ErrUnavailable {
		t.Fatalf("without a rebuild: released %v, %v", released, err)
	}
	held(true, true, true)

	// An embedder that is down: the record stays and the error is the embedder's.
	down := errors.New("embedder is down")
	if released, err := ReleaseVector(ctx, e.q, e.db, func(context.Context, *store.DB) error { return down }); released || !errors.Is(err, down) {
		t.Fatalf("a failed re-embedding: released %v, %v", released, err)
	}
	held(true, true, true)

	// A re-embedding that leaves the index inconsistent does not release it either.
	broken := func(context.Context, *store.DB) error {
		e.embed()
		e.exec(`UPDATE vector_rows SET inv_norm = 0 WHERE chunk_id = 'chk_1'`)
		return nil
	}
	if released, err := ReleaseVector(ctx, e.q, e.db, broken); released || err == nil {
		t.Fatalf("an index that fails its check was released: %v", err)
	}
	held(true, true, true)
	e.exec(`DELETE FROM vector_rows`)
	e.exec(`UPDATE vector_generations SET vectors = 0`)

	// The real thing: released, every vector is back, the other records stay.
	calls = 0
	released, err = ReleaseVector(ctx, e.q, e.db, embed)
	if !released || err != nil || calls != 1 {
		t.Fatalf("ReleaseVector = %v, %v (rebuild called %d times)", released, err, calls)
	}
	held(false, true, true)
	if n := e.count("vector_rows"); n != 3 {
		t.Errorf("%d vectors after the re-embedding, want 3", n)
	}
	// Released once: a second call has nothing to do.
	if released, err := ReleaseVector(ctx, e.q, e.db, embed); released || err != nil || calls != 1 {
		t.Errorf("a second call: released %v, %v, rebuild called %d times", released, err, calls)
	}
}

// A nil quarantine isolates nothing, so there is nothing to release.
func TestReleaseVectorWithoutAQuarantine(t *testing.T) {
	e := newVectorEnv(t, nil)
	if released, err := ReleaseVector(ctx, nil, e.db, nil); released || err != nil {
		t.Errorf("ReleaseVector(nil quarantine) = %v, %v", released, err)
	}
}

// VectorGenerations lists what a re-embedding has to reproduce: an isolated
// generation keeps its row with no vectors, a generation being deleted is not
// listed, and a database without the vector module has none.
func TestVectorGenerationsListsWhatAReEmbeddingMustReproduce(t *testing.T) {
	e := newVectorEnv(t, nil)
	got, err := VectorGenerations(ctx, e.db)
	want := VectorGeneration{ApplicationID: "app", TenantID: "tenant", WorkspaceID: "ws", GenerationID: "gen1", SourceID: "src1", Model: "model-4", Vectors: 3}
	if err != nil || len(got) != 1 || got[0] != want {
		t.Fatalf("VectorGenerations = %+v, %v; want [%+v]", got, err, want)
	}
	e.isolate()
	want.Vectors = 0
	if got, err := VectorGenerations(ctx, e.db); err != nil || len(got) != 1 || got[0] != want {
		t.Errorf("after the isolation: %+v, %v; want [%+v]", got, err, want)
	}
	e.exec(`UPDATE vector_generations SET state = 'dropping'`)
	if got, err := VectorGenerations(ctx, e.db); err != nil || len(got) != 0 {
		t.Errorf("a generation being deleted was listed: %+v, %v", got, err)
	}
	if got, err := VectorGenerations(ctx, newNotesEnv(t).db); err != nil || len(got) != 0 {
		t.Errorf("without the vector tables: %+v, %v", got, err)
	}
}
