package modes

import (
	"context"

	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// StreamLease is an external call that passed every gate and reserved budget, but has not
// settled yet. Used for provider streaming (ADR 0046): reservation happens before the stream
// starts; Settle* runs after completion, cancel, or failure. No provider switch mid-stream.
type StreamLease struct {
	Call   ExternalCall
	Scope  contracts.Scope
	r      *Router
	out    *Outcome
	closed bool
}

// OpenStream runs the same selection and gates as Execute through budget reservation, then
// returns a lease. The caller must SendStream using Call and then SettleSuccess / SettleFail /
// SettleCancel. Local-only outcomes return (nil, outcome, nil) with TargetLocal or a failure Kind.
func (r *Router) OpenStream(ctx context.Context, req Request) (*StreamLease, Outcome, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, Outcome{}, err
	}
	if !req.Role.Valid() {
		return nil, Outcome{}, contracts.New(contracts.ErrValidationFailed, "modes: unknown role")
	}
	if req.Channel == "" {
		req.Channel = pv.ChannelInteractive
	}
	if !req.Channel.Valid() {
		return nil, Outcome{}, contracts.New(contracts.ErrValidationFailed, "modes: unknown channel")
	}
	if req.Estimate == nil {
		return nil, Outcome{}, contracts.New(contracts.ErrInternal, "modes: estimate required for stream")
	}
	res, err := r.resolver.Resolve(ctx, req.Scope)
	if err != nil {
		return nil, Outcome{}, err
	}
	x := &run{r: r, req: req, out: Outcome{Role: req.Role, Mode: res.Mode, DecidedBy: res.DecidedBy, Target: TargetNone}}
	lease, o := x.openStream(ctx, res)
	if o.Kind != KindServed && (req.Role == RoleController || req.Role == RoleRealisation) {
		o.AIActionsDisabled = true
	}
	return lease, o, o.Err()
}

func (x *run) openStream(ctx context.Context, res Resolved) (*StreamLease, Outcome) {
	ext, err := x.req.Role.External()
	if err != nil {
		return nil, x.failed(&failure{kind: KindUnavailable, reason: detailReason(err, "role_local_only")})
	}
	if res.Mode == ModeLocal || res.KillSwitch {
		return nil, x.failed(&failure{kind: KindUnavailable, reason: "stream_local_only"})
	}
	rp, err := x.r.policy.RolePolicy(ctx, x.req.Scope, x.req.Role)
	if err != nil {
		switch detailReason(err, "") {
		case "llm_not_configured", "private_llm_starting", "private_llm_unavailable":
			return nil, x.failed(&failure{kind: KindUnavailable, reason: detailReason(err, "llm_not_configured")})
		}
		return nil, x.failed(&failure{kind: KindUnavailable, reason: "policy_unreadable"})
	}
	if len(rp.Profiles) == 0 {
		return nil, x.failed(&failure{kind: KindUnavailable, reason: "llm_not_configured"})
	}
	if res.Mode == ModeHybrid && sensitiveClass(x.req.Requirement.DataClass) {
		return nil, x.failed(&failure{kind: KindUnavailable, reason: "hybrid_sensitive_local"})
	}

	rq := x.req.Requirement
	rq.Role = ext
	scope := x.req.Scope

	var cur *providerreg.Admission
	var pending *failure
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
		pending = classifyCheck(err)
		break
	}
	if cur == nil {
		if pending == nil {
			pending = &failure{kind: KindUnsupportedModality, cond: CondUnsupportedModality, reason: "no_candidate_supports_request"}
		}
		return nil, x.failed(pending)
	}

	lease, f := x.reserve(ctx, rq, *cur)
	if f != nil {
		return nil, x.failed(f)
	}
	x.out.Kind, x.out.Reason, x.out.Target = KindServed, "served", TargetExternal
	return lease, x.out
}

// reserve is attempt without the send: kill switch, egress, budget, second kill check.
func (x *run) reserve(ctx context.Context, rq providerreg.Requirement, adm providerreg.Admission) (*StreamLease, *failure) {
	prof := adm.Profile
	scope := x.req.Scope
	x.out.Profile = prof.ProfileID

	if x.r.resolver.killed(ctx, scope.DeploymentID) {
		return nil, &failure{kind: KindEgressDenied, reason: DecidedByKillSwitch, forceLocal: true}
	}
	cand := x.req.Candidate
	cand.Scope, cand.Profile, cand.Role, cand.Channel = scope, prof, rq.Role, x.req.Channel
	gr, err := x.r.guard.Evaluate(ctx, cand)
	if err != nil || gr.Envelope == nil {
		if detailString(err, "check") == string(pv.EgressDecisionCheckNameKillSwitch) {
			return nil, &failure{kind: KindEgressDenied, reason: DecidedByKillSwitch, forceLocal: true}
		}
		return nil, &failure{kind: KindEgressDenied, cond: CondEgressDenied, reason: detailReason(err, "guard_failure")}
	}
	units, floor, err := x.req.Estimate(prof)
	if err != nil {
		return nil, &failure{kind: KindBudgetExhausted, cond: CondBudgetExhausted, reason: "estimate_failed"}
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
		return nil, budgetFailure(err)
	}
	x.out.ReservationID = rsv.ID
	if x.r.resolver.killed(ctx, scope.DeploymentID) {
		x.giveBack(ctx, rsv.ID)
		return nil, &failure{kind: KindEgressDenied, reason: DecidedByKillSwitch, forceLocal: true}
	}
	x.out.DataSent = true
	return &StreamLease{
		Call: ExternalCall{
			Admission: adm, Envelope: gr.Envelope, Decision: gr.Decision, Disclosure: gr.Disclosure,
			Reservation: rsv, FallbackFrom: x.fallbackFrom,
		},
		Scope: scope, r: x.r, out: &x.out,
	}, nil
}

// SettleSuccess records a completed stream.
func (l *StreamLease) SettleSuccess(ctx context.Context, er ExternalResult) {
	if l == nil || l.closed {
		return
	}
	l.closed = true
	if er.ReplyModel != "" {
		prof := l.Call.Admission.Profile
		if !providerreg.ReplyModelMatches(prof.ModelID, prof.ModelVersion, er.ReplyModel) {
			l.settleMismatch(ctx, er)
			return
		}
	}
	_, _ = l.r.budgets.Settle(context.WithoutCancel(ctx), l.Scope, l.Call.Reservation.ID, providerbudget.Actual{
		Outcome: pv.UsageRecordOutcomeSucceeded, Metering: er.Metering, MeteringBasis: er.MeteringBasis,
		ReportedMicroUnits: er.ReportedMicroUnits,
	})
}

func (l *StreamLease) settleMismatch(ctx context.Context, er ExternalResult) {
	if o, ok := l.r.profiles.(profileObserver); ok {
		reported := er.ReplyModel
		_, _ = o.Observe(context.WithoutCancel(ctx), l.Scope, l.Call.Admission.Profile.ProfileID, providerreg.Observation{ReplyModel: &reported})
	}
	kind := pv.ProviderErrorKindMalformedResponse
	basis := er.MeteringBasis
	if basis == "" {
		basis = pv.UsageRecordMeteringBasisProviderReported
	}
	_, _ = l.r.budgets.Settle(context.WithoutCancel(ctx), l.Scope, l.Call.Reservation.ID, providerbudget.Actual{
		Outcome: pv.UsageRecordOutcomeFailed, ErrorKind: &kind, Metering: er.Metering, MeteringBasis: basis,
		ReportedMicroUnits: er.ReportedMicroUnits,
	})
}

// SettleFail records a failed stream. metering may be nil.
func (l *StreamLease) SettleFail(ctx context.Context, kind pv.ProviderErrorKind, metering *providerbudget.Units) {
	if l == nil || l.closed {
		return
	}
	l.closed = true
	a := providerbudget.Actual{Outcome: pv.UsageRecordOutcomeFailed, ErrorKind: &kind, MeteringBasis: pv.UsageRecordMeteringBasisProviderReported}
	if metering != nil {
		a.Metering = *metering
	}
	_, _ = l.r.budgets.Settle(context.WithoutCancel(ctx), l.Scope, l.Call.Reservation.ID, a)
}

// SettleCancel records a cancelled stream (client gone / Close before Done).
func (l *StreamLease) SettleCancel(ctx context.Context) {
	if l == nil || l.closed {
		return
	}
	l.closed = true
	if rl, ok := l.r.budgets.(reservationReleaser); ok {
		if err := rl.Release(context.WithoutCancel(ctx), l.Scope, l.Call.Reservation.ID); err == nil {
			return
		}
	}
	_, _ = l.r.budgets.Settle(context.WithoutCancel(ctx), l.Scope, l.Call.Reservation.ID, providerbudget.Actual{
		Outcome: pv.UsageRecordOutcomeCancelled, MeteringBasis: pv.UsageRecordMeteringBasisProviderReported,
	})
}
