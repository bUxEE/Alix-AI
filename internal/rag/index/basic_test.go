package index

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/store/storetest"
)

// Every table of the module, the FTS5 table included, carries the scope
// columns (master plan 10.2).
func TestMigrationsScoped(t *testing.T) {
	f := newFixture(t)
	storetest.AssertScopedTables(t, f.db)
}

func TestSearchRoundTrip(t *testing.T) {
	f := newFixture(t)
	scope := f.scope("app", "t1", "w1")
	src := f.source()
	g := f.serving(scope, src,
		spec{"doc-a", "# Refunds\n\nCustomers may request a refund within thirty days of purchase."},
		spec{"doc-b", "# Shipping\n\nParcels leave the warehouse on weekdays."},
	)
	res := f.search(scope, Query{Text: "refund policy"})
	if len(res.Hits) != 1 || res.Hits[0].DocumentID != "doc-a" {
		t.Fatalf("hits = %s", joinHits(res))
	}
	h := res.Hits[0]
	if h.GenerationID != string(g.GenerationID) || h.SourceID != src || h.DocumentVersion != "v1" || h.Rank != 1 {
		t.Errorf("hit metadata wrong: %+v", h)
	}
	if h.Section != "Refunds" || len(h.Path) != 1 || h.Path[0] != "Refunds" {
		t.Errorf("heading context lost: section %q path %v", h.Section, h.Path)
	}
	if h.Score <= 0 || h.BM25 <= 0 {
		t.Errorf("score %v bm25 %v, want positive", h.Score, h.BM25)
	}
	if res.Version.Epoch == 0 || len(res.Version.Active) != 1 {
		t.Errorf("version %+v", res.Version)
	}
	_ = context.Background()
}
