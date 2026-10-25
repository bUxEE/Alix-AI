package index

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// fillers are documents that contain none of the test terms. FTS5's BM25 gives
// a term that occurs in more than half of the rows a weight of about zero, so a
// ranking test needs a corpus where its terms are the minority.
func fillers(n int) []spec {
	out := make([]spec, n)
	for i := range out {
		out[i] = spec{fmt.Sprintf("filler-%02d", i), fmt.Sprintf("Cafeteria menu %d offers soup, bread and fruit on weekdays.", i)}
	}
	return out
}

// BM25 ordering (master plan 6): within a scope results come best-first by
// BM25: more occurrences beat fewer, a short chunk beats a long one with the
// same count, a heading match beats a body match, and ties break
// deterministically.
func TestBM25Ordering(t *testing.T) {
	f := newFixture(t)
	scope := f.scope("app", "t1", "w1")
	docs := append(fillers(14),
		spec{"many", "The turbine spins, the turbine hums and the turbine cools."},
		spec{"short", "Turbine overhaul manual."},
		spec{"long", "Turbine " + strings.Repeat("assorted background words fill this passage. ", 8)},
		spec{"none", "Pump overhaul manual."},
		spec{"heading", "# Turbine safety\n\nGeneral notes about operations."},
		spec{"body-only", "# Operations\n\nGeneral notes about turbine safety."},
		spec{"twin-b", "Rotor inspection checklist."},
		spec{"twin-a", "Rotor inspection checklist."},
	)
	f.serving(scope, f.source(), docs...)

	res := f.search(scope, Query{Text: "turbine", Limit: 20})
	got := docIDs(res)
	pos := map[string]int{}
	for i, d := range got {
		pos[d] = i
	}
	for _, want := range []string{"many", "short", "long", "heading", "body-only"} {
		if _, ok := pos[want]; !ok {
			t.Fatalf("%q missing from %v", want, got)
		}
	}
	for _, absent := range []string{"none", "twin-a", "twin-b"} {
		if _, ok := pos[absent]; ok {
			t.Errorf("%q matched but holds no term: %v", absent, got)
		}
	}
	if !(pos["many"] < pos["short"] && pos["short"] < pos["long"]) {
		t.Errorf("tf and length ordering wrong, want many < short < long: %v", got)
	}
	if !(pos["heading"] < pos["body-only"]) {
		t.Errorf("a heading match must outrank the same words in the body: %v", got)
	}

	// Ranks are 1..n, scores are positive and non-increasing.
	for i, h := range res.Hits {
		if h.Rank != i+1 {
			t.Errorf("hit %d has rank %d", i, h.Rank)
		}
		if h.Score <= 0 || h.Score != h.BM25 {
			t.Errorf("hit %s: score %v bm25 %v (no identifier in the query: they must be equal and positive)", h.DocumentID, h.Score, h.BM25)
		}
		if i > 0 && h.Score > res.Hits[i-1].Score {
			t.Errorf("scores increase at rank %d", h.Rank)
		}
	}

	// Equal chunks tie; the order is deterministic (insertion order), run after run.
	for i := 0; i < 3; i++ {
		tie := docIDs(f.search(scope, Query{Text: "rotor checklist", Limit: 5}))
		if !equalStrings(tie, []string{"twin-b", "twin-a"}) {
			t.Fatalf("tie order run %d = %v, want insertion order [twin-b twin-a]", i, tie)
		}
	}

	// Limit cuts the ranking, it does not reorder it.
	top2 := docIDs(f.search(scope, Query{Text: "turbine", Limit: 2}))
	if !equalStrings(top2, got[:2]) {
		t.Errorf("limit 2 = %v, want the first two of %v", top2, got)
	}

	// Any-term versus all-terms.
	any := docIDs(f.search(scope, Query{Text: "turbine overhaul", Limit: 20}))
	all := docIDs(f.search(scope, Query{Text: "turbine overhaul", Limit: 20, MatchAll: true}))
	if any[0] != "short" {
		t.Errorf("the chunk with both terms must lead the any-term result: %v", any)
	}
	sort.Strings(all)
	if !equalStrings(all, []string{"short"}) {
		t.Errorf("all-terms result = %v, want only the chunk holding both", all)
	}
	if len(any) <= len(all) {
		t.Errorf("any-term result %v is not wider than all-terms %v", any, all)
	}

	// Stop words do not make a chunk match, and a query of only stop words still
	// answers instead of failing.
	if _, err := f.ix.Search(context.Background(), scope, Query{Text: "the and of", Limit: 5}); err != nil {
		t.Errorf("a query of only stop words failed: %v", err)
	}
	if got := docIDs(f.search(scope, Query{Text: "what is the turbine", Limit: 20})); len(got) != len(res.Hits) {
		t.Errorf("stop words changed the matches: %v vs %v", got, docIDs(res))
	}
}

// Heading text lives only in the parent chunk (M2-05), so each child is indexed
// with its section title, heading path and table header; queries on those terms
// find the child.
func TestHeadingPathAndTableHeaderAreIndexed(t *testing.T) {
	f := newFixture(t)
	scope := f.scope("app", "t1", "w1")
	docs := append(fillers(10),
		spec{"warranty", "# Warranty terms\n\nParts are covered for two years from delivery."},
		spec{"nested", "# Handbook\n\n## Returns\n\nUnopened items go back within thirty days."},
		spec{"table", "# Price list\n\n| Item | Currency |\n| --- | --- |\n| Widget | EUR |\n| Gadget | USD |\n"},
		spec{"amp", "# R&D budget\n\nSpending was flat this quarter."},
	)
	f.serving(scope, f.source(), docs...)

	for _, c := range []struct {
		query string
		doc   string
		what  string
	}{
		{"warranty", "warranty", "a term found only in the section title"},
		{"handbook", "nested", "a term found only in the outer heading of the path"},
		{"returns", "nested", "a term found only in the nearest heading"},
		{"currency", "table", "a term found only in the table header row"},
		{"price", "table", "a term found only in the heading above a table"},
		{"R&D", "amp", "a heading containing an ampersand"},
		{"budget", "amp", "a word after an ampersand in a heading"},
	} {
		res := f.search(scope, Query{Text: c.query, Limit: 5})
		found := false
		for _, h := range res.Hits {
			if h.DocumentID == c.doc {
				found = true
			}
		}
		if !found {
			t.Errorf("query %q (%s) did not find %s: %s", c.query, c.what, c.doc, joinHits(res))
		}
	}
	// And the context travels with the hit.
	res := f.search(scope, Query{Text: "currency", Limit: 5})
	var row *Hit
	for i := range res.Hits {
		if res.Hits[i].DocumentID == "table" {
			row = &res.Hits[i]
			break
		}
	}
	if row == nil || row.Header == "" || !strings.Contains(row.Header, "Currency") || row.Section != "Price list" {
		t.Fatalf("table row hit lost its context: %+v", row)
	}
	if strings.Contains(row.Text, "Currency") {
		t.Errorf("the header must be context outside the row text: %q", row.Text)
	}
	nested := f.search(scope, Query{Text: "returns", Limit: 5}).Hits[0]
	if len(nested.Path) != 2 || nested.Path[0] != "Handbook" || nested.Path[1] != "Returns" {
		t.Errorf("heading path = %v", nested.Path)
	}
	amp := f.search(scope, Query{Text: "R&D", Limit: 5}).Hits[0]
	if len(amp.Path) != 1 || amp.Path[0] != "R&D budget" {
		t.Errorf("a heading with an ampersand came back as %q (path is stored without HTML escaping)", amp.Path)
	}
}

// Hostile and degenerate query text never reaches FTS5 as syntax and never
// fails the request.
func TestQueryTextIsInertAndNeverBreaksSearch(t *testing.T) {
	f := newFixture(t)
	scope := f.scope("app", "t1", "w1")
	f.serving(scope, f.source(), append(fillers(6), spec{"d", "The turbine and the pump share one housing."})...)
	for _, q := range []string{
		"", " ", "\t\n", `"`, `""`, `"""`, `*`, `**`, `(`, `)`, `NEAR(`, `NEAR(a b)`, `col:`, `text:`, `:`, `^`, `-`, `+`,
		"́", "​", "a\x00b", `AND`, `OR`, `NOT`, `turbine AND`, `OR turbine`, `NOT turbine`, `turbine NOT`,
		`{text}: turbine`, `text : turbine`, `rowid: 1`, `ragindex_fts`, "'", "';--", `"turbine`, `turbine"`, `turb*`,
		strings.Repeat("turbine ", 400), strings.Repeat("(", 500),
	} {
		res, err := f.ix.Search(context.Background(), scope, Query{Text: q})
		if err != nil {
			t.Errorf("query %q failed: %v", q, err)
			continue
		}
		_ = res
	}
	// Operators are plain words: "turbine AND pump" is three words, any of which
	// match, and the NOT in "turbine NOT pump" excludes nothing.
	got := docIDs(f.search(scope, Query{Text: "turbine NOT pump"}))
	if !equalStrings(got, []string{"d"}) {
		t.Errorf("NOT acted as an operator: %v", got)
	}
	// Prefix syntax is inert: "turb*" is the word "turb", which matches nothing.
	if got := f.search(scope, Query{Text: "turb*"}); len(got.Hits) != 0 {
		t.Errorf("a prefix operator expanded: %s", joinHits(got))
	}
	if _, err := f.ix.Search(context.Background(), scope, Query{Text: strings.Repeat("x", MaxQueryBytes+1)}); err == nil {
		t.Error("an over-long query was accepted")
	}
	if _, err := f.ix.Search(context.Background(), scope, Query{Text: "\xff\xfe"}); err == nil {
		t.Error("an invalid UTF-8 query was accepted")
	}
	if _, err := f.ix.Search(context.Background(), scope, Query{Text: "turbine", Limit: DefaultMaxLimit + 1}); err == nil {
		t.Error("a limit over the maximum was accepted")
	}
}
