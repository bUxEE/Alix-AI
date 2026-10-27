package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func qualityCorpusDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "quality", "it")
}

// fakeQualityOpenAI returns canned Italian / JSON answers keyed by case markers in the prompt.
func fakeQualityOpenAI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		user := ""
		sys := ""
		for _, m := range body.Messages {
			switch m.Role {
			case "user":
				user = m.Content
			case "system":
				sys = m.Content
			}
		}
		content := qualityFakeReply(sys, user)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", jsonString(content))
			fmt.Fprintf(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": content}},
			},
		})
	}))
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func qualityFakeReply(sys, user string) string {
	blob := sys + "\n" + user
	switch {
	case strings.Contains(blob, "TaskProposal"):
		return `{"capability_id":null,"fields":{},"entity_refs":[],"intent":"out_of_scope","uncertainty":{"overall":0.9}}`
	case strings.Contains(blob, "AnalystStepProposal"):
		return `{"kind":"read","read":{"capability_id":"company.profile.read","arguments":{"company_id":"cmp_acme_01"}}}`
	case strings.Contains(user, "lasagna"):
		return "fuori ambito: la richiesta di ricette non riguarda verbali né governance aziendale."
	case strings.Contains(user, "decisioni del CdA"):
		return "in ambito: riguarda verbali e azioni di governance."
	case strings.Contains(blob, "budget_approvato") || strings.Contains(user, "45.000"):
		return "Il budget approvato è 45.000."
	case strings.Contains(user, "Dipendenti FTE") || strings.Contains(user, "fatturato consolidato"):
		return "I dipendenti FTE sono 86 e il fatturato consolidato 2025 è 1280000 euro."
	case strings.Contains(user, "retention") || strings.Contains(user, "registro trattamenti"):
		return "Decisioni: retention documenti a 7 anni; Rossi responsabile privacy. Azioni: Verdi aggiorna il registro entro ottobre; Bianchi forma 120 dipendenti."
	default:
		return "Il CdA ha approvato il budget IT di 45000 euro e l'apertura a Bologna. Bianchi prepara l'assunzione di 3 analisti. Prossima riunione a novembre."
	}
}

func TestLoadQualityCorpus(t *testing.T) {
	cases, err := LoadQualityCorpus(qualityCorpusDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 6 {
		t.Fatalf("want >=6 quality cases, got %d", len(cases))
	}
	kinds := map[string]bool{}
	for _, c := range cases {
		if c.Language != "it" {
			t.Fatalf("%s language=%q", c.ID, c.Language)
		}
		kinds[c.Kind] = true
	}
	for _, k := range []string{KindSummarise, KindExtract, KindFacts, KindDomain, KindJSONTask, KindJSONAnalyst, KindVerify} {
		if !kinds[k] {
			t.Fatalf("missing kind %s", k)
		}
	}
}

func TestItalianCheck(t *testing.T) {
	ok, ratio := italianCheck("Il budget approvato è di quarantacinquemila euro per l'anno prossimo.")
	if !ok {
		t.Fatalf("expected Italian OK, ratio=%.2f", ratio)
	}
	ok, _ = italianCheck("The budget is approved and we will hire three analysts next month for the board.")
	if ok {
		t.Fatal("expected English-heavy text to fail")
	}
}

func TestFactAccuracy(t *testing.T) {
	acc, probs := factAccuracy("Ci sono 86 dipendenti e fatturato 1280000.", QualityRef{
		MustContain: []string{"dipendent"}, MustNumbers: []string{"86", "1280000"},
		AllowedNumbers: []string{"86", "1280000"},
	})
	if acc != 1 || len(probs) != 0 {
		t.Fatalf("acc=%v probs=%v", acc, probs)
	}
	acc, probs = factAccuracy("Ci sono 99 dipendenti.", QualityRef{
		MustNumbers: []string{"86"}, AllowedNumbers: []string{"86"},
	})
	if acc >= 1 || len(probs) == 0 {
		t.Fatalf("want failure, acc=%v probs=%v", acc, probs)
	}
}

func TestScoreJSONTaskProposal(t *testing.T) {
	sch, err := LoadQualitySchemas("")
	if err != nil {
		t.Fatal(err)
	}
	ok, probs := scoreJSON(
		`{"capability_id":null,"fields":{},"entity_refs":[],"intent":"out_of_scope","uncertainty":{"overall":0.9}}`,
		sch.TaskProposal, "out_of_scope", "",
	)
	if !ok {
		t.Fatalf("valid task proposal failed: %v", probs)
	}
	ok, _ = scoreJSON(`{"intent":"task"}`, sch.TaskProposal, "", "")
	if ok {
		t.Fatal("invalid proposal should fail schema")
	}
}

func TestRunQualityAgainstFake(t *testing.T) {
	srv := fakeQualityOpenAI(t)
	defer srv.Close()
	cases, err := LoadQualityCorpus(qualityCorpusDir(t))
	if err != nil {
		t.Fatal(err)
	}
	sch, err := LoadQualitySchemas("")
	if err != nil {
		t.Fatal(err)
	}
	cfg := QualityConfig{
		Endpoint:   srv.URL + "/v1",
		APIKey:     "test-key",
		Model:      "fake-model",
		Corpus:     cases,
		Schemas:    sch,
		HTTPClient: srv.Client(),
	}
	rep, err := RunQuality(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Mode != "quality" || rep.Language != "it" {
		t.Fatalf("meta: %+v", rep)
	}
	if rep.Summary.N != len(cases) {
		t.Fatalf("n=%d want %d", rep.Summary.N, len(cases))
	}
	if rep.Summary.Passed < len(cases)-1 {
		t.Fatalf("expected most cases to pass on fake, passed=%d / %d cases=%+v",
			rep.Summary.Passed, rep.Summary.N, rep.Cases)
	}
	if rep.Summary.SchemaCases < 2 || rep.Summary.SchemaValidRate == nil || *rep.Summary.SchemaValidRate < 1 {
		t.Fatalf("schema summary: %+v", rep.Summary)
	}
	if rep.Summary.VerifierCases < 1 || rep.Summary.VerifierPassRate == nil || *rep.Summary.VerifierPassRate < 1 {
		t.Fatalf("verifier summary: %+v", rep.Summary)
	}
	dir := t.TempDir()
	if err := WriteQualityReports(dir, rep); err != nil {
		t.Fatal(err)
	}
}

func TestQualityUnauthorised(t *testing.T) {
	srv := fakeQualityOpenAI(t)
	defer srv.Close()
	cases, err := LoadQualityCorpus(qualityCorpusDir(t))
	if err != nil {
		t.Fatal(err)
	}
	sch, err := LoadQualitySchemas("")
	if err != nil {
		t.Fatal(err)
	}
	cfg := QualityConfig{
		Endpoint:   srv.URL + "/v1",
		APIKey:     "wrong",
		Model:      "fake",
		Corpus:     cases[:1],
		Schemas:    sch,
		HTTPClient: srv.Client(),
	}
	rep, err := RunQuality(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Summary.Passed != 0 || rep.Cases[0].Err == "" {
		t.Fatalf("want auth error case, got %+v", rep.Cases[0])
	}
}
