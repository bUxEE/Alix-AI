package index

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestExtractIdentifiers(t *testing.T) {
	raws := func(s string) []string {
		var out []string
		for _, id := range extractIdentifiers(s, 20) {
			out = append(out, id.Raw)
		}
		return out
	}
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"order ABC-123 shipped", []string{"ABC-123"}},
		{"SKU12345 and sku-9921-b", []string{"SKU12345", "sku-9921-b"}},
		{"PO-2024-0017.", []string{"PO-2024-0017"}},
		{"invoice 10293847 is due", []string{"10293847"}},
		{"in 2024 we sold 12 units", nil},
		{"the well-known fix", nil},
		{"call get_user_by_id then retry", []string{"get_user_by_id"}},
		{"error ERR_4021 and ERR-AUTH-EXPIRED", []string{"ERR_4021", "ERR-AUTH-EXPIRED"}},
		{"version 1.2.3 of 192.168.1.1", []string{"1.2.3", "192.168.1.1"}},
		{"ABC-123, ABC-123 and ABC123", []string{"ABC-123"}}, // one identifier, three spellings
		{"trailing dash ABC- and -ABC", nil},
		{"e.g. this", nil},
		{"A1", nil}, // too short
		{"Wi-Fi 6E ports", nil},
		{strings.Repeat("a1", 70), nil}, // longer than any identifier
		{"ORD-ÀÈ-77", []string{"ORD-ÀÈ-77"}},
	} {
		if got := raws(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("extractIdentifiers(%q) = %v, want %v", c.in, got, c.want)
		}
	}

	id := extractIdentifiers("ABC-123", 4)[0]
	if !reflect.DeepEqual(id.Keys, []string{"abc-123", "abc123"}) || !reflect.DeepEqual(id.Words, []string{"abc", "123"}) {
		t.Errorf("keys %v words %v", id.Keys, id.Words)
	}
	if got := extractIdentifiers("a_b-9 c.d_1 e-f-2 g_3 h_4", 3); len(got) != 3 {
		t.Errorf("limit not honoured: %d identifiers", len(got))
	}
	// Separator variants meet in the squashed key.
	k1 := extractIdentifiers("ABC-123", 1)[0].Keys
	k2 := extractIdentifiers("abc_123", 1)[0].Keys
	k3 := extractIdentifiers("ABC123", 1)[0].Keys
	if k1[0] != k2[0] || k1[1] != k3[0] {
		t.Errorf("variants do not share keys: %v %v %v", k1, k2, k3)
	}
}

// Exact identifiers (master plan 12.4) are matched whole and boosted: a chunk
// holding the exact code outranks chunks that BM25 alone prefers, a query
// typed without the separators still finds it, and an identifier that is not
// in the scope matches nothing.
func TestExactIdentifierBoost(t *testing.T) {
	boosted := newFixture(t)
	plain := newFixture(t, Config{ExactIdentifierBoost: -1}) // boost disabled: the control
	docs := append(fillers(14),
		// The chunk with the code is long and mentions "price" once. The query below
		// writes the code without its hyphen, so the tokenizer cannot match it (it
		// indexed "abc" and "1234") and BM25 sees only the word "price".
		spec{"has-code", "Part ABC-1234 is a replacement gasket for the older pump series, shipped in packs of ten. " +
			"It is listed for completeness only, together with its price, and its dimensions are given on the drawing. " + strings.Repeat("Further notes follow. ", 6)},
		// The decoys are short and repeat the query's other words.
		spec{"decoy-1", "Price price price list."},
		spec{"decoy-2", "Current price list, price changes announced."},
		spec{"has-parts", "# Parts ABC-5678 overview\n\nThis heading carries a code; the paragraph below it does not."},
		spec{"has-variant", "Spare abc_9000 stock is held at the depot."},
	)
	check := func(f *fixture) Result {
		scope := f.scope("app", "t1", "w1")
		f.serving(scope, f.source(), docs...)
		return f.search(scope, Query{Text: "price ABC1234", Limit: 5})
	}

	ctl := check(plain)
	if len(ctl.Hits) == 0 || ctl.Hits[0].DocumentID == "has-code" {
		t.Fatalf("control: BM25 alone must prefer a decoy over the long chunk with the code, or the test proves nothing: %s", joinHits(ctl))
	}

	res := check(boosted)
	if len(res.Hits) == 0 || res.Hits[0].DocumentID != "has-code" {
		t.Fatalf("the chunk with the exact identifier must rank first: %s", joinHits(res))
	}
	top := res.Hits[0]
	if len(top.ExactIdentifiers) != 1 || top.ExactIdentifiers[0] != "ABC1234" {
		t.Errorf("ExactIdentifiers = %v", top.ExactIdentifiers)
	}
	if top.Score < DefaultExactIdentifierBoost || top.BM25 <= 0 || top.Score-top.BM25 != DefaultExactIdentifierBoost {
		t.Errorf("score %v = bm25 %v + boost %v does not add up", top.Score, top.BM25, DefaultExactIdentifierBoost)
	}
	for _, h := range res.Hits[1:] {
		if len(h.ExactIdentifiers) != 0 {
			t.Errorf("%s claims an exact identifier it does not hold", h.DocumentID)
		}
	}

	scope := boosted.scope("app", "t1", "w1")
	if got := boosted.search(scope, Query{Text: "price ABC-1234", Limit: 5}); len(got.Hits) == 0 || got.Hits[0].DocumentID != "has-code" {
		t.Errorf("hyphenated query: %s", joinHits(got))
	}
	// Typed without the separator, in another case, or with another separator:
	// the same chunk, found through the identifier key and not through the
	// tokenizer (which would see "abc1234", a different word).
	for _, q := range []string{"ABC1234", "abc1234", "abc_1234", "Abc.1234", "ABC-1234"} {
		got := boosted.search(scope, Query{Text: q, Limit: 3})
		if len(got.Hits) == 0 || got.Hits[0].DocumentID != "has-code" {
			t.Errorf("query %q: %s", q, joinHits(got))
		}
	}
	// A code in a section title is found as well, and so is a variant spelling.
	if got := boosted.search(scope, Query{Text: "ABC-5678", Limit: 3}); len(got.Hits) == 0 || got.Hits[0].DocumentID != "has-parts" {
		t.Errorf("identifier in a heading: %s", joinHits(got))
	}
	if got := boosted.search(scope, Query{Text: "ABC-9000", Limit: 3}); len(got.Hits) == 0 || got.Hits[0].DocumentID != "has-variant" {
		t.Errorf("identifier spelled with another separator: %s", joinHits(got))
	}
	// An identifier nobody holds matches no chunk by identifier.
	if got := boosted.search(scope, Query{Text: "ABC-0000", Limit: 3}); len(got.Hits) != 0 {
		for _, h := range got.Hits {
			if len(h.ExactIdentifiers) != 0 {
				t.Errorf("an unknown identifier matched %s exactly", h.DocumentID)
			}
		}
	}
	// A different scope with no such code finds nothing for it.
	other := boosted.scope("app", "t2", "w1")
	boosted.serving(other, boosted.source(), spec{"x", "Unrelated page about weather."})
	if got := boosted.search(other, Query{Text: "ABC-1234", Limit: 3}); len(got.Hits) != 0 {
		t.Errorf("another scope found the identifier: %s", joinHits(got))
	}
	// An identifier-only query does not need any text term to match.
	only := boosted.search(scope, Query{Text: "ABC1234", Limit: 3})
	if len(only.Hits) == 0 || only.Hits[0].BM25 != 0 {
		t.Errorf("identifier-only match by squashed key should have no text score: %s", joinHits(only))
	}
	// MatchAll with an identifier still finds it through the identifier lane.
	if got := boosted.search(scope, Query{Text: "ABC1234 gasket", Limit: 3, MatchAll: true}); len(got.Hits) == 0 || got.Hits[0].DocumentID != "has-code" {
		t.Errorf("MatchAll: %s", joinHits(got))
	}
}

// Identifiers follow their chunk: a replaced or deleted document leaves none
// behind, and a tombstoned chunk's identifier is not found.
func TestIdentifierRowsFollowTheirChunk(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	scope := f.scope("app", "t1", "w1")
	src := f.source()
	g := f.serving(scope, src, spec{"d", "Gasket ABC-1234 in stock."})
	if n := f.count(`SELECT count(*) FROM ragindex_ident`); n != 2 {
		t.Fatalf("identifier rows = %d, want the dashed and the squashed key", n)
	}
	if _, err := f.ix.PutDocument(ctx, scope, string(g.GenerationID), f.doc("d", "v2", "Gasket XYZ-9 in stock.")); err != nil {
		t.Fatal(err)
	}
	if got := f.search(scope, Query{Text: "ABC-1234"}); len(got.Hits) != 0 {
		t.Errorf("the replaced document's identifier is still found: %s", joinHits(got))
	}
	if n := f.count(`SELECT count(*) FROM ragindex_ident WHERE ident LIKE 'abc%'`); n != 0 {
		t.Errorf("%d stale identifier rows", n)
	}
	if _, err := f.ix.DeleteDocument(ctx, scope, src, "d"); err != nil {
		t.Fatal(err)
	}
	if got := f.search(scope, Query{Text: "XYZ-9"}); len(got.Hits) != 0 {
		t.Errorf("a tombstoned chunk is found by its identifier: %s", joinHits(got))
	}
}
