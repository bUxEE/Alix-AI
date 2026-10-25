package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRoot(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func outcomeOf(t *testing.T, p ProviderReport, name string) Outcome {
	t.Helper()
	o, ok := p.scenario(name)
	if !ok {
		t.Fatalf("%s: no scenario %s", p.Provider, name)
	}
	return o
}

// TestComparisonFromRecordedResults: the comparison is built by replaying the recorded fixtures
// through the four real adapters, and says what the fixtures say.
func TestComparisonFromRecordedResults(t *testing.T) {
	root := testRoot(t)
	cmp, err := Build(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmp.Providers) != 4 {
		t.Fatalf("%d providers, want 4", len(cmp.Providers))
	}
	// The reference turn (1,500 in, 200 out) priced by the adapters from the shipped price tables
	// equals the arithmetic the efficiency reviews of X-19 to X-21 recorded.
	wantTurn := map[string]int64{"openai": 775, "anthropic": 2500, "gemini": 950, "meta": 2725}
	wantCorpus := map[string][3]int{corpusRouting: {80, 80, 0}, corpusConversation: {84, 84, 0}, corpusRAG: {50, 35, 15}} // cases, sent, not sent
	for _, p := range cmp.Providers {
		if got := p.ReferenceTurnCostMicros; got != wantTurn[p.Provider] {
			t.Errorf("%s: reference turn costs %d micro-units, want %d", p.Provider, got, wantTurn[p.Provider])
		}
		if len(p.Scenarios) != len(scenarioSpecs()) {
			t.Errorf("%s: %d scenarios, want %d", p.Provider, len(p.Scenarios), len(scenarioSpecs()))
		}
		for _, o := range p.Scenarios {
			if o.Leaked {
				t.Errorf("%s/%s: the error text carried the upstream body or the key", p.Provider, o.Scenario)
			}
			if o.Label != LabelRecorded {
				t.Errorf("%s/%s: label %q", p.Provider, o.Scenario, o.Label)
			}
		}
		for scenario, want := range map[string]string{
			"text": "ok", "tool_call_proposal": "ok", "json_schema_answer": "ok", "rate_limited_then_ok": "ok",
			"auth_rejected": "permission_denied", "quota_exhausted": "resource_exhausted",
			"provider_native_tool": "schema_invalid", "active_media_in_reply": "schema_invalid", "malformed_reply": "schema_invalid",
			"server_error_persistent": "unavailable", "connection_dropped": "unavailable",
		} {
			if got := outcomeOf(t, p, scenario).Result; got != want {
				t.Errorf("%s/%s: %s, want %s", p.Provider, scenario, got, want)
			}
		}
		if o := outcomeOf(t, p, "rate_limited_then_ok"); o.Requests != 2 || o.Backoffs != 1 {
			t.Errorf("%s: a rate limit with a wait should be retried once, saw %d requests and %d waits", p.Provider, o.Requests, o.Backoffs)
		}
		for _, name := range []string{"auth_rejected", "quota_exhausted"} {
			if o := outcomeOf(t, p, name); o.Requests != 1 {
				t.Errorf("%s/%s: %d requests, want 1 (no retry)", p.Provider, name, o.Requests)
			}
		}
		if outcomeOf(t, p, "tool_call_proposal").ToolCalls != 1 {
			t.Errorf("%s: the tool call proposal was not carried through", p.Provider)
		}
		for corpus, want := range wantCorpus {
			r, ok := p.corpus(corpus)
			if !ok {
				t.Fatalf("%s: no %s run", p.Provider, corpus)
			}
			if r.Cases != want[0] || r.Sent != want[1] || r.NotSent != want[2] {
				t.Errorf("%s/%s: cases %d sent %d not sent %d, want %v", p.Provider, corpus, r.Cases, r.Sent, r.NotSent, want)
			}
			if r.Errors != 0 || r.Decoded != r.Sent {
				t.Errorf("%s/%s: %d refused, %d of %d decoded unchanged", p.Provider, corpus, r.Errors, r.Decoded, r.Sent)
			}
			if r.CostMicros <= 0 || r.Currency != "USD" {
				t.Errorf("%s/%s: cost %d %q", p.Provider, corpus, r.CostMicros, r.Currency)
			}
		}
		rag, _ := p.corpus(corpusRAG)
		if rag.ExcerptKept != rag.Sent || rag.ExcerptUntrusted != rag.Sent {
			t.Errorf("%s: excerpt kept in %d and untrusted in %d of %d answered RAG requests", p.Provider, rag.ExcerptKept, rag.ExcerptUntrusted, rag.Sent)
		}
		if !p.Profile.ToolCalling.Supported || p.Profile.Streaming != true {
			t.Errorf("%s: the profile lost tool calling or streaming", p.Provider)
		}
	}
	// The cheapest tier by this arithmetic is not asserted as "best": no quality figure exists.
	// The recorded local runs are raw results (benchmarks/results/m5) that the published source
	// tree does not carry; where the checkout has them, they must all be read.
	if _, err := os.Stat(filepath.Join(root, "benchmarks", "results", "m5")); err == nil && len(cmp.Local) == 0 {
		t.Fatal("no recorded local results were read")
	}
	for _, r := range cmp.Local {
		if r.Label != LabelRecorded || r.Model == "" || r.Pass == "" {
			t.Errorf("local row %+v", r)
		}
	}
}

// TestConnectedModeFootprintHarness: the harness drives the four real adapters through latency
// injecting stubs, builds a result envelope that is labelled emulated and synthetic, and reports
// the injected delay as a floor under the measured one.
func TestConnectedModeFootprintHarness(t *testing.T) {
	root := testRoot(t)
	const base = 15 * time.Millisecond
	res, fp, err := RunFootprint(context.Background(), FootprintOptions{
		Root: root, LatencyBase: base, LatencyJitter: 5 * time.Millisecond, Seed: 7, Warmup: 2, Limit: 12,
		Workers: 2, CallsPerWorker: 4, SampleEvery: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Provider.Name != "fake" || !res.Synthetic || !res.Emulated || res.Profile != "experimental-4g" {
		t.Errorf("envelope labels: provider %s synthetic %v emulated %v profile %s", res.Provider.Name, res.Synthetic, res.Emulated, res.Profile)
	}
	if res.ContainerLimits.MemMB != 4096 || res.ContainerLimits.CPUs != 4 || res.ContainerLimits.Source != "profile-definition" {
		t.Errorf("container limits %+v", res.ContainerLimits)
	}
	if res.Corpus.CasesRun != 12 || len(res.Cases) != 12 || res.Config.Limit == nil || *res.Config.Limit != 12 {
		t.Errorf("cases run %d, held %d, limit %v", res.Corpus.CasesRun, len(res.Cases), res.Config.Limit)
	}
	if res.Errors.Count != 0 || res.Metrics.Pass.Passed != 12 {
		t.Errorf("errors %d, passed %d of 12", res.Errors.Count, res.Metrics.Pass.Passed)
	}
	// The envelope's aggregates are the ones tools/bench would re-derive from the cases.
	if got, want := res.Metrics, Aggregate(res.Cases); !sameValue(got, want) {
		t.Errorf("metrics do not match the cases")
	}
	if want := summarizeLatency(res.Cases, res.Latency.WarmupFirstRequestMS); !sameValue(res.Latency, want) {
		t.Errorf("latency does not match the cases")
	}
	if res.Latency.TotalMS.P50 == nil || *res.Latency.TotalMS.P50 < float64(base.Milliseconds()) {
		t.Errorf("per-call p50 %v is below the injected base delay of %v", res.Latency.TotalMS.P50, base)
	}
	if res.Memory.Sampled {
		t.Errorf("the envelope claims docker-stats sampling it did not do")
	}
	if len(fp.Phases) != 2 || fp.Phases[0].Calls != 12 || fp.Phases[1].Calls != 8 || fp.Phases[1].Concurrency != 2 {
		t.Errorf("phases %+v", fp.Phases)
	}
	for _, ph := range fp.Phases {
		if ph.Errors != 0 || ph.Label != LabelEmulated {
			t.Errorf("phase %+v", ph)
		}
		if ph.P50MS < float64(base.Milliseconds()) {
			t.Errorf("phase %q p50 %.3f ms is below the injected base", ph.Name, ph.P50MS)
		}
	}
	if !fp.Phases[0].AddedKnown || fp.Phases[0].AddedP50MS < 0 || fp.Phases[0].AddedP50MS > fp.Phases[0].P50MS || fp.Phases[1].AddedKnown {
		t.Errorf("added time: sequential %+v, concurrent %+v", fp.Phases[0], fp.Phases[1])
	}
	if fp.Label != LabelEmulated || fp.MemLimitMiB != 4096 || fp.InjectedBaseMS != 15 {
		t.Errorf("footprint %+v", fp)
	}
	if fp.Memory.Samples < 2 || fp.Memory.PeakGoHeapMiB <= 0 || fp.Memory.PeakGoroutines < 2 {
		t.Errorf("memory %+v", fp.Memory)
	}
	if !fp.Cgroup.Present && !containsNote(fp.Notes, "not inside a cgroup") {
		t.Errorf("outside a container the run must say so: %v", fp.Notes)
	}
	// The evidence file round-trips.
	b, err := json.Marshal(fp)
	if err != nil {
		t.Fatal(err)
	}
	var back Footprint
	if err := json.Unmarshal(b, &back); err != nil || back.Profile != fp.Profile {
		t.Errorf("footprint does not round-trip: %v", err)
	}
}

func sameValue(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func containsNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// TestEmulatedAndRecordedLabelsPresent: the rendered report labels every table row recorded or
// emulated, writes the figures that do not exist as "not measured", and keeps Local mode first
// and apart from the connected sections.
func TestEmulatedAndRecordedLabelsPresent(t *testing.T) {
	root := testRoot(t)
	cmp, err := Build(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	_, fp, err := RunFootprint(context.Background(), FootprintOptions{
		Root: root, LatencyBase: 5 * time.Millisecond, Seed: 1, Warmup: 1, Limit: 4, Workers: 1, CallsPerWorker: 2, SampleEvery: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	md := Render(cmp, fp, loadCoreIdle(root))
	if problems := CheckLabels(md); len(problems) > 0 {
		t.Fatalf("label check: %s", strings.Join(problems, "; "))
	}
	for _, want := range []string{"## 1. Local mode", "## 2. Provider comparison", "## 3. Connected-mode footprint", "No Lite controller qualified", notMeasured, "X-H02", "never `certified`"} {
		if !strings.Contains(md, want) {
			t.Errorf("the report lacks %q", want)
		}
	}
	local, prov, foot := strings.Index(md, "## 1. Local mode"), strings.Index(md, "## 2. Provider comparison"), strings.Index(md, "## 3. Connected-mode footprint")
	if !(local >= 0 && local < prov && prov < foot) {
		t.Errorf("sections out of order: %d %d %d", local, prov, foot)
	}
	// A rendering without a footprint run says so instead of inventing numbers.
	if bare := Render(cmp, nil, CoreIdle{}); !strings.Contains(bare, "No footprint run was supplied") || len(CheckLabels(bare)) > 0 {
		t.Errorf("a report with no footprint run must say so and still be labelled")
	}
	// The lint itself refuses an unlabelled table and a wrong label.
	for name, bad := range map[string]string{
		"no label column": "| A | B |\n|---|---|\n| 1 | 2 |\n recorded emulated not measured",
		"wrong label":     "| A | Label |\n|---|---|\n| 1 | measured |\n recorded emulated not measured",
		"missing word":    "| A | Label |\n|---|---|\n| 1 | recorded |\n",
	} {
		if len(CheckLabels(bad)) == 0 {
			t.Errorf("%s: the lint accepted it", name)
		}
	}
}

// TestFixturesMatchTheirSources: the embedded wire fixtures are byte copies of the adapters'
// testdata (and of the providerconf fixtures for OpenAI), so a changed adapter fixture shows up here.
func TestFixturesMatchTheirSources(t *testing.T) {
	root := testRoot(t)
	sources := map[string]string{
		"openai":    filepath.Join(root, "tools", "providerconf", "fixtures", "openai"),
		"anthropic": filepath.Join(root, "adapters", "external", "anthropic", "testdata"),
		"gemini":    filepath.Join(root, "adapters", "external", "gemini", "testdata"),
		"meta":      filepath.Join(root, "adapters", "external", "meta", "testdata"),
	}
	for provider, dir := range sources {
		entries, err := fixtureFS.ReadDir("fixtures/" + provider)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			want, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Errorf("%s/%s: %v", provider, e.Name(), err)
				continue
			}
			got, _ := fixtureFS.ReadFile("fixtures/" + provider + "/" + e.Name())
			if !bytes.Equal(got, want) {
				t.Errorf("%s/%s differs from %s", provider, e.Name(), dir)
			}
			if bytes.Contains(got, []byte("sk-")) {
				t.Errorf("%s/%s looks like it holds a key", provider, e.Name())
			}
		}
	}
}

// TestStubListensOnLoopbackOnly: nothing here can reach or be reached from another host.
func TestStubListensOnLoopbackOnly(t *testing.T) {
	for _, p := range providerNames {
		s, closeFn, err := newSubject(p, latencyModel{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s.stub.URL, "http://127.0.0.1:") {
			t.Errorf("%s: stub at %s", p, s.stub.URL)
		}
		closeFn()
	}
	if _, err := newStub("nope", latencyModel{}); err == nil {
		t.Error("an unknown provider built a stub")
	}
}

func TestCLIUsageErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := cli([]string{"footprint"}, &out, &errb); code != 2 {
		t.Errorf("footprint with no flags exited %d, want 2", code)
	}
	if code := cli([]string{"--nope"}, &out, &errb); code != 2 {
		t.Errorf("an unknown flag exited %d, want 2", code)
	}
	if code := cli([]string{"labels"}, &out, &errb); code != 2 {
		t.Errorf("labels with no file exited %d, want 2", code)
	}
}

// TestLocalRowsFromRecordedResultFiles: a recorded local-mode result file becomes one labelled row
// with its fractions, latency and memory (a small result written here, so the reading does not
// depend on the raw results being in the checkout).
func TestLocalRowsFromRecordedResultFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "benchmarks", "results", "m5")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const result = `{"profile": "lite", "model": {"registry_id": "m-1"},
 "metrics": {"pass": {"passed": 3, "total": 80}, "capability_routing": {"passed": 4, "total": 54},
  "refusal_correctness": {"passed": 5, "total": 16}, "injection_resistance": {"passed": 6, "total": 10}},
 "latency": {"total_ms": {"p50": 100, "p95": 200}}, "memory": {"peak_rss_mb": 900}}`
	if err := os.WriteFile(filepath.Join(dir, "localai_m-1_lite_run_x.json"), []byte(result), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := loadLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Label != LabelRecorded || r.Model != "m-1" || r.Profile != "lite" || r.Pass != "3/80" || r.Routing != "4/54" || r.Refusal != "5/16" || r.Injection != "6/10" {
		t.Errorf("row %+v", r)
	}
}
