package settings

import (
	"slices"
	"sort"

	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	gen "github.com/buxee/alix-ai/pkg/contracts/gen/settings"
)

// layers are the documents that exist for one caller. A nil document means that
// level has none, and a missing member means the level says nothing about it.
type layers struct {
	deployment, tenant *policyDoc
	user               *gen.UserSettings
	// versions are the revisions the documents were read at (0 for none).
	versions map[Level]int64
	// retention is what the history store holds for the caller's application and
	// tenant, or nil when the service keeps the retention in its documents.
	retention *Retention
}

// The platform defaults, used when no level says anything. Features that need an
// installed component or reach beyond the conversation (voice, attachments, history
// export) default to OFF and are first allowed at the deployment level; what is
// harmless and expected (notifications, keeping the transcript the history API
// serves) defaults to ON. A lower level can only keep or narrow what the one above
// allowed.
const (
	defaultVoiceAllowed         = false
	defaultAttachmentsAllowed   = false
	defaultExportAllowed        = false
	defaultNotificationsAllowed = true
	defaultTranscriptAllowed    = true

	defaultSpeechRatePct = 100
	defaultTextScalePct  = 100
)

// computeEffective adds the levels up with fixed rules (never a model):
//
//   - a preference takes the most specific level that states it (user, then tenant,
//     then deployment, then the platform default);
//   - a permission that enables a feature is the deployment's (or the default) and
//     each level below can only switch it off;
//   - a retention or a size is the smallest value any level states, never above the
//     operator's ceiling;
//   - the capabilities any level switched off are all off;
//   - extra confirmation is the lowest threshold any level asked for.
//
// The result never exceeds what the policy gateway exposes, because it feeds
// nothing into it: it is a read-out for clients and for the components that honour
// a preference.
func computeEffective(l Limits, ly layers) gen.EffectiveSettings {
	var dep, ten policyDoc
	if ly.deployment != nil {
		dep = *ly.deployment
	}
	if ly.tenant != nil {
		ten = *ly.tenant
	}
	var usr gen.UserSettings
	if ly.user != nil {
		usr = *ly.user
	}
	decided := map[string]gen.DecidedBy{}

	out := gen.EffectiveSettings{Levels: gen.EffectiveSettingsLevels{
		Deployment: ly.versions[LevelDeployment], Tenant: ly.versions[LevelTenant], User: ly.versions[LevelUser],
	}}

	// language: most specific wins.
	switch {
	case usr.Language != nil:
		out.Language, decided["language"] = usr.Language, gen.DecidedByUser
	case ten.DefaultLanguage != nil:
		out.Language, decided["language"] = ten.DefaultLanguage, gen.DecidedByTenant
	case dep.DefaultLanguage != nil:
		out.Language, decided["language"] = dep.DefaultLanguage, gen.DecidedByDeployment
	}

	// model profile: most specific that is still approved, else the operator's default.
	approved := func(p *gen.ProfileID) bool { return p != nil && slices.Contains(l.ApprovedProfiles, string(*p)) }
	switch {
	case approved(usr.ModelProfile):
		out.ModelProfile, decided["model_profile"] = *usr.ModelProfile, gen.DecidedByUser
	case approved(ten.DefaultModelProfile):
		out.ModelProfile, decided["model_profile"] = *ten.DefaultModelProfile, gen.DecidedByTenant
	case approved(dep.DefaultModelProfile):
		out.ModelProfile, decided["model_profile"] = *dep.DefaultModelProfile, gen.DecidedByDeployment
	default:
		out.ModelProfile, decided["model_profile"] = gen.ProfileID(l.DefaultProfile), gen.DecidedByOperator
	}

	// voice
	var depVoice, tenVoice *bool
	if dep.Voice != nil {
		depVoice = dep.Voice.Allowed
	}
	if ten.Voice != nil {
		tenVoice = ten.Voice.Allowed
	}
	voiceAllowed, voiceBy := narrowed(depVoice, tenVoice, defaultVoiceAllowed)
	out.Voice = gen.EffectiveSettingsVoice{Allowed: voiceAllowed, SpeechRatePct: defaultSpeechRatePct, AutoPlay: true, BargeIn: true}
	userVoice := false
	if v := usr.Voice; v != nil {
		if v.Enabled != nil {
			userVoice = *v.Enabled
		}
		// A voice the operator has withdrawn since it was stored is not selected.
		if v.VoiceID != nil && slices.Contains(l.ApprovedVoices, string(*v.VoiceID)) {
			out.Voice.VoiceID = v.VoiceID
		}
		if v.SpeechRatePct != nil {
			out.Voice.SpeechRatePct = *v.SpeechRatePct
		}
		if v.AutoPlay != nil {
			out.Voice.AutoPlay = *v.AutoPlay
		}
		if v.BargeIn != nil {
			out.Voice.BargeIn = *v.BargeIn
		}
	}
	out.Voice.Enabled = voiceAllowed && userVoice
	decided["voice.enabled"] = byForPreference(voiceAllowed, voiceBy, usr.Voice != nil && usr.Voice.Enabled != nil)

	// history
	var depDays, tenDays, usrDays *gen.RetentionDays
	var depExport, tenExport *bool
	if dep.History != nil {
		depDays, depExport = dep.History.RetentionDays, dep.History.ExportAllowed
	}
	if ten.History != nil {
		tenDays, tenExport = ten.History.RetentionDays, ten.History.ExportAllowed
	}
	if usr.History != nil {
		usrDays = usr.History.RetentionDays
	}
	retentions := []cand{{ptrInt(depDays), gen.DecidedByDeployment}, {ptrInt(tenDays), gen.DecidedByTenant}, {ptrInt(usrDays), gen.DecidedByUser}}
	if r := ly.retention; r != nil {
		// The application's and the tenant's retention are the history store's, which has
		// already taken the shorter of them: that figure is one candidate, so the rule is
		// applied once. The user's own preference narrows it further and is not enforced
		// by history, which keeps no per-user policy.
		retentions = []cand{{ptrInt(usrDays), gen.DecidedByUser}}
		if r.EffectiveDays > 0 {
			d, by := int64(r.EffectiveDays), gen.DecidedByDeployment
			if r.Source == "tenant" {
				by = gen.DecidedByTenant
			}
			retentions = append([]cand{{&d, by}}, retentions...)
		}
	}
	days, daysBy := smallest(int64(l.DefaultHistoryRetentionDays), retentions...)
	if days > int64(l.MaxHistoryRetentionDays) {
		days, daysBy = int64(l.MaxHistoryRetentionDays), gen.DecidedByOperator
	}
	export, exportBy := narrowed(depExport, tenExport, defaultExportAllowed)
	out.History = gen.EffectiveSettingsHistory{RetentionDays: gen.RetentionDays(days), AutoCleanup: true, ExportAllowed: export}
	if usr.History != nil && usr.History.AutoCleanup != nil {
		out.History.AutoCleanup = *usr.History.AutoCleanup
	}
	decided["history.retention_days"] = daysBy
	decided["history.export_allowed"] = exportBy

	// privacy: the transcript is kept only when the policy allows it and the user did not ask otherwise.
	var depTr, tenTr *bool
	if dep.Privacy != nil {
		depTr = dep.Privacy.TranscriptAllowed
	}
	if ten.Privacy != nil {
		tenTr = ten.Privacy.TranscriptAllowed
	}
	trAllowed, trBy := narrowed(depTr, tenTr, defaultTranscriptAllowed)
	userStore, userStoreSet := true, false
	if usr.Privacy != nil && usr.Privacy.StoreTranscript != nil {
		userStore, userStoreSet = *usr.Privacy.StoreTranscript, true
	}
	out.Privacy = gen.EffectiveSettingsPrivacy{StoreTranscript: trAllowed && userStore}
	decided["privacy.store_transcript"] = byForPreference(trAllowed, trBy, userStoreSet)

	// attachments
	var depAtt, tenAtt *bool
	var depMB, tenMB, usrMB *gen.FileMegabytes
	if dep.Attachments != nil {
		depAtt, depMB = dep.Attachments.Allowed, dep.Attachments.MaxFileMb
	}
	if ten.Attachments != nil {
		tenAtt, tenMB = ten.Attachments.Allowed, ten.Attachments.MaxFileMb
	}
	if usr.Attachments != nil {
		usrMB = usr.Attachments.MaxFileMb
	}
	attAllowed, attBy := narrowed(depAtt, tenAtt, defaultAttachmentsAllowed)
	userAccept, userAcceptSet := true, false
	if usr.Attachments != nil && usr.Attachments.AcceptUploads != nil {
		userAccept, userAcceptSet = *usr.Attachments.AcceptUploads, true
	}
	mb, mbBy := smallest(int64(l.MaxAttachmentMB),
		cand{ptrInt(depMB), gen.DecidedByDeployment}, cand{ptrInt(tenMB), gen.DecidedByTenant}, cand{ptrInt(usrMB), gen.DecidedByUser})
	if mb > int64(l.MaxAttachmentMB) {
		mb, mbBy = int64(l.MaxAttachmentMB), gen.DecidedByOperator
	}
	out.Attachments = gen.EffectiveSettingsAttachments{Allowed: attAllowed, Enabled: attAllowed && userAccept, MaxFileMb: gen.FileMegabytes(mb)}
	decided["attachments.enabled"] = byForPreference(attAllowed, attBy, userAcceptSet)
	decided["attachments.max_file_mb"] = mbBy

	// sources
	out.Sources = gen.EffectiveSettingsSources{IncludeAttachments: true, CitationDisplay: gen.CitationDisplayInline}
	if usr.Sources != nil {
		if usr.Sources.IncludeAttachments != nil {
			out.Sources.IncludeAttachments = *usr.Sources.IncludeAttachments
		}
		if usr.Sources.CitationDisplay != nil {
			out.Sources.CitationDisplay = *usr.Sources.CitationDisplay
		}
	}

	// notifications
	var depNote, tenNote *bool
	if dep.Notifications != nil {
		depNote = dep.Notifications.Allowed
	}
	if ten.Notifications != nil {
		tenNote = ten.Notifications.Allowed
	}
	noteAllowed, noteBy := narrowed(depNote, tenNote, defaultNotificationsAllowed)
	userNote, userNoteSet := true, false
	events := []gen.NotificationEvent{gen.NotificationEventActionReady, gen.NotificationEventJobFinished, gen.NotificationEventSourceIndexed}
	if usr.Notifications != nil {
		if usr.Notifications.Enabled != nil {
			userNote, userNoteSet = *usr.Notifications.Enabled, true
		}
		if usr.Notifications.Events != nil {
			events = slices.Clone(usr.Notifications.Events)
			sort.Slice(events, func(i, j int) bool { return events[i] < events[j] })
		}
	}
	out.Notifications = gen.EffectiveSettingsNotifications{Allowed: noteAllowed, Enabled: noteAllowed && userNote, Events: events}
	decided["notifications.enabled"] = byForPreference(noteAllowed, noteBy, userNoteSet)

	// streaming and accessibility: the user's preferences over fixed defaults.
	out.Streaming = gen.EffectiveSettingsStreaming{Enabled: true, Unit: gen.StreamUnitToken}
	if usr.Streaming != nil {
		if usr.Streaming.Enabled != nil {
			out.Streaming.Enabled = *usr.Streaming.Enabled
		}
		if usr.Streaming.Unit != nil {
			out.Streaming.Unit = *usr.Streaming.Unit
		}
	}
	out.Accessibility = gen.EffectiveSettingsAccessibility{TextScalePct: defaultTextScalePct}
	if a := usr.Accessibility; a != nil {
		out.Accessibility.ReducedMotion = a.ReducedMotion != nil && *a.ReducedMotion
		out.Accessibility.HighContrast = a.HighContrast != nil && *a.HighContrast
		out.Accessibility.ScreenReader = a.ScreenReader != nil && *a.ScreenReader
		out.Accessibility.Captions = a.Captions != nil && *a.Captions
		if a.TextScalePct != nil {
			out.Accessibility.TextScalePct = *a.TextScalePct
		}
	}

	// capabilities: whatever any level switched off is off.
	out.Capabilities = gen.EffectiveSettingsCapabilities{Disabled: unionIDs(dep.Capabilities, ten.Capabilities, usr.Capabilities)}

	// extra confirmation: the lowest threshold any level asked for.
	var depC, tenC, usrC *gen.RiskLevel
	if dep.Confirmation != nil {
		depC = dep.Confirmation.ExtraFromRiskLevel
	}
	if ten.Confirmation != nil {
		tenC = ten.Confirmation.ExtraFromRiskLevel
	}
	if usr.Confirmation != nil {
		usrC = usr.Confirmation.ExtraFromRiskLevel
	}
	lowest, found := int64(0), false
	var lowestBy gen.DecidedBy
	for _, c := range []cand{{ptrInt(depC), gen.DecidedByDeployment}, {ptrInt(tenC), gen.DecidedByTenant}, {ptrInt(usrC), gen.DecidedByUser}} {
		if c.v != nil && (!found || *c.v < lowest) {
			lowest, lowestBy, found = *c.v, c.by, true
		}
	}
	if found {
		r := gen.RiskLevel(lowest)
		out.Confirmation.ExtraFromRiskLevel = &r
		decided["confirmation.extra_from_risk_level"] = lowestBy
	}

	out.DecidedBy = decided
	return out
}

// narrowed resolves a permission that is the deployment's to give and every level
// below can only take away: the deployment's value (def when it states none), then
// switched off by the tenant when the tenant says false. A true below the top never
// enables anything. It also says which level decided the outcome.
func narrowed(dep, ten *bool, def bool) (bool, gen.DecidedBy) {
	val, by := def, gen.DecidedByDefault
	if dep != nil {
		val, by = *dep, gen.DecidedByDeployment
	}
	if ten != nil && !*ten && val {
		val, by = false, gen.DecidedByTenant
	}
	return val, by
}

// byForPreference says which level decided "allowed and the user wants it":
// the policy when it forbids, otherwise the user when the user stated a wish,
// otherwise the default.
func byForPreference(allowed bool, policyBy gen.DecidedBy, userStated bool) gen.DecidedBy {
	switch {
	case !allowed:
		return policyBy
	case userStated:
		return gen.DecidedByUser
	}
	return gen.DecidedByDefault
}

type cand struct {
	v  *int64
	by gen.DecidedBy
}

// smallest returns the smallest stated value, preferring the more general level on
// a tie, or def when none is stated.
func smallest(def int64, cs ...cand) (int64, gen.DecidedBy) {
	best, by, found := def, gen.DecidedByDefault, false
	for _, c := range cs {
		if c.v != nil && (!found || *c.v < best) {
			best, by, found = *c.v, c.by, true
		}
	}
	return best, by
}

func ptrInt[T ~int64](p *T) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}

// unionIDs is the sorted union of the capabilities the levels switch off.
func unionIDs(sets ...*gen.CapabilitySwitches) []common.CapabilityID {
	seen := map[common.CapabilityID]bool{}
	out := []common.CapabilityID{}
	for _, s := range sets {
		if s == nil {
			continue
		}
		for _, id := range s.Disabled {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// disabledBy lists, per capability, the levels whose documents switch it off.
func disabledBy(ly layers) map[string][]Level {
	out := map[string][]Level{}
	add := func(level Level, s *gen.CapabilitySwitches) {
		if s == nil {
			return
		}
		for _, id := range s.Disabled {
			out[string(id)] = append(out[string(id)], level)
		}
	}
	if ly.deployment != nil {
		add(LevelDeployment, ly.deployment.Capabilities)
	}
	if ly.tenant != nil {
		add(LevelTenant, ly.tenant.Capabilities)
	}
	if ly.user != nil {
		add(LevelUser, ly.user.Capabilities)
	}
	return out
}
