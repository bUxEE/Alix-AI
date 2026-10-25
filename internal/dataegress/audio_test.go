package dataegress

import (
	"testing"

	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// enableAudio writes the baseline at the four request levels with the audio setting max at each of them.
func (f *fixture) enableAudio(max [4]providers.EgressEligibleClass) {
	f.t.Helper()
	for i, lv := range []providers.EgressLevel{providers.EgressLevelDeployment, providers.EgressLevelApplication, providers.EgressLevelTenant, providers.EgressLevelWorkspace} {
		f.put(lv, func(in *PolicyInput) {
			in.Audio = &providers.EgressPolicyContentEgress{Enabled: true, MaxClass: max[i]}
		})
	}
}

// audioCandidate is one utterance of raw audio for an external speech recogniser, by a reference minted for the
// check, at the class its session states.
func (f *fixture) audioCandidate(class providers.DataClassification) Candidate {
	f.t.Helper()
	p := cloneProfile(f.t, f.prof)
	p.Roles = append(p.Roles, providers.RoleSTT)
	p.Modalities.Input = append(p.Modalities.Input, providers.ModalityAudio)
	p.Media = &providers.ProviderProfileMedia{Audio: &providers.ProviderProfileMediaHandling{MaxBytes: 1 << 20, MimeTypes: []string{"audio/wav"}, Retention: providers.RetentionModeZero}}
	c := f.candidate()
	c.Role, c.Profile = providers.RoleSTT, p
	c.Instructions = "Transcribe the audio."
	c.Messages = []Message{{Role: providers.RequestEnvelopeMessageRoleUser, Parts: []Part{{
		Kind: PartAudio, FileID: ids.New("fil"), SizeBytes: 32000, Category: string(providers.SendCategoryAudioContent), Class: string(class),
	}}}}
	return c
}

// Audio is a governed part: a policy that lets text leave keeps audio home until every level turns the audio setting
// on, an utterance with no class never leaves, and the decision names the audio check.
func TestEgressAudioIsGovernedApartFromText(t *testing.T) {
	f := newFixture(t)
	f.prof.AllowedDataClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal, providers.EgressEligibleClassConfidential}
	f.enableAll()

	// Text at class internal leaves; audio at class internal does not.
	f.mustAllow(f.candidate())
	res, _ := f.mustDeny(f.audioCandidate(providers.DataClassificationInternal), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameAudio)
	if got := lastCode(res); got != "audio_off_deployment" {
		t.Errorf("reason code %q, want audio_off_deployment", got)
	}
	// The files setting is not the audio setting.
	f.enableFiles(every(providers.EgressEligibleClassConfidential))
	res, _ = f.mustDeny(f.audioCandidate(providers.DataClassificationInternal), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameAudio)
	if got := lastCode(res); got != "audio_off_deployment" {
		t.Errorf("reason code %q, want audio_off_deployment", got)
	}

	// Unknown is the most restrictive class, and restricted never leaves, whatever the audio setting says.
	f.enableAudio(every(providers.EgressEligibleClassConfidential))
	f.mustDeny(f.audioCandidate(""), providers.EgressDecisionReasonUnclassifiedData, providers.EgressDecisionCheckNameClassification)
	f.mustDeny(f.audioCandidate(providers.DataClassificationRestricted), providers.EgressDecisionReasonRestrictedData, providers.EgressDecisionCheckNameClassification)

	// Up to max_class it leaves, with a decision that names the audio check after classification.
	f.enableAudio(every(providers.EgressEligibleClassInternal))
	res = f.mustAllow(f.audioCandidate(providers.DataClassificationInternal))
	if !hasCategory(res.Decision.Categories.Sent, providers.SendCategoryAudioContent) {
		t.Errorf("sent = %v, want audio_content", res.Decision.Categories.Sent)
	}
	audio := -1
	for i, c := range res.Decision.Checks {
		if c.Name == providers.EgressDecisionCheckNameAudio {
			audio = i
		}
	}
	if audio < 0 || !res.Decision.Checks[audio].Passed || res.Decision.Checks[audio-1].Name != providers.EgressDecisionCheckNameClassification {
		t.Errorf("the decision does not name the audio check: %+v", res.Decision.Checks)
	}
	res, _ = f.mustDeny(f.audioCandidate(providers.DataClassificationConfidential), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameAudio)
	if got := lastCode(res); got != "audio_class_above_max_deployment" {
		t.Errorf("reason code %q", got)
	}
	// The lowest level decides, and a level that turns it off keeps it home.
	f.enableAudio([4]providers.EgressEligibleClass{
		providers.EgressEligibleClassConfidential, providers.EgressEligibleClassConfidential, providers.EgressEligibleClassConfidential, providers.EgressEligibleClassPublic,
	})
	res, _ = f.mustDeny(f.audioCandidate(providers.DataClassificationInternal), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameAudio)
	if got := lastCode(res); got != "audio_class_above_max_workspace" {
		t.Errorf("reason code %q", got)
	}
	f.put(providers.EgressLevelTenant, func(in *PolicyInput) {
		in.Audio = &providers.EgressPolicyContentEgress{Enabled: false, MaxClass: providers.EgressEligibleClassConfidential}
	})
	res, _ = f.mustDeny(f.audioCandidate(providers.DataClassificationPublic), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameAudio)
	if got := lastCode(res); got != "audio_off_tenant" {
		t.Errorf("reason code %q", got)
	}
	// A request without audio records no audio check.
	for _, n := range checkNames(f.mustAllow(f.candidate()).Decision) {
		if n == providers.EgressDecisionCheckNameAudio {
			t.Fatal("a request without audio recorded an audio check")
		}
	}
}

// The disclosure lists raw audio only where the audio setting lets it leave, by the rule the guard applies: a profile
// that takes audio is not enough, and a policy that lets text leave does not let audio leave.
func TestDisclosureListsAudioOnlyUnderTheAudioSetting(t *testing.T) {
	f := newFixture(t)
	f.prof.AllowedDataClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic, providers.EgressEligibleClassInternal, providers.EgressEligibleClassConfidential}
	f.enableAll()
	in := DisclosureInput{Role: providers.RoleConversationalController}
	hears := f.audioCandidate(providers.DataClassificationPublic).Profile
	disclose := func(p providers.ProviderProfile) providers.EgressDisclosure {
		t.Helper()
		d, err := f.svc.Disclose(testCtx, f.scope, p, in)
		if err != nil {
			t.Fatal(err)
		}
		assertValid(t, urnDisclosure, d)
		return d
	}

	if d := disclose(hears); !d.ExternalEnabled || hasCategory(d.DataCategories, providers.SendCategoryAudioContent) {
		t.Errorf("text may leave, audio is off by default and is not disclosed: %+v", d)
	}
	f.enableAudio(every(providers.EgressEligibleClassInternal))
	f.mustAllow(f.audioCandidate(providers.DataClassificationInternal))
	if d := disclose(hears); !hasCategory(d.DataCategories, providers.SendCategoryAudioContent) || containsClass(d.DataClasses, providers.EgressEligibleClassConfidential) {
		t.Errorf("audio enabled at every level up to internal is disclosed, and no class above it: %+v", d)
	}
	// A profile that takes no audio discloses none, whatever the setting says.
	if d := disclose(f.prof); hasCategory(d.DataCategories, providers.SendCategoryAudioContent) {
		t.Errorf("a text-only profile discloses no audio: %v", d.DataCategories)
	}
	// Confidential audio, approved at every level, is disclosed with its class.
	f.enableAudio(every(providers.EgressEligibleClassConfidential))
	if d := disclose(hears); !hasCategory(d.DataCategories, providers.SendCategoryAudioContent) || !containsClass(d.DataClasses, providers.EgressEligibleClassConfidential) {
		t.Errorf("confidential audio may leave and is disclosed as such: %+v", d)
	}
	// One level that turns it off keeps audio home, and the disclosure says so.
	f.put(providers.EgressLevelTenant, func(in *PolicyInput) {
		in.Audio = &providers.EgressPolicyContentEgress{Enabled: false, MaxClass: providers.EgressEligibleClassConfidential}
	})
	f.mustDeny(f.audioCandidate(providers.DataClassificationPublic), providers.EgressDecisionReasonClassNotAllowed, providers.EgressDecisionCheckNameAudio)
	if d := disclose(hears); !d.ExternalEnabled || hasCategory(d.DataCategories, providers.SendCategoryAudioContent) || containsClass(d.DataClasses, providers.EgressEligibleClassConfidential) {
		t.Errorf("audio turned off at the tenant is not disclosed: %+v", d)
	}
}

// The audio setting is written only where it means something, and an allowance of internal or confidential audio
// needs an approver.
func TestEgressAudioSettingWrites(t *testing.T) {
	f := newFixture(t)
	ctx := testCtx
	on := func(c providers.EgressEligibleClass) *providers.EgressPolicyContentEgress {
		return &providers.EgressPolicyContentEgress{Enabled: true, MaxClass: c}
	}
	p := f.put(providers.EgressLevelWorkspace, func(in *PolicyInput) { in.Audio = on(providers.EgressEligibleClassInternal) })
	assertValid(t, urnPolicy, p)
	if p.Audio == nil || p.Audio.MaxClass != providers.EgressEligibleClassInternal || p.Files != nil {
		t.Fatalf("policy = %+v", p)
	}
	noApprove := scopeOf(tDeployment, tApp, tTenantA, tWsp1, "usr_bob", CapabilityRead, capabilityApplicationWrite)
	in := basePolicy(providers.EgressLevelApplication)
	in.AllowedClasses = []providers.EgressEligibleClass{providers.EgressEligibleClassPublic}
	in.Audio = on(providers.EgressEligibleClassInternal)
	if _, err := f.svc.PutPolicy(ctx, noApprove, in); contracts.CodeOf(err) != contracts.ErrPermissionDenied {
		t.Errorf("internal audio without an approver: %v", err)
	}
	in.Audio = on(providers.EgressEligibleClassPublic)
	if _, err := f.svc.PutPolicy(ctx, noApprove, in); err != nil {
		t.Errorf("public audio needs no approval: %v", err)
	}
	bad := basePolicy(providers.EgressLevelWorkspace)
	bad.ExpectedRevision, bad.Audio = p.Revision, on("restricted")
	if _, err := f.svc.PutPolicy(ctx, f.scope, bad); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("audio restricted: %v", err)
	}
	src := basePolicy(providers.EgressLevelSource)
	cls := providers.DataClassificationPublic
	src.SourceID, src.SourceClass, src.Audio = "src_01J0000000000000000000000S", &cls, on(providers.EgressEligibleClassPublic)
	if _, err := f.svc.PutPolicy(ctx, f.scope, src); contracts.CodeOf(err) != contracts.ErrValidationFailed {
		t.Errorf("audio at source level: %v", err)
	}
}
