package decision

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	core "github.com/buxee/alix-ai/internal/decision"
	"github.com/buxee/alix-ai/internal/dialogue"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

// The conversation-contract versions of the two decision types this layer issues. An
// operator's core.Config.Calibration must hold an entry for each (model, revision,
// version) it allows; a result under a version with no entry is calibration_failed.
const (
	ContractRouting = "dialogue-routing-v1"
	ContractAct     = "dialogue-act-v1"
)

// Config tunes the layer. The zero value takes the defaults.
type Config struct {
	// Margin is kept back from the dialogue engine's own decision deadline, so that the
	// engine's inner call ends before the outer one does (default 25 ms).
	Margin time.Duration
	// MaxUtteranceBytes is the longest utterance sent to a scorer (default 256, the
	// engine's bounded-state string cap). A longer one is never truncated: it is an
	// unsupported shape and the controller serves it.
	MaxUtteranceBytes int
	// MaxControlWords is the longest utterance the control detectors consult the scorer
	// for (default 8): a control act is a short utterance, a longer one is a request.
	MaxControlWords int
	// ProposalTokens is the estimated size, in generated tokens, of the proposal a
	// control act would have cost the controller. The default is the estimate of the
	// smallest valid control proposal; it is an estimate, never a measurement.
	ProposalTokens int
}

const controlProposalSample = `{"capability_id":null,"fields":{},"entity_refs":[],"intent":"cancel","uncertainty":{"overall":0.9}}`

func (c Config) normalized() Config {
	if c.Margin <= 0 {
		c.Margin = 25 * time.Millisecond
	}
	if c.MaxUtteranceBytes <= 0 {
		c.MaxUtteranceBytes = 256
	}
	if c.MaxControlWords <= 0 {
		c.MaxControlWords = 8
	}
	if c.ProposalTokens <= 0 {
		c.ProposalTokens = providers.EstimateTokens(controlProposalSample)
	}
	return c
}

// Observer is the metrics hook of the layer: the layer calls it with every Advice its engine
// produced (ObserveAdvice) and once for every turn whose control act the scorer detected, so the
// controller was not called (ControllerCallAvoided). *lifecycle.Metrics is one. It is told what
// happened and nothing else: it cannot change a decision, and a nil Observer records nothing.
type Observer interface {
	ObserveAdvice(adv core.Advice)
	ControllerCallAvoided()
}

// Deps are the layer's collaborators.
type Deps struct {
	// Clock is the dialogue engine's clock (the candidate set and the deadline are built
	// from it). Nil means clock.Real.
	Clock clock.Clock
	// Core is the engine policy of internal/decision. Start from core.DefaultConfig():
	// Enabled is false in it, and a layer that is not Enabled never builds a request,
	// never calls a provider and writes no audit event.
	Core core.Config
	// Registry holds the scorer in the `router` role; the layer follows its health on
	// every decision. Provider is used when Registry is nil (tests, simple roots).
	Registry *providers.Registry
	Provider core.DecisionModelProvider
	// Audit receives the decision.* events (required).
	Audit audit.Appender
	// Policy and Packages back the Resolver: they must be the gateway and the package
	// source the dialogue engine uses (required).
	Policy   *policy.Gateway
	Packages dialogue.PackageSource
	Config   Config
	// Observer, when set, is the metrics hook (see Observer). The audit events of the engine are
	// unchanged, so a root that counts from them instead keeps working.
	Observer Observer
}

// Layer is the decision layer as the dialogue engine sees it. Safe for concurrent use.
type Layer struct {
	d    Deps
	cfg  Config
	st   stats
	live *core.Engine // the provider currently in the router role
	none *core.Engine // no provider: records unavailable
	rt   *core.Engine // the router role holds a text model: records unsupported_runtime
	res  *Resolver
}

// New builds the layer.
func New(d Deps) (*Layer, error) {
	if d.Audit == nil || d.Policy == nil || d.Packages == nil {
		return nil, contracts.New(contracts.ErrValidationFailed, "decision: Deps.Audit, Deps.Policy and Deps.Packages are required")
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	res, err := NewResolver(d.Policy, d.Packages)
	if err != nil {
		return nil, err
	}
	l := &Layer{d: d, cfg: d.Config.normalized(), res: res}
	mk := func(p core.DecisionModelProvider) (*core.Engine, error) {
		return core.New(core.Deps{Clock: d.Clock, Provider: p, Resolver: res, Audit: d.Audit, Config: d.Core})
	}
	if l.live, err = mk(liveProvider{l}); err != nil {
		return nil, err
	}
	if l.none, err = mk(nil); err != nil {
		return nil, err
	}
	if l.rt, err = mk(unsupportedProvider{}); err != nil {
		return nil, err
	}
	return l, nil
}

// Stage returns the dialogue.DecisionStage to give dialogue.Deps.Decision.
func (l *Layer) Stage() dialogue.DecisionStage { return stage{l} }

// Stats returns a copy of the counters.
func (l *Layer) Stats() Snapshot { return l.st.snapshot() }

func (l *Layer) enabled() bool { return l.d.Core.Enabled }

// ---- the provider in the router role ----

// current is the scorer now serving the router role, or the typed reason there is none.
func (l *Layer) current() (core.DecisionModelProvider, gen.FallbackReason) {
	if l.d.Registry != nil {
		return core.ProviderFromRegistry(l.d.Registry)
	}
	if l.d.Provider == nil {
		return nil, gen.FallbackReasonUnavailable
	}
	return l.d.Provider, ""
}

// liveProvider delegates to whatever scorer serves the router role at the moment of the
// call, so the layer follows the role's health without being rebuilt.
type liveProvider struct{ l *Layer }

func (p liveProvider) Manifest() gen.ScorerManifest {
	if pr, _ := p.l.current(); pr != nil {
		return pr.Manifest()
	}
	return gen.ScorerManifest{}
}

func (p liveProvider) ScoreDecision(ctx context.Context, scope contracts.Scope, req gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	pr, _ := p.l.current()
	if pr == nil {
		return gen.DecisionScoreResult{}, contracts.New(contracts.ErrUnavailable, "decision: the router role has no scorer")
	}
	return pr.ScoreDecision(ctx, scope, req)
}

// unsupportedProvider stands for a router role that holds a plain text model: the engine
// records unsupported_runtime for it.
type unsupportedProvider struct{}

func (unsupportedProvider) Manifest() gen.ScorerManifest { return gen.ScorerManifest{} }
func (unsupportedProvider) ScoreDecision(context.Context, contracts.Scope, gen.DecisionRequest) (gen.DecisionScoreResult, error) {
	return gen.DecisionScoreResult{}, fmt.Errorf("%w", core.ErrUnsupportedRuntime)
}

// ---- ranking: engine, gates, re-resolution ----

type selection struct {
	id         string
	confidence float64
}

// rank sends a bounded request to the engine and returns the one selected id, re-resolved
// by the deterministic resolver, or the typed fallback reason. The engine records every
// outcome (invoked, accepted or fallback, and the final action) in the audit log.
func (l *Layer) rank(ctx context.Context, scope contracts.Scope, req gen.DecisionRequest) (selection, dialogue.FallbackReason) {
	eng := l.live
	if _, reason := l.current(); reason != "" {
		eng = l.none
		if reason == gen.FallbackReasonUnsupportedRuntime {
			eng = l.rt
		}
	}
	l.st.with(func(s *Snapshot) { s.EngineRequests++ })
	ctx = withExposureMemo(ctx)
	adv, err := eng.Score(ctx, scope, req)
	if err != nil {
		l.st.fallback(string(gen.FallbackReasonUnavailable), "")
		return selection{}, dialogue.FallbackUnavailable
	}
	if l.d.Observer != nil {
		l.d.Observer.ObserveAdvice(adv)
	}
	res, err := eng.Resolve(ctx, scope, adv)
	if err != nil {
		l.st.fallback(string(gen.FallbackReasonUnavailable), "")
		return selection{}, dialogue.FallbackUnavailable
	}
	if reason, ok := res.Fallback(); ok {
		l.st.fallback(string(reason), string(adv.RejectCode()))
		return selection{}, dialogue.FallbackReason(reason)
	}
	sel := res.Selected()
	if len(sel) != 1 {
		l.st.fallback(string(gen.FallbackReasonMalformedOutput), "")
		return selection{}, dialogue.FallbackMalformedOutput
	}
	l.st.with(func(s *Snapshot) { s.ScorerSelected++ })
	return selection{id: sel[0].Resolved().ID, confidence: sel[0].Suggestion().Confidence}, dialogue.FallbackNone
}

// budget is how long the engine may take: its own timeout, shortened to what is left of
// the dialogue engine's decision deadline minus a margin, so the engine's inner call always
// ends first and never records an acceptance the pipeline has already given up on.
func (l *Layer) budget(ctx context.Context) time.Duration {
	b := l.d.Core.Timeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl) - l.cfg.Margin; rem < b {
			b = rem
		}
	}
	return b
}

var localeRE = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8}){0,3}$`)

// request builds the bounded request. Collections are never nil, so a real adapter never
// sees JSON null.
func (l *Layer) request(scope contracts.Scope, typ gen.DecisionType, contract, language string, state map[string]gen.BoundedScalar,
	next []actions.PermittedNextAction, cands []gen.CandidateSetCandidate, budget time.Duration) gen.DecisionRequest {
	now := l.d.Clock.Now().UTC()
	if !localeRE.MatchString(language) || len(language) > 35 {
		language = "en"
	}
	if state == nil {
		state = map[string]gen.BoundedScalar{}
	}
	if next == nil {
		next = []actions.PermittedNextAction{}
	}
	return gen.DecisionRequest{
		TenantID:           common.Identifier(scope.TenantID),
		ApplicationID:      common.Identifier(scope.ApplicationID),
		SessionID:          common.Identifier(scope.SessionID),
		ContractVersion:    actions.OpaqueVersion(contract),
		DecisionType:       typ,
		SchemaVersion:      1,
		BoundedState:       state,
		AllowedNextActions: next,
		CandidateSet: gen.CandidateSet{
			Candidates:    cands,
			MaxSelections: 1,
			BuiltAt:       common.Timestamp(now.Format("2006-01-02T15:04:05.000Z")),
		},
		Locale:   gen.Locale(language),
		Deadline: common.Timestamp(now.Add(budget).Format("2006-01-02T15:04:05.000Z")),
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 { // do not cut a rune
		n--
	}
	return s[:n]
}
