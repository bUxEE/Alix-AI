package index

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

func (f *fixture) state(scope contracts.Scope, g Generation) rag.IndexGenerationState {
	f.t.Helper()
	got, err := f.ix.GetGeneration(context.Background(), scope, string(g.GenerationID))
	if err != nil {
		f.t.Fatalf("GetGeneration: %v", err)
	}
	return got.State
}

func (f *fixture) activeCount(source string) int {
	return f.count(`SELECT count(*) FROM ragindex_generation WHERE source_id = ? AND state = 'active'`, source)
}

// Generations flip atomically (master plan 12.3): the old generation serves
// until the new one is active, a failure inside the activation leaves the old
// one serving, and a reader never sees both generations or neither.
func TestGenerationActivationAtomic(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	src := f.source()
	const docs = 4
	mk := func(word string) []spec {
		var out []spec
		for i := 0; i < docs; i++ {
			out = append(out, spec{fmt.Sprintf("doc-%d", i), fmt.Sprintf("Release notes %d mention the %s marker for this build.", i, word)})
		}
		return out
	}
	gen1 := f.serving(scope, src, mk("alpha")...)
	gen2 := f.validated(scope, src, mk("beta")...)
	id1, id2 := string(gen1.GenerationID), string(gen2.GenerationID)

	// Until the switch, the old generation answers and the new one is invisible
	// (it is built, validated, retained, and not queryable).
	if res := f.search(scope, Query{Text: "marker", Limit: 20}); len(res.Hits) != docs || res.Hits[0].GenerationID != id1 {
		t.Fatalf("before activation: %s", joinHits(res))
	}
	if f.state(scope, gen2) != rag.IndexGenerationStateValidating {
		t.Fatalf("gen2 state = %s", f.state(scope, gen2))
	}
	versionBefore, err := f.ix.Version(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}

	// A building generation cannot be activated: it was never validated.
	building := f.begin(scope, f.source(), mk("gamma")...)
	if err := f.ix.ActivateGeneration(ctx, scope, string(building.GenerationID)); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("activating a building generation: err = %v, want conflict", err)
	}
	// A second build of the same source cannot start while one is in flight.
	if _, err := f.ix.BeginGeneration(ctx, scope, src, f.comps()); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("second concurrent build: err = %v, want conflict", err)
	}
	// Another scope cannot activate it, and gets the same answer as for a missing id.
	if err := f.ix.ActivateGeneration(ctx, f.scope("app", "t-other", "w1"), id2); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Fatalf("foreign scope activating: err = %v", err)
	}

	// Crash points: a failure after the old generation was superseded, and one
	// after the new one was marked active, both before the commit. Neither may
	// leave a trace: the old generation still serves and nothing changed.
	for _, step := range []string{"superseded", "activated"} {
		boom := errors.New("simulated crash at " + step)
		f.ix.activateHook = func(s string) error {
			if s == step {
				return boom
			}
			return nil
		}
		if err := f.ix.ActivateGeneration(ctx, scope, id2); !errors.Is(err, boom) {
			t.Fatalf("hook at %s: err = %v", step, err)
		}
		f.ix.activateHook = nil
		if f.state(scope, gen1) != rag.IndexGenerationStateActive || f.state(scope, gen2) != rag.IndexGenerationStateValidating {
			t.Fatalf("after a failure at %s: gen1 %s, gen2 %s", step, f.state(scope, gen1), f.state(scope, gen2))
		}
		if n := f.activeCount(src); n != 1 {
			t.Fatalf("after a failure at %s: %d active generations", step, n)
		}
		res := f.search(scope, Query{Text: "marker", Limit: 20})
		if len(res.Hits) != docs || res.Hits[0].GenerationID != id1 || !strings.Contains(res.Hits[0].Text, "alpha") {
			t.Fatalf("after a failure at %s the old generation does not serve: %s", step, joinHits(res))
		}
		if v, _ := f.ix.Version(ctx, scope); v.Epoch != versionBefore.Epoch {
			t.Fatalf("a rolled-back activation moved the epoch: %d -> %d", versionBefore.Epoch, v.Epoch)
		}
	}

	// Readers run while the generation flips back and forth: every result is
	// wholly one generation, complete, and never empty.
	var stop atomic.Bool
	var wg sync.WaitGroup
	var reads atomic.Int64
	var bad atomic.Value
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				res, err := f.ix.Search(ctx, scope, Query{Text: "marker", Limit: 20})
				if err != nil {
					bad.CompareAndSwap(nil, fmt.Sprintf("search error: %v", err))
					return
				}
				reads.Add(1)
				if len(res.Hits) != docs {
					bad.CompareAndSwap(nil, fmt.Sprintf("a reader saw %d hits, want %d: %s", len(res.Hits), docs, joinHits(res)))
					return
				}
				first := res.Hits[0].GenerationID
				for _, h := range res.Hits {
					if h.GenerationID != first {
						bad.CompareAndSwap(nil, "a reader saw two generations in one result: "+joinHits(res))
						return
					}
				}
				if len(res.Version.Active) != 1 || !strings.HasSuffix(res.Version.Active[0], first) {
					bad.CompareAndSwap(nil, fmt.Sprintf("version %v does not match the hits' generation %s", res.Version.Active, first))
					return
				}
			}
		}()
	}
	flips := []string{id2, id1, id2, id1, id2}
	for _, id := range flips {
		if err := f.ix.ActivateGeneration(ctx, scope, id); err != nil {
			stop.Store(true)
			wg.Wait()
			t.Fatalf("ActivateGeneration(%s): %v", id, err)
		}
		if n := f.activeCount(src); n != 1 {
			t.Errorf("%d active generations after a flip", n)
		}
		time.Sleep(15 * time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()
	if v := bad.Load(); v != nil {
		t.Fatal(v)
	}
	if reads.Load() < 20 {
		t.Fatalf("only %d reads overlapped the flips; the test is too weak to mean anything", reads.Load())
	}
	// The last flip made gen2 active; gen1 is retained as superseded (rollback).
	if f.state(scope, gen2) != rag.IndexGenerationStateActive || f.state(scope, gen1) != rag.IndexGenerationStateSuperseded {
		t.Fatalf("final states: gen1 %s, gen2 %s", f.state(scope, gen1), f.state(scope, gen2))
	}
	g2, _ := f.ix.GetGeneration(ctx, scope, id2)
	if g2.ActivatedAt == nil || g2.RetiredAt != nil {
		t.Errorf("active generation timestamps: activated %v retired %v", g2.ActivatedAt, g2.RetiredAt)
	}
	g1, _ := f.ix.GetGeneration(ctx, scope, id1)
	if g1.ActivatedAt == nil || g1.RetiredAt == nil {
		t.Errorf("superseded generation timestamps: activated %v retired %v", g1.ActivatedAt, g1.RetiredAt)
	}
}

// The state machine refuses what it must: writes into a generation that is not
// building or active, validation of a finished one, activation of a failed or
// invalidated one.
func TestGenerationLifecycleRules(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	src := f.source()

	g := f.begin(scope, src, spec{"d", "alpha text"})
	id := string(g.GenerationID)
	// A generation that is being built, or validated, is invisible to queries.
	if res := f.search(scope, Query{Text: "alpha"}); len(res.Hits) != 0 {
		t.Fatalf("a building generation is visible: %s", joinHits(res))
	}
	if _, err := f.ix.ValidateGeneration(ctx, scope, id, Expectation{}); err != nil {
		t.Fatal(err)
	}
	if res := f.search(scope, Query{Text: "alpha"}); len(res.Hits) != 0 {
		t.Fatalf("a validating generation is visible: %s", joinHits(res))
	}
	// Validated: no more writes, so what was validated is what gets activated.
	if _, err := f.ix.PutDocument(ctx, scope, id, f.doc("late", "v1", "late text")); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("put into a validating generation: err = %v, want conflict", err)
	}
	if err := f.ix.ActivateGeneration(ctx, scope, id); err != nil {
		t.Fatal(err)
	}
	// Active generations take incremental updates.
	if _, err := f.ix.PutDocument(ctx, scope, id, f.doc("late", "v1", "late text")); err != nil {
		t.Fatalf("incremental put into the active generation: %v", err)
	}

	// A failed generation never serves and cannot be activated.
	next := f.begin(scope, src, spec{"d", "beta text"})
	if err := f.ix.FailGeneration(ctx, scope, string(next.GenerationID), contracts.New(contracts.ErrUnavailable, "parser died")); err != nil {
		t.Fatal(err)
	}
	failed, _ := f.ix.GetGeneration(ctx, scope, string(next.GenerationID))
	if failed.State != rag.IndexGenerationStateFailed || failed.Error == nil || failed.Error.Code != "unavailable" || failed.ActivatedAt != nil || failed.RetiredAt != nil {
		t.Fatalf("failed generation: %+v", failed)
	}
	if err := f.ix.ActivateGeneration(ctx, scope, string(next.GenerationID)); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("activating a failed generation: err = %v", err)
	}

	// Invalidating the serving generation removes it from queries at once.
	if res := f.search(scope, Query{Text: "alpha"}); len(res.Hits) != 1 {
		t.Fatalf("before invalidation: %s", joinHits(res))
	}
	if err := f.ix.InvalidateGeneration(ctx, scope, id, contracts.New(contracts.ErrInternal, "operator withdrew it")); err != nil {
		t.Fatal(err)
	}
	if res := f.search(scope, Query{Text: "alpha"}); len(res.Hits) != 0 {
		t.Fatalf("an invalidated generation still serves: %s", joinHits(res))
	}
	if err := f.ix.ActivateGeneration(ctx, scope, id); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("re-activating an invalidated generation: err = %v", err)
	}
	inv, _ := f.ix.GetGeneration(ctx, scope, id)
	if inv.State != rag.IndexGenerationStateInvalidated || inv.RetiredAt == nil {
		t.Fatalf("invalidated generation: %+v", inv)
	}

	list, err := f.ix.ListGenerations(ctx, scope, src)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListGenerations = %d, %v", len(list), err)
	}
	if _, ok, err := f.ix.ActiveGeneration(ctx, scope, src); err != nil || ok {
		t.Fatalf("ActiveGeneration after invalidation: ok=%v err=%v", ok, err)
	}
}

// Validation fails closed: a generation that does not match what its builder
// expected, or whose rows are inconsistent, is marked failed and never serves.
func TestValidateGenerationFailsClosed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")

	// Expectation mismatch.
	g := f.begin(scope, f.source(), spec{"a", "first document"}, spec{"b", "second document"})
	five := 5
	rep, err := f.ix.ValidateGeneration(ctx, scope, string(g.GenerationID), Expectation{Chunks: &five})
	if contracts.CodeOf(err) != contracts.ErrValidationFailed || rep.OK() {
		t.Fatalf("expectation mismatch: err = %v, report %+v", err, rep)
	}
	if f.state(scope, g) != rag.IndexGenerationStateFailed {
		t.Fatalf("state after failed validation = %s", f.state(scope, g))
	}
	failed, _ := f.ix.GetGeneration(ctx, scope, string(g.GenerationID))
	if failed.Error == nil || failed.Error.Code != "validation_failed" {
		t.Errorf("failed generation carries no typed error: %+v", failed.Error)
	}
	if err := f.ix.ActivateGeneration(ctx, scope, string(g.GenerationID)); contracts.CodeOf(err) != contracts.ErrConflict {
		t.Fatalf("a failed validation was activatable: %v", err)
	}

	// A child without its parent (rows damaged behind the index's back).
	g2 := f.begin(scope, f.source(), spec{"a", "first document"})
	if _, err := f.db.ExecContext(ctx, `DELETE FROM ragindex_parent WHERE generation_id = ?`, string(g2.GenerationID)); err != nil {
		t.Fatal(err)
	}
	rep, err = f.ix.ValidateGeneration(ctx, scope, string(g2.GenerationID), Expectation{})
	if err == nil || rep.OrphanChildren != 1 {
		t.Fatalf("orphan child not detected: err = %v, report %+v", err, rep)
	}

	// Counts that drifted from the rows.
	g3 := f.begin(scope, f.source(), spec{"a", "first document"})
	if _, err := f.db.ExecContext(ctx, `UPDATE ragindex_generation SET chunks = chunks + 3 WHERE generation_id = ?`, string(g3.GenerationID)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.ix.ValidateGeneration(ctx, scope, string(g3.GenerationID), Expectation{}); err == nil {
		t.Fatal("drifted counts were accepted")
	}

	// A chunk whose full-text entry is missing: the probe finds it out.
	g4 := f.begin(scope, f.source(), spec{"a", "first document about turbines"})
	if _, err := f.db.ExecContext(ctx,
		`INSERT INTO ragindex_fts (ragindex_fts, rowid, text, section, path, header, application_id, tenant_id, workspace_id, source_id)
		 SELECT 'delete', rid, text, section, path, header, application_id, tenant_id, workspace_id, source_id FROM ragindex_chunk WHERE generation_id = ?`,
		string(g4.GenerationID)); err != nil {
		t.Fatal(err)
	}
	rep, err = f.ix.ValidateGeneration(ctx, scope, string(g4.GenerationID), Expectation{})
	if err == nil || rep.ProbesFailed != 1 {
		t.Fatalf("a missing index entry was not detected: err = %v, report %+v", err, rep)
	}

	// And a sound generation passes with the right expectation.
	one := 1
	g5 := f.begin(scope, f.source(), spec{"a", "first document"})
	if rep, err := f.ix.ValidateGeneration(ctx, scope, string(g5.GenerationID), Expectation{Documents: &one, Chunks: &one}); err != nil || !rep.OK() || rep.ProbesRun != 1 {
		t.Fatalf("sound generation: err = %v, report %+v", err, rep)
	}
}

// The probe finds a chunk whose first words hold characters the unicode61
// tokenizer keeps inside a token although they are not letters or decimal
// digits: other numbers (² ½ ① Ⅳ) and private-use characters. A probe that cut
// them out would fail a sound generation, and every rebuild would fail again.
func TestValidateGenerationProbesNumbersAndPrivateUse(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	docs := []spec{
		{"sup", "Superficie 120 m² totale"},
		{"half", "Mix ½ cup of sugar"},
		{"step", "Step ① of the install"},
		{"chap", "Capitolo Ⅳ inizio della storia"},
		{"pua", "Part x7 is the spare"},
	}
	g := f.begin(scope, f.source(), docs...)
	rep, err := f.ix.ValidateGeneration(ctx, scope, string(g.GenerationID), Expectation{})
	if err != nil || rep.ProbesFailed != 0 || rep.ProbesRun != len(docs) {
		t.Fatalf("sound generation failed its probes: err = %v, report %+v", err, rep)
	}
}

// The validation probe samples the same chunks it always did (the first, the
// last and those evenly spaced between, by rid, over the live chunks) without
// reading every rid of the generation.
func TestValidateGenerationProbeSample(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	// The sample as it was taken from the full list of rids.
	reference := func(tx *store.Tx, g genRow, n int) []int64 {
		rows, err := tx.QueryContext(ctx, `SELECT rid FROM ragindex_chunk
 WHERE application_id = ? AND tenant_id = ? AND workspace_id = ? AND source_id = ? AND generation_id = ? AND tombstoned_at IS NULL ORDER BY rid`,
			scope.ApplicationID, scope.TenantID, scope.WorkspaceID, g.sourceID, g.id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var all []int64
		for rows.Next() {
			var rid int64
			if err := rows.Scan(&rid); err != nil {
				t.Fatal(err)
			}
			all = append(all, rid)
		}
		if len(all) <= n {
			return all
		}
		var picks []int64
		for i := 0; i < n; i++ {
			picks = append(picks, all[i*(len(all)-1)/(n-1)])
		}
		return picks
	}
	n := f.ix.cfg.ValidateProbes
	for _, live := range []int{0, 1, n - 1, n, n + 1, 37, 100} {
		src := f.source()
		var docs []spec
		var dead []string
		for i := 0; len(docs)-len(dead) < live; i++ {
			id := fmt.Sprintf("d%03d", i)
			docs = append(docs, spec{id, fmt.Sprintf("Page %d about the harbour tides.", i)})
			if i%4 == 3 {
				dead = append(dead, id)
			}
		}
		g := f.begin(scope, src, docs...)
		for _, id := range dead {
			if _, err := f.ix.DeleteDocument(ctx, scope, src, id); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.db.View(ctx, func(tx *store.Tx) error {
			row, err := loadGeneration(ctx, tx, scope, string(g.GenerationID))
			if err != nil {
				return err
			}
			got, err := f.ix.probeRids(ctx, tx, scope, row, n)
			if err != nil {
				return err
			}
			if want := reference(tx, row, n); !slices.Equal(got, want) {
				t.Errorf("%d live chunks: sampled %v, want %v", live, got, want)
			}
			// A single probe takes the first chunk (the formula above has no
			// spacing for one pick).
			one, err := f.ix.probeRids(ctx, tx, scope, row, 1)
			if err != nil {
				return err
			}
			if all := reference(tx, row, live); len(all) > 0 && !slices.Equal(one, all[:1]) {
				t.Errorf("%d live chunks: a single probe sampled %v, want the first, %d", live, one, all[0])
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// PutDocument replaces an item atomically, is idempotent for an unchanged item,
// keeps the generation's counts exact, and lets a deleted item come back.
func TestPutDocumentReplacesAtomically(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	src := f.source()
	g := f.serving(scope, src, spec{"d", "# Policy\n\nOriginal wording about refunds."}, spec{"e", "Unrelated page about shipping."})
	id := string(g.GenerationID)
	counts := func() (int64, int64) {
		got, _ := f.ix.GetGeneration(ctx, scope, id)
		return int64(got.Counts.Documents), int64(got.Counts.Chunks)
	}
	if d, c := counts(); d != 2 || c != 2 {
		t.Fatalf("counts after build: %d documents, %d chunks", d, c)
	}
	epoch0 := f.search(scope, Query{Text: "refunds"}).Version.Epoch

	// Same version, same chunks: nothing written.
	res, err := f.ix.PutDocument(ctx, scope, id, f.doc("d", "v1", "# Policy\n\nOriginal wording about refunds."))
	if err != nil || !res.Unchanged {
		t.Fatalf("idempotent put: %+v, %v", res, err)
	}
	if e := f.search(scope, Query{Text: "refunds"}).Version.Epoch; e != epoch0 {
		t.Errorf("an unchanged put moved the epoch %d -> %d", epoch0, e)
	}

	// New version: the old text is gone, the new is found, in one step.
	res, err = f.ix.PutDocument(ctx, scope, id, f.doc("d", "v2", "# Policy\n\nRevised wording about returns."))
	if err != nil || res.Unchanged {
		t.Fatalf("replacing put: %+v, %v", res, err)
	}
	if got := f.search(scope, Query{Text: "refunds"}); len(got.Hits) != 0 {
		t.Fatalf("the replaced text is still found: %s", joinHits(got))
	}
	got := f.search(scope, Query{Text: "returns"})
	if len(got.Hits) != 1 || got.Hits[0].DocumentVersion != "v2" || got.Version.Epoch == epoch0 {
		t.Fatalf("the new text: %s version %+v", joinHits(got), got.Version)
	}
	if d, c := counts(); d != 2 || c != 2 {
		t.Fatalf("counts after replacing: %d documents, %d chunks", d, c)
	}
	// No stale rows or index entries left behind by the replacement.
	if n := f.count(`SELECT count(*) FROM ragindex_chunk WHERE document_id = 'd'`); n != 1 {
		t.Errorf("%d chunk rows for the replaced document", n)
	}
	if n := f.count(`SELECT count(*) FROM ragindex_fts WHERE ragindex_fts MATCH 'refunds'`); n != 0 {
		t.Errorf("the old text still has %d full-text entries", n)
	}

	// Delete, then bring it back.
	if n, err := f.ix.DeleteDocument(ctx, scope, src, "d"); err != nil || n != 1 {
		t.Fatalf("DeleteDocument: %d, %v", n, err)
	}
	if d, c := counts(); d != 1 || c != 1 {
		t.Fatalf("counts after deleting: %d documents, %d chunks", d, c)
	}
	if _, err := f.ix.PutDocument(ctx, scope, id, f.doc("d", "v3", "# Policy\n\nWording restored about returns.")); err != nil {
		t.Fatal(err)
	}
	if got := f.search(scope, Query{Text: "restored"}); len(got.Hits) != 1 {
		t.Fatalf("a deleted item that came back is not found: %s", joinHits(got))
	}
	if d, c := counts(); d != 2 || c != 2 {
		t.Fatalf("counts after restoring: %d documents, %d chunks", d, c)
	}

	// Malformed documents are refused, not repaired.
	bad := f.doc("x", "v1", "text")
	bad.Chunks.Children[0].ParentID = "chp_missing"
	if _, err := f.ix.PutDocument(ctx, scope, id, bad); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("orphan child: err = %v", err)
	}
	noID := f.doc("x", "v1", "text")
	noID.ID = ""
	if _, err := f.ix.PutDocument(ctx, scope, id, noID); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("empty document id: err = %v", err)
	}
	// Another scope cannot write into the generation.
	if _, err := f.ix.PutDocument(ctx, f.scope("app", "t2", "w1"), id, f.doc("z", "v1", "text")); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("foreign scope put: err = %v", err)
	}
}
