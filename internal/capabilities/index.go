package capabilities

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// Weights of the rule evidence. A term of the package's terminology is the
// strongest evidence the package can give; a word of a capability title is
// weaker. Each is divided by the number of visible capabilities that carry the
// same evidence, so a word every capability shares ("booking") counts for
// little and a word one capability owns ("lasagna") counts for a lot.
const (
	weightTerm    = 1.0
	weightKeyword = 0.4
)

// Bounds on what one package may contribute to the index. The package loader
// already limits a package; these keep the compiled index bounded even for a
// package that loader limits allow.
const (
	maxSynonymsPerTerm  = 32
	maxTermsPerContract = 64
	maxDescriptorRunes  = 600
	// maxLanguageTag is the longest BCP 47 tag the router will look at.
	maxLanguageTag = 35
)

// unit is one piece of rule evidence for one capability: a terminology term or a
// title word, with the phrases that stand for it. A unit matches when any of its
// phrases occurs in the utterance.
type unit struct {
	key     string
	kind    SignalKind
	weight  float64
	phrases []phrase
}

// nextAction is a capability a capability may lead to, and the state of the
// goal in which it may be suggested.
type nextAction struct {
	after        string
	capabilityID string
}

// capEntry is what the router keeps of one user capability.
type capEntry struct {
	id          string
	nextActions []nextAction
}

// vecSet is the cached embeddings of capability descriptors for one language.
type vecSet struct {
	model string
	dim   int
	vecs  map[string][]float32
}

// index is the compiled, immutable router view of one active package. It is
// built when the package is activated and replaced whole when the package
// changes; only the embedding cache is mutable, behind its own lock.
type index struct {
	pkg     Package
	app     string
	version string
	hash    string
	// languages are the supported tags, lower-case, in manifest order.
	languages []string
	ids       []string
	caps      map[string]*capEntry
	// units holds the rule evidence by language tag then capability ID. The
	// empty tag holds the language-neutral evidence (title words).
	units map[string]map[string][]unit
	// descriptors is the text embedded for a capability, by language tag then ID.
	descriptors map[string]map[string]string

	// fromSource and checkedAt are the registry's bookkeeping for an entry that
	// came from a Source and is re-checked after its max age.
	fromSource bool
	checkedAt  time.Time

	mu   sync.Mutex
	vecs map[string]*vecSet
}

// compile builds the index of an active package. It fails closed: a package
// that is not active, or whose typed view disagrees with the policy view, is an
// error, never a partial index.
func compile(p Package) (*index, error) {
	if p == nil {
		return nil, contracts.New(contracts.ErrInternal, "capabilities: no package to index")
	}
	if p.Status() != policy.StatusActive {
		return nil, contracts.New(contracts.ErrUnavailable, "capabilities: only an active package is indexed").
			WithDetails("application_id", p.ApplicationID()).
			WithDetails("status", string(p.Status()))
	}
	app := p.ApplicationID()
	if strings.TrimSpace(app) == "" {
		return nil, contracts.New(contracts.ErrSchemaInvalid, "capabilities: package has no application ID")
	}
	ix := &index{
		pkg:         p,
		app:         app,
		version:     p.PackageVersion(),
		hash:        p.ContentHash(),
		caps:        map[string]*capEntry{},
		units:       map[string]map[string][]unit{},
		descriptors: map[string]map[string]string{},
		vecs:        map[string]*vecSet{},
	}
	tags := map[string]string{} // lower-case tag -> tag as the manifest wrote it
	for _, l := range p.SupportedLanguages() {
		lc := strings.ToLower(strings.TrimSpace(l))
		if lc == "" {
			continue
		}
		if _, dup := tags[lc]; !dup {
			tags[lc] = l
			ix.languages = append(ix.languages, lc)
		}
	}
	terms := map[string]ap.TerminologyTerm{}
	for _, t := range p.Terminology() {
		terms[string(t.ID)] = t
	}

	for _, id := range p.CapabilityIDs() {
		spec, ok := p.Spec(id)
		if !ok {
			return nil, contracts.New(contracts.ErrSchemaInvalid, "capabilities: the package declares a capability that has no typed contract").
				WithDetails("capability_id", id)
		}
		if _, ok := p.Capability(id); !ok || string(spec.ID) != id {
			return nil, contracts.New(contracts.ErrSchemaInvalid, "capabilities: the typed and the policy view of a capability disagree").
				WithDetails("capability_id", id)
		}
		if spec.Audience != ap.CapabilityAudienceUser {
			// An admin capability is never offered to the model in a user
			// conversation (capability.schema.json), so it is never indexed.
			continue
		}
		ce := &capEntry{id: id}
		for _, na := range spec.NextActions {
			ce.nextActions = append(ce.nextActions, nextAction{after: string(na.After), capabilityID: string(na.CapabilityID)})
		}
		ix.caps[id] = ce
		ix.ids = append(ix.ids, id)

		// Language-neutral evidence: the words of the title.
		for _, tok := range keywordTokens(string(spec.Title)) {
			ix.addUnit("", id, unit{
				key: "kw:" + tok, kind: KindKeyword, weight: weightKeyword,
				phrases: []phrase{{tokens: []string{tok}, label: clipLabel(string(spec.Title))}},
			})
		}
		// Per language: the terms the capability's conversation contract uses.
		for _, lc := range ix.languages {
			c, ok := p.ContractFor(id, tags[lc])
			if !ok {
				continue
			}
			seen := map[string]bool{}
			var preferred []string
			for _, tid := range c.Terminology {
				if len(seen) >= maxTermsPerContract {
					break
				}
				key := string(tid)
				term, ok := terms[key]
				if !ok || seen[key] {
					continue
				}
				seen[key] = true
				lab, ok := labelFor(term, lc)
				if !ok {
					continue
				}
				var ps []phrase
				add := func(s string) {
					if ph, ok := newPhrase(s, lc); ok {
						ps = append(ps, ph)
					}
				}
				add(string(lab.Preferred))
				for i, s := range lab.Synonyms {
					if i >= maxSynonymsPerTerm {
						break
					}
					add(string(s))
				}
				if len(ps) == 0 {
					continue
				}
				preferred = append(preferred, string(lab.Preferred))
				ix.addUnit(lc, id, unit{key: "term:" + key, kind: KindTerminology, weight: weightTerm, phrases: ps})
			}
			if ix.descriptors[lc] == nil {
				ix.descriptors[lc] = map[string]string{}
			}
			ix.descriptors[lc][id] = descriptor(string(spec.Title), string(spec.Purpose), preferred)
		}
		// A capability with no contract in a language still has a descriptor
		// built from its title and purpose, so embedding can place it.
		for _, lc := range ix.languages {
			if ix.descriptors[lc] == nil {
				ix.descriptors[lc] = map[string]string{}
			}
			if _, ok := ix.descriptors[lc][id]; !ok {
				ix.descriptors[lc][id] = descriptor(string(spec.Title), string(spec.Purpose), nil)
			}
		}
	}
	sort.Strings(ix.ids)
	return ix, nil
}

func (ix *index) addUnit(lang, capID string, u unit) {
	m := ix.units[lang]
	if m == nil {
		m = map[string][]unit{}
		ix.units[lang] = m
	}
	m[capID] = append(m[capID], u)
}

// labelFor finds a term's labels in a language, comparing tags case-insensitively.
func labelFor(t ap.TerminologyTerm, lc string) (ap.TerminologyLabel, bool) {
	for tag, lab := range t.Labels {
		if strings.ToLower(tag) == lc {
			return lab, true
		}
	}
	return ap.TerminologyLabel{}, false
}

// descriptor is the text embedded for a capability: what the package says about
// it, never user text and never an example utterance (examples are test-only).
func descriptor(title, purpose string, labels []string) string {
	parts := []string{strings.TrimSpace(title), strings.TrimSpace(purpose)}
	if len(labels) > 0 {
		parts = append(parts, strings.Join(labels, ", "))
	}
	s := strings.Join(nonEmpty(parts), ". ")
	if r := []rune(s); len(r) > maxDescriptorRunes {
		s = string(r[:maxDescriptorRunes])
	}
	return s
}

func nonEmpty(in []string) []string {
	out := in[:0:0]
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// resolveLanguages decides which languages' evidence an utterance is matched
// against. A tag the package supports (exactly, or by primary subtag: "it-CH"
// for "it") selects that language alone. An empty or unsupported tag selects
// every supported language: the router has no language detector, and a shortlist
// widened across languages is still only a shortlist of visible capabilities.
func (ix *index) resolveLanguages(tag string) []string {
	if len(tag) > maxLanguageTag {
		tag = "" // not a language tag
	}
	tag = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(tag, "_", "-")))
	if tag != "" {
		for _, l := range ix.languages {
			if l == tag {
				return []string{l}
			}
		}
		primary := tag
		if i := strings.IndexByte(tag, '-'); i >= 0 {
			primary = tag[:i]
		}
		var out []string
		for _, l := range ix.languages {
			lp := l
			if i := strings.IndexByte(l, '-'); i >= 0 {
				lp = l[:i]
			}
			if lp == primary {
				out = append(out, l)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return append([]string(nil), ix.languages...)
}
