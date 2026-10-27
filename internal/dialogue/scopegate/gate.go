package scopegate

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// ID names the rule set. It changes whenever a rule, a word list or a bound changes, and
// Digest pins the content, so a result that says it ran behind "scopegate-v1" can be checked
// against the code that produced it.
const ID = "scopegate-v1"

// Bounds. They are the router's (internal/capabilities): the utterance is read up to 1000
// runes and 128 tokens, and a label up to 64 tokens.
const (
	maxUtteranceRunes = 1000
	maxUtteranceToks  = 128
	maxLabelToks      = 64
	maxEvidence       = 8
)

// Field is one field of a capability as the gate reads it: its name and, for an enum, the
// allowed values. Both are application wording.
type Field struct {
	Name string
	Enum []string
}

// Capability is one capability the caller may see (policy-exposed), as the gate reads it.
// ID, Description, the field names and the enum values are what every catalogue carries.
// Terms is optional application-supplied terminology, language tag to labels (the
// terminology of the capability's conversation contract, M1-08); it is what lets the gate
// read a language the description is not written in.
type Capability struct {
	ID          string
	Description string
	Fields      []Field
	Terms       map[string][]string
}

// DomainVocab is the application-package assistant.domain wording the gate may
// use when the analyst path is active (ADR 0047). It is a local projection: the
// gate stays stdlib-only and never imports the analyst contracts.
type DomainVocab struct {
	Description string
	InScope     []string
	OutOfScope  []string
	// Examples are flattened in-/out-of-domain example utterances (any language).
	Examples []string
}

// Request is one gate question: is this utterance about anything the caller may do.
type Request struct {
	// Utterance is the user's text for this turn.
	Utterance string
	// Catalogue is the capabilities the caller may see, and nothing else: the gate never
	// reads what the caller may not use (least capability).
	Catalogue []Capability
	// Engaged is true while the session has an active goal or a pending question: the
	// utterance may be the answer to it ("tomorrow at 8", "John Smith") and says nothing
	// about the catalogue by itself, so it is not judged.
	Engaged bool
	// AnalystActive is true when the effective analyst loop is on and a domain is
	// declared. Then vocabulary also includes Domain, so an in-domain question that
	// matches no capability is not refused here (the domain classifier decides).
	AnalystActive bool
	// Domain is the package domain wording used only when AnalystActive is true.
	Domain DomainVocab
}

// Verdict is the gate's answer.
type Verdict string

const (
	// VerdictRefuse: the utterance has nothing to do with any exposed capability. The
	// controller model is not called; the answer is the typed refusal.
	VerdictRefuse Verdict = "refuse"
	// VerdictPass: the utterance shares a word with the catalogue. The controller decides.
	VerdictPass Verdict = "pass"
	// VerdictAbstain: the gate cannot judge the utterance. The controller decides.
	VerdictAbstain Verdict = "abstain"
)

// Reason says why the verdict is what it is.
type Reason string

const (
	// ReasonNoVisibleCapability (refuse): nothing is exposed, so nothing is in scope.
	ReasonNoVisibleCapability Reason = "no_visible_capability"
	// ReasonNoOverlap (refuse): the utterance is readable, has content words and none of
	// them is a word of any exposed capability.
	ReasonNoOverlap Reason = "no_overlap"
	// ReasonOverlap (pass): at least one content word of the utterance is a word of an
	// exposed capability.
	ReasonOverlap Reason = "overlap"
	// ReasonEngaged (abstain): the session is waiting for the user's answer.
	ReasonEngaged Reason = "engaged"
	// ReasonNoContentWords (abstain): the utterance has no word that can be evidence
	// ("What can you do?", a greeting, a single letter).
	ReasonNoContentWords Reason = "no_content_words"
	// ReasonUnreadableLanguage (abstain): the utterance is in no language the catalogue's
	// wording covers, or in none the gate knows.
	ReasonUnreadableLanguage Reason = "unreadable_language"
)

// Match is one piece of evidence of a pass: a word of the utterance and the word of the
// catalogue it matches.
type Match struct {
	Word  string
	Label string
}

// Decision is the gate's result. It is advisory about the controller (a pass or an abstain
// grants nothing: the policy gateway, the schema check and the validation still decide
// everything) and authoritative about nothing but "skip the model call and answer with a
// refusal".
type Decision struct {
	Verdict Verdict
	Reason  Reason
	// Language is the language the utterance was read as ("en", "it"), empty when none.
	Language string
	// Matches is the evidence of a pass, sorted, at most eight.
	Matches []Match
}

// Refused reports whether the gate answers instead of the model.
func (d Decision) Refused() bool { return d.Verdict == VerdictRefuse }

// Refusal is the typed answer of a refusing gate: an out-of-scope TaskProposal with no
// capability and no data, and the reason.
type Refusal struct {
	Reason Reason
}

// Intent is the TaskProposal intent of a refusal. It is always "out_of_scope".
func (Refusal) Intent() string { return "out_of_scope" }

// ProposalJSON is the refusal as the model-authority part of a TaskProposal, in the key
// order of the contract (which is also the order the output grammar demands). It validates
// against the model-only TaskProposal schema and carries uncertainty 1: the refusal is a
// rule, not a guess.
func (Refusal) ProposalJSON() []byte {
	return []byte(`{"capability_id":null,"fields":{},"entity_refs":[],"intent":"out_of_scope","uncertainty":{"overall":1}}`)
}

// Refusal returns the typed refusal when the gate refused.
func (d Decision) Refusal() (Refusal, bool) {
	if d.Verdict != VerdictRefuse {
		return Refusal{}, false
	}
	return Refusal{Reason: d.Reason}, true
}

// Decide is the gate. It is a pure function of the request: no clock, no randomness, no
// model, no file, no network, and the same request always gives the same decision.
//
// The rules, in order, are the whole of the gate:
//
//  1. an empty catalogue refuses (ReasonNoVisibleCapability);
//  2. an engaged session abstains (ReasonEngaged);
//  3. an utterance with no content word abstains (ReasonNoContentWords);
//  4. an utterance whose language the catalogue does not cover abstains
//     (ReasonUnreadableLanguage);
//  5. a content word that matches a word of the catalogue (id, description, field name,
//     enum value, or a term in the utterance's language) passes (ReasonOverlap), using the
//     router's stem rule;
//  6. otherwise the utterance is unrelated and is refused (ReasonNoOverlap).
func Decide(req Request) Decision {
	if len(req.Catalogue) == 0 {
		return Decision{Verdict: VerdictRefuse, Reason: ReasonNoVisibleCapability}
	}
	if req.Engaged {
		return Decision{Verdict: VerdictAbstain, Reason: ReasonEngaged}
	}
	utt := truncateRunes(strings.TrimSpace(req.Utterance), maxUtteranceRunes)
	toks := tokenize(utt, maxUtteranceToks)
	content := contentWords(utt, maxUtteranceToks)
	if len(content) == 0 {
		return Decision{Verdict: VerdictAbstain, Reason: ReasonNoContentWords}
	}
	lang := detectLanguage(toks)
	covered := catalogueLanguages(req.Catalogue)
	if req.AnalystActive && domainDeclared(req.Domain) {
		for l := range domainLanguages(req.Domain) {
			covered[l] = true
		}
	}
	if lang == "" || !covered[lang] {
		return Decision{Verdict: VerdictAbstain, Reason: ReasonUnreadableLanguage, Language: lang}
	}
	vocab := vocabulary(req.Catalogue, lang)
	if req.AnalystActive && domainDeclared(req.Domain) {
		for _, w := range domainVocabulary(req.Domain) {
			vocab = append(vocab, w)
		}
	}
	var matches []Match
	seen := map[Match]bool{}
	for _, w := range content {
		for _, v := range vocab {
			if tokensMatch(w, v) {
				m := Match{Word: w, Label: v}
				if !seen[m] {
					seen[m] = true
					matches = append(matches, m)
				}
			}
		}
	}
	if len(matches) == 0 {
		return Decision{Verdict: VerdictRefuse, Reason: ReasonNoOverlap, Language: lang}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Word != matches[j].Word {
			return matches[i].Word < matches[j].Word
		}
		return matches[i].Label < matches[j].Label
	})
	if len(matches) > maxEvidence {
		matches = matches[:maxEvidence]
	}
	return Decision{Verdict: VerdictPass, Reason: ReasonOverlap, Language: lang, Matches: matches}
}

// catalogueLanguages is the set of languages the catalogue can be read in: the language of
// each description (by the same function-word detector), plus every language a capability
// supplies terms for.
func catalogueLanguages(cat []Capability) map[string]bool {
	out := map[string]bool{}
	for _, c := range cat {
		if l := detectLanguage(tokenize(c.Description, maxLabelToks)); l != "" {
			out[l] = true
		}
		for tag, labels := range c.Terms {
			if l := primaryTag(tag); l != "" && len(labels) > 0 {
				out[l] = true
			}
		}
	}
	return out
}

func domainDeclared(d DomainVocab) bool {
	return strings.TrimSpace(d.Description) != "" || len(d.InScope) > 0 || len(d.OutOfScope) > 0 || len(d.Examples) > 0
}

// domainLanguages are languages detectable from domain examples (and English when
// description/in_scope/out_of_scope carry English function words).
func domainLanguages(d DomainVocab) map[string]bool {
	out := map[string]bool{}
	add := func(s string) {
		if l := detectLanguage(tokenize(s, maxLabelToks)); l != "" {
			out[l] = true
		}
	}
	add(d.Description)
	for _, s := range d.InScope {
		add(s)
	}
	for _, s := range d.OutOfScope {
		add(s)
	}
	for _, s := range d.Examples {
		add(s)
	}
	return out
}

// domainVocabulary is content words of the declared domain.
func domainVocabulary(d DomainVocab) []string {
	set := map[string]bool{}
	add := func(s string) {
		for _, w := range contentWords(s, maxLabelToks) {
			set[w] = true
		}
	}
	add(d.Description)
	for _, s := range d.InScope {
		add(s)
	}
	for _, s := range d.OutOfScope {
		add(s)
	}
	for _, s := range d.Examples {
		add(s)
	}
	return sortedKeys(set)
}

// vocabulary is the distinct words of the catalogue that an utterance in lang can match:
// the capability ids, descriptions, field names and enum values (identifiers and wording
// the application chose; language-neutral), and the terms supplied for lang.
func vocabulary(cat []Capability, lang string) []string {
	set := map[string]bool{}
	add := func(s string) {
		for _, w := range contentWords(s, maxLabelToks) {
			set[w] = true
		}
	}
	for _, c := range cat {
		add(c.ID)
		add(c.Description)
		for _, f := range c.Fields {
			add(f.Name)
			for _, e := range f.Enum {
				add(e)
			}
		}
		for tag, labels := range c.Terms {
			if primaryTag(tag) != lang {
				continue
			}
			for _, l := range labels {
				add(l)
			}
		}
	}
	return sortedKeys(set)
}

func truncateRunes(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// Digest is the SHA-256 of everything that decides a verdict: the ID, the bounds and the
// word lists. It is recorded next to every measurement that ran behind the gate.
func Digest() string {
	h := sha256.New()
	w := func(s string) { h.Write([]byte(s)); h.Write([]byte{0}) }
	w(ID)
	w(strconv.Itoa(maxUtteranceRunes))
	w(strconv.Itoa(maxUtteranceToks))
	w(strconv.Itoa(maxLabelToks))
	w(strconv.Itoa(minContentRunes))
	w("stopwords")
	for _, k := range sortedKeys(stopwords) {
		w(k)
	}
	w("en")
	for _, k := range sortedKeys(enMarkers) {
		w(k)
	}
	w("it")
	for _, k := range sortedKeys(itMarkers) {
		w(k)
	}
	return hex.EncodeToString(h.Sum(nil))
}
