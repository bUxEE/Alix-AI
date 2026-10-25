package settings

import (
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

// layersOf decodes documents (empty string means no document at that level).
func layersOf(t testing.TB, dep, ten, usr string) layers {
	t.Helper()
	ly := layers{versions: map[Level]int64{}}
	for level, doc := range map[Level]string{LevelDeployment: dep, LevelTenant: ten, LevelUser: usr} {
		if doc == "" {
			continue
		}
		d, err := decodeDocument(level, []byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}
		ly.versions[level] = 1
		switch level {
		case LevelDeployment:
			ly.deployment = d.policy
		case LevelTenant:
			ly.tenant = d.policy
		case LevelUser:
			ly.user = d.user
		}
	}
	return ly
}

var testLimits = Limits{
	ApprovedProfiles: []string{"lite", "balanced"}, DefaultProfile: "lite", ApprovedVoices: []string{"it_female_1"},
	MaxHistoryRetentionDays: 365, DefaultHistoryRetentionDays: 90, MaxAttachmentMB: 25,
}

func TestEffectiveDefaults(t *testing.T) {
	eff := computeEffective(testLimits, layersOf(t, "", "", ""))
	want := gen.EffectiveSettings{
		Levels:        gen.EffectiveSettingsLevels{},
		ModelProfile:  "lite",
		Voice:         gen.EffectiveSettingsVoice{SpeechRatePct: 100, AutoPlay: true, BargeIn: true},
		History:       gen.EffectiveSettingsHistory{RetentionDays: 90, AutoCleanup: true},
		Privacy:       gen.EffectiveSettingsPrivacy{StoreTranscript: true},
		Attachments:   gen.EffectiveSettingsAttachments{MaxFileMb: 25},
		Sources:       gen.EffectiveSettingsSources{IncludeAttachments: true, CitationDisplay: gen.CitationDisplayInline},
		Notifications: gen.EffectiveSettingsNotifications{Allowed: true, Enabled: true, Events: []gen.NotificationEvent{"action_ready", "job_finished", "source_indexed"}},
		Streaming:     gen.EffectiveSettingsStreaming{Enabled: true, Unit: gen.StreamUnitToken},
		Accessibility: gen.EffectiveSettingsAccessibility{TextScalePct: 100},
		Capabilities:  gen.EffectiveSettingsCapabilities{Disabled: nil},
		DecidedBy: map[string]gen.DecidedBy{
			"model_profile": gen.DecidedByOperator, "voice.enabled": gen.DecidedByDefault, "history.retention_days": gen.DecidedByDefault,
			"history.export_allowed": gen.DecidedByDefault, "privacy.store_transcript": gen.DecidedByDefault,
			"attachments.enabled": gen.DecidedByDefault, "attachments.max_file_mb": gen.DecidedByDefault, "notifications.enabled": gen.DecidedByDefault,
		},
	}
	// Disabled is an empty, not nil, list on the wire.
	eff.Capabilities.Disabled, want.Capabilities.Disabled = nil, nil
	if diff := cmp.Diff(want, eff); diff != "" {
		t.Errorf("defaults (-want +got):\n%s", diff)
	}
	// Features that need an installed component default to OFF.
	if eff.Voice.Allowed || eff.Attachments.Allowed || eff.History.ExportAllowed {
		t.Error("voice, attachments and export are on by default")
	}
}

func TestEffectiveCascade(t *testing.T) {
	type check func(t *testing.T, e gen.EffectiveSettings)
	cases := []struct {
		name          string
		dep, ten, usr string
		check         check
	}{
		{"voice is the deployment's to allow and the user's to want", `{"voice":{"allowed":true}}`, ``, `{"voice":{"enabled":true,"voice_id":"it_female_1","speech_rate_pct":150}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if !e.Voice.Allowed || !e.Voice.Enabled || e.Voice.SpeechRatePct != 150 || e.Voice.VoiceID == nil || *e.Voice.VoiceID != "it_female_1" || e.DecidedBy["voice.enabled"] != gen.DecidedByUser {
					t.Errorf("%+v %v", e.Voice, e.DecidedBy["voice.enabled"])
				}
			}},
		{"a user can not enable voice the deployment never allowed", ``, ``, `{"voice":{"enabled":true}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.Voice.Allowed || e.Voice.Enabled || e.DecidedBy["voice.enabled"] != gen.DecidedByDefault {
					t.Errorf("%+v %v", e.Voice, e.DecidedBy["voice.enabled"])
				}
			}},
		{"a tenant can not enable what the deployment did not", ``, `{"voice":{"allowed":true},"attachments":{"allowed":true},"history":{"export_allowed":true}}`, `{"voice":{"enabled":true}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.Voice.Enabled || e.Attachments.Allowed || e.History.ExportAllowed {
					t.Errorf("a tenant enabled a feature: %+v %+v %t", e.Voice, e.Attachments, e.History.ExportAllowed)
				}
			}},
		{"a tenant narrows what the deployment allowed", `{"voice":{"allowed":true},"history":{"export_allowed":true}}`, `{"voice":{"allowed":false}}`, `{"voice":{"enabled":true}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.Voice.Allowed || e.Voice.Enabled || e.DecidedBy["voice.enabled"] != gen.DecidedByTenant || !e.History.ExportAllowed {
					t.Errorf("%+v %v", e.Voice, e.DecidedBy)
				}
			}},
		{"retention is the smallest any level states", `{"history":{"retention_days":200}}`, `{"history":{"retention_days":60}}`, `{"history":{"retention_days":120}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.History.RetentionDays != 60 || e.DecidedBy["history.retention_days"] != gen.DecidedByTenant {
					t.Errorf("%d by %v", e.History.RetentionDays, e.DecidedBy["history.retention_days"])
				}
			}},
		{"a user shortens retention", `{"history":{"retention_days":200}}`, ``, `{"history":{"retention_days":7,"auto_cleanup":false}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.History.RetentionDays != 7 || e.DecidedBy["history.retention_days"] != gen.DecidedByUser || e.History.AutoCleanup {
					t.Errorf("%+v", e.History)
				}
			}},
		{"a user can not lengthen retention beyond the tenant's", ``, `{"history":{"retention_days":30}}`, `{"history":{"retention_days":300}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.History.RetentionDays != 30 {
					t.Errorf("retention = %d", e.History.RetentionDays)
				}
			}},
		{"the transcript is kept unless a level forbids or the user declines", `{"privacy":{"transcript_allowed":false}}`, ``, `{"privacy":{"store_transcript":true}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.Privacy.StoreTranscript || e.DecidedBy["privacy.store_transcript"] != gen.DecidedByDeployment {
					t.Errorf("%+v %v", e.Privacy, e.DecidedBy)
				}
			}},
		{"a user declines the transcript", ``, ``, `{"privacy":{"store_transcript":false}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.Privacy.StoreTranscript || e.DecidedBy["privacy.store_transcript"] != gen.DecidedByUser {
					t.Errorf("%+v %v", e.Privacy, e.DecidedBy)
				}
			}},
		{"attachments: allowed from the top, smallest size wins", `{"attachments":{"allowed":true,"max_file_mb":20}}`, `{"attachments":{"max_file_mb":8}}`, `{"attachments":{"max_file_mb":12,"accept_uploads":true}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if !e.Attachments.Allowed || !e.Attachments.Enabled || e.Attachments.MaxFileMb != 8 || e.DecidedBy["attachments.max_file_mb"] != gen.DecidedByTenant {
					t.Errorf("%+v %v", e.Attachments, e.DecidedBy)
				}
			}},
		{"a user switches uploads off", `{"attachments":{"allowed":true}}`, ``, `{"attachments":{"accept_uploads":false}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if !e.Attachments.Allowed || e.Attachments.Enabled || e.DecidedBy["attachments.enabled"] != gen.DecidedByUser {
					t.Errorf("%+v %v", e.Attachments, e.DecidedBy)
				}
			}},
		{"the most specific language wins", `{"default_language":"en"}`, `{"default_language":"it"}`, `{"language":"en"}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.Language == nil || *e.Language != "en" || e.DecidedBy["language"] != gen.DecidedByUser {
					t.Errorf("%v %v", e.Language, e.DecidedBy["language"])
				}
			}},
		{"a tenant default language applies to a user who stated none", `{"default_language":"en"}`, `{"default_language":"it"}`, `{}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.Language == nil || *e.Language != "it" || e.DecidedBy["language"] != gen.DecidedByTenant {
					t.Errorf("%v %v", e.Language, e.DecidedBy["language"])
				}
			}},
		{"the profile is the most specific one that is still approved", ``, `{"default_model_profile":"balanced"}`, `{"model_profile":"experimental-4g"}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				// The user's profile is no longer approved (the operator withdrew it): the tenant's applies.
				if e.ModelProfile != "balanced" || e.DecidedBy["model_profile"] != gen.DecidedByTenant {
					t.Errorf("%s %v", e.ModelProfile, e.DecidedBy["model_profile"])
				}
			}},
		{"a voice the operator no longer approves is not selected", `{"voice":{"allowed":true}}`, ``, `{"voice":{"enabled":true,"voice_id":"en_male_2"}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				// The stored voice was approved when it was written; the operator withdrew it since.
				if e.Voice.VoiceID != nil || !e.Voice.Enabled {
					t.Errorf("%+v", e.Voice)
				}
			}},
		{"no approved profile anywhere falls back to the operator's default", `{"default_model_profile":"experimental-4g"}`, ``, ``,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.ModelProfile != "lite" || e.DecidedBy["model_profile"] != gen.DecidedByOperator {
					t.Errorf("%s %v", e.ModelProfile, e.DecidedBy["model_profile"])
				}
			}},
		{"switched-off capabilities add up", `{"capabilities":{"disabled":["items.delete"]}}`, `{"capabilities":{"disabled":["items.create","items.delete"]}}`, `{"capabilities":{"disabled":["items.get"]}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				got := []string{}
				for _, id := range e.Capabilities.Disabled {
					got = append(got, string(id))
				}
				if strings.Join(got, ",") != "items.create,items.delete,items.get" {
					t.Errorf("disabled = %v", got)
				}
			}},
		{"notifications: the policy can switch them off, the user chooses events", `{"notifications":{"allowed":false}}`, ``, `{"notifications":{"enabled":true,"events":["job_finished","action_ready"]}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.Notifications.Allowed || e.Notifications.Enabled || e.DecidedBy["notifications.enabled"] != gen.DecidedByDeployment {
					t.Errorf("%+v", e.Notifications)
				}
				if got := e.Notifications.Events; len(got) != 2 || got[0] != "action_ready" || got[1] != "job_finished" {
					t.Errorf("events = %v", got)
				}
			}},
		{"streaming and accessibility are the user's", ``, ``, `{"streaming":{"enabled":false,"unit":"sentence"},"accessibility":{"reduced_motion":true,"text_scale_pct":200,"captions":true}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.Streaming.Enabled || e.Streaming.Unit != gen.StreamUnitSentence || !e.Accessibility.ReducedMotion || e.Accessibility.TextScalePct != 200 || !e.Accessibility.Captions || e.Accessibility.HighContrast {
					t.Errorf("%+v %+v", e.Streaming, e.Accessibility)
				}
			}},
		{"extra confirmation takes the lowest threshold", `{"confirmation":{"extra_from_risk_level":2}}`, `{"confirmation":{"extra_from_risk_level":1}}`, `{"confirmation":{"extra_from_risk_level":3}}`,
			func(t *testing.T, e gen.EffectiveSettings) {
				if e.Confirmation.ExtraFromRiskLevel == nil || *e.Confirmation.ExtraFromRiskLevel != 1 || e.DecidedBy["confirmation.extra_from_risk_level"] != gen.DecidedByTenant {
					t.Errorf("%+v %v", e.Confirmation, e.DecidedBy)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ly := layersOf(t, tc.dep, tc.ten, tc.usr)
			eff := computeEffective(testLimits, ly)
			tc.check(t, eff)
			// The operator's ceilings hold whatever any level says.
			if int64(eff.History.RetentionDays) > int64(testLimits.MaxHistoryRetentionDays) || int64(eff.Attachments.MaxFileMb) > int64(testLimits.MaxAttachmentMB) {
				t.Errorf("a ceiling was exceeded: %+v %+v", eff.History, eff.Attachments)
			}
			// The view validates against its own schema.
			validateEffectiveSchema(t, eff)
		})
	}
}

// A ceiling lowered after a document was stored still caps it.
func TestEffectiveCappedByTheOperator(t *testing.T) {
	l := testLimits
	l.MaxHistoryRetentionDays, l.DefaultHistoryRetentionDays, l.MaxAttachmentMB = 30, 14, 4
	eff := computeEffective(l, layersOf(t, `{"history":{"retention_days":365},"attachments":{"allowed":true,"max_file_mb":20}}`, ``, ``))
	if eff.History.RetentionDays != 30 || eff.DecidedBy["history.retention_days"] != gen.DecidedByOperator {
		t.Errorf("retention = %d by %v", eff.History.RetentionDays, eff.DecidedBy["history.retention_days"])
	}
	if eff.Attachments.MaxFileMb != 4 || eff.DecidedBy["attachments.max_file_mb"] != gen.DecidedByOperator {
		t.Errorf("size = %d by %v", eff.Attachments.MaxFileMb, eff.DecidedBy["attachments.max_file_mb"])
	}
	// With nothing stated the default applies, itself never above the ceiling.
	eff = computeEffective(l, layersOf(t, "", "", ""))
	if eff.History.RetentionDays != 14 || eff.Attachments.MaxFileMb != 4 {
		t.Errorf("defaults = %d, %d", eff.History.RetentionDays, eff.Attachments.MaxFileMb)
	}
}

// TestEffectiveInvariants runs the cascade over many random documents and checks
// the properties that make settings safe: a lower level never widens, the
// operator's ceilings hold, switched-off capabilities and extra confirmation only
// accumulate.
func TestEffectiveInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(20261002))
	pickBool := func() *bool { b := rng.Intn(2) == 0; return &b }
	maybe := func() bool { return rng.Intn(3) != 0 }
	ids := []string{"items.get", "items.create", "items.delete", "a.b", "c.d"}
	randSwitches := func() *gen.CapabilitySwitches {
		if !maybe() {
			return nil
		}
		sw := &gen.CapabilitySwitches{}
		for _, id := range ids {
			if rng.Intn(3) == 0 {
				sw.Disabled = append(sw.Disabled, common.CapabilityID(id))
			}
		}
		return sw
	}
	randRisk := func() *gen.ConfirmationRaise {
		if !maybe() {
			return nil
		}
		r := gen.RiskLevel(rng.Intn(4))
		return &gen.ConfirmationRaise{ExtraFromRiskLevel: &r}
	}
	randPolicy := func() *policyDoc {
		if !maybe() {
			return nil
		}
		p := &policyDoc{Capabilities: randSwitches(), Confirmation: randRisk()}
		if maybe() {
			p.Voice = &gen.PolicyVoice{Allowed: pickBool()}
		}
		if maybe() {
			p.Attachments = &gen.PolicyAttachments{Allowed: pickBool()}
			if maybe() {
				mb := gen.FileMegabytes(1 + rng.Intn(1024))
				p.Attachments.MaxFileMb = &mb
			}
		}
		if maybe() {
			p.History = &gen.PolicyHistory{ExportAllowed: pickBool()}
			if maybe() {
				d := gen.RetentionDays(1 + rng.Intn(3650))
				p.History.RetentionDays = &d
			}
		}
		if maybe() {
			p.Notifications = &gen.PolicyNotifications{Allowed: pickBool()}
		}
		if maybe() {
			p.Privacy = &gen.PolicyPrivacy{TranscriptAllowed: pickBool()}
		}
		return p
	}
	randUser := func() *gen.UserSettings {
		if !maybe() {
			return nil
		}
		u := &gen.UserSettings{Capabilities: randSwitches(), Confirmation: randRisk()}
		if maybe() {
			u.Voice = &gen.UserSettingsVoice{Enabled: pickBool()}
		}
		if maybe() {
			u.Attachments = &gen.UserSettingsAttachments{AcceptUploads: pickBool()}
			if maybe() {
				mb := gen.FileMegabytes(1 + rng.Intn(1024))
				u.Attachments.MaxFileMb = &mb
			}
		}
		if maybe() {
			u.History = &gen.UserSettingsHistory{}
			if maybe() {
				d := gen.RetentionDays(1 + rng.Intn(3650))
				u.History.RetentionDays = &d
			}
		}
		if maybe() {
			u.Notifications = &gen.UserSettingsNotifications{Enabled: pickBool()}
		}
		return u
	}
	for i := 0; i < 2000; i++ {
		ly := layers{deployment: randPolicy(), tenant: randPolicy(), user: randUser(), versions: map[Level]int64{}}
		eff := computeEffective(testLimits, ly)

		// A feature is on only if the deployment allowed it, no level below switched it off,
		// and (for the user-wanted ones) the user wants it.
		depAllows := func(get func(*policyDoc) *bool) bool {
			return ly.deployment != nil && get(ly.deployment) != nil && *get(ly.deployment)
		}
		tenantDenies := func(get func(*policyDoc) *bool) bool {
			return ly.tenant != nil && get(ly.tenant) != nil && !*get(ly.tenant)
		}
		voice := func(p *policyDoc) *bool {
			if p.Voice == nil {
				return nil
			}
			return p.Voice.Allowed
		}
		att := func(p *policyDoc) *bool {
			if p.Attachments == nil {
				return nil
			}
			return p.Attachments.Allowed
		}
		exp := func(p *policyDoc) *bool {
			if p.History == nil {
				return nil
			}
			return p.History.ExportAllowed
		}
		for name, c := range map[string]struct {
			got bool
			get func(*policyDoc) *bool
		}{"voice": {eff.Voice.Allowed, voice}, "attachments": {eff.Attachments.Allowed, att}, "export": {eff.History.ExportAllowed, exp}} {
			if want := depAllows(c.get) && !tenantDenies(c.get); c.got != want {
				t.Fatalf("iteration %d: %s allowed = %t, want %t", i, name, c.got, want)
			}
		}
		if eff.Voice.Enabled && !eff.Voice.Allowed || eff.Attachments.Enabled && !eff.Attachments.Allowed {
			t.Fatalf("iteration %d: a feature is enabled although it is not allowed", i)
		}
		// Ceilings hold.
		if int64(eff.History.RetentionDays) > int64(testLimits.MaxHistoryRetentionDays) || int64(eff.Attachments.MaxFileMb) > int64(testLimits.MaxAttachmentMB) {
			t.Fatalf("iteration %d: a ceiling was exceeded: %d days, %d MB", i, eff.History.RetentionDays, eff.Attachments.MaxFileMb)
		}
		// Retention and size never exceed any level's own statement.
		for _, p := range []*policyDoc{ly.deployment, ly.tenant} {
			if p != nil && p.History != nil && p.History.RetentionDays != nil && eff.History.RetentionDays > *p.History.RetentionDays {
				t.Fatalf("iteration %d: retention %d above a level's %d", i, eff.History.RetentionDays, *p.History.RetentionDays)
			}
			if p != nil && p.Attachments != nil && p.Attachments.MaxFileMb != nil && eff.Attachments.MaxFileMb > *p.Attachments.MaxFileMb {
				t.Fatalf("iteration %d: size %d above a level's %d", i, eff.Attachments.MaxFileMb, *p.Attachments.MaxFileMb)
			}
		}
		if u := ly.user; u != nil {
			if u.History != nil && u.History.RetentionDays != nil && eff.History.RetentionDays > *u.History.RetentionDays {
				t.Fatalf("iteration %d: retention above the user's own", i)
			}
		}
		// Switched-off capabilities only accumulate; extra confirmation only lowers the threshold.
		off := map[string]bool{}
		for _, id := range eff.Capabilities.Disabled {
			off[string(id)] = true
		}
		var all []*gen.CapabilitySwitches
		var raises []*gen.ConfirmationRaise
		for _, p := range []*policyDoc{ly.deployment, ly.tenant} {
			if p != nil {
				all, raises = append(all, p.Capabilities), append(raises, p.Confirmation)
			}
		}
		if ly.user != nil {
			all, raises = append(all, ly.user.Capabilities), append(raises, ly.user.Confirmation)
		}
		for _, sw := range all {
			if sw == nil {
				continue
			}
			for _, id := range sw.Disabled {
				if !off[string(id)] {
					t.Fatalf("iteration %d: %s was switched off by a level and is on", i, id)
				}
			}
		}
		if !slices.IsSorted(idStrings(eff.Capabilities.Disabled)) {
			t.Fatalf("iteration %d: disabled is not sorted", i)
		}
		for _, r := range raises {
			if r != nil && r.ExtraFromRiskLevel != nil && (eff.Confirmation.ExtraFromRiskLevel == nil || *eff.Confirmation.ExtraFromRiskLevel > *r.ExtraFromRiskLevel) {
				t.Fatalf("iteration %d: a level asked for confirmation from %d and the effective threshold is weaker", i, *r.ExtraFromRiskLevel)
			}
		}
		// The view is always an instance of its schema.
		if i%100 == 0 {
			validateEffectiveSchema(t, eff)
		}
	}
}

func idStrings(in []common.CapabilityID) []string {
	out := make([]string, len(in))
	for i, id := range in {
		out[i] = string(id)
	}
	return out
}
