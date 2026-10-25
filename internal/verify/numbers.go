package verify

import "strings"

// part is one thing a numeric blob states: a number, a date or a time, with every reading it can
// have. A blob like "10-12" or "10:30-11:00" is several parts.
type part struct {
	readings [][]atom
	// nums are the canonical decimal readings when the part is a number; money composition and
	// ordinal handling work from them. Empty for a date or a time.
	nums []string
	// plainInt is the value when the blob is digits only and short, else -1.
	plainInt int
	digits   int
	isNumber bool
}

func (p part) isPlainInt() bool { return p.plainInt >= 0 }

func numberPart(nums []string, extra ...[]atom) part {
	rs := make([][]atom, 0, len(nums)+len(extra))
	for _, n := range nums {
		rs = append(rs, []atom{numberAtom(n)})
	}
	rs = append(rs, extra...)
	return part{readings: rs, nums: nums, plainInt: -1, isNumber: true}
}

func (l Language) decimalSep() string {
	if l == LangIT {
		return ","
	}
	return "."
}

// classifyBlob reads a numeric blob (digits joined by . , : / - ' or no-break spaces) into the
// parts it states. Numbers, dates and times are read in the conventions of lang; whatever is
// ambiguous keeps every reading.
func classifyBlob(b string, lang Language) []part {
	if strings.Contains(b, "-") {
		return classifyDashed(b, lang)
	}
	if strings.Contains(b, "/") {
		return classifySlashed(b, lang)
	}
	if strings.Contains(b, ":") {
		if p, ok := clockTime(b); ok {
			return []part{p}
		}
		return splitParts(b, ":", lang)
	}
	return []part{classifyGrouped(b, lang)}
}

// splitParts classifies each sep-separated piece of b on its own.
func splitParts(b, sep string, lang Language) []part {
	var out []part
	for _, piece := range strings.Split(b, sep) {
		if piece == "" {
			continue
		}
		out = append(out, classifyBlob(piece, lang)...)
	}
	return out
}

func classifyDashed(b string, lang Language) []part {
	pieces := strings.Split(b, "-")
	switch len(pieces) {
	case 3:
		if allDigits(pieces[0]) && allDigits(pieces[1]) && allDigits(pieces[2]) {
			switch {
			case len(pieces[0]) == 4 && len(pieces[1]) <= 2 && len(pieces[2]) <= 2:
				if rs := ymdReadings(pieces[0], pieces[1], pieces[2]); rs != nil {
					return []part{{readings: rs, plainInt: -1}}
				}
			case len(pieces[0]) <= 2 && len(pieces[1]) <= 2 && (len(pieces[2]) == 4 || len(pieces[2]) == 2):
				if rs := dmyReadings(pieces[0], pieces[1], pieces[2], lang); rs != nil {
					return []part{{readings: rs, plainInt: -1}}
				}
			}
		}
	case 2:
		if allDigits(pieces[0]) && allDigits(pieces[1]) && len(pieces[0]) == 4 && len(pieces[1]) <= 2 {
			y, m := atoiSmall(pieces[0]), atoiSmall(pieces[1])
			if validDate(y, m, 0) {
				// "2026-10": a year and a month, or two numbers.
				p := numberPart(nil, []atom{dateAtom(y, m, 0)}, []atom{numberAtom(canonDecimal(false, pieces[0], "")), numberAtom(canonDecimal(false, pieces[1], ""))})
				return []part{p}
			}
		}
	}
	return splitParts(b, "-", lang)
}

func classifySlashed(b string, lang Language) []part {
	pieces := strings.Split(b, "/")
	for _, p := range pieces {
		if !allDigits(p) {
			return splitParts(b, "/", lang)
		}
	}
	switch len(pieces) {
	case 3:
		var rs [][]atom
		if len(pieces[0]) == 4 {
			rs = ymdReadings(pieces[0], pieces[1], pieces[2])
		} else if len(pieces[0]) <= 2 && len(pieces[1]) <= 2 && (len(pieces[2]) == 4 || len(pieces[2]) == 2) {
			rs = dmyReadings(pieces[0], pieces[1], pieces[2], lang)
		}
		if rs != nil {
			return []part{{readings: rs, plainInt: -1}}
		}
	case 2:
		if len(pieces[0]) <= 2 && len(pieces[1]) <= 2 {
			rs := dmReadings(pieces[0], pieces[1], lang)
			// "3/4" is also a fraction or a ratio: two numbers.
			frac := []atom{numberAtom(canonDecimal(false, pieces[0], "")), numberAtom(canonDecimal(false, pieces[1], ""))}
			return []part{{readings: append(rs, frac), plainInt: -1}}
		}
	}
	return splitParts(b, "/", lang)
}

// clockTime reads "h:mm" or "hh:mm:ss".
func clockTime(b string) (part, bool) {
	pieces := strings.Split(b, ":")
	if len(pieces) < 2 || len(pieces) > 3 {
		return part{}, false
	}
	if len(pieces[0]) > 2 || len(pieces[1]) != 2 || (len(pieces) == 3 && len(pieces[2]) != 2) {
		return part{}, false
	}
	for _, p := range pieces {
		if !allDigits(p) {
			return part{}, false
		}
	}
	h, m, s := atoiSmall(pieces[0]), atoiSmall(pieces[1]), -1
	if len(pieces) == 3 {
		s = atoiSmall(pieces[2])
	}
	if h > 23 || m > 59 || s > 59 {
		return part{}, false
	}
	return part{readings: [][]atom{{timeAtom(h, m, s)}}, plainInt: -1}, true
}

func ymdReadings(ys, ms, ds string) [][]atom {
	y, m, d := atoiSmall(ys), atoiSmall(ms), atoiSmall(ds)
	if !validDate(y, m, d) || d == 0 {
		return nil
	}
	return [][]atom{{dateAtom(y, m, d)}}
}

func year2or4(s string) int {
	y := atoiSmall(s)
	if len(s) == 2 {
		if y < 70 {
			return 2000 + y
		}
		return 1900 + y
	}
	return y
}

// dmyReadings reads a day, month, year triple in the order of the language. Italian is day first.
// English is ambiguous when the first two numbers are both 12 or less and differ: the reading
// then has two atoms and both dates must be allowed, so a date that is only right in the other
// convention is never accepted.
func dmyReadings(as, bs, cs string, lang Language) [][]atom {
	y := year2or4(cs)
	return dayMonthReadings(atoiSmall(as), atoiSmall(bs), y, lang)
}

func dmReadings(as, bs string, lang Language) [][]atom {
	return dayMonthReadings(atoiSmall(as), atoiSmall(bs), 0, lang)
}

func dayMonthReadings(a, b, y int, lang Language) [][]atom {
	dmy, dmyOK := dateAtom(y, b, a), validDate(y, b, a) && a >= 1
	mdy, mdyOK := dateAtom(y, a, b), validDate(y, a, b) && b >= 1
	switch {
	case lang == LangIT:
		if dmyOK {
			return [][]atom{{dmy}}
		}
	case dmyOK && mdyOK && dmy != mdy:
		return [][]atom{{dmy, mdy}}
	case dmyOK:
		return [][]atom{{dmy}}
	case mdyOK:
		return [][]atom{{mdy}}
	}
	return nil
}

// classifyGrouped reads digits with optional . , ' or no-break-space separators as one number.
func classifyGrouped(b string, lang Language) part {
	if allDigits(b) {
		p := numberPart([]string{canonDecimal(false, b, "")})
		if len(b) <= 9 {
			p.plainInt, p.digits = atoiSmall(b), len(b)
		}
		return p
	}
	groups, seps := splitGroups(b)
	if n, ok := parseGroups(groups, seps, lang); ok {
		p := numberPart([]string{n})
		// "10.30" is a decimal or a time.
		if len(seps) == 1 && seps[0] == "." && len(groups[0]) <= 2 && len(groups[1]) == 2 {
			if h, m := atoiSmall(groups[0]), atoiSmall(groups[1]); h <= 23 && m <= 59 {
				p.readings = append(p.readings, []atom{timeAtom(h, m, -1)})
			}
		}
		// "14.10.2026" is a date, not a number.
		return p
	}
	// "14.10.2026": day.month.year.
	if len(seps) == 2 && seps[0] == "." && seps[1] == "." && len(groups[0]) <= 2 && len(groups[1]) <= 2 &&
		(len(groups[2]) == 4 || len(groups[2]) == 2) {
		if rs := dmyReadings(groups[0], groups[1], groups[2], LangIT); rs != nil {
			return part{readings: rs, plainInt: -1}
		}
	}
	// Invalid grouping: every group is a number of its own.
	nums := make([]string, 0, len(groups))
	rs := make([][]atom, 0, 1)
	var conj []atom
	for _, g := range groups {
		n := canonDecimal(false, g, "")
		nums = append(nums, n)
		conj = append(conj, numberAtom(n))
	}
	rs = append(rs, conj)
	return part{readings: rs, nums: nums, plainInt: -1, isNumber: true}
}

func splitGroups(b string) (groups, seps []string) {
	var cur strings.Builder
	for i := 0; i < len(b); {
		if isDigitByte(b[i]) {
			cur.WriteByte(b[i])
			i++
			continue
		}
		n, _ := separatorAt(b, i)
		groups = append(groups, cur.String())
		cur.Reset()
		seps = append(seps, b[i:i+n])
		i += n
	}
	groups = append(groups, cur.String())
	return groups, seps
}

// validGrouping reports whether groups are a number's thousands groups: a first group of one to
// three digits that does not start with 0, then groups of three.
func validGrouping(groups []string) bool {
	if len(groups[0]) < 1 || len(groups[0]) > 3 || groups[0][0] == '0' {
		return false
	}
	for _, g := range groups[1:] {
		if len(g) != 3 {
			return false
		}
	}
	return true
}

// parseGroups reads groups joined by seps as a single number in the conventions of lang. ok is
// false when the grouping is not one a number can have.
func parseGroups(groups, seps []string, lang Language) (string, bool) {
	dec := lang.decimalSep()
	same := true
	for _, s := range seps[1:] {
		if s != seps[0] {
			same = false
		}
	}
	switch {
	case len(seps) == 1:
		sep, g1, g2 := seps[0], groups[0], groups[1]
		switch {
		case sep == ":" || sep == "/" || sep == "-":
			return "", false
		case sep == "'" || sep == nbspStr:
			if len(g2) == 3 && len(g1) <= 3 {
				return canonDecimal(false, g1+g2, ""), true
			}
			return "", false
		case sep == dec:
			return canonDecimal(false, g1, g2), true
		default: // the other of . and , is this language's grouping character
			if len(g2) == 3 && len(g1) <= 3 && g1[0] != '0' {
				return canonDecimal(false, g1+g2, ""), true
			}
			return canonDecimal(false, g1, g2), true // lenient: "12,5" read as a decimal
		}
	case same:
		if seps[0] == "." || seps[0] == "," || seps[0] == "'" || seps[0] == nbspStr {
			if validGrouping(groups) {
				return canonDecimal(false, strings.Join(groups, ""), ""), true
			}
		}
		return "", false
	default:
		last := seps[len(seps)-1]
		if last != "." && last != "," {
			return "", false
		}
		for _, s := range seps[:len(seps)-1] {
			if s == last || s == ":" || s == "/" || s == "-" {
				return "", false
			}
		}
		head := groups[:len(groups)-1]
		if !validGrouping(head) {
			return "", false
		}
		return canonDecimal(false, strings.Join(head, ""), groups[len(groups)-1]), true
	}
}
