package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCorpusCoversWhatThePlanAsksFor loads the repository's corpus and checks
// that it has what the task asks of it: both languages, exact identifiers,
// tables, conflicting versions, deleted and unavailable sources, a second
// tenant and a restricted source, labelled questions of every kind.
func TestCorpusCoversWhatThePlanAsksFor(t *testing.T) {
	c := loadTestCorpus(t)
	if langs := c.Languages(); langs["en"] < 10 || langs["it"] < 10 {
		t.Errorf("questions per language: %v", langs)
	}
	kinds := map[string]int{}
	answerable, decline := 0, 0
	for _, q := range c.Questions {
		kinds[q.Kind]++
		if q.Outcome == OutcomeAnswer {
			answerable++
		} else {
			decline++
		}
	}
	for _, k := range []string{KindIdentifier, KindTable, KindSemantic, KindCrossLang, KindConflict, KindStale, KindUnavailable, KindDeleted, KindPermission, KindCrossTenant, KindUnanswerable} {
		if kinds[k] == 0 {
			t.Errorf("no question of kind %s", k)
		}
	}
	if answerable == 0 || decline == 0 {
		t.Errorf("answerable %d, to decline %d", answerable, decline)
	}
	states, versions, restricted, tenants := map[string]bool{}, 0, 0, map[string]bool{}
	for _, s := range c.Manifest.Sources {
		states[s.State] = true
		tenants[s.Tenant] = true
		if len(s.Versions) > 1 {
			versions++
		}
		if len(s.Readers) > 0 {
			restricted++
		}
	}
	for _, st := range []string{StateActive, StateStale, StateUnavailable, StateDeleted} {
		if !states[st] {
			t.Errorf("no source in state %s", st)
		}
	}
	if versions < 2 || restricted < 1 || len(tenants) < 2 {
		t.Errorf("sources with several versions %d, restricted %d, tenants %d", versions, restricted, len(tenants))
	}
	// A table is in the corpus as Markdown a chunker turns into table rows.
	tables := 0
	for _, s := range c.Manifest.Sources {
		for _, d := range s.Latest().Documents {
			if strings.Contains(c.Doc(d.File), "| --- |") {
				tables++
			}
		}
	}
	if tables < 3 {
		t.Errorf("%d documents with a table", tables)
	}
	if len(c.SHA256) != 64 || c.Files < 20 || c.Bytes == 0 {
		t.Errorf("digest %q, files %d, bytes %d", c.SHA256, c.Files, c.Bytes)
	}
	// Loading twice gives the same digest.
	c2, err := LoadCorpus(corpusDir)
	if err != nil || c2.SHA256 != c.SHA256 {
		t.Errorf("a second load: %v, digest %s vs %s", err, c2.SHA256, c.SHA256)
	}
}

// TestCorpusLoadIsStrict: a corpus whose labels have drifted from its documents,
// or that names something that is not there, does not load.
func TestCorpusLoadIsStrict(t *testing.T) {
	copyCorpus := func(t *testing.T) string {
		t.Helper()
		dst := filepath.Join(t.TempDir(), "corpus")
		if err := os.CopyFS(dst, os.DirFS(corpusDir)); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	edit := func(t *testing.T, dir, file, from, to string) {
		t.Helper()
		p := filepath.Join(dir, file)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), from) {
			t.Fatalf("%s has no %q", file, from)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(b), from, to, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name, file, from, to, want string
	}{
		{"anchor not in the document", "questions.jsonl", `"anchors":["AX-9000-B","24 months"]`, `"anchors":["AX-9000-B","25 months"]`, "anchor"},
		{"unknown question key", "questions.jsonl", `"kind":"identifier"`, `"kind":"identifier","oops":1`, "unknown field"},
		{"unknown manifest key", "corpus.json", `"format_version": 1`, `"format_version": 1, "oops": 1`, "unknown field"},
		{"dangling evidence source", "questions.jsonl", `"source":"acme-returns","doc":"returns-handbook","anchors":["RMA-48213"]`, `"source":"nowhere","doc":"returns-handbook","anchors":["RMA-48213"]`, "not defined"},
		{"principal of another tenant", "questions.jsonl", `"tenant":"globex","principal":"usr_carol","text":"What is the budget code`, `"tenant":"globex","principal":"usr_alice","text":"What is the budget code`, "not a principal"},
		{"unknown state", "corpus.json", `"state":"stale"`, `"state":"half-stale"`, "state"},
		{"missing document file", "corpus.json", `docs/acme-sla/sla-2024.md`, `docs/acme-sla/missing.md`, "does not exist"},
		{"bad question id", "questions.jsonl", `"id":"en-ident-01"`, `"id":"EN_IDENT_01"`, "bench case id"},
		{"a question to decline that carries facts", "questions.jsonl", `"kind":"deleted","tenant":"acme","principal":"usr_alice","text":"What were the opening hours of warehouse WH-Delta?","outcome":"decline",`, `"kind":"deleted","tenant":"acme","principal":"usr_alice","text":"What were the opening hours of warehouse WH-Delta?","outcome":"decline","facts":["x"],`, "decline"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyCorpus(t)
			edit(t, dir, tc.file, tc.from, tc.to)
			_, err := LoadCorpus(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadCorpus = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	if _, err := LoadCorpus(t.TempDir()); err == nil {
		t.Error("an empty directory loaded")
	}
}

func TestFoldAndContains(t *testing.T) {
	for _, tc := range []struct {
		hay, needle string
		want        bool
	}{
		{"Within 14   DAYS of delivery", "14 days", true},
		{"Alimentazione INSTABILE", "alimentazione instabile", true},
		{"Qualità e perché", "qualita e perche", true},
		{"l’aggiornamento", "l'aggiornamento", true},
		{"24 months", "25 months", false},
		{"anything", "", false},
	} {
		if got := containsFold(tc.hay, tc.needle); got != tc.want {
			t.Errorf("containsFold(%q, %q) = %v, want %v", tc.hay, tc.needle, got, tc.want)
		}
	}
}
