package verify

import (
	"strconv"
	"strings"
)

// extractor turns normalised text into claims. One pass over the tokens, left to right; a claim
// consumes the tokens it is made of, so the "14" of "14 October" and the "20991" of "apt_20991"
// are never read a second time as numbers.
type extractor struct {
	s       string
	lang    Language
	toks    []token
	out     []claim
	curs    map[string]bool // currency codes recognised in addition to the built-in ones
	usedCur map[int]bool    // currency tokens already attached to a number
	author  *authored       // text the core and host wrote, or nil
	inAuth  []bool          // inAuth[i]: toks[i] is part of authored text quoted verbatim
	inValue []bool          // inValue[i]: toks[i] is part of a host fact's value stated with its label
	markers [100]bool       // markers[n]: a list numeral n has been read (see isListMarker)
}

// extractClaims reads every checkable statement out of s. known are the spans of identifiers that
// occur verbatim (see findKnown). extraCurs are further currency codes to recognise, upper case.
func extractClaims(s string, lang Language, known []span, extraCurs map[string]bool, author *authored) []claim {
	e := newExtractor(s, lang, known, extraCurs, author)
	e.markAuthored()
	e.run()
	return e.out
}

func newExtractor(s string, lang Language, known []span, extraCurs map[string]bool, author *authored) *extractor {
	return &extractor{s: s, lang: lang, toks: scan(s, known), curs: extraCurs, usedCur: map[int]bool{}, author: author}
}

// words lists the folded words of the scanned text: every token that is not punctuation.
func (e *extractor) words() []string {
	out := make([]string, 0, len(e.toks))
	for _, t := range e.toks {
		if t.kind != tokPunct && t.kind != tokSym {
			out = append(out, t.low())
		}
	}
	return out
}

func (e *extractor) emit(kind claimKind, from, to int, readings [][]atom) {
	start, end := e.toks[from].start, e.toks[to-1].end
	e.out = append(e.out, claim{kind: kind, start: start, end: end, text: e.s[start:end], readings: readings})
}

func (e *extractor) run() {
	for i := 0; i < len(e.toks); {
		t := e.toks[i]
		switch t.kind {
		case tokKnown, tokSym:
			i++
		case tokPunct:
			i = e.punct(i)
		case tokInstant:
			rs := [][]atom{{{kind: atomUnknown}}}
			if a, ok := instantAtom(t.text); ok {
				rs = [][]atom{{a}}
			}
			e.emit(ckDate, i, i+1, rs)
			i++
		case tokMixed:
			if strings.HasPrefix(t.text, "src_") && typedIDRe.MatchString(t.text) {
				e.emit(ckCitation, i, i+1, [][]atom{{idAtom(t.text)}})
			} else {
				e.emit(ckID, i, i+1, [][]atom{{idAtom(t.text)}})
			}
			i++
		case tokUnknown:
			e.emit(ckNumber, i, i+1, [][]atom{{{kind: atomUnknown}}})
			i++
		case tokNum:
			i = e.number(i)
		case tokWord:
			i = e.word(i)
		default:
			i++
		}
	}
}

// punct recognises "[n]" citation markers; every other punctuation mark is ignored.
func (e *extractor) punct(i int) int {
	t := e.toks[i]
	if t.text != "[" || i+2 >= len(e.toks) {
		return i + 1
	}
	n, c := e.toks[i+1], e.toks[i+2]
	// The number may have been taken for a known identifier ("2" as a choice ID): it is still a marker.
	isNum := n.kind == tokNum && n.sfx == "" || n.kind == tokKnown && allDigits(n.text)
	if isNum && n.start == t.end && c.kind == tokPunct && c.text == "]" && c.start == n.end {
		digits := n.blob
		if n.kind == tokKnown {
			digits = n.text
		}
		if p := classifyGrouped(digits, e.lang); p.isPlainInt() && p.digits <= 4 {
			e.emit(ckCitation, i, i+3, [][]atom{{{kind: atomCiteIdx, n1: p.plainInt}}})
			return i + 3
		}
	}
	return i + 1
}

// isListMarker reports whether the number at toks[i] is a list numeral ("1." or "2)" at the start
// of a line) rather than a stated number. A list starts at 0 or 1 and goes on one at a time, so a
// numeral is one only when it is a start or the one before it was a numeral too ("50." under a
// line that ends in 5 is a number); a list may resume after a paragraph or a nested list.
func (e *extractor) isListMarker(i int) bool {
	t := e.toks[i]
	if !t.nl || t.sfx != "" || len(t.blob) > 2 || !allDigits(t.blob) || i+1 >= len(e.toks) {
		return false
	}
	p := e.toks[i+1]
	if p.kind != tokPunct || p.start != t.end || (p.text != "." && p.text != ")") {
		return false
	}
	if !(p.end >= len(e.s) || e.s[p.end] == ' ' || e.s[p.end] == '\n' || strings.HasPrefix(e.s[p.end:], nbspStr)) {
		return false
	}
	n := atoiSmall(t.blob)
	if n > 1 && !e.markers[n-1] {
		return false
	}
	e.markers[n] = true
	return true
}

func (e *extractor) wordAt(i int) (string, bool) {
	if i < 0 || i >= len(e.toks) || e.toks[i].kind != tokWord {
		return "", false
	}
	return e.toks[i].low(), true
}

func (e *extractor) punctAt(i int, p string) bool {
	return i >= 0 && i < len(e.toks) && e.toks[i].kind == tokPunct && e.toks[i].text == p
}

// monthOf classifies a folded word as a month: class 1 is a full name, 2 a full name that is also
// an ordinary English word, 3 an abbreviation. Class 0 is not a month.
func monthOf(w string) (m, class int) {
	if v, ok := monthFull[w]; ok {
		return v, 1
	}
	if v, ok := monthAmbiguous[w]; ok {
		return v, 2
	}
	if v, ok := monthAbbr[w]; ok {
		return v, 3
	}
	return 0, 0
}

var ordinalSuffixes = map[string]bool{"": true, "st": true, "nd": true, "rd": true, "th": true, "°": true, "º": true, "ª": true}

// dayToken reads toks[i] as a day of the month: a plain integer 1..31 with an optional ordinal
// suffix.
func (e *extractor) dayToken(i int) (int, bool) {
	if i < 0 || i >= len(e.toks) {
		return 0, false
	}
	t := e.toks[i]
	if t.kind != tokNum || !ordinalSuffixes[t.sfx] || !allDigits(t.blob) || len(t.blob) > 2 {
		return 0, false
	}
	d := atoiSmall(t.blob)
	return d, d >= 1 && d <= 31
}

// yearToken reads toks[i] as a four-digit year.
func (e *extractor) yearToken(i int) (int, bool) {
	if i < 0 || i >= len(e.toks) {
		return 0, false
	}
	t := e.toks[i]
	if t.kind != tokNum || t.sfx != "" || !allDigits(t.blob) || len(t.blob) != 4 {
		return 0, false
	}
	y := atoiSmall(t.blob)
	return y, y >= 1000 && y <= 2999
}

// yearAfter reads an optional year after a month at toks[j]: "2026" or ", 2026". It returns the
// year (0 when there is none) and the index after it.
func (e *extractor) yearAfter(j int) (int, int) {
	k := j
	if e.punctAt(k, ",") {
		k++
	}
	if y, ok := e.yearToken(k); ok {
		return y, k + 1
	}
	return 0, j
}

// tryDate reads a date that starts at toks[i]: a numeric date, "14 October [2026]" (also "14th of
// October", "14 ottobre"), "October 14[, 2026]" or "October 2026". It returns its readings and the
// index after it.
func (e *extractor) tryDate(i int) ([][]atom, int, bool) {
	if i < 0 || i >= len(e.toks) {
		return nil, i, false
	}
	t := e.toks[i]
	switch t.kind {
	case tokNum:
		if t.sfx == "" {
			if parts := classifyBlob(t.blob, e.lang); len(parts) == 1 && !parts[0].isNumber && isDateParts(parts[0]) {
				return parts[0].readings, i + 1, true
			}
		}
		d, ok := e.dayToken(i)
		if !ok {
			return nil, i, false
		}
		j := i + 1
		if w, ok := e.wordAt(j); ok && dateConnectors[w] {
			j++
		}
		w, ok := e.wordAt(j)
		if !ok {
			return nil, i, false
		}
		m, class := monthOf(w)
		if m == 0 {
			return nil, i, false
		}
		j++
		if class == 3 && e.punctAt(j, ".") {
			j++
		}
		y, next := e.yearAfter(j)
		if !validDate(y, m, d) {
			return nil, i, false
		}
		return [][]atom{{dateAtom(y, m, d)}}, next, true
	case tokWord:
		w := t.low()
		m, class := monthOf(w)
		if m == 0 {
			return nil, i, false
		}
		j := i + 1
		if class == 3 && e.punctAt(j, ".") {
			j++
		}
		if d, ok := e.dayToken(j); ok {
			y, next := e.yearAfter(j + 1)
			if !validDate(y, m, d) {
				return nil, i, false
			}
			return [][]atom{{dateAtom(y, m, d)}}, next, true
		}
		if y, ok := e.yearToken(j); ok {
			return [][]atom{{dateAtom(y, m, 0)}}, j + 1, true
		}
	}
	return nil, i, false
}

func isDateParts(p part) bool {
	if len(p.readings) == 0 {
		return false
	}
	for _, r := range p.readings {
		for _, a := range r {
			if a.kind != atomDate {
				return false
			}
		}
	}
	return true
}

func isTimePart(p part) bool {
	return !p.isNumber && len(p.readings) == 1 && len(p.readings[0]) == 1 && p.readings[0][0].kind == atomTime
}

func withSign(rs [][]atom) [][]atom {
	out := make([][]atom, len(rs))
	for i, r := range rs {
		nr := make([]atom, len(r))
		for j, a := range r {
			if a.kind == atomNumber && a.s != "0" {
				a.s = "-" + a.s
			}
			nr[j] = a
		}
		out[i] = nr
	}
	return out
}

// negativeBefore reports whether the number token is written with a minus sign.
func (e *extractor) negativeBefore(t token) bool {
	if t.start == 0 || e.s[t.start-1] != '-' {
		return false
	}
	if t.start == 1 {
		return true
	}
	prev := e.s[t.start-2]
	return prev == ' ' || prev == '(' || prev == '\n' || prev == ':' || prev == '=' || strings.HasSuffix(e.s[:t.start-1], nbspStr)
}

func hourFromClock(h int, pm bool) (int, bool) {
	if h < 1 || h > 12 {
		return 0, false
	}
	h %= 12
	if pm {
		h += 12
	}
	return h, true
}

func ampmOf(w string) (pm, ok bool) {
	switch w {
	case "am", "a.m":
		return false, true
	case "pm", "p.m":
		return true, true
	}
	return false, false
}

// number reads the numeric token at toks[i] together with what surrounds it.
func (e *extractor) number(i int) int {
	t := e.toks[i]
	if e.isListMarker(i) {
		return i + 2
	}
	// A date that starts with this token: "14 October 2026", "14/10/2026".
	if rs, next, ok := e.tryDate(i); ok {
		e.emit(ckDate, i, next, rs)
		return next
	}
	parts := classifyBlob(t.blob, e.lang)
	neg := e.negativeBefore(t)
	if len(parts) != 1 {
		for _, p := range parts {
			rs := p.readings
			if neg && p.isNumber {
				rs = withSign(rs)
			}
			e.emit(claimKindOf(p), i, i+1, rs)
		}
		return i + 1
	}
	p := parts[0]
	if !p.isNumber && !isTimePart(p) {
		e.emit(claimKindOf(p), i, i+1, p.readings)
		return i + 1
	}
	return e.compose(i, i+1, p, t.sfx, neg)
}

func claimKindOf(p part) claimKind {
	switch {
	case p.isNumber:
		return ckNumber
	case isTimePart(p):
		return ckTime
	case isDateParts(p):
		return ckDate
	}
	return ckNumber
}

// ampmAfter reads an am/pm marker attached to a number (sfx) or in the next token.
func (e *extractor) ampmAfter(end int, sfx string) (pm, has bool, next int) {
	if sfx == "am" || sfx == "pm" {
		return sfx == "pm", true, end
	}
	if sfx == "" {
		if w, ok := e.wordAt(end); ok {
			if pm2, ok2 := ampmOf(w); ok2 {
				return pm2, true, end + 1
			}
		}
	}
	return false, false, end
}

// scaleAfter reads a scale word attached to a number (sfx) or in the next token. It returns the
// power of ten and the index after it.
func (e *extractor) scaleAfter(end int, sfx string) (int, int, bool) {
	if sfx != "" {
		zeros, ok := scaleWords[sfx]
		return zeros, end, ok
	}
	if w, ok := e.wordAt(end); ok {
		if zeros, ok := scaleWords[w]; ok {
			return zeros, end + 1, true
		}
	}
	return 0, end, false
}

// compose finishes a number or clock time that occupies toks[i:end]: it may be the time of day
// ("10:30 pm", "alle 10"), an amount of money ("EUR 50", "50 euro") or just a number.
func (e *extractor) compose(i, end int, p part, sfx string, neg bool) int {
	// Clock time, possibly with am/pm.
	if isTimePart(p) {
		a := p.readings[0][0]
		pm, has, next := e.ampmAfter(end, sfx)
		if has {
			if h, ok := hourFromClock(a.n1, pm); ok {
				a.n1 = h
			}
		}
		e.emit(ckTime, i, next, [][]atom{{a}})
		return next
	}
	// "5k", "5 mila", "2,5 milioni": the scale multiplies the number, which is then neither an hour
	// nor a day.
	if zeros, next, ok := e.scaleAfter(end, sfx); ok && p.isNumber && len(p.nums) > 0 {
		nums := make([]string, len(p.nums))
		for k, n := range p.nums {
			nums[k] = shiftDecimal(n, zeros)
		}
		p, sfx, end = numberPart(nums), "", next
	}
	readings := p.readings
	if neg {
		readings = withSign(readings)
	}
	// am/pm after a bare hour ("10 am", "10pm") or a dot time ("10.30 pm").
	if pm, has, next := e.ampmAfter(end, sfx); has && !neg {
		if p.isPlainInt() && p.digits <= 2 {
			if h, ok := hourFromClock(p.plainInt, pm); ok {
				e.emit(ckTime, i, next, [][]atom{{timeAtom(h, 0, -1)}})
				return next
			}
		}
		for _, r := range p.readings {
			if len(r) == 1 && r[0].kind == atomTime {
				a := r[0]
				if h, ok := hourFromClock(a.n1, pm); ok {
					a.n1 = h
					e.emit(ckTime, i, next, [][]atom{{a}})
					return next
				}
			}
		}
	}
	// "10h30" and friends are handled by the scanner; "5 kg", "20%", "3rd" are plain numbers.
	cur, at, ok := e.currencyAround(i, end)
	if c, found := currencyWords[sfx]; !ok && found { // "50eur"
		cur, at, ok = c, -1, true
	}
	if ok && p.isNumber && len(p.nums) > 0 {
		var rs [][]atom
		for _, n := range p.nums {
			if neg && n != "0" {
				n = "-" + n
			}
			rs = append(rs, []atom{{kind: atomMoney, s: n, cur: cur}})
		}
		if at >= 0 {
			e.usedCur[at] = true
		}
		to := end
		if at >= end {
			to = at + 1
		}
		e.emit(ckMoney, i, to, rs)
		return to
	}
	// A percent sign that stands apart from the number.
	next := end
	if next < len(e.toks) && e.toks[next].kind == tokSym && e.toks[next].text == "%" {
		next++
	}
	// "at 10", "alle 10": a bare hour may be a time of day.
	if p.isPlainInt() && p.plainInt <= 23 && p.digits <= 2 && sfx == "" {
		if w, ok := e.wordAt(i - 1); ok && timePrefixes[w] {
			readings = append(append([][]atom{}, readings...), []atom{timeAtom(p.plainInt, 0, -1)})
		}
	}
	// "the 14th": an ordinal is also a day of the month.
	if p.isPlainInt() && p.plainInt >= 1 && p.plainInt <= 31 && sfx != "" && ordinalSuffixes[sfx] {
		readings = append(append([][]atom{}, readings...), []atom{dateAtom(0, 0, p.plainInt)})
	}
	e.emit(ckNumber, i, next, readings)
	return next
}

// currencyAround finds a currency symbol, word or code directly before toks[from] or directly
// after toks[to-1], unused by another number. It returns the ISO code and the token's index.
func (e *extractor) currencyAround(from, to int) (string, int, bool) {
	if c, ok := e.currencyAt(from - 1); ok && !e.usedCur[from-1] {
		return c, from - 1, true
	}
	if c, ok := e.currencyAt(to); ok && !e.usedCur[to] {
		return c, to, true
	}
	return "", 0, false
}

func (e *extractor) currencyAt(i int) (string, bool) {
	if i < 0 || i >= len(e.toks) {
		return "", false
	}
	t := e.toks[i]
	switch t.kind {
	case tokSym:
		if r := firstRune(t.text); r != '%' {
			c, ok := currencySymbols[r]
			return c, ok
		}
	case tokWord:
		w := t.low()
		if c, ok := currencyWords[w]; ok {
			return c, true
		}
		if up := strings.ToUpper(w); e.curs[up] && len(up) == 3 {
			return up, true
		}
	}
	return "", false
}

// numberWordsAt reads number words ("twenty-four", "ventiquattro", "two hundred") starting at
// toks[i]. It returns the value and the index after the last token used.
func (e *extractor) numberWordsAt(i int) (int64, int, bool) {
	t := e.toks[i]
	w := t.low()
	// Italian numbers are single words.
	if !strings.Contains(w, "-") {
		if len(w) >= 3 {
			if v, ok := parseItalianNumber(w, e.lang == LangIT); ok {
				return v, i + 1, true
			}
		}
	}
	// English numbers are one or more words, possibly hyphenated.
	if !isEnglishNumberWord(strings.SplitN(w, "-", 2)[0]) {
		return 0, i, false
	}
	var parts []string
	var tokenOf []int
	for j := i; j < len(e.toks) && e.toks[j].kind == tokWord && len(parts) < 12; j++ {
		for _, p := range strings.Split(e.toks[j].low(), "-") {
			parts = append(parts, p)
			tokenOf = append(tokenOf, j)
		}
	}
	v, used := parseEnglishNumber(parts)
	if used == 0 {
		return 0, i, false
	}
	return v, tokenOf[used-1] + 1, true
}

// withWeekday adds weekday wd to the readings of the date it qualifies.
func withWeekday(rs [][]atom, wd int) [][]atom {
	var withWd [][]atom
	for _, r := range rs {
		nr := append([]atom{}, r...)
		for _, a := range r {
			if a.kind == atomDate && a.n3 != 0 {
				nr = append(nr, atom{kind: atomWdDate, n1: wd, n2: a.n1, n3: a.n2, n4: a.n3})
			}
		}
		if len(nr) == len(r) {
			nr = append(nr, atom{kind: atomWeekday, n1: wd})
		}
		withWd = append(withWd, nr)
	}
	return withWd
}

// word reads the word token at toks[i].
func (e *extractor) word(i int) int {
	t := e.toks[i]
	w := t.low()

	// Months: "October", "Oct 14", "14 Oct" (the latter is read from the day).
	if m, class := monthOf(w); m > 0 {
		if rs, next, ok := e.tryDate(i); ok {
			e.emit(ckDate, i, next, rs)
			return next
		}
		if class == 1 {
			e.emit(ckDate, i, i+1, [][]atom{{dateAtom(0, m, 0)}})
		}
		return i + 1
	}

	// Weekdays, alone or in front of the date they qualify.
	if wd, ok := weekdayNames[w]; ok {
		j := i + 1
		for k := 0; k < 3 && j < len(e.toks); k++ {
			if e.punctAt(j, ",") || e.punctAt(j, "'") {
				j++
			} else if g, ok := e.wordAt(j); ok && weekdayDateGap[g] {
				j++
			} else {
				break
			}
		}
		if rs, next, ok := e.tryDate(j); ok {
			e.emit(ckDate, i, next, withWeekday(rs, wd))
			return next
		}
		e.emit(ckWeekday, i, i+1, [][]atom{{{kind: atomWeekday, n1: wd}}})
		return i + 1
	}
	// An abbreviated weekday is one only right in front of a date ("Wed 14 October", "lun.,
	// 14 ottobre"): several are ordinary words.
	if wd, ok := weekdayAbbr[w]; ok {
		j := i + 1
		if e.punctAt(j, ".") {
			j++
		}
		if e.punctAt(j, ",") {
			j++
		}
		if rs, next, ok := e.tryDate(j); ok {
			e.emit(ckDate, i, next, withWeekday(rs, wd))
			return next
		}
	}

	switch w {
	case "noon", "midday", "mezzogiorno":
		e.emit(ckTime, i, i+1, [][]atom{{timeAtom(12, 0, -1)}})
		return i + 1
	case "midnight", "mezzanotte":
		e.emit(ckTime, i, i+1, [][]atom{{timeAtom(0, 0, -1)}})
		return i + 1
	}

	// Number words.
	if v, next, ok := e.numberWordsAt(i); ok {
		p := numberPart([]string{canonDecimal(false, strconv.FormatInt(v, 10), "")})
		if v >= 0 && v <= 999999 {
			p.plainInt, p.digits = int(v), len(strconv.FormatInt(v, 10))
		}
		return e.compose(i, next, p, "", false)
	}

	// Completion claims ("booked", "prenotato", "successfully").
	for _, part := range strings.Split(w, "-") {
		if e.completionClaim(i, part) {
			e.emit(ckCompletion, i, i+1, nil)
			break
		}
	}
	return i + 1
}

// negatedBefore reports whether the completion word at toks[i] is negated in its clause: "was not
// booked", "nothing was saved", "non è stato prenotato". A negator reaches the word only across
// auxiliaries and adverbs; a quantifier negator ("no", "nothing", "nessun") also across up to two
// words of the noun phrase it quantifies ("no changes were saved").
func (e *extractor) negatedBefore(i int) bool {
	free := 0
	for k, steps := i-1, 0; k >= 0 && steps < 8; k, steps = k-1, steps+1 {
		t := e.toks[k]
		switch t.kind {
		case tokWord:
			w := t.low()
			if w == "t" && k >= 2 && e.punctAt(k-1, "'") && e.toks[k-2].kind == tokWord &&
				strings.HasSuffix(e.toks[k-2].low(), "n") {
				return true // n't
			}
			switch {
			case negators[w]:
				if quantifierNegators[w] {
					return free <= 2
				}
				return free == 0
			case w == "e" && strings.ContainsAny(t.text, "èÈ"), negationBridge[w] && w != "e":
				// auxiliary or adverb: keeps looking
			case clauseBreakers[w]:
				return false
			default:
				free++
				if free > 2 {
					return false
				}
			}
		case tokPunct:
			if t.text != "'" {
				return false
			}
		default:
			return false
		}
	}
	return false
}

var quantifierNegators = map[string]bool{
	"no": true, "nothing": true, "niente": true, "nulla": true, "nessun": true, "nessuno": true, "nessuna": true,
}

var clauseBreakers = map[string]bool{
	"and": true, "or": true, "but": true, "however": true, "while": true, "e": true, "ed": true,
	"o": true, "ma": true, "tuttavia": true, "mentre": true, "although": true, "though": true,
}
