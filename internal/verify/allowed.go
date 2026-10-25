package verify

import (
	"sort"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// allowedSet is everything a response may state: the atoms of the directive's facts, of the
// receipt's facts and records, and of every other string the core or the host authored for this
// turn (labels, choices, suggestions, source names and claims, messages). A response is checked
// against it; whatever it states that is not in the set is invented.
type allowedSet struct {
	lang     Language
	numbers  map[string]bool
	years    map[int]bool
	dates    []atom
	times    []atom
	money    map[[2]string]bool
	instants map[string]bool
	ids      map[string]bool
	wdDates  []atom
	weekdays map[int]bool
	currs    map[string]bool
	author   *authored
	// sources are the directive's citable sources: those not marked removed.
	sources []conversation.Citation
	citeIDs map[string]bool
}

func newAllowedSet(lang Language) *allowedSet {
	return &allowedSet{
		lang: lang, numbers: map[string]bool{}, years: map[int]bool{}, money: map[[2]string]bool{},
		instants: map[string]bool{}, ids: map[string]bool{}, weekdays: map[int]bool{},
		currs: map[string]bool{}, citeIDs: map[string]bool{}, author: newAuthored(),
	}
}

// addString allows everything the string states: its numbers, dates, times, amounts, identifiers
// and weekdays, read in the response's language.
func (a *allowedSet) addString(s string) {
	a.author.addPhraseWords(a.scanString(s))
}

// addStatedValue records a host fact, as the response states it, whose value holds a completion
// word: the label of the field and the value. The response may state it in that form without
// claiming completion (see markStatedValues).
func (a *allowedSet) addStatedValue(label, value string) {
	a.author.addStatedValue(label, value)
}

// noteCurrencies records the currency of a money value, so that every host string of the turn is
// read with it known whatever the order the strings and the facts come in. A code the lexicon does
// not list (KWD) is a currency to the response check because a money fact carries it; a string
// scanned without it read "25.000 KWD" as the number 25000 and the response as the amount 25000
// KWD (task M3-40).
func (a *allowedSet) noteCurrencies(tf typedFact) {
	if tf.kind == fkMoney {
		a.currs[tf.cur] = true
	}
}

// noteCanonicalCurrency is noteCurrencies for a canonical value that is not a named fact (a
// choice's value).
func (a *allowedSet) noteCanonicalCurrency(v actions.CanonicalValue) {
	if tf, err := decodeFact(actions.Fact{Field: "value", Value: v}); err == nil {
		a.noteCurrencies(tf)
	}
}

// addLabel allows a field label: its words, and a single-word label as a label.
func (a *allowedSet) addLabel(s string) {
	a.author.addLabelWords(a.scanString(s))
}

// scanString allows everything s states and returns its folded words.
func (a *allowedSet) scanString(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	// A host string is read as the response is: the currencies the turn's amounts are in are known to
	// both (noteCurrencies), or the same characters would be a bare number here and an amount there.
	e := newExtractor(normalise(s), a.lang, nil, a.currs, nil)
	e.run()
	for _, c := range e.out {
		switch c.kind {
		case ckCitation, ckCompletion:
			continue
		}
		for _, r := range c.readings {
			for _, at := range r {
				a.addAtom(at)
			}
		}
	}
	return e.words()
}

func (a *allowedSet) addID(id string) {
	if id != "" {
		a.ids[id] = true
		if allDigits(id) {
			if n, ok := parseCanonical(id); ok {
				a.numbers[n] = true
			}
		}
	}
}

func (a *allowedSet) addAtom(at atom) {
	switch at.kind {
	case atomNumber:
		a.numbers[at.s] = true
	case atomDate:
		a.dates = append(a.dates, at)
		if at.n1 != 0 {
			a.years[at.n1] = true
		}
	case atomTime:
		a.times = append(a.times, at)
	case atomMoney:
		a.money[[2]string{at.s, at.cur}] = true
		a.numbers[at.s] = true
		a.currs[at.cur] = true
	case atomInstant:
		a.instants[at.s] = true
	case atomID:
		a.addID(at.s)
	case atomWeekday:
		a.weekdays[at.n1] = true
	case atomWdDate:
		a.wdDates = append(a.wdDates, at)
		a.weekdays[at.n1] = true
	}
}

// addFact allows a decoded fact: its typed atoms, its text and its display form.
func (a *allowedSet) addFact(tf typedFact) {
	for _, at := range tf.atoms() {
		a.addAtom(at)
	}
	a.addString(tf.field)
	if tf.kind == fkText {
		a.addString(tf.text)
	}
	if tf.hasDisplay {
		a.addString(tf.display)
	}
}

// addCanonical allows a canonical value that is not a named fact (a choice's value).
func (a *allowedSet) addCanonical(v actions.CanonicalValue) {
	if tf, err := decodeFact(actions.Fact{Field: "value", Value: v}); err == nil {
		for _, at := range tf.atoms() {
			a.addAtom(at)
		}
		if tf.kind == fkText {
			a.addString(tf.text)
		}
	}
}

func (a *allowedSet) addChoices(cs []actions.Choice) {
	for _, c := range cs {
		a.addID(string(c.ChoiceID))
		a.addString(c.Label)
		if c.Description != nil {
			a.addString(*c.Description)
		}
		a.addCanonical(c.Value)
	}
}

func (a *allowedSet) addSource(c conversation.Citation) {
	a.sources = append(a.sources, c)
	a.citeIDs[string(c.SourceID)] = true
	a.addID(string(c.SourceID))
	a.addString(c.DisplayName)
	a.addString(string(c.Version))
	for _, claim := range c.Claims {
		a.addString(claim)
	}
	if loc := c.Locator; loc != nil {
		if loc.Page != nil {
			a.addAtom(numberAtom(canonDecimal(false, strconv.FormatInt(*loc.Page, 10), "")))
		}
		if loc.Section != nil {
			a.addString(*loc.Section)
		}
		if loc.Record != nil {
			a.addID(string(*loc.Record))
		}
		if loc.Span != nil {
			// A character span is not something a response states; its offsets stay disallowed.
			_ = loc.Span
		}
	}
}

// has reports whether a stated atom is allowed.
func (a *allowedSet) has(t atom) bool {
	switch t.kind {
	case atomNumber:
		if a.numbers[t.s] {
			return true
		}
		// A year the directive carries as part of a date may be stated on its own.
		if len(t.s) == 4 && allDigits(t.s) && a.years[atoiSmall(t.s)] {
			return true
		}
		return false
	case atomDate:
		for _, d := range a.dates {
			if dateCovers(d, t) {
				return true
			}
		}
		return false
	case atomTime:
		for _, x := range a.times {
			if timeEq(t, x) {
				return true
			}
		}
		return false
	case atomMoney:
		return a.money[[2]string{t.s, t.cur}]
	case atomInstant:
		return a.instants[t.s]
	case atomID:
		return a.ids[t.s]
	case atomWeekday:
		if a.weekdays[t.n1] {
			return true
		}
		for _, d := range a.dates {
			if d.n1 != 0 && d.n2 != 0 && d.n3 != 0 && weekdayOf(d.n1, d.n2, d.n3) == t.n1 {
				return true
			}
		}
		return false
	case atomWdDate:
		for _, d := range a.dates {
			if d.n1 != 0 && d.n2 != 0 && d.n3 != 0 && t.n3 == d.n2 && t.n4 == d.n3 &&
				(t.n2 == 0 || t.n2 == d.n1) && weekdayOf(d.n1, d.n2, d.n3) == t.n1 {
				return true
			}
		}
		for _, w := range a.wdDates {
			if w.n1 == t.n1 && w.n3 == t.n3 && w.n4 == t.n4 && (t.n2 == 0 || t.n2 == w.n2) {
				return true
			}
		}
		return false
	}
	return false
}

// idList returns the allowed identifiers, longest first, for findKnown. A bare number ("3" as a
// choice ID) is left out: it is read as the number it is, which addID allows, so it cannot hide the
// same digits in a date, a time or an amount ("3 October", "3 EUR", "3 pm"). An identifier that
// holds a completion word ("cancelled" as a status code) is left out too: it is read as words, so
// the completion check sees it, and stated as its fact it is data (statedForms).
func (a *allowedSet) idList() []string {
	out := make([]string, 0, len(a.ids))
	for id := range a.ids {
		if allDigits(id) || holdsCompletion(id) {
			continue
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}
