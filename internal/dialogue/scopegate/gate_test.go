package scopegate

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The catalogues below are invented for these tests. They are not the benchmark corpus.

func libraryCatalogue() []Capability {
	return []Capability{
		{ID: "loans.renew", Description: "Renew a book loan", Fields: []Field{{Name: "loan_ref"}}},
		{ID: "books.search", Description: "Search the library catalogue", Fields: []Field{{Name: "title"}, {Name: "author"}}},
		{ID: "rooms.reserve", Description: "Reserve a study room", Fields: []Field{{Name: "room_size", Enum: []string{"small", "large"}}, {Name: "date"}}},
		{ID: "fines.status", Description: "Check the fines on my account", Fields: nil},
	}
}

func gymCatalogue() []Capability {
	return []Capability{
		{ID: "classes.book", Description: "Book a fitness class", Fields: []Field{{Name: "class_type", Enum: []string{"yoga", "spinning", "pilates"}}, {Name: "date"}}},
		{ID: "membership.freeze", Description: "Freeze my membership", Fields: []Field{{Name: "months"}}},
		{ID: "trainers.list", Description: "List the personal trainers", Fields: nil},
	}
}

func decide(utt string, cat []Capability) Decision {
	return Decide(Request{Utterance: utt, Catalogue: cat})
}

func TestGateRefusesUtterancesUnrelatedToTheCatalogue(t *testing.T) {
	cases := []struct {
		name string
		cat  []Capability
		utt  string
	}{
		{"geography", libraryCatalogue(), "What is the tallest mountain in Europe?"},
		{"creative writing", libraryCatalogue(), "Compose a haiku about the sea."},
		{"technology", gymCatalogue(), "Explain how a neural network learns."},
		{"history", gymCatalogue(), "Who painted the Mona Lisa?"},
		{"banking", libraryCatalogue(), "Transfer 300 euros to my brother."},
		{"mixed case and punctuation", gymCatalogue(), "WHAT IS THE SPEED OF LIGHT?!"},
		{"accented", libraryCatalogue(), "Could you recommend a good café in Zürich?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := decide(tc.utt, tc.cat)
			if d.Verdict != VerdictRefuse || d.Reason != ReasonNoOverlap {
				t.Fatalf("Decide(%q) = %+v, want refuse/no_overlap", tc.utt, d)
			}
			if !d.Refused() {
				t.Fatal("Refused() is false for a refusal")
			}
			r, ok := d.Refusal()
			if !ok || r.Intent() != "out_of_scope" || r.Reason != ReasonNoOverlap {
				t.Fatalf("Refusal() = %+v, %v", r, ok)
			}
			var p struct {
				CapabilityID *string        `json:"capability_id"`
				Fields       map[string]any `json:"fields"`
				EntityRefs   []any          `json:"entity_refs"`
				Intent       string         `json:"intent"`
			}
			if err := json.Unmarshal(r.ProposalJSON(), &p); err != nil {
				t.Fatalf("the refusal is not JSON: %v", err)
			}
			if p.CapabilityID != nil || len(p.Fields) != 0 || len(p.EntityRefs) != 0 || p.Intent != "out_of_scope" {
				t.Fatalf("the refusal proposal is not an empty out_of_scope: %+v", p)
			}
		})
	}

	t.Run("nothing exposed refuses everything", func(t *testing.T) {
		d := Decide(Request{Utterance: "Renew my loan, please.", Catalogue: nil})
		if d.Verdict != VerdictRefuse || d.Reason != ReasonNoVisibleCapability {
			t.Fatalf("an empty catalogue gave %+v", d)
		}
	})

	t.Run("a word of a capability that is not exposed is not evidence", func(t *testing.T) {
		// The caller sees only the gym catalogue: a library word is unrelated to it.
		if d := decide("Renew my library loan.", gymCatalogue()); d.Verdict != VerdictRefuse {
			t.Fatalf("a word of an unexposed capability related the utterance: %+v", d)
		}
	})

	t.Run("application terms make another language readable", func(t *testing.T) {
		cat := libraryCatalogue()
		cat[0].Terms = map[string][]string{"it": {"prestito", "rinnovo", "rinnovare"}}
		cat[1].Terms = map[string][]string{"it": {"cerca", "libro", "catalogo"}}
		d := decide("Qual è la capitale della Francia?", cat)
		if d.Verdict != VerdictRefuse || d.Language != LangIT {
			t.Fatalf("an unrelated Italian utterance with Italian terms gave %+v", d)
		}
		if d := decide("Vorrei rinnovare il prestito del mio libro.", cat); d.Verdict != VerdictPass {
			t.Fatalf("a related Italian utterance with Italian terms gave %+v", d)
		}
	})
}

func TestGateNeverRefusesAnExampleOfAnExposedCapability(t *testing.T) {
	// An example is built from a capability's own wording: what its author wrote. The gate
	// may judge other utterances unrelated, but never one that is made of the capability's
	// own words. (This pins "own wording is never refused", not "no false refusals": a
	// request that shares no word with the catalogue is refused, and the report counts those.)
	var examples func(c Capability) []string
	examples = func(c Capability) []string {
		plain := strings.NewReplacer(".", " ", "_", " ").Replace(c.ID)
		out := []string{
			c.Description,
			"I would like to " + strings.ToLower(c.Description) + ".",
			strings.ToUpper(c.Description) + "!!!",
			"  " + c.Description + "  ",
			plain,
			"Can you help me with this: " + plain + "?",
		}
		for _, f := range c.Fields {
			out = append(out, "my "+strings.ReplaceAll(f.Name, "_", " ")+" is wrong")
			for _, e := range f.Enum {
				out = append(out, "the "+e+" one, please")
			}
		}
		return out
	}
	n := 0
	for _, cat := range [][]Capability{libraryCatalogue(), gymCatalogue()} {
		for _, c := range cat {
			for _, ex := range examples(c) {
				n++
				if d := decide(ex, cat); d.Refused() {
					t.Errorf("refused an example of %s: %q -> %+v", c.ID, ex, d)
				}
			}
		}
	}
	if n < 40 {
		t.Fatalf("only %d examples were checked", n)
	}

	t.Run("inflections of a catalogue word are the same word", func(t *testing.T) {
		for _, ex := range []string{"I want to book two classes", "Is there a booking for the yoga session?", "any spinning classes today?", "freezing the memberships", "list the trainers"} {
			if d := decide(ex, gymCatalogue()); d.Verdict != VerdictPass {
				t.Errorf("%q gave %+v, want pass", ex, d)
			}
		}
	})

	t.Run("an example in a language the catalogue has terms for", func(t *testing.T) {
		cat := gymCatalogue()
		cat[0].Terms = map[string][]string{"it": {"prenotare", "prenotazione", "lezione", "corso"}}
		for _, ex := range []string{"Vorrei prenotare una lezione di yoga.", "PRENOTAZIONE corso!", "Una lezione per domani, per favore."} {
			if d := decide(ex, cat); d.Refused() {
				t.Errorf("refused an Italian example of classes.book: %q -> %+v", ex, d)
			}
		}
	})

	t.Run("what the gate cannot read is left to the controller", func(t *testing.T) {
		cases := []struct {
			name string
			req  Request
			want Reason
		}{
			{"an answer to a pending question", Request{Utterance: "Tomorrow at eight.", Catalogue: gymCatalogue(), Engaged: true}, ReasonEngaged},
			{"no content word", Request{Utterance: "What can you do?", Catalogue: gymCatalogue()}, ReasonNoContentWords},
			{"a greeting made of stop words", Request{Utterance: "Hi, can I?", Catalogue: gymCatalogue()}, ReasonNoContentWords},
			{"an empty utterance", Request{Utterance: "   ", Catalogue: gymCatalogue()}, ReasonNoContentWords},
			{"a language the catalogue does not cover", Request{Utterance: "Vorrei sapere dove si trova la piscina.", Catalogue: gymCatalogue()}, ReasonUnreadableLanguage},
			{"a language the gate does not know", Request{Utterance: "Bonjour, je voudrais une serviette.", Catalogue: gymCatalogue()}, ReasonUnreadableLanguage},
			{"no function word to read the language by", Request{Utterance: "yoga tomorrow", Catalogue: gymCatalogue()}, ReasonUnreadableLanguage},
			{"an unspaced script", Request{Utterance: "今日の天気は", Catalogue: gymCatalogue()}, ReasonNoContentWords},
		}
		for _, tc := range cases {
			d := Decide(tc.req)
			if d.Verdict != VerdictAbstain || d.Reason != tc.want {
				t.Errorf("%s: %+v, want abstain/%s", tc.name, d, tc.want)
			}
			if _, ok := d.Refusal(); ok {
				t.Errorf("%s: an abstain produced a refusal", tc.name)
			}
		}
	})
}

func TestGateIsDeterministicAndNeedsNoModel(t *testing.T) {
	reqs := []Request{
		{Utterance: "I want to book a yoga class tomorrow", Catalogue: gymCatalogue()},
		{Utterance: "What is the capital of Peru?", Catalogue: gymCatalogue()},
		{Utterance: "Vorrei prenotare", Catalogue: gymCatalogue()},
		{Utterance: "Renew the loan and search the catalogue for the author", Catalogue: libraryCatalogue()},
	}
	want := make([]Decision, len(reqs))
	for i, r := range reqs {
		want[i] = Decide(r)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				for i, r := range reqs {
					if got := Decide(r); !reflect.DeepEqual(got, want[i]) {
						t.Errorf("run %d of %q: %+v, want %+v", n, r.Utterance, got, want[i])
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	// The evidence of a pass is sorted and bounded, so equal requests give equal bytes.
	d := Decide(reqs[3])
	if len(d.Matches) == 0 || len(d.Matches) > maxEvidence {
		t.Fatalf("evidence size %d", len(d.Matches))
	}
	for i := 1; i < len(d.Matches); i++ {
		a, b := d.Matches[i-1], d.Matches[i]
		if a.Word > b.Word || (a.Word == b.Word && a.Label >= b.Label) {
			t.Fatalf("evidence is not sorted: %+v", d.Matches)
		}
	}

	// Needs no model: the package imports nothing but a short list of standard-library
	// packages, which excludes every provider, adapter, network, file and process package.
	allowed := map[string]bool{
		"crypto/sha256": true, "encoding/hex": true, "sort": true, "strconv": true, "strings": true,
		"unicode": true, "unicode/utf8": true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !allowed[path] {
				t.Errorf("%s imports %q: the gate may not need anything beyond %v", e.Name(), path, allowed)
			}
		}
		checked++
	}
	if checked < 4 {
		t.Fatalf("only %d source files were inspected", checked)
	}
}

func meetingsDomain() DomainVocab {
	return DomainVocab{
		Description: "Meetings, minutes, companies and Italian company governance.",
		InScope:     []string{"company meetings and minutes", "corporate incentives and filings", "governance"},
		OutOfScope:  []string{"general chatbot use", "unrelated consumer topics"},
		Examples: []string{
			"Should we apply for incentive XX?",
			"Is a Persian cat big?",
			"Should my company have applied for the Italian 2026 company discount XX?",
		},
	}
}

func TestAnalystActiveDoesNotRefuseInDomainWithoutCapability(t *testing.T) {
	// Catalogue wording deliberately avoids "company"/"incentive"/"discount" so the
	// utterance has no capability overlap; with analyst+domain it still passes.
	cat := []Capability{
		{ID: "rooms.book", Description: "Book a conference room for the team", Fields: []Field{{Name: "date"}},
			Terms: map[string][]string{"en": {"room", "booking", "schedule"}}},
	}
	utt := "Should my company have applied for the Italian 2026 company discount XX?"
	if d := Decide(Request{Utterance: utt, Catalogue: cat}); d.Verdict != VerdictRefuse {
		t.Fatalf("without analyst the gate should refuse (no capability overlap): %+v", d)
	}
	d := Decide(Request{Utterance: utt, Catalogue: cat, AnalystActive: true, Domain: meetingsDomain()})
	if d.Refused() {
		t.Fatalf("analyst+domain must not refuse an in-domain / no-capability utterance: %+v", d)
	}
	if d.Verdict != VerdictPass {
		t.Fatalf("want pass, got %+v", d)
	}
}

func TestAnalystActiveStillRefusesClearlyUnrelated(t *testing.T) {
	cat := []Capability{
		{ID: "rooms.book", Description: "Book a conference room", Fields: []Field{{Name: "date"}}},
	}
	// "Persian cat" shares no content word with the meetings domain once "big"/"is" are stopwords;
	// "cat"/"Persian" are not in domain vocab (examples include the full out-of-domain sentence,
	// so "Persian"/"cat"/"big" ARE in domain examples — that would pass). Use geography instead.
	utt := "What is the tallest mountain in Europe?"
	d := Decide(Request{Utterance: utt, Catalogue: cat, AnalystActive: true, Domain: meetingsDomain()})
	if d.Verdict != VerdictRefuse || d.Reason != ReasonNoOverlap {
		t.Fatalf("clearly unrelated under analyst+domain: %+v", d)
	}
}

func TestDigestPinsTheRules(t *testing.T) {
	d := Digest()
	if len(d) != 64 || d != Digest() {
		t.Fatalf("digest %q is not a stable SHA-256", d)
	}
	// Changing a word list, a bound or the ID changes this value on purpose: a measurement
	// that was taken behind another rule set must not be reported as this one.
	const pinned = "4c241f7fec2eab515cc62cf197fc6cc526a258158c5a803c4037041146bc9b1e"
	if d != pinned {
		t.Fatalf("the gate's rules changed: digest %s (update the pin and the protocol)", d)
	}
}

func TestLanguageDetectionIsClosedClass(t *testing.T) {
	cases := map[string]string{
		"What's the capital of Australia?":   LangEN,
		"Qual è la capitale dell'Australia?": LangIT,
		"Write me a poem about autumn.":      LangEN,
		"Scrivimi una poesia sull'autunno.":  LangIT,
		"yoga":                               "",
		"a in per con no":                    "",     // function words of both languages are in neither list
		"Il tavolo and chair":                "",     // one of each: a tie is no evidence
		"Je voudrais un café":                LangIT, // "un" is Italian-only here: the detector knows two languages and says so
		"Dov'è il mio ordine?":               LangIT,
		"Where is my order?":                 LangEN,
		"Posso vedere il menu?":              LangIT,
		"Can I see what's on the menu?":      LangEN,
		"":                                   "",
	}
	for in, want := range cases {
		if got := detectLanguage(tokenize(in, 128)); got != want {
			t.Errorf("detectLanguage(%q) = %q, want %q", in, got, want)
		}
	}
	if primaryTag("en-GB") != LangEN || primaryTag("IT_it") != LangIT || primaryTag("fr") != "" || primaryTag("") != "" {
		t.Error("primaryTag does not reduce tags")
	}
}
