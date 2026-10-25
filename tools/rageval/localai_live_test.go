package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// The tests of the pieces M2-15 added to the live path, against the loopback
// mock of localai_test.go: the labels of a run that has a model, what it records
// about the model's cost, and the sweep that embeds with it.

// runMockedLocalAI runs the tool against the mock with the flags every such run
// needs and returns the exit code, the directory and the decoded result.
func runMockedLocalAI(t *testing.T, srv *httptest.Server, extra ...string) (code int, dir string, r RAGResult, stderr string) {
	t.Helper()
	dir = t.TempDir()
	args := append([]string{"run", "--embedder", "localai", "--embedding-model", "granite-embedding-97m-multilingual-r2-q4_k_m",
		"--localai-model", "mock-embed", "--endpoint", srv.URL, "--corpus", corpusDir, "--out", dir, "--tmp", t.TempDir()}, extra...)
	var stdout, errb bytes.Buffer
	code = run(args, &stdout, &errb)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), RAGExt) {
			b, _ := os.ReadFile(dir + "/" + e.Name())
			if err := json.Unmarshal(b, &r); err != nil {
				t.Fatal(err)
			}
		}
	}
	return code, dir, r, errb.String()
}

// ragFile is the path of the one .rag file of dir.
func ragFile(t *testing.T, dir string) string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), RAGExt) {
			return dir + "/" + e.Name()
		}
	}
	t.Fatalf("no %s file in %s", RAGExt, dir)
	return ""
}

// TestLocalAIRerankIsLabelledSynthetic: no adapter serves the Reranker role, so
// `--embedder localai --mode rerank` runs the harness's lexical reranker. It is
// allowed only because the result says what it is: synthetic, in the full
// result and for -reject-synthetic, while the embedder is still the model's and
// the bench-envelope companion (whose schema ties synthetic to the fake
// provider) stays a localai result.
func TestLocalAIRerankIsLabelledSynthetic(t *testing.T) {
	var calls atomic.Int64
	srv := mockLocalAI(t, &calls)
	code, dir, r, stderr := runMockedLocalAI(t, srv, "--mode", "rerank", "--scale-sizes", "none")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if r.Provider.Name != "localai" || r.Components.Embedder.Kind != "localai" || r.Components.Reranker != (RerankerInfo{Kind: "lexical", Synthetic: true}) || !r.Synthetic {
		t.Errorf("provider %s embedder %s reranker %+v synthetic %v", r.Provider.Name, r.Components.Embedder.Kind, r.Components.Reranker, r.Synthetic)
	}
	if r.Config.Mode != ModeRerank || !r.Config.Query.Rerank {
		t.Errorf("mode %s, rerank %v", r.Config.Mode, r.Config.Query.Rerank)
	}
	if c, out := checkDir(t, dir); c != 0 {
		t.Fatalf("check: exit %d\n%s", c, out)
	}
	var stdout, errb bytes.Buffer
	if c := run([]string{"check", "-reject-synthetic", dir}, &stdout, &errb); c != 1 || !strings.Contains(stdout.String(), "synthetic") {
		t.Errorf("-reject-synthetic let a lexical-reranker result through: exit %d\n%s", c, stdout.String())
	}
	// Taking either label off by hand is caught.
	path := ragFile(t, dir)
	orig, _ := os.ReadFile(path)
	for name, f := range map[string]func(map[string]any){
		"the result's synthetic flag":   func(m map[string]any) { m["synthetic"] = false },
		"the reranker's synthetic flag": func(m map[string]any) { at(m, "components", "reranker")["synthetic"] = false },
	} {
		mutate(t, path, f)
		if c, out := checkDir(t, dir); c == 0 || !strings.Contains(out, "synthetic") {
			t.Errorf("%s: check exit %d\n%s", name, c, out)
		}
		_ = os.WriteFile(path, orig, 0o600)
	}
}

// TestLocalAIRunRecordsWhatTheModelCost covers the pieces of the first live use
// that a mock can check: the dimension the runtime answered with, the name it
// reported (kept next to the registry id), the query-embedding stage of every
// question, and the container memory (scripted here: the real source is docker).
func TestLocalAIRunRecordsWhatTheModelCost(t *testing.T) {
	var calls atomic.Int64
	srv := mockLocalAI(t, &calls)
	src := &scriptedSource{read: []MemSample{{180, 8192}, {270, 8192}}}
	old := newMemorySource
	newMemorySource = func(container, bin string) (MemorySource, error) {
		if container != "alixai-localai" {
			t.Errorf("sampled container %q", container)
		}
		return src, nil
	}
	t.Cleanup(func() { newMemorySource = old })

	code, dir, r, stderr := runMockedLocalAI(t, srv, "--scale-sizes", "none", "--docker-container", "alixai-localai", "--docker-interval", "5ms")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	e := r.Components.Embedder
	if e.Dimensions == nil || *e.Dimensions != fakeDim || e.ReportedModel == nil || *e.ReportedModel != "mock-embed-file.gguf" || e.Model == nil || *e.Model != r.Model.RegistryID {
		t.Errorf("embedder %+v: the dimension, the reported name and the registry id must all be there", e)
	}
	if st, ok := r.Latency.Stages["embed"]; !ok || st.N != r.Corpus.Questions {
		t.Errorf("embed stage %+v over %d questions", r.Latency.Stages["embed"], r.Corpus.Questions)
	}
	for _, q := range r.Questions {
		for _, name := range stageNames {
			if _, ok := q.StagesMS[name]; !ok {
				t.Fatalf("question %s has no %s stage", q.ID, name)
			}
		}
	}
	cm := r.ContainerMemory
	if !cm.Sampled || cm.Source != MemSourceDocker || *cm.Container != "alixai-localai" || cm.Samples < 2 || *cm.PeakRSSMB != 270 || *cm.BaselineMB != 180 || *cm.LimitMBObserved != 8192 {
		t.Errorf("container memory %+v", cm)
	}
	if !strings.Contains(r.Isolation.Note, "container") {
		t.Errorf("isolation note %q does not say a container was sampled", r.Isolation.Note)
	}
	if c, out := checkDir(t, dir); c != 0 {
		t.Fatalf("check: exit %d\n%s", c, out)
	}
	// The companion carries the same memory block, so `bench check` applies its
	// 1% rule on the limit to it.
	var companion BenchResult
	entries, _ := os.ReadDir(dir)
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".json") {
			b, _ := os.ReadFile(dir + "/" + en.Name())
			if err := json.Unmarshal(b, &companion); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !reflect.DeepEqual(companion.Memory, cm) {
		t.Errorf("companion memory %+v, result %+v", companion.Memory, cm)
	}

	// A container that was not under the profile's cap is not a lite result: the
	// tool refuses to write it.
	src2 := &scriptedSource{read: []MemSample{{180, 4096}}}
	newMemorySource = func(string, string) (MemorySource, error) { return src2, nil }
	code, dir2, _, stderr := runMockedLocalAI(t, srv, "--scale-sizes", "none", "--docker-container", "alixai-localai")
	if code != 1 || !strings.Contains(stderr, "not under the limit") {
		t.Errorf("a 4 GiB container labelled lite: exit %d\n%s", code, stderr)
	}
	if entries, _ := os.ReadDir(dir2); len(entries) != 0 {
		t.Errorf("a refused run left %d files", len(entries))
	}
}

// TestLocalAIScaleSweepEmbedsWithTheModel: a sweep in a localai run builds its
// vectors and embeds its queries with the runtime, not with the fake, and says
// so; a result whose sweep claims another embedder is refused.
func TestLocalAIScaleSweepEmbedsWithTheModel(t *testing.T) {
	var calls atomic.Int64
	srv := mockLocalAI(t, &calls)
	code, dir, r, stderr := runMockedLocalAI(t, srv, "--scale-sizes", "40", "--scale-queries", "2", "--scale-pooled", "2", "--scale-in-process")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	s := r.Scale
	if s == nil || s.Embedder != "localai" || s.Shape.EmbeddingDim == nil || *s.Shape.EmbeddingDim != fakeDim || s.Shape.Scopes != 1 || s.Shape.CorpusChunks != 40 {
		t.Fatalf("scale %+v", s)
	}
	if s.Build.Vectors != 40 || s.Build.EmbeddingMS <= 0 {
		t.Errorf("the sweep did not embed with the runtime: build %+v", s.Build)
	}
	if c, out := checkDir(t, dir); c != 0 {
		t.Fatalf("check: exit %d\n%s", c, out)
	}
	mutate(t, ragFile(t, dir), func(m map[string]any) { at(m, "scale")["embedder"] = "fake" })
	if c, out := checkDir(t, dir); c == 0 || !strings.Contains(out, "the sweep embedded with fake") {
		t.Errorf("a sweep that claims the fake in a localai run: exit %d\n%s", c, out)
	}
}

// With localai and no --scale-sizes the sweep is one 1,000-chunk scope (a model
// costs about 40 ms a chunk on the Lite cap), not the fake's 100,000 chunks.
func TestScaleDefaultsFollowTheEmbedder(t *testing.T) {
	var calls atomic.Int64
	srv := mockLocalAI(t, &calls)
	code, _, r, stderr := runMockedLocalAI(t, srv, "--scale-queries", "1", "--scale-pooled", "1", "--scale-in-process")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, stderr)
	}
	if r.Scale == nil || r.Scale.Shape.CorpusChunks != 1000 || r.Scale.Shape.Scopes != 1 || r.Scale.Embedder != "localai" {
		t.Errorf("scale %+v", r.Scale)
	}
}
