package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// notMeasured is the cell text of a figure that does not exist yet.
const notMeasured = "not measured"

// table renders a Markdown table whose last column is always the label.
type table struct {
	head []string
	rows [][]string
}

func newTable(head ...string) *table {
	return &table{head: append(append([]string(nil), head...), "Label")}
}

func (t *table) add(label string, cells ...string) {
	if len(cells) != len(t.head)-1 {
		panic(fmt.Sprintf("providercompare: row of %d cells for %d columns", len(cells), len(t.head)-1))
	}
	t.rows = append(t.rows, append(append([]string(nil), cells...), label))
}

func (t *table) String() string {
	var sb strings.Builder
	sb.WriteString("| " + strings.Join(t.head, " | ") + " |\n")
	sb.WriteString("|" + strings.Repeat("---|", len(t.head)) + "\n")
	for _, r := range t.rows {
		sb.WriteString("| " + strings.Join(r, " | ") + " |\n")
	}
	return sb.String()
}

// rpm prints a requests-per-minute limit; a profile that states none holds 0.
func rpm(n int) string {
	if n <= 0 {
		return "unspecified"
	}
	return fmt.Sprint(n)
}

func f1(x float64) string { return fmt.Sprintf("%.1f", x) }
func f3(x float64) string { return fmt.Sprintf("%.3f", x) }

func ptr1(p *float64, unit string) string {
	if p == nil {
		return notMeasured
	}
	return f1(*p) + unit
}

// CoreIdle is the recorded idle footprint of `alixai serve` from M0-15.
type CoreIdle struct {
	WorkingSetMiB float64
	PrivateMiB    float64
	BinaryBytes   int64
	Threads       int
	Found         bool
}

func loadCoreIdle(root string) CoreIdle {
	b, err := os.ReadFile(filepath.Join(root, "docs", "evidence", "m0-4gb", "core-footprint.json"))
	if err != nil {
		return CoreIdle{}
	}
	var d struct {
		Sample struct {
			WS      float64 `json:"working_set_mib"`
			Private float64 `json:"private_mib"`
			Threads int     `json:"threads"`
		} `json:"sample"`
		Binary int64 `json:"binary_bytes"`
	}
	if json.Unmarshal(b, &d) != nil {
		return CoreIdle{}
	}
	return CoreIdle{WorkingSetMiB: d.Sample.WS, PrivateMiB: d.Sample.Private, BinaryBytes: d.Binary, Threads: d.Sample.Threads, Found: true}
}

// Render writes the whole report as Markdown. fp may be nil (no footprint run supplied).
func Render(c *Comparison, fp *Footprint, core CoreIdle) string {
	var sb strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&sb, format, a...) }

	w("# Connected-mode benchmark and provider comparison (X-23)\n\n")
	w("Written by `go run ./tools/providercompare`. The 4 GB/lightweight profile is benchmarked separately in local and connected modes.\n\n")
	w("## 0. Read this first\n\n")
	w("- **No provider was called.** There is no real key, no real money and no external network in this report. Real provider numbers are owed to X-H02 (human-gated, `//go:build live`).\n")
	w("- **Two labels only.** `recorded` is read from an artefact in this repository (a shipped provider profile, a recorded wire fixture replayed through the real adapter, or a benchmark result of an earlier task). `emulated` is produced here: by the latency-injecting loopback stub, inside the emulated 4 GiB container, or computed from recorded inputs on an assumed turn. Every table row ends in its label; a figure that does not exist is written `%s`.\n", notMeasured)
	w("- **Local mode is reported first and separately, and its verdicts are unchanged.** Connected-mode numbers qualify nothing: they do not make the Lite profile or the 4 GB profile certifiable and they do not offset a failed local gate (external mode must never be used to hide that the local mode has failed a hardware gate).\n")
	w("- **Quality of the hosted models is `%s`.** What the replay measures is each adapter's wire path, never a model.\n\n", notMeasured)

	renderLocal(&sb, c)
	renderProviders(&sb, c)
	renderFootprint(&sb, fp, core)
	renderLimits(&sb)
	return sb.String()
}

func renderLocal(sb *strings.Builder, c *Comparison) {
	w := func(format string, a ...any) { fmt.Fprintf(sb, format, a...) }
	w("## 1. Local mode (recorded; unchanged by this task)\n\n")
	w("Source: `benchmarks/results/m5/` (M5-09), 80 cases of routing-v0 per file, real LocalAI v4.10.0 runs in cgroup-capped containers (the files themselves say `emulated`). Two files per model and profile are two runs (one without and one with the deterministic scope gate); the files do not say which, see `docs/evidence/m5-models/report.md`.\n\n")
	t := newTable("Model", "Profile", "Pass (of 80)", "Routing", "Refusal", "Injection", "Total p50 ms", "Total p95 ms", "Peak RSS MiB")
	for _, r := range c.Local {
		t.add(LabelRecorded, r.Model, r.Profile, r.Pass, r.Routing, r.Refusal, r.Injection, ptr1(r.P50MS, ""), ptr1(r.P95MS, ""), ptr1(r.PeakRSSMB, ""))
	}
	sb.WriteString(t.String())
	w("\n**Verdicts, quoted from the tasks that reached them (not re-derived here):**\n\n")
	w("- **M0-17** and **M0-21** (`docs/efficiency/M0-17.md`, `docs/efficiency/M0-21.md`): with a grammar-constrained gateway and then with the deterministic out-of-scope gate, no Lite controller candidate cleared the pre-registered bar.\n")
	w("- **M5-09** (`docs/efficiency/M5-09.md`, `docs/evidence/m5-models/report.md`): \"No candidate passes the Lite bar, for the fourth time, and the bar was not lowered.\" Lite stays uncertified and serves the deterministic path; the 4 GB profile is not promoted (Qwen3.5-0.8B has the only usable routing and sits at 2047 MiB against a 2048 MiB line).\n")
	w("- **No Lite controller qualified. Nothing in sections 2 and 3 changes that.**\n\n")
}

func renderProviders(sb *strings.Builder, c *Comparison) {
	w := func(format string, a ...any) { fmt.Fprintf(sb, format, a...) }
	w("## 2. Provider comparison (connected mode)\n\n")
	w("Four real adapters (`adapters/external/{openai,anthropic,gemini,meta}`), built the way the composition root builds them (egress allowlist dialer, placeholder credential), each replaying recorded wire fixtures from a loopback stub. The fixtures were written from each vendor's documented shapes; they are not recordings of a live call.\n\n")

	w("### 2.1 Capability (recorded: shipped provider profiles)\n\n")
	t := newTable("Provider", "Model", "Streaming", "Structured output", "Tools", "Strict args", "Parallel", "Max tools", "Remote MCP", "Native features", "Image gen", "Voice", "Max context toks", "Max output toks")
	for _, p := range c.Providers {
		f := p.Profile
		voice := "none"
		if f.Voice.STT || f.Voice.TTS || f.Voice.Realtime {
			voice = fmt.Sprintf("stt %s, tts %s, realtime %s", yesNo(f.Voice.STT), yesNo(f.Voice.TTS), yesNo(f.Voice.Realtime))
		}
		t.add(LabelRecorded, p.Provider, f.ModelID, yesNo(f.Streaming), f.StructuredOutput, yesNo(f.ToolCalling.Supported), yesNo(f.ToolCalling.StrictArguments),
			yesNo(f.ToolCalling.ParallelCalls), fmt.Sprint(f.ToolCalling.MaxTools), yesNo(f.RemoteMCP.Supported), join(f.NativeFeatures.Available), yesNo(f.ImageGeneration.Supported),
			voice, fmt.Sprint(f.Limits.MaxContextToks), fmt.Sprint(f.Limits.MaxOutputToks))
	}
	sb.WriteString(t.String())
	w("\nData handling, as the profiles record it (all four are `unknown` until a human reads the terms, X-H01):\n\n")
	t = newTable("Provider", "Retention", "Training use", "Residency", "Requests per min", "Retries (max attempts)")
	for _, p := range c.Providers {
		f := p.Profile
		t.add(LabelRecorded, p.Provider, f.Terms.Retention, f.Terms.TrainingUse, f.Region.Residency, rpm(f.RateLimits.RequestsPerMin), fmt.Sprint(f.RateLimits.Retry.MaxAttempts))
	}
	sb.WriteString(t.String())

	w("\n### 2.2 Cost\n\n")
	w("Prices are the recorded price tables of the shipped profiles (micro-units of the currency per million tokens), which are themselves \"as summarised, not verified\" in the efficiency reviews of X-19 to X-21. The costs below are **computed by the adapters** from those tables on an assumed turn, so they are `emulated`.\n\n")
	t = newTable("Provider", "Model", "Currency", "Input micro per 1M toks", "Output micro per 1M toks", "Price table effective on")
	for _, p := range c.Providers {
		f := p.Profile
		t.add(LabelRecorded, p.Provider, f.ModelID, f.Pricing.Currency, fmt.Sprint(f.Pricing.Prices.In.MicroUnits), fmt.Sprint(f.Pricing.Prices.Out.MicroUnits), f.Pricing.EffectiveOn)
	}
	sb.WriteString(t.String())
	w("\n")
	t = newTable("Provider", fmt.Sprintf("One turn (%d in, %d out)", referenceToksIn, referenceToksOut), "routing-v0 replay", "conversation-v1 replay", "rag-questions replay")
	for _, p := range c.Providers {
		cell := func(name string) string {
			r, ok := p.corpus(name)
			if !ok {
				return notMeasured
			}
			return fmt.Sprintf("%s (%d in, %d out toks)", micros(r.CostMicros, r.Currency), r.ToksIn, r.ToksOut)
		}
		t.add(LabelEmulated, p.Provider, micros(p.ReferenceTurnCostMicros, p.Currency), cell(corpusRouting), cell(corpusConversation), cell(corpusRAG))
	}
	sb.WriteString(t.String())
	w("\nToken counts of the replays are `providers.EstimateTokens` of the text sent and of the reply (what the stub reports as usage), so they are an estimate and the cost an estimate on top of it. The lowest cost is not the best provider: no quality figure exists to weigh it against.\n")

	w("\n### 2.3 Quality: %s\n\n", notMeasured)
	w("Hosted-model quality on routing-v0, conversation-v1 and the RAG questions is `%s` for every provider: it needs a real key (X-H02). The stub plays the corpus's own expected answer, so the replay below measures whether an adapter **carries a reply through unchanged** (wire fidelity: request accepted, reply decoded, JSON equal), not whether any model would write that reply.\n\n", notMeasured)
	t = newTable("Provider", "Corpus", "Cases", "Sent", "Not sent (decided locally)", "Decoded unchanged", "Refused by the adapter")
	for _, p := range c.Providers {
		for _, r := range p.Corpora {
			t.add(r.Label, p.Provider, r.Corpus, fmt.Sprint(r.Cases), fmt.Sprint(r.Sent), fmt.Sprint(r.NotSent), fmt.Sprint(r.Decoded), fmt.Sprint(r.Errors))
		}
	}
	sb.WriteString(t.String())
	w("\nconversation-v1 contributes the turns its own expectations assign to the model (`decided_by: model`); the other turns are decided by the client, a rule or the router and never reach a provider. For comparison, the same corpus on a real local model is in section 1 (routing-v0 only; conversation-v1 and the RAG answer evaluation were gated out there by protocol rule G).\n")

	w("\n### 2.4 Latency\n\n")
	w("Provider latency is `%s` (X-H02). What can be said is the cost the adapter and the egress dialer add on this development box against a loopback stub with **no** injected delay; it is the floor under any network latency and it is the same order for every provider because each adapter does the same kind of work.\n\n", notMeasured)
	t = newTable("Provider", "Corpus", "Adapter overhead p50 ms", "Adapter overhead p95 ms", "Mean request bytes")
	for _, p := range c.Providers {
		for _, r := range p.Corpora {
			t.add(r.Label, p.Provider, r.Corpus, f3(r.OverheadP50MS), f3(r.OverheadP95MS), f1(r.RequestBytesMean))
		}
	}
	sb.WriteString(t.String())
	w("\nRequest bytes differ because each wire format wraps the same prompt differently (the Responses, Messages, generateContent and Chat Completions shapes).\n")

	w("\n### 2.5 RAG excerpt handling\n\n")
	w("An excerpt travels only as an untrusted user-role part (never in the system instruction) and only for the cases that are answered; the 15 `decline` cases of the corpus are decided locally before generation and send nothing. This table checks that the excerpt reached the stub inside the request body. It does **not** test the data-egress policy itself: a `local_only` source sends nothing by `internal/dataegress`, tested in `internal/dataegress` and `tests/security/extprovider`, and the excerpts here are stand-ins built from each case's expected facts.\n\n")
	t = newTable("Provider", "Answer cases sent", "Declines not sent", "Excerpt in request body", "Excerpt as untrusted user part", "Reply states every fact")
	for _, p := range c.Providers {
		r, _ := p.corpus(corpusRAG)
		t.add(r.Label, p.Provider, fmt.Sprint(r.Sent), fmt.Sprint(r.NotSent), fmt.Sprint(r.ExcerptKept), fmt.Sprint(r.ExcerptUntrusted), fmt.Sprint(r.Decoded))
	}
	sb.WriteString(t.String())

	w("\n### 2.6 Failure behaviour (recorded fixtures replayed through the real adapter)\n\n")
	w("`Result` is `ok` or the typed error code the adapter returned. `Requests` is how many requests reached the stub (more than one means the adapter retried); `Backoffs` is how many waits it asked for. `Leak` is whether the error text carried the upstream body, a marker planted in it, or the key. No scenario has a second endpoint to fall back to: a request count above one is a retry of the same call, never another provider. Which codes the controller then maps to a Local fallback is the operator's policy (X-22), not the adapter's.\n\n")
	t = newTable("Provider", "Scenario", "Result", "Requests", "Backoffs", "Leak")
	for _, p := range c.Providers {
		for _, o := range p.Scenarios {
			t.add(o.Label, p.Provider, o.Scenario, o.Result, fmt.Sprint(o.Requests), fmt.Sprint(o.Backoffs), yesNo(o.Leaked))
		}
	}
	sb.WriteString(t.String())
	w("\nReading the matrix: no adapter retries a 5xx answer or a dropped connection (the call may be commit-capable, so a blind repeat is unsafe); a rate limit that names a wait is retried once after that wait; a rejected key and an exhausted quota are not retried; a provider-native tool, active media or a malformed body is refused as `schema_invalid` and never reaches the caller as data.\n\nStreaming is not replayed by this tool; each adapter's own conformance suite covers it (`go test ./adapters/external/...`).\n\n")
}

func renderFootprint(sb *strings.Builder, fp *Footprint, core CoreIdle) {
	w := func(format string, a ...any) { fmt.Fprintf(sb, format, a...) }
	w("## 3. Connected-mode footprint under the experimental 4 GB profile (emulated)\n\n")
	w("Separate from section 1: no local model is loaded here. The question is what the connected-mode control-plane pieces cost on a 4 GiB / 4 CPU machine when the model is remote.\n\n")
	w("**What was run.** The four real adapters, the egress dialer and four loopback stubs that add an injected network delay, in one process; optionally `alixai serve` (idle core: built-in defaults, no model, no provider configured) beside it in the same container. **What was not run:** a full `alixai serve` with external mode enabled, SQLite sessions, RAG indexes, the credential service or the audit log under load; those are outside this harness.\n\n")
	if core.Found {
		t := newTable("Item", "Value")
		t.add(LabelRecorded, "Idle `alixai serve` working set, Windows dev box (M0-15; not emulated, not a Linux RSS)", f1(core.WorkingSetMiB)+" MiB")
		t.add(LabelRecorded, "Idle `alixai serve` private bytes, same sample", f1(core.PrivateMiB)+" MiB")
		t.add(LabelRecorded, "`alixai` binary size, same build", fmt.Sprintf("%.1f MiB", float64(core.BinaryBytes)/miB))
		sb.WriteString(t.String())
		w("\n")
	}
	if fp == nil {
		w("No footprint run was supplied to this rendering (`--footprint file.json`): %s.\n\n", notMeasured)
		return
	}
	w("Profile `%s`: %d MiB, %.0f CPUs (`profile-definition`, as `tools/bench` defines it). This run: OS `%s/%s`, cgroup v2 present: %s. Injected network delay per call: %.0f ms plus up to %.0f ms of jitter (an **input** of the emulation, not a result). Providers in the run: %s. Core process started in this run: %s.\n\n",
		fp.Profile, fp.MemLimitMiB, fp.CPUs, fp.OS, fp.Arch, yesNo(fp.Cgroup.Present), fp.InjectedBaseMS, fp.InjectedJitterMS, strings.Join(fp.Providers, ", "), yesNo(fp.CoreStarted))
	if fp.Cgroup.Present {
		t := newTable("Container fact", "Value")
		t.add(LabelEmulated, "memory.max", f1(fp.Cgroup.MemMaxMiB)+" MiB")
		t.add(LabelEmulated, "memory.swap.max (0 means no swap)", f1(fp.Cgroup.SwapMaxMiB)+" MiB")
		t.add(LabelEmulated, "memory.swap.current at the end", f1(fp.Cgroup.SwapCurrent)+" MiB")
		t.add(LabelEmulated, "memory.peak at the end", f1(fp.Cgroup.PeakMiB)+" MiB")
		t.add(LabelEmulated, "oom_kill count", fmt.Sprint(fp.Cgroup.OOMKill))
		t.add(LabelEmulated, "cpu.max", fp.Cgroup.CPUMax)
		sb.WriteString(t.String())
		w("\n")
	}
	t := newTable("Phase", "Calls", "Errors", "Concurrency", "Wall ms", "Per-call p50 ms", "Per-call p95 ms", "Per-call max ms", "Added by AlixAI p50 ms", "Added by AlixAI p95 ms")
	for _, p := range fp.Phases {
		a50, a95 := notMeasured, notMeasured
		if p.AddedKnown {
			a50, a95 = f3(p.AddedP50MS), f3(p.AddedP95MS)
		}
		t.add(p.Label, p.Name, fmt.Sprint(p.Calls), fmt.Sprint(p.Errors), fmt.Sprint(p.Concurrency), f1(p.WallMS), f3(p.P50MS), f3(p.P95MS), f3(p.MaxMS), a50, a95)
	}
	sb.WriteString(t.String())
	w("\nPer-call time is the injected delay plus what AlixAI adds. \"Added by AlixAI\" is each call's time minus the delay the stub injected into that very call, known only when calls arrive one at a time (the concurrent phase does not attribute a delay to a call, so it is `%s`). It says nothing about a provider's real latency.\n\n", notMeasured)
	m := fp.Memory
	t = newTable("Memory metric (MiB)", "Baseline", "Peak")
	opt := func(x float64) string {
		if x < 0 {
			return notMeasured
		}
		return f1(x)
	}
	t.add(m.Label, "Harness process resident set (Linux VmRSS)", opt(m.BaselineSelfRSSMiB), opt(m.PeakSelfRSSMiB))
	t.add(m.Label, "Core process `alixai serve` resident set (Linux VmRSS)", opt(m.BaselineCoreRSSMiB), opt(m.PeakCoreRSSMiB))
	t.add(m.Label, "Whole container, cgroup memory.current", opt(m.BaselineCgroupMiB), opt(m.PeakCgroupMiB))
	t.add(m.Label, "Go heap in use (harness)", "-", f1(m.PeakGoHeapMiB))
	t.add(m.Label, "Go runtime memory obtained from the OS (harness)", "-", f1(m.PeakGoSysMiB))
	sb.WriteString(t.String())
	w("\nPeak goroutines in the harness: %d (%d samples).\n\n", m.PeakGoroutines, m.Samples)
	if m.HeadroomMiB >= 0 {
		verdict := "above"
		if m.HeadroomMiB < headroomFloorMiB {
			verdict = "BELOW"
		}
		w("Headroom at the container's highest reading: %.1f MiB of %d MiB, %s the %d MiB headroom floor of [the resource budgets](../../operations/resource-budgets.md) (`max(15%%, 1 GB)`). **This is the connected-mode control-plane pieces alone; it is not the 4 GB profile's headroom.** The operating system, the SQLite and RAG working sets under load, the audit log and the host application are not in it, and the 4 GB aspiration stays an aspiration (master plan 7, additive plan section 12: network latency, local RAG and simultaneous application work still need measurement).\n\n", m.HeadroomMiB, profile4GMiB, verdict, headroomFloorMiB)
	}
	for _, n := range fp.Notes {
		w("- %s\n", n)
	}
	if len(fp.Notes) > 0 {
		w("\n")
	}
	w("The same run, in the bench result envelope: `benchmarks/results/x/` (provider `fake`, `synthetic: true`, profile `experimental-4g`, `emulated: true`; checked by `go run ./tools/bench check benchmarks/results/x`).\n\n")
}

func renderLimits(sb *strings.Builder) {
	w := func(format string, a ...any) { fmt.Fprintf(sb, format, a...) }
	w("## 4. What this report does not show\n\n")
	w("- Hosted-model quality, latency under real network conditions, real cost, rate-limit behaviour of a live account: `%s`, owed to X-H02.\n", notMeasured)
	w("- Any certified figure. Every container number is a cgroup cap on a larger machine (`emulated`), never `certified`.\n")
	w("- Any change to the Local-mode verdicts of section 1.\n")
	w("- The data-egress policy, budgets and kill switch under load; the full `alixai serve` in External or Hybrid mode.\n\n")
	w("## 5. Reproduce\n\n")
	w("```\ngo run ./tools/providercompare --footprint docs/evidence/x-benchmarks/footprint-4g.json --out docs/evidence/x-benchmarks/report.md\nsh tools/providercompare/run-4g.sh      # the emulated 4 GiB container run: writes benchmarks/results/x/ and footprint-4g.json\ngo run ./tools/bench check benchmarks/results/x\ngo test ./tools/providercompare/...\n```\n\n")
	w("The container is `--memory 4g --memory-swap 4g --cpus 4 --network none`, not privileged, removed on exit. Nothing is pulled: the image is one already on the machine and the binaries are bind-mounted. No LocalAI is started, so no LocalAI lock is taken.\n")
}

// CheckLabels is the structural rule of the report: every table has a Label column, every
// data row's label is exactly `recorded`, `emulated` or `not measured`, and the report names
// all three words. It returns the problems it finds.
func CheckLabels(md string) []string {
	var problems []string
	lines := strings.Split(md, "\n")
	labelCol := -1
	inTable := false
	for i, ln := range lines {
		if !strings.HasPrefix(ln, "|") {
			inTable, labelCol = false, -1
			continue
		}
		cells := splitRow(ln)
		if !inTable {
			inTable = true
			labelCol = -1
			if len(cells) > 0 && cells[len(cells)-1] == "Label" {
				labelCol = len(cells) - 1
			} else {
				problems = append(problems, fmt.Sprintf("line %d: a table without a Label column", i+1))
			}
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(strings.ReplaceAll(ln, "|", "")), "---") {
			continue
		}
		if labelCol < 0 {
			continue
		}
		if len(cells) != labelCol+1 {
			problems = append(problems, fmt.Sprintf("line %d: %d cells for %d columns", i+1, len(cells), labelCol+1))
			continue
		}
		switch cells[labelCol] {
		case LabelRecorded, LabelEmulated, notMeasured:
		default:
			problems = append(problems, fmt.Sprintf("line %d: label %q is not recorded, emulated or %s", i+1, cells[labelCol], notMeasured))
		}
	}
	for _, word := range []string{LabelRecorded, LabelEmulated, notMeasured} {
		if !strings.Contains(md, word) {
			problems = append(problems, "the report never uses the word "+word)
		}
	}
	return problems
}

func splitRow(ln string) []string {
	ln = strings.TrimSpace(ln)
	ln = strings.TrimPrefix(ln, "|")
	ln = strings.TrimSuffix(ln, "|")
	parts := strings.Split(ln, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
