package extprovidersec

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/internal/dataegress"
	"github.com/buxee/alix-ai/internal/modes"
	"github.com/buxee/alix-ai/internal/providerreg"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

func (w *world) profile(id pv.ProfileID) pv.ProviderProfile {
	w.t.Helper()
	p, err := w.reg.Registry.Get(ctxb(), receptionist(), id)
	if err != nil {
		w.t.Fatalf("Get: %v", err)
	}
	return p
}

// quarantinedFor fails unless the profile is quarantined for the reason.
func (w *world) quarantinedFor(id pv.ProfileID, reason pv.ProviderProfileQuarantineReason) {
	w.t.Helper()
	p := w.profile(id)
	if p.Lifecycle != pv.ProfileLifecycleQuarantined || p.QuarantineReason == nil || *p.QuarantineReason != reason {
		w.t.Fatalf("lifecycle %s reason %v, want quarantined for %s", p.Lifecycle, p.QuarantineReason, reason)
	}
}

// refusedAndSilent runs a turn and fails unless it was refused with nothing sent, reserved or run anywhere else.
func (w *world) refusedAndSilent(why string) {
	w.t.Helper()
	w.proposeBook()
	w.localProposeBook()
	res := w.say(receptionist(), sayBook)
	if !res.Failed() {
		w.t.Fatalf("%s: act %s, the request was served", why, res.Directive.Act)
	}
	w.nothingExecuted(res)
	if w.snd.sends() != 0 || w.budget.reservations() != 0 || w.local.Calls() != 0 {
		w.t.Fatalf("%s: sends %d reservations %d local calls %d", why, w.snd.sends(), w.budget.reservations(), w.local.Calls())
	}
}

// SEC-123
func TestSecExtRegionChangeQuarantinesProvider(t *testing.T) {
	sc := receptionist()
	moved := []pv.RegionCode{"us", "ap"} // the provider now also processes in a region nobody approved

	t.Run("control: the regions the provider reports unchanged leave the profile in service", func(t *testing.T) {
		w := newWorld(t)
		p, err := w.reg.Registry.Observe(ctxb(), sc, w.profA, providerreg.Observation{Regions: []pv.RegionCode{"eu", "us"}})
		if err != nil || p.Lifecycle != pv.ProfileLifecycleQualified {
			t.Fatalf("lifecycle %s err %v", p.Lifecycle, err)
		}
		healthy(t, w)
	})

	t.Run("a changed processing region quarantines the profile: no request, no alternate provider", func(t *testing.T) {
		w := newWorld(t, withSecondProfile(), withLocalFallback(transportConditions...))
		if _, err := w.reg.Registry.Observe(ctxb(), sc, w.profA, providerreg.Observation{Regions: moved}); err != nil {
			t.Fatal(err)
		}
		w.quarantinedFor(w.profA, pv.ProviderProfileQuarantineReasonRegionChanged)
		w.refusedAndSilent("region changed")

		// The router names the row: stale_profile, quarantined, nothing sent, no hop.
		o, err, sent, localRuns := w.routeDirect(sc, true)
		if o.Kind != modes.KindStaleProfile || contracts.CodeOf(err) != contracts.ErrQuarantined || o.Reason != string(providerreg.ReasonRegionChanged) {
			t.Fatalf("outcome %+v err %v", o, err)
		}
		if sent != 0 || localRuns != 0 || o.DataSent || len(o.Hops) != 0 {
			t.Fatalf("sent %d local runs %d outcome %+v", sent, localRuns, o)
		}
	})

	t.Run("a review does not end the quarantine; only a release with the accepted regions does, and it needs a new qualification", func(t *testing.T) {
		w := newWorld(t)
		if _, err := w.reg.Registry.Observe(ctxb(), sc, w.profA, providerreg.Observation{Regions: moved}); err != nil {
			t.Fatal(err)
		}
		p := w.profile(w.profA)
		_, _ = w.reg.Registry.Review(ctxb(), sc, w.profA, providerreg.ReviewInput{TermsHash: p.Terms.TermsHash, ReviewedOn: "2026-10-12"})
		w.quarantinedFor(w.profA, pv.ProviderProfileQuarantineReasonRegionChanged)
		w.refusedAndSilent("after a review")

		if _, err := w.reg.Registry.Release(ctxb(), sc, w.profA, providerreg.ReleaseInput{ReviewedOn: "2026-10-12", Regions: []pv.RegionCode{"us", "eu"}}); err != nil {
			t.Fatalf("Release: %v", err)
		}
		if got := w.profile(w.profA); got.Lifecycle == pv.ProfileLifecycleQualified {
			t.Fatalf("lifecycle %s: a released profile goes back to candidate and is qualified again deliberately", got.Lifecycle)
		}
		w.refusedAndSilent("released but not qualified")
		if _, err := w.reg.Registry.Qualify(ctxb(), sc, w.profA); err != nil {
			t.Fatalf("Qualify after release: %v", err)
		}
		healthy(t, w)
	})

	t.Run("a region the egress policy does not list is denied even for a qualified profile", func(t *testing.T) {
		// The operator accepts the new region on the profile but the data-egress policy still lists us and eu only:
		// the second gate holds on its own.
		w := newWorld(t)
		if _, err := w.reg.Registry.Observe(ctxb(), sc, w.profA, providerreg.Observation{Regions: moved}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.reg.Registry.Release(ctxb(), sc, w.profA, providerreg.ReleaseInput{ReviewedOn: "2026-10-12", Regions: moved}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.reg.Registry.Qualify(ctxb(), sc, w.profA); err != nil {
			t.Fatal(err)
		}
		w.refusedAndSilent("region not in the egress policy")
		// Once the policy chain lists it at every level, the same request goes.
		for _, lv := range []pv.EgressLevel{pv.EgressLevelDeployment, pv.EgressLevelApplication, pv.EgressLevelTenant, pv.EgressLevelWorkspace} {
			w.putPolicyWith(sc, lv, "", func(in *dataegress.PolicyInput) { in.AllowedRegions = []pv.RegionCode{"us", "eu", "ap"} })
		}
		healthy(t, w)
	})

	t.Run("an unknown residency is denied: what cannot be located cannot be approved", func(t *testing.T) {
		w := newWorld(t, withProfileEdit(func(p *pv.ProviderProfile) {
			p.Region.Residency, p.Region.ProcessingRegions = pv.ResidencyUnknown, nil
		}))
		w.refusedAndSilent("unknown residency")
	})

	t.Run("a change of the provider terms is the same kind of stop, and a review alone does not requalify", func(t *testing.T) {
		w := newWorld(t, withSecondProfile(), withLocalFallback(transportConditions...))
		h := pv.SHA256("sha256:" + strings.Repeat("c", 64))
		if _, err := w.reg.Registry.Observe(ctxb(), sc, w.profA, providerreg.Observation{TermsHash: &h}); err != nil {
			t.Fatal(err)
		}
		w.quarantinedFor(w.profA, pv.ProviderProfileQuarantineReasonTermsChanged)
		w.refusedAndSilent("terms changed")
		if _, err := w.reg.Registry.Review(ctxb(), sc, w.profA, providerreg.ReviewInput{TermsHash: h, ReviewedOn: "2026-10-12"}); err != nil {
			t.Fatalf("Review: %v", err)
		}
		if got := w.profile(w.profA); got.Lifecycle != pv.ProfileLifecycleCandidate || got.LastConformance != nil {
			t.Fatalf("lifecycle %s conformance %v: new terms reset qualification and clear the old report", got.Lifecycle, got.LastConformance)
		}
		w.refusedAndSilent("reviewed, not requalified")
	})
}
