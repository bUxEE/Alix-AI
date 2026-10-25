package capabilities

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Text matching. Everything here is plain string code with no model and no
// allocation beyond the token slices: normalise, split into tokens, compare
// tokens with a light stem rule. It is the whole of the deterministic matcher,
// so its behaviour is pinned by text_test.go.

// foldLatin maps a lower-case Latin letter with a diacritic to its base
// letters ("è" to "e", "ß" to "ss"). It returns "" for a rune it does not
// fold. Combining marks (decomposed accents) are dropped by the tokeniser, so
// "e" followed by U+0300 also lands on "e".
func foldLatin(r rune) string {
	switch {
	case r >= 'à' && r <= 'å', r == 'ā', r == 'ă', r == 'ą':
		return "a"
	case r == 'æ':
		return "ae"
	case r == 'ç', r == 'ć', r == 'ĉ', r == 'ċ', r == 'č':
		return "c"
	case r == 'ð', r == 'ď', r == 'đ':
		return "d"
	case r >= 'è' && r <= 'ë', r == 'ē', r == 'ĕ', r == 'ė', r == 'ę', r == 'ě':
		return "e"
	case r == 'ĝ', r == 'ğ', r == 'ġ', r == 'ģ':
		return "g"
	case r == 'ĥ', r == 'ħ':
		return "h"
	case r >= 'ì' && r <= 'ï', r == 'ĩ', r == 'ī', r == 'ĭ', r == 'į', r == 'ı':
		return "i"
	case r == 'ĵ':
		return "j"
	case r == 'ķ':
		return "k"
	case r == 'ĺ', r == 'ļ', r == 'ľ', r == 'ŀ', r == 'ł':
		return "l"
	case r == 'ñ', r == 'ń', r == 'ņ', r == 'ň':
		return "n"
	case r >= 'ò' && r <= 'ö', r == 'ø', r == 'ō', r == 'ŏ', r == 'ő':
		return "o"
	case r == 'œ':
		return "oe"
	case r == 'ŕ', r == 'ŗ', r == 'ř':
		return "r"
	case r == 'ś', r == 'ŝ', r == 'ş', r == 'š':
		return "s"
	case r == 'ß':
		return "ss"
	case r == 'ţ', r == 'ť', r == 'ŧ':
		return "t"
	case r >= 'ù' && r <= 'ü', r == 'ũ', r == 'ū', r == 'ŭ', r == 'ů', r == 'ű', r == 'ų':
		return "u"
	case r == 'ŵ':
		return "w"
	case r == 'ý', r == 'ÿ', r == 'ŷ':
		return "y"
	case r == 'ź', r == 'ż', r == 'ž':
		return "z"
	}
	return ""
}

// unspacedScript reports whether r belongs to a script written without spaces
// between words. Each such rune is its own token, so a label in Japanese, Thai
// or Chinese matches as a run of consecutive characters.
func unspacedScript(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana,
		unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar)
}

// tokenize lower-cases s, folds Latin diacritics, drops combining marks and
// splits on everything that is not a letter or a digit (apostrophes included,
// so "dov'è" is "dov" and "e" and "l'appuntamento" is "l" and "appuntamento").
// It returns at most max tokens; max <= 0 means no limit.
func tokenize(s string, max int) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		if max > 0 && len(out) >= max {
			break
		}
		r = unicode.ToLower(r)
		switch {
		case unicode.IsMark(r):
			// a decomposed accent: dropped, the base letter stays
		case unicode.IsLetter(r) && unspacedScript(r):
			flush()
			out = append(out, string(r))
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if f := foldLatin(r); f != "" {
				cur.WriteString(f)
			} else {
				cur.WriteRune(r)
			}
		default:
			flush()
		}
	}
	if max <= 0 || len(out) < max {
		flush()
	}
	if max > 0 && len(out) > max {
		out = out[:max]
	}
	return out
}

// commonPrefix returns the number of leading runes a and b share.
func commonPrefix(a, b string) int {
	n := 0
	for len(a) > 0 && len(b) > 0 {
		ra, sa := utf8.DecodeRuneInString(a)
		rb, sb := utf8.DecodeRuneInString(b)
		if ra != rb {
			break
		}
		n++
		a, b = a[sa:], b[sb:]
	}
	return n
}

// tokensMatch reports whether two normalised tokens are the same word, allowing
// for inflection. It is a deliberately small stem rule that works for English
// and Italian without a dictionary:
//
//   - equal tokens match;
//   - a token of fewer than four runes only matches itself;
//   - otherwise the tokens match when the shorter one is a prefix of the longer
//     and at most four runes shorter ("book" and "bookings", "cancel" and
//     "cancelled"), or when they share a prefix of at least five runes that is
//     at least three quarters of the shorter token ("prenotare" and
//     "prenotazione", "cancel" and "cancellation").
//
// Package authors list the forms that the rule does not reach (irregular
// forms, unrelated synonyms) as synonyms in their terminology.
func tokensMatch(a, b string) bool {
	if a == b {
		return true
	}
	la, lb := utf8.RuneCountInString(a), utf8.RuneCountInString(b)
	ls, ll := la, lb
	if ls > ll {
		ls, ll = ll, ls
	}
	if ls < 4 {
		return false
	}
	p := commonPrefix(a, b)
	if p == ls && ll-ls <= 4 {
		return true
	}
	return p >= 5 && p*4 >= ls*3
}

// phrase is a normalised label: the tokens that must appear consecutively.
type phrase struct {
	tokens []string
	// label is the text as the package wrote it, kept for Signal.Label.
	label string
	// lang is the language the label belongs to, "" for a language-neutral
	// source such as the capability title.
	lang string
}

// newPhrase normalises a label. It returns false for a label with no token.
func newPhrase(label, lang string) (phrase, bool) {
	toks := tokenize(label, 8)
	if len(toks) == 0 {
		return phrase{}, false
	}
	return phrase{tokens: toks, label: clipLabel(label), lang: lang}, true
}

// clipLabel bounds a package-authored label before it is copied into a Signal.
func clipLabel(s string) string {
	s = strings.TrimSpace(s)
	const max = 64
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max])
}

// matches reports whether the phrase occurs in utterance tokens: every phrase
// token matches the utterance token at the same offset, consecutively.
func (p phrase) matches(utt []string) bool {
	n := len(p.tokens)
	if n == 0 || len(utt) < n {
		return false
	}
	for i := 0; i+n <= len(utt); i++ {
		ok := true
		for j := 0; j < n; j++ {
			if !tokensMatch(p.tokens[j], utt[i+j]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// stopwords are function words of the two languages the corpus covers. They are
// dropped from titles before the title's words become keywords; a label an
// author wrote is never filtered, because the author chose it.
var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		a an and any are as at be been but by can could did do does for from get got had has have how i if in into is it its
		me my of on or our please should so than that the their them then there these they this to up us was we were what
		when where which who why will with would you your
		al alla alle allo anche che chi ci col come con cosa cui da dal dalla dei del della delle dello di dove e ed era essere
		gli ha hai ho il in io la le lo ma mi mia mie miei mio ne nel nella nelle non per perche piu poi quale quali quando quanto
		quella quello questa queste questi questo se si sono su sul sulla suo sua tra tu tuo tua un una uno vi voi vorrei
	`) {
		m[w] = true
	}
	return m
}()

// keywordTokens returns the tokens of s that can stand alone as evidence: not
// a stopword, at least three runes, spaced script only.
func keywordTokens(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range tokenize(s, 16) {
		if utf8.RuneCountInString(t) < 3 || stopwords[t] || seen[t] {
			continue
		}
		if r, _ := utf8.DecodeRuneInString(t); unspacedScript(r) {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
