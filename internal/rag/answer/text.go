package answer

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file is the no-model text analysis the claim check stands on (master plan
// rule 16: the smallest practical deterministic method first). It reads a claim
// and a passage the same way: lower-cased, accents folded, split into content
// terms (stemmed, stop words removed), numbers (with every reading they may
// have), identifiers, and a negation flag. It judges nothing; support.go does.

// sanitize replaces control and format characters (zero-width characters,
// bidirectional overrides, control codes) other than newline and tab with a
// space, so what a reader sees is what the model reads and what the check
// compares. It is applied to a passage once, and the one result is both what the
// model is shown and what a claim is checked against.
func sanitize(s string) string {
	clean := true
	for _, r := range s {
		if r != '\n' && r != '\t' && isInvisible(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case isInvisible(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isInvisible(r rune) bool {
	return r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// cleanClaim normalises the text of a claim: white space collapsed to single
// spaces. It reports false when the claim holds a control or format character,
// which can only be there to make what is shown differ from what was checked.
func cleanClaim(s string) (string, bool) {
	s = strings.Join(strings.Fields(s), " ")
	for _, r := range s {
		if isInvisible(r) {
			return "", false
		}
	}
	return s, s != ""
}

// clipRunes shortens s to at most n runes and marks the cut.
func clipRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n]), " ") + "..."
}

// The fence tags. They are the labels the system frame tells the model to treat as
// untrusted data. Text that contains one of them, in any case or spacing, is
// neutralised so data cannot close its own fence early.
const (
	tagEvidence = "evidence"
	tagQuestion = "question"
)

var fenceBreak = regexp.MustCompile(`(?i)<\s*/?\s*(` + tagEvidence + `|` + tagQuestion + `)`)

// fenceText neutralises fence tags inside untrusted text. The text is already
// sanitised by the caller.
func fenceText(text string) string {
	if strings.IndexByte(text, '<') < 0 { // a fence tag needs a '<'; most text has none
		return text
	}
	return fenceBreak.ReplaceAllStringFunc(text, func(m string) string {
		return "&lt;" + strings.TrimLeft(m, "< \t\r\n")
	})
}

// ----- folding -------------------------------------------------------------

var foldMap = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a', 'ā': 'a',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e', 'ē': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i', 'ī': 'i',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o', 'ō': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u', 'ū': 'u',
	'ç': 'c', 'ñ': 'n', 'ß': 's',
	'’': '\'', '‘': '\'', 'ʼ': '\'', '′': '\'',
	'−': '-', '‐': '-', '‑': '-', '–': '-',
}

// fold lower-cases s and strips the accents of the Latin letters English and
// Italian use, so "È" and "e" meet. It is the same function for claim and passage.
func fold(s string) string {
	ascii, lower := true, true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= utf8.RuneSelf {
			ascii = false
			break
		}
		if c >= 'A' && c <= 'Z' {
			lower = false
		}
	}
	if ascii && lower {
		return s // the usual case: nothing to fold, nothing to allocate
	}
	return strings.Map(func(r rune) rune {
		if r < utf8.RuneSelf {
			if r >= 'A' && r <= 'Z' {
				return r + ('a' - 'A')
			}
			return r
		}
		r = unicode.ToLower(r)
		if m, ok := foldMap[r]; ok {
			return m
		}
		return r
	}, s)
}

// ----- vocabulary ----------------------------------------------------------

// stopwords carry no claim: articles, prepositions, auxiliaries and pronouns of
// English and Italian. Words that change what a sentence means (all, only, each,
// more, before, after) are deliberately absent: a claim that says "all" must find
// "all". Negations are not stop words either, they are a polarity flag.
var stopwords = setOf(
	// English
	"an", "the", "and", "or", "but", "if", "of", "in", "on", "at", "to", "for", "from", "by", "with",
	"as", "is", "are", "was", "were", "be", "been", "being", "am", "it", "its", "this", "that", "these",
	"those", "there", "here", "which", "who", "whom", "whose", "what", "when", "where", "how", "why",
	"do", "does", "did", "have", "has", "had", "having", "will", "would", "shall", "should", "can",
	"could", "may", "might", "must", "than", "then", "so", "such", "very", "also", "about", "into",
	"per", "up", "out", "you", "your", "we", "our", "they", "their", "he", "she", "his", "her", "them",
	"us", "me", "my", "i", "any", "some", "these", "while", "because", "after", "until",
	// Italian
	"il", "lo", "la", "gli", "le", "un", "uno", "una", "di", "da", "con", "su", "tra", "fra", "ed",
	"ma", "se", "che", "chi", "cui", "sono", "era", "erano", "essere", "stato", "stati", "stata",
	"state", "ha", "hanno", "ho", "hai", "abbiamo", "avete", "avere", "questo", "questa", "questi",
	"queste", "quello", "quella", "quelli", "quelle", "del", "dello", "della", "dei", "degli", "delle",
	"al", "allo", "alla", "ai", "agli", "alle", "dal", "dallo", "dalla", "dai", "dagli", "dalle", "nel",
	"nello", "nella", "nei", "negli", "nelle", "sul", "sullo", "sulla", "sui", "sugli", "sulle", "col",
	"come", "anche", "si", "ci", "li", "ne", "mi", "ti", "vi", "suo", "sua", "suoi", "sue", "loro",
	"puo", "possono", "deve", "devono", "viene", "vengono", "ogni", "essa", "esso", "dove", "quando",
	"perche", "mentre", "poi", "quindi", "inoltre",
)

// negations are not content: they flip what a sentence says, so the claim and
// the passage that supports it must agree on whether there is one.
var negations = setOf(
	"not", "no", "never", "none", "nor", "neither", "without", "cannot", "nothing", "nobody", "nowhere",
	"non", "ne", "mai", "nessuno", "nessuna", "nessun", "niente", "nulla", "senza", "neppure", "nemmeno",
)

// elisions are the Italian words that lose a vowel before an apostrophe
// ("dell'ordine", "l'importo"): the part before the apostrophe is dropped.
var elisions = setOf(
	"l", "d", "dell", "dall", "nell", "sull", "all", "quest", "quell", "anch", "c", "un", "s", "m", "t",
	"v", "n", "po", "com", "dov", "cos", "senz", "bell", "sant",
)

func setOf(words ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(words))
	for _, w := range words {
		m[w] = struct{}{}
	}
	return m
}

// stem reduces a word to a form two inflections of it share, in English and
// Italian, with no dictionary: plural and verb endings come off, then one final
// vowel. It errs towards merging ("plane", "plan") which can only make a claim
// look better supported by a word of the same stem, never by a different number
// or identifier, and only on a window that already matches the claim's other terms.
func stem(w string) string {
	n := utf8.RuneCountInString(w)
	if n < 4 {
		return w
	}
	fired := true
	switch {
	case strings.HasSuffix(w, "ies") && n > 4:
		w = w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "ing") && n > 5:
		w = w[:len(w)-3]
	case strings.HasSuffix(w, "ed") && n > 4:
		w = w[:len(w)-2]
	case strings.HasSuffix(w, "es") && n > 4:
		w = w[:len(w)-2]
	case strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && n > 3:
		w = w[:len(w)-1]
	default:
		fired = false
	}
	if !fired && utf8.RuneCountInString(w) >= 4 {
		switch w[len(w)-1] {
		case 'a', 'e', 'i', 'o':
			w = w[:len(w)-1]
		}
	}
	return w
}

// ----- tokenising ----------------------------------------------------------

// analysis is what the check reads from a text.
type analysis struct {
	stems  []string   // distinct content stems, in order of first appearance
	nums   [][]string // one entry per numeric token: its readings (see numberReadings)
	idents []string   // distinct identifier-like tokens, lower-cased
	neg    bool       // the text contains a negation
}

// weight is the count of things a claim states: terms, numbers and identifiers.
func (a analysis) weight() int { return len(a.stems) + len(a.nums) + len(a.idents) }

// analyze reads s.
func analyze(s string) analysis {
	f := fold(s)
	var a analysis
	seenStem := map[string]struct{}{}
	seenIdent := map[string]struct{}{}
	addStem := func(w string) {
		if _, dup := seenStem[w]; !dup {
			seenStem[w] = struct{}{}
			a.stems = append(a.stems, w)
		}
	}
	addIdent := func(w string) {
		if _, dup := seenIdent[w]; !dup {
			seenIdent[w] = struct{}{}
			a.idents = append(a.idents, w)
		}
	}
	addNum := func(tok string, negative bool) {
		if r := numberReadings(tok, negative); len(r) > 0 {
			a.nums = append(a.nums, r)
		} else {
			// A numeric token with no valid reading ("1.2.3,4") cannot be
			// verified: it is kept as a token that no passage can satisfy.
			a.nums = append(a.nums, []string{"?:" + tok})
		}
	}
	addWord := func(w string) {
		w = strings.Trim(w, "'")
		if utf8.RuneCountInString(w) < 2 {
			return
		}
		if _, neg := negations[w]; neg {
			a.neg = true
			return
		}
		if _, stop := stopwords[w]; stop {
			return
		}
		addStem(stem(w))
	}
	for _, loc := range chunks(f) {
		ch := f[loc[0]:loc[1]]
		switch {
		case strings.HasPrefix(ch, "http://") || strings.HasPrefix(ch, "https://") || strings.Contains(ch, "@"):
			addIdent(strings.TrimRight(ch, ".,;:"))
			continue
		}
		hasDigit, hasLetter := false, false
		for _, r := range ch {
			switch {
			case unicode.IsDigit(r):
				hasDigit = true
			case unicode.IsLetter(r):
				hasLetter = true
			}
		}
		switch {
		case hasDigit && !hasLetter:
			negative := isNegative(f, loc[0])
			if isNumeric(ch) {
				addNum(ch, negative)
				continue
			}
			ascii := true
			for _, r := range ch {
				if unicode.IsDigit(r) && r > unicode.MaxASCII {
					ascii = false
				}
			}
			if !ascii {
				addIdent(ch)
				continue
			}
			for _, part := range strings.FieldsFunc(ch, func(r rune) bool { return !(r >= '0' && r <= '9') }) {
				addNum(part, negative)
			}
		case hasDigit && hasLetter:
			if num, unit, ok := splitUnit(ch); ok {
				addNum(num, isNegative(f, loc[0]))
				addWord(unit)
				continue
			}
			addIdent(ch)
		default:
			for _, piece := range strings.FieldsFunc(ch, func(r rune) bool { return !unicode.IsLetter(r) && r != '\'' }) {
				a.wordsOfPiece(piece, addWord)
			}
		}
	}
	return a
}

// wordsOfPiece feeds the words of one apostrophe-joined piece to add:
// "don't" is a negation, "dell'ordine" is "ordine", "company's" is "company".
func (a *analysis) wordsOfPiece(piece string, add func(string)) {
	if !strings.Contains(piece, "'") {
		add(piece)
		return
	}
	if strings.HasSuffix(piece, "n't") {
		a.neg = true
		return
	}
	parts := strings.Split(piece, "'")
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i < len(parts)-1 {
			if _, el := elisions[p]; el {
				continue
			}
		}
		add(p)
	}
}

// isNegative reports whether the number at byte offset i of f carries a minus sign:
// a hyphen or minus directly before it, itself not glued to a word or a digit.
func isNegative(f string, i int) bool {
	if i == 0 || f[i-1] != '-' {
		return false
	}
	if i == 1 {
		return true
	}
	prev, _ := utf8.DecodeLastRuneInString(f[:i-1])
	return !unicode.IsLetter(prev) && !unicode.IsDigit(prev)
}

// ----- numbers -------------------------------------------------------------

// numberReadings returns the canonical values a numeric token may have, each
// prefixed with its kind so a time, a date and a quantity never compare equal:
//
//	"1,234.56" and "1.234,56"   n:1234.56
//	"1.234"                     n:1234 and n:1.234 (a thousand, or a decimal: ambiguous)
//	"2.50"                      n:2.5
//	"14/10/2026", "2026-10-14"  d:14-10-2026 and d:2026-10-14 (the same date either way)
//	"10-20"                     d:10-20, d:20-10, n:10, n:20 (a range names its ends)
//	"09:05"                     t:9:05
//
// A token that has no valid reading ("1.2.3,4") has none. Two numbers support one
// another when any of their readings is equal, which is the same leniency about
// a locale's separators the response verifier (internal/verify) shows.
func numberReadings(tok string, negative bool) []string {
	sign := ""
	if negative {
		sign = "-"
	}
	isSep := func(r rune) bool { return r < '0' || r > '9' }
	switch {
	case strings.ContainsRune(tok, ':'):
		// "09:05" and "9:05" are one time: only the leading part loses its zeros.
		parts := strings.FieldsFunc(tok, isSep)
		if len(parts) < 2 {
			return nil
		}
		parts[0] = trimZeros(parts[0])
		return []string{"t:" + strings.Join(parts, ":")}
	case strings.ContainsRune(tok, '/'):
		return dateReadings(strings.FieldsFunc(tok, isSep), false)
	case strings.ContainsRune(tok, '-'):
		return dateReadings(strings.FieldsFunc(tok, isSep), true)
	}
	return plainReadings(tok, sign)
}

func dateReadings(parts []string, rangeLike bool) []string {
	if len(parts) < 2 {
		return nil
	}
	fwd := make([]string, len(parts))
	rev := make([]string, len(parts))
	for i, p := range parts {
		fwd[i] = trimZeros(p)
		rev[len(parts)-1-i] = fwd[i]
	}
	out := []string{"d:" + strings.Join(fwd, "-"), "d:" + strings.Join(rev, "-")}
	if rangeLike && len(parts) == 2 {
		out = append(out, "n:"+fwd[0], "n:"+fwd[1])
	}
	return out
}

// plainReadings handles digits with "." and "," only.
func plainReadings(tok, sign string) []string {
	hasDot, hasComma := strings.ContainsRune(tok, '.'), strings.ContainsRune(tok, ',')
	switch {
	case !hasDot && !hasComma:
		return []string{canonical(tok, "", sign)}
	case hasDot && hasComma:
		last := strings.LastIndexAny(tok, ".,")
		dec, grp := tok[last], byte('.')
		if dec == '.' {
			grp = ','
		}
		intPart, frac := tok[:last], tok[last+1:]
		if strings.IndexByte(intPart, dec) >= 0 || !validGrouping(intPart, grp) {
			return nil
		}
		return []string{canonical(removeByte(intPart, grp), frac, sign)}
	}
	sep := byte('.')
	if hasComma {
		sep = ','
	}
	if strings.Count(tok, string(sep)) > 1 {
		if !validGrouping(tok, sep) {
			return nil
		}
		return []string{canonical(removeByte(tok, sep), "", sign)}
	}
	i := strings.IndexByte(tok, sep)
	left, right := tok[:i], tok[i+1:]
	out := []string{canonical(left, right, sign)}
	if len(right) == 3 && left[0] != '0' && validGrouping(tok, sep) {
		out = append(out, canonical(left+right, "", sign))
	}
	return out
}

// validGrouping reports whether s is digits grouped by sep: one to three digits,
// then groups of exactly three.
func validGrouping(s string, sep byte) bool {
	parts := strings.Split(s, string(sep))
	for i, p := range parts {
		if p == "" || len(p) > 3 || (i > 0 && len(p) != 3) {
			return false
		}
	}
	return true
}

func removeByte(s string, b byte) string { return strings.ReplaceAll(s, string(b), "") }

func trimZeros(s string) string {
	t := strings.TrimLeft(s, "0")
	if t == "" {
		return "0"
	}
	return t
}

func canonical(intPart, frac, sign string) string {
	frac = strings.TrimRight(frac, "0")
	v := trimZeros(intPart)
	if frac != "" {
		v += "." + frac
	}
	if v == "0" {
		sign = ""
	}
	return "n:" + sign + v
}

// ----- sentences -----------------------------------------------------------

// maxSentences bounds the work on one passage; a passage is already capped by the
// package's token budget, so this is a backstop, not a limit anyone meets.
const maxSentences = 2048

// sentences splits a passage into the units a claim is matched against: at line
// ends, and after a full stop, question mark, exclamation mark, ellipsis or
// semicolon that is followed by white space. "3.5" and "e.g.x" do not split.
func sentences(text string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" && len(out) < maxSentences {
			out = append(out, s)
		}
		cur.Reset()
	}
	rs := []rune(text)
	for i, r := range rs {
		switch r {
		case '\n', '\r':
			flush()
			continue
		}
		cur.WriteRune(r)
		switch r {
		case '.', '!', '?', ';', '…':
			if i+1 >= len(rs) || unicode.IsSpace(rs[i+1]) {
				flush()
			}
		}
	}
	flush()
	return out
}

// chunks returns the byte ranges of the units of folded text: a URL, or a run of
// letters and digits that continues across one separator ('.', '_', '-', '/', '@',
// ':', '+', ',') when a letter or digit follows it, and across an apostrophe followed
// by letters. It is a hand-written scanner: the equivalent regular expression was the
// largest cost of verifying a claim after the model call.
func chunks(f string) [][2]int {
	var out [][2]int
	isAlnum := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	skip := func(j int, pred func(rune) bool) int {
		for j < len(f) {
			r, n := utf8.DecodeRuneInString(f[j:])
			if !pred(r) {
				break
			}
			j += n
		}
		return j
	}
	nextIs := func(j int, pred func(rune) bool) bool {
		if j >= len(f) {
			return false
		}
		r, _ := utf8.DecodeRuneInString(f[j:])
		return pred(r)
	}
	for i := 0; i < len(f); {
		if strings.HasPrefix(f[i:], "http://") || strings.HasPrefix(f[i:], "https://") {
			j := i + strings.Index(f[i:], "//") + 2
			end := skip(j, func(r rune) bool { return !unicode.IsSpace(r) && !strings.ContainsRune(`<>"'()[]`, r) })
			if end > j {
				out = append(out, [2]int{i, end})
				i = end
				continue
			}
		}
		r, n := utf8.DecodeRuneInString(f[i:])
		if !isAlnum(r) {
			i += n
			continue
		}
		j := i
		for {
			j = skip(j, isAlnum)
			for j < len(f) && f[j] == '\'' && nextIs(j+1, unicode.IsLetter) {
				j = skip(j+1, unicode.IsLetter)
			}
			if j < len(f) && strings.IndexByte("._-/@:+,", f[j]) >= 0 && nextIs(j+1, isAlnum) {
				j++
				continue
			}
			break
		}
		out = append(out, [2]int{i, j})
		i = j
	}
	return out
}

// isNumeric reports whether ch is digits joined by '.', ',', ':', '/' or '-'.
func isNumeric(ch string) bool {
	prevDigit := false
	for i := 0; i < len(ch); i++ {
		switch c := ch[i]; {
		case c >= '0' && c <= '9':
			prevDigit = true
		case strings.IndexByte(".,:/-", c) >= 0 && prevDigit && i+1 < len(ch):
			prevDigit = false
		default:
			return false
		}
	}
	return prevDigit
}

// splitUnit splits a number glued to a short unit ("10kg", "3.5m") into the number
// and the unit.
func splitUnit(ch string) (num, unit string, ok bool) {
	i := 0
	for i < len(ch) && ch[i] >= '0' && ch[i] <= '9' {
		i++
	}
	if i == 0 {
		return "", "", false
	}
	if i+1 < len(ch) && (ch[i] == '.' || ch[i] == ',') && ch[i+1] >= '0' && ch[i+1] <= '9' {
		i++
		for i < len(ch) && ch[i] >= '0' && ch[i] <= '9' {
			i++
		}
	}
	num, unit = ch[:i], ch[i:]
	if len(unit) < 1 || len(unit) > 4 {
		return "", "", false
	}
	for k := 0; k < len(unit); k++ {
		if unit[k] < 'a' || unit[k] > 'z' {
			return "", "", false
		}
	}
	return num, unit, true
}
