package verify

import "strings"

// authored is the text the core and the host wrote for this turn: fact values and displays,
// labels, messages, source claims and names. A response that quotes it verbatim is repeating what
// the host said, not making a claim of its own. That matters for completion: "Appointments can be
// cancelled free of charge" in a source, or "the appointment was created but the email was not
// sent" in a receipt's error, must not read as the response claiming the action was done.
type authored struct {
	phrases [][]string      // folded word sequences of authored strings of two or more words
	labels  map[string]bool // folded single-word field labels ("Created: ...")
	values  []statedValue   // host fact values that hold a completion word, with their labels
	seen    map[string]bool
}

// statedValue is a host fact as a response states it: "<label>: <value>". It is kept only for a
// value that holds a completion word or phrase, because that is the one case in which the way a
// value is stated decides whether it is data or a claim.
type statedValue struct {
	label []string // folded tokens of the field's label, punctuation included
	value []string // folded tokens of the value (its text, or its display form), punctuation included
}

func newAuthored() *authored {
	return &authored{labels: map[string]bool{}, seen: map[string]bool{}}
}

// foldedWords lists the folded words of s: every token that is not punctuation.
func foldedWords(s string) []string {
	var out []string
	for _, t := range scan(normalise(s), nil) {
		if t.kind != tokPunct && t.kind != tokSym {
			out = append(out, t.low())
		}
	}
	return out
}

// foldedTokens lists the folded text of every token of s, punctuation included, so that a
// sequence of them can be compared with the tokens of a response one for one.
func foldedTokens(s string) []string {
	ts := scan(normalise(s), nil)
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.low()
	}
	return out
}

func (a *authored) addPhraseWords(words []string) {
	if len(words) < 2 {
		return
	}
	if key := strings.Join(words, " "); !a.seen[key] {
		a.seen[key] = true
		a.phrases = append(a.phrases, words)
	}
}

func (a *authored) addLabelWords(words []string) {
	if len(words) == 1 {
		a.labels[words[0]] = true
		return
	}
	a.addPhraseWords(words)
}

// addStatedValue records that a host fact labelled label has the value value, if the value holds
// a completion word or phrase. Any other value needs no record: it can never be read as a claim.
func (a *authored) addStatedValue(label, value string) {
	if !holdsCompletion(value) {
		return
	}
	sv := statedValue{label: foldedTokens(label), value: foldedTokens(value)}
	if len(sv.label) == 0 || len(sv.value) == 0 {
		return
	}
	// The key cannot collide with a phrase key: it starts with a byte no folded word contains.
	if key := "\x00" + strings.Join(sv.label, " ") + "\x00" + strings.Join(sv.value, " "); !a.seen[key] {
		a.seen[key] = true
		a.values = append(a.values, sv)
	}
}

// holdsCompletion reports whether s contains a completion word or a completion phrase, whatever
// their context: a conservative filter, so a negated mention counts too.
func holdsCompletion(s string) bool {
	for _, t := range scan(normalise(s), nil) {
		if t.kind != tokWord {
			continue
		}
		for _, part := range strings.Split(t.low(), "-") {
			if completionWords[part] {
				return true
			}
		}
	}
	folded := collapse(s)
	for _, p := range completionPhrases {
		if containsPhrase(folded, p) {
			return true
		}
	}
	return false
}

// markAuthored flags the tokens of the response that are authored text and so are not the
// response's own claims: those inside a verbatim quotation of authored text, and those that are
// the value of a host fact in its "<label>: <value>" form.
func (e *extractor) markAuthored() {
	e.markQuotedPhrases()
	e.markStatedValues()
}

// markQuotedPhrases flags the tokens of the response that sit inside a verbatim quotation of
// authored text. Matching is on folded words, ignoring punctuation, so "start." and "start" agree.
func (e *extractor) markQuotedPhrases() {
	if e.author == nil || len(e.author.phrases) == 0 {
		return
	}
	var idx []int
	var words []string
	for i, t := range e.toks {
		if t.kind != tokPunct && t.kind != tokSym {
			idx = append(idx, i)
			words = append(words, t.low())
		}
	}
	for _, ph := range e.author.phrases {
		for s := 0; s+len(ph) <= len(words); s++ {
			if words[s] != ph[0] {
				continue
			}
			match := true
			for k := 1; k < len(ph); k++ {
				if words[s+k] != ph[k] {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			if e.inAuth == nil {
				e.inAuth = make([]bool, len(e.toks))
			}
			for k := 0; k < len(ph); k++ {
				e.inAuth[idx[s+k]] = true
			}
		}
	}
}

// markStatedValues flags the tokens of the response that are the value of a host fact the
// response states with the fact's own label: "Appointment status: cancelled". Such a word is the
// host's data, the way "Created: 14 October 2026" is, not the response saying that something was
// done. It is data only in exactly that form: the label of that fact, a colon, then the whole of
// that fact's value, ending its clause. The same word anywhere else ("Your appointment is
// cancelled"), under another fact's label, or inside a different value, is not, so a model cannot
// borrow it to claim completion.
func (e *extractor) markStatedValues() {
	if e.author == nil {
		return
	}
	for _, sv := range e.author.values {
		for s := 0; s < len(e.toks); s++ {
			if !e.tokensAre(s, sv.label) {
				continue
			}
			colon := s + len(sv.label)
			if !e.punctAt(colon, ":") || !e.tokensAre(colon+1, sv.value) {
				continue
			}
			from, end := colon+1, colon+1+len(sv.value)
			// The value is all of what follows the colon in its clause: a longer run of words is
			// a different value, and its extra words are the response's own.
			if end < len(e.toks) && !e.toks[end].nl && e.toks[end].kind != tokPunct {
				continue
			}
			if e.inValue == nil {
				e.inValue = make([]bool, len(e.toks))
			}
			for k := from; k < end; k++ {
				e.inValue[k] = true
			}
		}
	}
}

// tokensAre reports whether the tokens of the response from index at have exactly the folded
// text want.
func (e *extractor) tokensAre(at int, want []string) bool {
	if at < 0 || at+len(want) > len(e.toks) {
		return false
	}
	for k, w := range want {
		if e.toks[at+k].low() != w {
			return false
		}
	}
	return true
}

// phraseIsStatedData reports whether every occurrence of a completion phrase in the response lies
// wholly inside host fact values stated with their labels (see markStatedValues). folded is the
// response as collapse writes it, in which the phrase was found; when the occurrences found among
// the tokens cannot be reconciled with those in the folded text, the phrase is a claim.
func (e *extractor) phraseIsStatedData(phrase, folded string) bool {
	if e.inValue == nil {
		return false
	}
	pw := strings.Fields(phrase)
	var idx []int
	var words []string
	for i, t := range e.toks {
		if t.kind != tokPunct && t.kind != tokSym {
			idx = append(idx, i)
			words = append(words, t.low())
		}
	}
	found := 0
	for s := 0; s+len(pw) <= len(words); s++ {
		match := true
		for k, w := range pw {
			if words[s+k] != w {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		found++
		for k := range pw {
			if !e.inValue[idx[s+k]] {
				return false
			}
		}
	}
	return found > 0 && found == countPhrase(folded, phrase)
}

// completionClaim reports whether the completion word at toks[i] (part is its folded text, or one
// hyphen-separated part of it) is a claim that something was done.
func (e *extractor) completionClaim(i int, part string) bool {
	if !completionWords[part] || e.negatedBefore(i) {
		return false
	}
	if w, ok := e.wordAt(i - 1); ok && w == "being" {
		return false // "is being processed" says it is in progress
	}
	if e.inAuth != nil && e.inAuth[i] {
		return false
	}
	if e.inValue != nil && e.inValue[i] {
		return false // the value of a host fact, stated as that fact: data
	}
	if e.author != nil && e.author.labels[e.toks[i].low()] && e.punctAt(i+1, ":") {
		return false // a field label: "Created: 14 October 2026"
	}
	return true
}
