package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
)

// TestWriteResultExamples regenerates the examples of schemas/bench/result.schema.json
// from scripted runs, so they are always consistent with what the harness
// writes and with `bench check`:
//
//	BENCH_WRITE_EXAMPLES=1 go test ./tools/bench -run TestWriteResultExamples
//
// It does nothing otherwise. TestSchemaExamples then holds every example to
// the schema and the semantic check.
func TestWriteResultExamples(t *testing.T) {
	if os.Getenv("BENCH_WRITE_EXAMPLES") == "" {
		t.Skip("set BENCH_WRITE_EXAMPLES=1 to regenerate schemas/bench/examples/result")
	}
	full := realCorpus(t)
	_, resultSchema, _ := realSchemas(t)
	sample := sampleCorpus(t, full, "rt-en-01", "rt-en-02", "rt-it-04", "ex-en-01", "ex-it-06", "rf-en-01", "rf-it-02", "in-en-01", "in-it-05", "in-en-08")

	example := func(provider, model string, profile models.Profile, mutate func(*RunConfig, *fake.Provider)) *Result {
		clk := clock.NewFake(time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC))
		p := newOracle(sample)
		lat := func(id string) (time.Duration, time.Duration) {
			i := time.Duration(indexOf(sample, id))
			ttft := (400 + 25*(i%5)) * time.Millisecond
			return ttft, ttft + (1200+30*(i%4))*time.Millisecond
		}
		gen := &timedGen{inner: p, clk: clk, latency: lat}
		cfg := testRunConfig(t, sample, gen, clk)
		cfg.Provider, cfg.Profile = provider, profile
		cfg.Host = HostInfo{CPUModel: strPtr("Example CPU @ 3.00GHz"), LogicalCores: 8, RAMMB: i64Ptr(32768), OS: "linux", Arch: "amd64"}
		cfg.Git = GitInfo{SHA: strPtr("0123456789abcdef0123456789abcdef01234567"), Dirty: boolPtr(false)}
		m, err := ResolveModel(context.Background(), provider, model, "", ifThen(provider == "localai", model, ""))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Model, cfg.Backend = m.Model, backendFor(provider, m.Entry, ifThen(provider == "localai", "v4.10.0", ""))
		cfg.ContextConfigured, cfg.NativeMax = m.ContextBudget, m.NativeMax
		if mutate != nil {
			mutate(&cfg, p)
		}
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		res.RunID = "run_01J0000000000000000000000A"
		return res
	}

	// 1. The fake provider: synthetic, perfect, streamed, with a warm-up.
	fakeRun := example("fake", "fake", models.ProfileLite, nil)

	// 2. A LocalAI run under lite with container memory and some failures.
	localRun := example("localai", "qwen3.5-0.8b-q4_k_m", models.ProfileLite, func(cfg *RunConfig, p *fake.Provider) {
		p.On(caseIs("rt-en-02")).Reply(proposalJSON(`"menu.search"`, "task", `{}`))
		p.On(caseIs("ex-it-06")).Reply(proposalJSON(`"tickets.create"`, "task", `{"subject":"VPN non funziona","priority":"urgent"}`))
		p.On(caseIs("rf-en-01")).Fail(fake.FailTimeout)
		p.On(caseIs("in-en-01")).Fail(fake.FailMalformedJSON)
		cfg.Timeout = 90 * time.Second
	})
	local := []float64{410.5, 1980.25, 2210.5, 2190.75, 2204}
	base, peak, steady := summarizeMemory(local, 2)
	localRun.Memory = MemoryInfo{
		Sampled: true, Source: MemSourceDocker, Container: strPtr("alixai-localai"), IntervalMS: i64Ptr(2000),
		Samples: len(local), SampleErrors: 0, BaselineMB: base, PeakRSSMB: peak, SteadyRSSMB: steady, LimitMBObserved: round3(8192),
	}

	// 3. Experimental 4 GB, not streamed, grammar constrained, router role.
	g4 := example("fake", "fake", models.ProfileExperimental4G, func(cfg *RunConfig, _ *fake.Provider) {
		cfg.Stream, cfg.Constraint, cfg.Role, cfg.Warmup = false, ConstraintGrammar, providers.RoleRouter, 0
	})

	valid := map[string]*Result{
		"fake-lite-streamed":                  fakeRun,
		"localai-lite-with-memory-and-errors": localRun,
		"fake-4g-not-streamed-grammar":        g4,
	}
	dir := filepath.Join(repoPath(t, "schemas/bench/examples/result"))
	resetDir(t, filepath.Join(dir, "valid"))
	resetDir(t, filepath.Join(dir, "invalid"))
	for name, res := range valid {
		data, err := marshalResult(res)
		if err != nil {
			t.Fatal(err)
		}
		if problems := validateResultBytes(resultSchema, data); len(problems) > 0 {
			t.Fatalf("example %s does not pass check: %v", name, problems)
		}
		writeFile(t, filepath.Join(dir, "valid", name+".json"), data)
	}

	// Invalid examples: the local run with one thing wrong each.
	baseDoc := func() map[string]any {
		data, _ := marshalResult(localRun)
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	invalid := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"missing-host", func(m map[string]any) { delete(m, "host") }},
		{"unknown-top-level-key", func(m map[string]any) { m["notes"] = "x" }},
		{"unknown-nested-key", func(m map[string]any) { setPath(t, m, 1, "metrics", "bonus") }},
		{"format-version-2", func(m map[string]any) { m["format_version"] = 2 }},
		{"bad-run-id", func(m map[string]any) { m["run_id"] = "run_123" }},
		{"timestamp-not-utc", func(m map[string]any) { m["timestamp"] = "2026-10-02T11:30:00+02:00" }},
		{"profile-not-in-the-registry-enum", func(m map[string]any) { m["profile"] = "4g" }},
		{"emulated-not-a-boolean", func(m map[string]any) { m["emulated"] = "yes" }},
		{"fake-provider-claims-real-results", func(m map[string]any) { setPath(t, m, "fake", "provider", "name"); m["synthetic"] = false }},
		{"rate-above-one", func(m map[string]any) { setPath(t, m, 1.2, "metrics", "pass", "rate") }},
		{"negative-count", func(m map[string]any) { setPath(t, m, -1, "metrics", "pass", "total") }},
		{"corpus-hash-too-short", func(m map[string]any) { setPath(t, m, "abc123", "corpus", "sha256") }},
		{"artifact-hash-without-source", func(m map[string]any) { setPath(t, m, "none", "model", "artifact_hash_source") }},
		{"unknown-constraint", func(m map[string]any) { setPath(t, m, "regex", "config", "constraint") }},
		{"percentile-method-changed", func(m map[string]any) { setPath(t, m, "linear", "config", "percentile_method") }},
		{"memory-sampled-without-a-container", func(m map[string]any) { setPath(t, m, nil, "memory", "container") }},
		{"memory-not-sampled-but-numbers-given", func(m map[string]any) { setPath(t, m, false, "memory", "sampled") }},
		{"no-cases", func(m map[string]any) { m["cases"] = []any{} }},
		{"case-with-unknown-language", func(m map[string]any) { setPath(t, m, "fr", "cases", 0, "language") }},
		{"error-case-with-output", func(m map[string]any) {
			for i, c := range m["cases"].([]any) {
				if c.(map[string]any)["status"] == "error" {
					setPath(t, m, "{}", "cases", i, "output")
				}
			}
		}},
		{"invalid-output-that-passes", func(m map[string]any) {
			for i, c := range m["cases"].([]any) {
				if c.(map[string]any)["schema_valid"] == false && c.(map[string]any)["status"] == "ok" {
					setPath(t, m, true, "cases", i, "pass")
				}
			}
		}},
		{"latency-percentile-negative", func(m map[string]any) { setPath(t, m, -1, "latency", "total_ms", "p50") }},
		{"limits-not-numbers", func(m map[string]any) { setPath(t, m, "8g", "container_limits", "mem_mb") }},
	}
	for _, tt := range invalid {
		m := baseDoc()
		tt.mutate(m)
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "invalid", tt.name+".json"), append(b, '\n'))
	}
}

func resetDir(t testing.TB, dir string) {
	t.Helper()
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t testing.TB, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func i64Ptr(v int64) *int64 { return &v }

func ifThen(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func indexOf(c *Corpus, id string) int {
	for i, cs := range c.Cases {
		if cs.ID == id {
			return i
		}
	}
	return 0
}

// sampleCorpus builds a small corpus from some cases of a bigger one.
func sampleCorpus(t testing.TB, full *Corpus, ids ...string) *Corpus {
	t.Helper()
	out := &Corpus{ID: full.ID + "-sample"}
	var lines []string
	for _, id := range ids {
		for _, c := range full.Cases {
			if c.ID == id {
				out.Cases = append(out.Cases, c)
				b, err := json.Marshal(c)
				if err != nil {
					t.Fatal(err)
				}
				lines = append(lines, string(b))
			}
		}
	}
	if len(out.Cases) != len(ids) {
		t.Fatalf("sample asked for %d cases, found %d", len(ids), len(out.Cases))
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	out.SHA256 = hex.EncodeToString(sum[:])
	return out
}
