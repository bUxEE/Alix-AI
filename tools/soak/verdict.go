package main

import (
	"fmt"
	"math"
	"sort"
)

// The verdict is a pure function of the rules and the recorded observations, so
// `soak check` re-derives it from a stored result and a test asserts exact
// failure codes. Nothing here reads a clock, a process or the network.

// Failure codes. A run passes only with none.
const (
	FailIncomplete       = "incomplete"
	FailMemoryUnmeasured = "memory_unmeasured"
	FailNoSteadySample   = "no_steady_state_sample"
	FailHeadroomLow      = "headroom_low"
	FailSwapUsed         = "swap_used"
	FailSwapUnmeasured   = "swap_unmeasured"
	FailSampleErrors     = "sample_errors"
	FailProbeFailed      = "probe_failed"
	FailRoleLost         = "resident_role_lost"
	FailModelReload      = "model_reload"
	FailStackRestarted   = "stack_restarted"
	FailDecisionMismatch = "decision_axis_mismatch"
	FailQueueDepth       = "queue_depth"
	FailNoTurns          = "no_turns"
	FailNoIngest         = "no_ingest"
	FailTurnErrors       = "turn_errors"
	FailIngestErrors     = "ingest_errors"
	FailTurnLatency      = "turn_latency_p95"
)

const (
	statusOK   = "ok"
	roleRouter = "router"
)

// protectedRoles are the workers whose eviction is a defect, never a
// degradation (resource-budgets R3, master plan 7.4): the controller,
// retrieval (the embedder) and speech (STT, TTS, VAD). Only roles configured
// at the start of the run are protected.
var protectedRoles = []string{"controller", "embedder", "stt", "tts", "vad"}

// Thresholds are the limits a run is judged by. Zero on an optional limit
// means "report, do not fail".
type Thresholds struct {
	// MaxSwapMiB is the swap that may be in use: 0, because a certified
	// profile uses none (master plan 7.4).
	MaxSwapMiB float64 `json:"max_swap_mib"`
	// AllowUnmeasuredSwap lets a run pass although swap could not be read.
	AllowUnmeasuredSwap bool `json:"allow_unmeasured_swap"`
	// MaxReloads is the model reloads that may happen: 0 (resource-budgets R3.5).
	MaxReloads uint64 `json:"max_reloads"`
	// MaxErrorRate is the share of turns, and of ingestions, that may fail.
	MaxErrorRate float64 `json:"max_error_rate"`
	// MaxSampleErrorRate is the share of memory or stack readings that may fail.
	MaxSampleErrorRate float64 `json:"max_sample_error_rate"`
	// MaxTurnP95MS fails the run above this 95th percentile of turn latency;
	// 0 reports it only (the latency gates of 7.4 are measured elsewhere).
	MaxTurnP95MS float64 `json:"max_turn_p95_ms"`
	// MaxQueueDepth fails the run when more jobs than this are pending at once;
	// 0 reports it only.
	MaxQueueDepth int `json:"max_queue_depth"`
}

// DefaultThresholds are the limits of the plan: no swap, no reload, and a
// tolerance for the occasional failed operation.
func DefaultThresholds() Thresholds {
	return Thresholds{MaxErrorRate: 0.05, MaxSampleErrorRate: 0.10}
}

// Rules is everything Evaluate needs besides the observations. It is stored in
// the result.
type Rules struct {
	Profile  string  `json:"profile"`
	LimitMiB float64 `json:"limit_mib"`
	// RequiredHeadroomMiB is max(15% of LimitMiB, 1 GB), written out.
	RequiredHeadroomMiB float64 `json:"required_headroom_mib"`
	WarmupMS            int64   `json:"warmup_ms"`
	Thresholds
	Decision           string `json:"decision"`
	InteractiveEnabled bool   `json:"interactive_enabled"`
	IngestionEnabled   bool   `json:"ingestion_enabled"`
	ProbeEnabled       bool   `json:"probe_enabled"`
}

// Failure is one reason a run did not pass.
type Failure struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// LatencySummary is the outcome of one kind of operation.
type LatencySummary struct {
	Count     int      `json:"count"`
	Errors    int      `json:"errors"`
	ErrorRate float64  `json:"error_rate"`
	P50MS     *float64 `json:"p50_ms"`
	P95MS     *float64 `json:"p95_ms"`
	MaxMS     *float64 `json:"max_ms"`
}

// MemorySummary is the memory over the run. Steady figures use the samples
// taken after the warm-up.
type MemorySummary struct {
	SamplesOK           int      `json:"samples_ok"`
	SampleErrors        int      `json:"sample_errors"`
	SteadySamples       int      `json:"steady_samples"`
	BaselineMiB         *float64 `json:"baseline_mib"`
	PeakMiB             *float64 `json:"peak_mib"`
	SteadyMedianMiB     *float64 `json:"steady_median_mib"`
	LimitMiB            float64  `json:"limit_mib"`
	RequiredHeadroomMiB float64  `json:"required_headroom_mib"`
	// MinHeadroomMiB is the worst steady-state headroom, MedianHeadroomMiB the typical one.
	MinHeadroomMiB    *float64 `json:"min_headroom_mib"`
	MedianHeadroomMiB *float64 `json:"median_headroom_mib"`
}

// SwapSummary says whether swap was read and the most of it that was in use.
type SwapSummary struct {
	Measured bool    `json:"measured"`
	PeakMiB  float64 `json:"peak_mib"`
}

// StackSummary is what the probe saw.
type StackSummary struct {
	ProbeSamplesOK    int      `json:"probe_samples_ok"`
	ProbeErrors       int      `json:"probe_errors"`
	ProtectedRoles    []string `json:"protected_roles"`
	QueueKnown        bool     `json:"queue_known"`
	PeakQueuePending  int      `json:"peak_queue_pending"`
	PeakIngestPending int      `json:"peak_queue_ingest_pending"`
	PeakQueueRunning  int      `json:"peak_queue_running"`
	// ReloadsObserved is false when the probe has no governor counters (the
	// readiness and operations routes have none): then a reload cannot be
	// seen, only a protected role leaving ok.
	ReloadsObserved bool   `json:"reloads_observed"`
	ReloadsDelta    uint64 `json:"reloads_delta"`
	UnloadsDelta    uint64 `json:"unloads_delta"`
}

// Summary is the measured result of a run.
type Summary struct {
	Memory  MemorySummary  `json:"memory"`
	Swap    SwapSummary    `json:"swap"`
	Stack   StackSummary   `json:"stack"`
	Turns   LatencySummary `json:"turns"`
	Ingests LatencySummary `json:"ingests"`
}

type failFunc func(code, format string, args ...any)

// Evaluate applies the rules to the observations. The failures come in a fixed
// order and an empty list is a pass.
func Evaluate(r Rules, obs Observations) (Summary, []Failure) {
	fails := []Failure{}
	fail := func(code, format string, args ...any) {
		fails = append(fails, Failure{Code: code, Detail: fmt.Sprintf(format, args...)})
	}
	var s Summary
	if !obs.Completed {
		fail(FailIncomplete, "the run was cut short: the result covers only part of the planned duration")
	}
	s.Memory, s.Swap = evalMemory(r, obs, fail)
	s.Stack = evalStack(r, obs, fail)
	s.Turns = summarizeOps(len(obs.Turns), func(i int) (float64, string) { return obs.Turns[i].MS, obs.Turns[i].Err })
	s.Ingests = summarizeOps(len(obs.Ingests), func(i int) (float64, string) { return obs.Ingests[i].MS, obs.Ingests[i].Err })
	if r.InteractiveEnabled {
		evalLoad(r, s.Turns, "turn", FailNoTurns, FailTurnErrors, fail)
		if r.MaxTurnP95MS > 0 && s.Turns.P95MS != nil && *s.Turns.P95MS > r.MaxTurnP95MS {
			fail(FailTurnLatency, "turn latency p95 %.0f ms is above the limit of %.0f ms", *s.Turns.P95MS, r.MaxTurnP95MS)
		}
	}
	if r.IngestionEnabled {
		evalLoad(r, s.Ingests, "ingestion", FailNoIngest, FailIngestErrors, fail)
	}
	return s, fails
}

// evalLoad judges one kind of load: it must have run, and it must not fail
// more often than allowed.
func evalLoad(r Rules, l LatencySummary, what, noCode, errCode string, fail failFunc) {
	if l.Count-l.Errors == 0 {
		fail(noCode, "no %s completed: a run that put no such load on the stack proves nothing", what)
	}
	if l.Count > 0 && l.ErrorRate > r.MaxErrorRate {
		fail(errCode, "%d of %d %ss failed (%.1f%%), above the limit of %.1f%%", l.Errors, l.Count, what, l.ErrorRate*100, r.MaxErrorRate*100)
	}
}

func evalMemory(r Rules, obs Observations, fail failFunc) (MemorySummary, SwapSummary) {
	m := MemorySummary{LimitMiB: r.LimitMiB, RequiredHeadroomMiB: r.RequiredHeadroomMiB}
	var sw SwapSummary
	var used, steadyUsed, headroom []float64
	swapKnownAll := true
	for _, sm := range obs.Samples {
		if !sm.MemOK {
			m.SampleErrors++
			continue
		}
		m.SamplesOK++
		used = append(used, sm.Mem.UsedMiB)
		if sm.Mem.SwapKnown {
			sw.PeakMiB = math.Max(sw.PeakMiB, sm.Mem.SwapMiB)
		} else {
			swapKnownAll = false
		}
		if sm.AtMS >= r.WarmupMS {
			steadyUsed = append(steadyUsed, sm.Mem.UsedMiB)
			headroom = append(headroom, r.LimitMiB-sm.Mem.UsedMiB)
		}
	}
	m.SteadySamples = len(steadyUsed)
	sw.Measured = m.SamplesOK > 0 && swapKnownAll
	sw.PeakMiB = round3v(sw.PeakMiB)
	if m.SamplesOK > 0 {
		m.BaselineMiB = round3(used[0])
		m.PeakMiB = round3(maxOf(used))
	}
	var minHead float64
	if len(steadyUsed) > 0 {
		minHead = minOf(headroom)
		m.SteadyMedianMiB = round3(medianOf(steadyUsed))
		m.MinHeadroomMiB = round3(minHead)
		m.MedianHeadroomMiB = round3(medianOf(headroom))
	}

	switch {
	case m.SamplesOK == 0:
		fail(FailMemoryUnmeasured, "no memory reading succeeded (%d failed): memory and swap were not measured", m.SampleErrors)
	case len(steadyUsed) == 0:
		fail(FailNoSteadySample, "no memory reading was taken after the %d ms warm-up: the run was too short to measure steady state", r.WarmupMS)
	case minHead < r.RequiredHeadroomMiB:
		fail(FailHeadroomLow, "worst steady-state headroom %.1f MiB of %.0f MiB is below the required %.1f MiB (the greater of 15%% and 1 GB)",
			minHead, r.LimitMiB, r.RequiredHeadroomMiB)
	}
	if m.SamplesOK > 0 {
		if sw.PeakMiB > r.MaxSwapMiB {
			fail(FailSwapUsed, "swap in use reached %.0f MiB (limit %.0f MiB): a certified profile uses no swap", sw.PeakMiB, r.MaxSwapMiB)
		}
		if !swapKnownAll && !r.AllowUnmeasuredSwap {
			fail(FailSwapUnmeasured, "swap could not be read from every sample and --allow-unmeasured-swap is not set")
		}
	}
	if total := m.SamplesOK + m.SampleErrors; m.SampleErrors > 0 && float64(m.SampleErrors)/float64(total) > r.MaxSampleErrorRate {
		fail(FailSampleErrors, "%d of %d memory readings failed", m.SampleErrors, total)
	}
	return m, sw
}

// reloadWatch follows the governor's counters over a run.
type reloadWatch struct {
	seen                         bool
	restarted                    bool
	firstR, firstU, lastR, lastU uint64
}

func (w *reloadWatch) add(reloads, unloads uint64) {
	if !w.seen {
		w.seen = true
		w.firstR, w.firstU = reloads, unloads
	} else if reloads < w.lastR || unloads < w.lastU {
		w.restarted = true // a counter only goes down when the process began again
	}
	w.lastR, w.lastU = reloads, unloads
}

func evalStack(r Rules, obs Observations, fail failFunc) StackSummary {
	st := StackSummary{ProtectedRoles: []string{}}
	if !r.ProbeEnabled {
		return st
	}
	var first *Sample
	var protected []string
	var rw reloadWatch
	lost := map[string]int64{} // role -> first time it was not ok after the warm-up
	for i := range obs.Samples {
		sm := &obs.Samples[i]
		if !sm.StackOK {
			st.ProbeErrors++
			continue
		}
		st.ProbeSamplesOK++
		if first == nil {
			first = sm
			for _, role := range protectedRoles {
				if rs, ok := sm.Stack.Roles[role]; ok && rs.Configured {
					protected = append(protected, role)
				}
			}
		}
		if sm.Stack.QueueKnown {
			st.QueueKnown = true
			st.PeakQueuePending = max(st.PeakQueuePending, sm.Stack.QueuePending)
			st.PeakIngestPending = max(st.PeakIngestPending, sm.Stack.QueueIngestPending)
			st.PeakQueueRunning = max(st.PeakQueueRunning, sm.Stack.QueueRunning)
		}
		if sm.Stack.ReloadsKnown {
			rw.add(sm.Stack.Reloads, sm.Stack.Unloads)
		}
		if sm.AtMS >= r.WarmupMS && sm.Stack.Roles != nil {
			for _, role := range protected {
				if rs := sm.Stack.Roles[role]; !rs.Configured || rs.Status != statusOK {
					if _, seen := lost[role]; !seen {
						lost[role] = sm.AtMS
					}
				}
			}
		}
	}
	if protected != nil {
		st.ProtectedRoles = protected
	}
	st.ReloadsObserved = rw.seen
	if rw.seen && !rw.restarted {
		st.ReloadsDelta, st.UnloadsDelta = rw.lastR-rw.firstR, rw.lastU-rw.firstU
	}

	switch total := st.ProbeSamplesOK + st.ProbeErrors; {
	case st.ProbeSamplesOK == 0:
		fail(FailProbeFailed, "the stack could not be read in any of %d samples", total)
	case float64(st.ProbeErrors)/float64(total) > r.MaxSampleErrorRate:
		fail(FailProbeFailed, "the stack could not be read in %d of %d samples", st.ProbeErrors, total)
	}
	if first != nil {
		evalDecision(r, first.Stack.Roles, fail)
	}
	roles := make([]string, 0, len(lost))
	for role := range lost {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		fail(FailRoleLost, "%s was configured at the start and was not ok from %d ms: it was evicted or lost (resource-budgets R3.5)", role, lost[role])
	}
	switch {
	case rw.restarted:
		fail(FailStackRestarted, "a governor counter went down: the service restarted during the run")
	case rw.seen && st.ReloadsDelta > r.MaxReloads:
		fail(FailModelReload, "%d model reloads during the run (limit %d): each is a cold load after an unload (%d unloads)", st.ReloadsDelta, r.MaxReloads, st.UnloadsDelta)
	}
	if r.MaxQueueDepth > 0 && st.PeakQueuePending > r.MaxQueueDepth {
		fail(FailQueueDepth, "%d jobs were pending at once (limit %d)", st.PeakQueuePending, r.MaxQueueDepth)
	}
	return st
}

// evalDecision checks the decision-layer axis against the roles at the start of
// the run. The scorer is the router role: with the axis on it must be there and
// ok, with the axis off it must not be configured.
func evalDecision(r Rules, roles map[string]RoleState, fail failFunc) {
	if roles == nil {
		return
	}
	rs := roles[roleRouter]
	switch r.Decision {
	case DecisionOn:
		if !rs.Configured || rs.Status != statusOK {
			fail(FailDecisionMismatch, "--decision on, but the router role (the scorer) is not configured and ok at the start (configured %v, status %q)", rs.Configured, rs.Status)
		}
	case DecisionOff:
		if rs.Configured {
			fail(FailDecisionMismatch, "--decision off, but the router role (the scorer) is configured: the optional scorer is not disabled")
		}
	}
}

// summarizeOps reduces n operations to counts and the latency of the ones that
// succeeded: a failed call's time is the time to fail, not a latency.
func summarizeOps(n int, at func(i int) (ms float64, errCode string)) LatencySummary {
	l := LatencySummary{Count: n}
	var ok []float64
	for i := 0; i < n; i++ {
		ms, code := at(i)
		if code != "" {
			l.Errors++
			continue
		}
		ok = append(ok, ms)
	}
	if n > 0 {
		l.ErrorRate = round3v(float64(l.Errors) / float64(n))
	}
	if len(ok) > 0 {
		sort.Float64s(ok)
		l.P50MS = round3(nearestRank(ok, 50))
		l.P95MS = round3(nearestRank(ok, 95))
		l.MaxMS = round3(ok[len(ok)-1])
	}
	return l
}

// ---------------------------------------------------------------- numbers

// PercentileMethod names the percentile definition, as tools/bench does.
const PercentileMethod = "nearest-rank"

// nearestRank assumes sorted is ascending and non-empty and pct is 1..100.
func nearestRank(sorted []float64, pct int) float64 {
	n := len(sorted)
	rank := (pct*n + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

func round3(x float64) *float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return nil
	}
	r := round3v(x)
	return &r
}

func round3v(x float64) float64 { return math.Round(x*1000) / 1000 }

func maxOf(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs {
		m = math.Max(m, x)
	}
	return m
}

func minOf(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs {
		m = math.Min(m, x)
	}
	return m
}

func medianOf(xs []float64) float64 {
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}
