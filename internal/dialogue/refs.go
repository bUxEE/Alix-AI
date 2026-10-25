package dialogue

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/sessions"
	"github.com/buxee/alix-ai/internal/verify"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// Relative references (master plan 11.3, 22.3): "the second one", "that patient",
// "same time tomorrow". They are resolved from the session state by rules, never by
// the model: a model has no way to know which record the second one of a list was,
// and a value it guessed would be a fact it invented. A reference resolves only when
// the state names exactly one answer; otherwise it is left alone (the controller and
// the host's resolver handle the words as they always did) and nothing is guessed.
//
//   - an ordinal ("the second one", "number 3", "the last one", "il terzo") indexes the
//     choices the core just offered, else the records the latest deterministic result
//     presented, in the order it presented them;
//   - a demonstrative ("that patient", "that one", "quel paziente") is the one record of
//     the field's entity type the state holds (the active goal's, the latest result's,
//     a set-aside goal's, a completed goal's), and it is ambiguous, so unresolved, when
//     the latest result presented several;
//   - "same time <day>" is the time of day of the latest date-and-time the state holds,
//     on that day, in UTC (the engine's dates are UTC).
//
// A resolved entity keeps the data version the state recorded, so a later record
// revision invalidates it like any other resolved entity.

type refKind int

const (
	refOrdinal refKind = iota + 1
	refThat
	refSameTime
)

// relRef is one relative reference found in an utterance.
type relRef struct {
	kind refKind
	// ordinal is 1-based; -1 is the last.
	ordinal int
	// noun is the word after a demonstrative ("patient"); "" or one of the words
	// for "one" means any entity type.
	noun string
	// days is the offset from today of a same-time reference; weekday, when set,
	// replaces it with the next such weekday after today.
	days    int
	weekday *time.Weekday
}

var (
	ordWordsEN = map[string]int{
		"first": 1, "second": 2, "third": 3, "fourth": 4, "fifth": 5, "last": -1,
		"1st": 1, "2nd": 2, "3rd": 3, "4th": 4, "5th": 5,
	}
	ordWordsIT = map[string]int{
		"primo": 1, "prima": 1, "secondo": 2, "seconda": 2, "terzo": 3, "terza": 3, "quarto": 4, "quarta": 4,
		"quinto": 5, "quinta": 5, "ultimo": -1, "ultima": -1,
	}

	ordWordEN = regexp.MustCompile(`(?:^|\s)(?:(?:the|that|this)\s+)?(first|second|third|fourth|fifth|last|1st|2nd|3rd|4th|5th)(?:\s+(one|option|choice|result|match|item|entry))?(?:$|\s)`)
	ordNumEN  = regexp.MustCompile(`(?:^|\s)(?:number|option|choice|#)\s*(\d{1,2})(?:$|\s)`)
	ordWordIT = regexp.MustCompile(`(?:^|\s)(?:(?:il|lo|la|quel|quello|quella)\s+)?(primo|prima|secondo|seconda|terzo|terza|quarto|quarta|quinto|quinta|ultimo|ultima)(?:\s+(uno|una|opzione|scelta|risultato|elemento))?(?:$|\s)`)
	ordNumIT  = regexp.MustCompile(`(?:^|\s)(?:numero|opzione|scelta|n|#)\s*(\d{1,2})(?:$|\s)|(?:^|\s)(\d{1,2})[\x{00b0}\x{00ba}](?:$|\s)`)

	thatEN = regexp.MustCompile(`(?:^|\s)(?:that|this|the same|same|the previous|the latest)\s+([\p{L}][\p{L}'-]*)`)
	thatIT = regexp.MustCompile(`(?:^|\s)(?:quel|quello|quella|questo|questa|lo stesso|la stessa|stesso|stessa)\s+([\p{L}][\p{L}'-]*)`)

	sameTimeEN = regexp.MustCompile(`(?:^|\s)same (?:time|hour)(?:$|\s)`)
	sameTimeIT = regexp.MustCompile(`(?:^|\s)stess[ao] (?:ora|orario|momento)(?:$|\s)`)

	dayAfterEN = regexp.MustCompile(`(?:^|\s)day after tomorrow(?:$|\s)`)
	tomorrowEN = regexp.MustCompile(`(?:^|\s)tomorrow(?:$|\s)`)
	todayEN    = regexp.MustCompile(`(?:^|\s)today(?:$|\s)`)
	nextWeekEN = regexp.MustCompile(`(?:^|\s)next week(?:$|\s)`)
	inNEN      = regexp.MustCompile(`(?:^|\s)in (\d{1,3}) (day|days|week|weeks)(?:$|\s)`)
	weekdayEN  = regexp.MustCompile(`(?:^|\s)(?:next |on |this )?(monday|tuesday|wednesday|thursday|friday|saturday|sunday)(?:$|\s)`)

	dayAfterIT = regexp.MustCompile(`(?:^|\s)dopodomani(?:$|\s)`)
	tomorrowIT = regexp.MustCompile(`(?:^|\s)domani(?:$|\s)`)
	todayIT    = regexp.MustCompile(`(?:^|\s)oggi(?:$|\s)`)
	nextWeekIT = regexp.MustCompile(`(?:^|\s)(?:la )?(?:prossima settimana|settimana prossima)(?:$|\s)`)
	inNIT      = regexp.MustCompile(`(?:^|\s)(?:fra|tra) (\d{1,3}) (giorno|giorni|settimana|settimane)(?:$|\s)`)
	weekdayIT  = regexp.MustCompile(`(?:^|\s)(?:(?:il|di|prossimo) )?(luned[i\x{00ec}]|marted[i\x{00ec}]|mercoled[i\x{00ec}]|gioved[i\x{00ec}]|venerd[i\x{00ec}]|sabato|domenica)(?:$|\s)`)

	weekdays = map[string]time.Weekday{
		"monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday, "thursday": time.Thursday,
		"friday": time.Friday, "saturday": time.Saturday, "sunday": time.Sunday,
		"lunedi": time.Monday, "lunedì": time.Monday, "martedi": time.Tuesday, "martedì": time.Tuesday,
		"mercoledi": time.Wednesday, "mercoledì": time.Wednesday, "giovedi": time.Thursday, "giovedì": time.Thursday,
		"venerdi": time.Friday, "venerdì": time.Friday, "sabato": time.Saturday, "domenica": time.Sunday,
	}

	// oneWords are the nouns that stand for "any record": "that one".
	oneWords = toSet("one", "uno", "una", "ones")
)

// refFiller are the words an utterance may add around a reference and still be nothing
// but the reference, per language (a word of one language is not filler in the other:
// the Italian "secondo me" is an opinion, not the second one).
var (
	refFillerEN = toSet(
		"please", "ok", "okay", "actually", "i", "i'll", "i'd", "ill", "want", "would", "like", "take", "choose", "pick",
		"select", "use", "show", "me", "give", "go", "with", "the", "one", "it", "that", "this", "let's", "lets", "let",
		"make", "my", "choice", "option", "number", "is", "then", "and", "also", "again", "at", "on", "for", "do", "same",
		"time", "of", "to", "set", "change", "book", "schedule", "move", "um", "so", "yes", "no",
	)
	refFillerIT = toSet(
		"per", "favore", "voglio", "vorrei", "scelgo", "prendo", "il", "la", "lo", "l'", "quello", "quella", "mostrami",
		"dammi", "allora", "va", "bene", "uso", "metti", "fai", "alla", "stessa", "stesso", "ora", "orario", "di", "a", "un",
		"una", "sì", "si", "anzi", "invece", "cambia", "sposta", "prenota", "e", "poi", "anche", "mi", "ok", "opzione",
		"numero", "scelta", "vai",
	)
)

// parseRefs finds the relative references in an utterance. pure is true when the
// utterance is nothing but references (and filler words): then the words carry no
// other request and the references can be applied without a model.
func parseRefs(text string, lang verify.Language) (refs []relRef, pure bool) {
	p := phraseOf(text)
	if p == "" {
		return nil, false
	}
	var spans [][2]int // the byte ranges of p the references account for
	add := func(r relRef, loc []int) {
		refs = append(refs, r)
		spans = append(spans, [2]int{loc[0], loc[1]})
	}
	it := lang == verify.LangIT
	filler := refFillerEN
	if it {
		filler = refFillerIT
	}

	// "same time <day>"
	stRE, dayRE := sameTimeEN, dayRulesEN
	if it {
		stRE, dayRE = sameTimeIT, dayRulesIT
	}
	if loc := stRE.FindStringIndex(p); loc != nil {
		if r, dloc, ok := parseDay(p, dayRE); ok {
			add(r, loc)
			spans = append(spans, [2]int{dloc[0], dloc[1]})
		}
	}

	// ordinals
	ordRE, numRE, words := ordWordEN, ordNumEN, ordWordsEN
	if it {
		ordRE, numRE, words = ordWordIT, ordNumIT, ordWordsIT
	}
	if m := ordRE.FindStringSubmatchIndex(p); m != nil {
		word := p[m[2]:m[3]]
		suffix := m[4] >= 0
		// A bare ordinal word is an ordinal only when it is the whole utterance: "my first
		// name" and "secondo me" are not references.
		if suffix || onlyFillerAround(p, m[0], m[1], filler) {
			add(relRef{kind: refOrdinal, ordinal: words[word]}, m[:2])
		}
	}
	if m := numRE.FindStringSubmatchIndex(p); m != nil {
		d := ""
		for i := 2; i+1 < len(m); i += 2 {
			if m[i] >= 0 {
				d = p[m[i]:m[i+1]]
			}
		}
		if n, err := strconv.Atoi(d); err == nil && n >= 1 {
			add(relRef{kind: refOrdinal, ordinal: n}, m[:2])
		}
	}

	// demonstratives
	thatRE := thatEN
	if it {
		thatRE = thatIT
	}
	for _, m := range thatRE.FindAllStringSubmatchIndex(p, -1) {
		noun := p[m[2]:m[3]]
		if noun == "time" || noun == "hour" || noun == "ora" || noun == "orario" {
			continue // "same time" is the time reference
		}
		if _, isOrdinal := words[noun]; isOrdinal {
			continue // "quello ultimo" is the last one, not a record called "ultimo"
		}
		add(relRef{kind: refThat, noun: noun}, m[:2])
	}

	if len(refs) == 0 {
		return nil, false
	}
	return refs, onlyFillerOutside(p, spans, filler)
}

// onlyFillerAround reports whether the text outside [from,to) is only filler words.
func onlyFillerAround(p string, from, to int, filler map[string]bool) bool {
	return onlyFillerOutside(p, [][2]int{{from, to}}, filler)
}

func onlyFillerOutside(p string, spans [][2]int, filler map[string]bool) bool {
	covered := make([]bool, len(p))
	for _, s := range spans {
		for i := s[0]; i < s[1] && i < len(p); i++ {
			covered[i] = true
		}
	}
	var rest strings.Builder
	for i := 0; i < len(p); i++ {
		if covered[i] {
			rest.WriteByte(' ')
		} else {
			rest.WriteByte(p[i])
		}
	}
	for _, w := range strings.Fields(rest.String()) {
		if !filler[w] {
			return false
		}
	}
	return true
}

type dayRule struct {
	re   *regexp.Regexp
	days int
	// n marks a rule that reads a count and a unit from its groups.
	n bool
	// wd marks a weekday rule.
	wd bool
}

var (
	// order matters: "day after tomorrow" before "tomorrow".
	dayRulesEN = []dayRule{
		{re: dayAfterEN, days: 2}, {re: tomorrowEN, days: 1}, {re: nextWeekEN, days: 7}, {re: inNEN, n: true},
		{re: todayEN, days: 0}, {re: weekdayEN, wd: true},
	}
	dayRulesIT = []dayRule{
		{re: dayAfterIT, days: 2}, {re: tomorrowIT, days: 1}, {re: nextWeekIT, days: 7}, {re: inNIT, n: true},
		{re: todayIT, days: 0}, {re: weekdayIT, wd: true},
	}
)

// parseDay reads the day expression of a same-time reference.
func parseDay(p string, rules []dayRule) (relRef, []int, bool) {
	for _, r := range rules {
		m := r.re.FindStringSubmatchIndex(p)
		if m == nil {
			continue
		}
		ref := relRef{kind: refSameTime, days: r.days}
		switch {
		case r.n:
			n, err := strconv.Atoi(p[m[2]:m[3]])
			if err != nil {
				continue
			}
			unit := p[m[4]:m[5]]
			if strings.HasPrefix(unit, "week") || strings.HasPrefix(unit, "settiman") {
				n *= 7
			}
			ref.days = n
		case r.wd:
			wd, ok := weekdays[p[m[2]:m[3]]]
			if !ok {
				continue
			}
			ref.weekday = &wd
		}
		return ref, m[:2], true
	}
	return relRef{}, nil, false
}

// ---- resolution from the state ----

// refAssign is a value a reference resolved to, for one field.
type refAssign struct {
	field  string
	value  any
	origin sessions.SlotOrigin
	// entity is set when the value is a record the state already knew.
	entity *sessions.ResolvedEntity
}

// stateRefs resolves the relative references of this turn against the state and
// returns the values they give, by field. Two things are read: values the model put
// in a field that are themselves references ("the second one" as an item_id), and the
// user's own words. A field is assigned at most once and only when the state names
// exactly one answer; anything else is left to the rest of the pipeline.
func (t *turn) stateRefs(spec ap.Capability, visible []string, slots map[string]sessions.Slot, pending *sessions.Clarification, fields map[string]any) map[string]refAssign {
	out := map[string]refAssign{}
	vis := map[string]bool{}
	for _, f := range visible {
		vis[f] = true
	}
	defs := map[string]ap.CapabilityField{}
	for _, f := range spec.Fields {
		if vis[string(f.Name)] {
			defs[string(f.Name)] = f
		}
	}
	apply := func(field string, r relRef) {
		if _, done := out[field]; done {
			return
		}
		def := defs[field]
		if a, ok := t.resolveRef(r, def); ok {
			a.field = field
			out[field] = a
		}
	}
	// Values the model wrote that are references: for the field they were written in.
	names := make([]string, 0, len(fields))
	for n := range fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		s, ok := fields[name].(string)
		if _, known := defs[name]; !ok || !known {
			continue
		}
		if refs, pure := parseRefs(s, t.lang); pure && len(refs) == 1 {
			apply(name, refs[0])
		}
	}
	// The user's own words: each reference goes to the one field it can be for.
	refs, _ := parseRefs(t.userText, t.lang)
	for _, r := range refs {
		field, ok := t.refTarget(spec, defs, slots, pending, r)
		if ok {
			apply(field, r)
		}
	}
	return out
}

// refTarget picks the field a reference is for: the field the core is waiting for if
// it can take it, else the only field that can, else the only such field still empty.
func (t *turn) refTarget(spec ap.Capability, defs map[string]ap.CapabilityField, slots map[string]sessions.Slot, pending *sessions.Clarification, r relRef) (string, bool) {
	var cands []string
	for _, f := range spec.Fields {
		def, ok := defs[string(f.Name)]
		if !ok {
			continue
		}
		if t.fieldTakes(def, r) {
			cands = append(cands, string(f.Name))
		}
	}
	if pending != nil {
		for _, c := range cands {
			if c == pending.Field {
				return c, true
			}
		}
	}
	if len(cands) == 1 {
		return cands[0], true
	}
	var empty []string
	for _, c := range cands {
		if _, has := slots[c]; !has {
			empty = append(empty, c)
		}
	}
	if len(empty) == 1 {
		return empty[0], true
	}
	return "", false
}

// fieldTakes reports whether a reference can be a value of the field.
func (t *turn) fieldTakes(def ap.CapabilityField, r relRef) bool {
	switch r.kind {
	case refSameTime:
		return def.Type == ap.CapabilityFieldTypeDatetime
	case refOrdinal, refThat:
		if def.Type != ap.CapabilityFieldTypeEntityRef || def.EntityType == nil {
			return false
		}
		et := string(*def.EntityType)
		if r.kind == refThat && !t.nounIs(r.noun, et) {
			return false
		}
		if r.kind == refOrdinal {
			return len(t.entityPool(et, "")) > 0
		}
		return true
	}
	return false
}

// nounIs reports whether a noun names the entity type: its slug, or a label (the
// preferred word or a synonym) of the terminology term the type's slug names. "one"
// names any type.
func (t *turn) nounIs(noun, entityType string) bool {
	if oneWords[noun] {
		return true
	}
	norm := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, "_", " "), "-", " "))
	}
	if norm(noun) == norm(entityType) {
		return true
	}
	if t.pkg == nil {
		return false
	}
	for _, term := range t.pkg.Terminology() {
		if norm(string(term.ID)) != norm(entityType) {
			continue
		}
		for _, l := range term.Labels {
			if norm(string(l.Preferred)) == norm(noun) {
				return true
			}
			for _, syn := range l.Synonyms {
				if norm(string(syn)) == norm(noun) {
					return true
				}
			}
		}
	}
	return false
}

// resolveRef resolves one reference for one field from the state.
func (t *turn) resolveRef(r relRef, def ap.CapabilityField) (refAssign, bool) {
	field := string(def.Name)
	switch r.kind {
	case refSameTime:
		if def.Type != ap.CapabilityFieldTypeDatetime {
			return refAssign{}, false
		}
		v, ok := t.sameTimeValue(r, field)
		if !ok {
			return refAssign{}, false
		}
		return refAssign{value: v, origin: sessions.OriginUserTurn}, true
	case refOrdinal, refThat:
		if def.Type != ap.CapabilityFieldTypeEntityRef || def.EntityType == nil {
			return refAssign{}, false
		}
		et := string(*def.EntityType)
		var pick *sessions.ResolvedEntity
		if r.kind == refOrdinal {
			pool := t.entityPool(et, field)
			i := r.ordinal - 1
			if r.ordinal < 0 {
				i = len(pool) - 1
			}
			if i < 0 || i >= len(pool) {
				return refAssign{}, false // out of range: not guessed
			}
			pick = &pool[i]
		} else {
			if !t.nounIs(r.noun, et) {
				return refAssign{}, false
			}
			e, ok := t.thatEntity(et)
			if !ok {
				return refAssign{}, false
			}
			pick = &e
		}
		e := *pick
		e.Field = field
		return refAssign{value: e.EntityID, origin: sessions.OriginEntityResolution, entity: &e}, true
	}
	return refAssign{}, false
}

// entityPool is the list an ordinal indexes: the choices the core is offering for the
// field if there are some, else the records of the type the latest result that
// presented any of that type listed, in order.
func (t *turn) entityPool(entityType, field string) []sessions.ResolvedEntity {
	st := &t.sess.State
	if g := st.Goal; g != nil && g.Pending != nil && g.Pending.EntityType == entityType && (field == "" || g.Pending.Field == field) && len(g.Pending.Choices) > 0 {
		var out []sessions.ResolvedEntity
		for _, c := range g.Pending.Choices {
			out = append(out, sessions.ResolvedEntity{EntityType: entityType, EntityID: string(c.ChoiceID), Version: g.Pending.Version})
		}
		return out
	}
	for i := len(st.Results) - 1; i >= 0; i-- {
		var out []sessions.ResolvedEntity
		for _, it := range st.Results[i].Items {
			if it.EntityType == entityType {
				out = append(out, sessions.ResolvedEntity{EntityType: entityType, EntityID: it.EntityID, Version: it.Version})
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// thatEntity is the one record of the type the state holds, searched where the
// conversation is most likely to mean: the active goal, the latest result, the goals
// set aside, the goals completed. The latest result is ambiguous when it listed
// several records, and then nothing is chosen.
func (t *turn) thatEntity(entityType string) (sessions.ResolvedEntity, bool) {
	st := &t.sess.State
	last := func(es []sessions.ResolvedEntity) (sessions.ResolvedEntity, bool) {
		for i := len(es) - 1; i >= 0; i-- {
			if es[i].EntityType == entityType {
				return es[i], true
			}
		}
		return sessions.ResolvedEntity{}, false
	}
	if g := st.Goal; g != nil {
		if e, ok := last(g.Entities); ok {
			return e, true
		}
	}
	for i := len(st.Results) - 1; i >= 0; i-- {
		var found []sessions.ResolvedEntity
		seen := map[string]bool{}
		for _, it := range st.Results[i].Items {
			if it.EntityType == entityType && !seen[it.EntityID] {
				seen[it.EntityID] = true
				found = append(found, sessions.ResolvedEntity{EntityType: entityType, EntityID: it.EntityID, Version: it.Version})
			}
		}
		switch len(found) {
		case 0:
			continue
		case 1:
			return found[0], true
		}
		return sessions.ResolvedEntity{}, false // several were presented: "that one" is ambiguous
	}
	for i := len(st.Suspended) - 1; i >= 0; i-- {
		if e, ok := last(st.Suspended[i].Entities); ok {
			return e, true
		}
	}
	for i := len(st.Capsules) - 1; i >= 0; i-- {
		if e, ok := last(st.Capsules[i].Entities); ok {
			return e, true
		}
	}
	return sessions.ResolvedEntity{}, false
}

// sameTimeValue is the date and time a same-time reference stands for: the time of day
// of the latest date-and-time the state holds (the field itself first), on the day
// the reference names, in UTC.
func (t *turn) sameTimeValue(r relRef, field string) (string, bool) {
	tod, ok := t.timeOfDay(field)
	if !ok {
		return "", false
	}
	today := t.now.UTC()
	day := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	if r.weekday != nil {
		d := (int(*r.weekday) - int(day.Weekday()) + 7) % 7
		if d == 0 {
			d = 7
		}
		day = day.AddDate(0, 0, d)
	} else {
		day = day.AddDate(0, 0, r.days)
	}
	return day.Add(tod).Format("2006-01-02T15:04:05Z"), true
}

// timeOfDay finds the time of day of the latest date-and-time in the state: the
// active goal's, then the goals set aside, then the completed goals' capsules; in each
// the field's own value before any other.
func (t *turn) timeOfDay(field string) (time.Duration, bool) {
	st := &t.sess.State
	fromSlots := func(g *sessions.Goal) (time.Duration, bool) {
		if g == nil {
			return 0, false
		}
		if sl, ok := g.Slots[field]; ok {
			if d, ok := todOf(sl.Value); ok {
				return d, true
			}
		}
		names := make([]string, 0, len(g.Slots))
		for n := range g.Slots {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if sl := g.Slots[n]; sl.Type == "datetime" || sl.Type == "" {
				if d, ok := todOf(sl.Value); ok {
					return d, true
				}
			}
		}
		return 0, false
	}
	if d, ok := fromSlots(st.Goal); ok {
		return d, true
	}
	for i := len(st.Suspended) - 1; i >= 0; i-- {
		if d, ok := fromSlots(st.Suspended[i]); ok {
			return d, true
		}
	}
	for i := len(st.Capsules) - 1; i >= 0; i-- {
		c := st.Capsules[i]
		if v, ok := c.Facts[field]; ok {
			if d, ok := todOf(v); ok {
				return d, true
			}
		}
		names := make([]string, 0, len(c.Facts))
		for n := range c.Facts {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if d, ok := todOf(c.Facts[n]); ok {
				return d, true
			}
		}
	}
	return 0, false
}

// todOf reads the time of day of an RFC 3339 value.
func todOf(v any) (time.Duration, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, false
	}
	tm = tm.UTC()
	return time.Duration(tm.Hour())*time.Hour + time.Duration(tm.Minute())*time.Minute + time.Duration(tm.Second())*time.Second, true
}
