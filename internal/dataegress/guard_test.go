package dataegress

import (
	"context"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// The positive control: every denial test below changes one thing of this baseline,
// so an always-deny guard could not pass them.
func TestBaselineIsAllowed(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	src := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, func(in *PolicyInput) {})
	res := f.mustAllow(f.candidate(excerptPart(src, "Refunds are processed within 14 days. "+tExcerptMarker, "Refund policy v2")))

	d := res.Decision
	if len(d.Levels) != 4 || len(d.Checks) != 10 {
		t.Fatalf("an allow records all four levels and all ten checks: %d levels, %d checks", len(d.Levels), len(d.Checks))
	}
	for _, c := range d.Checks {
		if !c.Passed {
			t.Errorf("check %s did not pass in an allow", c.Name)
		}
	}
	if !hasCategory(d.Categories.Sent, providers.SendCategoryUserMessage) || !hasCategory(d.Categories.Sent, providers.SendCategoryRAGExcerpt) || !d.RAGExcerptsIncluded {
		t.Errorf("sent = %v, excerpts = %v", d.Categories.Sent, d.RAGExcerptsIncluded)
	}
	assertPresent(t, "wire payload", wire(t, res.Envelope), tUserMarker, tExcerptMarker)
	if res.Envelope.Internal.PayloadBytes <= 0 || res.Envelope.Internal.EstimatedToksIn < providers.Counter(res.Envelope.Internal.PayloadBytes) {
		t.Errorf("payload size and estimate are not recorded: %+v", res.Envelope.Internal)
	}
	if len(f.decisionEvents(tApp, tTenantA)) != 1 {
		t.Errorf("an allow is recorded in the audit log before the envelope is returned")
	}
}

func TestExternalDisabledByDefault(t *testing.T) {
	f := newFixture(t)
	// No policy anywhere: external mode is off, and the denial names the level.
	res, ce := f.mustDeny(f.candidate(), providers.EgressDecisionReasonExternalDisabled, providers.EgressDecisionCheckNameExternalEnabled)
	if res.Decision.Checks[len(res.Decision.Checks)-1].Reason != "no_policy_deployment" {
		t.Errorf("reason code = %q", res.Decision.Checks[len(res.Decision.Checks)-1].Reason)
	}
	if len(res.Decision.Levels) != 4 {
		t.Fatalf("the four levels are listed: %+v", res.Decision.Levels)
	}
	for _, l := range res.Decision.Levels {
		if l.PolicyID != nil || l.Enabled || l.SourceMode != nil {
			t.Errorf("a level with no policy is disabled and sets no mode: %+v", l)
		}
	}
	if ce.Details["check"] != "external_enabled" {
		t.Errorf("details = %v", ce.Details)
	}
	// What a host would show: nothing may leave.
	d, err := f.svc.Disclose(context.Background(), f.scope, f.prof, DisclosureInput{Role: providers.RoleConversationalController})
	if err != nil {
		t.Fatal(err)
	}
	if d.ExternalEnabled || len(d.DataClasses) != 0 || len(d.DataCategories) != 0 || d.RAGExcerptsIncluded {
		t.Errorf("disclosure of a disabled deployment lists data: %+v", d)
	}
	assertValid(t, urnDisclosure, d)

	// Three levels enabled are not enough: each level with no policy is disabled.
	for _, missing := range []providers.EgressLevel{providers.EgressLevelDeployment, providers.EgressLevelApplication, providers.EgressLevelTenant, providers.EgressLevelWorkspace} {
		g := newFixture(t)
		for _, lv := range []providers.EgressLevel{providers.EgressLevelDeployment, providers.EgressLevelApplication, providers.EgressLevelTenant, providers.EgressLevelWorkspace} {
			if lv != missing {
				g.put(lv, nil)
			}
		}
		r, _ := g.mustDeny(g.candidate(), providers.EgressDecisionReasonExternalDisabled, providers.EgressDecisionCheckNameExternalEnabled)
		if want := "no_policy_" + string(missing); string(r.Decision.Checks[len(r.Decision.Checks)-1].Reason) != want {
			t.Errorf("missing %s: reason code %q, want %q", missing, r.Decision.Checks[len(r.Decision.Checks)-1].Reason, want)
		}
	}
	// A level that exists but is disabled disables everything.
	g := newFixture(t)
	g.enableAll()
	g.put(providers.EgressLevelTenant, func(in *PolicyInput) { in.Enabled = false })
	r, _ := g.mustDeny(g.candidate(), providers.EgressDecisionReasonExternalDisabled, providers.EgressDecisionCheckNameExternalEnabled)
	if r.Decision.Checks[len(r.Decision.Checks)-1].Reason != "policy_disabled_tenant" {
		t.Errorf("reason code = %q", r.Decision.Checks[len(r.Decision.Checks)-1].Reason)
	}
	// And with everything enabled the same request goes through.
	g.put(providers.EgressLevelTenant, nil)
	g.mustAllow(g.candidate())
}

func TestRestrictedNeverEgresses(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	f.mustAllow(f.candidate())

	// A restricted part fails the whole request: critical restricted-data violations block it.
	restricted := statePart("SSN " + tUserMarker)
	restricted.Class = string(providers.DataClassificationRestricted)
	res, _ := f.mustDeny(f.candidate(restricted), providers.EgressDecisionReasonRestrictedData, providers.EgressDecisionCheckNameClassification)
	if res.Envelope != nil {
		t.Fatal("restricted content reached an envelope")
	}

	// No policy can make it eligible: restricted is not a class a policy can list.
	for _, lv := range []providers.EgressLevel{providers.EgressLevelWorkspace, providers.EgressLevelSource} {
		in := basePolicy(lv)
		in.AllowedClasses = []providers.EgressEligibleClass{"restricted"}
		if lv == providers.EgressLevelSource {
			cls := providers.DataClassificationPublic
			in.SourceID, in.SourceClass = "src_01J0000000000000000000000S", &cls
		}
		if _, err := f.svc.PutPolicy(context.Background(), f.scope, in); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("%s: a policy listing restricted must be refused, got %v", lv, err)
		}
	}
	// A restricted source is local_only or deny, never an excerpt source.
	for _, mode := range []providers.EgressMode{providers.EgressModeAllowExcerpt, providers.EgressModeAllowMetadata} {
		cls := providers.DataClassificationRestricted
		in := basePolicy(providers.EgressLevelSource)
		in.SourceID, in.SourceClass, in.SourceMode = "src_01J0000000000000000000000S", &cls, mode
		in.AllowedClasses = nil
		if _, err := f.svc.PutPolicy(context.Background(), f.scope, in); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("a restricted source with %s must be refused, got %v", mode, err)
		}
	}

	// A restricted source (local_only) keeps its excerpts out; the rest of the request goes.
	rsrc := f.newSource(providers.EgressModeLocalOnly, providers.DataClassificationRestricted, nil)
	rex := excerptPart(rsrc, "restricted body "+tExcerptMarker, "Restricted file")
	rex.Class = string(providers.DataClassificationRestricted)
	ok := f.mustAllow(f.candidate(rex))
	assertAbsent(t, "envelope", whole(t, ok.Envelope), tExcerptMarker, "restricted body")

	// An excerpt of an allow_excerpt source that claims to be restricted is a violation, not a filter case.
	psrc := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, nil)
	mis := excerptPart(psrc, "body "+tExcerptMarker, "title")
	mis.Class = string(providers.DataClassificationRestricted)
	f.mustDeny(f.candidate(mis), providers.EgressDecisionReasonRestrictedData, providers.EgressDecisionCheckNameClassification)

	// ... and so is the reverse: a source classified restricted whose policy lets excerpts through (a tampered row).
	if _, err := f.db.ExecContext(context.Background(),
		`UPDATE dataegress_policies SET document = replace(document, '"source_class":"internal"', '"source_class":"restricted"') WHERE source_id = ?`, string(psrc)); err != nil {
		t.Fatal(err)
	}
	ok2 := f.candidate(excerptPart(psrc, "body "+tExcerptMarker, "title"))
	f.mustDeny(ok2, providers.EgressDecisionReasonRestrictedData, providers.EgressDecisionCheckNameClassification)

	// The class names are exact: a near miss is unknown, hence most restrictive.
	for _, s := range []string{"Restricted", " restricted", "restricted\n", "regulated"} {
		p := statePart("x")
		p.Class = s
		res, _ := f.mustDeny(f.candidate(p), providers.EgressDecisionReasonUnclassifiedData, providers.EgressDecisionCheckNameClassification)
		if res.Envelope != nil {
			t.Errorf("class %q reached an envelope", s)
		}
	}
}
