package dataegress

import (
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

var allLevels = []providers.EgressLevel{
	providers.EgressLevelDeployment, providers.EgressLevelApplication, providers.EgressLevelTenant, providers.EgressLevelWorkspace,
}

func TestMostRestrictiveLevelWins(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(*PolicyInput)
		reason providers.EgressDecisionReason
		check  providers.EgressDecisionCheckName
		code   string // prefix; the level name follows
	}{
		{"disabled", func(in *PolicyInput) { in.Enabled = false }, providers.EgressDecisionReasonExternalDisabled, providers.EgressDecisionCheckNameExternalEnabled, "policy_disabled_"},
		{"no provider listed", func(in *PolicyInput) { in.AllowedProviders = nil }, providers.EgressDecisionReasonProviderNotAllowed, providers.EgressDecisionCheckNameProviderAllowlist, "provider_not_listed_"},
		{"another provider", func(in *PolicyInput) { in.AllowedProviders = []providers.ProviderID{"anthropic"} }, providers.EgressDecisionReasonProviderNotAllowed, providers.EgressDecisionCheckNameProviderAllowlist, "provider_not_listed_"},
		{"another model", func(in *PolicyInput) {
			in.AllowedModels = []providers.ModelRef{{ProviderID: tProvider, ModelID: "gpt-4.1"}}
		}, providers.EgressDecisionReasonModelNotAllowed, providers.EgressDecisionCheckNameModelAllowlist, "model_not_listed_"},
		{"the model of another provider", func(in *PolicyInput) {
			in.AllowedModels = []providers.ModelRef{{ProviderID: "anthropic", ModelID: tModel}}
		}, providers.EgressDecisionReasonModelNotAllowed, providers.EgressDecisionCheckNameModelAllowlist, "model_not_listed_"},
		{"a region missing", func(in *PolicyInput) { in.AllowedRegions = []providers.RegionCode{"us"} }, providers.EgressDecisionReasonRegionNotAllowed, providers.EgressDecisionCheckNameRegion, "region_not_listed_"},
		{"no region", func(in *PolicyInput) { in.AllowedRegions = nil }, providers.EgressDecisionReasonRegionNotAllowed, providers.EgressDecisionCheckNameRegion, "region_not_listed_"},
		{"internal data not listed", func(in *PolicyInput) {
			in.AllowedClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic}
		}, providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameClassification, "class_not_listed"},
		{"no class listed", func(in *PolicyInput) { in.AllowedClasses = nil }, providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameClassification, "class_not_listed"},
	}
	for _, lv := range allLevels {
		for _, tc := range cases {
			t.Run(string(lv)+"/"+tc.name, func(t *testing.T) {
				f := newFixture(t)
				f.enableAll()
				f.mustAllow(f.candidate()) // the control: the same request is allowed with this level at its baseline
				f.put(lv, tc.mut)          // one level, and only that level, narrows
				res, _ := f.mustDeny(f.candidate(), tc.reason, tc.check)
				want := tc.code
				if strings.HasSuffix(tc.code, "_") {
					want += string(lv)
				}
				if got := string(res.Decision.Checks[len(res.Decision.Checks)-1].Reason); got != want {
					t.Errorf("reason code = %q, want %q", got, want)
				}
				f.put(lv, nil)
				f.mustAllow(f.candidate())
			})
		}
	}

	t.Run("a wider level never widens a narrower one", func(t *testing.T) {
		f := newFixture(t)
		f.enableAll()
		both := []providers.ProviderID{tProvider, "anthropic"}
		f.put(providers.EgressLevelDeployment, func(in *PolicyInput) { in.AllowedProviders = both })
		f.put(providers.EgressLevelApplication, func(in *PolicyInput) { in.AllowedProviders = both })
		f.mustAllow(f.candidate())
		other := cloneProfile(t, f.prof)
		other.ProviderID = "anthropic"
		other.ModelID = "claude-x"
		c := f.candidate()
		c.Profile = other
		f.mustDeny(c, providers.EgressDecisionReasonProviderNotAllowed, providers.EgressDecisionCheckNameProviderAllowlist)
	})

	t.Run("source modes are a ceiling down the chain", func(t *testing.T) {
		f := newFixture(t)
		f.enableAll()
		src := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, nil)
		part := excerptPart(src, "body "+tExcerptMarker, "Title")
		mode := func(res Result) providers.EgressMode { return res.Decision.Sources[0].EffectiveMode }
		if got := mode(f.mustAllow(f.candidate(part))); got != providers.EgressModeAllowExcerpt {
			t.Fatalf("control: effective mode = %s", got)
		}
		f.put(providers.EgressLevelTenant, func(in *PolicyInput) { in.SourceMode = providers.EgressModeAllowMetadata })
		res := f.mustAllow(f.candidate(part))
		if mode(res) != providers.EgressModeAllowMetadata || res.Decision.RAGExcerptsIncluded {
			t.Errorf("a tenant ceiling of allow_metadata makes the source metadata only: %+v", res.Decision.Sources)
		}
		assertAbsent(t, "wire", whole(t, res.Envelope), tExcerptMarker)
		f.put(providers.EgressLevelWorkspace, func(in *PolicyInput) { in.SourceMode = providers.EgressModeLocalOnly })
		if got := mode(f.mustAllow(f.candidate(part))); got != providers.EgressModeLocalOnly {
			t.Errorf("effective mode = %s, want local_only", got)
		}
	})

	t.Run("a source cannot widen what its provider list excludes", func(t *testing.T) {
		f := newFixture(t)
		f.enableAll()
		src := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, func(in *PolicyInput) {
			in.AllowedProviders = []providers.ProviderID{"anthropic"} // not the provider of this request
		})
		res := f.mustAllow(f.candidate(excerptPart(src, "body "+tExcerptMarker, "Title")))
		assertAbsent(t, "envelope", whole(t, res.Envelope), tExcerptMarker)
		if s := res.Decision.Sources[0]; s.EffectiveMode != providers.EgressModeLocalOnly || res.Parts[2].Why != "source_provider_not_allowed" {
			t.Errorf("source = %+v, part = %+v", s, res.Parts[2])
		}
	})

	t.Run("mode order", func(t *testing.T) {
		order := []providers.EgressMode{providers.EgressModeDeny, providers.EgressModeLocalOnly, providers.EgressModeAllowMetadata, providers.EgressModeAllowExcerpt}
		for i, a := range order {
			for j, b := range order {
				want := order[min(i, j)]
				if got := moreRestrictiveMode(a, b); got != want {
					t.Errorf("moreRestrictiveMode(%s, %s) = %s, want %s", a, b, got, want)
				}
			}
			// A value that is not a mode is a deny.
			if got := moreRestrictiveMode(a, "bogus"); got != providers.EgressModeDeny {
				t.Errorf("moreRestrictiveMode(%s, bogus) = %s, want deny", a, got)
			}
		}
	})
}

func TestUnknownClassificationFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	f.mustAllow(f.candidate())

	for _, class := range []string{"", "secret", "PUBLIC", "internal ", "Internal", "unknown", "none", "null"} {
		p := statePart("state")
		p.Class = class
		res, _ := f.mustDeny(f.candidate(p), providers.EgressDecisionReasonUnclassifiedData, providers.EgressDecisionCheckNameClassification)
		if got := res.Decision.Checks[len(res.Decision.Checks)-1].Reason; got != "unknown_class" {
			t.Errorf("class %q: reason code %q", class, got)
		}
	}
	for _, cat := range []string{"", "mystery", "USER_MESSAGE", "credentials", "rag"} {
		p := statePart("state")
		p.Category = cat
		res, _ := f.mustDeny(f.candidate(p), providers.EgressDecisionReasonUnclassifiedData, providers.EgressDecisionCheckNameClassification)
		if got := res.Decision.Checks[len(res.Decision.Checks)-1].Reason; got != "unknown_category" {
			t.Errorf("category %q: reason code %q", cat, got)
		}
	}

	// Evidence of an allowing source whose own class is unknown is unclassified, not "kept out".
	src := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, nil)
	bad := excerptPart(src, "body", "T")
	bad.Class = "nope"
	f.mustDeny(f.candidate(bad), providers.EgressDecisionReasonUnclassifiedData, providers.EgressDecisionCheckNameClassification)
	// ... and so is a stored source policy that lost its class.
	if _, err := f.db.ExecContext(testCtx, `UPDATE dataegress_policies SET document = replace(document, '"source_class":"internal",', '') WHERE source_id = ?`, string(src)); err != nil {
		t.Fatal(err)
	}
	f.mustDeny(f.candidate(excerptPart(src, "body", "T")), providers.EgressDecisionReasonUnclassifiedData, providers.EgressDecisionCheckNameClassification)

	// A field marked local-only is removed before its class matters, which is how a local-only field of any class stays home.
	lo := statePart("home " + tLocalMarker)
	lo.Class, lo.LocalOnly = "unclassified", true
	res := f.mustAllow(f.candidate(lo))
	assertAbsent(t, "envelope", whole(t, res.Envelope), tLocalMarker)
	if !hasCategory(res.Decision.Categories.Stripped, providers.BlockedCategoryLocalOnlyField) {
		t.Errorf("stripped = %v, want local_only_field", res.Decision.Categories.Stripped)
	}

	// Confidential is never eligible outside a per-source approval, and a profile that does not allow a class blocks it.
	conf := statePart("c")
	conf.Class = string(providers.DataClassificationConfidential)
	f.mustDeny(f.candidate(conf), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameClassification)
	pub := statePart("p")
	pub.Class = string(providers.DataClassificationPublic)
	f.mustAllow(f.candidate(pub))
	g := f.candidate(pub)
	g.Profile = cloneProfile(t, f.prof)
	g.Profile.AllowedDataClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic}
	f.mustDeny(g, providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameClassification) // the baseline user message is internal
}

func TestConfidentialNeedsPerSourceApproval(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	f.prof.AllowedDataClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal, providers.EgressEligibleClassConfidential}
	body := "board minutes " + tExcerptMarker
	conf := func(src providers.SourceID) Part {
		p := excerptPart(src, body, "Minutes")
		p.Class = string(providers.DataClassificationConfidential)
		return p
	}

	// A source policy that lists confidential, approved by an authorised operator, lets excerpts through.
	approved := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationConfidential, nil)
	res := f.mustAllow(f.candidate(conf(approved)))
	assertPresent(t, "wire", wire(t, res.Envelope), tExcerptMarker)
	if !hasCategory(res.Disclosure.DataClasses, providers.EgressEligibleClassConfidential) {
		t.Errorf("the disclosure lists confidential: %v", res.Disclosure.DataClasses)
	}

	// Without CapabilityApprove nobody can write it.
	noApprove := scopeOf(tDeployment, tApp, tTenantA, tWsp1, "usr_bob", CapabilityRead, capabilitySourceWrite)
	in := basePolicy(providers.EgressLevelSource)
	cls := providers.DataClassificationConfidential
	in.SourceID, in.SourceClass = providers.SourceID("src_01J0000000000000000000000S"), &cls
	in.AllowedClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassConfidential}
	if _, err := f.svc.PutPolicy(testCtx, noApprove, in); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("writing a confidential allowance without approval: %v", err)
	}
	// Confidential cannot be listed above source level.
	up := basePolicy(providers.EgressLevelWorkspace)
	up.AllowedClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassConfidential}
	if _, err := f.svc.PutPolicy(testCtx, f.scope, up); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("confidential at workspace level: %v", err)
	}

	// A confidential source whose policy does not list the class keeps its excerpts out.
	unlisted := f.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationConfidential, func(in *PolicyInput) { in.AllowedClasses = nil })
	out := f.mustAllow(f.candidate(conf(unlisted)))
	assertAbsent(t, "envelope", whole(t, out.Envelope), tExcerptMarker)
	if out.Parts[2].Why != "source_class_not_allowed" {
		t.Errorf("part = %+v", out.Parts[2])
	}
	// A confidential source whose approval was removed (a tampered row) is not trusted.
	if _, err := f.db.ExecContext(testCtx, `UPDATE dataegress_policies SET document = json_remove(document, '$.approval') WHERE source_id = ?`, string(approved)); err != nil {
		t.Fatal(err)
	}
	out = f.mustAllow(f.candidate(conf(approved)))
	assertAbsent(t, "envelope", whole(t, out.Envelope), tExcerptMarker)
	// The profile must allow confidential data too.
	f2 := newFixture(t)
	f2.enableAll()
	src2 := f2.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationConfidential, nil)
	out = f2.mustAllow(f2.candidate(conf(src2))) // profile lists public and internal only
	assertAbsent(t, "envelope", whole(t, out.Envelope), tExcerptMarker)
	// A required confidential excerpt that may not leave denies.
	req := conf(src2)
	req.Required = true
	f2.mustDeny(f2.candidate(req), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameClassification)
}

func TestUnknownRegionDenied(t *testing.T) {
	type tc struct {
		name      string
		residency providers.Residency
		regions   []providers.RegionCode
		allowed   []providers.RegionCode
		want      regionResult
	}
	cases := []tc{
		{"unknown residency", providers.ResidencyUnknown, nil, []providers.RegionCode{"us", "eu", "global"}, regionUnknown},
		{"unknown residency with regions", providers.ResidencyUnknown, []providers.RegionCode{"us"}, []providers.RegionCode{"us"}, regionUnknown},
		{"residency that is not one", "weird", []providers.RegionCode{"us"}, []providers.RegionCode{"us"}, regionUnknown},
		{"empty residency", "", []providers.RegionCode{"us"}, []providers.RegionCode{"us"}, regionUnknown},
		{"no processing region", providers.ResidencySelectable, nil, []providers.RegionCode{"us"}, regionUnknown},
		{"pinned and listed", providers.ResidencyPinned, []providers.RegionCode{"us"}, []providers.RegionCode{"us", "eu"}, regionOK},
		{"pinned and not listed", providers.ResidencyPinned, []providers.RegionCode{"us"}, []providers.RegionCode{"eu"}, regionDenied},
		{"one of two regions missing", providers.ResidencySelectable, []providers.RegionCode{"us", "eu"}, []providers.RegionCode{"us"}, regionDenied},
		{"empty allowlist", providers.ResidencyPinned, []providers.RegionCode{"us"}, nil, regionDenied},
		{"global needs global listed", providers.ResidencyGlobal, []providers.RegionCode{"us", "eu"}, []providers.RegionCode{"us", "eu"}, regionDenied},
		{"global listed", providers.ResidencyGlobal, []providers.RegionCode{"us"}, []providers.RegionCode{"us", "global"}, regionOK},
		{"global alone is not a wildcard", providers.ResidencyPinned, []providers.RegionCode{"us"}, []providers.RegionCode{"global"}, regionDenied},
	}
	for _, c := range cases {
		got := regionVerdict(providers.ProviderProfileRegion{Residency: c.residency, ProcessingRegions: c.regions}, c.allowed)
		if got != c.want {
			t.Errorf("%s: verdict %d, want %d", c.name, got, c.want)
		}
	}

	f := newFixture(t)
	f.enableAll()
	f.mustAllow(f.candidate())
	// End to end: an unknown residency is denied whatever the policies list.
	c := f.candidate()
	c.Profile = cloneProfile(t, f.prof)
	c.Profile.Region = providers.ProviderProfileRegion{Residency: providers.ResidencyUnknown, ProcessingRegions: []providers.RegionCode{}, Compliance: []providers.ProviderProfileRegionComplianceItem{}}
	res, _ := f.mustDeny(c, providers.EgressDecisionReasonRegionUnknown, providers.EgressDecisionCheckNameRegion)
	if got := res.Decision.Checks[len(res.Decision.Checks)-1].Reason; got != "residency_unknown" {
		t.Errorf("reason code = %q", got)
	}
	d, err := f.svc.Disclose(testCtx, f.scope, c.Profile, DisclosureInput{Role: providers.RoleConversationalController})
	if err != nil {
		t.Fatal(err)
	}
	if d.ExternalEnabled || len(d.Regions) != 0 || d.Terms.Residency != providers.ResidencyUnknown {
		t.Errorf("an unknown residency is disclosed as unknown and nothing leaves: %+v", d)
	}
	// A level that omits a region the provider processes in denies, and names the level.
	f.put(providers.EgressLevelTenant, func(in *PolicyInput) { in.AllowedRegions = []providers.RegionCode{"eu"} })
	f.mustDeny(f.candidate(), providers.EgressDecisionReasonRegionNotAllowed, providers.EgressDecisionCheckNameRegion)

	// A policy cannot name an unknown or malformed region.
	for _, r := range []providers.RegionCode{"unknown", "US", "*", "us*", "", "europe", "us-"} {
		in := basePolicy(providers.EgressLevelWorkspace)
		in.AllowedRegions = []providers.RegionCode{r}
		if _, err := f.svc.PutPolicy(testCtx, f.scope, in); contracts.CodeOf(err) != contracts.ErrValidationFailed {
			t.Errorf("region %q accepted: %v", r, err)
		}
	}
	// The source level is region-checked too: a source that does not list the provider's regions stays home.
	f2 := newFixture(t)
	f2.enableAll()
	src := f2.newSource(providers.EgressModeAllowExcerpt, providers.DataClassificationInternal, func(in *PolicyInput) { in.AllowedRegions = []providers.RegionCode{"us"} })
	res2 := f2.mustAllow(f2.candidate(excerptPart(src, "body "+tExcerptMarker, "T")))
	assertAbsent(t, "envelope", whole(t, res2.Envelope), tExcerptMarker)
}

func TestProfileGates(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	f.mustAllow(f.candidate())
	deny := func(name string, mut func(*providers.ProviderProfile), reason providers.EgressDecisionReason, check providers.EgressDecisionCheckName, code string) {
		t.Helper()
		c := f.candidate()
		c.Profile = cloneProfile(t, f.prof)
		mut(&c.Profile)
		res, _ := f.mustDeny(c, reason, check)
		if got := string(res.Decision.Checks[len(res.Decision.Checks)-1].Reason); got != code {
			t.Errorf("%s: reason code %q, want %q", name, got, code)
		}
	}
	for _, lc := range []providers.ProfileLifecycle{providers.ProfileLifecycleCandidate, providers.ProfileLifecycleQuarantined, providers.ProfileLifecycleRetired, "bogus"} {
		deny("lifecycle "+string(lc), func(p *providers.ProviderProfile) { p.Lifecycle = lc },
			providers.EgressDecisionReasonProfileNotActive, providers.EgressDecisionCheckNameProfileActive, "profile_not_qualified")
	}
	deny("role", func(p *providers.ProviderProfile) { p.Roles = []providers.Role{providers.RoleEmbedding} },
		providers.EgressDecisionReasonProfileNotPermitted, providers.EgressDecisionCheckNameProfilePermitted, "role_not_permitted")
	// permitted_scopes: an empty list permits nobody; a narrower level that is omitted grants everything inside the parent.
	deny("empty grants", func(p *providers.ProviderProfile) { p.PermittedScopes = nil },
		providers.EgressDecisionReasonProfileNotPermitted, providers.EgressDecisionCheckNameProfilePermitted, "scope_not_permitted")
	deny("another application", func(p *providers.ProviderProfile) {
		p.PermittedScopes = []providers.ProviderProfileScopeGrant{{ApplicationID: tOtherApp}}
	}, providers.EgressDecisionReasonProfileNotPermitted, providers.EgressDecisionCheckNameProfilePermitted, "scope_not_permitted")
	deny("another tenant", func(p *providers.ProviderProfile) {
		tn := identifier(tTenantB)
		p.PermittedScopes = []providers.ProviderProfileScopeGrant{{ApplicationID: tApp, TenantID: &tn}}
	}, providers.EgressDecisionReasonProfileNotPermitted, providers.EgressDecisionCheckNameProfilePermitted, "scope_not_permitted")
	deny("another workspace", func(p *providers.ProviderProfile) {
		tn, ws := identifier(tTenantA), identifier(tWsp2)
		p.PermittedScopes = []providers.ProviderProfileScopeGrant{{ApplicationID: tApp, TenantID: &tn, WorkspaceID: &ws}}
	}, providers.EgressDecisionReasonProfileNotPermitted, providers.EgressDecisionCheckNameProfilePermitted, "scope_not_permitted")
	deny("a workspace without a tenant", func(p *providers.ProviderProfile) {
		ws := identifier(tWsp1)
		p.PermittedScopes = []providers.ProviderProfileScopeGrant{{ApplicationID: tApp, WorkspaceID: &ws}}
	}, providers.EgressDecisionReasonProfileNotPermitted, providers.EgressDecisionCheckNameProfilePermitted, "scope_not_permitted")

	allow := func(name string, grants []providers.ProviderProfileScopeGrant) {
		t.Helper()
		c := f.candidate()
		c.Profile = cloneProfile(t, f.prof)
		c.Profile.PermittedScopes = grants
		if _, err := f.eval(c); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	tn, ws := identifier(tTenantA), identifier(tWsp1)
	allow("a tenant grant covers its workspaces", []providers.ProviderProfileScopeGrant{{ApplicationID: tApp, TenantID: &tn}})
	allow("an exact grant", []providers.ProviderProfileScopeGrant{{ApplicationID: tApp, TenantID: &tn, WorkspaceID: &ws}})
	allow("any of several", []providers.ProviderProfileScopeGrant{{ApplicationID: tOtherApp}, {ApplicationID: tApp, TenantID: &tn}})
}
