package integrationaudit

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/internal/apppkg"
	"github.com/buxee/alix-ai/internal/policy"
	"github.com/buxee/alix-ai/pkg/contracts"
	ap "github.com/buxee/alix-ai/pkg/contracts/gen/applicationpackage"
)

// loaded returns a fresh typed copy of valid-minimal whose exported maps a test
// may change, to feed the checks a package the loader would have refused: the
// audit recomputes what it reports from the typed package, so a weakened loader
// cannot let a gap through.
func loaded(t *testing.T) *apppkg.Package {
	t.Helper()
	h := newHarness(t)
	rec := h.install(validMinimalDir)
	pkg, _, err := h.inst.Open(ctx, appID, rec.ManifestHash)
	if err != nil {
		t.Fatal(err)
	}
	return pkg
}

func runCheck(t *testing.T, c func(context.Context, *Input) (*Outcome, error), in *Input) *Outcome {
	t.Helper()
	o, err := c(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestDocumentationCheckRecomputesCoverage(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		o := runCheck(t, runDocumentation, &Input{Package: loaded(t)})
		if len(o.Findings) != 0 || len(o.Coverage) != 1 || o.Coverage[0].Covered != o.Coverage[0].Total || o.Coverage[0].Total == 0 {
			t.Errorf("valid-minimal is fully documented: %+v %+v", o.Findings, o.Coverage)
		}
	})
	t.Run("a facet missing in the default language", func(t *testing.T) {
		pkg := loaded(t)
		var kept []ap.DocumentationIndexEntry
		for _, e := range pkg.Documentation.Entries {
			if e.CapabilityID == "reservations.create" && e.Facet == ap.DocumentationIndexEntryFacetRiskLevel {
				continue
			}
			kept = append(kept, e)
		}
		pkg.Documentation.Entries = kept
		o := runCheck(t, runDocumentation, &Input{Package: pkg})
		if len(o.Findings) != 1 {
			t.Fatalf("want one finding, got %+v", o.Findings)
		}
		f := o.Findings[0]
		if f.CheckID != "docs.incomplete" || f.Severity != contracts.SeverityCritical || f.Capability != "reservations.create" || !strings.Contains(f.Message, "risk level") {
			t.Errorf("finding = %+v", f)
		}
		if o.Coverage[0].Covered != o.Coverage[0].Total-1 {
			t.Errorf("coverage = %+v, want one short", o.Coverage[0])
		}
		if statusOf(o) != StatusFail {
			t.Errorf("a documentation gap fails the check")
		}
	})
	t.Run("documentation only in another language does not count", func(t *testing.T) {
		pkg := loaded(t)
		for i := range pkg.Documentation.Entries {
			if pkg.Documentation.Entries[i].Language == pkg.Manifest.Languages.Default {
				pkg.Documentation.Entries[i].Language = "it"
			}
		}
		o := runCheck(t, runDocumentation, &Input{Package: pkg})
		if len(o.Findings) != 2 {
			t.Fatalf("both capabilities lose their default-language documentation: %+v", o.Findings)
		}
	})
	t.Run("a failure state without documentation", func(t *testing.T) {
		pkg := loaded(t)
		var kept []ap.DocumentationIndexEntry
		for _, e := range pkg.Documentation.Entries {
			if e.Facet == ap.DocumentationIndexEntryFacetFailureState && e.ErrorCode != nil && string(*e.ErrorCode) == "stale" {
				continue
			}
			kept = append(kept, e)
		}
		pkg.Documentation.Entries = kept
		o := runCheck(t, runDocumentation, &Input{Package: pkg})
		if len(o.Findings) != 1 || !strings.Contains(o.Findings[0].Message, "failure state stale") {
			t.Errorf("findings = %+v", o.Findings)
		}
	})
	t.Run("no package", func(t *testing.T) {
		o := runCheck(t, runDocumentation, &Input{})
		if statusOf(o) != StatusUnverified {
			t.Errorf("an unloadable package leaves documentation unverified, got %s", statusOf(o))
		}
	})
	t.Run("loader findings of the section are passed through", func(t *testing.T) {
		in := &Input{LoadFindings: []contracts.Finding{
			{CheckID: "docs.location_missing", Severity: contracts.SeverityCritical, Message: "m", Remediation: "r"},
			{CheckID: "contract.missing", Severity: contracts.SeverityCritical, Message: "m", Remediation: "r"},
		}}
		o := runCheck(t, runDocumentation, in)
		if len(o.Findings) != 1 || o.Findings[0].CheckID != "docs.location_missing" {
			t.Errorf("only the documentation findings belong to check 4: %+v", o.Findings)
		}
	})
}

func TestContractsCheckRecomputesCoverage(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		o := runCheck(t, runContracts, &Input{Package: loaded(t)})
		if len(o.Findings) != 0 || o.Coverage[0].Total != 4 || o.Coverage[0].Covered != 4 {
			t.Errorf("two user capabilities in two languages are covered: %+v %+v", o.Findings, o.Coverage)
		}
	})
	t.Run("a language missing", func(t *testing.T) {
		pkg := loaded(t)
		delete(pkg.Contracts, apppkg.ContractKey{Capability: "reservations.create", Language: "it"})
		o := runCheck(t, runContracts, &Input{Package: pkg})
		if len(o.Findings) != 1 {
			t.Fatalf("findings = %+v", o.Findings)
		}
		f := o.Findings[0]
		if f.CheckID != "contracts.incomplete" || f.Severity != contracts.SeverityCritical || f.Capability != "reservations.create" || !strings.Contains(f.Message, `"it"`) {
			t.Errorf("finding = %+v", f)
		}
		if o.Coverage[0].Covered != 3 {
			t.Errorf("coverage = %+v", o.Coverage[0])
		}
	})
	t.Run("a contract without a failure response or confirmation wording", func(t *testing.T) {
		pkg := loaded(t)
		key := apppkg.ContractKey{Capability: "reservations.create", Language: "en"}
		ct := pkg.Contracts[key]
		ct.Failures = ct.Failures[1:]
		ct.Confirmations = nil
		ct.Success = nil
		pkg.Contracts[key] = ct
		o := runCheck(t, runContracts, &Input{Package: pkg})
		if len(o.Findings) != 1 {
			t.Fatalf("findings = %+v", o.Findings)
		}
		m := o.Findings[0].Message
		for _, want := range []string{"a response for failure", "the confirmation wording", "a success response"} {
			if !strings.Contains(m, want) {
				t.Errorf("the finding must say %q: %s", want, m)
			}
		}
	})
	t.Run("an admin capability needs no contract", func(t *testing.T) {
		pkg := loaded(t)
		c := pkg.Capabilities["catalog.find_title"]
		c.Audience = ap.CapabilityAudienceAdmin
		pkg.Capabilities["catalog.find_title"] = c
		delete(pkg.Contracts, apppkg.ContractKey{Capability: "catalog.find_title", Language: "en"})
		delete(pkg.Contracts, apppkg.ContractKey{Capability: "catalog.find_title", Language: "it"})
		o := runCheck(t, runContracts, &Input{Package: pkg})
		if len(o.Findings) != 0 {
			t.Errorf("findings = %+v", o.Findings)
		}
	})
}

func TestRAGInventoryChecksDeclaredFields(t *testing.T) {
	in := func(pkg *apppkg.Package) *Input {
		return &Input{Package: pkg, Registered: []CheckInfo{{ID: "package.rag_inventory", Section: SectionRAGInventory}}}
	}
	t.Run("complete", func(t *testing.T) {
		o := runCheck(t, runRAGInventory, in(loaded(t)))
		if len(o.Findings) != 0 || len(o.Sources) != 1 {
			t.Fatalf("findings = %+v sources = %+v", o.Findings, o.Sources)
		}
		s := o.Sources[0]
		if s.Owner != "Library Operations" || s.Scope != "application=demo-library tenant=shared workspace=shared" || len(s.Missing) != 0 || len(s.Declared) != 8 {
			t.Errorf("source = %+v", s)
		}
		if len(o.Unverified) != 1 || o.Unverified[0].Subject != "rag_sources:live" {
			t.Errorf("with no live check, the live half is unverified: %+v", o.Unverified)
		}
		if statusOf(o) != StatusUnverified {
			t.Errorf("a source never reached live is never pass")
		}
	})
	t.Run("a live check owns the live half", func(t *testing.T) {
		i := in(loaded(t))
		i.Registered = append(i.Registered, CheckInfo{ID: "rag.live", Section: SectionRAGInventory})
		o := runCheck(t, runRAGInventory, i)
		if len(o.Unverified) != 0 {
			t.Errorf("unverified = %+v", o.Unverified)
		}
	})
	t.Run("missing fields", func(t *testing.T) {
		pkg := loaded(t)
		s := pkg.Sources["loan-policies"]
		s.Owner.Name = " "
		s.Include = nil
		s.Citation = ap.RAGSourceCitation{}
		s.Sync.Method = ""
		s.Scope.Tenant.Mode = ""
		pkg.Sources["loan-policies"] = s
		o := runCheck(t, runRAGInventory, in(pkg))
		if len(o.Findings) != 1 || o.Findings[0].CheckID != "rag.field_missing" || o.Findings[0].Severity != contracts.SeverityCritical {
			t.Fatalf("findings = %+v", o.Findings)
		}
		got := o.Sources[0].Missing
		if diff := cmp.Diff([]string{"owner", "scope", "include", "refresh", "citation"}, got); diff != "" {
			t.Errorf("missing (-want +got):\n%s", diff)
		}
		if statusOf(o) != StatusFail {
			t.Errorf("an undeclared field fails check 3")
		}
	})
	t.Run("an API source without a credential reference", func(t *testing.T) {
		pkg := loaded(t)
		s := pkg.Sources["loan-policies"]
		ep := ap.Slug("library-mcp")
		s.Root = ap.RAGSourceRoot{Value: ap.RAGSourceRootAPI{Endpoint: &ep}}
		s.CredentialRef = nil
		pkg.Sources["loan-policies"] = s
		o := runCheck(t, runRAGInventory, in(pkg))
		if len(o.Findings) != 1 || o.Findings[0].CheckID != "rag.api_source_without_credential" || o.Findings[0].Severity != contracts.SeverityWarning {
			t.Errorf("findings = %+v", o.Findings)
		}
	})
}

func TestPolicyCheckReadsThePermissionModel(t *testing.T) {
	t.Run("a faithful package", func(t *testing.T) {
		o := runCheck(t, runPolicy, &Input{Package: loaded(t)})
		if len(o.Findings) != 0 {
			t.Errorf("findings = %+v", o.Findings)
		}
	})
	t.Run("a capability nobody can use, an admin capability granted, a role with nothing", func(t *testing.T) {
		pkg := loaded(t)
		for _, hp := range pkg.Policies {
			var grants []policy.RoleGrant
			for _, g := range hp.RoleGrants {
				var caps []string
				for _, c := range g.Capabilities {
					if c != "reservations.create" {
						caps = append(caps, c)
					}
				}
				if g.Role == "visitor" {
					caps = nil
				}
				if len(caps) > 0 {
					grants = append(grants, policy.RoleGrant{Role: g.Role, Capabilities: caps})
				}
			}
			hp.RoleGrants = grants
		}
		o := runCheck(t, runPolicy, &Input{Package: pkg})
		ids := map[string]contracts.Severity{}
		for _, f := range o.Findings {
			ids[f.CheckID] = f.Severity
		}
		want := map[string]contracts.Severity{
			"permissions.capability_ungranted": contracts.SeverityWarning,
			"permissions.role_without_grants":  contracts.SeverityInfo,
		}
		if diff := cmp.Diff(want, ids); diff != "" {
			t.Errorf("findings (-want +got):\n%s", diff)
		}
		if len(o.Permissions.Missing) != 2 {
			t.Errorf("missing = %v", o.Permissions.Missing)
		}

		// grant an admin capability to a role
		c := pkg.Capabilities["catalog.find_title"]
		c.Audience = ap.CapabilityAudienceAdmin
		pkg.Capabilities["catalog.find_title"] = c
		o = runCheck(t, runPolicy, &Input{Package: pkg})
		if len(o.Permissions.Excessive) == 0 {
			t.Errorf("an admin capability granted to a role is excessive: %+v", o)
		}
	})
	t.Run("the loader's contradictions are listed", func(t *testing.T) {
		in := &Input{LoadFindings: []contracts.Finding{
			{CheckID: "policy.rule_hides_required_field", Severity: contracts.SeverityWarning, Message: "a rule hides a required field", Remediation: "r"},
		}}
		o := runCheck(t, runPolicy, in)
		if len(o.Findings) != 1 || len(o.Permissions.Contradictory) != 1 {
			t.Errorf("findings %+v contradictory %v", o.Findings, o.Permissions.Contradictory)
		}
	})
}

func TestExternalInventoryListsWhatCouldLeave(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		ext, fs := externalInventory(manifestDecl{}, map[string]sourceDecl{"a": {mode: "local_only", class: "internal"}}, []fieldDecl{{"c", "f", "internal", false}})
		if ext.DeclaredByPackage || ext.ForbidExternal || len(ext.Sources) != 0 || len(ext.Fields) != 0 {
			t.Errorf("ext = %+v", ext)
		}
		if len(fs) != 1 || fs[0].CheckID != "external.defaults_assumed" || fs[0].Severity != contracts.SeverityInfo {
			t.Errorf("findings = %+v", fs)
		}
	})
	t.Run("declarations", func(t *testing.T) {
		sources := map[string]sourceDecl{
			"allowed": {"allow_excerpt", "confidential", true, true},
			"denied":  {"deny", "internal", true, false},
			"meta":    {"allow_metadata", "internal", true, false},
			"local":   {"local_only", "restricted", true, true},
			"weird":   {"anything_goes", "internal", true, false},
		}
		fields := []fieldDecl{
			{"b.cap", "ssn", "restricted", true},
			{"b.cap", "name", "internal", true},
			{"a.cap", "notes", "confidential", true},
			{"a.cap", "city", "public", true},
			{"a.cap", "odd", "top-secret", true}, // unknown: the most restrictive
			{"a.cap", "plain", "internal", false},
		}
		ext, fs := externalInventory(manifestDecl{declared: true, forbid: true}, sources, fields)
		if !ext.DeclaredByPackage || !ext.ForbidExternal {
			t.Errorf("ext = %+v", ext)
		}
		var gotSources []string
		for _, s := range ext.Sources {
			gotSources = append(gotSources, s.ID+"="+s.Egress+"/"+s.Classification)
		}
		wantSources := []string{"allowed=allow_excerpt/confidential", "denied=deny/internal", "meta=allow_metadata/internal", "weird=anything_goes/internal"}
		if diff := cmp.Diff(wantSources, gotSources); diff != "" {
			t.Errorf("sources above local_only (-want +got):\n%s", diff)
		}
		var gotFields []string
		for _, f := range ext.Fields {
			gotFields = append(gotFields, f.Capability+"."+f.Field+"="+f.Classification)
		}
		wantFields := []string{"a.cap.notes=confidential", "a.cap.odd=top-secret", "b.cap.ssn=restricted"}
		if diff := cmp.Diff(wantFields, gotFields); diff != "" {
			t.Errorf("fields above internal (-want +got):\n%s", diff)
		}
		counts := map[string]int{}
		for _, f := range fs {
			counts[f.CheckID]++
			if f.Severity != contracts.SeverityInfo {
				t.Errorf("the inventory is informational: %+v", f)
			}
		}
		if counts["external.source_not_local_only"] != 4 || counts["external.field_classified"] != 3 || counts["external.forbid"] != 1 || counts["external.defaults_assumed"] != 0 {
			t.Errorf("counts = %v", counts)
		}
	})
}

// valid-minimal declares nothing about external mode: the report says the safe
// defaults were assumed and lists nothing as able to leave.
func TestExternalInventoryOnAPackageThatDeclaresNothing(t *testing.T) {
	h := newHarness(t)
	h.install(validMinimalDir)
	rep := h.audit(newStub(t)).Signed.Report
	if rep.External.DeclaredByPackage || len(rep.External.Sources) != 0 || len(rep.External.Fields) != 0 {
		t.Errorf("external = %+v", rep.External)
	}
	f := findings(rep, "external.defaults_assumed")
	if len(f) != 1 || f[0].Severity != contracts.SeverityInfo {
		t.Errorf("findings = %+v", f)
	}
	if rep.Summary.Outcome != DispositionClear {
		t.Errorf("an informational finding never blocks or needs acceptance: %s", rep.Summary.Outcome)
	}
}

// A package that declares egress modes and classifications is read through the
// typed members (X-03): the report lists what could leave and does not say the
// defaults were assumed.
func TestExternalInventoryReadsTheTypedDeclarations(t *testing.T) {
	cases := []struct {
		dir         string
		wantSources []string
		wantFields  []string
	}{
		{"../../examples/packages/egress-allow-excerpt", []string{"loan-policies=allow_excerpt/public"}, nil},
		{"../../examples/packages/egress-local-only", nil, []string{"catalog.find_title.max_results=confidential"}},
	}
	for _, c := range cases {
		t.Run(filepath.Base(c.dir), func(t *testing.T) {
			h := newHarness(t)
			h.install(c.dir)
			rep := h.audit(newStub(t)).Signed.Report
			if !rep.External.DeclaredByPackage {
				t.Errorf("external = %+v", rep.External)
			}
			if f := findings(rep, "external.defaults_assumed"); len(f) != 0 {
				t.Errorf("defaults_assumed although the package declares: %+v", f)
			}
			var gotSources, gotFields []string
			for _, s := range rep.External.Sources {
				gotSources = append(gotSources, s.ID+"="+s.Egress+"/"+s.Classification)
			}
			for _, f := range rep.External.Fields {
				gotFields = append(gotFields, f.Capability+"."+f.Field+"="+f.Classification)
			}
			if diff := cmp.Diff(c.wantSources, gotSources); diff != "" {
				t.Errorf("sources (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(c.wantFields, gotFields); diff != "" {
				t.Errorf("fields (-want +got):\n%s", diff)
			}
		})
	}
}
