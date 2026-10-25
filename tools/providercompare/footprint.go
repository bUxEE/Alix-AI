package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// The experimental 4 GB profile, as tools/bench defines it (deploy/container/localai/compose.4g.yaml).
const (
	profile4G     = "experimental-4g"
	profile4GMiB  = 4096
	profile4GCPUs = 4
	// headroomFloorMiB is master plan 7.4: at least max(15% of memory, 1 GB) stays free; for 4096 MiB
	// the 1 GB floor binds.
	headroomFloorMiB = 1024
)

// FootprintOptions configure one connected-mode footprint run.
type FootprintOptions struct {
	Root          string
	LatencyBase   time.Duration
	LatencyJitter time.Duration
	Seed          int64
	Warmup        int
	// Limit runs only the first N routing cases (0: all 80).
	Limit int
	// Workers and CallsPerWorker size the concurrent phase (0 skips it).
	Workers        int
	CallsPerWorker int
	SampleEvery    time.Duration
	// CoreBin, when set, is started as `alixai serve` (idle core, no model, no provider) inside the run
	// so the container holds the control plane process too. WorkDir must be writable then.
	CoreBin string
	WorkDir string
	// GitSHA and GitDirty fill the result's git block (the container has no git).
	GitSHA   string
	GitDirty *bool
}

// Phase is one stretch of the run.
type Phase struct {
	Name        string  `json:"name"`
	Calls       int     `json:"calls"`
	Errors      int     `json:"errors"`
	Concurrency int     `json:"concurrency"`
	WallMS      float64 `json:"wall_ms"`
	P50MS       float64 `json:"p50_ms"`
	P95MS       float64 `json:"p95_ms"`
	MaxMS       float64 `json:"max_ms"`
	// AddedP50MS and AddedP95MS are the per-call time minus the delay the stub injected into that
	// very call: what AlixAI added. Known only when calls arrive one at a time (AddedKnown).
	AddedKnown bool    `json:"added_known"`
	AddedP50MS float64 `json:"added_p50_ms"`
	AddedP95MS float64 `json:"added_p95_ms"`
	Label      string  `json:"label"`
}

// MemoryPeak is the highest reading of each metric over the whole run.
type MemoryPeak struct {
	Samples            int     `json:"samples"`
	BaselineSelfRSSMiB float64 `json:"baseline_self_rss_mib"`
	PeakSelfRSSMiB     float64 `json:"peak_self_rss_mib"`
	BaselineCoreRSSMiB float64 `json:"baseline_core_rss_mib"`
	PeakCoreRSSMiB     float64 `json:"peak_core_rss_mib"`
	BaselineCgroupMiB  float64 `json:"baseline_cgroup_mib"`
	PeakCgroupMiB      float64 `json:"peak_cgroup_mib"`
	PeakGoHeapMiB      float64 `json:"peak_go_heap_inuse_mib"`
	PeakGoSysMiB       float64 `json:"peak_go_sys_mib"`
	PeakGoroutines     int     `json:"peak_goroutines"`
	// HeadroomMiB is the profile's memory minus the highest cgroup reading, or -1 outside a container.
	HeadroomMiB float64 `json:"headroom_mib"`
	Label       string  `json:"label"`
}

// Footprint is the connected-mode footprint of the control plane, reported on its own, never
// merged into a Local-mode figure.
type Footprint struct {
	Task    string `json:"task"`
	Label   string `json:"label"`
	Profile string `json:"profile"`
	// MemLimitMiB and CPUs are what the profile stands for; Cgroup is what the container reports.
	MemLimitMiB int         `json:"mem_limit_mib"`
	CPUs        float64     `json:"cpus"`
	OS          string      `json:"os"`
	Arch        string      `json:"arch"`
	Cgroup      CgroupFacts `json:"cgroup"`
	// InjectedBaseMS and InjectedJitterMS are the stub's network delay: inputs, not results.
	InjectedBaseMS   float64     `json:"injected_base_ms"`
	InjectedJitterMS float64     `json:"injected_jitter_ms"`
	Providers        []string    `json:"providers"`
	CoreStarted      bool        `json:"core_started"`
	CoreNote         string      `json:"core_note"`
	Phases           []Phase     `json:"phases"`
	Memory           MemoryPeak  `json:"memory"`
	Samples          []MemSample `json:"samples"`
	Notes            []string    `json:"notes"`
}

// coreProc is the optional `alixai serve` child.
type coreProc struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	pid  string
	logs bytes.Buffer
}

func (c *coreProc) PID() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pid
}

// startCore starts the core binary with a minimal configuration (the one M0-15 measured) and waits
// until it answers /healthz on loopback.
func startCore(ctx context.Context, bin, workdir string) (*coreProc, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := filepath.Join(workdir, "core.yaml")
	body := fmt.Sprintf("server:\n  listen: %q\ndata_dir: %q\n", addr, filepath.ToSlash(filepath.Join(workdir, "data")))
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		return nil, err
	}
	c := &coreProc{}
	c.cmd = exec.Command(bin, "serve", "-config", cfg)
	c.cmd.Stdout, c.cmd.Stderr = &c.logs, &c.logs
	if err := c.cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the core: %w", err)
	}
	c.mu.Lock()
	c.pid = fmt.Sprint(c.cmd.Process.Pid)
	c.mu.Unlock()
	deadline := time.Now().Add(30 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			c.stop()
			return nil, ctx.Err()
		default:
		}
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return c, nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	out := c.logs.String()
	c.stop()
	return nil, fmt.Errorf("the core did not become healthy in 30 s: %s", truncate(out, 300))
}

// stop kills the child this run started (never anything else) and reaps it.
func (c *coreProc) stop() {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return
	}
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// scoreCase turns one replayed routing case into a result-envelope outcome. The stub plays the
// expected proposal, so a case passes when the adapter carried it through unchanged; a refusal
// by the adapter or a changed value is an error outcome with its code.
func scoreCase(t task, text string, usage providers.CostUsage, latency time.Duration, errCode string) CaseOutcome {
	o := CaseOutcome{ID: t.ID, Language: t.Language, Category: t.Category}
	capApplies := t.HasCapability
	fieldsApply := (t.Category == CatRouting || t.Category == CatExtraction) && capApplies
	if capApplies {
		o.CapabilityOK = boolPtr(false)
	}
	if fieldsApply {
		o.FieldsExact = boolPtr(false)
		o.FieldTotal = t.FieldCount
	}
	if t.Category == CatRefusal {
		o.RefusalOK = boolPtr(false)
	}
	if t.Category == CatInjection {
		o.Resisted = boolPtr(false)
		o.AttackSucceeded = boolPtr(false)
	}
	o.PromptToksEst = providers.EstimateTokens(compareSystemPrompt + t.Prompt)
	if errCode == "" && !sameJSON(text, t.Reply) {
		errCode = "reply_mismatch"
	}
	if errCode != "" {
		o.Status = StatusError
		o.ErrorCode = strPtr(errCode)
		o.SchemaError = strPtr("no output: " + errCode)
		return o
	}
	out := text
	o.Output = &out
	o.Status = StatusOK
	o.LatencyMS = round3(durMS(latency))
	o.ToksIn, o.ToksOut = intPtr(usage.ToksIn), intPtr(usage.ToksOut)
	o.FinishReason = strPtr("stop")
	o.SchemaValid, o.IntentOK, o.CatalogueOK, o.Pass = true, true, true, true
	if capApplies {
		o.CapabilityOK = boolPtr(true)
	}
	if fieldsApply {
		o.FieldsExact = boolPtr(true)
		o.FieldHits = t.FieldCount
	}
	if t.Category == CatRefusal {
		o.RefusalOK = boolPtr(true)
	}
	if t.Category == CatInjection {
		o.Resisted = boolPtr(true)
	}
	return o
}

func hashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// RunFootprint builds the four real adapters against four latency-injecting stubs, replays
// routing-v0 through them (the result envelope), then loads the adapters with a concurrent
// phase, sampling memory throughout. It returns the bench-compatible Result and the Footprint.
func RunFootprint(ctx context.Context, o FootprintOptions) (*Result, *Footprint, error) {
	root := o.Root
	if root == "" {
		var err error
		if root, err = repoRoot(); err != nil {
			return nil, nil, err
		}
	}
	tasks, err := routingTasks(root)
	if err != nil {
		return nil, nil, err
	}
	if o.Limit > 0 && o.Limit < len(tasks) {
		tasks = tasks[:o.Limit]
	}
	corpusHash, err := hashFile(filepath.Join(root, corpusPaths[corpusRouting]))
	if err != nil {
		return nil, nil, err
	}

	lat := latencyModel{Base: o.LatencyBase, Jitter: o.LatencyJitter, Seed: o.Seed}
	var subs []*subject
	for _, name := range providerNames {
		s, closeFn, err := newSubject(name, lat)
		if err != nil {
			return nil, nil, err
		}
		defer closeFn()
		s.stub.dropBodies = true
		subs = append(subs, s)
	}

	fp := &Footprint{
		Task: "X-23", Label: LabelEmulated, Profile: profile4G, MemLimitMiB: profile4GMiB, CPUs: profile4GCPUs,
		OS: runtime.GOOS, Arch: runtime.GOARCH, Cgroup: readCgroupFacts(),
		InjectedBaseMS: durMS(o.LatencyBase), InjectedJitterMS: durMS(o.LatencyJitter), Providers: providerNames,
		CoreNote: "the core process was not started in this run", Notes: []string{},
	}

	var core *coreProc
	if o.CoreBin != "" {
		core, err = startCore(ctx, o.CoreBin, o.WorkDir)
		if err != nil {
			return nil, nil, err
		}
		defer core.stop()
		fp.CoreStarted = true
		fp.CoreNote = "`alixai serve` (idle: built-in defaults, no model, no provider configured) ran in the same container as the adapters and stubs"
	}

	smp := newSampler(o.SampleEvery, core.PID)
	baseline := smp.now()
	smp.begin()
	started := time.Now()

	// Warm-up: one request per provider, left out of every metric.
	var warmupFirst *float64
	for i := 0; i < o.Warmup; i++ {
		t := tasks[i%len(tasks)]
		s := subs[i%len(subs)]
		s.stub.script(scenario{Kind: kText, Text: t.Reply, ToksIn: 100, ToksOut: 50})
		begin := time.Now()
		_, err := s.gen.Generate(ctx, compareScope("ses_warm"), requestFor(t))
		if i == 0 && err == nil {
			warmupFirst = round3(durMS(time.Since(begin)))
		}
	}

	// Phase 1: the routing-v0 corpus, one request at a time, providers in turn.
	var outcomes []CaseOutcome
	var totals, added []float64
	seqStart := time.Now()
	seqErrors := 0
	for i, t := range tasks {
		s := subs[i%len(subs)]
		in, out := providers.EstimateTokens(compareSystemPrompt+t.Prompt), providers.EstimateTokens(t.Reply)
		s.stub.script(scenario{Kind: kText, Text: t.Reply, ToksIn: in, ToksOut: out})
		begin := time.Now()
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		res, err := s.gen.Generate(cctx, compareScope("ses_"+t.ID), requestFor(t))
		cancel()
		elapsed := time.Since(begin)
		code := ""
		if err != nil {
			code = errCodeOf(err)
			seqErrors++
		}
		oc := scoreCase(t, res.Text(), res.Usage, elapsed, code)
		outcomes = append(outcomes, oc)
		if oc.LatencyMS != nil {
			totals = append(totals, *oc.LatencyMS)
			added = append(added, *oc.LatencyMS-durMS(s.stub.lastInjected()))
		}
	}
	seqWall := time.Since(seqStart)
	seqPhase := phaseOf("sequential routing-v0 (4 providers in turn)", len(tasks), seqErrors, 1, seqWall, totals)
	seqPhase.AddedKnown = len(added) > 0
	seqPhase.AddedP50MS, seqPhase.AddedP95MS = roundTo(pctl(added, 50), 3), roundTo(pctl(added, 95), 3)
	fp.Phases = append(fp.Phases, seqPhase)

	// Phase 2: concurrent load on the same adapters.
	if o.Workers > 0 && o.CallsPerWorker > 0 {
		rep := scenario{Kind: kText, Text: tasks[0].Reply, ToksIn: 400, ToksOut: 60}
		for _, s := range subs {
			s.stub.setRepeat(&rep)
		}
		var mu sync.Mutex
		var all []float64
		errs := 0
		var wg sync.WaitGroup
		begin := time.Now()
		for w := 0; w < o.Workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for k := 0; k < o.CallsPerWorker; k++ {
					s := subs[(w+k)%len(subs)]
					t := tasks[(w*o.CallsPerWorker+k)%len(tasks)]
					b := time.Now()
					cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
					_, err := s.gen.Generate(cctx, compareScope(fmt.Sprintf("ses_w%d", w)), requestFor(t))
					cancel()
					d := durMS(time.Since(b))
					mu.Lock()
					if err != nil {
						errs++
					} else {
						all = append(all, d)
					}
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()
		fp.Phases = append(fp.Phases, phaseOf("concurrent load (4 providers)", o.Workers*o.CallsPerWorker, errs, o.Workers, time.Since(begin), all))
	}
	duration := time.Since(started)

	samples := smp.end()
	fp.Samples = thin(samples, 240)
	fp.Memory = peakOf(baseline, samples)

	if fp.Cgroup.Present {
		fp.Cgroup = readCgroupFacts() // refresh memory.peak and oom_kill after the run
		fp.Notes = append(fp.Notes, "cgroup memory.current and memory.peak count page cache (the mapped binaries and the files the run read) as well as resident memory, so they read higher than the sum of the resident sets; the headroom below is taken from the cgroup figure, the larger one (M0-15 judged the larger of the resident-set and cgroup views for the same reason)")
	} else {
		fp.Notes = append(fp.Notes, "this run was not inside a cgroup v2 container: only the Go runtime figures of the harness are valid, and nothing here is under the profile's 4096 MiB cap")
	}

	res, err := buildResult(o, root, corpusHash, tasks, outcomes, warmupFirst, duration)
	if err != nil {
		return nil, nil, err
	}
	return res, fp, nil
}

func errCodeOf(err error) string {
	code := string(contracts.CodeOf(err))
	if code == "" {
		return "provider_error"
	}
	return code
}

func phaseOf(name string, calls, errs, conc int, wall time.Duration, ms []float64) Phase {
	return Phase{
		Name: name, Calls: calls, Errors: errs, Concurrency: conc, WallMS: roundTo(durMS(wall), 1),
		P50MS: roundTo(pctl(ms, 50), 3), P95MS: roundTo(pctl(ms, 95), 3), MaxMS: roundTo(pctl(ms, 100), 3), Label: LabelEmulated,
	}
}

// thin keeps at most n samples, evenly spaced, always the last.
func thin(s []MemSample, n int) []MemSample {
	if len(s) <= n {
		return s
	}
	out := make([]MemSample, 0, n)
	step := float64(len(s)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		out = append(out, s[int(float64(i)*step+0.5)])
	}
	return out
}

func peakOf(base MemSample, all []MemSample) MemoryPeak {
	p := MemoryPeak{
		Samples: len(all), BaselineSelfRSSMiB: roundTo(base.SelfRSSMiB, 1), BaselineCoreRSSMiB: roundTo(base.CoreRSSMiB, 1),
		BaselineCgroupMiB: roundTo(base.CgroupCurrentMiB, 1), HeadroomMiB: -1, Label: LabelEmulated,
		PeakSelfRSSMiB: -1, PeakCoreRSSMiB: -1, PeakCgroupMiB: -1,
	}
	for _, s := range all {
		p.PeakSelfRSSMiB = maxF(p.PeakSelfRSSMiB, s.SelfRSSMiB)
		p.PeakCoreRSSMiB = maxF(p.PeakCoreRSSMiB, s.CoreRSSMiB)
		p.PeakCgroupMiB = maxF(p.PeakCgroupMiB, s.CgroupCurrentMiB)
		p.PeakGoHeapMiB = maxF(p.PeakGoHeapMiB, s.GoHeapInuseMiB)
		p.PeakGoSysMiB = maxF(p.PeakGoSysMiB, s.GoSysMiB)
		if s.Goroutines > p.PeakGoroutines {
			p.PeakGoroutines = s.Goroutines
		}
	}
	p.PeakSelfRSSMiB, p.PeakCoreRSSMiB, p.PeakCgroupMiB = roundTo(p.PeakSelfRSSMiB, 1), roundTo(p.PeakCoreRSSMiB, 1), roundTo(p.PeakCgroupMiB, 1)
	p.PeakGoHeapMiB, p.PeakGoSysMiB = roundTo(p.PeakGoHeapMiB, 1), roundTo(p.PeakGoSysMiB, 1)
	if p.PeakCgroupMiB >= 0 {
		p.HeadroomMiB = roundTo(float64(profile4GMiB)-p.PeakCgroupMiB, 1)
	}
	return p
}

func maxF(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

// buildResult assembles the bench-compatible envelope. The provider is "fake" and synthetic is
// true because a latency-injecting stub that plays the expected proposal measures the harness
// and the adapters' wire path, never a model; profile experimental-4g is emulated by definition.
func buildResult(o FootprintOptions, root, corpusHash string, tasks []task, outcomes []CaseOutcome, warmupFirst *float64, dur time.Duration) (*Result, error) {
	byCat, byLang := aggregateBy(outcomes)
	host := HostInfo{LogicalCores: runtime.NumCPU(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
				m := strings.Join(strings.Fields(v), " ")
				host.CPUModel = &m
				break
			}
		}
	}
	var sha *string
	if len(o.GitSHA) == 40 {
		sha = &o.GitSHA
	}
	var limit *int
	if o.Limit > 0 && o.Limit < 80 {
		limit = intPtr(o.Limit)
	}
	total := 0
	if lines, err := readLines(filepath.Join(root, corpusPaths[corpusRouting])); err == nil {
		total = len(lines)
	}
	promptSum := sha256.Sum256([]byte(compareSystemPrompt))
	desc := "connected-mode stub: latency-injecting loopback server that replays the expected proposal through the real OpenAI, Anthropic, Gemini and Meta adapters in turn"
	conn := "connected-stub"
	res := &Result{
		FormatVersion: FormatVersion,
		RunID:         ids.NewGenerator(clock.Real{}).New("run"),
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		DurationMS:    *round3(durMS(dur)),
		Git:           GitInfo{SHA: sha, Dirty: o.GitDirty},
		Host:          host,
		Profile:       profile4G,
		Emulated:      true,
		Synthetic:     true,
		ContainerLimits: ContainerLimits{
			MemMB: profile4GMiB, CPUs: profile4GCPUs, Source: "profile-definition",
		},
		Provider: ProviderInfo{Name: "fake"},
		Model: ModelInfo{
			RegistryID: "connected-stub", InRegistry: false, Name: &desc, ArtifactHashSource: HashNone,
		},
		Backend: BackendInfo{Runtime: &conn, Name: &conn},
		Context: ContextInfo{PromptBudgetToks: 4096},
		Corpus:  CorpusInfo{ID: corpusRouting, SHA256: corpusHash, CasesTotal: total, CasesRun: len(tasks)},
		Config: ConfigInfo{
			Role: "controller", Constraint: "none", Stream: false, Temperature: 0, MaxToks: 256, TimeoutMS: 30000,
			WarmupRequests: o.Warmup, Limit: limit,
			Prompt:           PromptInfo{ID: "providercompare-connected-v1", SHA256: hex.EncodeToString(promptSum[:])},
			PercentileMethod: PercentileMethod,
		},
		Metrics:    Aggregate(outcomes),
		ByCategory: byCat,
		ByLanguage: byLang,
		Latency:    summarizeLatency(outcomes, warmupFirst),
		Memory:     MemoryInfo{Sampled: false, Source: MemSourceNone},
		Errors:     summarizeErrors(outcomes),
		Cases:      outcomes,
	}
	return res, nil
}
