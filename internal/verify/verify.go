package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// verification holds a directive (and the receipt it names) prepared for checking or rendering:
// validated, its facts decoded, and the set of everything a response may state computed.
type verification struct {
	d       *conversation.DialogueDirective
	r       *actions.ActionReceipt
	opts    Options
	lang    Language
	facts   []typedFact
	rfacts  []typedFact
	must    map[string]bool
	allowed *allowedSet
	// citable[i] reports whether d.Sources[i] may be cited.
	citable []bool
}

// prepare validates the inputs and builds the allowed set. Any problem with the directive or the
// receipt is an error and never a verdict: an inconsistent directive cannot verify anything.
func prepare(d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options) (*verification, error) {
	if d == nil {
		return nil, invalidf("directive is nil")
	}
	switch opts.Language {
	case LangEN, LangIT:
	default:
		return nil, failure(contracts.ErrValidationFailed, ErrUnsupportedLanguage, "language %q is not supported (supported: en, it)", string(opts.Language))
	}
	if !d.Act.Valid() {
		return nil, invalidf("act %q is not a dialogue act", string(d.Act))
	}
	v := &verification{d: d, r: r, opts: opts, lang: opts.Language, must: map[string]bool{}}
	byField := map[string]bool{}
	for _, f := range d.Facts {
		tf, err := decodeFact(f)
		if err != nil {
			return nil, err
		}
		v.facts = append(v.facts, tf)
		byField[tf.field] = true
	}
	for _, name := range d.MustState {
		if !byField[string(name)] {
			return nil, invalidf("must_state names %q, which no fact carries", string(name))
		}
		v.must[string(name)] = true
	}
	if r != nil {
		if !r.Status.Valid() {
			return nil, invalidf("receipt status %q is not valid", string(r.Status))
		}
		if d.ReceiptID == nil || *d.ReceiptID != r.ReceiptID {
			return nil, failure(contracts.ErrInternal, ErrReceiptMismatch, "the directive does not name this receipt")
		}
		for _, f := range r.Facts {
			tf, err := decodeFact(f)
			if err != nil {
				return nil, err
			}
			v.rfacts = append(v.rfacts, tf)
		}
	}
	v.allowed = v.buildAllowed()
	return v, nil
}

func (v *verification) label(field string) string {
	return fieldLabel(v.opts, field)
}

// buildAllowed collects everything this turn's response may state.
func (v *verification) buildAllowed() *allowedSet {
	a := newAllowedSet(v.lang)
	d, r := v.d, v.r
	// The currencies of the turn's amounts come first: every string below is read with them known,
	// wherever it stands among the facts.
	for _, tf := range v.facts {
		a.noteCurrencies(tf)
	}
	for _, tf := range v.rfacts {
		a.noteCurrencies(tf)
	}
	if q := d.NextQuestion; q != nil {
		for _, c := range q.Choices {
			a.noteCanonicalCurrency(c.Value)
		}
	}
	for _, c := range d.AllowedSuggestions {
		a.noteCanonicalCurrency(c.Value)
	}
	for _, tf := range v.facts {
		a.addFact(tf)
		a.addLabel(v.label(tf.field))
	}
	for _, tf := range v.rfacts {
		a.addFact(tf)
		a.addLabel(v.label(tf.field))
	}
	// A fact the template states is stated as "<label>: <value>", and its value is data whatever
	// the word: the host's "cancelled" in a status is not the response saying it was cancelled.
	for _, tf := range v.statedFacts() {
		for _, val := range tf.statedForms() {
			a.addStatedValue(v.label(tf.field), val)
		}
	}
	for _, f := range d.MissingFields {
		a.addString(string(f))
		a.addLabel(v.label(string(f)))
	}
	for _, is := range d.InvalidFields {
		a.addString(is.Message)
		if is.Field != nil {
			a.addString(string(*is.Field))
			a.addLabel(v.label(string(*is.Field)))
		}
	}
	if q := d.NextQuestion; q != nil {
		a.addString(string(q.Field))
		a.addLabel(v.label(string(q.Field)))
		a.addChoices(q.Choices)
	}
	a.addChoices(d.AllowedSuggestions)
	for _, na := range d.PermittedNextActions {
		if na.Label != nil {
			a.addString(*na.Label)
		}
		if na.CapabilityID != nil {
			a.addID(string(*na.CapabilityID))
		}
	}
	v.citable = make([]bool, len(d.Sources))
	for i, s := range d.Sources {
		if s.Availability == conversation.CitationAvailabilityRemoved {
			continue // a removed source is neither citable nor stateable
		}
		v.citable[i] = true
		a.addSource(s)
	}
	a.addID(string(d.TurnID))
	if d.CapabilityID != nil {
		a.addID(string(*d.CapabilityID))
	}
	if d.PreparationID != nil {
		a.addID(string(*d.PreparationID))
	}
	if d.ReceiptID != nil {
		a.addID(string(*d.ReceiptID))
	}
	for _, l := range v.opts.FieldLabels {
		a.addLabel(l)
	}
	if r != nil {
		a.addID(string(r.ReceiptID))
		a.addID(string(r.PreparationID))
		a.addID(string(r.CapabilityID))
		a.addID(string(r.AuditRef))
		for _, rec := range r.Records {
			a.addID(string(rec.EntityType))
			a.addID(string(rec.EntityID))
		}
		for _, is := range r.Errors {
			a.addString(is.Message)
			if is.Field != nil {
				a.addString(string(*is.Field))
			}
		}
		if r.FollowUp != nil {
			a.addID(string(r.FollowUp.Reference))
		}
	}
	return a
}

// Verify checks model-realised text against the DialogueDirective it realises and, for an
// outcome, the ActionReceipt the directive names. The text passes only when it:
//
//   - states every fact named in must_state (numbers, dates, times, amounts and identifiers by
//     value after normalisation, text by phrase), or its display form;
//   - states no number, date, time, amount, weekday or identifier that the directive and receipt
//     do not carry (see the package comment for what counts as one);
//   - makes no completion claim ("booked", "prenotato", "successfully") unless the receipt is
//     committed;
//   - cites only sources the directive lists.
//
// The returned error means the directive or receipt is unusable (ErrInvalidDirective,
// ErrReceiptMismatch) or the language is unsupported (ErrUnsupportedLanguage); it is never
// returned together with an OK result. Verify is deterministic, pure and takes no model.
func Verify(text string, d *conversation.DialogueDirective, r *actions.ActionReceipt, opts Options) (Result, error) {
	v, err := prepare(d, r, opts)
	if err != nil {
		return Result{}, err
	}
	return v.check(text), nil
}

var (
	// "[source: Name]" and "(fonte: Name)".
	bracketCiteRe = regexp.MustCompile(`(?i)[\[(]\s*(?:sources?|fonti|fonte|sorgenti|sorgente)\s*:\s*([^\]\)\n]{1,200})[\])]`)
	// A line that starts "Source: Name" or "Fonte: Name", or a header line "Sources:" whose names
	// follow as a list.
	citeHeaderRe = regexp.MustCompile(`(?i)^[ \t]*(?:\*\*)?(?:sources?|fonti|fonte)[ \t]*:(?:\*\*)?[ \t]*(.*)$`)
	listPrefixRe = regexp.MustCompile(`^[ \t]*(?:[-*•]|\d{1,2}[.)]|\[\d{1,2}\])[ \t]*`)
)

func fingerprint(s string) string {
	// Bounded work whatever the claim: a long run of digits is one claim per part.
	h := sha256.New()
	if len(s) > 128 {
		h.Write([]byte(strconv.Itoa(len(s))))
		s = s[:128]
	}
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil)[:4])
}

func (v *verification) check(text string) Result {
	var vs []Violation
	seen := map[Violation]bool{}
	add := func(x Violation) {
		if len(vs) < maxViolations && !seen[x] {
			seen[x] = true
			vs = append(vs, x)
		}
	}
	if strings.TrimSpace(text) == "" {
		return Result{Violations: []Violation{{Kind: ViolationEmptyText}}}
	}
	if len(text) > MaxTextBytes {
		return Result{Violations: []Violation{{Kind: ViolationTextTooLong}}}
	}
	if hasInvisible(text) {
		add(Violation{Kind: ViolationInvisibleCharacters})
	}
	norm := normalise(text)
	ex := newExtractor(norm, v.lang, findKnown(norm, v.allowed.idList()), v.allowed.currs, v.allowed.author)
	ex.markAuthored()
	ex.run()
	claims := ex.out

	completion := false
	for _, c := range claims {
		switch c.kind {
		case ckCompletion:
			completion = true
		case ckCitation:
			if !v.citationOK(c) {
				add(Violation{Kind: ViolationCitationNotInEvidence, Fingerprint: fingerprint(c.text)})
			}
		default:
			if len(vs) < maxViolations && !v.claimAllowed(c) {
				add(v.blame(c))
			}
		}
	}

	words := ex.words()
	folded := collapse(text)
	for _, tf := range v.facts {
		if v.must[tf.field] && !tf.presentIn(norm, words, claims) {
			add(Violation{Kind: ViolationMissingFact, Field: tf.field})
		}
	}

	for _, name := range namedCitations(norm) {
		if !v.nameCitable(name) {
			add(Violation{Kind: ViolationCitationNotInEvidence, Fingerprint: fingerprint(name)})
		}
	}

	if !completion {
		for _, p := range completionPhrases {
			if containsPhrase(folded, p) && !ex.phraseIsStatedData(p, folded) {
				completion = true
				break
			}
		}
	}
	if completion && !(v.r != nil && v.r.Status == actions.ActionReceiptStatusCommitted) {
		add(Violation{Kind: ViolationCompletionWithoutReceipt})
	}
	if v.d.Act == conversation.DialogueDirectiveActReportOutcome && v.r == nil {
		add(Violation{Kind: ViolationReceiptMissing})
	}
	return Result{OK: len(vs) == 0, Violations: vs}
}

// claimAllowed reports whether some reading of the claim is made only of allowed atoms.
func (v *verification) claimAllowed(c claim) bool {
	for _, r := range c.readings {
		ok := len(r) > 0
		for _, a := range r {
			if !v.allowed.has(a) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// blame builds the violation for a claim no reading of which is allowed, from the first atom of
// its first reading that is not.
func (v *verification) blame(c claim) Violation {
	out := Violation{Fingerprint: fingerprint(c.text)}
	var bad atom
	if len(c.readings) > 0 {
		for _, a := range c.readings[0] {
			if !v.allowed.has(a) {
				bad = a
				break
			}
		}
	}
	switch bad.kind {
	case atomDate:
		out.Kind, out.Got = ViolationUnknownDate, formatDateAtom(bad)
	case atomInstant:
		out.Kind, out.Got = ViolationUnknownDate, bad.s
	case atomTime:
		out.Kind, out.Got = ViolationUnknownTime, formatTimeAtom(bad)
	case atomMoney:
		out.Got = bad.s + " " + bad.cur
		out.Kind = ViolationUnknownAmount
		for k := range v.allowed.money {
			if k[0] == bad.s && k[1] != bad.cur {
				out.Kind = ViolationCurrencyMismatch
				break
			}
		}
	case atomID:
		out.Kind = ViolationUnknownIdentifier
	case atomWeekday, atomWdDate:
		out.Kind = ViolationWeekdayMismatch
	case atomNumber:
		out.Kind, out.Got = ViolationUnknownNumber, bad.s
	default: // a date or a number the normaliser could not read
		out.Kind = ViolationUnknownNumber
		if c.kind == ckDate {
			out.Kind = ViolationUnknownDate
		}
	}
	// A claim in the identifier class is reported as such even when its first atom was not.
	if c.kind == ckID {
		out.Kind, out.Got = ViolationUnknownIdentifier, ""
	}
	return out
}

func formatDateAtom(a atom) string {
	parts := []string{}
	if a.n1 != 0 {
		parts = append(parts, pad(a.n1, 4))
	} else {
		parts = append(parts, "????")
	}
	if a.n2 != 0 {
		parts = append(parts, pad(a.n2, 2))
	} else {
		parts = append(parts, "??")
	}
	if a.n3 != 0 {
		parts = append(parts, pad(a.n3, 2))
	} else {
		parts = append(parts, "??")
	}
	return strings.Join(parts, "-")
}

func formatTimeAtom(a atom) string {
	s := pad(a.n1, 2) + ":" + pad(a.n2, 2)
	if a.n3 >= 0 {
		s += ":" + pad(a.n3, 2)
	}
	return s
}

func pad(n, width int) string {
	s := formatInt(int64(n))
	for len(s) < width {
		s = "0" + s
	}
	return s
}

// citationOK reports whether a citation marker refers to a source the directive lists and has
// not marked removed.
func (v *verification) citationOK(c claim) bool {
	if len(c.readings) == 0 || len(c.readings[0]) == 0 {
		return false
	}
	a := c.readings[0][0]
	switch a.kind {
	case atomCiteIdx:
		return a.n1 >= 1 && a.n1 <= len(v.citable) && v.citable[a.n1-1]
	case atomID:
		return v.allowed.citeIDs[a.s]
	}
	return false
}

// namedCitations returns the source names a response cites in the "[source: Name]" and
// "Source: Name" forms. A semicolon or bar separates several names.
func namedCitations(norm string) []string {
	if low := strings.ToLower(norm); !strings.Contains(low, "sourc") && !strings.Contains(low, "font") && !strings.Contains(low, "sorgent") {
		return nil // no citation keyword: skip the patterns
	}
	var out []string
	collect := func(m []string) {
		for _, piece := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ';' || r == '|' }) {
			if piece = strings.Trim(strings.TrimSpace(piece), `"'*_ `); piece != "" {
				out = append(out, piece)
			}
		}
	}
	for _, m := range bracketCiteRe.FindAllStringSubmatch(norm, -1) {
		collect(m)
	}
	lines := strings.Split(norm, "\n")
	for i := 0; i < len(lines); i++ {
		m := citeHeaderRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		if strings.TrimSpace(m[1]) != "" {
			collect(m)
			continue
		}
		// A header alone: the names are the list items that follow ("- Name", "1. Name", "[1] Name").
		for j := i + 1; j < len(lines) && listPrefixRe.MatchString(lines[j]); j++ {
			collect([]string{"", listPrefixRe.ReplaceAllString(lines[j], "")})
			i = j
		}
	}
	return out
}

// nameCitable reports whether a cited name is a listed source: its display name or its ID,
// optionally followed by a locator ("Clinic policy, page 3").
func (v *verification) nameCitable(name string) bool {
	n := collapse(name)
	for i, s := range v.d.Sources {
		if !v.citable[i] {
			continue
		}
		for _, cand := range []string{collapse(s.DisplayName), collapse(string(s.SourceID))} {
			if cand == "" || !strings.HasPrefix(n, cand) {
				continue
			}
			if len(n) == len(cand) || !isAlnum(firstRune(n[len(cand):])) {
				return true
			}
		}
	}
	return false
}
