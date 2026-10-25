package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const examplesDir = "../../schemas/bench/examples/decision-result"

// exampleResult runs the fake scorer over a few cases of the corpus and pins everything
// that varies from run to run, so the committed examples are stable.
func exampleResult(t *testing.T, configs string, profile string, caseIDs ...string) *Result {
	t.Helper()
	full := loadTestCorpus(t)
	sub := &Corpus{SHA256: full.SHA256}
	for _, id := range caseIDs {
		for _, c := range full.Cases {
			if c.ID == id {
				sub.Cases = append(sub.Cases, c)
			}
		}
	}
	specs, _, err := SelectConfigs(configs, ScorerFake)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), RunOptions{Configs: specs, Scorer: ScorerFake, Corpus: sub, Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	r.RunID = "run_01HZX3Y5XQK7M2N4P6R8T0V1WA"
	r.Timestamp = "2026-10-12T09:00:00Z"
	r.DurationMS = 0
	r.Git = GitInfo{}
	r.Host = HostInfo{LogicalCores: 4, OS: "linux", Arch: "amd64"}
	return r
}

var exampleCases = []string{
	"dec-en-exact_command-01", "dec-it-exact_command-01", "dec-en-paraphrase-01", "dec-it-paraphrase-01",
	"dec-en-unauthorised-01", "dec-it-unauthorised-01", "dec-en-out_of_distribution-01", "dec-it-out_of_distribution-01",
}

// TestWriteResultExamples regenerates the examples when DECBENCH_WRITE_EXAMPLES=1.
func TestWriteResultExamples(t *testing.T) {
	if os.Getenv("DECBENCH_WRITE_EXAMPLES") != "1" {
		t.Skip("set DECBENCH_WRITE_EXAMPLES=1 to regenerate schemas/bench/examples/decision-result")
	}
	write := func(sub, name string, v any) {
		dir := filepath.Join(filepath.FromSlash(examplesDir), sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("valid", "fake-lite-all-configurations", exampleResult(t, "all", ProfileLite, exampleCases...))
	write("valid", "fake-4g-direct-controller-only", exampleResult(t, "4", Profile4G, exampleCases[:4]...))
	peak, resident, ctrl, audio := 900.0, 2600.0, 640.0, 180.0
	sup := exampleResult(t, "4", ProfileHigh, exampleCases[:4]...)
	sup.Live.PeakRSSMB, sup.Live.ResidentModelMB, sup.Live.FirstAudioMS = &peak, &resident, &audio
	sup.Live.MemoryReserveMB = round3(32768 - peak - resident)
	sup.Controller = ControllerInfo{Source: ControllerSupplied, LatencyMS: &ctrl}
	for i := range sup.Configurations[0].Cases {
		c := &sup.Configurations[0].Cases[i]
		c.FirstResponseMS = *round3(c.DecisionMS + ctrl)
		c.FirstAudioMS = round3(c.FirstResponseMS + audio)
	}
	sup.Configurations[0].Latency = summarizeLatency(sup.Configurations[0].Cases)
	write("valid", "fake-high-supplied-live-numbers", sup)

	// Invalid examples: each breaks one rule the schema states.
	base := func() map[string]any {
		b, _ := json.Marshal(exampleResult(t, "4", ProfileLite, exampleCases[:4]...))
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	cfg := func(m map[string]any) map[string]any { return m["configurations"].([]any)[0].(map[string]any) }
	cs := func(m map[string]any) map[string]any { return cfg(m)["cases"].([]any)[0].(map[string]any) }
	mutations := map[string]func(map[string]any){
		"emulated-false":            func(m map[string]any) { m["emulated"] = false },
		"format-version-2":          func(m map[string]any) { m["format_version"] = 2 },
		"bad-run-id":                func(m map[string]any) { m["run_id"] = "run_123" },
		"unknown-top-level-key":     func(m map[string]any) { m["certified"] = true },
		"unknown-profile":           func(m map[string]any) { m["profile"] = "certified" },
		"unknown-scorer-kind":       func(m map[string]any) { m["scorer"].(map[string]any)["kind"] = "openai" },
		"missing-live-block":        func(m map[string]any) { delete(m, "live") },
		"percentile-method-changed": func(m map[string]any) { m["config"].(map[string]any)["percentile_method"] = "linear" },
		"configuration-id-6":        func(m map[string]any) { cfg(m)["id"] = 6 },
		"no-configurations":         func(m map[string]any) { m["configurations"] = []any{} },
		"latency-with-a-mean": func(m map[string]any) {
			cfg(m)["latency"].(map[string]any)["decision_ms"].(map[string]any)["mean"] = 1.5
		},
		"negative-latency": func(m map[string]any) { cs(m)["decision_ms"] = -1 },
		"rate-above-one": func(m map[string]any) {
			cfg(m)["metrics"].(map[string]any)["task_completion"].(map[string]any)["rate"] = 1.5
		},
		"unknown-category":            func(m map[string]any) { cs(m)["category"] = "chat" },
		"unknown-language":            func(m map[string]any) { cs(m)["language"] = "fr" },
		"unknown-fallback-reason":     func(m map[string]any) { cs(m)["fallback_reason"] = "because" },
		"case-without-outcome":        func(m map[string]any) { delete(cs(m), "outcome_kind") },
		"confidence-above-one":        func(m map[string]any) { cs(m)["confidence"] = 1.2 },
		"controller-source-unknown":   func(m map[string]any) { m["controller"].(map[string]any)["source"] = "live" },
		"calibration-bins-changed":    func(m map[string]any) { cfg(m)["calibration"].(map[string]any)["bins"] = 20 },
		"cache-negative":              func(m map[string]any) { cfg(m)["cache"].(map[string]any)["hits"] = -1 },
		"corpus-hash-too-short":       func(m map[string]any) { m["corpus"].(map[string]any)["sha256"] = "abc" },
		"container-limits-not-number": func(m map[string]any) { m["container_limits"].(map[string]any)["cpus"] = "four" },
	}
	for name, mut := range mutations {
		m := base()
		mut(m)
		write("invalid", name, m)
	}
}

// TestResultExamplesBehaveAsDeclared checks the committed examples: every valid one passes
// the schema AND the aggregate re-derivation of `check`; every invalid one is rejected.
func TestResultExamplesBehaveAsDeclared(t *testing.T) {
	schema, err := LoadResultSchema("")
	if err != nil {
		t.Fatal(err)
	}
	for sub, wantValid := range map[string]bool{"valid": true, "invalid": false} {
		files, err := filepath.Glob(filepath.Join(filepath.FromSlash(examplesDir), sub, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("no %s examples (%v)", sub, err)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			ps := validateResultBytes(schema, b)
			if wantValid && len(ps) != 0 {
				t.Errorf("%s should be valid: %v", filepath.Base(f), ps)
			}
			if !wantValid {
				if len(ps) == 0 {
					t.Errorf("%s should be rejected", filepath.Base(f))
				} else if !strings.HasPrefix(ps[0], "schema:") && !strings.HasPrefix(ps[0], "not valid") {
					t.Errorf("%s should be rejected by the schema itself, got %s", filepath.Base(f), ps[0])
				}
			}
		}
	}
}
