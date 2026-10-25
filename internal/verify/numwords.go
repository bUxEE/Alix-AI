package verify

import "strings"

// Number words, English and Italian. A response that says "twenty-four hours" or "ventiquattro
// ore" states a number as surely as "24" does, so the extractor reads them too. The parsers are
// small and conservative: where a word is as likely to be an ordinary word as a number it is not
// read as one, and the response is checked on what it states with digits.
//
// Not read as numbers, on purpose: English "one" and Italian "un", "uno", "una" and "sei" on their
// own (a pronoun, an article, a verb: "one of the options", "un momento", "sei sicuro"); Italian
// "due" in a response that is not Italian ("due date"). They still count inside a compound
// ("twenty-one", "ventuno", "centosei").

var (
	enUnits = map[string]int64{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9}
	enTeens = map[string]int64{"ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
		"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19}
	enTens = map[string]int64{"twenty": 20, "thirty": 30, "forty": 40, "fifty": 50, "sixty": 60, "seventy": 70,
		"eighty": 80, "ninety": 90}
)

func isEnglishNumberWord(p string) bool {
	if p == "zero" || p == "hundred" || p == "thousand" || p == "million" {
		return true
	}
	if _, ok := enUnits[p]; ok {
		return true
	}
	if _, ok := enTeens[p]; ok {
		return true
	}
	_, ok := enTens[p]
	return ok
}

// parseEnglishNumber reads the longest run of number words at the start of parts and returns its
// value and how many parts it used (0 when parts does not start with a number word). A lone "one"
// is not a number.
func parseEnglishNumber(parts []string) (val int64, used int) {
	var total, group int64
	lastScale := int64(1) << 62
	i := 0
loop:
	for i < len(parts) {
		p := parts[i]
		switch {
		case p == "zero":
			if i != 0 || (len(parts) > 1 && isEnglishNumberWord(parts[1])) {
				break loop
			}
			return 0, 1
		case enTens[p] != 0:
			if group%100 != 0 {
				break loop
			}
			group += enTens[p]
		case enTeens[p] != 0:
			if group%100 != 0 {
				break loop
			}
			group += enTeens[p]
		case enUnits[p] != 0:
			r := group % 100
			if !(r == 0 || (r >= 20 && r%10 == 0)) {
				break loop
			}
			group += enUnits[p]
		case p == "hundred":
			if group >= 100 {
				break loop
			}
			if group == 0 {
				group = 1
			}
			group *= 100
		case p == "thousand" || p == "million":
			scale := int64(1000)
			if p == "million" {
				scale = 1_000_000
			}
			if scale >= lastScale || (group == 0 && total == 0 && i > 0) {
				break loop
			}
			if group == 0 {
				group = 1
			}
			total += group * scale
			group = 0
			lastScale = scale
		case p == "and":
			// "one hundred and twenty": the "and" only joins when a number word follows.
			if (group >= 100 || total > 0) && i+1 < len(parts) && isEnglishNumberWord(parts[i+1]) && parts[i+1] != "hundred" {
				i++
				continue
			}
			break loop
		default:
			break loop
		}
		i++
	}
	// Trim a dangling "and".
	for i > 0 && parts[i-1] == "and" {
		i--
	}
	if i == 0 {
		return 0, 0
	}
	if i == 1 && parts[0] == "one" {
		return 0, 0
	}
	return total + group, i
}

var (
	itUnits = []string{"", "uno", "due", "tre", "quattro", "cinque", "sei", "sette", "otto", "nove"}
	itTeens = []string{"dieci", "undici", "dodici", "tredici", "quattordici", "quindici", "sedici",
		"diciassette", "diciotto", "diciannove"}
	itTens = []string{"", "", "venti", "trenta", "quaranta", "cinquanta", "sessanta", "settanta", "ottanta", "novanta"}
	// itBelow100 maps every Italian word for 1..99 (accents stripped) to its value.
	itBelow100 = buildItalianBelow100()
)

func buildItalianBelow100() map[string]int64 {
	m := map[string]int64{}
	for i := 1; i <= 9; i++ {
		m[itUnits[i]] = int64(i)
	}
	for i, w := range itTeens {
		m[w] = int64(10 + i)
	}
	for t := 2; t <= 9; t++ {
		m[itTens[t]] = int64(t * 10)
		stem := strings.TrimSuffix(itTens[t], "a")
		stem = strings.TrimSuffix(stem, "i")
		for u := 1; u <= 9; u++ {
			s := itTens[t]
			if u == 1 || u == 8 { // venti+uno -> ventuno, venti+otto -> ventotto
				s = stem
			}
			m[s+itUnits[u]] = int64(t*10 + u)
		}
	}
	return m
}

// italianBelow1000 reads 1..999 ("cento", "duecentoventi", "centosei").
func italianBelow1000(w string) (int64, bool) {
	if w == "" {
		return 0, false
	}
	if v, ok := itBelow100[w]; ok {
		return v, true
	}
	idx := strings.Index(w, "cent")
	if idx < 0 {
		return 0, false
	}
	var hundreds int64 = 1
	if idx > 0 {
		h, ok := itBelow100[w[:idx]]
		if !ok || h < 2 || h > 9 {
			return 0, false
		}
		hundreds = h
	}
	rest := w[idx+4:]
	if rest == "o" {
		return hundreds * 100, true
	}
	// "centottanta" and "centotto" are cento with the final o elided before an o-word.
	if v, ok := itBelow100[rest]; ok {
		return hundreds*100 + v, true
	}
	if strings.HasPrefix(rest, "o") {
		if v, ok := itBelow100[rest[1:]]; ok {
			return hundreds*100 + v, true
		}
	}
	return 0, false
}

// parseItalianNumber reads one Italian number word, written as a single word as Italian does
// ("ventitré", "duemilacinquecento"). w must already be folded (accents stripped).
func parseItalianNumber(w string, allowDue bool) (int64, bool) {
	switch w {
	case "zero":
		return 0, true
	case "uno", "un", "una", "sei":
		return 0, false
	case "due":
		if !allowDue {
			return 0, false
		}
	}
	if w == "" || len(w) > 40 {
		return 0, false
	}
	for _, r := range w {
		if r < 'a' || r > 'z' {
			return 0, false
		}
	}
	if strings.HasPrefix(w, "milione") || strings.Contains(w, "milioni") {
		var left int64 = 1
		rest := ""
		if strings.HasPrefix(w, "milione") {
			rest = w[len("milione"):]
		} else {
			i := strings.Index(w, "milioni")
			l, ok := parseItalianNumber(w[:i], true)
			if !ok || l < 2 {
				return 0, false
			}
			left, rest = l, w[i+len("milioni"):]
		}
		rem, ok := italianBelow1e6(rest)
		if !ok {
			return 0, false
		}
		return left*1_000_000 + rem, true
	}
	return italianBelow1e6(w)
}

// italianBelow1e6 reads 1..999999, or "" as zero (the remainder of a larger number).
func italianBelow1e6(w string) (int64, bool) {
	if w == "" {
		return 0, true
	}
	if strings.HasPrefix(w, "mille") {
		rem, ok := italianBelow1000OrEmpty(w[len("mille"):])
		if !ok {
			return 0, false
		}
		return 1000 + rem, true
	}
	if i := strings.Index(w, "mila"); i > 0 {
		left, ok := italianBelow1000(w[:i])
		if !ok || left < 2 {
			return 0, false
		}
		rem, ok := italianBelow1000OrEmpty(w[i+4:])
		if !ok {
			return 0, false
		}
		return left*1000 + rem, true
	}
	return italianBelow1000(w)
}

func italianBelow1000OrEmpty(w string) (int64, bool) {
	if w == "" {
		return 0, true
	}
	return italianBelow1000(w)
}
