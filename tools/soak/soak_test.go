package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/sdk-go/sdktest"
)

// The harness is tested end to end with its own fakes: the real SDK driver over
// real HTTP to sdktest's fake API, a scripted memory source and a scripted
// stack probe. A run is a fraction of a second.

// testMix is a mix that runs fast: a few steps (one of which prepares a booking
// the driver must reject) and an ingestion that settles at once.
func testMix(interactive, ingestion bool) Mix {
	return Mix{
		ID: "test-mix",
		Interactive: InteractiveMix{Enabled: interactive, ThinkMS: 5, Steps: []Step{
			{ID: "book-prepare", Class: classBooking, Language: "en", Text: "Book Luca Ferri for 2026-10-14 10:30"},
			{ID: "book-free", Class: classBooking, Language: "en", Text: "Which appointments are free?"},
			{ID: "rag-lead", Class: classRAG, Language: "en", Text: "How far ahead can an appointment be booked?"},
		}},
		Ingestion: IngestionMix{Enabled: ingestion, Source: "clinic-policies", Op: IngestRebuild, PauseMS: 5, SettleMS: 0, TimeoutMS: 5000},
	}
}

func testConfig(profile models.Profile) Config {
	return Config{
		Duration: 600 * time.Millisecond, Profile: profile, Decision: DecisionOff,
		SampleInterval: 25 * time.Millisecond, Warmup: 0,
		Mix: testMix(true, true), MixSHA256: strings.Repeat("a", 64),
		Thresholds: DefaultThresholds(), Synthetic: true, Target: TargetInfo{Driver: "fake"},
	}
}

// runFake runs the harness against a fresh fake stack. script may change the
// stack: tie a change to the probe's read count with f.Probe.After, never to
// the clock, so that it is never part of the baseline on a slow machine.
func runFake(t *testing.T, cfg Config, script func(*fakeStack)) *Result {
	t.Helper()
	f, err := newFakeStack(cfg.Profile, cfg.Mix, cfg.Decision)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	if script != nil {
		script(f)
	}
	deps, err := f.Deps()
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func codes(fails []Failure) []string {
	out := []string{}
	for _, f := range fails {
		out = append(out, f.Code)
	}
	slices.Sort(out)
	return out
}

// A short run with fakes samples memory, swap, roles, queue and the reload
// counters, drives turns and re-ingestion through the real SDK driver, and passes
// when nothing is wrong. With a scripted reload or a lost role it fails, with
// exactly that failure: the default rate is zero (resource-budgets R3.5).
//
// SEC-078: a soak run samples evictions and reloads and fails above the
// configured rate.
func TestSoakShortRunWithFakes(t *testing.T) {
	t.Run("a healthy stack passes", func(t *testing.T) {
		cfg := testConfig(models.ProfileLite)
		cfg.Duration, cfg.Warmup = time.Second, 100*time.Millisecond // the first run of a cold process is slow
		var f *fakeStack
		res := runFake(t, cfg, func(s *fakeStack) { f = s })

		if !res.Passed() || len(res.Failures) != 0 {
			t.Fatalf("verdict %s, failures %v\n%s", res.Verdict, res.Failures, res.Report())
		}
		if !res.Observations.Completed {
			t.Error("the run was not completed")
		}
		if n := len(res.Observations.Samples); n < 3 {
			t.Errorf("only %d samples in %s at 25 ms", n, cfg.Duration)
		}
		if res.Summary.Memory.SteadySamples == 0 || res.Summary.Memory.MinHeadroomMiB == nil {
			t.Errorf("no steady-state memory: %+v", res.Summary.Memory)
		}
		if !res.Summary.Swap.Measured || res.Summary.Swap.PeakMiB != 0 {
			t.Errorf("swap: %+v", res.Summary.Swap)
		}
		st := res.Summary.Stack
		if !st.QueueKnown || !st.ReloadsObserved || st.ReloadsDelta != 0 || !slices.Equal(st.ProtectedRoles, []string{"controller", "embedder"}) {
			t.Errorf("stack summary: %+v", st)
		}
		if res.Summary.Turns.Count < 1 || res.Summary.Turns.Errors != 0 || res.Summary.Turns.P95MS == nil {
			t.Errorf("turns: %+v\n%s", res.Summary.Turns, res.Report())
		}
		if res.Summary.Ingests.Count < 1 || res.Summary.Ingests.Errors != 0 {
			t.Errorf("ingests: %+v", res.Summary.Ingests)
		}

		// The driver rejects what it prepares: one more request per booking turn, never a confirmation.
		prepared := 0
		for _, tr := range res.Observations.Turns {
			if tr.Step == "book-prepare" {
				prepared++
			}
		}
		if prepared == 0 {
			t.Fatal("the booking step never ran")
		}
		// (a turn, or the rejection of one, that the end of the run cut off is sent but not recorded)
		if got, want := len(f.API.RequestsOf("sendTurn")), len(res.Observations.Turns)+prepared; got < want || got > want+2 {
			t.Errorf("%d turn requests for %d turns and %d prepared actions, want %d to %d", got, len(res.Observations.Turns), prepared, want, want+2)
		}
		if n := len(f.API.RequestsOf("confirmAction")); n != 0 {
			t.Errorf("the soak confirmed %d actions", n)
		}

		// What it wrote verifies, and says what it is.
		if problems := VerifyResult(res); len(problems) != 0 {
			t.Errorf("VerifyResult: %v", problems)
		}
		if !res.Emulated || !res.Synthetic || res.ContainerLimits.MemMB != 8192 || res.Target.Memory.Kind != MemSourceFake {
			t.Errorf("labels: emulated %v synthetic %v limits %+v target %+v", res.Emulated, res.Synthetic, res.ContainerLimits, res.Target)
		}
	})

	t.Run("a model reload fails the run", func(t *testing.T) {
		cfg := testConfig(models.ProfileLite)
		res := runFake(t, cfg, func(f *fakeStack) {
			f.Probe.After(3, func(s *StackStats) { s.Unloads++; s.Reloads++ })
		})
		if diff := cmp.Diff([]string{FailModelReload}, codes(res.Failures)); diff != "" {
			t.Fatalf("failures (-want +got):\n%s\n%s", diff, res.Report())
		}
		if res.Summary.Stack.ReloadsDelta != 1 || res.Passed() {
			t.Errorf("reloads delta %d, passed %v", res.Summary.Stack.ReloadsDelta, res.Passed())
		}
	})

	t.Run("a reload within the configured rate passes", func(t *testing.T) {
		cfg := testConfig(models.ProfileLite)
		cfg.Thresholds.MaxReloads = 1
		res := runFake(t, cfg, func(f *fakeStack) {
			f.Probe.After(3, func(s *StackStats) { s.Unloads++; s.Reloads++ })
		})
		if !res.Passed() {
			t.Fatalf("one reload with max-reloads 1 failed the run: %v", res.Failures)
		}
	})

	t.Run("the eviction of a resident role fails the run", func(t *testing.T) {
		cfg := testConfig(models.ProfileLite)
		res := runFake(t, cfg, func(f *fakeStack) {
			f.Probe.After(3, func(s *StackStats) { s.Roles["embedder"] = RoleState{Configured: true, Status: "unavailable"} })
		})
		if diff := cmp.Diff([]string{FailRoleLost}, codes(res.Failures)); diff != "" {
			t.Fatalf("failures (-want +got):\n%s\n%s", diff, res.Report())
		}
		if !strings.Contains(res.Failures[0].Detail, "embedder") {
			t.Errorf("the failure does not name the role: %s", res.Failures[0].Detail)
		}
	})

	t.Run("failed turns and ingestions are counted, and fail the run above the rate", func(t *testing.T) {
		cfg := testConfig(models.ProfileLite)
		res := runFake(t, cfg, func(f *fakeStack) {
			f.API.OnTurn(func(sdktest.Turn) sdktest.TurnPlan {
				return sdktest.FailTurn(contracts.ErrUnavailable, "the runtime is down")
			})
		})
		got := codes(res.Failures)
		if !slices.Contains(got, FailTurnErrors) || res.Summary.Turns.Errors != res.Summary.Turns.Count {
			t.Fatalf("failures %v, turns %+v", got, res.Summary.Turns)
		}
		if slices.Contains(got, FailIngestErrors) || slices.Contains(got, FailNoIngest) {
			t.Errorf("a healthy ingestion was blamed: %v", got)
		}
		if len(res.Observations.Turns) == 0 || res.Observations.Turns[0].Err != "unavailable" {
			t.Errorf("the typed code of the failed turn is %q", res.Observations.Turns[0].Err)
		}
	})
}

// Swap in use fails the run, and only for that.
func TestSoakFailsOnSwap(t *testing.T) {
	cfg := testConfig(models.ProfileLite)
	res := runFake(t, cfg, func(f *fakeStack) {
		f.Mem.Set(MemSample{UsedMiB: 2000, SwapMiB: 64, SwapKnown: true})
	})
	if diff := cmp.Diff([]string{FailSwapUsed}, codes(res.Failures)); diff != "" {
		t.Fatalf("failures (-want +got):\n%s\n%s", diff, res.Report())
	}
	if res.Passed() || res.Summary.Swap.PeakMiB != 64 {
		t.Errorf("passed %v, swap %+v", res.Passed(), res.Summary.Swap)
	}
	if problems := VerifyResult(res); len(problems) != 0 {
		t.Errorf("VerifyResult: %v", problems)
	}
}

// Steady-state headroom under max(15% of the profile's memory, 1 GB) fails the
// run, and only for that. Lite is 8192 MiB: the rule asks for 1228.8 MiB free.
func TestSoakFailsOnLowHeadroom(t *testing.T) {
	cfg := testConfig(models.ProfileLite)
	res := runFake(t, cfg, func(f *fakeStack) {
		f.Mem.Set(MemSample{UsedMiB: 7200, SwapKnown: true}) // 992 MiB free
	})
	if diff := cmp.Diff([]string{FailHeadroomLow}, codes(res.Failures)); diff != "" {
		t.Fatalf("failures (-want +got):\n%s\n%s", diff, res.Report())
	}
	m := res.Summary.Memory
	if res.Passed() || m.MinHeadroomMiB == nil || *m.MinHeadroomMiB != 992 || m.RequiredHeadroomMiB != 1228.8 {
		t.Errorf("passed %v, memory %+v", res.Passed(), m)
	}
}

// An interrupt still returns the result, marked incomplete, which fails.
func TestSoakInterruptIsIncompleteAndFails(t *testing.T) {
	cfg := testConfig(models.ProfileLite)
	cfg.Duration = time.Minute
	f, err := newFakeStack(cfg.Profile, cfg.Mix, cfg.Decision)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	deps, err := f.Deps()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.Probe.After(3, func(*StackStats) { cancel() }) // the interrupt arrives after the preflight, whatever the speed
	start := time.Now()
	res, err := Run(ctx, cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the interrupt took %s to be honoured", took)
	}
	if res.Observations.Completed || res.Passed() || !slices.Contains(codes(res.Failures), FailIncomplete) {
		t.Errorf("completed %v, verdict %s, failures %v", res.Observations.Completed, res.Verdict, res.Failures)
	}
	if n := len(res.Observations.Samples); n < 2 {
		t.Errorf("%d samples: the first and the last are always taken", n)
	}
	if problems := VerifyResult(res); len(problems) != 0 {
		t.Errorf("VerifyResult: %v", problems)
	}
}

// A stack that cannot be soaked is refused before the clock starts, with the
// reason, and no result is written.
func TestSoakRefusesToStart(t *testing.T) {
	cases := []struct {
		name     string
		decision string
		script   func(*fakeStack)
		want     string
	}{
		{"memory cannot be read", DecisionOff, func(f *fakeStack) { f.Mem.Fail(errors.New("no docker")) }, "memory cannot be read"},
		{"stack cannot be read", DecisionOff, func(f *fakeStack) { f.Probe.Fail(errors.New("connection refused")) }, "the stack cannot be read"},
		{"stack not ready", DecisionOff, func(f *fakeStack) { f.API.SetReady(false) }, "not ready"},
		{"controller not ok", DecisionOff, func(f *fakeStack) {
			f.Probe.Update(func(s *StackStats) { s.Roles["controller"] = RoleState{Configured: true, Status: "degraded"} })
		}, "controller role is not ok"},
		{"decision on without a scorer", DecisionOn, func(f *fakeStack) {
			f.Probe.Update(func(s *StackStats) { delete(s.Roles, roleRouter) })
		}, "router role"},
		{"decision off with a scorer configured", DecisionOff, func(f *fakeStack) {
			f.Probe.Update(func(s *StackStats) { s.Roles[roleRouter] = RoleState{Configured: true, Status: statusOK} })
		}, "router role"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := testConfig(models.ProfileBalanced)
			cfg.Decision = c.decision
			f, err := newFakeStack(cfg.Profile, cfg.Mix, cfg.Decision)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			c.script(f)
			deps, err := f.Deps()
			if err != nil {
				t.Fatal(err)
			}
			res, err := Run(context.Background(), cfg, deps)
			if err == nil || res != nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("result %v, error %v, want an error containing %q", res != nil, err, c.want)
			}
		})
	}
}

// The decision axis is a label that is checked: the same load with the scorer on
// passes when the stack has it, and the result says so.
func TestSoakDecisionAxisOn(t *testing.T) {
	cfg := testConfig(models.ProfileBalanced)
	cfg.Decision = DecisionOn
	res := runFake(t, cfg, nil)
	if !res.Passed() || res.Decision.Axis != DecisionOn || res.Rules.Decision != DecisionOn {
		t.Fatalf("verdict %s, decision %+v, failures %v", res.Verdict, res.Decision, res.Failures)
	}
	if res.ContainerLimits.MemMB != 16384 || res.Summary.Memory.RequiredHeadroomMiB != 2457.6 {
		t.Errorf("balanced limits %+v, headroom %v", res.ContainerLimits, res.Summary.Memory.RequiredHeadroomMiB)
	}
	// Eviction of the controller is a failure with the scorer on as well.
	res = runFake(t, cfg, func(f *fakeStack) {
		f.Probe.After(3, func(s *StackStats) { s.Roles["controller"] = RoleState{Configured: true, Status: "unavailable"} })
	})
	if diff := cmp.Diff([]string{FailRoleLost}, codes(res.Failures)); diff != "" {
		t.Errorf("failures (-want +got):\n%s", diff)
	}
}

func TestDecisionAxisByProfile(t *testing.T) {
	cases := []struct {
		profile   models.Profile
		axis      string
		qualified bool
		ok        bool
	}{
		{models.ProfileLite, DecisionOff, false, true},
		{models.ProfileLite, DecisionOn, false, false},
		{models.ProfileLite, DecisionOn, true, true},
		{models.ProfileBalanced, DecisionOn, false, true},
		{models.ProfileExperimental4G, DecisionOff, false, true},
		{models.ProfileExperimental4G, DecisionOn, true, false},
		{models.ProfileBalanced, "maybe", false, false},
	}
	for _, c := range cases {
		err := checkDecisionAxis(c.profile, c.axis, c.qualified)
		if (err == nil) != c.ok {
			t.Errorf("%s/%s/qualified=%v: err %v, want ok=%v", c.profile, c.axis, c.qualified, err, c.ok)
		}
	}
}
