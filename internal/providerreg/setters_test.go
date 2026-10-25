package providerreg

import (
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// A price or notice change is a change to the profile, not a new profile: the id, credential and
// conformance report stay, a new notice invalidates the old acknowledgement and takes a qualified profile
// back to candidate until the operator acknowledges it and qualifies it again.
func TestSetPricingAndBillingNoticeNeedNoNewProfile(t *testing.T) {
	g := newRig(t)
	p := g.qualified(credRef(1))

	table := p.Pricing
	table.Prices.ToksIn = &providers.PriceTablePrice{PerUnits: 1000000, MicroUnits: 250000}
	table.EffectiveOn = "2026-10-02"
	priced := g.prof(g.reg.SetPricing(bg, g.scope, p.ProfileID, table))
	if priced.ProfileID != p.ProfileID || priced.Lifecycle != providers.ProfileLifecycleQualified ||
		priced.Pricing.Prices.ToksIn.MicroUnits != 250000 || priced.CredRef == nil || priced.LastConformance == nil || priced.BillingNoticeAck == nil {
		t.Fatalf("after SetPricing: %+v", priced)
	}
	if _, err := g.route(g.scope, p.ProfileID, req()); err != nil {
		t.Fatalf("a repriced profile was refused: %v", err)
	}
	if !hasEvent(eventKinds(t, g, p.ProfileID), EventPricingSet) {
		t.Error("the price change is not in the event record")
	}

	for name, mut := range map[string]func(*providers.PriceTable){
		"bad currency": func(t *providers.PriceTable) { t.Currency = "usd" },
		"future date":  func(t *providers.PriceTable) { t.EffectiveOn = "2027-01-01" },
		"zero per units": func(t *providers.PriceTable) {
			t.Prices.ToksIn = &providers.PriceTablePrice{PerUnits: 0, MicroUnits: 1}
		},
		"negative price": func(t *providers.PriceTable) {
			t.Prices.ToksOut = &providers.PriceTablePrice{PerUnits: 1, MicroUnits: -1}
		},
		"http source": func(t *providers.PriceTable) { u := "http://example.com/prices"; t.SourceURL = &u },
	} {
		bad := table
		mut(&bad)
		if _, err := g.reg.SetPricing(bg, g.scope, p.ProfileID, bad); ReasonOf(err) != ReasonInvalidProfile {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := g.prof(g.reg.Get(bg, g.scope, p.ProfileID)); got.Pricing.Prices.ToksIn.MicroUnits != 250000 {
		t.Fatal("a refused price table was stored")
	}

	// The same notice changes nothing; a different one drops the acknowledgement and the qualification.
	if got := g.prof(g.reg.SetBillingNotice(bg, g.scope, p.ProfileID, p.BillingNotice)); got.BillingNoticeAck == nil || got.Lifecycle != providers.ProfileLifecycleQualified {
		t.Fatalf("the same notice changed the profile: %v", got.Lifecycle)
	}
	n := p.BillingNotice
	n.Version, n.Text = "2026-10-09.1", "A consumer chat subscription is not API access; API calls are billed separately."
	changed := g.prof(g.reg.SetBillingNotice(bg, g.scope, p.ProfileID, n))
	if changed.ProfileID != p.ProfileID || changed.BillingNotice.Version != n.Version || changed.BillingNoticeAck != nil ||
		changed.Lifecycle != providers.ProfileLifecycleCandidate || changed.CredRef == nil || changed.LastConformance == nil {
		t.Fatalf("after SetBillingNotice: %+v", changed)
	}
	if _, err := g.reg.Qualify(bg, g.scope, p.ProfileID); err == nil {
		t.Fatal("qualified with an unacknowledged notice")
	}
	g.ack(changed)
	if got := g.prof(g.reg.Qualify(bg, g.scope, p.ProfileID)); got.Lifecycle != providers.ProfileLifecycleQualified {
		t.Fatalf("after the new acknowledgement: %v", got.Lifecycle)
	}
	if !hasEvent(eventKinds(t, g, p.ProfileID), EventBillingNoticeSet) {
		t.Error("the notice change is not in the event record")
	}

	// A notice that claims a subscription is API access or has no text is refused; so is a retired
	// profile, and another application's profile is not found.
	for name, mut := range map[string]func(*providers.ProviderProfileBillingNotice){
		"claims API": func(n *providers.ProviderProfileBillingNotice) { n.SubscriptionGrantsAPIAccess = true },
		"no text":    func(n *providers.ProviderProfileBillingNotice) { n.Text = " " },
		"no version": func(n *providers.ProviderProfileBillingNotice) { n.Version = "" },
		"http bill":  func(n *providers.ProviderProfileBillingNotice) { u := "http://example.com"; n.URL = &u },
	} {
		bad := n
		mut(&bad)
		if _, err := g.reg.SetBillingNotice(bg, g.scope, p.ProfileID, bad); ReasonOf(err) != ReasonInvalidProfile {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := g.reg.SetPricing(bg, g.scopeOf("other-app", tTenant, tWsp), p.ProfileID, table); ReasonOf(err) != ReasonProfileNotFound {
		t.Errorf("another application: %v", err)
	}
	g.prof(g.reg.Retire(bg, g.scope, p.ProfileID))
	if _, err := g.reg.SetPricing(bg, g.scope, p.ProfileID, table); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("a retired profile was repriced: %v", err)
	}
	if _, err := g.reg.SetBillingNotice(bg, g.scope, p.ProfileID, n); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("a retired profile got a notice: %v", err)
	}
}
