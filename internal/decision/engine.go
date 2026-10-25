package decision

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/decision"
)

const tracePrefix = "trc"

// Deps are the engine's collaborators. The resolver and the audit appender are
// required: without them the engine cannot guard or record anything, so it refuses to
// exist. A nil Provider is allowed and means the layer is unavailable.
type Deps struct {
	Clock    clock.Clock
	Provider DecisionModelProvider
	Resolver CandidateResolver
	Audit    audit.Appender
	IDs      *ids.Generator
	Config   Config
}

// Engine guards a DecisionModelProvider. It is safe for concurrent use.
type Engine struct {
	clk      clock.Clock
	prov     DecisionModelProvider
	resolver CandidateResolver
	audit    audit.Appender
	ids      *ids.Generator
	cfg      Config
	cache    *resultCache
}

// New validates d and returns the engine.
func New(d Deps) (*Engine, error) {
	if d.Resolver == nil {
		return nil, fmt.Errorf("decision: a candidate resolver is required")
	}
	if d.Audit == nil {
		return nil, fmt.Errorf("decision: an audit appender is required")
	}
	if err := d.Config.validate(); err != nil {
		return nil, err
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	gid := d.IDs
	if gid == nil {
		gid = ids.NewGenerator(clk)
	}
	cfg := d.Config
	cfg.SupportedTypes = append([]gen.DecisionType(nil), cfg.SupportedTypes...)
	cfg.Calibration = append([]Calibration(nil), cfg.Calibration...)
	return &Engine{
		clk: clk, prov: d.Provider, resolver: d.Resolver, audit: d.Audit, ids: gid, cfg: cfg,
		cache: newResultCache(clk, cfg.CacheEntries, int64(cfg.CacheTTL)),
	}, nil
}

// PurgeTenant drops every cached result of one tenant (for example when its
// authorisations change).
func (e *Engine) PurgeTenant(applicationID, tenantID string) {
	e.cache.purgeTenant(applicationID, tenantID)
}

// Advice is the guarded outcome of one scoring request. It is ADVISORY: a proposal
// from a scorer that passed the gates (or a typed reason it did not). It carries no
// permission, authorisation, validation, preparation, confirmation or commit result,
// and offers no way to produce one. Its only consumer is Engine.Resolve, which
// re-resolves the selected ids through the deterministic adapter; everything else the
// caller does with it is routing hints. The fields are unexported so a score cannot
// be edited into an accepted result.
type Advice struct {
	result gen.DecisionScoreResult
	reject RejectCode
}

// Suggestion is one candidate the scorer favoured, with its raw score and its
// calibrated confidence. It is a hint, not a decision.
type Suggestion struct {
	CandidateID string
	Score       float64
	Confidence  float64
}

// Result returns a copy of the DecisionScoreResult (the schema type).
func (a Advice) Result() gen.DecisionScoreResult { return cloneResult(a.result) }

// Accepted reports whether the scorer's proposal passed every gate.
func (a Advice) Accepted() bool {
	return a.result.Disposition == gen.DecisionScoreResultDispositionAccepted
}

// FallbackReason returns the typed reason no proposal was accepted.
func (a Advice) FallbackReason() (gen.FallbackReason, bool) {
	if a.result.FallbackReason == nil {
		return "", false
	}
	return *a.result.FallbackReason, true
}

// RejectCode returns the guard's code when the reason is candidate_set_rejected.
func (a Advice) RejectCode() RejectCode { return a.reject }

// TraceID returns the trace id of the scoring.
func (a Advice) TraceID() string { return string(a.result.TraceID) }

// Suggestions returns the scorer's favoured candidates, best first. Empty unless the
// advice was accepted.
func (a Advice) Suggestions() []Suggestion {
	out := make([]Suggestion, len(a.result.Selections))
	for i, s := range a.result.Selections {
		out[i] = Suggestion{string(s.CandidateID), float64(s.Score), float64(s.CalibratedConfidence)}
	}
	return out
}

// run is the state of one Score call.
type run struct {
	e     *Engine
	scope contracts.Scope
	req   gen.DecisionRequest
	start time.Time
	trace string

	manifest *gen.ScorerManifest
	ood      gen.DecisionScoreResultOodStatus
	cache    gen.DecisionScoreResultCacheStatus
	invoked  bool
}

// Score runs the guard, the provider and the gates for one request. It returns an
// error only when scope is missing or malformed (there is then no tenant to record
// against); every other failure is an Advice with a typed fallback reason.
func (e *Engine) Score(ctx context.Context, scope contracts.Scope, req gen.DecisionRequest) (Advice, error) {
	if err := scope.Validate(); err != nil {
		return Advice{}, err
	}
	if !req.DecisionType.Valid() {
		// Decoded JSON can never carry one; only a Go caller can, and the result type
		// could not encode an echo of it.
		return Advice{}, contracts.New(contracts.ErrValidationFailed, "decision: unknown decision type")
	}
	r := &run{
		e: e, scope: scope, req: req, start: e.clk.Now(), trace: e.ids.New(tracePrefix),
		ood: gen.DecisionScoreResultOodStatusNotEvaluated, cache: gen.DecisionScoreResultCacheStatusBypassed,
	}
	cfg := e.cfg
	if !cfg.Enabled {
		return r.fallback(ctx, gen.FallbackReasonDisabled, RejectNone), nil
	}
	if e.prov == nil {
		return r.fallback(ctx, gen.FallbackReasonUnavailable, RejectNone), nil
	}
	m := e.prov.Manifest()
	r.manifest = &m

	// The request must be for the caller's own scope: the scope is verified by the
	// identity layer, the request body is only data.
	if string(req.TenantID) != scope.TenantID || string(req.ApplicationID) != scope.ApplicationID || string(req.SessionID) != scope.SessionID {
		return r.fallback(ctx, gen.FallbackReasonTenantMismatch, RejectNone), nil
	}
	if !e.shapeSupported(req) {
		r.ood = gen.DecisionScoreResultOodStatusUnsupportedShape
		return r.fallback(ctx, gen.FallbackReasonOodUnsupportedShape, RejectNone), nil
	}
	idset, code := e.checkCandidates(ctx, scope, req)
	if code != RejectNone {
		return r.fallback(ctx, gen.FallbackReasonCandidateSetRejected, code), nil
	}
	deadline, ok := parseTimestamp(string(req.Deadline))
	if !ok {
		return r.fallback(ctx, gen.FallbackReasonCandidateSetRejected, RejectInvalid), nil
	}
	budget := deadline.Sub(r.start)
	if budget <= 0 {
		return r.fallback(ctx, gen.FallbackReasonTimeout, RejectNone), nil
	}
	if budget > cfg.Timeout {
		budget = cfg.Timeout
	}
	maxSel := int(req.CandidateSet.MaxSelections)

	key := cacheKey(scope, m, req)
	res, hit := e.cache.get(key)
	if e.cache != nil {
		r.cache = gen.DecisionScoreResultCacheStatusMiss
	}
	if hit {
		r.cache = gen.DecisionScoreResultCacheStatusHit
	}

	// Record the invocation before the provider is reached. If it cannot be recorded
	// the scorer is not called.
	r.invoked = true
	if _, err := eventInvoked.Append(ctx, e.audit, mustLocator(scope), InvokedAudit{
		TraceID: r.trace, DecisionType: string(req.DecisionType), ProviderID: string(m.ProviderID),
		ModelID: string(m.ModelID), ModelRevision: string(m.ModelRevision),
		ContractVersion: string(req.ContractVersion), Candidates: len(req.CandidateSet.Candidates),
		CacheStatus: string(r.cache),
	}); err != nil {
		return r.fallbackNoAudit(gen.FallbackReasonUnavailable, RejectNone), nil
	}

	if !hit {
		var err error
		res, err = e.callProvider(ctx, budget, scope, req)
		if err != nil {
			return r.fallback(ctx, providerFailure(err), RejectNone), nil
		}
		if e.clk.Now().Sub(r.start) > budget {
			// A result that arrives after its deadline is a timeout even if it parsed.
			return r.fallback(ctx, gen.FallbackReasonTimeout, RejectNone), nil
		}
	}

	reason := e.gate(m, req, idset, maxSel, res)
	r.ood = res.OodStatus
	if !r.ood.Valid() {
		r.ood = gen.DecisionScoreResultOodStatusNotEvaluated
	}
	if reason != "" {
		return r.fallback(ctx, reason, RejectNone), nil
	}
	if !hit {
		e.cache.put(key, res)
	}
	return r.accept(ctx, res), nil
}

func mustLocator(scope contracts.Scope) audit.Locator {
	loc, err := audit.ForScope(scope)
	if err != nil {
		// Score validated the scope; ForScope can only add platform-application
		// checks. A zero Locator is refused by every append, which fails closed.
		return audit.Locator{}
	}
	return loc
}

func (r *run) base(d gen.DecisionScoreResultDisposition) gen.DecisionScoreResult {
	now := r.e.clk.Now()
	res := gen.DecisionScoreResult{
		Disposition:     d,
		ProviderID:      "none",
		ContractVersion: r.req.ContractVersion,
		DecisionType:    r.req.DecisionType,
		SchemaVersion:   1,
		TenantID:        common.Identifier(r.scope.TenantID),
		ApplicationID:   common.Identifier(r.scope.ApplicationID),
		Selections:      []gen.DecisionScoreResultSelection{},
		OodStatus:       r.ood,
		LatencyMS:       common.DurationMS(now.Sub(r.start).Milliseconds()),
		CacheStatus:     r.cache,
		TraceID:         common.TraceID(r.trace),
		IssuedAt:        common.Timestamp(now.UTC().Format(timestampLayout)),
	}
	if m := r.manifest; m != nil {
		id, rev := m.ModelID, m.ModelRevision
		res.ProviderID, res.ModelID, res.ModelRevision = m.ProviderID, &id, &rev
	}
	return res
}

func (r *run) fallbackAudit(reason gen.FallbackReason, code RejectCode, res gen.DecisionScoreResult) FallbackAudit {
	a := FallbackAudit{
		TraceID: r.trace, DecisionType: string(r.req.DecisionType), Reason: string(reason),
		RejectCode: string(code), Invoked: r.invoked, ProviderID: string(res.ProviderID),
		ContractVersion: string(r.req.ContractVersion), OodStatus: string(res.OodStatus),
		LatencyMS: int64(res.LatencyMS),
	}
	if res.ModelRevision != nil {
		a.ModelRevision = string(*res.ModelRevision)
	}
	return a
}

// fallback records a decision.fallback event and returns the fallback advice. A
// fallback is safe whether or not it could be recorded (the deterministic path
// continues), so a failed append leaves the audit ref empty instead of changing the
// outcome.
func (r *run) fallback(ctx context.Context, reason gen.FallbackReason, code RejectCode) Advice {
	res := r.base(gen.DecisionScoreResultDispositionFallback)
	res.FallbackReason = &reason
	ev, err := eventFallback.Append(ctx, r.e.audit, mustLocator(r.scope), r.fallbackAudit(reason, code, res))
	if err == nil {
		res.AuditRef = actions.AuditRef(ev.ID)
	}
	return Advice{result: res, reject: code}
}

func (r *run) fallbackNoAudit(reason gen.FallbackReason, code RejectCode) Advice {
	res := r.base(gen.DecisionScoreResultDispositionFallback)
	res.FallbackReason = &reason
	return Advice{result: res, reject: code}
}

// accept builds the accepted advice from a result that passed the gates, taking only
// the selections from the provider: identity, latency, cache status, trace and audit
// ref are the engine's own. If the acceptance cannot be recorded it is not used.
func (r *run) accept(ctx context.Context, from gen.DecisionScoreResult) Advice {
	res := r.base(gen.DecisionScoreResultDispositionAccepted)
	res.OodStatus = gen.DecisionScoreResultOodStatusInDistribution
	res.Selections = append(res.Selections, from.Selections...)
	sort.SliceStable(res.Selections, func(i, j int) bool {
		a, b := res.Selections[i], res.Selections[j]
		if a.CalibratedConfidence != b.CalibratedConfidence {
			return a.CalibratedConfidence > b.CalibratedConfidence
		}
		return a.CandidateID < b.CandidateID
	})
	sel := make([]selectedAudit, len(res.Selections))
	for i, s := range res.Selections {
		sel[i] = selectedAudit{string(s.CandidateID), int(math.Round(float64(s.Score) * 1000)), int(math.Round(float64(s.CalibratedConfidence) * 1000))}
	}
	ev, err := eventAccepted.Append(ctx, r.e.audit, mustLocator(r.scope), AcceptedAudit{
		TraceID: r.trace, DecisionType: string(r.req.DecisionType), ProviderID: string(res.ProviderID),
		ModelID: string(*res.ModelID), ModelRevision: string(*res.ModelRevision),
		ContractVersion: string(r.req.ContractVersion), CacheStatus: string(res.CacheStatus),
		LatencyMS: int64(res.LatencyMS), Selected: sel,
	})
	if err != nil {
		return r.fallback(ctx, gen.FallbackReasonUnavailable, RejectNone)
	}
	res.AuditRef = actions.AuditRef(ev.ID)
	return Advice{result: res}
}

// FinalAction is what the deterministic side finally did with an Advice.
type FinalAction string

// The final actions.
const (
	// FinalSelectionResolved: every selected id was re-resolved by the adapter and
	// the normal pipeline continues with the resolved candidates.
	FinalSelectionResolved FinalAction = "selection_resolved"
	// FinalSelectionRefused: the adapter refused a selected id; the pipeline
	// continues without the scorer's proposal.
	FinalSelectionRefused FinalAction = "selection_refused"
	// FinalFallbackRouted: no proposal was accepted; the controller or the exact
	// rules routed the turn. Dialogue records it with RecordFinal.
	FinalFallbackRouted FinalAction = "fallback_routed"
)

// RecordFinal appends the decision.final_action event for adv.
func (e *Engine) RecordFinal(ctx context.Context, scope contracts.Scope, adv Advice, action FinalAction, selected []string) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	a := FinalAudit{TraceID: adv.TraceID(), Action: string(action), Selected: selected}
	if reason, ok := adv.FallbackReason(); ok {
		a.Reason = string(reason)
	}
	_, err := eventFinal.Append(ctx, e.audit, mustLocator(scope), a)
	return err
}

// ResolvedSelection is a selected candidate that the deterministic adapter has
// re-resolved. Only Engine.Resolve creates one.
type ResolvedSelection struct {
	suggestion Suggestion
	resolved   Resolved
}

// Suggestion returns the scorer's score and confidence for the candidate.
func (s ResolvedSelection) Suggestion() Suggestion { return s.suggestion }

// Resolved returns what the adapter resolved the candidate id to.
func (s ResolvedSelection) Resolved() Resolved { return s.resolved }

// Resolution is the outcome of re-resolving an Advice. Either every selection was
// resolved by the adapter, or there is a typed fallback reason and no selection.
type Resolution struct {
	selected []ResolvedSelection
	reason   gen.FallbackReason
}

// Selected returns the resolved selections (empty on fallback).
func (r Resolution) Selected() []ResolvedSelection {
	return append([]ResolvedSelection(nil), r.selected...)
}

// Fallback returns the reason the proposal was not used.
func (r Resolution) Fallback() (gen.FallbackReason, bool) { return r.reason, r.reason != "" }

// Resolve re-resolves every selected candidate id through the CandidateResolver. The
// scorer's selection is usable only through the returned Resolution. A refused id, a
// resolver answer for a different id, or an advice from another tenant yields a
// fallback (policy_conflict or tenant_mismatch) and nothing else of the proposal is
// kept. The outcome is recorded as decision.final_action; if that cannot be recorded
// the proposal is not used.
func (e *Engine) Resolve(ctx context.Context, scope contracts.Scope, adv Advice) (Resolution, error) {
	if err := scope.Validate(); err != nil {
		return Resolution{}, err
	}
	refuse := func(reason gen.FallbackReason, action FinalAction) (Resolution, error) {
		_ = e.RecordFinal(ctx, scope, adv, action, nil)
		return Resolution{reason: reason}, nil
	}
	if !adv.Accepted() {
		reason, _ := adv.FallbackReason()
		if reason == "" {
			reason = gen.FallbackReasonUnavailable
		}
		return refuse(reason, FinalFallbackRouted)
	}
	if string(adv.result.TenantID) != scope.TenantID || string(adv.result.ApplicationID) != scope.ApplicationID {
		return refuse(gen.FallbackReasonTenantMismatch, FinalSelectionRefused)
	}
	out := make([]ResolvedSelection, 0, len(adv.result.Selections))
	selIDs := make([]string, 0, len(adv.result.Selections))
	for _, s := range adv.Suggestions() {
		got, err := e.resolver.Resolve(ctx, scope, s.CandidateID)
		switch {
		case contracts.HasCode(err, contracts.ErrScopeViolation) || contracts.HasCode(err, contracts.ErrScopeMissing):
			return refuse(gen.FallbackReasonTenantMismatch, FinalSelectionRefused)
		case err != nil || got.ID != s.CandidateID:
			return refuse(gen.FallbackReasonPolicyConflict, FinalSelectionRefused)
		}
		out = append(out, ResolvedSelection{suggestion: s, resolved: got})
		selIDs = append(selIDs, s.CandidateID)
	}
	if err := e.RecordFinal(ctx, scope, adv, FinalSelectionResolved, selIDs); err != nil {
		return Resolution{reason: gen.FallbackReasonUnavailable}, nil
	}
	return Resolution{selected: out}, nil
}

// errScorerPanic is what callProvider returns for a provider that panicked. It carries no
// panic value: that is scorer-controlled text and never reaches the audit trail.
var errScorerPanic = contracts.New(contracts.ErrUnavailable, "decision: the scorer panicked")

// callProvider runs the provider under its deadline and turns a panic into an error, so the
// invocation recorded before the call always ends in a terminal event (the caller records
// decision.fallback with reason unavailable). The scorer is untrusted code; its crash must not
// unwind through Score and leave decision.invoked as the last event of the trail.
func (e *Engine) callProvider(ctx context.Context, budget time.Duration, scope contracts.Scope, req gen.DecisionRequest) (res gen.DecisionScoreResult, err error) {
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	defer func() {
		if recover() != nil {
			res, err = gen.DecisionScoreResult{}, errScorerPanic
		}
	}()
	return e.prov.ScoreDecision(cctx, scope, req)
}
