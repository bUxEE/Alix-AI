package providerreg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/common"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Register stores a new candidate profile in the caller's application and returns it with its
// minted id. The content (endpoint, model, capabilities, terms, permitted scopes, data classes,
// fallback, review date, billing notice) comes from the operator and is validated; the id,
// credential, lifecycle, acknowledgement, conformance report and timestamps are the registry's
// and must be empty in the draft. A named fallback must already exist in the application.
func (r *Registry) Register(ctx context.Context, scope contracts.Scope, draft providers.ProviderProfile) (providers.ProviderProfile, error) {
	app, err := appOf(scope)
	if err != nil {
		return providers.ProviderProfile{}, err
	}
	now := r.clk.Now()
	if err := validateDraft(draft, app, now); err != nil {
		return providers.ProviderProfile{}, err
	}
	p := draft
	p.ProfileID = providers.ProfileID(r.ids.New("pprf"))
	p.Lifecycle = providers.ProfileLifecycleCandidate
	p.CreatedAt, p.UpdatedAt = common.Timestamp(timestamp(now)), common.Timestamp(timestamp(now))
	err = r.db.Tx(ctx, func(tx *store.Tx) error {
		if p.Fallback.ProfileID != nil {
			if _, found, err := loadProfile(ctx, tx, app, *p.Fallback.ProfileID); err != nil {
				return err
			} else if !found {
				return fail(contracts.ErrValidationFailed, ReasonFallbackInvalid, "the fallback profile does not exist in this application")
			}
		}
		if err := insertProfile(ctx, tx, app, p, now); err != nil {
			return err
		}
		return insertEvent(ctx, tx, app, Event{
			ProfileID: p.ProfileID, Kind: EventRegistered, FromLifecycle: providers.ProfileLifecycleCandidate,
			ToLifecycle: providers.ProfileLifecycleCandidate, Actor: actorOf(scope), At: now,
		})
	})
	if err != nil {
		return providers.ProviderProfile{}, err
	}
	return p, nil
}

// BindCredential records the credential REFERENCE on the profile. The value must have the shape
// of an opaque reference minted by the credential service; anything else (a pasted key or token
// included) is refused and never echoed. The registry never receives a secret.
func (r *Registry) BindCredential(ctx context.Context, scope contracts.Scope, id providers.ProfileID, ref providers.CredentialRefID) (providers.ProviderProfile, error) {
	if _, err := appOf(scope); err != nil {
		return providers.ProviderProfile{}, err
	}
	if !validCredRef(ref) {
		return providers.ProviderProfile{}, invalid("not a credential reference: bind the opaque reference the credential service returned, never a key or token")
	}
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		}
		e.p.CredRef = &ref
		e.note(EventCredentialBound, "", string(ref))
		return nil
	})
}

// NoticeDigest is the digest of a billing notice as it was shown: version, text and billing URL.
// An acknowledgement is valid only for the notice whose digest it recorded.
func NoticeDigest(n providers.ProviderProfileBillingNotice) providers.SHA256 {
	url := ""
	if n.URL != nil {
		url = *n.URL
	}
	sum := sha256.Sum256([]byte(n.Version + "\n" + n.Text + "\n" + url))
	return providers.SHA256("sha256:" + hex.EncodeToString(sum[:]))
}

// Acknowledgement is what an operator confirms about the provider-specific billing notice
// (additive plan section 4.1: a consumer subscription is not API access).
type Acknowledgement struct {
	// NoticeVersion is the version of the notice the operator was shown.
	NoticeVersion string
	// ConsumerSubscriptionIsNotAPIAccess and SeparateAPIBillingUnderstood must both be true.
	ConsumerSubscriptionIsNotAPIAccess bool
	SeparateAPIBillingUnderstood       bool
}

// AcknowledgeBillingNotice records that the operator in scope has seen the profile's current
// billing notice: who (the scope's principal), when (the clock), which version and its digest.
// The version must be the profile's current one; a notice that changes afterwards invalidates
// the acknowledgement and blocks activation again.
func (r *Registry) AcknowledgeBillingNotice(ctx context.Context, scope contracts.Scope, id providers.ProfileID, a Acknowledgement) (providers.ProviderProfile, error) {
	if _, err := appOf(scope); err != nil {
		return providers.ProviderProfile{}, err
	}
	if !a.ConsumerSubscriptionIsNotAPIAccess || !a.SeparateAPIBillingUnderstood {
		return providers.ProviderProfile{}, fail(contracts.ErrValidationFailed, ReasonBillingConfirmationsMissing, "both billing confirmations are required")
	}
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		}
		if a.NoticeVersion != e.p.BillingNotice.Version {
			return fail(contracts.ErrValidationFailed, ReasonBillingNoticeVersionMismatch, "the acknowledged notice is not the profile's current notice")
		}
		appID := common.Identifier(e.app)
		ack := providers.BillingNoticeAck{
			AckID:         providers.AcknowledgementID(r.ids.New("ack")),
			ProfileID:     e.p.ProfileID,
			ProviderID:    e.p.ProviderID,
			NoticeVersion: e.p.BillingNotice.Version,
			NoticeDigest:  NoticeDigest(e.p.BillingNotice),
			Scope: providers.BillingNoticeAckScope{
				DeploymentID:  common.Identifier(scope.DeploymentID),
				ApplicationID: &appID,
			},
			Confirmations:  providers.BillingNoticeAckConfirmations{ConsumerSubscriptionIsNotAPIAccess: true, SeparateAPIBillingUnderstood: true},
			AcknowledgedBy: common.Identifier(scope.PrincipalID),
			AcknowledgedAt: common.Timestamp(timestamp(e.now)),
		}
		e.p.BillingNoticeAck = &ack
		e.note(EventBillingAcknowledged, "", string(ack.AckID))
		return nil
	})
}

// ackCurrent reports whether the profile holds an acknowledgement of exactly its current notice.
func ackCurrent(p providers.ProviderProfile) bool {
	a := p.BillingNoticeAck
	return a != nil && a.ProfileID == p.ProfileID && a.NoticeVersion == p.BillingNotice.Version &&
		a.NoticeDigest == NoticeDigest(p.BillingNotice) && a.AcknowledgedBy != "" &&
		a.Confirmations.ConsumerSubscriptionIsNotAPIAccess && a.Confirmations.SeparateAPIBillingUnderstood
}

// declaredMedia is what the profile declares for the three conditional multimodal checks.
func declaredMedia(p providers.ProviderProfile) (image, file, audio bool) {
	has := func(m providers.Modality) bool {
		return slices.Contains(p.Modalities.Input, m) || slices.Contains(p.Modalities.Output, m)
	}
	return has(providers.ModalityImage), has(providers.ModalityFile), has(providers.ModalityAudio)
}

// reportAge parses a report's completed_at.
func reportCompleted(rep *providers.ConformanceReport) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, string(rep.CompletedAt))
	return t.UTC(), err == nil
}

// sameModel reports whether the report was produced for exactly this profile, provider, model id
// and model version.
func sameModel(p providers.ProviderProfile, rep *providers.ConformanceReport) bool {
	return rep.Provider.ProfileID == p.ProfileID && rep.Provider.ProviderID == p.ProviderID &&
		rep.Provider.ModelID == p.ModelID && rep.Provider.ModelVersion == p.ModelVersion
}

// reportVerdict decides whether a recorded report qualifies the profile. It returns "" when it
// does. It does not trust the report's outcome: it recomputes the verdict from the checks,
// refuses a report for another model version or declaration, and refuses one that is older
// than the review interval or from the future.
func reportVerdict(p providers.ProviderProfile, rep *providers.ConformanceReport, now time.Time, checkAge bool) Reason {
	if rep == nil {
		return ReasonConformanceMissing
	}
	image, file, audio := declaredMedia(p)
	if !sameModel(p, rep) || rep.Declared.Image != image || rep.Declared.File != file || rep.Declared.Audio != audio {
		return ReasonConformanceMismatch
	}
	raw, err := json.Marshal(rep.Groups)
	if err != nil {
		return ReasonConformanceFailed
	}
	var groups map[string]map[string]struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(raw, &groups); err != nil || len(groups) == 0 {
		return ReasonConformanceFailed
	}
	for _, checks := range groups {
		for _, c := range checks {
			// A check passes. not_applicable is allowed only below, for undeclared media.
			if c.Result != "passed" && c.Result != "not_applicable" {
				return ReasonConformanceFailed
			}
		}
	}
	m := rep.Groups.Multimodal
	for _, c := range []struct {
		declared bool
		result   providers.ConformanceReportConditionalCheckResultResult
	}{
		{image, m.ImageInputOutput.Result}, {file, m.FileInputOutput.Result}, {audio, m.AudioInputOutput.Result},
	} {
		if c.declared && c.result != providers.ConformanceReportConditionalCheckResultResultPassed {
			return ReasonConformanceFailed
		}
	}
	if rep.Outcome != providers.ConformanceReportOutcomePassed {
		return ReasonConformanceFailed
	}
	done, ok := reportCompleted(rep)
	if !ok || done.After(now) {
		return ReasonConformanceMismatch
	}
	if !checkAge {
		return ""
	}
	if due := done.AddDate(0, 0, int(min(max(p.ReviewIntervalDays, 1), MaxReviewIntervalDays))); !now.Before(due) {
		return ReasonConformanceStale
	}
	return ""
}

// qualificationBlockers lists, in a fixed order, what still stands between the profile and
// production qualification, except the credential's live status (the credential service is asked
// separately). Empty means the stored record qualifies. checkAge applies the review-interval
// age limit to the report: Qualify applies it; a profile that is already qualified is held to
// the policy review date instead, so a report ageing out never silently disables a profile.
func qualificationBlockers(p providers.ProviderProfile, now time.Time, checkAge bool) []Reason {
	var out []Reason
	if p.CredRef == nil || !validCredRef(*p.CredRef) {
		out = append(out, ReasonCredentialMissing)
	}
	if !ackCurrent(p) {
		out = append(out, ReasonBillingNoticeNotAcknowledged)
	}
	if v := reportVerdict(p, p.LastConformance, now, checkAge); v != "" {
		out = append(out, v)
	}
	if stale, _ := staleness(p, now); stale {
		out = append(out, ReasonStaleReview)
	}
	return out
}

// RecordConformance records a conformance report on the profile. The report must be for exactly
// this profile, provider, model id and model version: a report for another version is refused
// and never recorded, and a report older than the one recorded is refused. A failing report is
// recorded (it is a fact) but never qualifies; on a qualified profile it quarantines it
// (conformance_failed); a passing report releases a profile quarantined for that reason back to
// candidate. Qualification itself is a separate, explicit step: Qualify.
func (r *Registry) RecordConformance(ctx context.Context, scope contracts.Scope, id providers.ProfileID, rep providers.ConformanceReport) (providers.ProviderProfile, error) {
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		}
		done, ok := reportCompleted(&rep)
		if !sameModel(e.p, &rep) || !ok {
			return fail(contracts.ErrValidationFailed, ReasonConformanceMismatch, "the report is not for this profile, model id and model version")
		}
		if old := e.p.LastConformance; old != nil {
			if prev, ok := reportCompleted(old); ok && done.Before(prev) {
				return fail(contracts.ErrValidationFailed, ReasonConformanceStale, "a newer report is already recorded")
			}
		}
		e.p.LastConformance = &rep
		verdict := reportVerdict(e.p, &rep, e.now, true)
		e.note(EventConformanceRecorded, verdict, string(rep.ReportID))
		switch {
		case verdict == ReasonConformanceFailed && e.p.Lifecycle == providers.ProfileLifecycleQualified:
			e.quarantine(providers.ProviderProfileQuarantineReasonConformanceFailed)
		case verdict == "" && e.p.Lifecycle == providers.ProfileLifecycleQuarantined &&
			e.p.QuarantineReason != nil && *e.p.QuarantineReason == providers.ProviderProfileQuarantineReasonConformanceFailed:
			e.setLifecycle(providers.ProfileLifecycleCandidate)
			e.note(EventReleased, ReasonConformanceFailed, string(rep.ReportID))
		}
		return nil
	})
}

// Qualify makes a candidate production-qualified. It needs, together: a bound credential the
// credential service reports active, an acknowledged current billing notice, a passing
// conformance report for the exact model id and version, and a fresh policy review. Anything
// missing is a refusal listing what (Details["missing"]); a stale review also quarantines the
// profile. A quarantined or retired profile is never qualified directly.
func (r *Registry) Qualify(ctx context.Context, scope contracts.Scope, id providers.ProfileID) (providers.ProviderProfile, error) {
	p, err := r.Get(ctx, scope, id)
	if err != nil {
		return providers.ProviderProfile{}, err
	}
	credActive := false
	if p.CredRef != nil && validCredRef(*p.CredRef) {
		st, err := r.creds.CredentialStatus(ctx, scope, *p.CredRef)
		credActive = err == nil && st == providers.CredentialRefStatusActive
	}
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		switch e.p.Lifecycle {
		case providers.ProfileLifecycleQualified:
			return nil
		case providers.ProfileLifecycleQuarantined:
			reason := ReasonOperatorHold
			if e.p.QuarantineReason != nil {
				reason = Reason(*e.p.QuarantineReason)
			}
			return fail(contracts.ErrQuarantined, reason, "a quarantined provider profile must be reviewed before it can be qualified")
		case providers.ProfileLifecycleRetired:
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		}
		blockers := qualificationBlockers(e.p, e.now, true)
		if !credActive && !slices.Contains(blockers, ReasonCredentialMissing) {
			blockers = append(blockers, ReasonCredentialInvalid)
		}
		if slices.Contains(blockers, ReasonStaleReview) {
			e.quarantine(providers.ProviderProfileQuarantineReasonStaleReview)
		}
		if len(blockers) > 0 {
			code, reason := contracts.ErrPermissionDenied, blockers[0]
			if e.keep {
				code, reason = contracts.ErrQuarantined, ReasonStaleReview
			}
			return fail(code, reason, "the provider profile cannot be production-qualified yet").
				WithDetails("missing", reasonStrings(blockers))
		}
		e.setLifecycle(providers.ProfileLifecycleQualified)
		e.note(EventQualified, "", string(e.p.LastConformance.ReportID))
		return nil
	})
}

// PinModelVersion changes the model version a profile is bound to. A changed version resets
// qualification: the conformance report is cleared and the profile returns to candidate, so it
// must pass the suite again for the new version (ADR-0028). A profile has no other way to change
// its model version. The acknowledgement and credential are kept.
//
// A pin never lifts a quarantine that is not about the model version: a profile held for
// terms_changed, region_changed, excessive_permissions, stale_review, conformance_failed or
// operator_hold takes the new version and loses its report, but stays quarantined for its own
// reason until the call that ends that reason (Review, Release, RecordConformance). Only a
// model_version_changed quarantine ends with the pin.
func (r *Registry) PinModelVersion(ctx context.Context, scope contracts.Scope, id providers.ProfileID, version providers.ModelVersion) (providers.ProviderProfile, error) {
	if _, err := appOf(scope); err != nil {
		return providers.ProviderProfile{}, err
	}
	if !versionRE.MatchString(string(version)) {
		return providers.ProviderProfile{}, invalid("the model version is malformed")
	}
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		}
		if e.p.ModelVersion == version {
			return nil
		}
		e.p.ModelVersion = version
		e.p.LastConformance = nil
		if e.p.Lifecycle != providers.ProfileLifecycleQuarantined ||
			(e.p.QuarantineReason != nil && *e.p.QuarantineReason == providers.ProviderProfileQuarantineReasonModelVersionChanged) {
			e.setLifecycle(providers.ProfileLifecycleCandidate)
		}
		e.note(EventModelVersionPinned, ReasonModelVersionChanged, "")
		return nil
	})
}

// Observation is what the provider adapter currently sees. A nil or empty member is not
// observed.
type Observation struct {
	// TermsHash is the hash of the provider terms as they read now.
	TermsHash *providers.SHA256
	// ModelVersion is the version the provider actually serves for the pinned model id.
	ModelVersion *providers.ModelVersion
	// ReplyModel is the model name a provider put in a reply. It is compared with the pinned model id and
	// version by ReplyModelMatches; a name that does not match is a drifted model (model_version_changed),
	// the same as a changed ModelVersion. The router reports it for every reply that differs.
	ReplyModel *string
	// Regions are the processing regions the provider now reports.
	Regions []providers.RegionCode
	// ExcessivePermissions is true when the provider or the credential service reported that the
	// credential grants more than the profile needs (excessive_permissions_reported).
	ExcessivePermissions bool
}

// Observe compares what the adapter sees with what the profile recorded, and quarantines the
// profile when they differ: a changed terms hash (terms_changed), a drifted model version
// (model_version_changed), a changed processing region (region_changed) or a reported excess of
// credential permissions (excessive_permissions). A profile already quarantined keeps its
// reason; a retired profile is left alone.
func (r *Registry) Observe(ctx context.Context, scope contracts.Scope, id providers.ProfileID, obs Observation) (providers.ProviderProfile, error) {
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleQuarantined || e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return nil
		}
		switch {
		case obs.TermsHash != nil && *obs.TermsHash != e.p.Terms.TermsHash:
			e.quarantine(providers.ProviderProfileQuarantineReasonTermsChanged)
		case obs.ModelVersion != nil && *obs.ModelVersion != e.p.ModelVersion:
			e.quarantine(providers.ProviderProfileQuarantineReasonModelVersionChanged)
		case obs.ReplyModel != nil && !ReplyModelMatches(e.p.ModelID, e.p.ModelVersion, *obs.ReplyModel):
			e.quarantine(providers.ProviderProfileQuarantineReasonModelVersionChanged)
		case len(obs.Regions) > 0 && !sameRegions(obs.Regions, e.p.Region.ProcessingRegions):
			e.quarantine(providers.ProviderProfileQuarantineReasonRegionChanged)
		case obs.ExcessivePermissions:
			e.quarantine(providers.ProviderProfileQuarantineReasonExcessivePermissions)
		}
		return nil
	})
}

func sameRegions(a, b []providers.RegionCode) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// ReviewInput is an operator's review of the provider terms, pricing, retention, region and
// tool behaviour.
type ReviewInput struct {
	// TermsHash is the hash of the terms text that was read.
	TermsHash providers.SHA256
	// ReviewedOn is the date of the review (YYYY-MM-DD), not in the future and not before the
	// previous review.
	ReviewedOn string
}

// Review records an operator's policy review: the terms hash and dates are updated. A profile
// quarantined for a stale review, changed terms or an operator hold returns to candidate and
// must be qualified again; one quarantined for another reason (model version, failed
// conformance, region, excessive permissions) stays quarantined (Release ends the last two).
// A qualified profile whose terms hash changed is reset to candidate; with unchanged terms it
// stays qualified and its review date moves forward. A changed terms hash always clears the
// conformance report: the report was produced under the old terms, so the profile is qualified
// again only on a report recorded after the terms were reviewed (ADR-0028).
func (r *Registry) Review(ctx context.Context, scope contracts.Scope, id providers.ProfileID, in ReviewInput) (providers.ProviderProfile, error) {
	if _, err := appOf(scope); err != nil {
		return providers.ProviderProfile{}, err
	}
	if !sha256RE.MatchString(string(in.TermsHash)) {
		return providers.ProviderProfile{}, invalid("the terms hash is malformed")
	}
	day, err := parseDate(in.ReviewedOn)
	if err != nil || day.After(r.clk.Now().UTC().Truncate(24*time.Hour)) {
		return providers.ProviderProfile{}, invalid("the review date is malformed or in the future")
	}
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		}
		prev, perr := parseDate(e.p.LastPolicyReview)
		if perr == nil && day.Before(prev) {
			return invalid("a review cannot be dated before the previous review")
		}
		next := e.p
		next.LastPolicyReview = in.ReviewedOn
		if stale, _ := staleness(next, e.now); stale {
			return invalid("the review is already stale at its own interval")
		}
		changed := in.TermsHash != e.p.Terms.TermsHash
		e.p.Terms.TermsHash, e.p.Terms.RecordedOn, e.p.LastPolicyReview = in.TermsHash, in.ReviewedOn, in.ReviewedOn
		if changed {
			e.p.LastConformance = nil
		}
		switch e.p.Lifecycle {
		case providers.ProfileLifecycleQuarantined:
			if e.p.QuarantineReason == nil {
				return contracts.New(contracts.ErrInternal, "providerreg: a quarantined profile names no reason")
			}
			switch *e.p.QuarantineReason {
			case providers.ProviderProfileQuarantineReasonStaleReview, providers.ProviderProfileQuarantineReasonTermsChanged, providers.ProviderProfileQuarantineReasonOperatorHold:
				reason := Reason(*e.p.QuarantineReason)
				e.setLifecycle(providers.ProfileLifecycleCandidate)
				e.note(EventReleased, reason, "")
			}
		case providers.ProfileLifecycleQualified:
			if changed {
				e.setLifecycle(providers.ProfileLifecycleCandidate)
			}
		}
		reason := Reason("")
		if changed {
			reason = ReasonTermsChanged
		}
		e.note(EventReviewed, reason, "")
		return nil
	})
}

// ReleaseInput is an operator's decision to end a region_changed or excessive_permissions
// quarantine.
type ReleaseInput struct {
	// ReviewedOn is the date of the review that justifies the release (YYYY-MM-DD), not in the
	// future and not before the previous review. It becomes the profile's review date.
	ReviewedOn string
	// Regions are the processing regions the operator accepted. Required for region_changed:
	// non-empty unless the profile's residency is unknown, in which case empty. They replace the
	// recorded regions, so the next Observe compares against what was accepted. Ignored for
	// excessive_permissions.
	Regions []providers.RegionCode
}

// Release ends a quarantine that Review does not end: region_changed (the provider moved the
// processing region) and excessive_permissions (the credential grants more than the profile
// needs). Both are human decisions, so the call needs a review date, and each has its own
// condition: region_changed records the regions the operator accepted, and excessive_permissions
// needs the credential service to report the bound credential active again (rotated or
// narrowed). The profile returns to candidate and must be qualified again. A profile quarantined
// for any other reason, or not quarantined, is refused (not_releasable): other reasons end
// through Review, RecordConformance or PinModelVersion.
func (r *Registry) Release(ctx context.Context, scope contracts.Scope, id providers.ProfileID, in ReleaseInput) (providers.ProviderProfile, error) {
	if _, err := appOf(scope); err != nil {
		return providers.ProviderProfile{}, err
	}
	day, err := parseDate(in.ReviewedOn)
	if err != nil || day.After(r.clk.Now().UTC().Truncate(24*time.Hour)) {
		return providers.ProviderProfile{}, invalid("the review date is malformed or in the future")
	}
	for _, rc := range in.Regions {
		if !regionRE.MatchString(string(rc)) {
			return providers.ProviderProfile{}, invalid("a processing region is malformed")
		}
	}
	cur, err := r.Get(ctx, scope, id)
	if err != nil {
		return providers.ProviderProfile{}, err
	}
	credActive := false
	if cur.CredRef != nil && validCredRef(*cur.CredRef) {
		st, err := r.creds.CredentialStatus(ctx, scope, *cur.CredRef)
		credActive = err == nil && st == providers.CredentialRefStatusActive
	}
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		}
		if e.p.Lifecycle != providers.ProfileLifecycleQuarantined || e.p.QuarantineReason == nil {
			return fail(contracts.ErrValidationFailed, ReasonNotReleasable, "the provider profile is not quarantined")
		}
		reason := *e.p.QuarantineReason
		switch reason {
		case providers.ProviderProfileQuarantineReasonRegionChanged:
			if (len(in.Regions) == 0) != (e.p.Region.Residency == providers.ResidencyUnknown) {
				return invalid("name the accepted processing regions (none when the residency is unknown)")
			}
			e.p.Region.ProcessingRegions = slices.Clone(in.Regions)
		case providers.ProviderProfileQuarantineReasonExcessivePermissions:
			if !credActive {
				return fail(contracts.ErrPermissionDenied, ReasonCredentialInvalid, "the credential must be reported active before the profile is released")
			}
		default:
			return fail(contracts.ErrValidationFailed, ReasonNotReleasable, "only region_changed and excessive_permissions end through Release")
		}
		prev, perr := parseDate(e.p.LastPolicyReview)
		if perr == nil && day.Before(prev) {
			return invalid("a review cannot be dated before the previous review")
		}
		next := e.p
		next.LastPolicyReview = in.ReviewedOn
		if stale, _ := staleness(next, e.now); stale {
			return invalid("the review is already stale at its own interval")
		}
		e.p.LastPolicyReview = in.ReviewedOn
		e.setLifecycle(providers.ProfileLifecycleCandidate)
		e.note(EventReleased, Reason(reason), "")
		return nil
	})
}

// Hold quarantines a profile on the operator's own decision (operator_hold).
func (r *Registry) Hold(ctx context.Context, scope contracts.Scope, id providers.ProfileID) (providers.ProviderProfile, error) {
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		switch e.p.Lifecycle {
		case providers.ProfileLifecycleRetired:
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		case providers.ProfileLifecycleQuarantined:
			return nil
		}
		e.quarantine(providers.ProviderProfileQuarantineReasonOperatorHold)
		return nil
	})
}

// Retire ends a profile's use for good. A retired profile is never routed or qualified again.
func (r *Registry) Retire(ctx context.Context, scope contracts.Scope, id providers.ProfileID) (providers.ProviderProfile, error) {
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return nil
		}
		e.setLifecycle(providers.ProfileLifecycleRetired)
		e.note(EventRetired, "", "")
		return nil
	})
}

// Revalidate quarantines every candidate or qualified profile of the caller's application whose
// policy review is stale and returns their ids. Run it at the configured interval; Check does
// the same lazily for the profile it is asked about, so a missed sweep never lets a stale
// profile through.
func (r *Registry) Revalidate(ctx context.Context, scope contracts.Scope) ([]providers.ProfileID, error) {
	all, err := r.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	var held []providers.ProfileID
	for _, p := range all {
		if p.Lifecycle != providers.ProfileLifecycleCandidate && p.Lifecycle != providers.ProfileLifecycleQualified {
			continue
		}
		if stale, _ := staleness(p, r.clk.Now()); !stale {
			continue
		}
		q, err := r.mutate(ctx, scope, p.ProfileID, systemActor, func(e *edit) error {
			if e.p.Lifecycle == providers.ProfileLifecycleCandidate || e.p.Lifecycle == providers.ProfileLifecycleQualified {
				if stale, _ := staleness(e.p, e.now); stale {
					e.quarantine(providers.ProviderProfileQuarantineReasonStaleReview)
				}
			}
			return nil
		})
		if err != nil {
			return held, err
		}
		if q.Lifecycle == providers.ProfileLifecycleQuarantined {
			held = append(held, q.ProfileID)
		}
	}
	return held, nil
}

// SetPermittedScopes replaces the applications, tenants and workspaces that may use the profile.
// Grants name only the profile's own application; omitting a narrower level grants every value
// of it; an empty list permits nobody.
func (r *Registry) SetPermittedScopes(ctx context.Context, scope contracts.Scope, id providers.ProfileID, grants []providers.ProviderProfileScopeGrant) (providers.ProviderProfile, error) {
	app, err := appOf(scope)
	if err != nil {
		return providers.ProviderProfile{}, err
	}
	if err := validateGrants(app, grants); err != nil {
		return providers.ProviderProfile{}, err
	}
	return r.mutate(ctx, scope, id, actorOf(scope), func(e *edit) error {
		if e.p.Lifecycle == providers.ProfileLifecycleRetired {
			return fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
		}
		e.p.PermittedScopes = slices.Clone(grants)
		e.note(EventPermissionsSet, "", "")
		return nil
	})
}
