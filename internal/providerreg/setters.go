package providerreg

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Events recorded by the setters of this file.
const (
	EventPricingSet       = "pricing_set"
	EventBillingNoticeSet = "billing_notice_set"
)

var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// maxMicroUnits bounds a price so a later multiplication cannot overflow an int64.
const maxMicroUnits = 1 << 50

// validatePricing holds a price table to the grammar of schemas/providers/price-table.
func validatePricing(t providers.PriceTable, now time.Time) error {
	if !currencyRE.MatchString(string(t.Currency)) {
		return invalid("the currency is three upper case letters")
	}
	d, err := parseDate(t.EffectiveOn)
	if err != nil || d.After(now.UTC().Truncate(24*time.Hour)) {
		return invalid("the price date is malformed or in the future")
	}
	if t.SourceURL != nil && (!strings.HasPrefix(*t.SourceURL, "https://") || len(*t.SourceURL) > 300) {
		return invalid("the price source is an https page")
	}
	p := t.Prices
	for _, u := range []*providers.PriceTablePrice{p.ToksIn, p.ToksOut, p.ToksInCached, p.ImagesIn, p.ImagesOut, p.AudioSecondsIn, p.AudioSecondsOut, p.FileBytesIn, p.Requests} {
		if u != nil && (u.PerUnits < 1 || u.MicroUnits < 0 || u.MicroUnits > maxMicroUnits || u.PerUnits > maxMicroUnits) {
			return invalid("a price covers at least one unit and is a non-negative amount of micro-units")
		}
	}
	return nil
}

// SetPricing replaces the profile's price table. The table is the operator's (host) and is re-read at
// the review interval, so a price change is a change to the profile and not a new profile: the id, the
// credential, the acknowledgement, the conformance report and the lifecycle are kept. A quarantined
// profile stays quarantined; a retired one is refused. A later call is priced from the new table; a call
// already reserved keeps the estimate it was admitted with.
func (r *Registry) SetPricing(ctx context.Context, scope contracts.Scope, id providers.ProfileID, table providers.PriceTable) (providers.ProviderProfile, error) {
	if _, err := appOf(scope); err != nil {
		return providers.ProviderProfile{}, err
	}
	if err := validatePricing(table, r.clk.Now()); err != nil {
		return providers.ProviderProfile{}, err
	}
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		}
		e.p.Pricing = table
		e.note(EventPricingSet, "", "")
		return nil
	})
}

// SetBillingNotice replaces the profile's billing notice. A notice that differs from the current one
// (version, text or URL) invalidates the recorded acknowledgement, which is dropped, and a qualified
// profile returns to candidate so that the operator acknowledges the new notice and qualifies it again;
// it keeps its id, credential and conformance report. A quarantined profile stays quarantined for its own
// reason. The notice is held to the rules of a draft: a version and a text, and never a claim that a
// subscription grants API access. The same notice is a no-op.
func (r *Registry) SetBillingNotice(ctx context.Context, scope contracts.Scope, id providers.ProfileID, n providers.ProviderProfileBillingNotice) (providers.ProviderProfile, error) {
	if _, err := appOf(scope); err != nil {
		return providers.ProviderProfile{}, err
	}
	if n.SubscriptionGrantsAPIAccess || strings.TrimSpace(n.Version) == "" || strings.TrimSpace(n.Text) == "" {
		return providers.ProviderProfile{}, invalid("a billing notice needs a version and text and never claims a subscription is API access")
	}
	if n.URL != nil && (!strings.HasPrefix(*n.URL, "https://") || len(*n.URL) > 300) {
		return providers.ProviderProfile{}, invalid("the billing page is an https URL")
	}
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		}
		if NoticeDigest(e.p.BillingNotice) == NoticeDigest(n) && e.p.BillingNotice.Version == n.Version {
			return nil
		}
		e.p.BillingNotice = n
		e.p.BillingNoticeAck = nil
		if e.p.Lifecycle == providers.ProfileLifecycleQualified {
			e.setLifecycle(providers.ProfileLifecycleCandidate)
		}
		e.note(EventBillingNoticeSet, ReasonBillingNoticeNotAcknowledged, "")
		return nil
	})
}
