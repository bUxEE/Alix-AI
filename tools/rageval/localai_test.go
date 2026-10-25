package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/buxee/alix-ai/internal/providers/fake"
)

// mockLocalAI is a plain-HTTP loopback stand-in for the part of LocalAI the
// tool talks to: readiness, version, the model list and embeddings. It serves
// the fake embedder's vectors under a model name that is not the registry's, so
// the pinned-model path is exercised too.
func mockLocalAI(t *testing.T, embedCalls *atomic.Int64) *httptest.Server {
	t.Helper()
	const reported = "mock-embed-file.gguf"
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "v4.10.0"})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "mock-embed"}}})
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model != "mock-embed" || len(req.Input) == 0 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		embedCalls.Add(1)
		data := make([]map[string]any, len(req.Input))
		toks := 0
		for i, in := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": fake.EmbedVector(in, fakeDim)}
			toks += len(in) / 3
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "emb-1", "model": reported, "data": data,
			"usage": map[string]int{"prompt_tokens": toks, "total_tokens": toks},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRunWithAMockedLocalAI runs the tool against a loopback stand-in for
// LocalAI, which is as far as the live path can be tested here: the wiring of
// the adapter, the pinned model id, the labels of a non-fake run, and that the
// result passes its own check. The live run itself is M2-15's.
func TestRunWithAMockedLocalAI(t *testing.T) {
	var calls atomic.Int64
	srv := mockLocalAI(t, &calls)
	const model = "granite-embedding-97m-multilingual-r2-q4_k_m"

	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"run", "--embedder", "localai", "--embedding-model", model, "--localai-model", "mock-embed",
		"--endpoint", srv.URL, "--corpus", corpusDir, "--out", dir, "--scale-sizes", "none", "--tmp", t.TempDir()}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("rageval run exited %d:\n%s", code, stderr.String())
	}
	if calls.Load() == 0 {
		t.Fatal("LocalAI was never asked for an embedding")
	}
	if c, out := checkDir(t, dir); c != 0 {
		t.Fatalf("check: exit %d\n%s", c, out)
	}
	entries, _ := os.ReadDir(dir)
	var r RAGResult
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), RAGExt) {
			b, _ := os.ReadFile(dir + "/" + e.Name())
			if err := json.Unmarshal(b, &r); err != nil {
				t.Fatal(err)
			}
		}
	}
	switch {
	case r.Provider.Name != "localai" || r.Synthetic || !r.Emulated:
		t.Errorf("provider %s synthetic %v emulated %v", r.Provider.Name, r.Synthetic, r.Emulated)
	case r.Model.RegistryID != model || !r.Model.InRegistry || r.Model.RuntimeModelName == nil || *r.Model.RuntimeModelName != "mock-embed":
		t.Errorf("model %+v", r.Model)
	case r.Components.Embedder.Kind != "localai" || r.Components.Embedder.ReportedModel == nil || *r.Components.Embedder.ReportedModel != "mock-embed-file.gguf":
		t.Errorf("embedder %+v", r.Components.Embedder)
	case r.Backend.RuntimeVersion == nil || *r.Backend.RuntimeVersion != "v4.10.0" || r.Backend.Runtime == nil || *r.Backend.Runtime != "localai":
		t.Errorf("backend %+v", r.Backend)
	case r.Modes.Served["hybrid"] != r.Corpus.Questions:
		t.Errorf("served %v", r.Modes.Served)
	}
	if err := checkSynthetic(dir); err != nil {
		t.Error(err)
	}

	// Full-text only contacts no runtime at all, whatever --embedder says, and its
	// result is labelled as the fake's: nothing real took part.
	dir2 := t.TempDir()
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"run", "--embedder", "localai", "--mode", "fts", "--endpoint", "http://127.0.0.1:1",
		"--corpus", corpusDir, "--out", dir2, "--scale-sizes", "none", "--tmp", t.TempDir()}, &stdout, &stderr); code != 0 {
		t.Fatalf("an fts run with an unreachable LocalAI exited %d:\n%s", code, stderr.String())
	}
	if c, out := checkDir(t, dir2); c != 0 {
		t.Fatalf("check: exit %d\n%s", c, out)
	}
	// A LocalAI that is not there is a failed run, not a silent fallback to the fake.
	dir3 := t.TempDir()
	stderr.Reset()
	if code := run([]string{"run", "--embedder", "localai", "--endpoint", "http://127.0.0.1:1", "--corpus", corpusDir, "--out", dir3, "--scale-sizes", "none"}, &stdout, &stderr); code != 1 {
		t.Errorf("an unreachable LocalAI exited %d, want 1: %s", code, stderr.String())
	}
	if entries, _ := os.ReadDir(dir3); len(entries) != 0 {
		t.Errorf("a failed run left %d files", len(entries))
	}
}

// checkSynthetic confirms that -reject-synthetic lets a result of a real
// embedder through.
func checkSynthetic(dir string) error {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"check", "-reject-synthetic", dir}, &stdout, &stderr); code != 0 {
		return &checkError{stdout.String() + stderr.String()}
	}
	return nil
}

type checkError struct{ out string }

func (e *checkError) Error() string {
	return "check -reject-synthetic refused a non-synthetic result: " + e.out
}
