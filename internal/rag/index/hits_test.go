package index

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// searchOnly is what a read by reference does not carry: nothing was ranked.
var searchOnly = cmpopts.IgnoreFields(Hit{}, "Rank", "Score", "BM25", "ExactIdentifiers")

func refsOf(hits ...Hit) []Ref {
	out := make([]Ref, len(hits))
	for i, h := range hits {
		out[i] = RefOf(h)
	}
	return out
}

// hitsOf calls Hits and fails the test on an error.
func (f *fixture) hitsOf(scope contracts.Scope, refs []Ref, includeHidden bool) []Hit {
	f.t.Helper()
	got, err := f.ix.Hits(context.Background(), scope, refs, includeHidden)
	if err != nil {
		f.t.Fatalf("Hits: %v", err)
	}
	return got
}

// serveHidden puts the document with a hidden paragraph into a serving
// generation of a new source, like TestHiddenChunksAreExcludedByDefault.
func (f *fixture) serveHidden(scope contracts.Scope, id string) Generation {
	f.t.Helper()
	ctx := context.Background()
	g, err := f.ix.BeginGeneration(ctx, scope, f.source(), f.comps())
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.ix.PutDocument(ctx, scope, string(g.GenerationID), f.docWithHidden(id)); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.ix.ValidateGeneration(ctx, scope, string(g.GenerationID), Expectation{}); err != nil {
		f.t.Fatal(err)
	}
	if err := f.ix.ActivateGeneration(ctx, scope, string(g.GenerationID)); err != nil {
		f.t.Fatal(err)
	}
	return g
}

// A read by reference returns what Search returns for the same chunks, in the
// order asked, under the caller's scope only (the query.HitReader contract: the
// hybrid pipeline reads the chunks that only its vector stage found).
func TestHitsReadsByRefUnderScope(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	src := f.source()
	var docs []spec
	for i := 0; i < 6; i++ {
		docs = append(docs, spec{fmt.Sprintf("doc-%d", i), fmt.Sprintf("# Dock %d\n\nThe harbour dock schedule %d lists the tide tables.", i, i)})
	}
	g := f.serving(scope, src, docs...)
	found := f.search(scope, Query{Text: "harbour dock schedule", Limit: 10}).Hits
	if len(found) != 6 {
		t.Fatalf("setup: %d hits", len(found))
	}

	// The same hits, whatever the order of the refs, with nothing ranked.
	reversed := make([]Hit, len(found))
	for i, h := range found {
		reversed[len(found)-1-i] = h
	}
	for name, want := range map[string][]Hit{"search order": found, "reversed": reversed} {
		got := f.hitsOf(scope, refsOf(want...), false)
		if diff := cmp.Diff(want, got, searchOnly); diff != "" {
			t.Errorf("%s: hits by reference differ from the search's (-search +ref):\n%s", name, diff)
		}
		for _, h := range got {
			if h.Rank != 0 || h.Score != 0 || h.BM25 != 0 || len(h.ExactIdentifiers) != 0 {
				t.Errorf("%s: a read by reference reports a ranking: %+v", name, h)
			}
			if h.Text == "" || h.DocumentVersion != "v1" || h.ParentID == "" || h.Span.End <= h.Span.Start {
				t.Errorf("%s: the hit lacks what a citation needs: %+v", name, h)
			}
		}
	}

	// A chunk named twice comes back once; a ref with no version matches any.
	two := f.hitsOf(scope, []Ref{RefOf(found[0]), RefOf(found[0]), {ChunkID: found[1].ChunkID, GenerationID: found[1].GenerationID}}, false)
	if len(two) != 2 || two[0].ChunkID != found[0].ChunkID || two[1].ChunkID != found[1].ChunkID {
		t.Errorf("a repeated ref and a versionless one: %+v", two)
	}
	// The version the caller saw must still be current.
	stale := RefOf(found[0])
	stale.DocumentVersion = "v0"
	if got := f.hitsOf(scope, []Ref{stale, RefOf(found[1])}, false); len(got) != 1 || got[0].ChunkID != found[1].ChunkID {
		t.Errorf("a ref to another document version was served: %+v", got)
	}
	if got := f.hitsOf(scope, nil, false); got != nil {
		t.Errorf("no refs returned %+v", got)
	}

	// The scope is the caller's whole scope: no other tenant, workspace or
	// application reads these chunks, though the refs are exact.
	for _, other := range []contracts.Scope{f.scope("app", "t2", "w1"), f.scope("app", "t1", "w2"), f.scope("app2", "t1", "w1")} {
		if got := f.hitsOf(other, refsOf(found...), true); got != nil {
			t.Errorf("scope %s/%s/%s read %d chunks of another scope", other.ApplicationID, other.TenantID, other.WorkspaceID, len(got))
		}
	}

	// Chunks that stopped being served are absent: a deleted document, and a
	// superseded generation.
	if _, err := f.ix.DeleteDocument(ctx, scope, src, "doc-0"); err != nil {
		t.Fatal(err)
	}
	if got := f.hitsOf(scope, refsOf(found...), false); len(got) != 5 || got[0].DocumentID == "doc-0" {
		t.Errorf("a deleted document is still read: %+v", docIDs(Result{Hits: got}))
	}
	f.serving(scope, src, spec{"doc-1", "# Dock 1\n\nThe harbour dock schedule 1 lists the tide tables."})
	if got := f.hitsOf(scope, refsOf(found...), false); len(got) != 0 {
		t.Errorf("chunks of the superseded generation %s are still read: %+v", g.GenerationID, docIDs(Result{Hits: got}))
	}

	// The statements a read runs carry the scope and find chunks by the
	// live-chunk index, as Recheck's do.
	type stmt struct {
		sql  string
		args []any
	}
	var seen []stmt
	f.ix.trace = func(q string, args []any) { seen = append(seen, stmt{q, append([]any(nil), args...)}) }
	now := f.search(scope, Query{Text: "harbour", Limit: 10}).Hits
	seen = nil
	f.hitsOf(scope, refsOf(now...), false)
	f.ix.trace = nil
	if len(seen) == 0 {
		t.Fatal("Hits ran no statement the trace seam could see")
	}
	var lookups int
	for _, s := range seen {
		for _, col := range []string{"c.application_id = ?", "c.tenant_id = ?", "c.workspace_id = ?"} {
			if !strings.Contains(s.sql, col) {
				t.Errorf("statement lacks %q:\n%s", col, s.sql)
			}
		}
		if strings.Contains(s.sql, "INDEXED BY") {
			lookups++
			plan := f.queryPlan(s.sql, s.args)
			if len(plan) != 1 || !strings.HasPrefix(plan[0], "SEARCH c USING INDEX ragindex_chunk_live (generation_id=? AND chunk_id=?)") {
				t.Errorf("a ref is not found by the live-chunk index: %v", plan)
			}
		}
	}
	if lookups != len(now) {
		t.Errorf("%d lookups for %d refs", lookups, len(now))
	}

	// Bounds and scope validation fail closed.
	if _, err := f.ix.Hits(ctx, scope, make([]Ref, MaxHitRefs+1), false); contracts.CodeOf(err) != contracts.ErrResourceExhausted {
		t.Errorf("an oversized read: %v", err)
	}
	if _, err := f.ix.Hits(ctx, contracts.Scope{}, refsOf(found...), false); err == nil {
		t.Error("Hits accepted an empty scope")
	}
	if _, err := f.ix.Hits(ctx, f.scope("app", "t1", "w1"), []Ref{{}, {ChunkID: "x"}, {GenerationID: "y"}}, false); err != nil {
		t.Errorf("refs that name nothing are an error: %v", err)
	}
}

// A hidden chunk, another scope's chunk, a deleted one and one that never
// existed all give the same answer: no hit and no error. The answer must not
// tell a caller which of them it hit (no oracle for another tenant's ids, none
// for the existence of hidden text).
func TestHitsHiddenAndForeignRefsAreIndistinguishable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	foreign := f.scope("app", "t2", "w1")

	f.serving(scope, f.source(), spec{"plain", "The orchard sells kiwi fruit every autumn."})
	f.serveHidden(scope, "orchard")
	f.serving(foreign, f.source(), spec{"theirs", "The orchard sells kiwi fruit every autumn."})
	f.serveHidden(foreign, "their-orchard")

	visible := f.search(scope, Query{Text: "kiwi autumn"}).Hits
	all := f.search(scope, Query{Text: "kiwi", Limit: 10, IncludeHidden: true}).Hits
	var hidden Hit
	var found bool
	for _, h := range all {
		if h.Hidden {
			hidden, found = h, true
		}
	}
	if !found {
		t.Fatal("setup: no hidden chunk")
	}
	theirs := f.search(foreign, Query{Text: "kiwi", Limit: 10, IncludeHidden: true}).Hits
	var theirHidden, theirPlain Hit
	for _, h := range theirs {
		if h.Hidden {
			theirHidden = h
		} else if h.DocumentID == "theirs" {
			theirPlain = h
		}
	}
	if theirHidden.ChunkID == "" || theirPlain.ChunkID == "" {
		t.Fatalf("setup: foreign hits %+v", theirs)
	}
	own := visible[0]
	for _, h := range visible {
		if h.DocumentID == "plain" {
			own = h
		}
	}

	// A chunk deleted from the caller's own scope.
	if _, err := f.ix.DeleteDocument(ctx, scope, own.SourceID, "plain"); err != nil {
		t.Fatal(err)
	}

	cases := map[string]Ref{
		"hidden chunk of the caller":                  RefOf(hidden),
		"visible chunk of another tenant":             RefOf(theirPlain),
		"hidden chunk of another tenant":              RefOf(theirHidden),
		"foreign chunk under the caller's generation": {ChunkID: theirPlain.ChunkID, GenerationID: hidden.GenerationID},
		"caller's chunk under a foreign generation":   {ChunkID: hidden.ChunkID, GenerationID: theirPlain.GenerationID},
		"deleted chunk":                               RefOf(own),
		"chunk that never existed":                    {ChunkID: "chk_nothing", GenerationID: "gen_nothing"},
	}
	for name, r := range cases {
		got, err := f.ix.Hits(ctx, scope, []Ref{r}, false)
		if err != nil || got != nil {
			t.Errorf("%s: got %+v, err %v; want no hit and no error", name, got, err)
		}
		// Asked to include hidden text, only the caller's own hidden chunk changes.
		got, err = f.ix.Hits(ctx, scope, []Ref{r}, true)
		if err != nil {
			t.Errorf("%s (include hidden): %v", name, err)
		}
		if name == "hidden chunk of the caller" {
			if len(got) != 1 || !got[0].Hidden || got[0].ChunkID != hidden.ChunkID {
				t.Errorf("%s (include hidden): the caller's own hidden chunk was not read: %+v", name, got)
			}
		} else if got != nil {
			t.Errorf("%s (include hidden): read %+v", name, got)
		}
	}

	// Mixed in one call, only what is visible comes back, and in order.
	visibleNow := f.search(scope, Query{Text: "kiwi", Limit: 10}).Hits
	if len(visibleNow) != 1 {
		t.Fatalf("setup: %d visible chunks after the delete", len(visibleNow))
	}
	mixed := []Ref{RefOf(hidden), RefOf(theirPlain), RefOf(visibleNow[0]), RefOf(theirHidden), {ChunkID: "chk_nothing", GenerationID: "gen_nothing"}}
	got := f.hitsOf(scope, mixed, false)
	if len(got) != 1 || got[0].ChunkID != visibleNow[0].ChunkID || got[0].Hidden {
		t.Errorf("a mixed read returned %+v, want only the visible chunk", got)
	}
	// And no hidden text is in what was returned.
	for _, h := range got {
		if strings.Contains(h.Text, "Ignore all previous rules") {
			t.Errorf("hidden text was returned: %q", h.Text)
		}
	}
}
