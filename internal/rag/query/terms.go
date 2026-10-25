package query

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/buxee/alix-ai/internal/rag/index"
)

// maxIdentTokens is the most identifier-shaped tokens kept whole. The index
// recognises at most eight identifiers in a query.
const maxIdentTokens = 8

// reduceQuery bounds the question before it reaches the full-text stage.
//
// BM25 scores every row that matches any term, so inside a large scope the
// cost of a query grows with its number of terms (docs/efficiency/M2-09.md has
// the measurements). The index already keeps at most 16 terms and drops stop
// words; this keeps at most maxTerms plain terms, so the pipeline's cap can be
// tighter than the index's. The reduction is deterministic and uses no model:
//
//   - a question with at most maxTerms distinct words is returned unchanged;
//   - identifier-shaped tokens (a token with a digit, or with an inner
//     separator such as "ABC-123" or "a/b") are always kept, whole, because the
//     exact-identifier lookup of the index needs them as written;
//   - the remaining budget goes to the longest plain words (a long word is more
//     often a rare one, and rare terms carry BM25's ranking), in the order
//     they were written. A stop word is only kept when the budget outlasts every
//     content word; the index drops the ones that remain.
//
// The result is the kept tokens joined by single spaces, in their original
// order. It is query text, never an FTS5 expression: the index quotes every
// term itself.
func reduceQuery(text string, maxTerms int) string {
	type token struct {
		raw   string
		words []string
		ident bool
	}
	var toks []token
	distinct := map[string]bool{}
	for _, raw := range strings.FieldsFunc(text, unicode.IsSpace) {
		raw = strings.TrimFunc(raw, func(r rune) bool { return !isWordRune(r) })
		if raw == "" {
			continue
		}
		words := splitWordsLower(raw)
		if len(words) == 0 {
			continue
		}
		toks = append(toks, token{raw: raw, words: words, ident: identShaped(raw)})
		for _, w := range words {
			distinct[w] = true
		}
	}
	if len(distinct) <= maxTerms {
		return fitIndex(strings.Join(rawsOf(toks, func(t token) string { return t.raw }), " "))
	}

	keep := make([]bool, len(toks))
	used := map[string]bool{}
	budget := maxTerms
	idents := 0
	for i, t := range toks {
		if t.ident && idents < maxIdentTokens {
			idents++
			keep[i] = true
			for _, w := range t.words {
				used[w] = true
			}
		}
	}
	// Plain words, content words before stop words, then by length, longest
	// first; ties keep question order.
	var plain []int
	for i, t := range toks {
		if !keep[i] && !t.ident {
			plain = append(plain, i)
		}
	}
	sort.SliceStable(plain, func(a, b int) bool {
		ta, tb := toks[plain[a]], toks[plain[b]]
		if sa, sb := stopWords[ta.words[0]], stopWords[tb.words[0]]; sa != sb {
			return !sa // a stop word never takes a slot from a content word
		}
		return utf8.RuneCountInString(ta.raw) > utf8.RuneCountInString(tb.raw)
	})
	for _, i := range plain {
		if budget <= 0 {
			break
		}
		w := toks[i].words[0]
		if used[w] {
			continue
		}
		used[w] = true
		keep[i] = true
		budget--
	}
	var kept []string
	for i, t := range toks {
		if keep[i] {
			kept = append(kept, t.raw)
		}
	}
	return fitIndex(strings.Join(kept, " "))
}

// fitIndex cuts the reduced text to the index's byte limit, on a rune
// boundary. A question within MaxQuestionChars can be over it: text with no
// spaces (Chinese, Japanese) is one word, so the term cap leaves it whole, and
// 2000 such characters are 6000 bytes.
func fitIndex(s string) string {
	if len(s) <= index.MaxQueryBytes {
		return s
	}
	cut := index.MaxQueryBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func rawsOf[T any](in []T, f func(T) string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

// splitWordsLower splits s into lower-cased runs of letters, digits and marks.
func splitWordsLower(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !isWordRune(r) })
}

// identShaped reports whether a token looks like a part number, order code or
// SKU: it holds a digit, or two alphanumeric runs joined by one of - _ . / : #.
// It is deliberately a little generous (the index decides what is an
// identifier); an extra kept token only costs one term.
func identShaped(tok string) bool {
	hasDigit := false
	for _, r := range tok {
		if unicode.IsDigit(r) {
			hasDigit = true
			break
		}
	}
	if hasDigit && utf8.RuneCountInString(tok) >= 3 {
		return true
	}
	return strings.ContainsAny(tok, "-_./:#") && len(splitWordsLower(tok)) > 1
}

// stopWords mirrors the function words internal/rag/index drops from a query's
// OR-expression (English and Italian). The index drops them anyway; knowing them
// here keeps a stop word from using one of the pipeline's few term slots, so the
// cap buys content words.
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
