package verify

import (
	"strings"
	"sync"
	"time"

	// The verifier compares local wording against instants in IANA zones. A host (or a scratch
	// container, or a Windows service) may carry no zone database, so the database is embedded:
	// about 0.40 MB of binary, measured in docs/efficiency/M1-13.md.
	_ "time/tzdata"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

type factKind uint8

const (
	fkText factKind = iota + 1
	fkInteger
	fkDecimal
	fkBoolean
	fkDate
	fkTime
	fkDatetime
	fkMoney
	fkIdentifier
)

// typedFact is an actions.Fact decoded and checked: its value is of a known kind and well formed,
// and every spelling the verifier compares by is computed once.
type typedFact struct {
	field      string
	kind       factKind
	text       string // text value or identifier
	num        string // integer, decimal or money amount, canonical
	raw        string // the value as the host wrote it (money and decimal keep their digits)
	cur        string // money currency, upper case
	boolean    bool
	date       atom // fkDate
	tm         atom // fkTime
	inst       atom // fkDatetime: the instant
	instTime   time.Time
	loc        *time.Location // fkDatetime: the zone the host displays it in; UTC when it names none
	zoneNamed  bool
	localDate  atom // fkDatetime: the date in loc
	localTime  atom // fkDatetime: the time of day in loc
	display    string
	hasDisplay bool
}

func invalidf(format string, args ...any) error {
	return failure(contracts.ErrSchemaInvalid, ErrInvalidDirective, format, args...)
}

// unwrapValue returns the variant held by a CanonicalValue as a value, whether it was stored as a
// value or as a pointer. A nil or unknown variant is not usable.
func unwrapValue(v actions.CanonicalValueValue) (actions.CanonicalValueValue, bool) {
	switch x := v.(type) {
	case actions.TextValue, actions.IntegerValue, actions.DecimalValue, actions.BooleanValue, actions.DateValue,
		actions.TimeValue, actions.DatetimeValue, actions.MoneyValue, actions.IdentifierValue:
		return x, true
	case *actions.TextValue:
		if x != nil {
			return *x, true
		}
	case *actions.IntegerValue:
		if x != nil {
			return *x, true
		}
	case *actions.DecimalValue:
		if x != nil {
			return *x, true
		}
	case *actions.BooleanValue:
		if x != nil {
			return *x, true
		}
	case *actions.DateValue:
		if x != nil {
			return *x, true
		}
	case *actions.TimeValue:
		if x != nil {
			return *x, true
		}
	case *actions.DatetimeValue:
		if x != nil {
			return *x, true
		}
	case *actions.MoneyValue:
		if x != nil {
			return *x, true
		}
	case *actions.IdentifierValue:
		if x != nil {
			return *x, true
		}
	}
	return nil, false
}

func decodeFact(f actions.Fact) (typedFact, error) {
	tf := typedFact{field: string(f.Field)}
	if f.Field == "" {
		return tf, invalidf("a fact has no field name")
	}
	if f.Display != nil {
		tf.display, tf.hasDisplay = *f.Display, true
	}
	v, ok := unwrapValue(f.Value.Value)
	if !ok {
		return tf, invalidf("fact %q has no value of a known kind", f.Field)
	}
	switch x := v.(type) {
	case actions.TextValue:
		tf.kind, tf.text = fkText, x.Value
	case actions.IntegerValue:
		tf.kind = fkInteger
		tf.raw = formatInt(x.Value)
		tf.num, _ = parseCanonical(tf.raw)
	case actions.DecimalValue:
		n, ok := parseCanonical(x.Value)
		if !ok {
			return tf, invalidf("fact %q: %q is not a decimal", f.Field, x.Value)
		}
		tf.kind, tf.num, tf.raw = fkDecimal, n, x.Value
	case actions.BooleanValue:
		tf.kind, tf.boolean = fkBoolean, x.Value
	case actions.DateValue:
		d, err := time.Parse("2006-01-02", x.Value)
		if err != nil {
			return tf, invalidf("fact %q: %q is not a date", f.Field, x.Value)
		}
		tf.kind, tf.date = fkDate, dateAtom(d.Year(), int(d.Month()), d.Day())
	case actions.TimeValue:
		a, ok := parseTimeOfDay(x.Value)
		if !ok {
			return tf, invalidf("fact %q: %q is not a time", f.Field, x.Value)
		}
		tf.kind, tf.tm = fkTime, a
	case actions.DatetimeValue:
		t, err := time.Parse(time.RFC3339Nano, string(x.Value))
		if err != nil {
			return tf, invalidf("fact %q: %q is not an instant", f.Field, string(x.Value))
		}
		tf.kind, tf.instTime, tf.loc = fkDatetime, t.UTC(), time.UTC
		if x.Zone != nil {
			name := *x.Zone
			if name == "" || strings.EqualFold(name, "local") {
				return tf, invalidf("fact %q: %q is not an IANA zone", f.Field, name)
			}
			loc, err := loadZone(name)
			if err != nil {
				return tf, invalidf("fact %q: zone %q cannot be loaded", f.Field, name)
			}
			tf.loc, tf.zoneNamed = loc, true
		}
		tf.inst = atom{kind: atomInstant, s: t.UTC().Format("2006-01-02T15:04:05.999999999Z")}
		l := t.In(tf.loc)
		tf.localDate = dateAtom(l.Year(), int(l.Month()), l.Day())
		tf.localTime = timeAtom(l.Hour(), l.Minute(), l.Second())
	case actions.MoneyValue:
		n, ok := parseCanonical(x.Value)
		if !ok {
			return tf, invalidf("fact %q: %q is not an amount", f.Field, x.Value)
		}
		if len(x.Currency) != 3 || strings.ToUpper(x.Currency) != x.Currency {
			return tf, invalidf("fact %q: %q is not an ISO 4217 currency", f.Field, x.Currency)
		}
		tf.kind, tf.num, tf.raw, tf.cur = fkMoney, n, x.Value, x.Currency
	case actions.IdentifierValue:
		if x.Value == "" {
			return tf, invalidf("fact %q has an empty identifier", f.Field)
		}
		tf.kind, tf.text = fkIdentifier, string(x.Value)
	}
	return tf, nil
}

func formatInt(v int64) string {
	neg := v < 0
	u := uint64(v)
	if neg {
		u = -u
	}
	var b [24]byte
	n := len(b)
	for {
		n--
		b[n] = byte('0' + u%10)
		u /= 10
		if u == 0 {
			break
		}
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}

// parseTimeOfDay reads "HH:MM" or "HH:MM:SS".
func parseTimeOfDay(s string) (atom, bool) {
	p, ok := clockTime(s)
	if !ok {
		return atom{}, false
	}
	return p.readings[0][0], true
}

// statedForms lists the strings a response states a fact's value as: the host's display form, and
// for a text fact or an identifier its text. These are the host's own words, never numbers.
func (tf typedFact) statedForms() []string {
	var out []string
	if tf.hasDisplay && strings.TrimSpace(tf.display) != "" {
		out = append(out, tf.display)
	}
	if (tf.kind == fkText || tf.kind == fkIdentifier) && strings.TrimSpace(tf.text) != "" {
		out = append(out, tf.text)
	}
	return out
}

// atoms returns the atoms a fact makes allowed.
func (tf typedFact) atoms() []atom {
	switch tf.kind {
	case fkInteger, fkDecimal:
		return []atom{numberAtom(tf.num)}
	case fkMoney:
		return []atom{{kind: atomMoney, s: tf.num, cur: tf.cur}, numberAtom(tf.num)}
	case fkDate:
		return []atom{tf.date}
	case fkTime:
		return []atom{tf.tm}
	case fkDatetime:
		return []atom{tf.inst, tf.localDate, tf.localTime}
	case fkIdentifier:
		if allDigits(tf.text) {
			if n, ok := parseCanonical(tf.text); ok {
				return []atom{idAtom(tf.text), numberAtom(n)}
			}
		}
		return []atom{idAtom(tf.text)}
	}
	return nil
}

func timeEq(t, a atom) bool {
	if t.n1 != a.n1 || t.n2 != a.n2 {
		return false
	}
	ts, as := t.n3, a.n3
	if ts < 0 {
		ts = 0
	}
	if as < 0 {
		as = 0
	}
	return ts == as
}

// dateCovers reports whether a stated date t is no more specific than the allowed date a: every
// component t states is the same in a.
func dateCovers(a, t atom) bool {
	return (t.n1 == 0 || t.n1 == a.n1) && (t.n2 == 0 || t.n2 == a.n2) && (t.n3 == 0 || t.n3 == a.n3)
}

// matchesAtom reports whether a stated atom a is this fact. Weekday atoms are neutral: they are
// checked as claims of their own.
func (tf typedFact) matchesAtom(a atom) bool {
	switch a.kind {
	case atomWeekday, atomWdDate:
		return true
	}
	switch tf.kind {
	case fkInteger, fkDecimal:
		return a.kind == atomNumber && a.s == tf.num
	case fkMoney:
		return a.kind == atomMoney && a.s == tf.num && a.cur == tf.cur
	case fkDate:
		return a.kind == atomDate && a.n2 != 0 && a.n3 != 0 && dateCovers(tf.date, a)
	case fkTime:
		return a.kind == atomTime && timeEq(a, tf.tm)
	case fkDatetime:
		return a.kind == atomInstant && a.s == tf.inst.s
	case fkIdentifier:
		return a.kind == atomID && a.s == tf.text
	}
	return false
}

// presentIn reports whether the response states this fact. norm is the response normalised, words its
// folded words (punctuation ignored) and claims its extracted claims.
func (tf typedFact) presentIn(norm string, words []string, claims []claim) bool {
	if tf.hasDisplay && tf.display != "" && containsText(norm, words, tf.display) {
		return true
	}
	switch tf.kind {
	case fkText:
		return containsText(norm, words, tf.text)
	case fkIdentifier:
		return len(findKnown(norm, []string{tf.text})) > 0
	case fkBoolean:
		return booleanStated(words, tf.boolean)
	case fkDatetime:
		if claimsMatch(claims, tf) {
			return true
		}
		dateOK, timeOK := false, false
		for _, c := range claims {
			for _, r := range c.readings {
				var real []atom
				for _, a := range r {
					if a.kind != atomWeekday && a.kind != atomWdDate {
						real = append(real, a)
					}
				}
				if len(real) != 1 {
					continue
				}
				if a := real[0]; a.kind == atomDate && a.n2 != 0 && a.n3 != 0 && dateCovers(tf.localDate, a) {
					dateOK = true
				} else if a.kind == atomTime && timeEq(a, tf.localTime) {
					timeOK = true
				}
			}
		}
		return dateOK && timeOK
	}
	return claimsMatch(claims, tf)
}

// claimsMatch reports whether some claim has a reading made only of atoms of this fact (and
// neutral weekday atoms), with at least one of them real.
func claimsMatch(claims []claim, tf typedFact) bool {
	for _, c := range claims {
		for _, r := range c.readings {
			real := false
			ok := len(r) > 0
			for _, a := range r {
				if a.kind != atomWeekday && a.kind != atomWdDate {
					real = true
				}
				if !tf.matchesAtom(a) {
					ok = false
					break
				}
			}
			if ok && real {
				return true
			}
		}
	}
	return false
}

// booleanStated is the weakest of the presence checks: a boolean has no text of its own, so
// without a display form the response must carry a plain affirmation or denial. Hosts that need a
// precise check give the fact a display form.
func booleanStated(list []string, v bool) bool {
	words := map[string]bool{}
	for _, w := range list {
		words[w] = true
	}
	if v {
		return words["yes"] || words["true"] || words["si"] || words["vero"]
	}
	return words["no"] || words["not"] || words["false"] || words["non"] || words["falso"]
}

// containsPhrase reports whether needle occurs in hay as whole words (hay and needle already
// folded by collapse).
func containsPhrase(hay, needle string) bool {
	if needle == "" {
		return false
	}
	for from := 0; from <= len(hay)-len(needle); {
		k := strings.Index(hay[from:], needle)
		if k < 0 {
			return false
		}
		start := from + k
		end := start + len(needle)
		before := start == 0 || !isAlnum(lastRune(hay[:start])) || !isAlnum(firstRune(needle))
		after := end == len(hay) || !isAlnum(firstRune(hay[end:])) || !isAlnum(lastRune(needle))
		if before && after {
			return true
		}
		from = start + 1
	}
	return false
}

// countPhrase counts the occurrences of needle in hay as whole words, overlapping ones included,
// by the same rule as containsPhrase.
func countPhrase(hay, needle string) int {
	if needle == "" {
		return 0
	}
	n := 0
	for from := 0; from <= len(hay)-len(needle); {
		k := strings.Index(hay[from:], needle)
		if k < 0 {
			break
		}
		start := from + k
		end := start + len(needle)
		before := start == 0 || !isAlnum(lastRune(hay[:start])) || !isAlnum(firstRune(needle))
		after := end == len(hay) || !isAlnum(firstRune(hay[end:])) || !isAlnum(lastRune(needle))
		if before && after {
			n++
		}
		from = start + 1
	}
	return n
}

// containsText reports whether the response states the text: its folded words occur in order, as
// whole words, ignoring punctuation and case ("Dr Rossi" states "Dr. Rossi"). A text with no
// words in it (only punctuation) is matched literally.
func containsText(norm string, words []string, text string) bool {
	need := foldedWords(text)
	if len(need) == 0 {
		return strings.TrimSpace(text) != "" && strings.Contains(norm, strings.TrimSpace(normalise(text)))
	}
	return containsWords(words, need)
}

// containsWords reports whether need occurs in hay as a run of consecutive words.
func containsWords(hay, need []string) bool {
	for i := 0; i+len(need) <= len(hay); i++ {
		match := true
		for k, w := range need {
			if hay[i+k] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

var zoneCache sync.Map // zone name -> *time.Location

// loadZone loads an IANA zone once per name; parsing the zone database costs far more than a
// verification.
func loadZone(name string) (*time.Location, error) {
	if loc, ok := zoneCache.Load(name); ok {
		return loc.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	zoneCache.Store(name, loc)
	return loc, nil
}
