package history

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/internal/upgrade"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// Master plan 18: an isolated derived index is reported, not silently empty. The
// upgrade empties a damaged search index and records it in its quarantine; a
// search that ran anyway would answer "nothing matched" to every question. The
// answer is unavailable, a typed error the gateway carries to the client (the API
// page has no field a label could ride in).
func TestIsolatedSearchIndexIsUnavailableNotEmpty(t *testing.T) {
	ctx := context.Background()
	var q *upgrade.Quarantine
	e := newEnv(t, withDeps(func(d *Deps) {
		q = upgrade.NewQuarantine(t.TempDir(), d.Clock)
		d.Quarantine = q
	}))
	me := scopeOf(testApp, tenA, ws1, alice, "ses_1")
	e.say(me, "the turbine is vibrating", "check the turbine bearings first")
	search := func() (SearchPage, error) { return e.svc.Search(ctx, me, SearchQuery{Text: "turbine"}) }

	// The name this package consults is the one the upgrade finds and isolates.
	var index upgrade.DerivedIndex
	idx, err := upgrade.FTSIndexes(ctx, e.db)
	noErr(t, err)
	for _, ix := range idx {
		if ix.Name() == SearchIndexName {
			index = ix
		}
	}
	if index == nil {
		t.Fatalf("the upgrade does not list %s among the full-text indexes of the database", SearchIndexName)
	}

	if page, err := search(); err != nil || len(page.Hits) != 2 {
		t.Fatalf("control: %+v, %v: a healthy index answers in full", page, err)
	}

	// What the upgrade does to a damaged index.
	noErr(t, q.Add(SearchIndexName, "test: damaged"))
	noErr(t, index.Isolate(ctx))
	if n := e.ftsMatches("turbine"); n != 0 {
		t.Fatalf("the isolated index still holds %d entries: the fixture did not isolate it", n)
	}

	page, err := search()
	wantCode(t, err, contracts.ErrUnavailable)
	if len(page.Hits) != 0 {
		t.Errorf("page = %+v: an isolated index answered with hits", page)
	}
	if ce, ok := err.(*contracts.Error); !ok || ce.Details["index"] != SearchIndexName {
		t.Errorf("the error does not name the index: %v", err)
	}
	// Validation comes first: a bad query is still the caller's error.
	if _, err := e.svc.Search(ctx, me, SearchQuery{Text: "   "}); err == nil {
		t.Error("an empty query was answered while the index is isolated")
	}
	// The capability is still checked first.
	if _, err := e.svc.Search(ctx, with(me, CapRead), SearchQuery{Text: "turbine"}); err == nil {
		t.Error("a caller without the search capability learnt the state of the index")
	}

	t.Run("without a quarantine the same search is an empty page that says nothing: the hazard", func(t *testing.T) {
		bare := newEnv(t)
		bare.say(me, "the turbine is vibrating", "check the turbine bearings first")
		bareIdx, err := upgrade.FTSIndexes(ctx, bare.db)
		noErr(t, err)
		for _, ix := range bareIdx {
			noErr(t, ix.Isolate(ctx)) // emptied, and no quarantine told the service
		}
		page, err := bare.svc.Search(ctx, me, SearchQuery{Text: "turbine"})
		noErr(t, err)
		if len(page.Hits) != 0 {
			t.Fatalf("page = %+v", page)
		}
	})

	// Rebuilt from the transcript and released: the label goes and the hits are back.
	noErr(t, index.Rebuild(ctx))
	noErr(t, q.Clear(SearchIndexName))
	e.checkIndex()
	if page, err := search(); err != nil || len(page.Hits) != 2 {
		t.Errorf("after the rebuild: %+v, %v", page, err)
	}
}
