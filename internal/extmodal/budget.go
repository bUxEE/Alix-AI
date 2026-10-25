package extmodal

import (
	"context"
	"time"

	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerbudget"
	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Budgets is the provider budget an external call is reserved against. *providerbudget.Service implements it.
type Budgets interface {
	Reserve(ctx context.Context, scope contracts.Scope, call providerbudget.Call) (providerbudget.Reservation, error)
	Settle(ctx context.Context, scope contracts.Scope, id pv.ReservationID, a providerbudget.Actual) (providerbudget.Settlement, error)
	Release(ctx context.Context, scope contracts.Scope, id pv.ReservationID) error
}

var _ Budgets = (*providerbudget.Service)(nil)

// notEstimable refuses a call that has no upper bound to reserve.
func notEstimable(m Modality, msg string) error {
	return contracts.New(contracts.ErrResourceExhausted, "extmodal: "+msg+", so no external call is made").
		WithDetails("reason", string(ReasonNotEstimable)).WithDetails("modality", string(m))
}

// outputCap is the most output toks the target can return: the smaller of what the profile and the adapter
// state, or 0 when neither does.
func (t Target) outputCap() int {
	c := t.Caps.MaxOutputToks
	if p := int(t.Profile.Limits.MaxOutputToks); p > 0 && (c <= 0 || p < c) {
		c = p
	}
	return max(c, 0)
}

// richUnits is the upper bound of a rich request, as the text router prices one
// (providers.Budget.EstimateRichCost): the estimated prompt with a fixed cost per media reference, the output cap
// the request carries, and every image it sends.
func richUnits(rr providers.RichRequest) providerbudget.Units {
	u := providerbudget.Units{ToksIn: pv.Counter(providers.Budget{}.EstimateRich(rr)), ToksOut: pv.Counter(rr.MaxToks), Requests: 1}
	for _, m := range rr.Messages {
		for _, p := range m.Parts {
			if p.Kind == providers.ModImage {
				u.ImagesIn++
			}
		}
	}
	return u
}

// hold is the reservation of one external call.
type hold struct {
	s     *Service
	scope contracts.Scope
	id    pv.ReservationID
	units providerbudget.Units
	start time.Time
}

// reserve holds the upper bound of an external call on the provider budget before anything is sent, exactly as
// the text router does (internal/modes): no budget, or a budget that refuses (exhausted, rate limited, no budget
// for the call's channel), means the call is not made, and nothing else is tried. The reservation belongs to the
// egress decision that allowed the call's text.
func (s *Service) reserve(ctx context.Context, scope contracts.Scope, t Target, m Modality, d TextDecision, units providerbudget.Units) (*hold, error) {
	if s.d.Budgets == nil {
		err := contracts.New(contracts.ErrResourceExhausted, "extmodal: no provider budget is configured, so no external call is made").
			WithDetails("reason", string(ReasonNoBudget)).WithDetails("modality", string(m))
		s.observe(Event{Modality: m, Provider: string(t.Profile.ProviderID), Model: string(t.Profile.ModelID), Reason: ReasonNoBudget})
		return nil, err
	}
	p := t.Profile
	rsv, err := s.d.Budgets.Reserve(ctx, scope, providerbudget.Call{
		Provider: pv.ProviderRef{ProfileID: p.ProfileID, ProviderID: p.ProviderID, ModelID: p.ModelID, ModelVersion: p.ModelVersion},
		Role:     m.role(), Channel: pv.ChannelInteractive, EnvelopeID: d.EnvelopeID, DecisionID: d.DecisionID,
		Prices: p.Pricing, Units: units,
	})
	if err != nil {
		if contracts.CodeOf(err) == contracts.ErrValidationFailed {
			// The call cannot be priced on the budget (a unit the price table does not price, another currency):
			// there is no bound to reserve, so no call.
			err = contracts.Wrap(contracts.ErrResourceExhausted, err, "extmodal: the call cannot be priced on the provider budget, so no external call is made").
				WithDetails("reason", string(ReasonNotEstimable)).WithDetails("modality", string(m))
		}
		s.observe(Event{Modality: m, Provider: string(p.ProviderID), Model: string(p.ModelID), Reason: ReasonOf(err)})
		return nil, err
	}
	return &hold{s: s, scope: scope, id: rsv.ID, units: units, start: s.clk.Now()}, nil
}

// settle records what the call consumed. It runs even when the caller's context is cancelled: spend that happened
// is recorded. A settlement that fails leaves the reservation to time out holding its bound.
func (h *hold) settle(ctx context.Context, a providerbudget.Actual) {
	a.Latency = pv.UsageRecordLatency{TotalMS: pv.Counter(max(h.s.clk.Now().Sub(h.start).Milliseconds(), 0))}
	_, _ = h.s.d.Budgets.Settle(context.WithoutCancel(ctx), h.scope, h.id, a)
}

// succeeded settles a call that returned, with the usage the provider reported (or AlixAI estimated).
func (h *hold) succeeded(ctx context.Context, u providers.CostUsage) {
	a := providerbudget.Actual{
		Outcome: pv.UsageRecordOutcomeSucceeded, MeteringBasis: pv.UsageRecordMeteringBasisProviderReported,
		Metering: providerbudget.Units{
			ToksIn: pv.Counter(u.ToksIn), ToksOut: pv.Counter(u.ToksOut), ImagesIn: pv.Counter(u.ImagesIn), ImagesOut: pv.Counter(u.ImagesOut), Requests: 1,
		},
	}
	if u.Estimated {
		a.MeteringBasis = pv.UsageRecordMeteringBasisEstimated
	} else if u.CostMicros > 0 {
		m := pv.MicroUnits(u.CostMicros)
		a.ReportedMicroUnits = &m
	}
	h.settle(ctx, a)
}

// estimated settles a call that returned with no usage of its own: the reserved units, as AlixAI estimated them.
func (h *hold) estimated(ctx context.Context) {
	h.settle(ctx, providerbudget.Actual{Outcome: pv.UsageRecordOutcomeSucceeded, MeteringBasis: pv.UsageRecordMeteringBasisEstimated, Metering: h.units})
}

// failed ends the reservation of a call that returned an error. A call the adapter refused before anything was
// sent gives the reservation back; one the provider refused before processing it settles as failed with nothing
// metered; any other may have been billed and settles at the reserved bound (uncertain).
func (h *hold) failed(ctx context.Context, err error) {
	ce := modes.CallErrorOf(err)
	if !ce.Sent {
		_ = h.s.d.Budgets.Release(context.WithoutCancel(ctx), h.scope, h.id)
		return
	}
	kind := ce.Kind
	if !kind.Valid() {
		kind = pv.ProviderErrorKindInternal
	}
	a := providerbudget.Actual{Outcome: pv.UsageRecordOutcomeUncertain, ErrorKind: &kind, MeteringBasis: pv.UsageRecordMeteringBasisProviderReported}
	switch kind {
	case pv.ProviderErrorKindAuthenticationFailed, pv.ProviderErrorKindPermissionDenied, pv.ProviderErrorKindQuotaExceeded,
		pv.ProviderErrorKindRateLimited, pv.ProviderErrorKindBadRequest, pv.ProviderErrorKindContextTooLarge,
		pv.ProviderErrorKindModelNotFound, pv.ProviderErrorKindContentFiltered:
		a.Outcome = pv.UsageRecordOutcomeFailed
	}
	h.settle(ctx, a)
}
