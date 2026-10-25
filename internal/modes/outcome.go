package modes

import (
	"errors"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Kind is the typed result of a routed request: the failure and fallback table of additive plan
// section 13, one value per row, plus Served.
type Kind string

// The outcomes. The first group are the section 13 rows; the second are the same table's neighbours
// that need their own name so a caller can never mistake them for another row.
const (
	// KindServed: the request was served, by the local provider or an external one.
	KindServed Kind = "served"

	// KindUnavailable: provider unavailable (or the local provider could not serve).
	KindUnavailable Kind = "unavailable"
	// KindTimeout: a typed timeout; the local goal state is untouched.
	KindTimeout Kind = "timeout"
	// KindUncertain: the request may have reached the provider and the outcome is unknown, on a
	// request that could cause a commit. Reconcile before anything is retried.
	KindUncertain Kind = "uncertain_outcome"
	// KindUnsupportedModality: no qualified candidate supports what the request needs.
	KindUnsupportedModality Kind = "unsupported_modality"
	// KindEgressDenied: the data-egress guard refused; the data stayed local.
	KindEgressDenied Kind = "egress_denied"
	// KindBudgetExhausted: the budget refused (or the provider's account is out of quota); no
	// external call was made, or none will be repeated.
	KindBudgetExhausted Kind = "budget_exhausted"
	// KindStaleProfile: the provider profile is quarantined, stale, not qualified or retired.
	KindStaleProfile Kind = "stale_profile"
	// KindNoFallback: a fallback was due and the local provider cannot serve; AI action execution
	// is disabled while the application and its deterministic APIs keep working.
	KindNoFallback Kind = "no_fallback"

	// KindInvalidCredential: the provider credential is missing, invalid or rejected.
	KindInvalidCredential Kind = "invalid_credential"
	// KindRateLimited: a rate limit (the budget's or the provider's) applies; see RetryAfter.
	KindRateLimited Kind = "rate_limited"
	// KindMalformedOutput: the provider returned a malformed response or tool call.
	KindMalformedOutput Kind = "malformed_output"
	// KindProviderError: any other definite provider refusal (bad request, content filtered...).
	KindProviderError Kind = "provider_error"
)

// Target is where a request was (or would have been) served.
type Target string

// The targets.
const (
	TargetNone     Target = "none"
	TargetLocal    Target = "local"
	TargetExternal Target = "external"
)

// Hop records one fallback: from a profile to another profile, or to the local provider.
type Hop struct {
	From      string
	To        string // a profile id, or "local"
	Condition Condition
}

// LocalHop is the To of a hop to the local provider.
const LocalHop = "local"

// Outcome is everything the host and the audit trail need to know about one routed request. It holds
// no request or response content.
type Outcome struct {
	Kind Kind
	// Reason is a fixed lower_snake code (a providerreg or providerbudget reason, an egress rule, or
	// one of this package's), never free text.
	Reason string
	Role   Role
	// Mode is the effective mode the request was routed under; DecidedBy says which level (or the
	// kill switch) set it.
	Mode      Mode
	DecidedBy string
	Target    Target
	// Profile is the external profile involved at the end, or empty.
	Profile pv.ProfileID
	// Hops lists every fallback taken, in order. A fallback is never taken unrecorded.
	Hops []Hop
	// DataSent is true once an external request may have left the deployment.
	DataSent bool
	// ReservationID is the budget reservation of the last external call, or empty.
	ReservationID pv.ReservationID
	// RetryAfter is set for a rate limit.
	RetryAfter time.Duration
	// AIActionsDisabled is true when the controller could not be served: AI action execution is off
	// while the application, deterministic APIs and search keep working.
	AIActionsDisabled bool
	// SettleFailed is true when the budget could not be settled; the reservation holds its upper
	// bound until it times out.
	SettleFailed bool
	// QuarantineFailed is true when a reply from another model than the pinned one was refused but the
	// profile could not be quarantined; the operator must hold it by hand.
	QuarantineFailed bool
}

// Served reports whether the request was served.
func (o Outcome) Served() bool { return o.Kind == KindServed }

// code maps an outcome kind to the error code a caller sees. None of them invites a blind retry of
// a request that may have been processed: uncertain_outcome and quarantined are not retryable codes.
func (k Kind) code() contracts.ErrCode {
	switch k {
	case KindUnavailable:
		return contracts.ErrUnavailable
	case KindTimeout:
		return contracts.ErrTimeout
	case KindUncertain:
		return contracts.ErrUncertainOutcome
	case KindUnsupportedModality:
		return contracts.ErrValidationFailed
	case KindEgressDenied, KindInvalidCredential:
		return contracts.ErrPermissionDenied
	case KindBudgetExhausted:
		return contracts.ErrResourceExhausted
	case KindStaleProfile:
		return contracts.ErrQuarantined
	case KindNoFallback:
		return contracts.ErrDegraded
	case KindRateLimited:
		return contracts.ErrRateLimited
	case KindMalformedOutput:
		return contracts.ErrSchemaInvalid
	}
	return contracts.ErrInternal
}

// Err returns nil for a served request and otherwise a *contracts.Error carrying Details["outcome"],
// ["reason"], ["role"], ["data_sent"] and ["alternate_provider"] ("none" unless a fallback hop was
// recorded). A failure is never a success-looking result.
func (o Outcome) Err() error {
	if o.Kind == KindServed {
		return nil
	}
	alt := "none"
	if len(o.Hops) > 0 {
		alt = o.Hops[len(o.Hops)-1].To
	}
	e := contracts.New(o.Kind.code(), "modes: the request was not served: "+string(o.Kind)).
		WithDetails("outcome", string(o.Kind)).
		WithDetails("reason", o.Reason).
		WithDetails("role", string(o.Role)).
		WithDetails("data_sent", o.DataSent).
		WithDetails("alternate_provider", alt).
		WithDetails("ai_actions_disabled", o.AIActionsDisabled)
	if o.RetryAfter > 0 {
		e = e.WithDetails("retry_after_ms", o.RetryAfter.Milliseconds())
	}
	return e
}

// OutcomeOf returns the Kind carried by an error returned from Execute, or "".
func OutcomeOf(err error) Kind {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		if s, ok := ce.Details["outcome"].(string); ok {
			return Kind(s)
		}
	}
	return ""
}
