package modes

import (
	"context"
	"errors"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Profiles is the profile registry the router uses. *providerreg.Registry implements it.
type Profiles interface {
	Get(ctx context.Context, scope contracts.Scope, id pv.ProfileID) (pv.ProviderProfile, error)
	Check(ctx context.Context, scope contracts.Scope, id pv.ProfileID, req providerreg.Requirement) (providerreg.Admission, error)
	Fallback(ctx context.Context, scope contracts.Scope, from pv.ProfileID, cond providerreg.FallbackCondition, req providerreg.Requirement) (providerreg.FallbackRecord, providerreg.Admission, error)
}

// Budgets is the budget service the router uses. *providerbudget.Service implements it.
type Budgets interface {
	Reserve(ctx context.Context, scope contracts.Scope, call providerbudget.Call) (providerbudget.Reservation, error)
	Settle(ctx context.Context, scope contracts.Scope, id pv.ReservationID, a providerbudget.Actual) (providerbudget.Settlement, error)
	States(ctx context.Context, scope contracts.Scope, provider pv.ProviderID, channel pv.Channel) ([]pv.BudgetState, error)
}

var (
	_ Profiles         = (*providerreg.Registry)(nil)
	_ Budgets          = (*providerbudget.Service)(nil)
	_ dataegress.Guard = (*dataegress.Service)(nil)
)

// Config wires a Router. Every part is required: a router missing the guard, the budget, the
// registry or the kill switch could not apply the gates, so it is not built.
type Config struct {
	Modes      ModeSource
	KillSwitch KillSwitch
	Policy     PolicySource
	Local      LocalProviders
	Profiles   Profiles
	Guard      dataegress.Guard
	Budgets    Budgets
	// Clock times the external call for the usage record. Nil means the wall clock.
	Clock clock.Clock
}

// Router decides which provider serves a request and runs the gates around an external call.
type Router struct {
	resolver *Resolver
	policy   PolicySource
	local    LocalProviders
	profiles Profiles
	guard    dataegress.Guard
	budgets  Budgets
	clk      clock.Clock
}

// New returns a Router.
func New(cfg Config) (*Router, error) {
	res, err := NewResolver(cfg.Modes, cfg.KillSwitch)
	if err != nil {
		return nil, err
	}
	if cfg.Policy == nil || cfg.Local == nil || cfg.Profiles == nil || cfg.Guard == nil || cfg.Budgets == nil {
		return nil, contracts.New(contracts.ErrInternal, "modes: a policy, local providers, profile registry, egress guard and budget are required")
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	return &Router{resolver: res, policy: cfg.Policy, local: cfg.Local, profiles: cfg.Profiles, guard: cfg.Guard, budgets: cfg.Budgets, clk: clk}, nil
}

// Resolver returns the mode resolver of the router.
func (r *Router) Resolver() *Resolver { return r.resolver }

// Estimator prices the worst case of the request for one profile: the metered units (the bounded
// input, the output cap and any media) and an estimate floor in micro-units. It is called once per
// external attempt, with the profile that attempt would use, so a fallback target is priced on its own
// table. The caller derives both from providers.Budget.EstimateRichCost over the profile's prices.
type Estimator func(p pv.ProviderProfile) (providerbudget.Units, pv.MicroUnits, error)

// Request is one routed request.
type Request struct {
	Scope   contracts.Scope
	Role    Role
	Channel pv.Channel // empty means interactive
	// Requirement states what the request needs from a provider (capabilities, sizes, and the most
	// restrictive data class). The router sets its Role from the routed role.
	Requirement providerreg.Requirement
	// Candidate is the request as the pipeline built it. The router sets Scope, Profile, Role and
	// Channel; the pipeline supplies the content with its metadata.
	Candidate dataegress.Candidate
	// Estimate prices the request for a profile. Required for an external call.
	Estimate Estimator
	// CommitCapable is true when the request could cause an application commit (the response drives a
	// prepare, confirm or commit path). A network failure that may have reached the provider is then
	// uncertain_outcome, never a retry or fallback.
	CommitCapable bool
}

// ExternalCall is what the sender of an external request receives: only after every gate passed.
type ExternalCall struct {
	Admission providerreg.Admission
	// Envelope is the filtered request: the only thing that may be sent.
	Envelope    *pv.RequestEnvelope
	Decision    pv.EgressDecision
	Disclosure  pv.EgressDisclosure
	Reservation providerbudget.Reservation
	// FallbackFrom is the profile this call replaced, or nil.
	FallbackFrom *pv.ProfileID
}

// ExternalResult is what a successful external call consumed.
type ExternalResult struct {
	Metering           providerbudget.Units
	MeteringBasis      pv.UsageRecordMeteringBasis
	ReportedMicroUnits *pv.MicroUnits
	// ReplyModel is the model name the provider put in its reply. The router compares it with the model the
	// profile is pinned to (providerreg.ReplyModelMatches) on every reply: a reply from another model is
	// discarded, settled as failed and quarantines the profile (SEC-128). Empty means the sender could not
	// read one; a sender that can read it must set it.
	ReplyModel string
}

// CallError is how a sender reports a failed external call. Sent says whether bytes may have reached
// the provider: false only when the sender can prove nothing left (refused dial, local validation).
// Any other error from a sender is read as connection_lost with Sent true.
type CallError struct {
	Kind       pv.ProviderErrorKind
	Sent       bool
	RetryAfter time.Duration
	// Metering is what the provider reported consuming, when it said so.
	Metering *providerbudget.Units
	Err      error
}

func (e *CallError) Error() string { return "modes: external call failed: " + string(e.Kind) }

// Unwrap returns the sender's own error.
func (e *CallError) Unwrap() error { return e.Err }

// Executors are the two ways to serve a request. The router calls at most one of them per hop and
// never both for one provider.
type Executors struct {
	// Local runs the request on the local provider.
	Local func(ctx context.Context) error
	// External sends an admitted, filtered request. It makes the HTTP call (an adapter) and returns
	// *CallError on failure.
	External func(ctx context.Context, call ExternalCall) (ExternalResult, error)
}

// failure is a failed step before it becomes an Outcome.
type failure struct {
	kind   Kind
	reason string
	// cond is the condition a policy may name for it; empty means nothing may be named.
	cond Condition
	// profileFallback: the profile's own named fallback may be followed (providerreg decides).
	profileFallback bool
	// noLocal: not even a named local fallback may follow (the outcome is unknown).
	noLocal bool
	// forceLocal: the kill switch is engaged; serve locally, whatever the policy names.
	forceLocal bool
	retryAfter time.Duration
}

type run struct {
	r            *Router
	req          Request
	ex           Executors
	out          Outcome
	fallbackFrom *pv.ProfileID
}

// Execute routes one request. It returns the Outcome and, for anything but a served request, the same
// result as an error (see Outcome.Err), so a failure cannot be read as a success. An error and an
// empty Outcome mean the request itself was invalid.
func (r *Router) Execute(ctx context.Context, req Request, ex Executors) (Outcome, error) {
	if err := req.Scope.Validate(); err != nil {
		return Outcome{}, err
	}
	if !req.Role.Valid() {
		return Outcome{}, contracts.New(contracts.ErrValidationFailed, "modes: unknown role")
	}
	if req.Channel == "" {
		req.Channel = pv.ChannelInteractive
	}
	if !req.Channel.Valid() {
		return Outcome{}, contracts.New(contracts.ErrValidationFailed, "modes: unknown channel")
	}
	res, err := r.resolver.Resolve(ctx, req.Scope)
	if err != nil {
		return Outcome{}, err
	}
	x := &run{r: r, req: req, ex: ex, out: Outcome{Role: req.Role, Mode: res.Mode, DecidedBy: res.DecidedBy, Target: TargetNone}}
	o := x.route(ctx, res)
	if o.Kind != KindServed && (req.Role == RoleController || req.Role == RoleRealisation) {
		o.AIActionsDisabled = true
	}
	return o, o.Err()
}

func sensitiveClass(c pv.DataClassification) bool {
	return c != pv.DataClassificationPublic && c != pv.DataClassificationInternal
}

func (x *run) route(ctx context.Context, res Resolved) Outcome {
	ext, err := x.req.Role.External()
	if err != nil {
		return x.local(ctx, detailReason(err, "role_local_only"), false)
	}
	if res.Mode == ModeLocal {
		if res.KillSwitch {
			return x.local(ctx, DecidedByKillSwitch, false)
		}
		return x.local(ctx, "mode_local", false)
	}
	rp, err := x.r.policy.RolePolicy(ctx, x.req.Scope, x.req.Role)
	if err != nil {
		if detailReason(err, "") == "llm_not_configured" {
			return x.failed(&failure{kind: KindUnavailable, reason: "llm_not_configured"})
		}
		return x.local(ctx, "policy_unreadable", false)
	}
	if len(rp.Profiles) == 0 {
		// No principal binding and no deployment default: keep 1.1 local service when a
		// local model is configured; otherwise External answers llm_not_configured.
		if res.Mode == ModeExternal && (x.ex.Local == nil || !x.r.local.Usable(x.req.Role)) {
			return x.failed(&failure{kind: KindUnavailable, reason: "llm_not_configured"})
		}
		return x.local(ctx, "no_external_route", false)
	}
	if res.Mode == ModeHybrid && sensitiveClass(x.req.Requirement.DataClass) {
		// Hybrid keeps privacy-sensitive work local without contacting a provider.
		return x.local(ctx, "hybrid_sensitive_local", false)
	}
	if x.ex.External == nil || x.req.Estimate == nil {
		return x.failed(&failure{kind: KindUnavailable, reason: "external_sender_missing"})
	}

	rq := x.req.Requirement
	rq.Role = ext
	scope := x.req.Scope

	// Selection: skip a candidate only when it cannot serve the request. Every other refusal ends it.
	var cur *providerreg.Admission
	var pending *failure
	fromID := rp.Profiles[0]
	for _, id := range rp.Profiles {
		a, err := x.r.profiles.Check(ctx, scope, id, rq)
		if err == nil && a.Profile.ProfileID != id {
			err = contracts.New(contracts.ErrQuarantined, "modes: the registry admitted another profile")
		}
		if err == nil {
			cur = &a
			break
		}
		if providerreg.ReasonOf(err) == providerreg.ReasonCapability {
			continue
		}
		fromID, pending = id, classifyCheck(err)
		break
	}
	if cur == nil && pending == nil {
		pending = &failure{kind: KindUnsupportedModality, cond: CondUnsupportedModality, reason: "no_candidate_supports_request", profileFallback: true}
	}

	hops := 0
	for {
		f := pending
		if cur != nil {
			fromID = cur.Profile.ProfileID
			if f = x.attempt(ctx, rq, *cur); f == nil {
				o := x.out
				o.Kind, o.Reason, o.Target = KindServed, "served", TargetExternal
				return o
			}
		}
		pending = nil
		if f.forceLocal {
			return x.local(ctx, f.reason, false)
		}
		if hops == 0 && f.profileFallback {
			rec, adm, ferr := x.r.profiles.Fallback(ctx, scope, fromID, providerreg.FallbackCondition(f.cond), rq)
			if ferr == nil {
				from := rec.From
				x.fallbackFrom = &from
				x.out.Hops = append(x.out.Hops, Hop{From: string(rec.From), To: string(rec.To), Condition: f.cond})
				cur, hops = &adm, hops+1
				continue
			}
		}
		if f.cond != "" && !f.noLocal && rp.FallsBackOn(f.cond) {
			x.out.Hops = append(x.out.Hops, Hop{From: string(fromID), To: LocalHop, Condition: f.cond})
			return x.local(ctx, "fallback_"+string(f.cond), true)
		}
		return x.failed(f)
	}
}

// local serves the request with the local provider. A request that reaches it as a fallback and cannot
// be served is no_fallback; one that was routed there is unavailable.
func (x *run) local(ctx context.Context, reason string, viaFallback bool) Outcome {
	o := x.out
	o.Reason = reason
	fail := func(why string) Outcome {
		o.Target, o.Kind, o.Reason = TargetNone, KindUnavailable, why
		if viaFallback {
			o.Kind = KindNoFallback
		}
		return o
	}
	if x.ex.Local == nil || !x.r.local.Usable(x.req.Role) {
		return fail("local_unavailable")
	}
	if err := x.ex.Local(ctx); err != nil {
		return fail("local_failed")
	}
	o.Kind, o.Target = KindServed, TargetLocal
	return o
}

func (x *run) failed(f *failure) Outcome {
	o := x.out
	o.Kind, o.Reason, o.Target, o.RetryAfter = f.kind, f.reason, TargetNone, f.retryAfter
	return o
}

// attempt runs the external gates and the call for one admitted profile. It returns nil when the
// request was served.
func (x *run) attempt(ctx context.Context, rq providerreg.Requirement, adm providerreg.Admission) *failure {
	prof := adm.Profile
	scope := x.req.Scope
	x.out.Profile = prof.ProfileID

	// Kill switch, read again right before the call: an emergency disable stops everything pending.
	if x.r.resolver.killed(ctx, scope.DeploymentID) {
		return &failure{kind: KindEgressDenied, reason: DecidedByKillSwitch, forceLocal: true}
	}

	// Egress guard: nothing is built, reserved or sent for a request the guard refuses.
	cand := x.req.Candidate
	cand.Scope, cand.Profile, cand.Role, cand.Channel = scope, prof, rq.Role, x.req.Channel
	gr, err := x.r.guard.Evaluate(ctx, cand)
	if err != nil || gr.Envelope == nil {
		if detailString(err, "check") == string(pv.EgressDecisionCheckNameKillSwitch) {
			return &failure{kind: KindEgressDenied, reason: DecidedByKillSwitch, forceLocal: true}
		}
		return &failure{kind: KindEgressDenied, cond: CondEgressDenied, reason: detailReason(err, "guard_failure")}
	}

	// Budget: an upper-bound estimate is reserved before the call; no budget means no call.
	units, floor, err := x.req.Estimate(prof)
	if err != nil {
		return &failure{kind: KindBudgetExhausted, cond: CondBudgetExhausted, reason: "estimate_failed"}
	}
	rsv, err := x.r.budgets.Reserve(ctx, scope, providerbudget.Call{
		Provider:      pv.ProviderRef{ProfileID: prof.ProfileID, ProviderID: prof.ProviderID, ModelID: prof.ModelID, ModelVersion: prof.ModelVersion},
		Role:          rq.Role,
		Channel:       x.req.Channel,
		EnvelopeID:    gr.Envelope.EnvelopeID,
		DecisionID:    gr.Decision.DecisionID,
		Prices:        prof.Pricing,
		Units:         units,
		EstimateFloor: floor,
		FallbackFrom:  x.fallbackFrom,
	})
	if err != nil {
		return budgetFailure(err)
	}
	x.out.ReservationID = rsv.ID

	// Kill switch, read a last time between the reservation and the send: the reservation takes time and
	// the switch may be engaged meanwhile. Nothing has left yet, so the reservation is given back and the
	// request goes no further (SEC-129).
	if x.r.resolver.killed(ctx, scope.DeploymentID) {
		x.giveBack(ctx, rsv.ID)
		return &failure{kind: KindEgressDenied, reason: DecidedByKillSwitch, forceLocal: true}
	}
	x.out.DataSent = true

	started := x.r.clk.Now()
	er, serr := x.ex.External(ctx, ExternalCall{Admission: adm, Envelope: gr.Envelope, Decision: gr.Decision, Disclosure: gr.Disclosure, Reservation: rsv, FallbackFrom: x.fallbackFrom})
	latency := pv.UsageRecordLatency{TotalMS: pv.Counter(max(x.r.clk.Now().Sub(started).Milliseconds(), 0))}
	if serr == nil && er.ReplyModel != "" && !providerreg.ReplyModelMatches(prof.ModelID, prof.ModelVersion, er.ReplyModel) {
		return x.replyModelMismatch(ctx, prof, rsv.ID, er, latency)
	}
	if serr == nil {
		x.settle(ctx, rsv.ID, providerbudget.Actual{
			Outcome: pv.UsageRecordOutcomeSucceeded, Metering: er.Metering, MeteringBasis: er.MeteringBasis,
			ReportedMicroUnits: er.ReportedMicroUnits, Latency: latency,
		})
		return nil
	}
	f, a := x.classifySend(serr)
	a.Latency = latency
	x.settle(ctx, rsv.ID, a)
	return f
}

// profileObserver is the optional part of a Profiles that takes what the provider is seen to do.
// *providerreg.Registry implements it.
type profileObserver interface {
	Observe(ctx context.Context, scope contracts.Scope, id pv.ProfileID, obs providerreg.Observation) (pv.ProviderProfile, error)
}

// replyModelMismatch handles a reply that names another model than the one the profile is pinned to. The call
// happened and may have been billed, so it settles as failed with what the provider reported; the reply is
// discarded (the failure is a malformed output, never a served request) and the profile is quarantined through
// the registry, so no further request goes to it until it is reviewed. If the quarantine cannot be recorded the
// reply is still refused.
func (x *run) replyModelMismatch(ctx context.Context, prof pv.ProviderProfile, id pv.ReservationID, er ExternalResult, latency pv.UsageRecordLatency) *failure {
	if o, ok := x.r.profiles.(profileObserver); ok {
		reported := er.ReplyModel
		if _, err := o.Observe(context.WithoutCancel(ctx), x.req.Scope, prof.ProfileID, providerreg.Observation{ReplyModel: &reported}); err != nil {
			x.out.QuarantineFailed = true
		}
	} else {
		x.out.QuarantineFailed = true
	}
	kind := pv.ProviderErrorKindMalformedResponse
	basis := er.MeteringBasis
	if basis == "" {
		basis = pv.UsageRecordMeteringBasisProviderReported
	}
	x.settle(ctx, id, providerbudget.Actual{
		Outcome: pv.UsageRecordOutcomeFailed, ErrorKind: &kind, Metering: er.Metering, MeteringBasis: basis,
		ReportedMicroUnits: er.ReportedMicroUnits, Latency: latency,
	})
	return &failure{kind: KindMalformedOutput, cond: CondMalformedOutput, reason: "reply_model_mismatch"}
}

// settle records what the call consumed. It runs even when the caller's context is cancelled: spend
// that happened is recorded.
func (x *run) settle(ctx context.Context, id pv.ReservationID, a providerbudget.Actual) {
	if _, err := x.r.budgets.Settle(context.WithoutCancel(ctx), x.req.Scope, id, a); err != nil {
		x.out.SettleFailed = true
	}
}

// reservationReleaser is the optional part of a Budgets that gives a reservation back without charging
// anything. *providerbudget.Service implements it.
type reservationReleaser interface {
	Release(ctx context.Context, scope contracts.Scope, id pv.ReservationID) error
}

// giveBack ends the reservation of a call that was never made. A budget that cannot release it settles it as
// cancelled with no metering (the reserved request is counted, nothing else), and if that fails too the
// reservation holds its bound until it times out, which SettleFailed reports.
func (x *run) giveBack(ctx context.Context, id pv.ReservationID) {
	ctx = context.WithoutCancel(ctx)
	if rl, ok := x.r.budgets.(reservationReleaser); ok {
		if rl.Release(ctx, x.req.Scope, id) != nil {
			x.out.SettleFailed = true
		}
		return
	}
	x.settle(ctx, id, providerbudget.Actual{Outcome: pv.UsageRecordOutcomeCancelled, MeteringBasis: pv.UsageRecordMeteringBasisProviderReported})
}

// classifySend turns a failed send into the section 13 row and the settlement of the reservation.
func (x *run) classifySend(err error) (*failure, providerbudget.Actual) {
	ce := &CallError{}
	if !errors.As(err, &ce) || ce == nil {
		// A bare error from the sender: read as connection_lost with Sent true, unless it proves nothing left.
		if ce = notSentCallError(err); ce == nil {
			ce = &CallError{Kind: pv.ProviderErrorKindConnectionLost, Sent: true}
		}
	}
	kind := ce.Kind
	unknownKind := !kind.Valid()
	if unknownKind {
		kind = pv.ProviderErrorKindInternal
	}
	f := &failure{retryAfter: ce.RetryAfter}
	rejected := false // the provider refused before processing, so nothing was billed
	// Bytes may have reached the provider and the request could cause a commit, and nothing proves it
	// was refused: the outcome is unknown. Exactly one send was made; no retry and no fallback follow.
	maybeProcessed := ce.Sent && x.req.CommitCapable
	switch {
	case maybeProcessed && (unknownKind || kind == pv.ProviderErrorKindConnectionLost || kind == pv.ProviderErrorKindTimeout):
		f.kind, f.reason, f.noLocal = KindUncertain, string(kind)+"_after_send", true
		if unknownKind {
			f.reason = "unknown_failure_after_send"
		}
	default:
		x.classifyKind(kind, ce, f, &rejected)
	}

	a := providerbudget.Actual{Outcome: pv.UsageRecordOutcomeFailed, ErrorKind: &kind, MeteringBasis: pv.UsageRecordMeteringBasisProviderReported}
	switch {
	case ce.Metering != nil:
		a.Metering = *ce.Metering
	case ce.Sent && !rejected:
		// It may have been billed and nothing says how much: settle at the reserved bound.
		a.Outcome = pv.UsageRecordOutcomeUncertain
	}
	return f, a
}

// classifyKind fills f for a definite provider error kind. rejected is set when the provider refused
// the request before processing it.
func (x *run) classifyKind(kind pv.ProviderErrorKind, ce *CallError, f *failure, rejected *bool) {
	switch kind {
	case pv.ProviderErrorKindAuthenticationFailed, pv.ProviderErrorKindPermissionDenied:
		*rejected = true
		f.kind, f.cond, f.reason = KindInvalidCredential, CondInvalidCredential, "provider_rejected_credential"
	case pv.ProviderErrorKindQuotaExceeded:
		*rejected = true
		f.kind, f.cond, f.reason = KindBudgetExhausted, CondBudgetExhausted, "provider_quota_exceeded"
	case pv.ProviderErrorKindRateLimited:
		*rejected = true
		f.kind, f.cond, f.reason, f.profileFallback = KindRateLimited, CondRateLimited, "provider_rate_limited", true
	case pv.ProviderErrorKindTimeout:
		f.kind, f.cond, f.reason, f.profileFallback = KindTimeout, CondTimeout, "provider_timeout", true
	case pv.ProviderErrorKindUnavailable:
		f.kind, f.cond, f.reason, f.profileFallback = KindUnavailable, CondUnavailable, "provider_unavailable", true
	case pv.ProviderErrorKindUnsupportedModality:
		f.kind, f.cond, f.reason, f.profileFallback = KindUnsupportedModality, CondUnsupportedModality, "provider_unsupported_modality", true
	case pv.ProviderErrorKindMalformedResponse:
		f.kind, f.cond, f.reason = KindMalformedOutput, CondMalformedOutput, "malformed_provider_output"
	case pv.ProviderErrorKindConnectionLost:
		f.kind, f.cond, f.reason, f.profileFallback = KindUnavailable, CondUnavailable, "connection_lost", true
	case pv.ProviderErrorKindBadRequest, pv.ProviderErrorKindContextTooLarge, pv.ProviderErrorKindModelNotFound, pv.ProviderErrorKindContentFiltered:
		*rejected = true
		f.kind, f.reason = KindProviderError, string(kind)
	default:
		f.kind, f.reason = KindProviderError, string(kind)
	}
}

// classifyCheck maps a refused profile gate to its outcome. Nothing here selects another provider.
func classifyCheck(err error) *failure {
	reason := providerreg.ReasonOf(err)
	switch reason {
	case providerreg.ReasonCredentialInvalid, providerreg.ReasonCredentialMissing:
		return &failure{kind: KindInvalidCredential, cond: CondInvalidCredential, reason: string(reason)}
	case providerreg.ReasonDataClassDenied, providerreg.ReasonScopeNotPermitted:
		return &failure{kind: KindEgressDenied, cond: CondEgressDenied, reason: string(reason)}
	case "":
		return &failure{kind: KindStaleProfile, cond: CondStaleProfile, reason: "profile_check_failed"}
	}
	return &failure{kind: KindStaleProfile, cond: CondStaleProfile, reason: string(reason)}
}

// budgetFailure maps a refused reservation. A budget never selects another provider.
func budgetFailure(err error) *failure {
	reason := detailReason(err, "budget_check_failed")
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil && ce.Code == contracts.ErrRateLimited {
		f := &failure{kind: KindRateLimited, cond: CondRateLimited, reason: reason, profileFallback: true}
		switch v := ce.Details["retry_after_ms"].(type) {
		case int64:
			f.retryAfter = time.Duration(v) * time.Millisecond
		case int:
			f.retryAfter = time.Duration(v) * time.Millisecond
		case float64:
			f.retryAfter = time.Duration(v) * time.Millisecond
		}
		return f
	}
	return &failure{kind: KindBudgetExhausted, cond: CondBudgetExhausted, reason: reason}
}

func detailString(err error, key string) string {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		if s, ok := ce.Details[key].(string); ok {
			return s
		}
	}
	return ""
}

// detailReason returns Details["reason"] (or ["rule"]) of err, else def.
func detailReason(err error, def string) string {
	if s := detailString(err, "reason"); s != "" {
		return s
	}
	if s := detailString(err, "rule"); s != "" {
		return s
	}
	return def
}
