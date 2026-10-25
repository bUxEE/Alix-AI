package dialogue

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/buxee/alix-ai/internal/adapters"
	"github.com/buxee/alix-ai/internal/capabilities"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/providers"
	pfake "github.com/buxee/alix-ai/internal/providers/fake"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	act "github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// ---- the decision stage ----

type stubStage struct {
	mu    sync.Mutex
	res   DecisionResult
	panic bool
	delay time.Duration
	reqs  []DecisionRequest
}

func (s *stubStage) Decide(ctx context.Context, req DecisionRequest) DecisionResult {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	s.mu.Unlock()
	if s.panic {
		panic("scorer crashed")
	}
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
		}
	}
	return s.res
}

func (s *stubStage) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func withDecision(s DecisionStage) envOpt { return withDeps(func(d *Deps) { d.Decision = s }) }

// The default stage is a no-op that always falls back: the pipeline is the one
// there would be with no scorer at all.
func TestDecisionStageDefaultFallsBackAndChangesNothing(t *testing.T) {
	run := func(opts ...envOpt) (Result, string) {
		e := newEnv(t, opts...)
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
		e.onResponse("Please confirm: create an item named widget.")
		res := e.say(memberScope(), sayCreate)
		return res, e.allPromptText()
	}
	def, defPrompts := run()
	explicit, explicitPrompts := run(withDecision(NoopDecisionStage{}))
	if def.Trace.Decision != string(FallbackDisabled) || explicit.Trace.Decision != string(FallbackDisabled) {
		t.Fatalf("decision = %q / %q, want %q", def.Trace.Decision, explicit.Trace.Decision, FallbackDisabled)
	}
	if def.Directive.Act != explicit.Directive.Act || def.Response.Text != explicit.Response.Text || defPrompts != explicitPrompts {
		t.Fatal("the pipeline differs between the default stage and an explicit no-op stage")
	}
	if got := (NoopDecisionStage{}).Decide(context.Background(), DecisionRequest{}); got.Selected != nil || got.Fallback != FallbackDisabled {
		t.Fatalf("no-op stage = %+v", got)
	}
}

// A selected candidate narrows what the controller is offered, and nothing else:
// the capability still goes through the policy gateway, the host and the
// confirmation.
func TestDecisionStageSelectionNarrowsPromptButSkipsNothing(t *testing.T) {
	stage := &stubStage{res: DecisionResult{Selected: &DecisionSelection{CapabilityID: capCreate, Confidence: 0.93}}}
	e := newEnv(t, withDecision(stage))
	e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onResponse("Please confirm: create an item named widget.")

	res := e.say(memberScope(), sayCreate)

	if res.Trace.Decision != "selected" {
		t.Fatalf("decision = %q", res.Trace.Decision)
	}
	if stage.calls() != 1 {
		t.Fatalf("stage calls = %d", stage.calls())
	}
	// The stage saw the bounded, authorised candidate set and no values.
	req := stage.reqs[0]
	if len(req.Candidates) < 2 || len(req.Candidates) > capabilities.ShortlistCeiling {
		t.Fatalf("candidates = %+v", req.Candidates)
	}
	for _, c := range req.Candidates {
		if c.CapabilityID == capAdmin {
			t.Fatalf("an unauthorised capability reached the stage: %+v", c)
		}
	}
	if req.Scope.TenantID != tenantA || req.Utterance == "" {
		t.Fatalf("stage request = %+v", req)
	}
	// The prompt lists only the selected capability.
	prompt := strings.Join(e.prompts()[0].Texts(), "\n")
	mustContain(t, prompt, capCreate, "the proposal prompt")
	mustNotContain(t, prompt, capGet, "the proposal prompt")
	// Policy, prepare and confirmation all still ran.
	wantAct(t, res, "request_confirmation")
	if res.Prepared == nil || e.hostCalls(adapters.OpPrepare) != 1 || e.hostCalls(adapters.OpCommit) != 0 || e.app.Effects() != 0 {
		t.Fatalf("the selection skipped a step: host calls %v", e.app.Calls())
	}
	if e.recs.denied(capCreate, "prepare") != 0 {
		t.Fatal("unexpected denial")
	}
	// The model was still asked for the typed task, and the response was still verified.
	if len(res.Trace.ModelCalls) != 2 || res.Response.Reason != verify.ReasonVerified {
		t.Fatalf("trace = %+v, response = %+v", res.Trace.ModelCalls, res.Response)
	}
}

func TestDecisionStageCannotSelectOutsideTheAuthorisedSet(t *testing.T) {
	cases := map[string]struct {
		stage *stubStage
		want  FallbackReason
	}{
		"not a candidate":        {&stubStage{res: DecisionResult{Selected: &DecisionSelection{CapabilityID: capAdmin, Confidence: 0.99}}}, FallbackCandidateSetReject},
		"unknown id":             {&stubStage{res: DecisionResult{Selected: &DecisionSelection{CapabilityID: "items.nope", Confidence: 0.99}}}, FallbackCandidateSetReject},
		"selects and falls back": {&stubStage{res: DecisionResult{Selected: &DecisionSelection{CapabilityID: capCreate}, Fallback: FallbackLowConfidence}}, FallbackMalformedOutput},
		"neither":                {&stubStage{res: DecisionResult{}}, FallbackMalformedOutput},
		"unknown reason":         {&stubStage{res: DecisionResult{Fallback: "because"}}, FallbackMalformedOutput},
		"confidence is NaN":      {&stubStage{res: DecisionResult{Selected: &DecisionSelection{CapabilityID: capCreate, Confidence: math.NaN()}}}, FallbackMalformedOutput},
		"confidence over one":    {&stubStage{res: DecisionResult{Selected: &DecisionSelection{CapabilityID: capCreate, Confidence: 4}}}, FallbackMalformedOutput},
		"a typed fallback":       {&stubStage{res: DecisionResult{Fallback: FallbackLowConfidence}}, FallbackLowConfidence},
		"a crash":                {&stubStage{panic: true}, FallbackUnavailable},
		"too slow":               {&stubStage{delay: 3 * time.Second, res: DecisionResult{Selected: &DecisionSelection{CapabilityID: capCreate, Confidence: 1}}}, FallbackTimeout},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, withDecision(c.stage), withConfig(Config{DecisionTimeout: 40 * time.Millisecond}))
			e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
			e.onResponse("Please confirm: create an item named widget.")
			res := e.say(memberScope(), sayCreate)
			if res.Trace.Decision != string(c.want) {
				t.Fatalf("decision = %q, want %q", res.Trace.Decision, c.want)
			}
			// Whatever the stage did, the turn is the one there would be without it.
			wantAct(t, res, "request_confirmation")
			prompt := strings.Join(e.prompts()[0].Texts(), "\n")
			mustContain(t, prompt, capCreate, "the proposal prompt")
			mustContain(t, prompt, capGet, "the proposal prompt")
		})
	}
}

func TestDecisionStageIsNotCalledForDeterministicOutcomes(t *testing.T) {
	stage := &stubStage{res: DecisionResult{Fallback: FallbackDisabled}}
	e := newEnv(t, withDecision(stage))
	e.say(memberScope(), "what is the weather like in Paris today") // router: out of scope
	e.say(memberScope(), "what can you do")                         // exact rule: help
	if stage.calls() != 0 {
		t.Fatalf("the stage was called %d times for requests the deterministic rules settle", stage.calls())
	}
}

func TestFallbackReasonsAreTheStableStrings(t *testing.T) {
	want := []string{"disabled", "unavailable", "timeout", "malformed_output", "low_confidence", "calibration_failed",
		"ood_unsupported_shape", "model_mismatch", "tenant_mismatch", "policy_conflict", "candidate_set_rejected",
		"resource_gate", "unsupported_runtime"}
	got := []FallbackReason{FallbackDisabled, FallbackUnavailable, FallbackTimeout, FallbackMalformedOutput, FallbackLowConfidence,
		FallbackCalibrationFailed, FallbackOODUnsupportedShape, FallbackModelMismatch, FallbackTenantMismatch, FallbackPolicyConflict,
		FallbackCandidateSetReject, FallbackResourceGate, FallbackUnsupportedRuntime}
	for i, w := range want {
		if string(got[i]) != w || !got[i].Valid() {
			t.Fatalf("reason %d = %q, want %q", i, got[i], w)
		}
	}
}

// ---- the prompt guard and the provider selector ----

type recGuard struct {
	mu     sync.Mutex
	inputs []GuardInput
	fn     func(GuardInput) (PromptRequest, error)
}

func (g *recGuard) Guard(_ context.Context, in GuardInput) (PromptRequest, error) {
	g.mu.Lock()
	g.inputs = append(g.inputs, in)
	g.mu.Unlock()
	if g.fn != nil {
		return g.fn(in)
	}
	return in.Request, nil
}

func (g *recGuard) seen() []GuardInput {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]GuardInput(nil), g.inputs...)
}

func withGuard(g PromptGuard) envOpt { return withDeps(func(d *Deps) { d.Guard = g }) }

func TestPromptGuardSeesEveryRequestWithItsMetadata(t *testing.T) {
	g := &recGuard{}
	e := newEnv(t, withGuard(g))
	e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e.onResponse("Please confirm: create an item named widget.")
	e.say(memberScope(), sayCreate)

	seen := g.seen()
	if len(seen) != 2 || len(e.prompts()) != 2 {
		t.Fatalf("guard calls = %d, provider calls = %d, want 2 each: every request passes the guard", len(seen), len(e.prompts()))
	}
	for _, in := range seen {
		if !in.Provider.Local || in.Provider.Name == "" {
			t.Fatalf("provider = %+v", in.Provider)
		}
		if in.Scope.TenantID != tenantA {
			t.Fatalf("scope = %+v", in.Scope)
		}
		for _, it := range in.Request.Items {
			if it.Meta.Kind == "" || it.Meta.SourceID == "" || it.Meta.Class.Rank() > ClassRestricted.Rank() {
				t.Fatalf("an item carries no usable metadata: %+v", it.Meta)
			}
			if it.Role == providers.MsgSystem && !it.Trusted {
				t.Fatal("a system item is not trusted")
			}
		}
	}
	// Proposal: frame + the user's message. Response: frame + the directive, which is
	// the application's data and never leaves the machine.
	kinds := func(in GuardInput) []ItemKind {
		var out []ItemKind
		for _, it := range in.Request.Items {
			out = append(out, it.Meta.Kind)
		}
		return out
	}
	if k := kinds(seen[0]); len(k) != 2 || k[0] != ItemFrame || k[1] != ItemUserMessage {
		t.Fatalf("proposal items = %v", k)
	}
	if k := kinds(seen[1]); len(k) != 2 || k[0] != ItemFrame || k[1] != ItemDirective {
		t.Fatalf("response items = %v", k)
	}
	d := seen[1].Request.Items[1].Meta
	if !d.LocalOnly || d.Class != ClassRestricted {
		t.Fatalf("directive metadata = %+v, want restricted and local-only", d)
	}
	if u := seen[0].Request.Items[1].Meta; u.Class != ClassConfidential || u.LocalOnly {
		t.Fatalf("user message metadata = %+v", u)
	}
}

func TestPromptRequestStripsItsMetadata(t *testing.T) {
	p := PromptRequest{
		Role: providers.RoleController, MaxTokens: 9, Temperature: 0.5,
		Items: []ContextItem{
			{Role: providers.MsgSystem, Trusted: true, Content: "frame", Meta: ItemMeta{Kind: ItemFrame, SourceID: "package:x", Class: ClassInternal}},
			{Role: providers.MsgUser, Content: "hello", Meta: ItemMeta{Kind: ItemUserMessage, SourceID: "SECRET-SOURCE", Class: ClassRestricted, LocalOnly: true}},
		},
	}
	req := p.ChatRequest()
	if len(req.Messages) != 2 || req.Messages[0].Content != "frame" || !req.Messages[0].Trusted || req.Messages[1].Trusted || req.MaxTokens != 9 {
		t.Fatalf("request = %+v", req)
	}
	b := strings.Join([]string{req.Messages[0].Content, req.Messages[1].Content}, "|")
	mustNotContain(t, b, "SECRET-SOURCE", "the request")
}

// A guard may filter the request; the filtered request is what is sent.
func TestPromptGuardFiltersWhatIsSent(t *testing.T) {
	g := &recGuard{fn: func(in GuardInput) (PromptRequest, error) {
		out := in.Request
		out.Items = nil
		for _, it := range in.Request.Items {
			if it.Meta.Kind != ItemState && it.Meta.Kind != ItemTurn {
				out.Items = append(out.Items, it)
			}
		}
		return out, nil
	}}
	e := newEnv(t, withGuard(g))
	e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget"}))
	e.onResponse("Sure. How many would you like?")
	e.say(memberScope(), "create an item named widget")
	e.say(memberScope(), "quantity is 4")

	p := e.prompts()[2] // the proposal of the second turn
	text := strings.Join(p.Texts(), "\n")
	mustNotContain(t, text, "<session_state>", "the filtered prompt")
	mustNotContain(t, text, "<earlier_message>", "the filtered prompt")
	// The guard removed state and history, so the runtime saw the frame and the message only.
	if n := len(p.Chat.Messages); n != 2 {
		t.Fatalf("messages sent = %d, want 2", n)
	}
}

func TestPromptGuardDenialStopsTheCallAndTheTurn(t *testing.T) {
	g := &recGuard{fn: func(GuardInput) (PromptRequest, error) {
		return PromptRequest{}, contracts.New(contracts.ErrPermissionDenied, "data egress denied")
	}}
	e := newEnv(t, withGuard(g))
	res := e.say(memberScope(), sayCreate)
	wantAct(t, res, "refuse")
	if res.Code != contracts.ErrPermissionDenied {
		t.Fatalf("code = %s", res.Code)
	}
	if len(e.prompts()) != 0 || len(e.app.Calls()) != 0 {
		t.Fatalf("a denied prompt was sent (%d) or acted on (%d host calls)", len(e.prompts()), len(e.app.Calls()))
	}
}

// A denied response prompt degrades to the template; it never hides a receipt.
func TestPromptGuardDenialOnTheResponseKeepsTheReceipt(t *testing.T) {
	g := &recGuard{fn: func(in GuardInput) (PromptRequest, error) {
		for _, it := range in.Request.Items {
			if it.Meta.Kind == ItemDirective {
				return PromptRequest{}, contracts.New(contracts.ErrPermissionDenied, "records may not leave")
			}
		}
		return in.Request, nil
	}}
	e := newEnv(t, withGuard(g), withLevel(level2))
	e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	res := e.say(memberScope(), sayCreate)
	wantAct(t, res, "request_confirmation")
	if res.Response.Source != verify.SourceTemplate || res.Response.Reason != verify.ReasonDegraded {
		t.Fatalf("response = %+v, want the template", res.Response)
	}
	out := e.turn(memberScope(), TurnInput{Confirm: &Confirmation{PreparationID: string(res.Prepared.PreparationID), Token: res.Confirmation}})
	if out.Receipt == nil || out.Receipt.Status != act.ActionReceiptStatusCommitted || e.app.Effects() != 1 {
		t.Fatalf("receipt = %+v, effects = %d", out.Receipt, e.app.Effects())
	}
	if out.Response.Source != verify.SourceTemplate {
		t.Fatalf("response = %+v", out.Response)
	}
}

func TestDefaultGuardAllowsLocalAndDeniesAnythingElse(t *testing.T) {
	req := PromptRequest{Items: []ContextItem{{Role: providers.MsgUser, Content: "x"}}}
	got, err := LocalOnlyGuard{}.Guard(context.Background(), GuardInput{Provider: ProviderInfo{Name: "local", Local: true}, Request: req})
	if err != nil || len(got.Items) != 1 {
		t.Fatalf("local: %v %+v", err, got)
	}
	_, err = LocalOnlyGuard{}.Guard(context.Background(), GuardInput{Provider: ProviderInfo{Name: "hosted", Local: false}, Request: req})
	wantCode(t, err, contracts.ErrPermissionDenied)
}

type fixedSelector struct {
	p   Provider
	err error
}

func (s fixedSelector) Select(context.Context, contracts.Scope, providers.Role) (Provider, error) {
	return s.p, s.err
}

func TestProviderSelectorErrorFailsTheTurnClosed(t *testing.T) {
	e := newEnv(t, withDeps(func(d *Deps) {
		d.Providers = fixedSelector{err: contracts.New(contracts.ErrUnavailable, "no provider is configured")}
	}))
	res := e.say(memberScope(), sayCreate)
	wantAct(t, res, "report_failure")
	if res.Code != contracts.ErrUnavailable || len(e.app.Calls()) != 0 {
		t.Fatalf("code = %s, host calls = %d", res.Code, len(e.app.Calls()))
	}
}

// With the default guard, a provider that is not local is never sent a prompt: only
// a real guard may release context to one, and no hidden fallback exists.
func TestNonLocalProviderIsDeniedWithoutARealGuard(t *testing.T) {
	hosted := pfake.New()
	hosted.On(pfake.Any()).Reply(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
	e := newEnv(t, withDeps(func(d *Deps) {
		d.Providers = fixedSelector{p: Provider{Name: "hosted", Local: false, Generator: hosted}}
	}))
	res := e.say(memberScope(), sayCreate)
	wantAct(t, res, "refuse")
	if res.Code != contracts.ErrPermissionDenied {
		t.Fatalf("code = %s", res.Code)
	}
	if len(hosted.Prompts()) != 0 || len(e.prompts()) != 0 {
		t.Fatal("a prompt reached a provider that is not local")
	}
	e.noHostWrites()
}

// Local mode is unchanged: the explicit default guard and selector give the same
// prompts and the same turn as the defaults.
func TestLocalModeIsTheSameWithExplicitDefaults(t *testing.T) {
	run := func(opts ...envOpt) (Result, string) {
		e := newEnv(t, opts...)
		e.onProposal(proposalJSON(capCreate, "task", map[string]any{"name": "widget", "quantity": 3}))
		e.onResponse("Please confirm: create an item named widget.")
		res := e.say(memberScope(), sayCreate)
		return res, e.allPromptText()
	}
	a, ap := run()
	b, bp := run(withGuard(LocalOnlyGuard{}), withDecision(NoopDecisionStage{}))
	if a.Directive.Act != b.Directive.Act || a.Response.Text != b.Response.Text || ap != bp {
		t.Fatal("explicit defaults changed a Local mode turn")
	}
}

func TestLocalSelectorWrapsTheBudget(t *testing.T) {
	if _, err := LocalSelector(nil, defaultBudget()); err == nil {
		t.Fatal("a selector over no generator was built")
	}
	bad := defaultBudget()
	bad.MaxPromptTokens = 0
	if _, err := LocalSelector(pfake.New(), bad); err == nil {
		t.Fatal("a selector over a zero budget was built")
	}
	sel, err := LocalSelector(pfake.New(), defaultBudget())
	if err != nil {
		t.Fatal(err)
	}
	p, err := sel.Select(context.Background(), memberScope(), providers.RoleController)
	if err != nil || !p.Local || p.Generator == nil {
		t.Fatalf("provider = %+v, %v", p, err)
	}
}

// ---- the rate limiter ----

func TestWindowLimiterAllowsUpToTheLimitThenRefuses(t *testing.T) {
	clk := clock.NewFake(t0)
	l := NewWindowLimiter(clk, RateConfig{MaxTurns: 3, Window: time.Minute})
	sc := memberScope()
	for i := 0; i < 3; i++ {
		if err := l.Allow(context.Background(), sc); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
	}
	err := l.Allow(context.Background(), sc)
	wantCode(t, err, contracts.ErrRateLimited)
	if !contracts.CodeOf(err).Retryable() {
		t.Fatal("rate_limited must be retryable")
	}
	// Another principal has its own window.
	other := sc
	other.PrincipalID = "usr-other"
	if err := l.Allow(context.Background(), other); err != nil {
		t.Fatalf("another principal: %v", err)
	}
	// The window slides.
	clk.Advance(61 * time.Second)
	if err := l.Allow(context.Background(), sc); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	// An invalid scope is refused, never counted.
	bad := sc
	bad.TenantID = ""
	wantCode(t, l.Allow(context.Background(), bad), contracts.ErrScopeMissing)
}

func TestWindowLimiterIsBounded(t *testing.T) {
	clk := clock.NewFake(t0)
	l := NewWindowLimiter(clk, RateConfig{MaxTurns: 1, Window: time.Minute, MaxKeys: 2})
	for _, p := range []string{"a", "b"} {
		sc := memberScope()
		sc.PrincipalID = p
		if err := l.Allow(context.Background(), sc); err != nil {
			t.Fatal(err)
		}
	}
	sc := memberScope()
	sc.PrincipalID = "c"
	wantCode(t, l.Allow(context.Background(), sc), contracts.ErrResourceExhausted)
	clk.Advance(2 * time.Minute) // idle keys are evicted
	if err := l.Allow(context.Background(), sc); err != nil {
		t.Fatalf("after eviction: %v", err)
	}
}

func TestTurnIsRateLimitedBeforeAnyWork(t *testing.T) {
	e := newEnv(t, withDeps(func(d *Deps) { d.Limiter = NewWindowLimiter(d.Clock, RateConfig{MaxTurns: 1, Window: time.Hour}) }))
	e.say(memberScope(), "what is the weather like in Paris today")
	_, err := e.eng.Turn(ctxb(), memberScope(), TurnInput{Text: sayCreate})
	wantCode(t, err, contracts.ErrRateLimited)
	if len(e.prompts()) != 0 || len(e.app.Calls()) != 0 {
		t.Fatal("a rate-limited turn did work")
	}
}
