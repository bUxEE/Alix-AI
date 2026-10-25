package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRun runs the tool through its command line into a fresh directory and
// returns the directory and the .rag and .json files it wrote.
func writeRun(t *testing.T, extra ...string) (dir, rag, bench string) {
	t.Helper()
	dir = t.TempDir()
	args := append([]string{"run", "--embedder", "fake", "--corpus", corpusDir, "--out", dir, "--scale-sizes", "30,90", "--scale-small-scopes", "1", "--scale-queries", "3", "--scale-pooled", "4", "--scale-in-process", "--warmup", "1", "--tmp", t.TempDir()}, extra...)
	var stdout, stderr bytes.Buffer
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("rageval run exited %d:\n%s", code, stderr.String())
	}
	rags, _ := filepath.Glob(filepath.Join(dir, "*"+RAGExt))
	jsons, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(rags) != 1 || len(jsons) != 1 {
		t.Fatalf("the run wrote %v and %v, want one .rag and one .json", rags, jsons)
	}
	for _, want := range []string{"recall@", "MRR", "citation precision", "unsupported answers", "scale sweep", "corpus-wide"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("the summary has no %q:\n%s", want, stdout.String())
		}
	}
	return dir, rags[0], jsons[0]
}

func checkDir(t *testing.T, dir string) (code int, out string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code = run([]string{"check", dir}, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// mutate rewrites a JSON file through a map, so a test can change one value.
func mutate(t *testing.T, path string, f func(m map[string]any)) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	f(m)
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func at(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m = m[k].(map[string]any)
	}
	return m
}

// TestCheckPassesARealRunAndRejectsEveryTamperedValue: `rageval check` accepts
// what the tool writes and refuses a result whose aggregate, flag, label or
// companion has been changed by hand, each for its own reason.
func TestCheckPassesARealRunAndRejectsEveryTamperedValue(t *testing.T) {
	dir, rag, bench := writeRun(t)
	if code, out := checkDir(t, dir); code != 0 || !strings.Contains(out, "ok - 1 result file") {
		t.Fatalf("check of an untouched run: exit %d\n%s", code, out)
	}
	pristineRag, _ := os.ReadFile(rag)
	pristineBench, _ := os.ReadFile(bench)
	restore := func() {
		_ = os.WriteFile(rag, pristineRag, 0o600)
		_ = os.WriteFile(bench, pristineBench, 0o600)
	}

	tests := []struct {
		name   string
		tamper func(t *testing.T)
		want   string
	}{
		{"a headline count", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) {
				at(m, "metrics", "correct")["passed"] = at(m, "metrics", "correct")["passed"].(float64) + 1
			})
		}, "metrics do not match the questions"},
		{"a per-kind block", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) { at(m, "by_kind", "identifier")["leaks"] = 3.0 })
		}, "by_kind does not match"},
		{"the leak flag of a question", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) {
				q := m["questions"].([]any)[0].(map[string]any)
				q["forbidden_hits"] = 2.0
			})
		}, "leak is false but"},
		{"the correct flag of a question", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) {
				q := m["questions"].([]any)[0].(map[string]any)
				q["correct"] = !q["correct"].(bool)
			})
		}, "correct is"},
		{"a ranked list", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) {
				for _, qi := range m["questions"].([]any) {
					q := qi.(map[string]any)
					if q["outcome"] == "answer" && len(q["ranked"].([]any)) > 1 {
						q["ranked"] = []any{[]any{}}
						return
					}
				}
			})
		}, "metrics do not match the questions"},
		{"the emulated label", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) { m["emulated"] = false })
		}, "emulated is false"},
		{"the synthetic label", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) { m["synthetic"] = false })
		}, "synthetic is false"},
		{"the container limits", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) { at(m, "container_limits")["mem_mb"] = 1024.0 })
		}, "container_limits"},
		{"a stage of the retrieval pipeline", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) { at(m, "latency", "stages_ms", "fts")["p95"] = 12345.0 })
		}, "latency does not match"},
		{"the embedder of the sweep", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) { at(m, "scale")["embedder"] = "localai" })
		}, "the sweep embedded with localai"},
		{"the pinned scopes of the sweep", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) { at(m, "scale")["pin_scopes"] = 1.0 })
		}, "expected 1"},
		{"the label of a reranker that is not there", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) { at(m, "components", "reranker")["synthetic"] = true })
		}, "labelled synthetic"},
		{"a container that was not under the profile's cap", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) {
				m["container_memory"] = map[string]any{
					"sampled": true, "source": "docker-stats", "container": "alixai-localai", "interval_ms": 2000.0, "samples": 3.0,
					"sample_errors": 0.0, "baseline_mb": 100.0, "peak_rss_mb": 120.0, "steady_rss_mb": 110.0, "limit_mb_observed": 4096.0,
				}
			})
		}, "not under the limit"},
		{"an ingestion rate", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) {
				in := at(m, "ingestion")
				in["chunks_per_second"] = in["chunks_per_second"].(float64) * 2
			})
		}, "ingestion elapsed time and rates"},
		{"a latency percentile", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) { at(m, "latency", "end_to_end_ms")["p95"] = 0.001 })
		}, "latency does not match"},
		{"a scale sample", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) {
				b := at(m, "scale")["buckets"].([]any)[0].(map[string]any)
				s := at(b, "samples_ms")["short"].([]any)
				s[0] = s[0].(float64) + 500
			})
		}, "latency_ms does not match the samples"},
		{"the scale shape", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) { at(m, "scale", "shape")["corpus_chunks"] = 7.0 })
		}, "corpus chunks"},
		{"a key the schema does not know", func(t *testing.T) {
			mutate(t, rag, func(m map[string]any) { m["extra"] = true })
		}, "schema:"},
		{"the companion", func(t *testing.T) {
			mutate(t, bench, func(m map[string]any) { m["duration_ms"] = 1.0 })
		}, "digest"},
		{"the companion with its digest fixed up", func(t *testing.T) {
			mutate(t, bench, func(m map[string]any) {
				c := m["cases"].([]any)[0].(map[string]any)
				c["pass"] = !c["pass"].(bool)
			})
			b, _ := os.ReadFile(bench)
			mutate(t, rag, func(m map[string]any) { at(m, "companion")["sha256"] = sha256Hex(b) })
		}, "companion"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restore()
			tc.tamper(t)
			code, out := checkDir(t, dir)
			if code == 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("check exit %d, want a failure mentioning %q:\n%s", code, tc.want, out)
			}
		})
	}
	restore()
	if code, out := checkDir(t, dir); code != 0 {
		t.Fatalf("check after restoring: exit %d\n%s", code, out)
	}
	// A result without its companion is not a result.
	if err := os.Remove(bench); err != nil {
		t.Fatal(err)
	}
	if code, out := checkDir(t, dir); code == 0 || !strings.Contains(out, "companion") {
		t.Errorf("check without the companion: exit %d\n%s", code, out)
	}
}

func TestCheckFindsNothingIsAnError(t *testing.T) {
	if code, _ := checkDir(t, t.TempDir()); code != 1 {
		t.Errorf("an empty directory passed: exit %d", code)
	}
	if code, _ := checkDir(t, filepath.Join(t.TempDir(), "missing")); code != 1 {
		t.Errorf("a missing directory passed: exit %d", code)
	}
	// `bench check` must not pick the .rag up (it validates every *.json), and
	// `rageval check` must not pick the .json up.
	dir, _, _ := writeRun(t)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("a stray file %s was left behind", e.Name())
		}
	}
}

func TestRejectSyntheticFlag(t *testing.T) {
	dir, _, _ := writeRun(t)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"check", "-reject-synthetic", dir}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "synthetic") {
		t.Errorf("-reject-synthetic: exit %d\n%s", code, stdout.String())
	}
}

// TestRunCommandLine covers the usage errors and the modes of the command line.
func TestRunCommandLine(t *testing.T) {
	var out, errb bytes.Buffer
	for _, args := range [][]string{
		{},
		{"nope"},
		{"run"},
		{"run", "--embedder", "fake", "--corpus", corpusDir},
		{"run", "--embedder", "fake", "--out", "x"},
		{"run", "--embedder", "gpt", "--corpus", corpusDir, "--out", "x"},
		{"run", "--embedder", "fake", "--corpus", corpusDir, "--out", "x", "--mode", "magic"},
		{"run", "--embedder", "fake", "--corpus", corpusDir, "--out", "x", "--k", "a,b"},
		{"run", "--embedder", "fake", "--corpus", corpusDir, "--out", "x", "--profile", "huge"},
		{"run", "--embedder", "fake", "--corpus", corpusDir, "--out", "x", "--scale-sizes", "x"},
		{"run", "--embedder", "fake", "--corpus", corpusDir, "--out", "x", "--embedding-model", "granite"},
		{"run", "--embedder", "fake", "--corpus", corpusDir, "--out", "x", "--scale-pin-scopes", "-1"},
		{"run", "--embedder", "fake", "--corpus", corpusDir, "--out", "x", "--docker-interval", "0s"},
		{"run", "--embedder", "fake", "--corpus", corpusDir, "--out", "x", "stray"},
		{"check"},
	} {
		out.Reset()
		errb.Reset()
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("run(%v) exited %d, want 2 (usage): %s%s", args, code, out.String(), errb.String())
		}
	}
	// k above the package size is a failure of the run, with the reason.
	errb.Reset()
	if code := run([]string{"run", "--embedder", "fake", "--corpus", corpusDir, "--out", t.TempDir(), "--k", "1,9", "--scale-sizes", "none"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "k 9") {
		t.Errorf("k over the package size: exit %d: %s", code, errb.String())
	}
	// A corpus that does not load fails the run.
	errb.Reset()
	if code := run([]string{"run", "--embedder", "fake", "--corpus", t.TempDir(), "--out", t.TempDir(), "--scale-sizes", "none"}, &out, &errb); code != 1 {
		t.Errorf("an empty corpus: exit %d: %s", code, errb.String())
	}
}

// TestModesAndProfile: full-text only, rerank and another profile each write a
// result that says what it is and passes its own check.
func TestModesAndProfile(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		mode    string
		profile string
		served  string
	}{
		{[]string{"--mode", "fts"}, "fts", "lite", "fts_only"},
		{[]string{"--mode", "rerank", "--profile", "4g"}, "rerank", "experimental-4g", "hybrid"},
		{[]string{"--mode", "hybrid", "--profile", "balanced", "--vector-min-score", "0.2", "--k", "1,2"}, "hybrid", "balanced", "hybrid"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			dir, rag, _ := writeRun(t, tc.args...)
			if code, out := checkDir(t, dir); code != 0 {
				t.Fatalf("check: exit %d\n%s", code, out)
			}
			b, _ := os.ReadFile(rag)
			var r RAGResult
			if err := json.Unmarshal(b, &r); err != nil {
				t.Fatal(err)
			}
			if r.Config.Mode != tc.mode || r.Profile != tc.profile || !r.Emulated || !r.Synthetic {
				t.Errorf("mode %s profile %s emulated %v synthetic %v", r.Config.Mode, r.Profile, r.Emulated, r.Synthetic)
			}
			if r.Modes.Served[tc.served] != r.Corpus.Questions {
				t.Errorf("served %v, want every question %s", r.Modes.Served, tc.served)
			}
			if r.Scale == nil || r.Scale.Mode != tc.mode || r.Scale.Isolated {
				t.Errorf("scale %+v", r.Scale)
			}
			if tc.mode == "fts" && (r.Components.Embedder.Kind != "none" || r.Index.Vectors != 0 || r.Scale.Build.Vectors != 0) {
				t.Errorf("an fts run has an embedder: %+v, %d vectors", r.Components.Embedder, r.Index.Vectors)
			}
			if tc.mode == "rerank" && (r.Components.Reranker.Kind != "lexical" || !r.Config.Query.Rerank) {
				t.Errorf("a rerank run has no reranker: %+v", r.Components.Reranker)
			}
			if tc.args[len(tc.args)-1] == "1,2" && len(r.Metrics.Retrieval.K) != 2 {
				t.Errorf("k = %v", r.Metrics.Retrieval.K)
			}
		})
	}
}

// TestRunNeverOverwrites: a result is never overwritten.
func TestRunNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	if _, err := writeNew(dir, "a.rag", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := writeNew(dir, "a.rag", []byte("y")); err == nil {
		t.Error("an existing result was overwritten")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.rag")); string(b) != "x" {
		t.Errorf("content %q", b)
	}
}
