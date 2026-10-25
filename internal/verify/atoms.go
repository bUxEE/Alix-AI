package verify

import (
	"strings"
	"time"
)

// An atom is one normalised statement a text can make: a number, a date, a time, an amount of
// money, an instant, an identifier, a weekday, a citation. The extractor reduces the response and
// every authorised string to atoms, so "14 ottobre 2026", "14/10/2026" and "2026-10-14" are the
// same atom and compare by value, never by spelling.
type atomKind uint8

const (
	atomNumber   atomKind = iota + 1 // s: canonical decimal
	atomDate                         // n1 year, n2 month, n3 day; 0 means "not stated"
	atomTime                         // n1 hour, n2 minute, n3 second (-1: not stated)
	atomMoney                        // s: canonical decimal amount, cur: ISO 4217 code
	atomInstant                      // s: canonical UTC RFC 3339 instant
	atomID                           // s: the identifier, exactly
	atomWeekday                      // n1: time.Weekday
	atomWdDate                       // a weekday and the date it qualifies: n1 weekday, n2 y, n3 m, n4 d
	atomCiteIdx                      // n1: 1-based position in the directive's sources
	atomCiteName                     // s: a cited source name, folded
	atomUnknown                      // a number the normaliser could not read: never allowed
)

type atom struct {
	kind           atomKind
	s              string
	cur            string
	n1, n2, n3, n4 int
}

func numberAtom(s string) atom { return atom{kind: atomNumber, s: s} }

func dateAtom(y, m, d int) atom { return atom{kind: atomDate, n1: y, n2: m, n3: d} }

func timeAtom(h, m, s int) atom { return atom{kind: atomTime, n1: h, n2: m, n3: s} }

func idAtom(s string) atom { return atom{kind: atomID, s: s} }

// A claim is a stretch of the response that states something checkable, with every reading the
// stretch can have. A stretch can have several: "10.30" is a decimal or a time, "1.234" is 1234
// or 1.234 depending on the language, "3/4" is a date or a fraction. A reading is a conjunction:
// all its atoms must be allowed ("04/05/2026" in English, where the day and month cannot be told
// apart, is two atoms and both dates must be allowed). A claim is acceptable when any one reading
// is.
type claimKind uint8

const (
	ckNumber claimKind = iota + 1
	ckDate
	ckTime
	ckMoney
	ckID
	ckWeekday
	ckCitation
	ckCompletion
)

type claim struct {
	kind     claimKind
	start    int
	end      int
	text     string
	readings [][]atom
}

// canonDecimal returns the canonical spelling of a decimal number given as sign, integer digits
// and fraction digits: no leading zeros, no trailing fraction zeros, no "-0". The input must be
// ASCII digits only; the empty integer part reads as zero.
func canonDecimal(neg bool, intPart, frac string) string {
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	frac = strings.TrimRight(frac, "0")
	out := intPart
	if frac != "" {
		out += "." + frac
	}
	if neg && out != "0" {
		out = "-" + out
	}
	return out
}

// shiftDecimal multiplies a canonical decimal by ten to the power zeros.
func shiftDecimal(n string, zeros int) string {
	neg := strings.HasPrefix(n, "-")
	ip, fp, _ := strings.Cut(strings.TrimPrefix(n, "-"), ".")
	fp += strings.Repeat("0", zeros)
	return canonDecimal(neg, ip+fp[:zeros], fp[zeros:])
}

// parseCanonical parses "-?digits(.digits)?" (the schema's decimalValue grammar, and the
// spelling of an integer or money value) into canonical form. ok is false for anything else.
func parseCanonical(s string) (string, bool) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	ip, fp, hasDot := strings.Cut(s, ".")
	if ip == "" || !allDigits(ip) || hasDot && (fp == "" || !allDigits(fp)) {
		return "", false
	}
	return canonDecimal(neg, ip, fp), true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigitByte(s[i]) {
			return false
		}
	}
	return true
}

func atoiSmall(s string) int {
	n := 0
	for i := 0; i < len(s) && i < 9; i++ {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

func isLeap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

// daysIn returns the number of days in month m of year y; y == 0 (no year stated) counts the
// month at its longest, so "29 February" is accepted without a year.
func daysIn(y, m int) int {
	switch m {
	case 4, 6, 9, 11:
		return 30
	case 2:
		if y == 0 || isLeap(y) {
			return 29
		}
		return 28
	}
	return 31
}

// validDate reports whether (y, m, d) is a real calendar date. y == 0 and d == 0 mean "not stated".
func validDate(y, m, d int) bool {
	if m < 1 || m > 12 {
		return false
	}
	if y != 0 && (y < 1000 || y > 2999) {
		return false
	}
	if d == 0 {
		return true
	}
	return d >= 1 && d <= daysIn(y, m)
}

// weekdayOf returns the weekday of a full calendar date.
func weekdayOf(y, m, d int) int {
	return int(time.Date(y, time.Month(m), d, 12, 0, 0, 0, time.UTC).Weekday())
}

// instantAtom parses an RFC 3339 instant with an explicit zone into its canonical UTC form.
func instantAtom(s string) (atom, bool) {
	s = strings.Replace(strings.Replace(s, "t", "T", 1), "z", "Z", 1)
	var t time.Time
	var err error
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999Z07:00", "2006-01-02T15:04:05.999999999Z0700",
		"2006-01-02T15:04Z07:00", "2006-01-02T15:04Z0700",
	} {
		if t, err = time.Parse(layout, s); err == nil {
			return atom{kind: atomInstant, s: t.UTC().Format("2006-01-02T15:04:05.999999999Z")}, true
		}
	}
	return atom{}, false
}
