package index

import (
	"context"
	"testing"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// rawMatches counts full-text entries for a term across the whole index,
// bypassing every scope and visibility rule: it shows what physically exists.
func (f *fixture) rawMatches(term string) int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM ragindex_fts WHERE ragindex_fts MATCH ?`, term)
}

// SEC-028: deletion and revocation hide content immediately (a tombstone);
// physical removal comes later (compaction). A revocation that happens while a
// query runs is caught by the final recheck.
func TestTombstoneHidesImmediately(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	other := f.scope("app", "t2", "w1")
	src, otherSrc := f.source(), f.source()
	gen := f.serving(scope, src,
		spec{"keep", "# Keep\n\nThe lighthouse keeper logs every ship that passes."},
		spec{"drop", "# Drop\n\nThe lighthouse beam is serviced yearly by the harbour board."},
	)
	f.serving(other, otherSrc, spec{"theirs", "The lighthouse in the other tenant is untouched by any deletion."})

	res := f.search(scope, Query{Text: "lighthouse", Limit: 10})
	if len(res.Hits) != 2 {
		t.Fatalf("before deletion: %s", joinHits(res))
	}
	var dropHit Hit
	for _, h := range res.Hits {
		if h.DocumentID == "drop" {
			dropHit = h
		}
	}
	parent, ok, err := f.ix.ParentOf(ctx, scope, dropHit, false)
	if err != nil || !ok || parent.Section != "Drop" {
		t.Fatalf("parent before deletion: %+v ok=%v err=%v", parent, ok, err)
	}
	epoch0 := res.Version.Epoch

	// A query is in flight: it already holds the hits. The document is deleted.
	n, err := f.ix.DeleteDocument(ctx, scope, src, "drop")
	if err != nil || n != 1 {
		t.Fatalf("DeleteDocument: %d, %v", n, err)
	}

	// Immediately: no search, no recheck, no parent expansion returns it.
	after := f.search(scope, Query{Text: "lighthouse", Limit: 10})
	if got := docIDs(after); !equalStrings(got, []string{"keep"}) {
		t.Fatalf("after deleting 'drop': %v", got)
	}
	if after.Version.Epoch == epoch0 {
		t.Error("a deletion did not move the epoch, so a cached result for this scope would still be served")
	}
	verdicts, err := f.ix.Recheck(ctx, scope, []Ref{RefOf(dropHit), RefOf(res.Hits[0])}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range verdicts {
		if v.Ref.ChunkID == dropHit.ChunkID {
			if v.Visible || v.Reason != ReasonGone {
				t.Errorf("recheck of the deleted chunk: %+v", v)
			}
		} else if res.Hits[0].DocumentID != "drop" && !v.Visible {
			t.Errorf("recheck of a surviving chunk: %+v", v)
		}
	}
	if _, ok, _ := f.ix.ParentOf(ctx, scope, dropHit, false); ok {
		t.Error("the parent of a deleted chunk is still expandable")
	}
	if got := f.search(scope, Query{Text: "harbour board serviced"}); len(got.Hits) != 0 {
		t.Errorf("deleted text still found: %s", joinHits(got))
	}
	// An exact-identifier lookup hides it too.
	g2, _ := f.ix.GetGeneration(ctx, scope, string(gen.GenerationID))
	if g2.Counts.Documents != 1 || g2.Counts.Chunks != 1 {
		t.Errorf("generation counts after deletion: %+v", g2.Counts)
	}

	// Physically it is all still there: tombstoned, not compacted.
	if n := f.count(`SELECT count(*) FROM ragindex_chunk WHERE document_id = 'drop' AND tombstoned_at IS NOT NULL`); n != 1 {
		t.Fatalf("tombstoned chunk rows = %d, want 1 (rows stay until compaction)", n)
	}
	if n := f.rawMatches("harbour"); n != 1 {
		t.Fatalf("the full-text entry of the tombstoned chunk is gone already (%d), tombstoning must not touch the index", n)
	}

	// Compaction with a grace period keeps it; after the grace it is removed.
	st, err := f.ix.Compact(ctx, CompactOptions{Grace: time.Hour})
	if err != nil || st.Chunks != 0 {
		t.Fatalf("compaction inside the grace period: %+v, %v", st, err)
	}
	f.clk.Advance(2 * time.Hour)
	st, err = f.ix.Compact(ctx, CompactOptions{Grace: time.Hour})
	if err != nil || st.Chunks != 1 || st.Parents != 1 || st.Documents != 1 || st.More {
		t.Fatalf("compaction after the grace period: %+v, %v", st, err)
	}
	if n := f.count(`SELECT count(*) FROM ragindex_chunk WHERE document_id = 'drop'`); n != 0 {
		t.Errorf("%d chunk rows left after compaction", n)
	}
	if n := f.rawMatches("harbour"); n != 0 {
		t.Errorf("%d full-text entries left after compaction", n)
	}
	if got := docIDs(f.search(scope, Query{Text: "lighthouse", Limit: 10})); !equalStrings(got, []string{"keep"}) {
		t.Errorf("compaction changed what is visible: %v", got)
	}

	// Revoking a whole source mid-query: the recheck drops everything and the
	// generation is invalidated; the other tenant is unaffected.
	live := f.search(scope, Query{Text: "lighthouse"})
	if len(live.Hits) != 1 {
		t.Fatal("setup")
	}
	n, err = f.ix.DeleteSource(ctx, scope, src)
	if err != nil || n != 1 {
		t.Fatalf("DeleteSource: %d, %v", n, err)
	}
	verdicts, err = f.ix.Recheck(ctx, scope, []Ref{RefOf(live.Hits[0])}, false)
	if err != nil || verdicts[0].Visible {
		t.Fatalf("recheck after the source was revoked mid-query: %+v, %v", verdicts, err)
	}
	if got := f.search(scope, Query{Text: "lighthouse"}); len(got.Hits) != 0 {
		t.Fatalf("a deleted source is still found: %s", joinHits(got))
	}
	if st := f.state(scope, gen); st != rag.IndexGenerationStateInvalidated {
		t.Errorf("generation state after the source was deleted = %s", st)
	}
	if got := f.search(other, Query{Text: "lighthouse"}); len(got.Hits) != 1 {
		t.Errorf("another tenant lost its content: %s", joinHits(got))
	}
	// Compaction removes the rows and the generation itself.
	st, err = f.ix.Compact(ctx, CompactOptions{})
	if err != nil || st.Chunks != 1 || st.Generations != 1 {
		t.Fatalf("compacting a deleted source: %+v, %v", st, err)
	}
	for _, table := range []string{"ragindex_parent", "ragindex_document"} {
		if n := f.count(`SELECT count(*) FROM ` + table + ` WHERE tenant_id = 't1'`); n != 0 {
			t.Errorf("%s still holds %d rows for the deleted source", table, n)
		}
	}
	if n := f.count(`SELECT count(*) FROM ragindex_generation WHERE tenant_id = 't1'`); n != 0 {
		t.Errorf("%d generation rows left", n)
	}
	if n := f.count(`SELECT count(*) FROM ragindex_ident WHERE tenant_id = 't1'`); n != 0 {
		t.Errorf("%d identifier rows left", n)
	}
	if n := f.rawMatches("lighthouse"); n != 1 {
		t.Errorf("full-text entries for lighthouse = %d, want only the other tenant's", n)
	}
}

// Deleting a workspace or a tenant hides everything under it at once, leaves
// every other scope alone, and a tenant deletion is bounded by the caller's own
// application and tenant.
func TestDeleteScopeAndTenantHideEverything(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w1 := f.scope("app", "t1", "w1")
	w2 := f.scope("app", "t1", "w2")
	t2 := f.scope("app", "t2", "w1")
	otherApp := f.scope("app2", "t1", "w1")
	for _, s := range []contracts.Scope{w1, w2, t2, otherApp} {
		f.serving(s, f.source(), spec{"d", "the beacon warns ships of rocks"}, spec{"e", "beacon maintenance schedule"})
	}
	count := func(s contracts.Scope) int { return len(f.search(s, Query{Text: "beacon", Limit: 10}).Hits) }
	for _, s := range []contracts.Scope{w1, w2, t2, otherApp} {
		if count(s) != 2 {
			t.Fatalf("setup: %d hits", count(s))
		}
	}
	if n, err := f.ix.DeleteScope(ctx, w1); err != nil || n != 2 {
		t.Fatalf("DeleteScope: %d, %v", n, err)
	}
	if count(w1) != 0 || count(w2) != 2 || count(t2) != 2 || count(otherApp) != 2 {
		t.Fatalf("after DeleteScope(w1): %d %d %d %d", count(w1), count(w2), count(t2), count(otherApp))
	}
	// DeleteTenant from w1 covers w2 as well (same application and tenant), and
	// nothing else; the tenant id "t1" of app2 is a different tenant.
	if n, err := f.ix.DeleteTenant(ctx, w1); err != nil || n != 2 {
		t.Fatalf("DeleteTenant: %d, %v", n, err)
	}
	if count(w2) != 0 || count(t2) != 2 || count(otherApp) != 2 {
		t.Fatalf("after DeleteTenant(app/t1): w2 %d, t2 %d, app2/t1 %d", count(w2), count(t2), count(otherApp))
	}
	if _, err := f.ix.Compact(ctx, CompactOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM ragindex_chunk WHERE application_id = 'app' AND tenant_id = 't1'`); n != 0 {
		t.Errorf("%d chunk rows left for the deleted tenant", n)
	}
	if n := f.count(`SELECT count(*) FROM ragindex_chunk`); n != 4 {
		t.Errorf("%d chunk rows left in total, want the other two scopes' 4", n)
	}
}

// Compaction is bounded: it stops at its row budget and says so, and repeated
// passes finish the work.
func TestCompactionIsBounded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	src := f.source()
	var docs []spec
	for i := 0; i < 25; i++ {
		docs = append(docs, spec{string(rune('a'+i%26)) + "-doc-" + string(rune('A'+i/26)), "page about harbour tides number " + string(rune('a'+i%26))})
	}
	f.serving(scope, src, docs...)
	if _, err := f.ix.DeleteSource(ctx, scope, src); err != nil {
		t.Fatal(err)
	}
	passes, removed := 0, 0
	for {
		st, err := f.ix.Compact(ctx, CompactOptions{MaxRows: 10, BatchRows: 4})
		if err != nil {
			t.Fatal(err)
		}
		removed += st.Chunks
		passes++
		if st.Chunks > 10 {
			t.Fatalf("a pass removed %d chunks over its budget of 10", st.Chunks)
		}
		if !st.More {
			break
		}
		if passes > 20 {
			t.Fatal("compaction does not finish")
		}
	}
	if removed != 25 || passes < 3 {
		t.Fatalf("removed %d chunks in %d passes", removed, passes)
	}
	if n := f.count(`SELECT count(*) FROM ragindex_chunk`); n != 0 {
		t.Errorf("%d chunk rows left", n)
	}
}

// Superseded generations are kept for rollback up to the configured number;
// failed and abandoned builds are removed.
func TestCompactionRetention(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	src := f.source()
	var gens []Generation
	for i := 0; i < 4; i++ {
		gens = append(gens, f.serving(scope, src, spec{"d", "stable page about moorings"}))
		f.clk.Advance(time.Minute)
	}
	// gens[3] serves; 0..2 are superseded. One is kept for rollback.
	st, err := f.ix.Compact(ctx, CompactOptions{})
	if err != nil || st.Generations != 2 {
		t.Fatalf("compaction kept the wrong number: %+v, %v", st, err)
	}
	if got := f.state(scope, gens[2]); got != rag.IndexGenerationStateSuperseded {
		t.Errorf("the newest superseded generation was removed: %s", got)
	}
	if _, err := f.ix.GetGeneration(ctx, scope, string(gens[0].GenerationID)); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("the oldest generation is still there: %v", err)
	}
	// Rolling back to the retained generation works.
	if err := f.ix.ActivateGeneration(ctx, scope, string(gens[2].GenerationID)); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if res := f.search(scope, Query{Text: "moorings"}); len(res.Hits) != 1 || res.Hits[0].GenerationID != string(gens[2].GenerationID) {
		t.Fatalf("after rollback: %s", joinHits(res))
	}

	// An abandoned build is failed after the stale period and then removed.
	stale := f.begin(scope, f.source(), spec{"x", "half built generation"})
	f.clk.Advance(48 * time.Hour)
	// A build that is not stale yet is left alone.
	st, err = f.ix.Compact(ctx, CompactOptions{StaleBuild: 72 * time.Hour})
	if err != nil || st.Abandoned != 0 || f.state(scope, stale) != rag.IndexGenerationStateBuilding {
		t.Fatalf("a fresh build was touched: %+v, %v", st, err)
	}
	st, err = f.ix.Compact(ctx, CompactOptions{})
	if err != nil || st.Abandoned != 1 || st.Generations != 1 {
		t.Fatalf("abandoned build: %+v, %v", st, err)
	}
	if _, err := f.ix.GetGeneration(ctx, scope, string(stale.GenerationID)); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("the failed build was not removed: %v", err)
	}
}

// A build is abandoned by its last activity, not by its age: one that started
// long ago and is still being written is left alone, and compacting one scope
// never touches another scope's build.
func TestCompactionJudgesABuildByItsLastActivity(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	src := f.source()
	build := f.begin(scope, src, spec{"x", "first page of a long build"})
	f.clk.Advance(48 * time.Hour)
	if _, err := f.ix.PutDocument(ctx, scope, string(build.GenerationID), f.doc("y", "v1", "a page written two days into the build")); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(time.Hour)
	st, err := f.ix.Compact(ctx, CompactOptions{})
	if err != nil || st.Abandoned != 0 || f.state(scope, build) != rag.IndexGenerationStateBuilding {
		t.Fatalf("a build that is still being written was abandoned: %+v, %v", st, err)
	}

	f.clk.Advance(24 * time.Hour)
	other := f.scope("app", "t2", "w1")
	f.serving(other, f.source(), spec{"o", "another tenant's page"})
	st, err = f.ix.CompactScope(ctx, other, CompactOptions{})
	if err != nil || st.Abandoned != 0 || f.state(scope, build) != rag.IndexGenerationStateBuilding {
		t.Fatalf("compacting another scope touched this scope's build: %+v, %v", st, err)
	}
	st, err = f.ix.CompactScope(ctx, scope, CompactOptions{})
	if err != nil || st.Abandoned != 1 {
		t.Fatalf("a build silent for a day was not abandoned by its own scope: %+v, %v", st, err)
	}
}
