package main

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/models"
)

func okRoles() map[string]RoleState {
	return map[string]RoleState{"controller": {true, statusOK}, "embedder": {true, statusOK}}
}

// smp is a healthy sample at t ms using used MiB.
func smp(at int64, used float64) Sample {
	return Sample{
		AtMS: at, MemOK: true, Mem: MemSample{UsedMiB: used, SwapKnown: true},
		StackOK: true, Stack: StackStats{Roles: okRoles(), QueueKnown: true, ReloadsKnown: true},
	}
}

// goodObs is a run that passes under rulesFor(lite): four samples a second
// apart (the warm-up is one second), three turns, two ingestions.
func goodObs() Observations {
	return Observations{
		Completed: true,
		Samples:   []Sample{smp(0, 1000), smp(1000, 1100), smp(2000, 1100), smp(3000, 1100)},
		Turns:     []TurnRecord{{AtMS: 10, Step: "a", Class: classBooking, MS: 100}, {AtMS: 20, Step: "b", Class: classRAG, MS: 200}, {AtMS: 30, Step: "c", Class: classRAG, MS: 300}},
		Ingests:   []IngestRecord{{AtMS: 5, MS: 1000}, {AtMS: 2000, MS: 1200}},
	}
}

func rulesFor(p models.Profile) Rules {
	cfg := Config{Profile: p, Decision: DecisionOff, Warmup: 1000 * 1e6, Mix: testMix(true, true), Thresholds: DefaultThresholds()}
	return cfg.Rules(true)
}

func TestRequiredHeadroom(t *testing.T) {
	// max(15% of the memory, 1 GB): the floor wins below 6826.7 MiB, the percentage above.
	for _, c := range []struct{ limit, want float64 }{
		{4096, 1024}, {6000, 1024}, {6826, 1024}, {8192, 1228.8}, {16384, 2457.6},
	} {
		if got := RequiredHeadroomMiB(c.limit); got < c.want-0.001 || got > c.want+0.001 {
			t.Errorf("RequiredHeadroomMiB(%.0f) = %.3f, want %.3f", c.limit, got, c.want)
		}
	}
	for _, c := range []struct {
		p    models.Profile
		want float64
	}{{models.ProfileExperimental4G, 1024}, {models.ProfileLite, 1228.8}, {models.ProfileBalanced, 2457.6}} {
		if got := rulesFor(c.p).RequiredHeadroomMiB; got != c.want {
			t.Errorf("%s: required headroom %.1f, want %.1f", c.p, got, c.want)
		}
	}
}

func TestEvaluate(t *testing.T) {
	type edit func(*Observations, *Rules)
	cases := []struct {
		name string
		edit edit
		want []string
	}{
		{"a healthy run passes", func(*Observations, *Rules) {}, nil},
		{"the run was cut short", func(o *Observations, _ *Rules) { o.Completed = false }, []string{FailIncomplete}},

		// headroom: exactly the required amount passes, a MiB less fails; the 4 GB profile's floor is 1 GB
		{"headroom exactly at the rule passes", func(o *Observations, _ *Rules) { setUsed(o, 8192-1228.8) }, nil},
		{"headroom a MiB under the rule fails", func(o *Observations, _ *Rules) { setUsed(o, 8192-1228.8+1) }, []string{FailHeadroomLow}},
		{"headroom is judged on the worst steady sample", func(o *Observations, _ *Rules) { o.Samples[2].Mem.UsedMiB = 7500 }, []string{FailHeadroomLow}},
		{"a spike in the warm-up is not steady state", func(o *Observations, _ *Rules) { o.Samples[0].Mem.UsedMiB = 8000 }, nil},
		{"the 4 GB floor is 1 GB, not 15%", func(o *Observations, r *Rules) {
			*r = rulesFor(models.ProfileExperimental4G)
			setUsed(o, 4096-1024)
		}, nil},
		{"the 4 GB profile under its floor fails", func(o *Observations, r *Rules) {
			*r = rulesFor(models.ProfileExperimental4G)
			setUsed(o, 4096-1023)
		}, []string{FailHeadroomLow}},
		{"balanced asks for 15%", func(o *Observations, r *Rules) {
			*r = rulesFor(models.ProfileBalanced)
			setUsed(o, 16384-2400)
		}, []string{FailHeadroomLow}},

		// swap
		{"swap in use fails", func(o *Observations, _ *Rules) { o.Samples[2].Mem.SwapMiB = 1 }, []string{FailSwapUsed}},
		{"swap during the warm-up counts too", func(o *Observations, _ *Rules) { o.Samples[0].Mem.SwapMiB = 5 }, []string{FailSwapUsed}},
		{"swap within a configured allowance passes", func(o *Observations, r *Rules) {
			o.Samples[2].Mem.SwapMiB = 5
			r.MaxSwapMiB = 8
		}, nil},
		{"swap that was not measured fails", func(o *Observations, _ *Rules) { o.Samples[1].Mem.SwapKnown = false }, []string{FailSwapUnmeasured}},
		{"swap that was not measured passes when allowed", func(o *Observations, r *Rules) {
			o.Samples[1].Mem.SwapKnown = false
			r.AllowUnmeasuredSwap = true
		}, nil},

		// memory readings
		{"no memory reading at all", func(o *Observations, _ *Rules) {
			for i := range o.Samples {
				o.Samples[i].MemOK = false
			}
		}, []string{FailMemoryUnmeasured, FailSampleErrors}},
		{"a run shorter than its warm-up has no steady state", func(_ *Observations, r *Rules) { r.WarmupMS = 99999 }, []string{FailNoSteadySample}},
		{"too many failed readings", func(o *Observations, _ *Rules) {
			o.Samples[1].MemOK = false
			o.Samples[2].MemOK = false
		}, []string{FailSampleErrors}},

		// the stack
		{"a reload fails", func(o *Observations, _ *Rules) { o.Samples[3].Stack.Reloads = 1 }, []string{FailModelReload}},
		{"a reload within the allowance passes", func(o *Observations, r *Rules) {
			o.Samples[3].Stack.Reloads = 1
			r.MaxReloads = 1
		}, nil},
		{"a reload counter that goes down is a restart", func(o *Observations, _ *Rules) {
			o.Samples[1].Stack.Reloads, o.Samples[1].Stack.Unloads = 4, 4
			o.Samples[2].Stack.Reloads, o.Samples[2].Stack.Unloads = 0, 0
		}, []string{FailStackRestarted}},
		{"a protected role not ok after the warm-up is an eviction", func(o *Observations, _ *Rules) {
			o.Samples[3].Stack.Roles["controller"] = RoleState{true, "unavailable"}
		}, []string{FailRoleLost}},
		{"a protected role that is gone is an eviction", func(o *Observations, _ *Rules) {
			delete(o.Samples[3].Stack.Roles, "embedder")
		}, []string{FailRoleLost}},
		{"a role not ok during the warm-up is loading, not evicted", func(o *Observations, _ *Rules) {
			o.Samples[0].Stack.Roles["controller"] = RoleState{true, "degraded"}
		}, nil},
		{"a role that was never configured is not protected", func(o *Observations, _ *Rules) {
			for i := range o.Samples {
				o.Samples[i].Stack.Roles["stt"] = RoleState{false, "unavailable"}
			}
		}, nil},
		{"an optional role is not protected", func(o *Observations, _ *Rules) {
			for i := range o.Samples {
				o.Samples[i].Stack.Roles["reranker"] = RoleState{true, statusOK}
			}
			o.Samples[3].Stack.Roles["reranker"] = RoleState{true, "unavailable"}
		}, nil},
		{"the queue deeper than the limit fails", func(o *Observations, r *Rules) {
			o.Samples[2].Stack.QueuePending = 6
			r.MaxQueueDepth = 5
		}, []string{FailQueueDepth}},
		{"the queue is only reported without a limit", func(o *Observations, _ *Rules) { o.Samples[2].Stack.QueuePending = 600 }, nil},
		{"a stack that cannot be read", func(o *Observations, _ *Rules) {
			for i := range o.Samples {
				o.Samples[i].StackOK = false
			}
		}, []string{FailProbeFailed}},
		{"a probe that fails too often", func(o *Observations, _ *Rules) { o.Samples[2].StackOK = false }, []string{FailProbeFailed}},
		{"no probe, no stack checks", func(o *Observations, r *Rules) {
			r.ProbeEnabled = false
			for i := range o.Samples {
				o.Samples[i].StackOK = false
			}
		}, nil},

		// the decision axis
		{"the scorer on, and there", func(o *Observations, r *Rules) {
			r.Decision = DecisionOn
			for i := range o.Samples {
				o.Samples[i].Stack.Roles[roleRouter] = RoleState{true, statusOK}
			}
		}, nil},
		{"the scorer on, and absent", func(_ *Observations, r *Rules) { r.Decision = DecisionOn }, []string{FailDecisionMismatch}},
		{"the scorer off, and configured", func(o *Observations, _ *Rules) {
			for i := range o.Samples {
				o.Samples[i].Stack.Roles[roleRouter] = RoleState{true, statusOK}
			}
		}, []string{FailDecisionMismatch}},

		// the load
		{"no turn at all", func(o *Observations, _ *Rules) { o.Turns = nil }, []string{FailNoTurns}},
		{"no ingestion at all", func(o *Observations, _ *Rules) { o.Ingests = nil }, []string{FailNoIngest}},
		{"no turn needed when the mix has none", func(o *Observations, r *Rules) {
			o.Turns = nil
			r.InteractiveEnabled = false
		}, nil},
		{"every turn failed", func(o *Observations, _ *Rules) {
			for i := range o.Turns {
				o.Turns[i].Err = "unavailable"
			}
		}, []string{FailNoTurns, FailTurnErrors}},
		{"one failed turn in three is too many", func(o *Observations, _ *Rules) { o.Turns[1].Err = "timeout" }, []string{FailTurnErrors}},
		{"one failed ingestion in two is too many", func(o *Observations, _ *Rules) { o.Ingests[0].Err = "unavailable" }, []string{FailIngestErrors}},
		{"turn latency over the limit", func(_ *Observations, r *Rules) { r.MaxTurnP95MS = 250 }, []string{FailTurnLatency}},
		{"turn latency under the limit", func(_ *Observations, r *Rules) { r.MaxTurnP95MS = 300 }, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs, rules := goodObs(), rulesFor(models.ProfileLite)
			c.edit(&obs, &rules)
			_, fails := Evaluate(rules, obs)
			if diff := cmp.Diff(c.want, nilIfEmpty(codes(fails))); diff != "" {
				t.Errorf("failures (-want +got):\n%s\n%v", diff, fails)
			}
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func setUsed(o *Observations, used float64) {
	for i := range o.Samples {
		o.Samples[i].Mem.UsedMiB = used
	}
}

// One failed ingestion in many is within the tolerance; the summary is a pure
// function of the observations.
func TestEvaluateSummary(t *testing.T) {
	obs, rules := goodObs(), rulesFor(models.ProfileLite)
	for i := 0; i < 19; i++ {
		obs.Turns = append(obs.Turns, TurnRecord{Step: "x", MS: float64(400 + i)})
	}
	obs.Turns[3].Err = "timeout" // 1 of 22: 4.5%, under the 5% limit
	sum, fails := Evaluate(rules, obs)
	if len(fails) != 0 {
		t.Fatalf("failures: %v", fails)
	}
	if sum.Turns.Count != 22 || sum.Turns.Errors != 1 {
		t.Errorf("turns: %+v", sum.Turns)
	}
	m := sum.Memory
	if m.BaselineMiB == nil || *m.BaselineMiB != 1000 || *m.PeakMiB != 1100 || *m.SteadyMedianMiB != 1100 || *m.MinHeadroomMiB != 8192-1100 {
		t.Errorf("memory: %+v", m)
	}
	if m.SteadySamples != 3 || m.SamplesOK != 4 {
		t.Errorf("sample counts: %+v", m)
	}
	sum2, fails2 := Evaluate(rules, obs)
	if !sameJSON(sum, sum2) || !sameJSON(fails, fails2) {
		t.Error("Evaluate is not deterministic")
	}

	// nearest-rank: of 1..20 the p50 is the 10th value and the p95 the 19th.
	var ops []TurnRecord
	for i := 1; i <= 20; i++ {
		ops = append(ops, TurnRecord{MS: float64(i)})
	}
	l := summarizeOps(len(ops), func(i int) (float64, string) { return ops[i].MS, "" })
	if *l.P50MS != 10 || *l.P95MS != 19 || *l.MaxMS != 20 {
		t.Errorf("percentiles: %+v", l)
	}
}
