package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fakeOpenAI serves a minimal streaming chat.completions response.
func fakeOpenAI(t *testing.T) *httptest.Server {
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
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// small delay so TTFT < Full
		time.Sleep(5 * time.Millisecond)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(5 * time.Millisecond)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":40}}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
}

func corpusDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	return filepath.Join(filepath.Dir(file), "testdata", "corpus")
}

func TestLoadCorpus(t *testing.T) {
	turns, err := LoadCorpus(corpusDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) < 4 {
		t.Fatalf("want >=4 turns, got %d", len(turns))
	}
	var sawEN, sawIT bool
	for _, tr := range turns {
		if tr.ApproxInputTokens < 3000 || tr.ApproxInputTokens > 6000 {
			t.Fatalf("turn %s approx_input_tokens=%d want 3000..6000", tr.ID, tr.ApproxInputTokens)
		}
		if tr.MaxTokens != 300 {
			t.Fatalf("turn %s max_tokens=%d", tr.ID, tr.MaxTokens)
		}
		if len(tr.Tools) == 0 {
			t.Fatalf("turn %s missing tools", tr.ID)
		}
		switch tr.Language {
		case "en":
			sawEN = true
		case "it":
			sawIT = true
		}
	}
	if !sawEN || !sawIT {
		t.Fatalf("need en and it turns (en=%v it=%v)", sawEN, sawIT)
	}
}

func TestRunAgainstFake(t *testing.T) {
	srv := fakeOpenAI(t)
	defer srv.Close()
	turns, err := LoadCorpus(corpusDir(t))
	if err != nil {
		t.Fatal(err)
	}
	// Use one short turn repeatedly for speed.
	turns = turns[:1]
	cfg := Config{
		Endpoint:      srv.URL + "/v1",
		APIKey:        "test-key",
		Model:         "fake-model",
		Corpus:        turns,
		Concurrencies: []int{1, 4},
		TargetFull:    2 * time.Second,
		Timeout:       5 * time.Second,
		HTTPClient:    srv.Client(),
	}
	rep, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Aggregates) != 2 {
		t.Fatalf("aggregates=%d", len(rep.Aggregates))
	}
	for _, a := range rep.Aggregates {
		if a.Errors != 0 {
			t.Fatalf("concurrency %d errors=%d", a.Concurrency, a.Errors)
		}
		if a.TTFT_P50_ms <= 0 || a.Full_P50_ms <= 0 {
			t.Fatalf("missing latency: %+v", a)
		}
		if a.Full_P50_ms < a.TTFT_P50_ms {
			t.Fatalf("full p50 < ttft p50: %+v", a)
		}
	}
	if rep.MaxConcurrentWithinTarget < 1 {
		t.Fatalf("expected at least concurrency 1 within target, got %d", rep.MaxConcurrentWithinTarget)
	}
	dir := t.TempDir()
	if err := WriteReports(dir, rep); err != nil {
		t.Fatal(err)
	}
}

func TestUnauthorisedFake(t *testing.T) {
	srv := fakeOpenAI(t)
	defer srv.Close()
	turns, err := LoadCorpus(corpusDir(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Endpoint:      srv.URL + "/v1",
		APIKey:        "wrong",
		Model:         "fake-model",
		Corpus:        turns[:1],
		Concurrencies: []int{1},
		TargetFull:    time.Second,
		HTTPClient:    srv.Client(),
	}
	rep, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Aggregates[0].Errors != 1 {
		t.Fatalf("want 1 error, got %+v", rep.Aggregates[0])
	}
}

func TestSummariseMaxConcurrent(t *testing.T) {
	samples := []Sample{
		{Concurrency: 1, Full: 100 * time.Millisecond, TTFT: 10 * time.Millisecond, CompletionTokens: 10},
		{Concurrency: 4, Full: 500 * time.Millisecond, TTFT: 20 * time.Millisecond, CompletionTokens: 10},
		{Concurrency: 16, Full: 5 * time.Second, TTFT: 50 * time.Millisecond, CompletionTokens: 10},
	}
	aggs := Summarise(samples, time.Second)
	if MaxConcurrentWithinTarget(aggs) != 4 {
		t.Fatalf("got %d aggs=%+v", MaxConcurrentWithinTarget(aggs), aggs)
	}
}
