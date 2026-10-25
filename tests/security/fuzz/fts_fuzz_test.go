package fuzzsec

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/rag/chunk"
	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/internal/store/storetest"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/rag"
)

// ftsTenant is one tenant of the fuzz index: its scope, its source and the two
// strings that identify its rows (a tag every chunk carries and a marker only
// the first document carries). Both are generated per run.
type ftsTenant struct {
	scope  contracts.Scope
	source string
	tag    string
	marker string
}

// ftsRig is a real FTS5 index over a throwaway SQLite database holding two
// tenants. The documents are plain words, so that no chunk holds an
// identifier-shaped token: the identifier path of the index then adds nothing
// and a query's hit set is decided by the quoted terms alone.
type ftsRig struct {
	db   *store.DB
	ix   *index.Index
	a, b ftsTenant
}

// ftsCorpus is the document set of every tenant (the tag and the marker are
// substituted). The words are chosen so that the literal reading of a query and
// its FTS5-operator reading give different hit sets:
//
//	vault   heading "Secret vault" (a term in the section column) and the marker
//	notes   "secret" only in the body, "banana" and "cherry"
//	fruit   "apple banana cherry" together
//	order   "alpha ... delta" three words apart (NEAR)
//	prefix  "abcdef abcxyz" (a prefix query "abc*")
//	colon   "column value" (the words of "column : value")
var ftsCorpus = []struct{ id, md string }{
	{"vault", "# Secret vault\n\nThe {marker} marker is stored here with apple {tag}.\n"},
	{"notes", "# Notes\n\nSome secret remarks about banana and cherry {tag}.\n"},
	{"fruit", "# Fruit\n\napple banana cherry together {tag}.\n"},
	{"order", "# Order\n\nalpha beta gamma delta epsilon {tag}.\n"},
	{"prefix", "# Prefix\n\nabcdef abcxyz are long words {tag}.\n"},
	{"colon", "# Colon\n\ncolumn value pairs appear here {tag}.\n"},
}

func newFTSRig(tb testing.TB) *ftsRig {
	tb.Helper()
	reg := store.NewRegistry()
	if err := reg.Register(index.Module, index.Migrations()); err != nil {
		tb.Fatal(err)
	}
	db := storetest.Open(tb, reg)
	ix, err := index.New(db, clock.NewFake(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)), index.Config{})
	if err != nil {
		tb.Fatal(err)
	}
	chk, err := chunk.New(chunk.DefaultConfig())
	if err != nil {
		tb.Fatal(err)
	}
	r := &ftsRig{db: db, ix: ix}
	build := func(tenant string) ftsTenant {
		ten := ftsTenant{
			scope:  storetest.Scope("fuzz-app", tenant, "ws-1"),
			source: ids.New("src"),
			tag:    "tag" + randWord(tb, 12),
			marker: "mk" + randWord(tb, 12),
		}
		ctx := context.Background()
		gen, err := ix.BeginGeneration(ctx, ten.scope, ten.source, index.Components{
			Parser:   rag.ComponentVersion{Name: "alixai-parser", Version: "1.0.0"},
			Chunker:  chk.Component(),
			Embedder: &rag.ComponentVersion{Name: "granite-embedding", Version: "97m"},
		})
		if err != nil {
			tb.Fatalf("BeginGeneration: %v", err)
		}
		for _, d := range ftsCorpus {
			md := strings.NewReplacer("{tag}", ten.tag, "{marker}", ten.marker).Replace(d.md)
			res, err := chk.Chunk(ctx, chunk.FromMarkdown(d.id, md))
			if err != nil {
				tb.Fatalf("chunking %s: %v", d.id, err)
			}
			if _, err := ix.PutDocument(ctx, ten.scope, string(gen.GenerationID), index.Document{ID: d.id, Version: "v1", Chunks: res}); err != nil {
				tb.Fatalf("PutDocument %s: %v", d.id, err)
			}
		}
		if _, err := ix.ValidateGeneration(ctx, ten.scope, string(gen.GenerationID), index.Expectation{}); err != nil {
			tb.Fatalf("ValidateGeneration: %v", err)
		}
		if err := ix.ActivateGeneration(ctx, ten.scope, string(gen.GenerationID)); err != nil {
			tb.Fatalf("ActivateGeneration: %v", err)
		}
		return ten
	}
	r.a, r.b = build("tenant-a"), build("tenant-b")
	// The premise of the differential: no chunk of the corpus holds an
	// identifier. If the chunker or the identifier rules ever change that, the
	// check says so instead of passing on a different premise.
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM ragindex_ident`).Scan(&n); err != nil {
		tb.Fatal(err)
	}
	if n != 0 {
		tb.Fatalf("the corpus holds %d identifier rows; the fuzz corpus must hold none", n)
	}
	return r
}

func (r *ftsRig) tenant(second bool) (own, other ftsTenant) {
	if second {
		return r.b, r.a
	}
	return r.a, r.b
}

// search runs the production search of one tenant over the whole corpus (the
// limit is far above the corpus size, so the hit set is never cut).
func (r *ftsRig) search(ctx context.Context, own ftsTenant, text string, matchAll bool) (index.Result, error) {
	return r.ix.Search(ctx, own.scope, index.Query{Text: text, Limit: index.DefaultMaxLimit, MatchAll: matchAll})
}

// docsOf returns the sorted document ids of the hits.
func docsOf(res index.Result) []string {
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.DocumentID)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// rawMatch is the DELIBERATELY UNQUOTED builder stand-in: the text goes to the
// same FTS5 table as the MATCH expression itself, with no scope predicate at
// all. It exists to prove that the checks around it can see operator injection
// and a cross-tenant row if either ever happened. It returns the document ids
// (and tenants) of the rows, or the SQLite error.
func (r *ftsRig) rawMatch(ctx context.Context, expr string) (docs []string, tenants map[string]bool, err error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT c.document_id, c.tenant_id FROM ragindex_fts CROSS JOIN ragindex_chunk AS c ON c.rid = ragindex_fts.rowid WHERE ragindex_fts MATCH ?`, expr)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	tenants = map[string]bool{}
	for rows.Next() {
		var d, t string
		if err := rows.Scan(&d, &t); err != nil {
			return nil, nil, err
		}
		docs = append(docs, d)
		tenants[t] = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	slices.Sort(docs)
	return slices.Compact(docs), tenants, nil
}

// oracleWords are the words a query is made of, by the contract the index
// documents (matchexpr.go): runs of letters, digits and combining marks,
// lower-cased by the index; everything else separates words. Joined by single
// spaces they are the neutral spelling of any query: the same words with no
// punctuation left to act as syntax. It is deliberately the contract, not a copy
// of the builder (no stop words, no term cap): those apply identically to both
// spellings, so the hit sets must be equal.
func oracleWords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r))
	})
}

// checkFTSSearch is every property of one search. It returns a description of
// the first violation, "" when none. It never calls tb.Fatal (it may run on a
// helper goroutine).
func checkFTSSearch(r *ftsRig, text string, matchAll, second bool) string {
	own, other := r.tenant(second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	res, err := r.search(ctx, own, text, matchAll)
	if took := time.Since(start); took > 10*time.Second {
		return fmt.Sprintf("the search took %s", took)
	}
	if err != nil {
		// A refusal is fine when the text is one the index documents as refused.
		// Anything else is FTS5 rejecting the expression (ErrInternal) or a row of
		// another scope reaching the result (ErrScopeViolation).
		if contracts.CodeOf(err) == contracts.ErrValidationFailed && (!utf8.ValidString(text) || len(text) > index.MaxQueryBytes) {
			return ""
		}
		return fmt.Sprintf("Search(%q) failed with code %q: %v", text, contracts.CodeOf(err), err)
	}
	if len(res.Hits) > index.DefaultMaxLimit {
		return fmt.Sprintf("%d hits over the limit of %d", len(res.Hits), index.DefaultMaxLimit)
	}
	if msg := foreignHit(own, other, res); msg != "" {
		return msg
	}

	// Differential: the operator syntax in the text changed nothing. The same
	// words with the punctuation removed must give the same hit set.
	canon := strings.Join(oracleWords(text), " ")
	if !utf8.ValidString(text) || len(text) > index.MaxQueryBytes || len(canon) > index.MaxQueryBytes {
		return ""
	}
	if canon == "" {
		if len(res.Hits) != 0 {
			return fmt.Sprintf("a text with no words returned %d hits", len(res.Hits))
		}
		return ""
	}
	res2, err := r.search(ctx, own, canon, matchAll)
	if err != nil {
		return fmt.Sprintf("Search(%q), the words of %q, failed: %v", canon, text, err)
	}
	if a, b := docsOf(res), docsOf(res2); !slices.Equal(a, b) {
		return fmt.Sprintf("punctuation changed the hit set: %q gives %v, its words %q give %v", text, a, canon, b)
	}
	return ""
}

// foreignHit applies the scope rules to a result: every hit is a chunk of the
// caller's source and corpus, none is the other tenant's, none is hidden.
func foreignHit(own, other ftsTenant, res index.Result) string {
	for _, h := range res.Hits {
		switch {
		case h.SourceID != own.source:
			return fmt.Sprintf("hit of source %q, the caller's is %q", h.SourceID, own.source)
		case strings.Contains(h.Text, other.tag) || strings.Contains(h.Text, other.marker):
			return "a chunk of the other tenant was returned: " + h.Text
		case !strings.Contains(h.Text, own.tag):
			return "a hit that is not a chunk of the caller's corpus: " + h.Text
		case h.Hidden:
			return "a hidden chunk was returned"
		}
	}
	return ""
}

// ftsSeeds are the pathological queries: every FTS5 operator and special form,
// the unbalanced and the unicode quotes, and the words of the corpus.
func ftsSeeds(r *ftsRig) []string {
	return []string{
		"apple", "apple banana", "banana cherry",
		"title:secret", "section:secret", "text:apple", "path:fruit", "header:x", "tenant_id:tenant-b", "{section text}: secret",
		"NEAR(alpha delta, 1)", "NEAR(a b, 1)", "NEAR(", "NEAR/2", "alpha NEAR delta",
		"a*", "abc*", "ab* OR x*", "*", "**", `"abc"*`,
		`"unbalanced`, `"apple banana`, `apple"`, `""`, `"`, `"""`, `"a" "b`,
		"apple OR banana", "apple AND banana", "apple NOT banana", "apple AND NOT banana", "AND", "OR", "NOT", "NEAR", "AND OR NOT NEAR",
		"-x", "-apple", "^x", "^apple", "+apple", "apple -banana",
		"column : value", "column:value", ":", "::", "a:b:c",
		"( ( (", "))))", "(apple", "(apple OR banana) AND (cherry OR delta)", "()",
		"rank", "rowid", "bm25(ragindex_fts)", "ragindex_fts", "highlight", "MATCH", "fts5", "*:*",
		"“apple banana”", "＂apple＂", "‘apple’", "«apple»", "„“",
		"apple'; DROP TABLE ragindex_chunk; --", "apple\x00banana", "apple\nbanana\r\ncherry", "apple\tOR\tbanana",
		r.a.marker, r.b.marker, r.a.marker + " OR " + r.b.marker, r.b.marker + " AND " + r.a.tag, "tag" + r.b.tag,
		strings.Repeat("apple ", 3000), strings.Repeat("a", 3000), strings.Repeat("zz", 2100),
		strings.Repeat("é", 400), "́́", "\xff\xfe", "ABC-123 abc/def a.b.c x_y",
	}
}

// SEC-024
// FuzzFTSQueryBuilder: arbitrary user text reaches the index
// only as quoted terms. It drives the production search of a real FTS5 index
// (internal/rag/index over a temporary SQLite database) with two tenants and
// asserts, for any text: no panic; the produced MATCH expression is accepted by
// SQLite FTS5 (an expression FTS5 rejects comes back as an internal error, which
// is never an acceptable answer to user text); the hits are within the caller's
// scope; and no FTS5 syntax in the text acted as syntax (the same words without
// the punctuation give the same hit set).
//
// The expression itself is not exported, so the "every term is a double-quoted
// string" property is asserted by its behaviour, not by tokenising it (NEEDS in
// the report: an exported index.MatchExpression, or an exported trace hook).
//
// The controls run before the fuzzing: a deliberately unquoted stand-in on the
// same table shows that FTS5 syntax errors, column filters, NEAR, prefix, NOT
// and AND do change a result, and that without the scope predicate the other
// tenant's rows come back. The checks above therefore can see what they say they
// cannot see.
//
//	go test -run '^$' -fuzz '^FuzzFTSQueryBuilder$' -fuzztime 5s ./tests/security/fuzz/
func FuzzFTSQueryBuilder(f *testing.F) {
	r := newFTSRig(f)
	ftsControls(f, r)
	for _, s := range ftsSeeds(r) {
		f.Add(s, false, false)
	}
	f.Add("apple banana", true, true)
	f.Add("section:secret", true, false)
	f.Fuzz(func(t *testing.T, text string, matchAll, second bool) {
		var msg string
		within(t, 60*time.Second, "the search", func() { msg = checkFTSSearch(r, text, matchAll, second) })
		if msg != "" {
			t.Fatal(msg)
		}
	})
}

// ftsControls proves that the properties of the fuzz target are able to fail.
func ftsControls(f *testing.F, r *ftsRig) {
	f.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Positive control: an ordinary query finds its documents, and only its own.
	res, err := r.search(ctx, r.a, "apple", false)
	if err != nil {
		f.Fatalf("control: an ordinary query failed: %v", err)
	}
	if got, want := docsOf(res), []string{"fruit", "vault"}; !slices.Equal(got, want) {
		f.Fatalf("control: \"apple\" returned %v, want %v", got, want)
	}
	if msg := checkFTSSearch(r, "apple", false, false); msg != "" {
		f.Fatalf("control: a plain query violates a property: %s", msg)
	}

	// The unquoted stand-in: operator injection is visible.
	type probe struct {
		name, text string
		// rawError: the stand-in must hit an FTS5 error. Otherwise its row set must
		// differ from the production search of the same text.
		rawError bool
	}
	for _, p := range []probe{
		{"column filter", "section:secret", false},
		{"unknown column", "title:secret", true},
		{"NEAR", "NEAR(alpha delta, 1)", false},
		{"prefix", "abc*", false},
		{"AND", "apple AND banana", false},
		{"NOT", "apple NOT banana", false},
		{"initial token", "^apple", false},
		{"unbalanced quote", `"apple banana`, true},
		{"bare operator", "AND", true},
		{"hyphenated word", "other-tenant-term", true},
	} {
		raw, _, rawErr := r.rawMatch(ctx, p.text)
		prod, perr := r.search(ctx, r.a, p.text, false)
		if perr != nil {
			f.Fatalf("control %s: the production search failed: %v", p.name, perr)
		}
		switch {
		case p.rawError && rawErr == nil:
			f.Fatalf("control %s: the unquoted stand-in accepted %q (%v): the test cannot see a syntax error", p.name, p.text, raw)
		case !p.rawError && rawErr != nil:
			f.Fatalf("control %s: the unquoted stand-in failed: %v", p.name, rawErr)
		case !p.rawError && slices.Equal(raw, docsOf(prod)):
			f.Fatalf("control %s: the unquoted stand-in and the production search agree on %v: the test cannot see this operator", p.name, raw)
		}
	}

	// The unquoted stand-in without a scope predicate returns the other tenant:
	// a leak, if production ever behaved like it.
	_, tenants, err := r.rawMatch(ctx, r.a.marker+" OR "+r.b.marker)
	if err != nil {
		f.Fatalf("control: %v", err)
	}
	if !tenants["tenant-a"] || !tenants["tenant-b"] {
		f.Fatalf("control: the unscoped stand-in returned tenants %v, want both: the leak detector is blind", tenants)
	}
	for _, text := range []string{r.b.marker, r.a.marker + " OR " + r.b.marker} {
		res, err := r.search(ctx, r.a, text, false)
		if err != nil {
			f.Fatalf("control: %v", err)
		}
		if msg := foreignHit(r.a, r.b, res); msg != "" {
			f.Fatalf("tenant a, query %q: %s", text, msg)
		}
	}
	// And the scope check itself flags a planted foreign hit.
	planted := index.Result{Hits: []index.Hit{{SourceID: r.a.source, Text: "x " + r.b.tag + " " + r.a.tag}}}
	if foreignHit(r.a, r.b, planted) == "" {
		f.Fatal("control: the scope check did not flag a planted foreign chunk")
	}
}
