package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// TestCorpusValid checks the shipped corpus, tests/performance/corpus/routing-v0.jsonl:
// every line satisfies the corpus-case schema and the semantic rules, the
// expected proposals are valid TaskProposals under the real contract schema,
// the corpus is big and varied enough to mean something, and nothing in it
// looks like personal data.
func TestCorpusValid(t *testing.T) {
	c := realCorpus(t)
	_, _, caseSchema := realSchemas(t)

	if c.ID != "routing-v0" {
		t.Errorf("corpus id = %q, want routing-v0", c.ID)
	}
	if len(c.SHA256) != 64 {
		t.Errorf("corpus hash %q is not a SHA-256", c.SHA256)
	}
	if len(c.Cases) < 60 {
		t.Fatalf("the corpus holds %d cases, want at least 60", len(c.Cases))
	}

	type cell struct{ category, language string }
	cells := map[cell]int{}
	ids := map[string]bool{}
	texts := map[string]bool{}
	for _, cs := range c.Cases {
		cells[cell{cs.Category, cs.Language}]++
		if ids[cs.ID] {
			t.Errorf("duplicate id %s", cs.ID)
		}
		ids[cs.ID] = true
		if texts[cs.Language+"|"+cs.Text] {
			t.Errorf("%s: the same text appears twice in %s", cs.ID, cs.Language)
		}
		texts[cs.Language+"|"+cs.Text] = true
	}
	for _, cat := range Categories {
		for _, lang := range Languages {
			if n := cells[cell{cat, lang}]; n < 5 {
				t.Errorf("category %s in %s has %d cases, want at least 5", cat, lang, n)
			}
		}
	}

	t.Run("every line satisfies the corpus-case schema", func(t *testing.T) {
		for _, cs := range c.Cases {
			raw, err := json.Marshal(cs)
			if err != nil {
				t.Fatal(err)
			}
			inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			if err := caseSchema.Validate(inst); err != nil {
				t.Errorf("%s: %v", cs.ID, err)
			}
		}
	})

	t.Run("expected proposals are valid TaskProposals under the contract", func(t *testing.T) {
		// The full contract schema, trace_id included: the core attaches it, so
		// the test does too. This ties the corpus to schemas/conversation.
		full, err := compileFile(repoPath(t, "schemas"), proposalSchemaFile)
		if err != nil {
			t.Fatal(err)
		}
		for _, cs := range c.Cases {
			doc := map[string]any{
				"capability_id": cs.Expected.CapabilityID,
				"fields":        cs.Expected.Fields,
				"entity_refs":   []any{},
				"intent":        cs.Expected.Intent,
				"uncertainty":   map[string]any{"overall": 1},
				"trace_id":      "trc_01J0000000000000000000000B",
			}
			raw, _ := json.Marshal(doc)
			inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			if err := full.Validate(inst); err != nil {
				t.Errorf("%s: expected proposal violates schemas/conversation/task-proposal: %v", cs.ID, err)
			}
		}
	})

	t.Run("covers the behaviours the benchmark exists to measure", func(t *testing.T) {
		var outOfScope, cancel, help, injectionNotInCatalogue, injectionDelete, withToday int
		for _, cs := range c.Cases {
			switch cs.Expected.Intent {
			case IntentOutOfScope:
				outOfScope++
			case IntentCancel:
				cancel++
			case IntentHelp:
				help++
			}
			if cs.Today != "" {
				withToday++
			}
			if cs.Category == CatInjection {
				for _, id := range cs.Attack.ForbiddenCapabilities {
					if _, in := cs.capability(id); !in {
						injectionNotInCatalogue++
						break
					}
				}
				if strings.Contains(strings.ToLower(cs.Text), "delete all bookings") || strings.Contains(strings.ToLower(cs.Text), "cancella tutte le prenotazioni") {
					injectionDelete++
				}
			}
		}
		if outOfScope < 16 {
			t.Errorf("only %d out-of-scope cases", outOfScope)
		}
		if cancel == 0 || help == 0 {
			t.Errorf("cancel cases %d, help cases %d: both intents must be exercised", cancel, help)
		}
		if injectionNotInCatalogue < 10 {
			t.Errorf("only %d injection cases ask for a capability the catalogue lacks", injectionNotInCatalogue)
		}
		if injectionDelete < 2 {
			t.Errorf("the 'delete all bookings' injection must be present in English and Italian, found %d", injectionDelete)
		}
		if withToday < len(c.Cases)/2 {
			t.Errorf("only %d cases carry today's date", withToday)
		}
	})

	t.Run("no personal data", func(t *testing.T) {
		email := regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9.-]+)`)
		longDigits := regexp.MustCompile(`[0-9]{9,}`)
		allowed := regexp.MustCompile(`(^|\.)(example\.(com|org|net)|test|invalid|localhost)$`)
		for _, cs := range c.Cases {
			all := cs.Text
			if cs.Attack != nil {
				all += " " + strings.Join(cs.Attack.ForbiddenValues, " ")
			}
			for _, m := range email.FindAllStringSubmatch(all, -1) {
				if !allowed.MatchString(strings.ToLower(strings.TrimRight(m[1], "."))) {
					t.Errorf("%s: e-mail %q is not on a reserved domain", cs.ID, m[0])
				}
			}
			if longDigits.MatchString(all) {
				t.Errorf("%s: a run of nine or more digits could be a telephone, card or account number", cs.ID)
			}
			if strings.Contains(strings.ToLower(all), "http://") || strings.Contains(strings.ToLower(all), "https://") {
				t.Errorf("%s: contains a URL", cs.ID)
			}
		}
	})

	t.Run("relative dates resolve against the stated today", func(t *testing.T) {
		relative := []struct {
			words  []string
			offset int
		}{
			{[]string{"day after tomorrow", "dopodomani"}, 2},
			{[]string{"tomorrow", "domani"}, 1},
		}
		checked := 0
		for _, cs := range c.Cases {
			today, err := time.Parse("2006-01-02", cs.Today)
			if err != nil {
				continue
			}
			text := strings.ToLower(cs.Text)
			for _, r := range relative {
				if !containsAny(text, r.words) {
					continue
				}
				want := today.AddDate(0, 0, r.offset).Format("2006-01-02")
				found := false
				for _, v := range cs.Expected.Fields {
					if v == want {
						found = true
					}
				}
				if !found {
					t.Errorf("%s: the text says %v, so some field must be %s (today is %s); fields %v", cs.ID, r.words, want, cs.Today, cs.Expected.Fields)
				}
				checked++
				break // the longer phrase is listed first
			}
		}
		if checked < 4 {
			t.Errorf("only %d cases use relative dates", checked)
		}
	})
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func TestCorpusRejectsInvalidCases(t *testing.T) {
	const catalogue = `[{"id":"a.create","description":"Create","fields":[{"name":"n","type":"integer","required":true},{"name":"kind","type":"enum","required":false,"enum":["x","y"]}]},{"id":"a.list","description":"List","fields":[]}]`
	line := func(category, extra, expected string) string {
		return `{"id":"t-01","language":"en","category":"` + category + `","catalogue":` + catalogue + `,"text":"hello"` + extra + `,"expected":` + expected + `}`
	}
	task := `{"capability_id":"a.create","intent":"task","fields":{"n":3}}`
	tests := []struct {
		name string
		in   string
		want string // substring of the error; "" means the corpus must load
	}{
		{"valid extraction", line("extraction", "", task), ""},
		{"valid refusal", line("refusal", "", `{"capability_id":null,"intent":"out_of_scope","fields":{}}`), ""},
		{"valid injection", line("injection", `,"attack":{"forbidden_capabilities":["a.wipe"],"forbidden_values":[]}`, `{"capability_id":null,"intent":"out_of_scope","fields":{}}`), ""},
		{"unknown field", strings.Replace(line("routing", "", `{"capability_id":"a.list","intent":"task","fields":{}}`), `"text"`, `"colour":"red","text"`, 1), "unknown field"},
		{"capability not in the catalogue", line("extraction", "", `{"capability_id":"a.nope","intent":"task","fields":{"n":3}}`), "not in the catalogue"},
		{"field not declared", line("extraction", "", `{"capability_id":"a.create","intent":"task","fields":{"zzz":3}}`), "not declared"},
		{"integer given as a string", line("extraction", "", `{"capability_id":"a.create","intent":"task","fields":{"n":"3"}}`), "JSON number"},
		{"integer given as a fraction", line("extraction", "", `{"capability_id":"a.create","intent":"task","fields":{"n":3.5}}`), "not an integer"},
		{"enum value outside the list", line("extraction", "", `{"capability_id":"a.create","intent":"task","fields":{"n":3,"kind":"z"}}`), "not one of"},
		{"out of scope with a capability", line("refusal", "", `{"capability_id":"a.list","intent":"out_of_scope","fields":{}}`), "no capability and no fields"},
		{"refusal expecting a task", line("refusal", "", `{"capability_id":"a.list","intent":"task","fields":{}}`), "must expect intent out_of_scope"},
		{"extraction without fields", line("extraction", "", `{"capability_id":"a.list","intent":"task","fields":{}}`), "at least one field"},
		{"injection without an attack", line("injection", "", `{"capability_id":null,"intent":"out_of_scope","fields":{}}`), "attack is required"},
		{"attack on a routing case", line("routing", `,"attack":{"forbidden_capabilities":["a.wipe"],"forbidden_values":[]}`, `{"capability_id":"a.list","intent":"task","fields":{}}`), "forbidden otherwise"},
		{"attack names nothing", line("injection", `,"attack":{"forbidden_capabilities":[],"forbidden_values":[]}`, `{"capability_id":null,"intent":"out_of_scope","fields":{}}`), "names no forbidden"},
		{"expected capability is forbidden", line("injection", `,"attack":{"forbidden_capabilities":["a.list"],"forbidden_values":[]}`, `{"capability_id":"a.list","intent":"task","fields":{}}`), "also forbidden"},
		{"task without a capability", line("routing", "", `{"capability_id":null,"intent":"task","fields":{}}`), "needs a capability_id"},
		{"unknown language", strings.Replace(line("routing", "", `{"capability_id":"a.list","intent":"task","fields":{}}`), `"en"`, `"fr"`, 1), "language"},
		{"two lines with one id", line("routing", "", `{"capability_id":"a.list","intent":"task","fields":{}}`) + "\n" + line("routing", "", `{"capability_id":"a.list","intent":"task","fields":{}}`), "duplicate case id"},
		{"not json", "this is not json", "line 1"},
		{"two values on a line", line("routing", "", `{"capability_id":"a.list","intent":"task","fields":{}}`) + " {}", "more than one"},
		{"empty corpus", "\n\n", "no cases"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCorpus("t-corpus", []byte(tt.in+"\n"))
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("want a valid corpus, got %v", err)
			case tt.want != "" && err == nil:
				t.Fatalf("want an error containing %q, got none", tt.want)
			case tt.want != "" && !strings.Contains(err.Error(), tt.want):
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}

	t.Run("catalogue size", func(t *testing.T) {
		one := `{"id":"t-01","language":"en","category":"routing","catalogue":[{"id":"a.list","description":"List","fields":[]}],"text":"hi","expected":{"capability_id":"a.list","intent":"task","fields":{}}}`
		if _, err := ParseCorpus("t", []byte(one)); err == nil || !strings.Contains(err.Error(), "2 to 6") {
			t.Errorf("a one-capability catalogue must be refused, got %v", err)
		}
	})

	t.Run("corpus id", func(t *testing.T) {
		if _, err := ParseCorpus("Bad Name", []byte("{}")); err == nil {
			t.Error("a corpus id with a space and capitals must be refused")
		}
	})
}
