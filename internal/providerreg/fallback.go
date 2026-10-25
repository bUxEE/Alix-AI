package providerreg

import (
	"context"
	"slices"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// FallbackCondition is why a caller asks for the fallback. It is a closed set the caller states
// explicitly; it is never derived from an error code, so a failure cannot be re-labelled into a
// reason to switch provider.
type FallbackCondition string

// The conditions a profile's policy may name (schema: fallback.on).
const (
	FallbackOnUnavailable         FallbackCondition = "unavailable"
	FallbackOnTimeout             FallbackCondition = "timeout"
	FallbackOnRateLimited         FallbackCondition = "rate_limited"
	FallbackOnUnsupportedModality FallbackCondition = "unsupported_modality"
)

// The conditions that NEVER select an alternate provider. They exist as constants so a caller
// that asks is refused with a typed reason instead of being unrepresentable by accident.
const (
	FallbackOnInvalidCredential FallbackCondition = "invalid_credential"
	FallbackOnStaleTerms        FallbackCondition = "stale_terms"
	FallbackOnEgressDenied      FallbackCondition = "egress_denied"
	FallbackOnBudgetExhausted   FallbackCondition = "budget_exhausted"
)

// allowedOn maps a condition a caller may ask for to the schema value a policy lists.
func (c FallbackCondition) allowedOn() (providers.ProviderProfileFallbackOnItem, bool) {
	item := providers.ProviderProfileFallbackOnItem(c)
	switch c {
	case FallbackOnUnavailable, FallbackOnTimeout, FallbackOnRateLimited, FallbackOnUnsupportedModality:
		return item, true
	}
	return "", false
}

// FallbackRecord is the record of one fallback hop. The caller keeps it with its audit trail; the
// registry also records it as an event on the primary profile.
type FallbackRecord struct {
	From      providers.ProfileID
	To        providers.ProfileID
	Condition FallbackCondition
	At        time.Time
}

// SetFallback replaces the profile's fallback: the one other profile of the application it may
// hand over to, and the conditions for it. The target must exist, must not be the profile
// itself and must not lead back to it. Nil target with no conditions removes the fallback.
func (r *Registry) SetFallback(ctx context.Context, scope contracts.Scope, id providers.ProfileID, f providers.ProviderProfileFallback) (providers.ProviderProfile, error) {
	if _, err := appOf(scope); err != nil {
		return providers.ProviderProfile{}, err
	}
	if err := validateFallbackShape(f, id); err != nil {
		return providers.ProviderProfile{}, err
	}
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		}
		if f.ProfileID != nil {
			if err := checkFallbackTarget(ctx, e.q, e.app, id, *f.ProfileID); err != nil {
				return err
			}
		}
		e.p.Fallback = providers.ProviderProfileFallback{ProfileID: f.ProfileID, On: slices.Clone(f.On)}
		related := ""
		if f.ProfileID != nil {
			related = string(*f.ProfileID)
		}
		e.note(EventFallbackSet, "", related)
		return nil
	})
}

// checkFallbackTarget refuses a target that does not exist in the application or whose own
// fallbacks lead back to self.
func checkFallbackTarget(ctx context.Context, q store.Reader, app string, self, target providers.ProfileID) error {
	cur := target
	for range maxChainLen {
		p, found, err := loadProfile(ctx, q, app, cur)
		if err != nil {
			return err
		}
		if !found {
			return fail(contracts.ErrValidationFailed, ReasonFallbackInvalid, "the fallback profile does not exist in this application")
		}
		if p.Fallback.ProfileID == nil {
			return nil
		}
		cur = *p.Fallback.ProfileID
		if cur == self {
			return fail(contracts.ErrValidationFailed, ReasonFallbackInvalid, "the fallback chain would lead back to this profile")
		}
	}
	return fail(contracts.ErrValidationFailed, ReasonFallbackInvalid, "the fallback chain is too long")
}

// Chain returns the fallback chain data: the profile followed by each fallback it names in turn.
// It is data for inspection and settings screens; following it is Fallback's job, one hop at a
// time, under the policy of each profile.
func (r *Registry) Chain(ctx context.Context, scope contracts.Scope, id providers.ProfileID) ([]providers.ProfileID, error) {
	var chain []providers.ProfileID
	cur := id
	for range maxChainLen {
		p, err := r.Get(ctx, scope, cur)
		if err != nil {
			return nil, err
		}
		if slices.Contains(chain, cur) {
			return nil, fail(contracts.ErrInternal, ReasonFallbackInvalid, "the fallback chain loops")
		}
		chain = append(chain, cur)
		if p.Fallback.ProfileID == nil {
			return chain, nil
		}
		cur = *p.Fallback.ProfileID
	}
	return nil, fail(contracts.ErrInternal, ReasonFallbackInvalid, "the fallback chain is too long")
}

// Fallback returns the admission for the fallback of profile from, for a request that could not
// be served by it, and records the hop. It follows the fallback only when ALL of these hold:
//
//   - the condition is one a policy may name (unavailable, timeout, rate_limited,
//     unsupported_modality); invalid credentials, stale terms, denied egress and an exhausted
//     budget are refused with fallback_condition_forbidden whatever the caller says;
//   - the primary profile is itself permitted for the scope, qualified and fresh, and its
//     credential is valid: a primary with an invalid credential or stale terms never hands over,
//     even when the caller reports the failure as "unavailable";
//   - the primary's policy names a fallback for exactly this condition;
//   - the target passes every gate (Check) for the same request: permitted scope, qualified,
//     fresh, allowed data class, capabilities, valid credential.
//
// Only one hop is made. A refusal never selects another provider.
func (r *Registry) Fallback(ctx context.Context, scope contracts.Scope, from providers.ProfileID, cond FallbackCondition, req Requirement) (FallbackRecord, Admission, error) {
	item, ok := cond.allowedOn()
	if !ok {
		return FallbackRecord{}, Admission{}, fail(contracts.ErrPermissionDenied, ReasonFallbackConditionForbidden, "this condition never selects an alternate provider")
	}
	primary, err := r.Get(ctx, scope, from)
	if err != nil {
		return FallbackRecord{}, Admission{}, err
	}
	if primary, err = r.usable(ctx, scope, primary); err != nil {
		return FallbackRecord{}, Admission{}, err
	}
	if err := r.credentialUsable(ctx, scope, primary); err != nil {
		return FallbackRecord{}, Admission{}, err
	}
	if primary.Fallback.ProfileID == nil || !slices.Contains(primary.Fallback.On, item) {
		return FallbackRecord{}, Admission{}, fail(contracts.ErrPermissionDenied, ReasonFallbackNotNamed, "the profile's policy names no fallback for this condition")
	}
	target := *primary.Fallback.ProfileID
	adm, err := r.Check(ctx, scope, target, req)
	if err != nil {
		return FallbackRecord{}, Admission{}, err
	}
	rec := FallbackRecord{From: from, To: target, Condition: cond, At: r.clk.Now()}
	if _, err := r.mutate(ctx, scope, from, actorOf(scope), func(e *edit) error {
		e.note(EventFallbackUsed, Reason(cond), string(target))
		return nil
	}); err != nil {
		return FallbackRecord{}, Admission{}, err
	}
	return rec, adm, nil
}
