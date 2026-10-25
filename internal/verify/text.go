package verify

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Characters the normaliser keeps as markers because later stages give them a meaning.
const (
	// nbsp stands for every no-break or thin space. Italian and French text writes a thousands
	// separator or the gap before a currency symbol with one, so the scanner must tell it apart
	// from an ordinary space.
	nbsp = '\u00A0'
	// unknownNumber replaces a numeric character the normaliser cannot map to ASCII digits (a roman
	// numeral, a vulgar fraction it does not know). It becomes a number no fact can equal, so the
	// response fails closed instead of the character vanishing.
	unknownNumber = '\uE000'
)

// digitValue returns the decimal value of a Unicode decimal digit (category Nd), ASCII or not.
func digitValue(r rune) (int, bool) {
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}
	if r < 0x80 || !unicode.Is(unicode.Nd, r) {
		return 0, false
	}
	// Every Nd range is made of aligned blocks of ten starting at the digit zero.
	for _, rg := range unicode.Nd.R16 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10, true
		}
	}
	for _, rg := range unicode.Nd.R32 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10, true
		}
	}
	return 0, false
}

// superDigits maps superscript, subscript and circled digits to the digits they display.
var superDigits = map[rune]string{
	'\u00B2': "2", '\u00B3': "3", '\u00B9': "1", '\u2070': "0", '\u2074': "4", '\u2075': "5",
	'\u2076': "6", '\u2077': "7", '\u2078': "8", '\u2079': "9",
	'\u2080': "0", '\u2081': "1", '\u2082': "2", '\u2083': "3", '\u2084': "4", '\u2085': "5",
	'\u2086': "6", '\u2087': "7", '\u2088': "8", '\u2089': "9",
	'\u00BC': "1/4", '\u00BD': "1/2", '\u00BE': "3/4",
}

// normalise turns model or host text into the form the extractor reads. It is a small, explicit
// stand-in for Unicode NFKC (the module does not depend on golang.org/x/text) covering what lets a
// number, date, time or identifier hide from a scan: full-width and other non-ASCII digits, zero
// width and other format characters that split a token while displaying as nothing, assorted
// spaces, dashes and apostrophes. Anything numeric it cannot map becomes unknownNumber.
func normalise(s string) string {
	if plainASCII(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	prevCR := false
	for _, r := range s {
		if r == '\n' && prevCR { // "\r\n" is one line break
			prevCR = false
			continue
		}
		prevCR = r == '\r'
		switch {
		case r == '\n' || r == '\r':
			b.WriteByte('\n')
			continue
		case r == '\t':
			b.WriteByte(' ')
			continue
		case r == '\u00A0' || r == '\u202F' || r == '\u2009' || r == '\u2007' || r == '\u2008':
			b.WriteRune(nbsp)
			continue
		case unicode.IsSpace(r):
			b.WriteByte(' ')
			continue
		case unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r) || r == '\u00AD':
			// Invisible: zero-width space/joiner, bidi controls, BOM, soft hyphen, C0/C1 controls.
			continue
		}
		if r >= '\uFF01' && r <= '\uFF5E' { // full-width ASCII
			r -= 0xFEE0
		}
		if v, ok := digitValue(r); ok {
			b.WriteByte(byte('0' + v))
			continue
		}
		if m, ok := superDigits[r]; ok {
			b.WriteString(m)
			continue
		}
		switch r {
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212', '\uFE58', '\uFE63':
			b.WriteByte('-')
			continue
		case '\u2018', '\u2019', '\u02BC', '`':
			b.WriteByte('\'')
			continue
		case '\u201C', '\u201D', '\u201E', '\u00AB', '\u00BB':
			b.WriteByte('"')
			continue
		case '\u066B':
			b.WriteByte('.')
			continue
		case '\u066C':
			b.WriteByte(',')
			continue
		}
		if r >= 0x80 && (unicode.Is(unicode.Nl, r) || unicode.Is(unicode.No, r)) {
			b.WriteRune(unknownNumber)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// fold lower-cases a word and strips the accents of the letters the supported languages use, so
// "lunedì", "Lunedi" and "LUNEDÌ" compare equal. It is deliberately small: it does not need to be
// a general transliterator.
func fold(s string) string {
	if isASCII(s) {
		return strings.ToLower(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch r {
		case 'à', 'á', 'â', 'ä':
			r = 'a'
		case 'è', 'é', 'ê', 'ë':
			r = 'e'
		case 'ì', 'í', 'î', 'ï':
			r = 'i'
		case 'ò', 'ó', 'ô', 'ö':
			r = 'o'
		case 'ù', 'ú', 'û', 'ü':
			r = 'u'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// collapse folds a phrase for substring comparison: lower case, accents stripped, every run of
// spaces and punctuation-free whitespace reduced to one space.
func collapse(s string) string {
	s = fold(normalise(s))
	return strings.Join(strings.Fields(strings.ReplaceAll(s, string(nbsp), " ")), " ")
}

// isAlnum reports whether r is a letter, a digit or an underscore.
func isAlnum(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}

// plainASCII reports whether normalise would return s unchanged: printable ASCII and line feeds,
// with no backtick (which normalise turns into an apostrophe).
func plainASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' {
			continue
		}
		if c < 0x20 || c > 0x7E || c == '`' {
			return false
		}
	}
	return true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// hasInvisible reports whether s contains a format character (category Cf: zero-width space,
// joiners, bidirectional controls, the byte order mark, the soft hyphen) or a control character
// other than a line break or tab.
func hasInvisible(s string) bool {
	if plainASCII(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
		case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Cc, r):
			return true
		}
	}
	return false
}
