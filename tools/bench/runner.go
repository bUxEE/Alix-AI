package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/models"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// RunConfig is everything Run needs. The command line builds it from flags;
// tests build it directly with a scripted provider and a fake clock.
type RunConfig struct {
	// Provider names the provider for the record ("fake" or "localai").
	Provider string
	// Gen answers the requests. Run wraps it with the budget below.
	Gen   providers.TextGenerator
	Clock clock.Clock

	Corpus *Corpus
	// Limit runs only the first Limit cases when positive.
	Limit int
	// Warmup requests are sent first and excluded from every metric.
	Warmup int

	Role       providers.Role
	Constraint string
	// Prompt names the template (PromptV0, PromptV1); empty means PromptID.
	Prompt string
	// Gate puts the deterministic out-of-scope gate (internal/dialogue/scopegate) in front of
	// the model: a case it refuses is answered with the typed refusal and never reaches the
	// provider (M0-21). Off by default, so every earlier run stays reproducible. Warm-up
	// requests bypass it, because the first request exists to load the model.
	Gate    bool
	Stream  bool
	MaxToks int
	// PromptBudget caps the estimated prompt of every request.
	PromptBudget int
	Timeout      time.Duration

	Profile models.Profile
	Model   ModelInfo
	Backend BackendInfo
	// ContextConfigured and NativeMax are the context facts, nil when unknown.
	ContextConfigured *int
	NativeMax         *int

	// Proposal validates the model's output (LoadProposalSchema).
	Proposal *jsonschema.Schema

	// Memory turns container memory sampling on when set.
	Memory *MemoryPlan

	Host HostInfo
	Git  GitInfo

	// Progress is called after each measured case with (done, total).
	Progress func(done, total int)
}

// ErrNoCompletions is returned with the Result when every case ended in an
// error: nothing was measured, so the caller must not publish the result.
var ErrNoCompletions = errors.New("bench: every case ended in an error, nothing was measured")

// benchScope is the verified scope the harness runs under. The session is the
// case id, which also lets the scripted fake find the case; the capability
// scopes are the case's catalogue (least capability).
func benchScope(c Case) contracts.Scope {
	caps := make([]string, len(c.Catalogue))
	for i, k := range c.Catalogue {
		caps[i] = k.ID
	}
	return contracts.Scope{
		DeploymentID: "bench", ApplicationID: "bench", TenantID: "bench", WorkspaceID: "bench",
		PrincipalID: "bench", Roles: []string{"bench"}, CapabilityScopes: caps, SessionID: c.ID,
	}
}

// callResult is what one provider call produced.
type callResult struct {
	text    *string // nil when the provider failed
	errCode string
	latency time.Duration
	ttft    time.Duration
	hasTTFT bool
	usage   providers.Usage
	finish  providers.FinishReason
	failure error // the raw error, for the caller to inspect the run's own context
	// gated is true when the out-of-scope gate answered and no request was sent.
	gated bool
}

// Run executes the corpus against the provider and returns the Result. It
// returns an error (and no result) only when the run itself cannot continue:
// invalid configuration, a cancelled context, an unusable memory sampler.
// Failures of individual cases are outcomes, not errors. When no case
// completed it returns the result together with ErrNoCompletions.
func Run(ctx context.Context, cfg RunConfig) (*Result, error) {
	if err := cfg.check(); err != nil {
		return nil, err
	}
	budget := providers.Budget{
		MaxPromptTokens: cfg.PromptBudget, MaxOutputTokens: cfg.MaxToks, MaxBatchItems: 1, Timeout: cfg.Timeout,
	}
	gen, err := providers.WithBudget(cfg.Gen, budget)
	if err != nil {
		return nil, err
	}
	limits, _ := LimitsFor(cfg.Profile)

	cases := cfg.Corpus.Cases
	if cfg.Limit > 0 && cfg.Limit < len(cases) {
		cases = cases[:cfg.Limit]
	}
	pc := PromptConfig{Role: cfg.Role, Constraint: cfg.Constraint, MaxToks: cfg.MaxToks, Prompt: cfg.Prompt}

	started := cfg.Clock.Now().UTC()
	r := &runner{cfg: cfg, gen: gen, budget: budget, pc: pc}

	var mem *memCollector
	if cfg.Memory != nil {
		if mem, err = startMemory(ctx, *cfg.Memory); err != nil {
			return nil, fmt.Errorf("memory sampling was requested but the first reading failed: %w", err)
		}
		// Fail before any case is sent: a container that is not under the limit
		// the profile stands for would make every number mislabelled.
		if lim := mem.observedLimit(); lim > 0 && !limitMatches(lim, limits.MemMB) {
			stopMemory(ctx, mem)
			return nil, fmt.Errorf("the container reports a memory limit of %.0f MiB but profile %s stands for %d MiB: fix the container or the --profile label",
				lim, cfg.Profile, limits.MemMB)
		}
	}

	var warmupFirst *float64
	for i := 0; i < cfg.Warmup; i++ {
		cr, _, err := r.call(ctx, cases[i%len(cases)], false)
		if err != nil {
			stopMemory(ctx, mem)
			return nil, err
		}
		if i == 0 && cr.text != nil {
			warmupFirst = round3(durMS(cr.latency))
		}
	}
	if mem != nil {
		mem.markMeasure()
	}

	phaseStart := cfg.Clock.Now()
	outcomes := make([]CaseOutcome, 0, len(cases))
	for i, c := range cases {
		cr, est, err := r.call(ctx, c, cfg.Gate)
		if err != nil {
			stopMemory(ctx, mem)
			return nil, err
		}
		outcomes = append(outcomes, r.outcome(c, cr, est))
		if cfg.Progress != nil {
			cfg.Progress(i+1, len(cases))
		}
	}
	duration := cfg.Clock.Now().Sub(phaseStart)

	memInfo := noMemory()
	if mem != nil {
		memInfo = mem.stop(ctx)
	}

	var limit *int
	if cfg.Limit > 0 {
		limit = intPtr(cfg.Limit)
	}
	byCat, byLang := aggregateBy(outcomes)
	res := &Result{
		FormatVersion:   FormatVersion,
		RunID:           ids.NewGenerator(cfg.Clock).New("run"),
		Timestamp:       started.Format(time.RFC3339),
		DurationMS:      *round3(durMS(duration)),
		Git:             cfg.Git,
		Host:            cfg.Host,
		Profile:         string(cfg.Profile),
		Emulated:        IsEmulated(cfg.Profile),
		Synthetic:       cfg.Provider == "fake",
		ContainerLimits: limits,
		Provider:        ProviderInfo{Name: cfg.Provider},
		Model:           cfg.Model,
		Backend:         cfg.Backend,
		Context: ContextInfo{
			ConfiguredToks: cfg.ContextConfigured, NativeMaxToks: cfg.NativeMax, PromptBudgetToks: cfg.PromptBudget,
		},
		Corpus: CorpusInfo{
			ID: cfg.Corpus.ID, SHA256: cfg.Corpus.SHA256, CasesTotal: len(cfg.Corpus.Cases), CasesRun: len(cases),
		},
		Config: ConfigInfo{
			Role: string(cfg.Role), Constraint: cfg.Constraint, Stream: cfg.Stream, Temperature: 0,
			MaxToks: cfg.MaxToks, TimeoutMS: cfg.Timeout.Milliseconds(), WarmupRequests: cfg.Warmup, Limit: limit,
			Prompt:           PromptInfo{ID: pc.promptID(), SHA256: PromptSHA256For(pc.promptID())},
			PercentileMethod: PercentileMethod,
		},
		Metrics:    Aggregate(outcomes),
		ByCategory: byCat,
		ByLanguage: byLang,
		Latency:    summarizeLatency(outcomes, warmupFirst),
		Memory:     memInfo,
		Errors:     summarizeErrors(outcomes),
		Cases:      outcomes,
	}
	if res.Errors.Count == len(outcomes) {
		return res, ErrNoCompletions
	}
	return res, nil
}

func stopMemory(ctx context.Context, m *memCollector) {
	if m != nil {
		m.stop(ctx)
	}
}

func (c RunConfig) check() error {
	switch {
	case c.Gen == nil:
		return errors.New("bench: no provider")
	case c.Clock == nil:
		return errors.New("bench: no clock")
	case c.Corpus == nil || len(c.Corpus.Cases) == 0:
		return errors.New("bench: no corpus")
	case c.Proposal == nil:
		return errors.New("bench: no TaskProposal schema")
	case !IsEmulated(c.Profile):
		return fmt.Errorf("bench: unknown profile %q", string(c.Profile))
	case c.Role != providers.RoleController && c.Role != providers.RoleRouter:
		return fmt.Errorf("bench: role %q is not served by a text generator", string(c.Role))
	case c.Constraint != ConstraintSchema && c.Constraint != ConstraintGrammar && c.Constraint != ConstraintNone:
		return fmt.Errorf("bench: unknown constraint %q", c.Constraint)
	case c.Prompt != "" && !ValidPromptID(c.Prompt):
		return fmt.Errorf("bench: unknown prompt %q", c.Prompt)
	case c.MaxToks < 1 || c.PromptBudget < 1 || c.Timeout <= 0:
		return errors.New("bench: max tokens, prompt budget and timeout must be positive")
	case c.Warmup < 0 || c.Limit < 0:
		return errors.New("bench: warm-up and limit must not be negative")
	case c.Provider != "fake" && c.Provider != "localai":
		return fmt.Errorf("bench: unknown provider %q", c.Provider)
	}
	return nil
}

type runner struct {
	cfg    RunConfig
	gen    providers.TextGenerator
	budget providers.Budget
	pc     PromptConfig
}

// call sends one case to the provider and measures it with the run's clock. It
// returns an error only when the run's own context ended; a provider failure is
// in the callResult. est is the estimated prompt size.
func (r *runner) call(ctx context.Context, c Case, useGate bool) (callResult, int, error) {
	if useGate {
		start := r.cfg.Clock.Now()
		if text, refused := gateAnswer(c); refused {
			return callResult{text: &text, latency: r.cfg.Clock.Now().Sub(start), finish: providers.FinishStop, gated: true}, 0, nil
		}
	}
	req, err := BuildRequest(c, r.pc)
	if err != nil {
		return callResult{}, 0, err
	}
	est := r.budget.EstimateChat(req)
	scope := benchScope(c)
	clk := r.cfg.Clock

	fail := func(err error) (callResult, int, error) {
		if ctx.Err() != nil {
			return callResult{}, est, fmt.Errorf("bench: stopped: %w", ctx.Err())
		}
		return callResult{errCode: string(contracts.CodeOf(err)), failure: err}, est, nil
	}

	start := clk.Now()
	if !r.cfg.Stream {
		res, err := r.gen.Chat(ctx, scope, req)
		if err != nil {
			return fail(err)
		}
		text := res.Text
		return callResult{text: &text, latency: clk.Now().Sub(start), usage: res.Usage, finish: res.FinishReason}, est, nil
	}

	s, err := r.gen.ChatStream(ctx, scope, req)
	if err != nil {
		return fail(err)
	}
	defer s.Close()
	var (
		sb      strings.Builder
		cr      callResult
		sawDone bool
	)
	for {
		ch, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fail(err)
		}
		if ch.Delta != "" && !cr.hasTTFT {
			cr.ttft, cr.hasTTFT = clk.Now().Sub(start), true
		}
		sb.WriteString(ch.Delta)
		if ch.Done {
			sawDone, cr.usage, cr.finish = true, ch.Usage, ch.FinishReason
		}
	}
	if !sawDone {
		return fail(contracts.New(contracts.ErrUnavailable, "stream ended before completion"))
	}
	cr.latency = clk.Now().Sub(start)
	text := sb.String()
	cr.text = &text
	return cr, est, nil
}

// outcome scores a call and adds the measurements.
func (r *runner) outcome(c Case, cr callResult, est int) CaseOutcome {
	o := ScoreCase(c, cr.text, cr.errCode, r.cfg.Proposal)
	o.PromptToksEst = est
	if cr.text == nil {
		o.Status = StatusError
		o.ErrorCode = strPtr(cr.errCode)
		return o
	}
	o.Status = StatusOK
	o.LatencyMS = round3(durMS(cr.latency))
	if cr.gated {
		// No model ran: no tokens went in or out, and the answer is not an estimate.
		o.ToksIn, o.ToksOut = intPtr(0), intPtr(0)
		o.FinishReason = strPtr(string(cr.finish))
		return o
	}
	if cr.hasTTFT {
		o.TTFTMS = round3(durMS(cr.ttft))
	}
	toksIn, toksOut := cr.usage.InputTokens, cr.usage.OutputTokens
	if toksOut == 0 && *cr.text != "" {
		toksOut, o.ToksEstimated = providers.EstimateTokens(*cr.text), true
	}
	o.ToksIn, o.ToksOut = intPtr(toksIn), intPtr(toksOut)
	if cr.finish != "" {
		o.FinishReason = strPtr(string(cr.finish))
	}
	return o
}

// durMS converts a duration to milliseconds with microsecond precision.
func durMS(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// summarizeLatency derives the latency block from the outcomes alone, which is
// how `bench check` verifies it.
func summarizeLatency(cases []CaseOutcome, warmupFirst *float64) LatencyInfo {
	var totals, ttfts, prompts []float64
	var toksIn, toksOut, decodeToks int
	var totalMS, decodeMS float64
	for _, c := range cases {
		if c.Status != StatusOK || c.LatencyMS == nil {
			continue
		}
		totals = append(totals, *c.LatencyMS)
		prompts = append(prompts, float64(c.PromptToksEst))
		totalMS += *c.LatencyMS
		out := 0
		if c.ToksOut != nil {
			out = *c.ToksOut
			toksOut += out
		}
		if c.ToksIn != nil {
			toksIn += *c.ToksIn
		}
		if c.TTFTMS != nil {
			ttfts = append(ttfts, *c.TTFTMS)
			if gen := *c.LatencyMS - *c.TTFTMS; gen > 0 {
				decodeMS += gen
				decodeToks += out
			}
		}
	}
	info := LatencyInfo{
		TotalMS: Summarize(totals), TTFTMS: Summarize(ttfts), PromptToksEst: Summarize(prompts),
		ToksIn: toksIn, ToksOut: toksOut, WarmupFirstRequestMS: warmupFirst,
	}
	if totalMS > 0 {
		info.ToksPerSecond = round3(float64(toksOut) / (totalMS / 1000))
	}
	if decodeMS > 0 {
		info.DecodeToksPerSecond = round3(float64(decodeToks) / (decodeMS / 1000))
	}
	return info
}

// summarizeErrors counts the cases that ended in an error by code.
func summarizeErrors(cases []CaseOutcome) ErrorsInfo {
	e := ErrorsInfo{ByCode: map[string]int{}}
	for _, c := range cases {
		if c.Status == StatusError {
			e.Count++
			if c.ErrorCode != nil {
				e.ByCode[*c.ErrorCode]++
			}
		}
	}
	return e
}
