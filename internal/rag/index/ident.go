package index

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Exact identifiers (master plan 12.4 "deterministic filters and exact
// identifiers"). A part number, an order code or a SKU is one opaque string to
// the person asking and to the document, but the full-text tokenizer splits
// "ABC-123" into "abc" and "123", so BM25 alone matches any chunk holding both
// words anywhere. The index therefore also records each identifier whole, in a
// normalised key, and a query that contains an identifier looks it up exactly.
//
// What counts as an identifier is decided by shape alone (no model, no word
// list), by identifierRules below. It is deliberately a little generous: an
// extra key only costs a few bytes, while a missed one loses the exact match.

// Limits on what one chunk or one query may contribute.
const (
	maxIdentRunes     = 128 // longer tokens are not identifiers (a SHA-256 in hex is 64)
	minIdentRunes     = 3
	maxIdentsPerChunk = 64
	maxIdentsPerQuery = 8
	identSeparators   = "-_./:#"
	minAllDigitsLen   = 5
)

// identToken is one identifier found in text.
type identToken struct {
	// Raw is the token as written ("ABC-123").
	Raw string
	// Keys are its normalised forms, the strings stored and looked up: the
	// lower-cased token with every separator written as "-" ("abc-123"), and,
	// when it has separators, the same with them removed ("abc123"), so a
	// query typed without the hyphen still finds the document that has it.
	Keys []string
	// Words are Raw split on its separators and lower-cased, the tokens the
	// full-text tokenizer will have made of it ("abc", "123"), for building an
	// adjacency phrase.
	Words []string
}

func isIdentSep(r rune) bool { return strings.ContainsRune(identSeparators, r) }

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// extractIdentifiers returns the identifiers in s in order of first
// appearance, without duplicates (by squashed key), at most limit of them.
func extractIdentifiers(s string, limit int) []identToken {
	var out []identToken
	seen := map[string]bool{}
	runes := []rune(s)
	n := len(runes)
	flush := func(start, end int) {
		if end <= start {
			return
		}
		tok, ok := classifyIdentifier(runes[start:end])
		if !ok || seen[tok.Keys[len(tok.Keys)-1]] {
			return
		}
		seen[tok.Keys[len(tok.Keys)-1]] = true
		out = append(out, tok)
	}
	i := 0
	for i < n && len(out) < limit {
		if !isAlnum(runes[i]) {
			i++
			continue
		}
		start := i
		for i < n {
			r := runes[i]
			if isAlnum(r) {
				i++
				continue
			}
			// A separator belongs to the token only between two alphanumerics.
			if isIdentSep(r) && i+1 < n && isAlnum(runes[i+1]) {
				i++
				continue
			}
			break
		}
		flush(start, i)
	}
	return out
}

// classifyIdentifier applies the shape rules to one maximal token.
func classifyIdentifier(tok []rune) (identToken, bool) {
	if len(tok) < minIdentRunes || len(tok) > maxIdentRunes {
		return identToken{}, false
	}
	var letters, digits, seps, upper, lower int
	for _, r := range tok {
		switch {
		case unicode.IsLetter(r):
			letters++
			if unicode.IsUpper(r) {
				upper++
			} else if unicode.IsLower(r) {
				lower++
			}
		case unicode.IsDigit(r):
			digits++
		default:
			seps++
		}
	}
	hasUnderscore := false
	for _, r := range tok {
		if r == '_' {
			hasUnderscore = true
			break
		}
	}
	segments := 1
	for _, r := range tok {
		if isIdentSep(r) {
			segments++
		}
	}
	ok := false
	switch {
	case seps == 0 && digits > 0 && letters > 0:
		// SKU12345, A1B2C3, ERR4021.
		ok = true
	case seps == 0 && letters == 0:
		// A bare number is an identifier only when long enough not to be a
		// quantity, a year or a page.
		ok = digits >= minAllDigitsLen
	case seps > 0:
		// ABC-123, 2024-05-01, 1.2.3, snake_case_name, ERR-AUTH-EXPIRED.
		// A hyphenated lower-case phrase ("well-known") is not one.
		ok = digits > 0 || hasUnderscore || (segments >= 2 && letters > 0 && lower == 0 && upper == letters)
	}
	if !ok {
		return identToken{}, false
	}
	raw := string(tok)
	lowerRaw := strings.ToLower(raw)
	var dashed, squashed strings.Builder
	var words []string
	var word strings.Builder
	endWord := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, r := range lowerRaw {
		if isIdentSep(r) {
			dashed.WriteByte('-')
			endWord()
			continue
		}
		dashed.WriteRune(r)
		squashed.WriteRune(r)
		word.WriteRune(r)
	}
	endWord()
	keys := []string{dashed.String()}
	if seps > 0 {
		keys = append(keys, squashed.String())
	}
	return identToken{Raw: raw, Keys: keys, Words: words}, true
}

// chunkIdentKeys returns the distinct identifier keys of the strings, for the
// ident table: the text, plus the section title and table header, because a
// code in a heading or a column header is as findable as one in the body.
func chunkIdentKeys(parts ...string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, p := range parts {
		if p == "" || !utf8.ValidString(p) {
			continue
		}
		for _, t := range extractIdentifiers(p, maxIdentsPerChunk) {
			for _, k := range t.Keys {
				if !seen[k] {
					seen[k] = true
					keys = append(keys, k)
				}
			}
		}
		if len(keys) >= maxIdentsPerChunk*2 {
			break
		}
	}
	return keys
}
