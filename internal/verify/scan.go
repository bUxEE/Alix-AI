package verify

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokKind uint8

const (
	tokNum     tokKind = iota // digits with internal separators, optional suffix ("14th", "10pm")
	tokWord                   // letters, possibly joined by - . (no digits)
	tokMixed                  // identifier-like: letters and digits, URL, e-mail, typed ID
	tokInstant                // ISO 8601 date-time ("2026-10-14T08:30:00Z")
	tokSym                    // currency symbol or percent sign
	tokPunct                  // any other single character
	tokUnknown                // a numeric character normalise could not map
	tokKnown                  // an identifier of the directive found verbatim
)

type token struct {
	kind  tokKind
	start int
	end   int    // exclusive, includes any suffix
	text  string // the exact slice s[start:end]
	blob  string // tokNum: the numeric part without its suffix (synthetic "10:30" for "10h30")
	sfx   string // tokNum: attached suffix, lower-cased ("th", "pm", "kg", "%")
	nl    bool   // first token on its line
	fw    string // tokWord: the folded text, computed once
}

type span struct{ start, end int }

var (
	instantRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}(?::\d{2}(?:\.\d{1,9})?)?(?:[Zz]|[+-]\d{2}:?\d{2})?`)
	typedIDRe = regexp.MustCompile(`^[a-z][a-z0-9]{1,7}_[0-9A-HJKMNP-TV-Z]{26}$`)
)

const nbspStr = string(nbsp)

func isDigitByte(b byte) bool { return b >= '0' && b <= '9' }

// separatorAt returns the byte length of a numeric separator at s[j] (. , : / - ' or a no-break
// space) and whether there is one.
func separatorAt(s string, j int) (int, bool) {
	switch s[j] {
	case '.', ',', ':', '/', '-', '\'':
		return 1, true
	}
	if strings.HasPrefix(s[j:], nbspStr) {
		return len(nbspStr), true
	}
	return 0, false
}

// scanNumber scans the numeric blob starting at s[i], an ASCII digit: digits with single internal
// separators, each followed by a digit. A no-break space only joins when exactly three digits
// follow (a thousands group). It returns the end of the blob.
func scanNumber(s string, i int) int {
	j := i
	for j < len(s) && isDigitByte(s[j]) {
		j++
	}
	for j < len(s) {
		n, ok := separatorAt(s, j)
		if !ok {
			break
		}
		k := j + n
		if k >= len(s) || !isDigitByte(s[k]) {
			break
		}
		if strings.HasPrefix(s[j:], nbspStr) {
			e := k
			for e < len(s) && isDigitByte(s[e]) {
				e++
			}
			if e-k != 3 {
				break
			}
		}
		j = k
		for j < len(s) && isDigitByte(s[j]) {
			j++
		}
	}
	return j
}

// scanWordEnd scans a word starting at s[i] (a letter or underscore) and reports its end. A word
// is letters, digits and underscores, joined by '-', '.' or '@' when an alphanumeric follows, or a
// URL (scheme://...).
func scanWordEnd(s string, i int) int {
	j := i
	letterOnly := true
	for j < len(s) {
		r, w := utf8.DecodeRuneInString(s[j:])
		if !isAlnum(r) {
			break
		}
		if r == '_' || unicode.IsDigit(r) {
			letterOnly = false
		}
		j += w
	}
	if letterOnly && strings.HasPrefix(s[j:], "://") {
		for j < len(s) {
			r, w := utf8.DecodeRuneInString(s[j:])
			if unicode.IsSpace(r) || r == nbsp || strings.ContainsRune(`)]>"<`, r) {
				break
			}
			j += w
		}
		for j > i && strings.ContainsRune(".,;:!?'", rune(s[j-1])) {
			j--
		}
		return j
	}
	for j < len(s) && (s[j] == '-' || s[j] == '.' || s[j] == '@') && j+1 < len(s) {
		r, _ := utf8.DecodeRuneInString(s[j+1:])
		if !isAlnum(r) {
			break
		}
		j++
		for j < len(s) {
			r, w := utf8.DecodeRuneInString(s[j:])
			if !isAlnum(r) {
				break
			}
			j += w
		}
	}
	return j
}

// classifyWord decides whether a scanned word is an ordinary word or identifier-like.
func classifyWord(w string) tokKind {
	hasLetter, hasDigit := false, false
	for _, r := range w {
		switch {
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsLetter(r):
			hasLetter = true
		}
	}
	switch {
	case hasLetter && hasDigit:
		return tokMixed
	case innerUnderscore(w):
		// snake_case: "usr_alice", "tenant_acme". Identifiers are usually written this way, so an
		// unknown one is invented. A leading or trailing underscore ("_italic_") is markup.
		return tokMixed
	case strings.ContainsRune(w, '@'), strings.Contains(w, "://"), strings.HasPrefix(strings.ToLower(w), "www."):
		return tokMixed
	case typedIDRe.MatchString(w):
		return tokMixed
	case randomLetters(w):
		return tokMixed
	}
	return tokWord
}

// randomLetters reports a long run of letters whose case keeps changing, the
// shape of a token or nonce in base64 that happens to hold no digit
// ("UbgFXUxjcxDQdzzlvsWgCw"). A word, even a long or capitalised one, changes
// case once or twice at most.
func randomLetters(w string) bool {
	if len(w) < 16 {
		return false
	}
	shifts, prevUpper := 0, false
	for i, r := range w {
		if r > unicode.MaxASCII || !unicode.IsLetter(r) {
			return false
		}
		upper := unicode.IsUpper(r)
		if i > 0 && upper != prevUpper {
			shifts++
		}
		prevUpper = upper
	}
	return shifts >= 6
}

// scan splits normalised text into tokens. known are sorted, non-overlapping spans of identifiers
// found verbatim in the text (see findKnown); they become tokKnown tokens and are never scanned.
func scan(s string, known []span) []token {
	toks := make([]token, 0, len(s)/4+4)
	lineStart := true
	ki := 0
	i := 0
	emit := func(t token) {
		t.text = s[t.start:t.end]
		switch t.kind {
		case tokNum:
			if t.blob == "" {
				t.blob = t.text
			}
		case tokWord:
			t.fw = fold(t.text)
		}
		t.nl = lineStart
		lineStart = false
		toks = append(toks, t)
	}
	for i < len(s) {
		for ki < len(known) && known[ki].end <= i {
			ki++
		}
		if ki < len(known) && known[ki].start == i {
			emit(token{kind: tokKnown, start: i, end: known[ki].end})
			i = known[ki].end
			continue
		}
		r, w := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\n':
			lineStart = true
			i += w
		case r == ' ' || r == nbsp:
			i += w
		case isDigitByte(s[i]):
			i = scanNumberToken(s, i, emit)
		case r == unknownNumber:
			emit(token{kind: tokUnknown, start: i, end: i + w})
			i += w
		case isAlnum(r):
			j := scanWordEnd(s, i)
			emit(token{kind: classifyWord(s[i:j]), start: i, end: j})
			i = j
		case r == '%' || unicode.Is(unicode.Sc, r):
			emit(token{kind: tokSym, start: i, end: i + w})
			i += w
		default:
			emit(token{kind: tokPunct, start: i, end: i + w})
			i += w
		}
	}
	return toks
}

// scanNumberToken scans a number token (or an identifier that starts with digits) at s[i] and
// returns the position after it.
func scanNumberToken(s string, i int, emit func(token)) int {
	if loc := instantRe.FindStringIndex(s[i:]); loc != nil {
		end := i + loc[1]
		if end >= len(s) || !isAlnum(rune(s[end])) {
			emit(token{kind: tokInstant, start: i, end: end})
			return end
		}
	}
	j := scanNumber(s, i)
	if j >= len(s) {
		emit(token{kind: tokNum, start: i, end: j})
		return j
	}
	r, w := utf8.DecodeRuneInString(s[j:])
	switch {
	case r == '%' || r == '°' || r == 'º' || r == 'ª':
		emit(token{kind: tokNum, start: i, end: j + w, blob: s[i:j], sfx: string(r)})
		return j + w
	case unicode.IsLetter(r):
		k := j
		for k < len(s) {
			r2, w2 := utf8.DecodeRuneInString(s[k:])
			if !unicode.IsLetter(r2) {
				break
			}
			k += w2
		}
		if k < len(s) && (isDigitByte(s[k]) || s[k] == '_') {
			// "10h30" is a time; any other letters-then-digits run is an identifier.
			if strings.EqualFold(s[j:k], "h") && k+1 < len(s) && isDigitByte(s[k]) && isDigitByte(s[k+1]) &&
				(k+2 == len(s) || !isAlnum(rune(s[k+2]))) {
				emit(token{kind: tokNum, start: i, end: k + 2, blob: s[i:j] + ":" + s[k:k+2]})
				return k + 2
			}
			e := scanWordEnd(s, i)
			emit(token{kind: tokMixed, start: i, end: e})
			return e
		}
		emit(token{kind: tokNum, start: i, end: k, blob: s[i:j], sfx: strings.ToLower(s[j:k])})
		return k
	}
	emit(token{kind: tokNum, start: i, end: j})
	return j
}

// findKnown returns the sorted, non-overlapping spans where one of ids occurs in s as a whole
// token. An occurrence is a whole token when it is not glued to a longer alphanumeric run, a
// longer dotted/dashed/colon-joined run, or a longer number. Longer identifiers win overlaps.
func findKnown(s string, ids []string) []span {
	var spans []span
	for _, id := range ids {
		if id == "" || len(id) > len(s) {
			continue
		}
		for from := 0; from+len(id) <= len(s); {
			k := strings.Index(s[from:], id)
			if k < 0 {
				break
			}
			start := from + k
			end := start + len(id)
			if idBoundaryBefore(s, start) && idBoundaryAfter(s, end) {
				spans = append(spans, span{start, end})
			}
			from = start + 1
		}
	}
	if len(spans) == 0 {
		return nil
	}
	sortSpans(spans)
	out := spans[:0]
	for _, sp := range spans {
		if n := len(out); n > 0 && sp.start < out[n-1].end {
			continue // overlaps a longer or earlier span
		}
		out = append(out, sp)
	}
	return out
}

// sortSpans orders spans by start, longer first on ties. There are few of them.
func sortSpans(sp []span) {
	for i := 1; i < len(sp); i++ {
		for j := i; j > 0; j-- {
			a, b := sp[j-1], sp[j]
			if a.start < b.start || a.start == b.start && a.end >= b.end {
				break
			}
			sp[j-1], sp[j] = b, a
		}
	}
}

const idJoiners = ".,:/-'@"

func idBoundaryBefore(s string, i int) bool {
	if i == 0 {
		return true
	}
	r, w := utf8.DecodeLastRuneInString(s[:i])
	if isAlnum(r) {
		return false
	}
	if strings.ContainsRune(idJoiners, r) && i-w > 0 {
		r2, _ := utf8.DecodeLastRuneInString(s[:i-w])
		if isAlnum(r2) {
			return false
		}
	}
	return true
}

func idBoundaryAfter(s string, i int) bool {
	if i >= len(s) {
		return true
	}
	r, w := utf8.DecodeRuneInString(s[i:])
	if isAlnum(r) {
		return false
	}
	if strings.ContainsRune(idJoiners, r) && i+w < len(s) {
		r2, _ := utf8.DecodeRuneInString(s[i+w:])
		if isAlnum(r2) {
			return false
		}
	}
	return true
}

// innerUnderscore reports whether w has an underscore between two letters or digits.
func innerUnderscore(w string) bool {
	rs := []rune(w)
	for i := 1; i+1 < len(rs); i++ {
		if rs[i] == '_' && isAlnum(rs[i-1]) && rs[i-1] != '_' && isAlnum(rs[i+1]) && rs[i+1] != '_' {
			return true
		}
	}
	return false
}

// low returns the folded text of the token.
func (t token) low() string {
	if t.fw != "" {
		return t.fw
	}
	return fold(t.text)
}
