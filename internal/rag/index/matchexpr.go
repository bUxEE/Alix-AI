package index

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/pkg/contracts"
)

// Limits on a query. They bound the work one request can ask of the index:
// BM25 scores every row that matches any term, so the number of terms and the
// text they come from are capped before they reach SQLite.
const (
	// MaxQueryBytes is the longest query text accepted.
	MaxQueryBytes = 4096
	// MaxQueryTerms is the most distinct terms one query contributes to the
	// full-text expression. When a query has more, the longest are kept: a
	// long word is more often a rare one, and rare terms carry the ranking.
	MaxQueryTerms = 16
)

// parsedQuery is a query reduced to what the index can safely run.
type parsedQuery struct {
	// words are the distinct lower-cased terms kept for the full-text
	// expression, in query order.
	words []string
	// idents are the identifier-shaped tokens of the query.
	idents []identToken
	// match is the FTS5 expression, empty when there is nothing to match.
	match string
}

// parseQuery turns free text into an FTS5 expression built only from quoted
// terms. Nothing the user typed is ever passed to FTS5 as syntax: every term is
// a double-quoted string made of letters, digits and combining marks, so the
// operators (AND, OR, NOT, NEAR, column filters, "*", parentheses, "^") are
// plain words and a query can never name a column or reach another scope.
// Authorisation does not rely on this (the scope is a separate SQL predicate),
// but it keeps the expression's meaning and cost predictable.
func parseQuery(text string, matchAll bool) (parsedQuery, error) {
	if !utf8.ValidString(text) {
		return parsedQuery{}, contracts.New(contracts.ErrValidationFailed, "index: query is not valid UTF-8")
	}
	if len(text) > MaxQueryBytes {
		return parsedQuery{}, contracts.Newf(contracts.ErrValidationFailed, "index: query is %d bytes, over the limit of %d", len(text), MaxQueryBytes)
	}
	var pq parsedQuery
	pq.idents = extractIdentifiers(text, maxIdentsPerQuery)

	all := queryWords(text)
	pq.words = selectTerms(all)
	if len(pq.words) == 0 && len(pq.idents) == 0 {
		return pq, nil
	}

	join := " OR "
	if matchAll {
		join = " AND "
	}
	var terms []string
	for _, w := range pq.words {
		terms = append(terms, quoteTerm(w))
	}
	if !matchAll {
		// An identifier also matches as an adjacent phrase of its parts
		// ("abc 123" for ABC-123), so BM25 prefers the chunk that has it whole
		// over one that has the parts scattered.
		for _, id := range pq.idents {
			if len(id.Words) > 1 {
				terms = append(terms, quoteTerm(strings.Join(id.Words, " ")))
			}
		}
	}
	pq.match = strings.Join(terms, join)
	return pq, nil
}

// quoteTerm double-quotes a term for FTS5. The inputs are already restricted to
// letters, digits, marks and single spaces; doubling quotes keeps it correct
// even if that ever changes.
func quoteTerm(t string) string {
	return `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
}

// splitWords splits text into lower-cased words in order, duplicates kept: runs
// of letters, digits and combining marks. Everything else separates words.
func splitWords(text string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			out = append(out, strings.ToLower(b.String()))
			b.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// probeWords splits text into words the way the index's unicode61 tokenizer
// does, for a phrase that must find the text again: runs of letters, numbers
// of every kind (² ½ ① Ⅳ, not only decimal digits), marks and private-use
// characters. Cutting out a character the tokenizer keeps inside a token ("m"
// for "m²") would make the phrase miss its own chunk. Keeping a mark the
// tokenizer treats as a separator is harmless: FTS5 splits the phrase there
// too. The words are left as written; FTS5 folds the phrase as it folded the
// text.
func probeWords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || unicode.Is(unicode.Co, r))
	})
}

// queryWords is splitWords without duplicates, in order of first appearance.
func queryWords(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range splitWords(text) {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// selectTerms drops stop words (unless that would leave nothing) and keeps at
// most MaxQueryTerms, preferring longer words, in the original order.
func selectTerms(words []string) []string {
	kept := make([]string, 0, len(words))
	for _, w := range words {
		if !stopWords[w] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		kept = words
	}
	if len(kept) <= MaxQueryTerms {
		return kept
	}
	idx := make([]int, len(kept))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return utf8.RuneCountInString(kept[idx[a]]) > utf8.RuneCountInString(kept[idx[b]])
	})
	idx = idx[:MaxQueryTerms]
	sort.Ints(idx)
	out := make([]string, len(idx))
	for i, j := range idx {
		out[i] = kept[j]
	}
	return out
}

// stopWords are English and Italian function words dropped from a query's
// OR-expression. They occur in nearly every chunk, so as OR terms they would
// make BM25 score the whole scope for no ranking gain. A query made only of
// them keeps them. The index text is never filtered, so phrases and exact
// identifiers are unaffected.
var stopWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		a an and are as at be been but by can do does for from had has have how i if in is it its
		of on or that the their then there these this those to was were what when where which who
		whom why will with would you your
		al alla alle allo agli ai anche che chi ci come con cui da dal dalla dalle dei del della
		delle dello degli di dove e ed era erano gli ha hanno ho il in io la le lo ma mi ne nel
		nella nelle nei nello non o per piu più qual quale quali quando quanto se si sia sono su
		sul sulla sulle tra fra tu un una uno
	`) {
		m[w] = true
	}
	return m
}()
