package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/providers/fake"
)

// scriptedLatency gives case i (0-based, corpus order) a total latency of
// (i+1)*10 ms and a time to first token of a tenth of it.
func scriptedLatency(corpus *Corpus) func(string) (time.Duration, time.Duration) {
	idx := map[string]int{}
	for i, c := range corpus.Cases {
		idx[c.ID] = i
	}
	return func(id string) (time.Duration, time.Duration) {
		total := time.Duration(idx[id]+1) * 10 * time.Millisecond
		return total / 10, total
	}
}

// TestBenchRunsWithFakeProvider runs the whole corpus through the harness with
// a scripted provider that is perfect except for nine named cases, each wrong
// in a different way, and checks every number the harness reports against
// counts worked out by hand from the corpus (not read back from the harness).
func TestBenchRunsWithFakeProvider(t *testing.T) {
	corpus := realCorpus(t)
	clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))

	p := fake.New()
	byID := map[string]Case{}
	for _, c := range corpus.Cases {
		byID[c.ID] = c
	}
	p.On(fake.Any()).Usage(100, 20).ReplyFunc(func(call fake.Call) (string, error) {
		return oracleReply(byID[call.Scope.SessionID]), nil
	})
	script := func(id, reply string) {
		p.On(caseIs(id)).Usage(100, 20).Reply(reply)
	}
	// A: the wrong capability on a routing case.
	script("rt-en-02", proposalJSON(`"menu.search"`, "task", `{}`))
	// B: a wrong value (party_size 5 instead of 4).
	script("ex-en-01", proposalJSON(`"bookings.create"`, "task", `{"party_size":5,"date":"2026-10-13","time":"19:30","customer_name":"Rossi"}`))
	// C: an invented but declared field (notes).
	script("ex-it-02", proposalJSON(`"bookings.create"`, "task", `{"party_size":6,"date":"2026-10-14","time":"21:00","customer_name":"Kowalski","notes":"x"}`))
	// D: a refusal case answered as a task.
	script("rf-en-03", proposalJSON(`"appointments.book"`, "task", `{}`))
	// E: out of scope that still names a capability: the contract forbids it.
	script("rf-it-01", proposalJSON(`"bookings.create"`, "out_of_scope", `{}`))
	// F: the injection works: the model proposes the capability the attacker wants.
	script("in-en-01", proposalJSON(`"bookings.delete_all"`, "task", `{}`))
	// G: the injection fails but the model does the wrong thing anyway.
	script("in-en-03", proposalJSON(`"leave.request"`, "task", `{}`))
	// H: truncated JSON on an injection case.
	p.On(caseIs("in-en-02")).Usage(100, 20).Fail(fake.FailMalformedJSON)
	// I: the runtime times out.
	p.On(caseIs("rt-it-03")).Fail(fake.FailTimeout)

	gen := &timedGen{inner: p, clk: clk, latency: scriptedLatency(corpus)}
	cfg := testRunConfig(t, corpus, gen, clk)
	res, err := Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	t.Run("the result passes its own check", func(t *testing.T) {
		_, resultSchema, _ := realSchemas(t)
		data, err := marshalResult(res)
		if err != nil {
			t.Fatal(err)
		}
		if problems := validateResultBytes(resultSchema, data); len(problems) > 0 {
			t.Fatalf("the harness's own result is refused by check:\n%v", problems)
		}
	})

	ratio := func(passed, total int) Ratio { return newRatio(passed, total) }

	t.Run("headline metrics", func(t *testing.T) {
		want := MetricSet{
			Cases:               80,
			SchemaValid:         ratio(77, 80), // E, H and I are not valid proposals
			CapabilityRouting:   ratio(52, 54), // A and I of the 54 cases that expect a capability
			Intent:              ratio(74, 80), // D, E, F, G, H, I
			FieldExactMatch:     ratio(41, 44), // B, C and I of the 44 routing and extraction cases
			FieldValueAccuracy:  ratio(67, 68), // only B lost a value
			CatalogueConformity: ratio(76, 80), // E, F (invented capability), H, I
			RefusalCorrectness:  ratio(14, 16), // D and E
			InjectionResistance: ratio(14, 16), // F fell for it; H is invalid, which is not resistance
			Pass:                ratio(71, 80), // A to I
		}
		if diff := cmp.Diff(want, res.Metrics); diff != "" {
			t.Errorf("metrics (-want +got):\n%s", diff)
		}
	})

	t.Run("per category", func(t *testing.T) {
		want := ByCategory{
			Routing: MetricSet{
				Cases: 24, SchemaValid: ratio(23, 24), CapabilityRouting: ratio(18, 20), Intent: ratio(23, 24),
				FieldExactMatch: ratio(19, 20), FieldValueAccuracy: ratio(0, 0), CatalogueConformity: ratio(23, 24),
				RefusalCorrectness: ratio(0, 0), InjectionResistance: ratio(0, 0), Pass: ratio(22, 24),
			},
			Extraction: MetricSet{
				Cases: 24, SchemaValid: ratio(24, 24), CapabilityRouting: ratio(24, 24), Intent: ratio(24, 24),
				FieldExactMatch: ratio(22, 24), FieldValueAccuracy: ratio(67, 68), CatalogueConformity: ratio(24, 24),
				RefusalCorrectness: ratio(0, 0), InjectionResistance: ratio(0, 0), Pass: ratio(22, 24),
			},
			Refusal: MetricSet{
				Cases: 16, SchemaValid: ratio(15, 16), CapabilityRouting: ratio(0, 0), Intent: ratio(14, 16),
				FieldExactMatch: ratio(0, 0), FieldValueAccuracy: ratio(0, 0), CatalogueConformity: ratio(15, 16),
				RefusalCorrectness: ratio(14, 16), InjectionResistance: ratio(0, 0), Pass: ratio(14, 16),
			},
			Injection: MetricSet{
				Cases: 16, SchemaValid: ratio(15, 16), CapabilityRouting: ratio(10, 10), Intent: ratio(13, 16),
				FieldExactMatch: ratio(0, 0), FieldValueAccuracy: ratio(0, 0), CatalogueConformity: ratio(14, 16),
				RefusalCorrectness: ratio(0, 0), InjectionResistance: ratio(14, 16), Pass: ratio(13, 16),
			},
		}
		if diff := cmp.Diff(want, res.ByCategory); diff != "" {
			t.Errorf("by_category (-want +got):\n%s", diff)
		}
	})

	t.Run("per language", func(t *testing.T) {
		// English holds A, B, D, F, G, H; Italian holds C, E, I.
		if got, want := res.ByLanguage.EN.Pass, ratio(34, 40); !ratioEq(got, want) {
			t.Errorf("en pass = %+v, want %+v", got, want)
		}
		if got, want := res.ByLanguage.IT.Pass, ratio(37, 40); !ratioEq(got, want) {
			t.Errorf("it pass = %+v, want %+v", got, want)
		}
		if res.ByLanguage.EN.Cases != 40 || res.ByLanguage.IT.Cases != 40 {
			t.Errorf("language split %d/%d, want 40/40", res.ByLanguage.EN.Cases, res.ByLanguage.IT.Cases)
		}
	})

	t.Run("errors are outcomes, not aborts", func(t *testing.T) {
		if res.Errors.Count != 1 || res.Errors.ByCode["timeout"] != 1 || len(res.Errors.ByCode) != 1 {
			t.Errorf("errors = %+v, want one timeout", res.Errors)
		}
		var timedOut *CaseOutcome
		for i := range res.Cases {
			if res.Cases[i].ID == "rt-it-03" {
				timedOut = &res.Cases[i]
			}
		}
		if timedOut == nil || timedOut.Status != StatusError || timedOut.ErrorCode == nil || *timedOut.ErrorCode != "timeout" ||
			timedOut.Output != nil || timedOut.LatencyMS != nil || timedOut.Pass || timedOut.SchemaValid {
			t.Errorf("the timed-out case = %+v", timedOut)
		}
		if len(res.Cases) != 80 {
			t.Errorf("%d cases recorded, want all 80 (a failure must stay in its denominator)", len(res.Cases))
		}
	})

	t.Run("latency percentiles", func(t *testing.T) {
		// Totals are 10, 20 ... 800 ms except the timed-out case (index 14: 150 ms), so 79 samples.
		// Nearest rank: p50 -> 40th value, p90 -> 72nd, p95 -> 76th, p99 -> 79th.
		got := res.Latency.TotalMS
		want := Stats{N: 79, Mean: round3(32250.0 / 79), Min: round3(10), P50: round3(410), P90: round3(730), P95: round3(770), P99: round3(800), Max: round3(800)}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("total_ms (-want +got):\n%s", diff)
		}
		// Time to first token is a tenth of each total.
		wantTTFT := Stats{N: 79, Mean: round3(3225.0 / 79), Min: round3(1), P50: round3(41), P90: round3(73), P95: round3(77), P99: round3(80), Max: round3(80)}
		if diff := cmp.Diff(wantTTFT, res.Latency.TTFTMS); diff != "" {
			t.Errorf("ttft_ms (-want +got):\n%s", diff)
		}
	})

	t.Run("throughput", func(t *testing.T) {
		// 79 completed cases, each reporting 100 tokens in and 20 out.
		if res.Latency.ToksIn != 7900 || res.Latency.ToksOut != 1580 {
			t.Errorf("tokens in/out = %d/%d, want 7900/1580", res.Latency.ToksIn, res.Latency.ToksOut)
		}
		// 1580 tokens over 32.25 s of total latency; and over the 29.025 s after the first token.
		if !floatEq(res.Latency.ToksPerSecond, *round3(1580 / 32.25)) {
			t.Errorf("toks_per_second = %v, want %v", deref(res.Latency.ToksPerSecond), 1580/32.25)
		}
		if !floatEq(res.Latency.DecodeToksPerSecond, *round3(1580 / 29.025)) {
			t.Errorf("decode_toks_per_second = %v, want %v", deref(res.Latency.DecodeToksPerSecond), 1580/29.025)
		}
	})

	t.Run("warm-up is measured apart and excluded", func(t *testing.T) {
		// The warm-up request is the first case (10 ms); it must not be among the 79 samples
		// or the 80 recorded cases twice.
		if !floatEq(res.Latency.WarmupFirstRequestMS, 10) {
			t.Errorf("warmup_first_request_ms = %v, want 10", deref(res.Latency.WarmupFirstRequestMS))
		}
		// The measured phase is the 79 completed latencies, 32 250 ms; the timed-out call
		// failed before any time passed.
		if res.DurationMS != 32250 {
			t.Errorf("duration_ms = %v, want 32250", res.DurationMS)
		}
		// Warm-up + 80 measured requests reached the provider.
		if got := p.Calls(); got != 81 {
			t.Errorf("provider saw %d calls, want 81", got)
		}
	})

	t.Run("what was sent", func(t *testing.T) {
		for _, pr := range p.Prompts()[:3] {
			if pr.Op != fake.OpChatStream {
				t.Errorf("op = %s, want streamed chat", pr.Op)
			}
			req := pr.Chat
			if req.Output.Kind() != providers.ConstraintJSONSchema {
				t.Errorf("constraint = %q, want a JSON Schema", req.Output.Kind())
			}
			if req.Temperature != 0 || req.MaxTokens != 256 {
				t.Errorf("temperature %v, max_toks %d; want greedy decoding capped at 256", req.Temperature, req.MaxTokens)
			}
			if len(req.Messages) != 2 || req.Messages[0].Role != providers.MsgSystem || !req.Messages[0].Trusted ||
				req.Messages[1].Role != providers.MsgUser || req.Messages[1].Trusted {
				t.Errorf("messages = %+v, want a trusted system message and an untrusted user message", req.Messages)
			}
			if pr.Scope.ApplicationID != "bench" || len(pr.Scope.CapabilityScopes) < 2 {
				t.Errorf("scope = %+v", pr.Scope)
			}
		}
	})

	t.Run("the record", func(t *testing.T) {
		if res.Profile != "lite" || !res.Emulated || !res.Synthetic || res.Provider.Name != "fake" {
			t.Errorf("profile %q emulated %v synthetic %v provider %q", res.Profile, res.Emulated, res.Synthetic, res.Provider.Name)
		}
		if res.ContainerLimits.MemMB != 8192 || res.ContainerLimits.CPUs != 4 {
			t.Errorf("limits = %+v", res.ContainerLimits)
		}
		if res.Corpus.ID != "routing-v0" || res.Corpus.SHA256 != corpus.SHA256 || res.Corpus.CasesTotal != 80 || res.Corpus.CasesRun != 80 {
			t.Errorf("corpus = %+v", res.Corpus)
		}
		if res.Config.Prompt.SHA256 != PromptSHA256() || res.Config.PercentileMethod != PercentileMethod {
			t.Errorf("config = %+v", res.Config)
		}
		if res.Timestamp != "2026-10-02T12:00:00Z" {
			t.Errorf("timestamp = %q, want the injected clock's time", res.Timestamp)
		}
		if res.Memory.Sampled || res.Memory.Source != MemSourceNone {
			t.Errorf("memory = %+v, want no sampling", res.Memory)
		}
	})
}

func ratioEq(a, b Ratio) bool {
	return a.Passed == b.Passed && a.Total == b.Total && cmp.Equal(a.Rate, b.Rate)
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestRunVariants covers the other ways the harness can be asked to run:
// without streaming, with a limit, with no warm-up, with each constraint.
func TestRunVariants(t *testing.T) {
	corpus := realCorpus(t)
	newGen := func() (*fake.Provider, *clock.Fake, *timedGen) {
		clk := clock.NewFake(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
		p := newOracle(corpus)
		return p, clk, &timedGen{inner: p, clk: clk, latency: scriptedLatency(corpus)}
	}

	t.Run("limit and no warm-up", func(t *testing.T) {
		p, clk, gen := newGen()
		cfg := testRunConfig(t, corpus, gen, clk)
		cfg.Limit, cfg.Warmup = 5, 0
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Cases) != 5 || res.Corpus.CasesRun != 5 || res.Corpus.CasesTotal != 80 || res.Config.Limit == nil || *res.Config.Limit != 5 {
			t.Errorf("limit not honoured: %d cases, corpus %+v, limit %v", len(res.Cases), res.Corpus, res.Config.Limit)
		}
		if p.Calls() != 5 {
			t.Errorf("provider saw %d calls, want 5 (no warm-up)", p.Calls())
		}
		if res.Latency.WarmupFirstRequestMS != nil || res.Config.WarmupRequests != 0 {
			t.Errorf("warm-up recorded although none ran: %+v", res.Latency.WarmupFirstRequestMS)
		}
	})

	t.Run("without streaming there is no time to first token", func(t *testing.T) {
		p, clk, gen := newGen()
		cfg := testRunConfig(t, corpus, gen, clk)
		cfg.Stream, cfg.Limit = false, 4
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if res.Latency.TTFTMS.N != 0 || res.Latency.TTFTMS.P50 != nil || res.Latency.DecodeToksPerSecond != nil {
			t.Errorf("ttft = %+v decode = %v, want none", res.Latency.TTFTMS, deref(res.Latency.DecodeToksPerSecond))
		}
		if res.Latency.TotalMS.N != 4 || res.Config.Stream {
			t.Errorf("total latency n = %d stream = %v", res.Latency.TotalMS.N, res.Config.Stream)
		}
		for _, pr := range p.Prompts() {
			if pr.Op != fake.OpChat {
				t.Errorf("op = %s, want chat", pr.Op)
			}
		}
	})

	t.Run("constraints", func(t *testing.T) {
		for _, tt := range []struct {
			constraint string
			kind       providers.ConstraintKind
		}{
			{ConstraintSchema, providers.ConstraintJSONSchema},
			{ConstraintGrammar, providers.ConstraintGrammar},
			{ConstraintNone, providers.ConstraintNone},
		} {
			p, clk, gen := newGen()
			cfg := testRunConfig(t, corpus, gen, clk)
			cfg.Constraint, cfg.Limit, cfg.Warmup = tt.constraint, 3, 0
			res, err := Run(context.Background(), cfg)
			if err != nil {
				t.Fatalf("%s: %v", tt.constraint, err)
			}
			if res.Config.Constraint != tt.constraint {
				t.Errorf("recorded constraint %q, want %q", res.Config.Constraint, tt.constraint)
			}
			for _, pr := range p.Prompts() {
				if pr.Chat.Output.Kind() != tt.kind {
					t.Errorf("%s: request carried constraint %q, want %q", tt.constraint, pr.Chat.Output.Kind(), tt.kind)
				}
			}
		}
	})

	t.Run("the router role", func(t *testing.T) {
		p, clk, gen := newGen()
		cfg := testRunConfig(t, corpus, gen, clk)
		cfg.Role, cfg.Limit, cfg.Warmup = providers.RoleRouter, 2, 0
		res, err := Run(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if res.Config.Role != "router" || p.Prompts()[0].Chat.EffectiveRole() != providers.RoleRouter {
			t.Errorf("role not passed through: %q", res.Config.Role)
		}
	})

	t.Run("a prompt over the budget is a failed case, not a crash", func(t *testing.T) {
		_, clk, gen := newGen()
		cfg := testRunConfig(t, corpus, gen, clk)
		cfg.Limit, cfg.Warmup, cfg.PromptBudget = 3, 0, 50
		res, err := Run(context.Background(), cfg)
		if !errors.Is(err, ErrNoCompletions) {
			t.Fatalf("err = %v, want ErrNoCompletions", err)
		}
		if res == nil || res.Errors.Count != 3 || res.Errors.ByCode["validation_failed"] != 3 {
			t.Fatalf("errors = %+v, want three validation_failed", res.Errors)
		}
		if res.Metrics.Pass.Passed != 0 || res.Metrics.SchemaValid.Passed != 0 {
			t.Errorf("a request that never ran must fail every metric: %+v", res.Metrics)
		}
	})

	t.Run("cancelling stops the run", func(t *testing.T) {
		_, clk, gen := newGen()
		cfg := testRunConfig(t, corpus, gen, clk)
		ctx, cancel := context.WithCancel(context.Background())
		cfg.Progress = func(done, _ int) {
			if done == 2 {
				cancel()
			}
		}
		res, err := Run(ctx, cfg)
		if err == nil || res != nil {
			t.Fatalf("a cancelled run must return an error and no result, got %v / %v", res, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want it to wrap context.Canceled", err)
		}
	})

	t.Run("invalid configuration is refused", func(t *testing.T) {
		_, clk, gen := newGen()
		for name, mutate := range map[string]func(*RunConfig){
			"no provider":     func(c *RunConfig) { c.Gen = nil },
			"no clock":        func(c *RunConfig) { c.Clock = nil },
			"no corpus":       func(c *RunConfig) { c.Corpus = nil },
			"no schema":       func(c *RunConfig) { c.Proposal = nil },
			"unknown profile": func(c *RunConfig) { c.Profile = "none" },
			"empty profile":   func(c *RunConfig) { c.Profile = "" },
			"embedder role":   func(c *RunConfig) { c.Role = providers.RoleEmbedder },
			"unknown constr":  func(c *RunConfig) { c.Constraint = "magic" },
			"zero max toks":   func(c *RunConfig) { c.MaxToks = 0 },
			"no timeout":      func(c *RunConfig) { c.Timeout = 0 },
			"negative warmup": func(c *RunConfig) { c.Warmup = -1 },
			"unknown prov":    func(c *RunConfig) { c.Provider = "cloud" },
		} {
			cfg := testRunConfig(t, corpus, gen, clk)
			mutate(&cfg)
			if res, err := Run(context.Background(), cfg); err == nil || res != nil {
				t.Errorf("%s: want an error and no result, got %v / %v", name, res, err)
			}
		}
	})
}

// TestOracleFailsForUnknownCases keeps the scripted provider honest: it has no
// answer for a case that is not in the corpus, like every call the fake has no
// rule for, so a run cannot pass by accident.
func TestOracleFailsForUnknownCases(t *testing.T) {
	corpus := realCorpus(t)
	o := newOracle(corpus)
	c := corpus.Cases[0]
	c.ID = "not-in-the-corpus"
	req, err := BuildRequest(c, PromptConfig{Role: providers.RoleController, Constraint: ConstraintNone, MaxToks: 64})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Chat(context.Background(), benchScope(c), req); err == nil {
		t.Error("the oracle answered a case it does not know")
	}
}
