package ragsec

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/buxee/alix-ai/internal/rag/index"
	"github.com/buxee/alix-ai/internal/rag/query"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// ftsWord is a random lower-case word of n letters. Markers and identifying
// words are generated per run, and letters only, so that no identifier-shaped
// token (a digit or an inner separator) can arise from them.
func ftsWord(t testing.TB, n int) string {
	t.Helper()
	const letters = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, n)
	for i := range b {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
		if err != nil {
			t.Fatal(err)
		}
		b[i] = letters[v.Int64()]
	}
	return string(b)
}

// ftsSections are the sections of every tenant's one document. {id} is a word
// unique to the section (so a result says which section it came from), {tag}
// the tenant's word and {marker} the tenant's secret. The words are chosen so
// that the literal reading of a query and its FTS5 reading give different rows:
//
//	vault   "secret" in the heading (the section column), plus the marker
//	notes   "secret" only in the body, and the word "and"
//	fruit   "apple banana cherry" together, "apple" the first word of the text
//	order   "alpha" and "delta" three words apart (NEAR)
//	prefix  "abcdef abcxyz" (a prefix query "abc*")
//	colon   "column value" (the words of "column : value")
//
// No section has the word "a", "x" or "near", and none has a digit or a
// separator inside a word, so no chunk is identifier-shaped.
var ftsSections = []struct{ name, md string }{
	{"vault", "# Secret vault\n\nThe {marker} marker is stored here with apple. {id} {tag}\n\n"},
	{"notes", "# Notes\n\nSome secret remarks about banana and cherry. {id} {tag}\n\n"},
	{"fruit", "# Fruit\n\napple banana cherry together. {id} {tag}\n\n"},
	{"order", "# Order\n\nalpha beta gamma delta epsilon. {id} {tag}\n\n"},
	{"prefix", "# Prefix\n\nabcdef abcxyz are long words. {id} {tag}\n\n"},
	{"colon", "# Colon\n\ncolumn value pairs appear here. {id} {tag}\n\n"},
}

// ftsFloodSections is how many sections hold the flood word: far more than the
// candidate cap of the query pipeline, so that a cap that did not bite is seen.
const ftsFloodSections = 120

// ftsTenant is one tenant of the test: its scope, its source and what identifies
// its rows.
type ftsTenant struct {
	name   string
	scope  contracts.Scope
	src    *src
	tag    string
	marker string
	// ids maps a section name to the word that only that section of this tenant has.
	ids map[string]string
}

// docsIn returns the sorted names of the sections whose identifying word is in
// any of the texts, and the texts that are of no section of the tenant.
func (ten *ftsTenant) docsIn(texts []string) (docs, strangers []string) {
	for _, tx := range texts {
		found := false
		for name, id := range ten.ids {
			if strings.Contains(tx, id) {
				docs = append(docs, name)
				found = true
			}
		}
		if !found {
			strangers = append(strangers, tx)
		}
	}
	slices.Sort(docs)
	return slices.Compact(docs), strangers
}

// ftsCorpus is two tenants of one application with look-alike documents.
type ftsCorpus struct {
	*world
	a, b  *ftsTenant
	flood string
}

func newFTSCorpus(t *testing.T) *ftsCorpus {
	t.Helper()
	w := newWorld(t, ftsOnly()) // full text only: a vector stage returns its top K whatever the match and would blur every "returns nothing"
	c := &ftsCorpus{world: w, flood: ftsWord(t, 30)}
	build := func(name string) *ftsTenant {
		ten := &ftsTenant{
			name:   name,
			scope:  scopeFor(name, "ws-1", "usr-"+name),
			tag:    "tag" + ftsWord(t, 12),
			marker: "mk" + ftsWord(t, 12),
			ids:    map[string]string{"flood": "fl" + ftsWord(t, 12)},
		}
		var md strings.Builder
		for _, sec := range ftsSections {
			ten.ids[sec.name] = "id" + ftsWord(t, 12)
			md.WriteString(strings.NewReplacer("{id}", ten.ids[sec.name], "{tag}", ten.tag, "{marker}", ten.marker).Replace(sec.md))
		}
		md.WriteString("# Flood\n\n")
		for range ftsFloodSections {
			// The heading is a random word, never a number: a number would be a
			// word of the query "NEAR(a b, 1)" and match every section.
			fmt.Fprintf(&md, "## Part %s\n\n%s %s filler %s\n\n", ftsWord(t, 8), c.flood, ten.tag, ten.ids["flood"])
		}
		ten.src = w.addSource(ten.scope, "library-"+name)
		ten.src.writeString("notes.md", md.String())
		w.syncOK(ten.src)
		return ten
	}
	c.a, c.b = build("tenant-a"), build("tenant-b")
	return c
}

// ftsRaw is the DELIBERATELY UNQUOTED builder stand-in. It sends the text to the
// real FTS5 table of the world as the MATCH expression itself, with no quoting
// at all. With scope true it joins the chunk table on the tenant's scope columns
// (so that what it shows is the effect of the operators, not of another
// tenant's rows); with scope false it has no scope predicate (a leak, were
// production built like this). It returns the texts of the rows or the SQLite
// error.
func (c *ftsCorpus) ftsRaw(expr string, own *ftsTenant, scope bool) ([]string, map[string]bool, error) {
	q := `SELECT c.text, c.tenant_id FROM ragindex_fts CROSS JOIN ragindex_chunk AS c ON c.rid = ragindex_fts.rowid WHERE ragindex_fts MATCH ?`
	args := []any{expr}
	if scope {
		q += ` AND c.application_id = ? AND c.tenant_id = ? AND c.workspace_id = ?`
		args = append(args, own.scope.ApplicationID, own.scope.TenantID, own.scope.WorkspaceID)
	}
	rows, err := c.db.QueryContext(bg, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var texts []string
	tenants := map[string]bool{}
	for rows.Next() {
		var tx, tn string
		if err := rows.Scan(&tx, &tn); err != nil {
			return nil, nil, err
		}
		texts = append(texts, tx)
		tenants[tn] = true
	}
	return texts, tenants, rows.Err()
}

// ftsRetrieval is what the pipeline did for one question.
type ftsRetrieval struct {
	err      error
	items    []string // the text of every evidence item
	sources  []string // the source of every evidence item
	searched []string // what the lexical index was asked (the text after the pipeline reduced it)
	diag     query.Diagnostics
	took     time.Duration
}

// retrieve runs the real query pipeline for the tenant. The clock of the world is
// a fake one, so the duration is measured on the wall clock.
func (c *ftsCorpus) retrieve(ten *ftsTenant, text string) ftsRetrieval {
	c.t.Helper()
	c.log.reset()
	start := time.Now()
	res, err := c.pipe.Query(bg, ten.scope, query.Request{Text: text})
	r := ftsRetrieval{err: err, took: time.Since(start), diag: res.Diagnostics, searched: c.log.searched()}
	if err == nil {
		for _, it := range res.Package.Items {
			r.items = append(r.items, it.Text)
			r.sources = append(r.sources, string(it.SourceID))
		}
	}
	return r
}

// ftsRawKind says what the unquoted stand-in must do with a case's text, to show
// that the case is one in which an operator WOULD have acted.
type ftsRawKind int

const (
	rawAny     ftsRawKind = iota // no claim (the case is not one the stand-in can tell apart)
	rawErrors                    // FTS5 rejects the unquoted text
	rawDiffers                   // FTS5 accepts it and returns other rows than the literal reading
	rawSame                      // FTS5 accepts it and returns the same rows (a plain query)
)

// ftsCase is one pathological query. In text, %OWN% and %OTHER% are the secret
// marker of the asking tenant and of the other one.
type ftsCase struct {
	name string
	text string
	// want is the set of sections the LITERAL reading of the text matches, the
	// same for both tenants (their corpora have the same sections).
	want []string
	raw  ftsRawKind
}

func ftsCases() []ftsCase {
	return []ftsCase{
		// Controls: plain words behave as words.
		{"plain word", "apple", []string{"fruit", "vault"}, rawSame},
		{"plain words", "banana cherry", []string{"fruit", "notes"}, rawAny},

		// Column filters.
		{"column filter on a column that exists", "section:secret", []string{"notes", "vault"}, rawDiffers},
		{"column filter on a column that does not", "title:secret", []string{"notes", "vault"}, rawErrors},
		{"column filter with spaces", "column : value", []string{"colon"}, rawErrors},
		{"column filter on the scope column", "tenant_id:%OTHER%", nil, rawAny},
		{"column-set filter", "{section text}: secret", []string{"notes", "vault"}, rawAny},

		// NEAR, prefix, initial token.
		{"NEAR with its arguments", "NEAR(alpha delta, 1)", []string{"order"}, rawDiffers},
		{"NEAR from the brief", "NEAR(a b, 1)", nil, rawAny},
		{"NEAR without parentheses", "alpha NEAR delta", []string{"order"}, rawAny},
		{"prefix star", "abc*", nil, rawDiffers},
		{"prefix star on a stop word", "a*", nil, rawDiffers},
		{"only stars", "* ** ***", nil, rawErrors},
		{"initial token", "^apple", []string{"fruit", "vault"}, rawDiffers},
		{"initial token alone", "^x", nil, rawAny},

		// Boolean operators.
		{"OR", "apple OR banana", []string{"fruit", "notes", "vault"}, rawAny},
		{"AND", "apple AND banana", []string{"fruit", "notes", "vault"}, rawDiffers},
		{"NOT", "apple NOT banana", []string{"fruit", "notes", "vault"}, rawDiffers},
		{"bare AND (a stop word: kept, because nothing else is left)", "AND", []string{"notes"}, rawErrors},
		{"bare OR", "OR", nil, rawErrors},
		{"bare NOT", "NOT", nil, rawErrors},
		{"bare NEAR", "NEAR", nil, rawAny},
		{"all of them", "AND OR NOT NEAR", nil, rawErrors},
		{"minus", "-x", nil, rawAny},
		{"minus on a word", "-apple", []string{"fruit", "vault"}, rawAny},
		{"plus", "+apple", []string{"fruit", "vault"}, rawAny},

		// Quotes and brackets.
		{"unbalanced quote", `"unbalanced`, nil, rawErrors},
		{"unbalanced quote before words", `"apple banana`, []string{"fruit", "notes", "vault"}, rawErrors},
		{"quote alone", `"`, nil, rawErrors},
		{"empty phrase", `""`, nil, rawAny},
		{"three quotes", `"""`, nil, rawAny},
		{"brackets", "( ( (", nil, rawErrors},
		{"closing brackets", "))))", nil, rawErrors},
		{"unicode quotes", "“apple banana”", []string{"fruit", "notes", "vault"}, rawAny},
		{"fullwidth quotes", "＂apple＂", []string{"fruit", "vault"}, rawAny},
		{"guillemets", "«apple»", []string{"fruit", "vault"}, rawAny},

		// Words FTS5 and SQLite give a meaning.
		{"FTS5 special words", "rank rowid highlight MATCH fts5", nil, rawAny},
		{"the table name", "ragindex_fts", nil, rawAny},
		{"a function call", "bm25(ragindex_fts)", nil, rawAny},
		{"SQL", "apple'; DROP TABLE ragindex_chunk; --", []string{"fruit", "vault"}, rawErrors},

		// Another tenant's secret, spelled with operators.
		{"own marker", "%OWN%", []string{"vault"}, rawSame},
		{"the other tenant's marker", "%OTHER%", nil, rawAny},
		{"own marker OR the other's", "%OWN% OR %OTHER%", []string{"vault"}, rawAny},
		{"own marker AND the other's", "%OWN% AND %OTHER%", []string{"vault"}, rawDiffers},
		{"the other's marker with a prefix star", "%OTHER%*", nil, rawAny},
		{"the other's marker in a column filter", "text:%OTHER%", nil, rawAny},
		{"the other's marker or a word", "%OTHER% apple", []string{"fruit", "vault"}, rawAny},

		// Very long tokens (one term of 3000 letters: under every cap, no match).
		{"one very long token", strings.Repeat("a", 1900), nil, rawAny},
	}
}

// SEC-024
func TestSecFTSQuerySyntaxNeutralised(t *testing.T) {
	c := newFTSCorpus(t)
	tenants := []*ftsTenant{c.a, c.b}
	other := map[*ftsTenant]*ftsTenant{c.a: c.b, c.b: c.a}

	// foreign is everything of the other tenant that must never come back.
	foreign := func(of *ftsTenant) []string {
		o := other[of]
		out := []string{o.tag, o.marker}
		for _, id := range o.ids {
			out = append(out, id)
		}
		return out
	}
	// check applies the scope rules to a retrieval: every item is a section of the
	// asking tenant, from its own source, and none holds anything of the other.
	check := func(t testing.TB, what string, ten *ftsTenant, r ftsRetrieval) (docs []string) {
		t.Helper()
		docs, strangers := ten.docsIn(r.items)
		for _, s := range strangers {
			t.Errorf("%s: an item that is no section of %s: %q", what, ten.name, s)
		}
		for i, src := range r.sources {
			if src != ten.src.id {
				t.Errorf("%s: item %d is of source %s, %s's is %s", what, i, src, ten.name, ten.src.id)
			}
		}
		for _, tx := range r.items {
			if n, found := containsAny(tx, foreign(ten)...); found {
				t.Errorf("%s: an item of %s holds %q, which belongs to %s", what, ten.name, n, other[ten].name)
			}
		}
		return docs
	}

	// Positive controls: the stack finds ordinary text, in each tenant's own corpus,
	// and a hit set of exactly the sections that hold the word.
	t.Run("positive control", func(t *testing.T) {
		for _, ten := range tenants {
			r := c.retrieve(ten, "apple")
			if r.err != nil {
				t.Fatalf("%s: %v", ten.name, r.err)
			}
			if got := check(t, "apple", ten, r); !slices.Equal(got, []string{"fruit", "vault"}) {
				t.Errorf("%s: \"apple\" returned %v, want [fruit vault]", ten.name, got)
			}
			if r.diag.Mode != query.ModeFTSOnly {
				t.Errorf("%s: mode %s, want fts_only", ten.name, r.diag.Mode)
			}
			// Each tenant finds its OWN marker and not the other's.
			own := c.retrieve(ten, ten.marker)
			if own.err != nil || len(own.items) == 0 || !strings.Contains(strings.Join(own.items, " "), ten.marker) {
				t.Errorf("%s: its own marker was not found: %v %v", ten.name, own.err, own.items)
			}
		}
	})

	// The detector can see what it claims to see: a planted foreign item is flagged,
	// and the unscoped stand-in shows the other tenant's rows for the same words.
	t.Run("detector controls", func(t *testing.T) {
		planted := ftsRetrieval{items: []string{"The marker is stored here " + c.b.ids["vault"] + " " + c.b.tag}, sources: []string{c.b.src.id}}
		if _, strangers := c.a.docsIn(planted.items); len(strangers) != 1 {
			t.Fatal("a planted item of another tenant was not seen as a stranger")
		}
		if _, found := containsAny(planted.items[0], foreign(c.a)...); !found {
			t.Fatal("a planted item of another tenant does not hold a foreign string")
		}
		_, tenantsSeen, err := c.ftsRaw(c.a.marker+" OR "+c.b.marker, c.a, false)
		if err != nil {
			t.Fatal(err)
		}
		if !tenantsSeen[c.a.name] || !tenantsSeen[c.b.name] {
			t.Fatalf("the unquoted, unscoped stand-in returned the rows of %v, want both tenants: it cannot show a leak", tenantsSeen)
		}
		// The same text, through the pipeline: only the asking tenant's section.
		for _, ten := range tenants {
			r := c.retrieve(ten, ten.marker+" OR "+other[ten].marker)
			if r.err != nil {
				t.Fatal(r.err)
			}
			if got := check(t, "marker OR marker", ten, r); !slices.Equal(got, []string{"vault"}) {
				t.Errorf("%s: %q matched %v, want [vault]", ten.name, "marker OR marker", got)
			}
		}
	})

	for _, tc := range ftsCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, ten := range tenants {
				text := strings.NewReplacer("%OWN%", ten.marker, "%OTHER%", other[ten].marker).Replace(tc.text)
				r := c.retrieve(ten, text)
				what := fmt.Sprintf("%s asking %q", ten.name, tc.text)
				if r.err != nil {
					t.Errorf("%s: failed with code %q: %v", what, contracts.CodeOf(r.err), r.err)
					continue
				}
				// The pipeline was bounded: its time, the text it passed on and its candidates.
				if r.took > 10*time.Second {
					t.Errorf("%s: took %s", what, r.took)
				}
				for _, s := range r.searched {
					if len(s) > index.MaxQueryBytes {
						t.Errorf("%s: %d bytes reached the index, over its limit of %d", what, len(s), index.MaxQueryBytes)
					}
				}
				if r.diag.FTSCandidates > query.DefaultFTSCandidates || len(r.items) > query.DefaultMaxItems {
					t.Errorf("%s: %d candidates and %d items, over the caps of %d and %d", what, r.diag.FTSCandidates, len(r.items), query.DefaultFTSCandidates, query.DefaultMaxItems)
				}
				// The syntax in the text acted as words: exactly the sections of the
				// literal reading came back, from the asking tenant's own corpus.
				if got := check(t, what, ten, r); !slices.Equal(got, tc.want) {
					t.Errorf("%s: returned %v, the literal reading of the words gives %v", what, got, tc.want)
				}
				// The same words with no punctuation at all give the same rows.
				if words := strings.Join(strings.FieldsFunc(text, func(r rune) bool { return !ftsWordRune(r) }), " "); words != "" && len(words) < 2000 {
					plain := c.retrieve(ten, words)
					if plain.err != nil {
						t.Errorf("%s: the same words, %q, failed: %v", what, words, plain.err)
					} else if pg := check(t, what+" (plain)", ten, plain); !slices.Equal(pg, tc.want) {
						t.Errorf("%s: the words alone, %q, returned %v, the text returned %v", what, words, pg, tc.want)
					}
				}

				// The stand-in: would an operator have acted on this text? (Only for the
				// cases that claim it; the others say nothing about the stand-in.)
				if tc.raw == rawAny {
					continue
				}
				texts, _, rerr := c.ftsRaw(text, ten, true)
				switch tc.raw {
				case rawErrors:
					if rerr == nil {
						rawDocs, _ := ten.docsIn(texts)
						t.Errorf("%s: the unquoted stand-in accepted the text (%v): the test cannot see an FTS5 syntax error here", what, rawDocs)
					}
				case rawDiffers, rawSame:
					if rerr != nil {
						t.Errorf("%s: the unquoted stand-in failed: %v", what, rerr)
						continue
					}
					rawDocs, _ := ten.docsIn(texts)
					if same := slices.Equal(rawDocs, tc.want); same != (tc.raw == rawSame) {
						t.Errorf("%s: the unquoted stand-in returned %v and the literal reading is %v: %s", what, rawDocs, tc.want,
							map[bool]string{true: "they agree, so this text cannot show an operator", false: "they differ, but this is a plain query"}[same])
					}
				}
			}
		})
	}

	// Floods: bounded text, bounded candidates, bounded time. The flood word is in
	// 120 chunks of each tenant.
	t.Run("flood word reaches the candidate cap and no further", func(t *testing.T) {
		for _, ten := range tenants {
			r := c.retrieve(ten, c.flood)
			if r.err != nil {
				t.Fatalf("%s: %v", ten.name, r.err)
			}
			if got := check(t, "flood word", ten, r); !slices.Equal(got, []string{"flood"}) {
				t.Errorf("%s: the flood word returned %v, want [flood]", ten.name, got)
			}
			// 120 chunks match; the cap is what stopped it (a cap that did not bite
			// would show 120).
			if r.diag.FTSCandidates != query.DefaultFTSCandidates {
				t.Errorf("%s: %d candidates for a word held by %d chunks, want the cap of %d", ten.name, r.diag.FTSCandidates, ftsFloodSections, query.DefaultFTSCandidates)
			}
			if len(r.items) == 0 || len(r.items) > query.DefaultMaxItems {
				t.Errorf("%s: %d items, want 1 to %d", ten.name, len(r.items), query.DefaultMaxItems)
			}
			// And the index is the one that was cut: a direct search with a high limit
			// does find every chunk of the tenant, and none of the other's.
			res, err := c.ix.Search(bg, ten.scope, index.Query{Text: c.flood, Limit: index.DefaultMaxLimit})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Hits) != ftsFloodSections {
				t.Errorf("%s: the index holds %d chunks with the flood word, want %d", ten.name, len(res.Hits), ftsFloodSections)
			}
			for _, h := range res.Hits {
				if h.SourceID != ten.src.id || strings.Contains(h.Text, other[ten].tag) {
					t.Fatalf("%s: a chunk of another scope: %q", ten.name, h.Text)
				}
			}
		}
	})

	t.Run("floods of terms are reduced before they reach the index", func(t *testing.T) {
		// About 300 distinct terms in under 2000 characters, and the flood word last.
		var terms []string
		for i := 0; i < 300; i++ {
			terms = append(terms, ftsWord(t, 5))
		}
		text := strings.Join(terms, " ") + " " + c.flood
		if len(text) >= query.MaxQuestionChars {
			t.Fatalf("the flood is %d bytes, over the question limit", len(text))
		}
		for _, ten := range tenants {
			r := c.retrieve(ten, text)
			if r.err != nil {
				t.Fatalf("%s: %v", ten.name, r.err)
			}
			if len(r.searched) != 1 {
				t.Fatalf("%s: the index was searched %d times, want once: %q", ten.name, len(r.searched), r.searched)
			}
			if n := len(strings.Fields(r.searched[0])); n > query.DefaultMaxQueryTerms {
				t.Errorf("%s: %d terms reached the index, over the pipeline's cap of %d", ten.name, n, query.DefaultMaxQueryTerms)
			}
			if !strings.Contains(r.searched[0], c.flood) {
				t.Errorf("%s: the longest word of the flood (the one that matches) was cut: %q", ten.name, r.searched[0])
			}
			if got := check(t, "term flood", ten, r); !slices.Equal(got, []string{"flood"}) {
				t.Errorf("%s: the flood returned %v, want [flood]", ten.name, got)
			}
			if r.diag.FTSCandidates != query.DefaultFTSCandidates {
				t.Errorf("%s: %d candidates, want the cap of %d", ten.name, r.diag.FTSCandidates, query.DefaultFTSCandidates)
			}
			if r.took > 10*time.Second {
				t.Errorf("%s: took %s", ten.name, r.took)
			}
		}
	})

	t.Run("a question over the size limit is refused before it reaches the index", func(t *testing.T) {
		for _, tc := range []struct{ name, text string }{
			{"10000 distinct terms", strings.Join(func() []string {
				var w []string
				for i := 0; i < 10000; i++ {
					w = append(w, ftsWord(t, 4))
				}
				return w
			}(), " ")},
			{"10000 copies of one term", strings.Repeat("apple ", 10000)},
			{"one token of 100000 letters", strings.Repeat("z", 100000)},
			{"a NUL", "apple\x00banana"},
			{"invalid UTF-8", "apple \xff\xfe banana"},
			{"empty", ""},
			{"only operators and spaces", "   "},
		} {
			for _, ten := range tenants {
				r := c.retrieve(ten, tc.text)
				if code := contracts.CodeOf(r.err); code != contracts.ErrValidationFailed {
					t.Errorf("%s, %s: error code %q (%v), want validation_failed", tc.name, ten.name, code, r.err)
				}
				if len(r.searched) != 0 {
					t.Errorf("%s, %s: a refused question reached the index: %d bytes", tc.name, ten.name, len(r.searched[0]))
				}
				if r.took > 10*time.Second {
					t.Errorf("%s, %s: took %s", tc.name, ten.name, r.took)
				}
			}
		}
	})

	// The index itself, called without the pipeline in front of it: its own bounds.
	t.Run("the index refuses what is over its limits and never answers with an internal error", func(t *testing.T) {
		for _, tc := range []struct{ name, text string }{
			{"over 4096 bytes", strings.Repeat("apple ", 1000)},
			{"one long token", strings.Repeat("q", index.MaxQueryBytes+1)},
			{"invalid UTF-8", "apple \xc3\x28"},
		} {
			_, err := c.ix.Search(bg, c.a.scope, index.Query{Text: tc.text})
			if code := contracts.CodeOf(err); code != contracts.ErrValidationFailed {
				t.Errorf("%s: code %q (%v), want validation_failed", tc.name, code, err)
			}
		}
		// Anything within the limits is accepted by FTS5, whatever it looks like.
		for _, tc := range ftsCases() {
			text := strings.NewReplacer("%OWN%", c.a.marker, "%OTHER%", c.b.marker).Replace(tc.text)
			if _, err := c.ix.Search(bg, c.a.scope, index.Query{Text: text, Limit: index.DefaultMaxLimit}); err != nil {
				var ce *contracts.Error
				if errors.As(err, &ce) && ce.Code == contracts.ErrInternal {
					t.Errorf("%s: FTS5 rejected the expression built from %q: %v", tc.name, tc.text, err)
				} else {
					t.Errorf("%s: %v", tc.name, err)
				}
			}
		}
	})

	// The store is intact after all of that (the SQL in the texts was not run).
	t.Run("the index is intact", func(t *testing.T) {
		var n int
		if err := c.db.QueryRowContext(bg, `SELECT count(*) FROM ragindex_chunk`).Scan(&n); err != nil {
			t.Fatalf("the chunk table is gone: %v", err)
		}
		if n == 0 {
			t.Fatal("the chunk table is empty")
		}
		ctx, cancel := context.WithTimeout(bg, 10*time.Second)
		defer cancel()
		if err := c.db.IntegrityCheck(ctx); err != nil {
			t.Fatalf("integrity check: %v", err)
		}
		r := c.retrieve(c.a, "apple")
		if got := check(t, "apple again", c.a, r); r.err != nil || !slices.Equal(got, []string{"fruit", "vault"}) {
			t.Errorf("the plain query now returns %v (%v)", got, r.err)
		}
	})
}

// ftsWordRune is what a query's words are made of, by the contract the index
// documents: letters, digits and combining marks.
func ftsWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}
