package dataegress

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// filePart is an excerpt of a file a user uploaded, sent as text.
func filePart(text string, class providers.DataClassification) Part {
	return Part{Kind: PartText, Text: text, Category: string(providers.SendCategoryFileContent), Class: string(class)}
}

// enableFiles writes the baseline at the four request levels with the files setting max at each of them.
func (f *fixture) enableFiles(max [4]providers.EgressEligibleClass) {
	f.t.Helper()
	for i, lv := range []providers.EgressLevel{providers.EgressLevelDeployment, providers.EgressLevelApplication, providers.EgressLevelTenant, providers.EgressLevelWorkspace} {
		f.put(lv, func(in *PolicyInput) {
			in.Files = &providers.EgressPolicyContentEgress{Enabled: true, MaxClass: max[i]}
		})
	}
}

func every(c providers.EgressEligibleClass) [4]providers.EgressEligibleClass {
	return [4]providers.EgressEligibleClass{c, c, c, c}
}

func checkNames(d providers.EgressDecision) []providers.EgressDecisionCheckName {
	var out []providers.EgressDecisionCheckName
	for _, c := range d.Checks {
		out = append(out, c.Name)
	}
	return out
}

// publicWith replaces the baseline content with one public part and adds part in a user message, so that only the
// added part depends on internal data being allowed.
func publicWith(c Candidate, part Part) Candidate {
	pub := statePart(`{"status":"open"}`)
	pub.Class = string(providers.DataClassificationPublic)
	c.Messages = []Message{
		{Role: providers.RequestEnvelopeMessageRoleUser, Parts: []Part{pub}},
		{Role: providers.RequestEnvelopeMessageRoleUser, Parts: []Part{part}},
	}
	return c
}

func lastCode(res Result) providers.ReasonCode {
	return res.Decision.Checks[len(res.Decision.Checks)-1].Reason
}

// An excerpt of an uploaded file stays local unless every level of the chain turns the files setting on: a policy
// that lets internal text leave does not let an excerpt of that class leave, and the decision says why.
func TestEgressFileExcerptStaysLocalByDefault(t *testing.T) {
	f := newFixture(t)
	f.enableAll()
	excerpt := filePart("clause 4 of the lease "+tExcerptMarker, providers.DataClassificationInternal)

	// A request with no excerpt records no files check: its decision is what it always was.
	plain := f.mustAllow(f.candidate())
	for _, n := range checkNames(plain.Decision) {
		if n == providers.EgressDecisionCheckNameFiles {
			t.Fatalf("a request without an excerpt recorded a files check: %v", checkNames(plain.Decision))
		}
	}

	res, _ := f.mustDeny(f.candidate(excerpt), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameFiles)
	if got := lastCode(res); got != "files_off_deployment" {
		t.Errorf("reason code %q, want files_off_deployment", got)
	}
	// One level that does not enable it is enough to keep it home, and so is a level that turns it off.
	f.enableFiles(every(providers.EgressEligibleClassInternal))
	f.put(providers.EgressLevelWorkspace, nil)
	res, _ = f.mustDeny(f.candidate(excerpt), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameFiles)
	if got := lastCode(res); got != "files_off_workspace" {
		t.Errorf("reason code %q, want files_off_workspace", got)
	}
	f.put(providers.EgressLevelTenant, func(in *PolicyInput) {
		in.Files = &providers.EgressPolicyContentEgress{Enabled: false, MaxClass: providers.EgressEligibleClassInternal}
	})
	res, _ = f.mustDeny(f.candidate(excerpt), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameFiles)
	if got := lastCode(res); got != "files_off_tenant" {
		t.Errorf("reason code %q, want files_off_tenant", got)
	}
}

// With the files setting on, an excerpt leaves up to the lowest max_class along the chain, to the providers the
// chain already allows, and the decision names the files check.
func TestEgressFileExcerptLeavesUpToMaxClass(t *testing.T) {
	f := newFixture(t)
	f.prof.AllowedDataClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal, providers.EgressEligibleClassConfidential}
	f.enableFiles(every(providers.EgressEligibleClassInternal))
	body := "clause 4 of the lease " + tExcerptMarker

	res := f.mustAllow(f.candidate(filePart(body, providers.DataClassificationInternal)))
	assertPresent(t, "wire", wire(t, res.Envelope), tExcerptMarker)
	if !hasCategory(res.Decision.Categories.Sent, providers.SendCategoryFileContent) {
		t.Errorf("sent = %v, want file_content", res.Decision.Categories.Sent)
	}
	files := -1
	for i, c := range res.Decision.Checks {
		if c.Name == providers.EgressDecisionCheckNameFiles {
			files = i
		}
	}
	if files < 0 || !res.Decision.Checks[files].Passed || res.Decision.Checks[files].Reason != "passed" ||
		res.Decision.Checks[files-1].Name != providers.EgressDecisionCheckNameClassification {
		t.Errorf("the decision does not name the files check after classification: %+v", res.Decision.Checks)
	}
	if last := res.Parts[len(res.Parts)-1]; !last.Kept || last.Why != "" {
		t.Errorf("the excerpt was not kept as it is: %+v", last)
	}
	f.mustAllow(f.candidate(filePart(body, providers.DataClassificationPublic)))

	// Above max_class it is refused.
	res, _ = f.mustDeny(f.candidate(filePart(body, providers.DataClassificationConfidential)), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameFiles)
	if got := lastCode(res); got != "files_class_above_max_deployment" {
		t.Errorf("reason code %q", got)
	}

	// Confidential leaves only where every level sets max_class confidential (with an approval), and the
	// disclosure says so; the lowest level decides.
	f.enableFiles(every(providers.EgressEligibleClassConfidential))
	res = f.mustAllow(f.candidate(filePart(body, providers.DataClassificationConfidential)))
	if !hasCategory(res.Disclosure.DataClasses, providers.EgressEligibleClassConfidential) {
		t.Errorf("the disclosure does not list confidential: %v", res.Disclosure.DataClasses)
	}
	f.enableFiles([4]providers.EgressEligibleClass{
		providers.EgressEligibleClassConfidential, providers.EgressEligibleClassConfidential, providers.EgressEligibleClassConfidential, providers.EgressEligibleClassPublic,
	})
	res, _ = f.mustDeny(f.candidate(filePart(body, providers.DataClassificationInternal)), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameFiles)
	if got := lastCode(res); got != "files_class_above_max_workspace" {
		t.Errorf("reason code %q", got)
	}

	// The files setting widens nothing else: public and internal still need allowed_classes, the profile must
	// allow the class, and the provider must be allowed.
	f.enableFiles(every(providers.EgressEligibleClassConfidential))
	f.put(providers.EgressLevelWorkspace, func(in *PolicyInput) {
		in.AllowedClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic}
		in.Files = &providers.EgressPolicyContentEgress{Enabled: true, MaxClass: providers.EgressEligibleClassConfidential}
	})
	res, _ = f.mustDeny(publicWith(f.candidate(), filePart(body, providers.DataClassificationInternal)), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameFiles)
	if got := lastCode(res); got != "class_not_listed_workspace" {
		t.Errorf("reason code %q", got)
	}
	f.enableFiles(every(providers.EgressEligibleClassConfidential))
	g := f.candidate(filePart(body, providers.DataClassificationConfidential))
	g.Profile = cloneProfile(t, f.prof)
	g.Profile.AllowedDataClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal}
	res, _ = f.mustDeny(g, providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameFiles)
	if got := lastCode(res); got != "class_not_in_profile" {
		t.Errorf("reason code %q", got)
	}
	f.put(providers.EgressLevelTenant, func(in *PolicyInput) {
		in.AllowedProviders = []providers.ProviderID{"anthropic"}
		in.Files = &providers.EgressPolicyContentEgress{Enabled: true, MaxClass: providers.EgressEligibleClassConfidential}
	})
	f.mustDeny(f.candidate(filePart(body, providers.DataClassificationConfidential)), providers.EgressDecisionReasonProviderNotAllowed, providers.EgressDecisionCheckNameProviderAllowlist)

	// A level whose approval was removed (a tampered row) does not let internal or confidential excerpts out.
	f.enableFiles(every(providers.EgressEligibleClassConfidential))
	if _, err := f.db.ExecContext(testCtx, `UPDATE dataegress_policies SET document = json_remove(document, '$.approval') WHERE level = 'tenant'`); err != nil {
		t.Fatal(err)
	}
	res, _ = f.mustDeny(publicWith(f.candidate(), filePart(body, providers.DataClassificationConfidential)), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameFiles)
	if got := lastCode(res); got != "files_not_approved_tenant" {
		t.Errorf("reason code %q", got)
	}
}

// The disclosure lists file content for an excerpt of an uploaded file only where the files setting lets it leave, by
// the rule the guard applies. An excerpt is text, so this holds for a profile that takes no file media too.
func TestDisclosureListsFileExcerptsOnlyUnderTheFilesSetting(t *testing.T) {
	f := newFixture(t)
	f.prof.AllowedDataClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal, providers.EgressEligibleClassConfidential}
	f.enableAll()
	in := DisclosureInput{Role: providers.RoleConversationalController}
	disclose := func() providers.EgressDisclosure {
		t.Helper()
		d, err := f.svc.Disclose(testCtx, f.scope, f.prof, in)
		if err != nil {
			t.Fatal(err)
		}
		assertValid(t, urnDisclosure, d)
		return d
	}
	body := "clause 4 of the lease " + tExcerptMarker

	if d := disclose(); !d.ExternalEnabled || hasCategory(d.DataCategories, providers.SendCategoryFileContent) {
		t.Errorf("text may leave, excerpts are off by default and are not disclosed: %+v", d)
	}
	f.enableFiles(every(providers.EgressEligibleClassConfidential))
	f.mustAllow(f.candidate(filePart(body, providers.DataClassificationConfidential)))
	if d := disclose(); !hasCategory(d.DataCategories, providers.SendCategoryFileContent) || !containsClass(d.DataClasses, providers.EgressEligibleClassConfidential) {
		t.Errorf("confidential excerpts may leave and are disclosed as such: %+v", d)
	}
	f.put(providers.EgressLevelWorkspace, func(in *PolicyInput) {
		in.Files = &providers.EgressPolicyContentEgress{Enabled: false, MaxClass: providers.EgressEligibleClassConfidential}
	})
	f.mustDeny(f.candidate(filePart(body, providers.DataClassificationPublic)), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameFiles)
	if d := disclose(); !d.ExternalEnabled || hasCategory(d.DataCategories, providers.SendCategoryFileContent) || containsClass(d.DataClasses, providers.EgressEligibleClassConfidential) {
		t.Errorf("excerpts turned off at the workspace are not disclosed: %+v", d)
	}
}

// Restricted, unclassified and credential content in an excerpt never leaves, whatever the files setting says.
func TestEgressRestrictedOrCredentialExcerptNeverLeaves(t *testing.T) {
	f := newFixture(t)
	f.prof.AllowedDataClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal, providers.EgressEligibleClassConfidential}
	f.enableFiles(every(providers.EgressEligibleClassConfidential))
	secret := tSecret()

	f.mustDeny(f.candidate(filePart("payroll "+tExcerptMarker, providers.DataClassificationRestricted)), providers.EgressDecisionReasonRestrictedData, providers.EgressDecisionCheckNameClassification)
	f.mustDeny(f.candidate(filePart("payroll "+tExcerptMarker, "")), providers.EgressDecisionReasonUnclassifiedData, providers.EgressDecisionCheckNameClassification)

	// An excerpt the caller declares a credential is removed whole.
	cred := filePart("api credential "+secret, providers.DataClassificationConfidential)
	cred.Category = string(providers.BlockedCategoryCredential)
	res := f.mustAllow(f.candidate(cred))
	assertAbsent(t, "envelope", whole(t, res.Envelope), secret, "api credential")
	if !hasCategory(res.Decision.Categories.Stripped, providers.BlockedCategoryCredential) {
		t.Errorf("stripped = %v", res.Decision.Categories.Stripped)
	}
	// A secret inside an excerpt is scrubbed, and the part says so.
	res = f.mustAllow(f.candidate(filePart("the key is "+secret+" "+tExcerptMarker, providers.DataClassificationConfidential)))
	assertAbsent(t, "envelope", whole(t, res.Envelope), secret)
	assertPresent(t, "wire", wire(t, res.Envelope), tExcerptMarker)
	if last := res.Parts[len(res.Parts)-1]; !last.Kept || last.Why != "scrubbed" || !hasCategory(res.Decision.Categories.Stripped, providers.BlockedCategoryCredential) {
		t.Errorf("part %+v, stripped %v", last, res.Decision.Categories.Stripped)
	}
}

// The files setting is written only where it means something, an allowance of internal or confidential needs an
// approver, and a policy without it is stored exactly as before.
func TestEgressFilesSettingWrites(t *testing.T) {
	f := newFixture(t)
	ctx := testCtx
	on := func(c providers.EgressEligibleClass) *providers.EgressPolicyContentEgress {
		return &providers.EgressPolicyContentEgress{Enabled: true, MaxClass: c}
	}

	p := f.put(providers.EgressLevelWorkspace, func(in *PolicyInput) { in.Files = on(providers.EgressEligibleClassConfidential) })
	assertValid(t, urnPolicy, p)
	if p.Files == nil || !p.Files.Enabled || p.Files.MaxClass != providers.EgressEligibleClassConfidential || p.Approval == nil {
		t.Fatalf("policy = %+v", p)
	}
	got, found, err := f.svc.GetPolicy(ctx, f.scope, providers.EgressLevelWorkspace, "")
	if err != nil || !found || got.Files == nil || *got.Files != *p.Files {
		t.Fatalf("read back %+v %v %v", got.Files, found, err)
	}

	// No files setting: the stored document has no files member at all.
	plain := f.put(providers.EgressLevelTenant, nil)
	raw, _ := json.Marshal(plain)
	if plain.Files != nil || strings.Contains(string(raw), `"files"`) {
		t.Errorf("a policy without the setting: %s", raw)
	}

	// Internal and confidential need an approver; public, or a setting that is off, does not.
	noApprove := scopeOf(tDeployment, tApp, tTenantA, tWsp1, "usr_bob", CapabilityRead, capabilityApplicationWrite)
	pub := basePolicy(providers.EgressLevelApplication)
	pub.AllowedClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic}
	for _, c := range []providers.EgressEligibleClass{providers.EgressEligibleClassInternal, providers.EgressEligibleClassConfidential} {
		in := pub
		in.Files = on(c)
		if _, err := f.svc.PutPolicy(ctx, noApprove, in); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
			t.Errorf("files %s without an approver: %v", c, err)
		}
	}
	in := pub
	in.Files = &providers.EgressPolicyContentEgress{Enabled: false, MaxClass: providers.EgressEligibleClassConfidential}
	if w, err := f.svc.PutPolicy(ctx, noApprove, in); err != nil || w.Approval != nil {
		t.Errorf("an off setting needs no approval: %+v %v", w.Approval, err)
	}
	in = pub
	in.ExpectedRevision, in.Files = 1, on(providers.EgressEligibleClassPublic)
	if _, err := f.svc.PutPolicy(ctx, noApprove, in); err != nil {
		t.Errorf("public files need no approval: %v", err)
	}

	// Restricted is not a class it can name, and a source policy has no files setting (uploads have no source).
	bad := basePolicy(providers.EgressLevelWorkspace)
	bad.ExpectedRevision, bad.Files = p.Revision, on("restricted")
	if _, err := f.svc.PutPolicy(ctx, f.scope, bad); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("files restricted: %v", err)
	}
	src := basePolicy(providers.EgressLevelSource)
	cls := providers.DataClassificationPublic
	src.SourceID, src.SourceClass, src.Files = "src_01J0000000000000000000000S", &cls, on(providers.EgressEligibleClassPublic)
	if _, err := f.svc.PutPolicy(ctx, f.scope, src); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("files at source level: %v", err)
	}
}
