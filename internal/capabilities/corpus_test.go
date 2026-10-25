package capabilities

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The routing corpus (tests/performance/corpus/routing-v0.jsonl) was written to
// evaluate the conversational controller: each row is a catalogue of capabilities
// (id, a one-line description, field names), an utterance and the expected
// capability. This file reuses it to measure the router, which is the stage that
// decides which of those capabilities the controller is shown. What the router
// can know about a capability is what a package gives it, so the measurement is
// made on three packages built from each catalogue:
//
//	catalogue  the catalogue only: the description as the capability title, no
//	           terminology. What the router can do with nothing but the corpus.
//	lexicon    the catalogue plus the terminology in testdata/corpus_lexicon.json
//	           (English and Italian labels, written by the task author after
//	           reading the corpus; see the disclosure in that file). Rules only.
//	tuned      the lexicon plus the words added after the first pass missed
//	           ("tuning" in that file). An upper bound: it was fitted to this corpus.
//
// and, for the lexicon packages, with the optional embedder: the deterministic
// fake (a hashed bag of words, 384 dimensions like the Lite embedding model). It
// is NOT a semantic model: its numbers show what the embedding stage does with a
// similarity it is given, not how good a real embedding model is. Real-model
// numbers need the model loaded (milestone M0-13) and are recorded as pending in
// docs/efficiency/M1-08.md.

type corpusField struct {
	Name string `json:"name"`
}

type corpusCap struct {
	ID          string        `json:"id"`
	Description string        `json:"description"`
	Fields      []corpusField `json:"fields"`
}

type corpusRow struct {
	ID        string      `json:"id"`
	Language  string      `json:"language"`
	Category  string      `json:"category"`
	Text      string      `json:"text"`
	Catalogue []corpusCap `json:"catalogue"`
	Expected  struct {
		CapabilityID *string `json:"capability_id"`
		Intent       string  `json:"intent"`
	} `json:"expected"`
}

func loadCorpus(t testing.TB) []corpusRow {
	t.Helper()
	f, err := os.Open("../../tests/performance/corpus/routing-v0.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows []corpusRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var r corpusRow
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("corpus row: %v", err)
		}
		rows = append(rows, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

type lexicon struct {
	Terms        map[string]map[string][]string `json:"terms"`
	Capabilities map[string][]string            `json:"capabilities"`
	Tuning       map[string]map[string][]string `json:"tuning"`
}

func loadLexicon(t testing.TB) lexicon {
	t.Helper()
	b, err := os.ReadFile("testdata/corpus_lexicon.json")
	if err != nil {
		t.Fatal(err)
	}
	var l lexicon
	if err := json.Unmarshal(b, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

type packageKind string

const (
	kindCatalogue packageKind = "catalogue"
	kindLexicon   packageKind = "lexicon"
	kindTuned     packageKind = "tuned"
)

// corpusPackages builds one package per distinct catalogue of the corpus.
type corpusPackages struct {
	byKey map[string]*testPkg
	app   map[string]string // catalogue key -> application ID
	caps  map[string][]string
}

// catalogueKey identifies a package: the capabilities of the catalogue and the
// language its descriptions are written in (the corpus localises the description
// to the language of the row, so an English row and an Italian row with the same
// capabilities are two packages).
func catalogueKey(r corpusRow) string {
	ids := make([]string, len(r.Catalogue))
	for i, x := range r.Catalogue {
		ids[i] = x.ID
	}
	return r.Language + "|" + strings.Join(ids, ",")
}

func buildCorpusPackages(t testing.TB, rows []corpusRow, lex lexicon, kind packageKind) *corpusPackages {
	t.Helper()
	cp := &corpusPackages{byKey: map[string]*testPkg{}, app: map[string]string{}, caps: map[string][]string{}}
	n := 0
	for _, r := range rows {
		key := catalogueKey(r)
		if _, ok := cp.byKey[key]; ok {
			continue
		}
		n++
		app := fmt.Sprintf("corpus-%02d", n)
		def := pkgDef{app: app, langs: []string{"en", "it"}, roles: []string{"member"}, grants: map[string][]string{}}
		used := map[string]bool{}
		for _, c := range r.Catalogue {
			cd := capDef{id: c.ID, title: c.Description, purpose: c.Description}
			for _, f := range c.Fields {
				cd.fields = append(cd.fields, f.Name)
			}
			if kind != kindCatalogue {
				ids := lex.Capabilities[c.ID]
				if len(ids) == 0 {
					t.Fatalf("lexicon has no terms for %s", c.ID)
				}
				cd.terms = map[string][]string{"en": ids, "it": ids}
				for _, id := range ids {
					used[id] = true
				}
			}
			def.caps = append(def.caps, cd)
			def.grants["member"] = append(def.grants["member"], c.ID)
			cp.caps[app] = append(cp.caps[app], c.ID)
		}
		var ids []string
		for id := range used {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			labels, ok := lex.Terms[id]
			if !ok {
				t.Fatalf("lexicon has no term %q", id)
			}
			td := termDef{id: id, labels: map[string][]string{}}
			for lang, ws := range labels {
				td.labels[lang] = append([]string(nil), ws...)
			}
			if kind == kindTuned {
				for lang, ws := range lex.Tuning[id] {
					td.labels[lang] = append(td.labels[lang], ws...)
				}
			}
			def.terms = append(def.terms, td)
		}
		cp.byKey[key] = def.build(t)
		cp.app[key] = app
	}
	return cp
}

func (cp *corpusPackages) all() []Package {
	keys := make([]string, 0, len(cp.byKey))
	for k := range cp.byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Package, 0, len(keys))
	for _, k := range keys {
		out = append(out, cp.byKey[k])
	}
	return out
}

func (cp *corpusPackages) scope(r corpusRow) contracts.Scope {
	key := catalogueKey(r)
	return scopeFor(cp.app[key], "member", cp.caps[cp.app[key]])
}

// countingEmbedder counts the calls the router makes.
type countingEmbedder struct {
	inner      providers.Embedder
	query, doc atomic.Int64
}

func (c *countingEmbedder) Embed(ctx context.Context, sc contracts.Scope, r providers.EmbedRequest) (providers.EmbedResult, error) {
	if r.Purpose == providers.EmbedQuery {
		c.query.Add(1)
	} else {
		c.doc.Add(1)
	}
	return c.inner.Embed(ctx, sc, r)
}

// corpusMetrics is the outcome of one run over the corpus.
type corpusMetrics struct {
	name string

	capRows   int    // rows that expect a capability
	topK      [6]int // topK[k]: rows whose expected capability is within the first k candidates
	routed    int    // capability rows with a non-empty shortlist
	byLang    map[string][2]int
	byCat     map[string][2]int
	oosRows   int // rows that expect out_of_scope
	oosOK     int // ...and got an empty shortlist
	intents   int // cancel/help rows: a dialogue intent, not a capability
	intentHit int // ...that got a non-empty shortlist (informational)
	decisive  int
	embedUsed int
	embedQ    int64
	embedD    int64
	shortlist int // total candidates returned, over rows with a shortlist
	listed    int // rows with a shortlist
	catSize   int // total catalogue size over those rows
	misses    []string
	oosMisses []string
}

func (m *corpusMetrics) pct(n, d int) string {
	if d == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", 100*float64(n)/float64(d), n, d)
}

func runCorpus(t testing.TB, rows []corpusRow, cp *corpusPackages, name string, opts []RouterOption) *corpusMetrics {
	t.Helper()
	rt, _ := newRouter(t, opts, cp.all()...)
	m := &corpusMetrics{name: name, byLang: map[string][2]int{}, byCat: map[string][2]int{}}
	for _, r := range rows {
		res, err := rt.Route(context.Background(), reqFor(cp.scope(r), r.Text, inLanguage(r.Language)))
		if err != nil {
			t.Fatalf("%s: %v", r.ID, err)
		}
		if len(res.Candidates) > ShortlistCeiling {
			t.Fatalf("%s: %d candidates", r.ID, len(res.Candidates))
		}
		if res.Embedding == EmbeddingUsed {
			m.embedUsed++
		}
		if len(res.Candidates) > 0 {
			m.listed++
			m.shortlist += len(res.Candidates)
			m.catSize += len(r.Catalogue)
			if res.Decisive {
				m.decisive++
			}
		}
		switch {
		case r.Expected.CapabilityID != nil:
			m.capRows++
			exp := *r.Expected.CapabilityID
			if len(res.Candidates) > 0 {
				m.routed++
			}
			hit := false
			for i, c := range res.Candidates {
				if c.CapabilityID == exp {
					for k := i + 1; k <= 5; k++ {
						m.topK[k]++
					}
					hit = true
					break
				}
			}
			l, c := m.byLang[r.Language], m.byCat[r.Category]
			l[1]++
			c[1]++
			if len(res.Candidates) > 0 && res.Candidates[0].CapabilityID == exp {
				l[0]++
				c[0]++
			} else {
				got := "-"
				if len(res.Candidates) > 0 {
					got = strings.Join(res.IDs(), ",")
				}
				m.misses = append(m.misses, fmt.Sprintf("%s %q want %s got [%s] top5=%v", r.ID, r.Text, exp, got, hit))
			}
			m.byLang[r.Language], m.byCat[r.Category] = l, c
		case r.Expected.Intent == "out_of_scope":
			m.oosRows++
			if len(res.Candidates) == 0 {
				m.oosOK++
			} else {
				m.oosMisses = append(m.oosMisses, fmt.Sprintf("%s %q shortlisted [%s]", r.ID, r.Text, strings.Join(res.IDs(), ",")))
			}
		default:
			m.intents++
			if len(res.Candidates) > 0 {
				m.intentHit++
			}
		}
	}
	return m
}

func (m *corpusMetrics) log(t testing.TB) {
	t.Helper()
	var meanK, meanCat float64
	if m.listed > 0 {
		meanK = float64(m.shortlist) / float64(m.listed)
		meanCat = float64(m.catSize) / float64(m.listed)
	}
	langs := []string{"en", "it"}
	cats := []string{"routing", "extraction", "injection"}
	var parts []string
	for _, l := range langs {
		parts = append(parts, fmt.Sprintf("%s %s", l, m.pct(m.byLang[l][0], m.byLang[l][1])))
	}
	for _, c := range cats {
		parts = append(parts, fmt.Sprintf("%s %s", c, m.pct(m.byCat[c][0], m.byCat[c][1])))
	}
	t.Logf("%-34s top1 %s | top3 %s | top5 %s | out_of_scope %s | mean shortlist %.2f of catalogue %.2f | decisive %d/%d | embed used %d, query calls %d, descriptor calls %d",
		m.name, m.pct(m.topK[1], m.capRows), m.pct(m.topK[3], m.capRows), m.pct(m.topK[5], m.capRows),
		m.pct(m.oosOK, m.oosRows), meanK, meanCat, m.decisive, m.listed, m.embedUsed, m.embedQ, m.embedD)
	t.Logf("%-34s   by language/category (top1): %s | cancel/help rows given a shortlist: %d/%d", "", strings.Join(parts, " | "), m.intentHit, m.intents)
}

func fakeEmbedder() *countingEmbedder {
	return &countingEmbedder{inner: fake.New(fake.WithEmbedDim(384))}
}

// TestRoutingCorpusAccuracy measures the router on the routing corpus: rules only
// on three packages, then the lexicon package with the optional embedder at a few
// similarity thresholds. The figures are logged (go test -v) and copied into
// docs/efficiency/M1-08.md; the assertions are floors that keep the rules-only
// stage from regressing, plus the properties that must hold at any accuracy.
func TestRoutingCorpusAccuracy(t *testing.T) {
	rows := loadCorpus(t)
	lex := loadLexicon(t)
	if len(rows) != 80 {
		t.Fatalf("corpus has %d rows; the denominators in the efficiency review assume 80", len(rows))
	}

	catalogue := runCorpus(t, rows, buildCorpusPackages(t, rows, lex, kindCatalogue), "rules / catalogue only", nil)
	lexicon := runCorpus(t, rows, buildCorpusPackages(t, rows, lex, kindLexicon), "rules / lexicon (first pass)", nil)
	tunedPk := buildCorpusPackages(t, rows, lex, kindTuned)
	tuned := runCorpus(t, rows, tunedPk, "rules / lexicon (tuned, upper bound)", nil)
	for _, m := range []*corpusMetrics{catalogue, lexicon, tuned} {
		m.log(t)
	}
	t.Logf("denominators: %d capability rows, %d out_of_scope rows, %d cancel/help rows", lexicon.capRows, lexicon.oosRows, lexicon.intents)
	for _, s := range lexicon.misses {
		t.Logf("lexicon top-1 miss: %s", s)
	}
	for _, s := range lexicon.oosMisses {
		t.Logf("lexicon out_of_scope miss: %s", s)
	}

	var withEmb []*corpusMetrics
	for _, min := range []float64{0.2, 0.3, 0.4, 0.5} {
		emb := fakeEmbedder()
		m := runCorpus(t, rows, buildCorpusPackages(t, rows, lex, kindLexicon),
			fmt.Sprintf("rules+fake embedder (min sim %.1f)", min),
			[]RouterOption{WithEmbedder(emb), WithConfig(Config{MinSimilarity: min})})
		m.embedQ, m.embedD = emb.query.Load(), emb.doc.Load()
		m.log(t)
		withEmb = append(withEmb, m)
	}
	{
		emb := fakeEmbedder()
		m := runCorpus(t, rows, buildCorpusPackages(t, rows, lex, kindCatalogue), "catalogue only + fake embedder (0.3)",
			[]RouterOption{WithEmbedder(emb), WithConfig(Config{MinSimilarity: 0.3})})
		m.embedQ, m.embedD = emb.query.Load(), emb.doc.Load()
		m.log(t)
	}

	// Denominators: 54 rows expect a capability, 22 expect out_of_scope, 4 are
	// dialogue intents (cancel, help) that no capability answers.
	if lexicon.capRows != 54 || lexicon.oosRows != 22 || lexicon.intents != 4 {
		t.Fatalf("denominators = %d/%d/%d, want 54/22/4", lexicon.capRows, lexicon.oosRows, lexicon.intents)
	}

	// What must hold whatever the accuracy.
	for _, m := range append([]*corpusMetrics{catalogue, lexicon, tuned}, withEmb...) {
		if m.listed > 0 && float64(m.shortlist)/float64(m.listed) > float64(DefaultConfig().MaxShortlist) {
			t.Errorf("%s: mean shortlist %.2f exceeds the bound", m.name, float64(m.shortlist)/float64(m.listed))
		}
	}
	if lexicon.embedUsed != 0 || catalogue.embedUsed != 0 {
		t.Error("a rules-only run used an embedder")
	}

	// Floors for the rules-only stage, set a few points under the measured values
	// (docs/efficiency/M1-08.md): a change that loses accuracy has to say so.
	if got := lexicon.topK[1]; got < floorLexiconTop1 {
		t.Errorf("lexicon top-1 = %d/%d, floor %d", got, lexicon.capRows, floorLexiconTop1)
	}
	if got := lexicon.topK[3]; got < floorLexiconTop3 {
		t.Errorf("lexicon top-3 = %d/%d, floor %d", got, lexicon.capRows, floorLexiconTop3)
	}
	if got := lexicon.oosOK; got < floorLexiconOOS {
		t.Errorf("lexicon out_of_scope = %d/%d, floor %d", got, lexicon.oosRows, floorLexiconOOS)
	}
	if lexicon.topK[1] <= catalogue.topK[1] {
		t.Errorf("terminology does not help: lexicon top-1 %d vs catalogue-only %d", lexicon.topK[1], catalogue.topK[1])
	}
	if tuned.topK[1] < lexicon.topK[1] {
		t.Errorf("tuning lowered accuracy: %d < %d", tuned.topK[1], lexicon.topK[1])
	}
}

// Floors, in rows, for TestRoutingCorpusAccuracy. Set after the first run and
// recorded in docs/efficiency/M1-08.md.
const (
	floorLexiconTop1 = 45
	floorLexiconTop3 = 50
	floorLexiconOOS  = 12
)
