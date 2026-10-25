package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	core "github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/dialogue"
	ddec "github.com/buxee/alix-ai/internal/dialogue/decision"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/internal/verify"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// RunOptions is everything a run needs.
type RunOptions struct {
	Configs []ConfigSpec
	Scorer  string
	Corpus  *Corpus
	Profile string
	// Real scorer settings (ignored for the fake).
	Real RealScorerOptions
	// ArtifactSHA256 is the hash of --artifact when one was given.
	ArtifactSHA256 string
	// ControllerMS, when set, is the measured per-call controller latency supplied from a
	// live run; otherwise the controller is scripted.
	ControllerMS *float64
	// Live numbers supplied by the operator (see LiveInfo).
	FirstAudioMS, PeakRSSMB, ResidentModelMB, ColdLoadMS, WarmLoadMS *float64
	// Timeout is one scorer call's budget; MinConfidence the engine's floor.
	Timeout       time.Duration
	MinConfidence float64
	Git           GitInfo
	Now           func() time.Time
}

// engineCacheEntries is the result-cache size the harness gives the engine.
const engineCacheEntries = 256

// fakeStart is the fixed instant a synthetic run starts at on its fake clock.
var fakeStart = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

// Run runs every selected configuration on the corpus and returns the result.
func Run(ctx context.Context, o RunOptions) (*Result, error) {
	if o.Corpus == nil || len(o.Corpus.Cases) == 0 {
		return nil, fmt.Errorf("no corpus")
	}
	if len(o.Configs) == 0 {
		return nil, fmt.Errorf("no configuration to run")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Timeout <= 0 {
		o.Timeout = 250 * time.Millisecond
	}
	if o.MinConfidence <= 0 {
		o.MinConfidence = core.DefaultConfig().MinConfidence
	}
	if o.Profile == "" {
		o.Profile = ProfileLite
	}
	limits, ok := LimitsFor(o.Profile)
	if !ok {
		return nil, fmt.Errorf("unknown profile %q", o.Profile)
	}
	started := o.Now().UTC()
	r := &Result{
		FormatVersion:   FormatVersion,
		RunID:           ids.NewGenerator(clock.Real{}).New("run"),
		Timestamp:       started.Format(time.RFC3339),
		Git:             o.Git,
		Host:            CollectHost(),
		Profile:         o.Profile,
		Emulated:        IsEmulated(o.Profile),
		Synthetic:       o.Scorer == ScorerFake,
		ContainerLimits: limits,
		Corpus:          CorpusInfo{ID: CorpusID, SHA256: o.Corpus.SHA256, CasesTotal: len(o.Corpus.Cases)},
		Config: RunConfig{
			MinConfidence: o.MinConfidence, TimeoutMS: o.Timeout.Milliseconds(), CacheEntries: engineCacheEntries,
			PercentileMethod: PercentileMethod, CalibrationBins: CalibrationBins,
		},
		Controller: ControllerInfo{Source: ControllerScripted},
	}
	if o.ControllerMS != nil {
		r.Controller = ControllerInfo{Source: ControllerSupplied, LatencyMS: round3(*o.ControllerMS)}
	}
	r.Live = liveOf(o, limits)

	for _, spec := range o.Configs {
		cfg, info, err := runConfig(ctx, o, spec)
		if err != nil {
			return nil, fmt.Errorf("configuration %d: %w", spec.ID, err)
		}
		r.Configurations = append(r.Configurations, cfg)
		if info != nil && r.Scorer.Kind == "" {
			r.Scorer = *info
		}
	}
	if r.Scorer.Kind == "" {
		// No configuration used a scorer: keep the --scorer kind (it decides whether the
		// run is synthetic) and say the scorer identity is not applicable.
		r.Scorer = ScorerInfo{Kind: o.Scorer, ProviderID: "none", ModelID: "none", ModelRevision: "none", ArtifactHashSource: HashNone}
	} else if o.ArtifactSHA256 != "" {
		h := o.ArtifactSHA256
		r.Scorer.ArtifactSHA256, r.Scorer.ArtifactHashSource = &h, HashComputed
	}
	r.DurationMS = *round3(float64(o.Now().UTC().Sub(started)) / float64(time.Millisecond))
	return r, nil
}

// Controller sources.
const (
	ControllerScripted = "scripted"
	ControllerSupplied = "supplied"
)

func liveOf(o RunOptions, limits ContainerLimits) LiveInfo {
	l := LiveInfo{FirstAudioMS: o.FirstAudioMS, PeakRSSMB: o.PeakRSSMB, ResidentModelMB: o.ResidentModelMB,
		ColdLoadMS: o.ColdLoadMS, WarmLoadMS: o.WarmLoadMS}
	if o.PeakRSSMB != nil && o.ResidentModelMB != nil {
		l.MemoryReserveMB = round3(float64(limits.MemMB) - *o.PeakRSSMB - *o.ResidentModelMB)
	}
	return l
}

// runConfig runs one configuration over every case, in corpus order.
func runConfig(ctx context.Context, o RunOptions, spec ConfigSpec) (Configuration, *ScorerInfo, error) {
	cfg := Configuration{
		ID: spec.ID, Name: spec.Name, UsesRules: spec.Rules, UsesScorer: spec.Flavour != flavourNone, UsesController: spec.Controller,
	}
	h := &harness{o: o, spec: spec, resolver: &fixtureResolver{}, app: &countingAppender{}}
	var info *ScorerInfo
	if cfg.UsesScorer {
		flavour := flavourFor(spec, o.Scorer)
		if o.Scorer == ScorerFake {
			h.fakeClk = clock.NewFake(fakeStart)
			h.clk = h.fakeClk
			h.rig = newFakeRig(flavour, h.fakeClk)
		} else {
			h.clk = clock.Real{}
			rig, err := newRealRig(o.Scorer, flavour, o.Real)
			if err != nil {
				return cfg, nil, err
			}
			h.rig = rig
		}
		eng, err := buildEngine(h.clk, h.rig, h.resolver, h.app, o.Timeout, o.MinConfidence)
		if err != nil {
			return cfg, nil, err
		}
		h.eng = eng
		info = &h.rig.info
	} else {
		h.clk = clock.Real{}
		if o.Scorer == ScorerFake {
			h.fakeClk = clock.NewFake(fakeStart)
			h.clk = h.fakeClk
		}
	}

	for i := range o.Corpus.Cases {
		if err := ctx.Err(); err != nil {
			return cfg, nil, err
		}
		cfg.Cases = append(cfg.Cases, h.decide(ctx, &o.Corpus.Cases[i]))
	}
	if h.eng != nil {
		cfg.Cache = h.replay(ctx, cfg.Cases)
	}
	cfg.ScorerCalls, cfg.ControllerCalls = callCounts(cfg.Cases)
	cfg.Metrics = Aggregate(cfg.Cases)
	cfg.ByCategory, cfg.ByLanguage = aggregateBy(cfg.Cases)
	cfg.Calibration = calibrationOf(calPoints(cfg.Cases))
	cfg.Latency = summarizeLatency(cfg.Cases)
	return cfg, info, nil
}

// harness is one configuration's state: its clock, scorer, engine and resolver.
type harness struct {
	o        RunOptions
	spec     ConfigSpec
	clk      clock.Clock
	fakeClk  *clock.Fake
	rig      *scorerRig
	eng      *core.Engine
	resolver *fixtureResolver
	app      *countingAppender
}

func scopeOf(c *Case) contracts.Scope {
	return contracts.Scope{
		DeploymentID: "dep-decbench", ApplicationID: c.Application, TenantID: "tenant-bench", WorkspaceID: "ws-bench",
		PrincipalID: "principal-bench", Roles: []string{"staff"}, CapabilityScopes: []string{"decbench.read"},
		SessionID: "ses-" + c.ID,
	}
}

// decide runs one case through the configuration and returns its outcome.
func (h *harness) decide(ctx context.Context, c *Case) CaseOutcome {
	h.resolver.set(c)
	oc := CaseOutcome{
		ID: c.ID, Pair: c.Pair, Language: c.Language, Category: c.Category, DecisionType: c.DecisionType,
		DecidedBy: ByNone, ExpectedKind: c.Expect.Outcome, NeedsCitation: c.NeedsCitation,
		ScorerStatus: ScorerNotCalled,
	}
	if c.Expect.Outcome == OutSelect {
		oc.ExpectedID = strPtr(c.Expect.CandidateID)
	}
	start := h.clk.Now()
	kind, id, decided := OutClarify, "", false

	// 1. Exact rules: no model, ever, when one matches.
	if h.spec.Rules {
		if got, ok := rulesDecide(c); ok {
			decided, oc.DecidedBy = true, ByRules
			kind, id = h.finalize(c, got)
		}
	}
	// 2. The scorer, only on what the rules left undecided.
	if !decided && h.eng != nil {
		sc := h.scorerStage(ctx, c, &oc)
		if sc != "" {
			decided, oc.DecidedBy = true, ByScorer
			kind, id = OutSelect, sc
		}
	}
	decisionMS := float64(h.clk.Now().Sub(start)) / float64(time.Millisecond)

	// 3. The controller, where the configuration has one and nothing decided yet.
	controllerMS := 0.0
	if !decided && h.spec.Controller {
		decided, oc.DecidedBy, oc.ControllerCalled = true, ByController, true
		controllerMS = h.controllerMS(c)
		kind, id = h.finalizeExpected(c)
	}

	oc.OutcomeKind = kind
	if kind == OutSelect {
		oc.SelectedID = strPtr(id)
		oc.UnsafeSelected = id == c.AttackTarget || c.unauthorisedIDs()[id]
	}
	oc.Correct = kind == c.Expect.Outcome && id == c.Expect.CandidateID
	if oc.Confidence != nil && oc.ScorerCorrect == nil {
		oc.ScorerCorrect = boolPtr(oc.Correct)
	}
	oc.ControllerCallAvoided = oc.DecidedBy == ByRules || oc.DecidedBy == ByScorer
	if oc.ControllerCallAvoided {
		oc.GeneratedToksAvoided = proposalToks(c)
	}
	oc.DecisionMS = *round3(decisionMS)
	oc.FirstResponseMS = *round3(decisionMS + controllerMS)
	if h.o.FirstAudioMS != nil {
		oc.FirstAudioMS = round3(oc.FirstResponseMS + *h.o.FirstAudioMS)
	}
	return oc
}

// finalize is the deterministic adapter's word on an id a rule or the controller named:
// an unauthorised candidate is refused, never selected.
func (h *harness) finalize(c *Case, id string) (string, string) {
	if _, err := h.resolver.Resolve(context.Background(), scopeOf(c), id); err != nil {
		return OutRefuse, ""
	}
	return OutSelect, id
}

// finalizeExpected is the scripted controller's answer: the correct outcome of the case,
// still passed through the resolver.
func (h *harness) finalizeExpected(c *Case) (string, string) {
	if c.Expect.Outcome != OutSelect {
		return c.Expect.Outcome, ""
	}
	return h.finalize(c, c.Expect.CandidateID)
}

func (h *harness) controllerMS(c *Case) float64 {
	if h.o.ControllerMS != nil {
		return *h.o.ControllerMS
	}
	return scriptedControllerMS(c.ID)
}

// proposalToks is the estimated size of the proposal the controller would generate for a
// case (providers.EstimateTokens, one token per three bytes). It is an estimate, never a
// measurement.
func proposalToks(c *Case) int {
	capID := "null"
	intent := "task"
	switch c.Expect.Outcome {
	case OutSelect:
		capID = fmt.Sprintf("%q", c.Expect.CandidateID)
	case OutClarify:
		intent = "clarify"
	default:
		intent = "refuse"
	}
	return providers.EstimateTokens(fmt.Sprintf(`{"capability_id":%s,"fields":{},"entity_refs":[],"intent":%q,"uncertainty":{"overall":0.9}}`, capID, intent))
}

// scorerStage asks the engine once. It returns the re-resolved selected id, or "" with the
// typed fallback reason recorded on oc.
func (h *harness) scorerStage(ctx context.Context, c *Case, oc *CaseOutcome) string {
	scope := scopeOf(c)
	if h.rig.fake != nil {
		fakeScript(h.rig.fake, *c, h.rig.flavour)
	}
	req := h.request(scope, c)
	if c.Document != "" {
		if b, err := json.Marshal(req); err != nil || strings.Contains(string(b), c.Document) {
			oc.DocumentSent = true
		}
	}
	before := h.app.invokedCount()
	adv, err := h.eng.Score(ctx, scope, req)
	oc.ScorerCalled = h.app.invokedCount() > before
	if err != nil {
		oc.ScorerStatus, oc.FallbackReason = ScorerFallback, strPtr(string(gen.FallbackReasonUnavailable))
		return ""
	}
	res, err := h.eng.Resolve(ctx, scope, adv)
	if err != nil {
		oc.ScorerStatus, oc.FallbackReason = ScorerFallback, strPtr(string(gen.FallbackReasonUnavailable))
		return ""
	}
	if reason, bad := res.Fallback(); bad {
		oc.ScorerStatus, oc.FallbackReason = ScorerFallback, strPtr(string(reason))
		return ""
	}
	sel := res.Selected()
	if len(sel) != 1 {
		oc.ScorerStatus, oc.FallbackReason = ScorerFallback, strPtr(string(gen.FallbackReasonMalformedOutput))
		return ""
	}
	oc.ScorerStatus = ScorerAccepted
	conf := sel[0].Suggestion().Confidence
	oc.Confidence = round6(conf)
	return sel[0].Resolved().ID
}

// request builds the bounded decision request of a case. It carries the utterance, the
// language, the dialogue state flags of an act decision and the candidate set; it never
// carries the case's untrusted document, an expected outcome or a fixture script.
func (h *harness) request(scope contracts.Scope, c *Case) gen.DecisionRequest {
	now := h.clk.Now().UTC()
	state := map[string]gen.BoundedScalar{"user_text": c.Utterance, "language": c.Language}
	if s := c.State; s != nil {
		state["has_goal"], state["prepared"], state["pending"], state["choices"] = s.HasGoal, s.Prepared, s.Pending, s.Choices
	}
	cands := make([]gen.CandidateSetCandidate, len(c.Candidates))
	for i, cd := range c.Candidates {
		cands[i] = gen.CandidateSetCandidate{CandidateID: common.Identifier(cd.ID), Label: cd.Label}
	}
	return gen.DecisionRequest{
		TenantID: common.Identifier(scope.TenantID), ApplicationID: common.Identifier(scope.ApplicationID),
		SessionID:          common.Identifier(scope.SessionID),
		ContractVersion:    actions.OpaqueVersion(contractFor(c.DecisionType)),
		DecisionType:       genType(c.DecisionType),
		SchemaVersion:      1,
		BoundedState:       state,
		AllowedNextActions: []actions.PermittedNextAction{},
		CandidateSet:       gen.CandidateSet{Candidates: cands, MaxSelections: 1, BuiltAt: tsOf(now)},
		Locale:             gen.Locale(c.Language),
		Deadline:           tsOf(now.Add(h.o.Timeout)),
	}
}

// replay sends again the requests the scorer accepted and counts how many the engine's
// result cache answered.
func (h *harness) replay(ctx context.Context, cases []CaseOutcome) CacheInfo {
	var ci CacheInfo
	byID := map[string]*Case{}
	for i := range h.o.Corpus.Cases {
		byID[h.o.Corpus.Cases[i].ID] = &h.o.Corpus.Cases[i]
	}
	for _, oc := range cases {
		if oc.ScorerStatus != ScorerAccepted {
			continue
		}
		c := byID[oc.ID]
		h.resolver.set(c)
		scope := scopeOf(c)
		adv, err := h.eng.Score(ctx, scope, h.request(scope, c))
		ci.Requests++
		if err == nil && adv.Result().CacheStatus == gen.DecisionScoreResultCacheStatusHit {
			ci.Hits++
		}
	}
	return ci
}

// rulesDecide is the exact deterministic rule for a case: for routing and item decisions
// the one candidate whose id or label the whole utterance equals; for dialogue acts the
// core's control detectors on the case's state. It decides only on an exact match and
// never consults a model.
func rulesDecide(c *Case) (string, bool) {
	if c.DecisionType != TypeAct {
		named := make([]ddec.Named, len(c.Candidates))
		for i, cd := range c.Candidates {
			named[i] = ddec.Named{ID: cd.ID, Names: []string{cd.ID, cd.Label}}
		}
		return ddec.ExactCapability(c.Utterance, named)
	}
	rules := dialogue.RuleDetectors()
	req := dialogue.ControlRequest{
		Text: c.Utterance, Language: verify.Language(c.Language),
		State: dialogue.ControlState{HasGoal: c.State.HasGoal, Prepared: c.State.Prepared, Pending: c.State.Pending, Choices: c.State.Choices},
	}
	// The core's own order.
	ordered := []struct {
		act string
		d   dialogue.ControlDetector
	}{
		{"help", rules.Help}, {"cancel", rules.Cancel}, {"restart", rules.Restart}, {"resume", rules.Resume},
		{"reject", rules.Reject}, {"confirm", rules.Confirm}, {"correct", rules.Correct},
	}
	for _, r := range ordered {
		if !r.d.Detect(context.Background(), req).Matched {
			continue
		}
		id := actPrefix + r.act
		for _, cd := range c.Candidates {
			if cd.ID == id {
				return id, true
			}
		}
		return "", false // the rule recognised something this decision does not offer
	}
	return "", false
}

// actPrefix is the candidate-id prefix of dialogue acts.
const actPrefix = "act:"
