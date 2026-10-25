package providerreg

import (
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// The grammars of schemas/providers/shared and provider-profile, restated so the registry holds
// a profile to them without loading the schema files.
var (
	providerIDRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	modelIDRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,127}$`)
	versionRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,63}$`)
	sha256RE     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	regionRE     = regexp.MustCompile(`^(global|[a-z]{2}(-[a-z0-9]{1,16}){0,3})$`)
	// credRefRE is the only shape a credential reference can have: an opaque id minted by the
	// credential service. A raw key or token does not match it, so a secret cannot be stored
	// where a reference belongs.
	credRefRE  = regexp.MustCompile(`^cred_[0-9A-HJKMNP-TV-Z]{26}$`)
	profileRE  = regexp.MustCompile(`^pprf_[0-9A-HJKMNP-TV-Z]{26}$`)
	endpointRE = regexp.MustCompile(`^(https://[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+(:[0-9]{1,5})?|http://(127\.0\.0\.1|localhost|\[::1\])(:[0-9]{1,5})?)(/[A-Za-z0-9._~%!$&'()*+,;=:-]*)*$`)
)

const maxGrants = 256

func invalid(msg string) *contracts.Error {
	return fail(contracts.ErrValidationFailed, ReasonInvalidProfile, msg)
}

// validScopeValue is a permitted-scope identifier: present, bounded, printable, never a wildcard.
func validScopeValue(v string) bool {
	if v == "" || len(v) > contracts.MaxScopeValueLen || v == "*" || strings.TrimSpace(v) != v {
		return false
	}
	return !strings.ContainsFunc(v, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) })
}

// validCredRef reports whether ref has the shape of a credential reference.
func validCredRef(ref providers.CredentialRefID) bool { return credRefRE.MatchString(string(ref)) }

// validateGrants checks permitted_scopes against the application that owns the profile: a grant
// can name only its own application, a workspace needs its tenant, and there is no wildcard.
func validateGrants(app string, grants []providers.ProviderProfileScopeGrant) error {
	if len(grants) > maxGrants {
		return invalid("too many permitted scopes")
	}
	for _, g := range grants {
		if !validScopeValue(string(g.ApplicationID)) {
			return invalid("a permitted scope names no valid application")
		}
		if string(g.ApplicationID) != app {
			return fail(contracts.ErrScopeViolation, ReasonScopeNotPermitted, "a permitted scope names another application")
		}
		if g.TenantID != nil && !validScopeValue(string(*g.TenantID)) {
			return invalid("a permitted scope names an invalid tenant")
		}
		if g.WorkspaceID != nil {
			if g.TenantID == nil {
				return invalid("a permitted workspace needs its tenant")
			}
			if !validScopeValue(string(*g.WorkspaceID)) {
				return invalid("a permitted scope names an invalid workspace")
			}
		}
	}
	return nil
}

// permits reports whether the grants allow the request scope. Omitting a narrower level grants
// every value of it inside the named parent; a grant with a workspace and no tenant is
// malformed and permits nothing; no grants permit nobody.
func permits(grants []providers.ProviderProfileScopeGrant, s contracts.Scope) bool {
	for _, g := range grants {
		if string(g.ApplicationID) != s.ApplicationID {
			continue
		}
		if g.WorkspaceID != nil && g.TenantID == nil {
			continue
		}
		if g.TenantID != nil && string(*g.TenantID) != s.TenantID {
			continue
		}
		if g.WorkspaceID != nil && string(*g.WorkspaceID) != s.WorkspaceID {
			continue
		}
		return true
	}
	return false
}

func validateClasses(classes []providers.EgressEligibleClass) error {
	seen := map[providers.EgressEligibleClass]bool{}
	for _, c := range classes {
		if !c.Valid() || seen[c] {
			return invalid("allowed data classes hold an unknown or repeated class")
		}
		seen[c] = true
	}
	return nil
}

// validateFallbackShape checks the fallback member on its own: a target exactly when conditions
// are listed, and only conditions the schema allows.
func validateFallbackShape(f providers.ProviderProfileFallback, self providers.ProfileID) error {
	if (f.ProfileID == nil) != (len(f.On) == 0) {
		return fail(contracts.ErrValidationFailed, ReasonFallbackInvalid, "a fallback names a profile exactly when it lists conditions")
	}
	seen := map[providers.ProviderProfileFallbackOnItem]bool{}
	for _, c := range f.On {
		if !c.Valid() || seen[c] {
			return fail(contracts.ErrValidationFailed, ReasonFallbackInvalid, "a fallback lists an unknown or repeated condition")
		}
		seen[c] = true
	}
	if f.ProfileID != nil {
		if !profileRE.MatchString(string(*f.ProfileID)) {
			return fail(contracts.ErrValidationFailed, ReasonFallbackInvalid, "the fallback profile id is malformed")
		}
		if self != "" && *f.ProfileID == self {
			return fail(contracts.ErrValidationFailed, ReasonFallbackInvalid, "a profile cannot be its own fallback")
		}
	}
	return nil
}

// validateDraft checks operator-supplied content for a new profile. The core-owned members
// (id, credential, lifecycle, acknowledgement, report, timestamps) must be empty: they are
// recorded by the registry, never accepted from a caller.
func validateDraft(p providers.ProviderProfile, app string, now time.Time) error {
	if p.ProfileID != "" || p.CredRef != nil || p.BillingNoticeAck != nil || p.LastConformance != nil ||
		p.Lifecycle != "" || p.QuarantineReason != nil || p.CreatedAt != "" || p.UpdatedAt != "" {
		return invalid("the id, credential, lifecycle, acknowledgement, conformance report and timestamps are recorded by the registry, not supplied")
	}
	if !providerIDRE.MatchString(string(p.ProviderID)) || !modelIDRE.MatchString(string(p.ModelID)) || !versionRE.MatchString(string(p.ModelVersion)) {
		return invalid("provider id, model id or model version is malformed")
	}
	if !endpointRE.MatchString(p.Endpoint) || len(p.Endpoint) > 300 || strings.Contains(p.Endpoint, "/../") || strings.HasSuffix(p.Endpoint, "/..") {
		return invalid("the endpoint must be https (or loopback http) with no credentials, query or fragment")
	}
	for _, r := range p.Roles {
		if !r.Valid() {
			return invalid("unknown role")
		}
	}
	for _, m := range slices.Concat(p.Modalities.Input, p.Modalities.Output) {
		if !m.Valid() {
			return invalid("unknown modality")
		}
	}
	if !p.StructuredOutput.Valid() || !p.UsageMetering.Valid() {
		return invalid("unknown structured-output or usage-metering value")
	}
	if p.Limits.MaxContextToks < 1 || p.Limits.MaxOutputToks < 1 || p.Limits.MaxRequestBytes < 1 {
		return invalid("limits must be positive")
	}
	if p.BillingNotice.SubscriptionGrantsAPIAccess || strings.TrimSpace(p.BillingNotice.Version) == "" || strings.TrimSpace(p.BillingNotice.Text) == "" {
		return invalid("a profile needs a billing notice (version and text) and never claims a subscription is API access")
	}
	if !sha256RE.MatchString(string(p.Terms.TermsHash)) {
		return invalid("the terms hash is malformed")
	}
	for _, rc := range p.Region.ProcessingRegions {
		if !regionRE.MatchString(string(rc)) {
			return invalid("a processing region is malformed")
		}
	}
	if err := validateMediaRules(p); err != nil {
		return err
	}
	today := now.UTC().Truncate(24 * time.Hour)
	for _, d := range []string{p.LastPolicyReview, p.Terms.RecordedOn} {
		t, err := parseDate(d)
		if err != nil || t.After(today) {
			return invalid("a review or terms date is malformed or in the future")
		}
	}
	if p.ReviewIntervalDays < 1 || p.ReviewIntervalDays > MaxReviewIntervalDays {
		return invalid("the review interval is 1 to 30 days (ADR-0028)")
	}
	if stale, _ := staleness(p, now); stale {
		return invalid("the policy review is already stale: review the terms first")
	}
	if err := validateGrants(app, p.PermittedScopes); err != nil {
		return err
	}
	if err := validateClasses(p.AllowedDataClasses); err != nil {
		return err
	}
	return validateFallbackShape(p.Fallback, "")
}

// validateMediaRules restates the schema's cross-field rules the generated type cannot express:
// a declared capability needs its modality and its media handling.
func validateMediaRules(p providers.ProviderProfile) error {
	in := func(ms []providers.Modality, m providers.Modality) bool { return slices.Contains(ms, m) }
	if p.ImageGeneration.Supported && !in(p.Modalities.Output, providers.ModalityImage) {
		return invalid("image generation needs the image output modality")
	}
	if p.Voice.STT && !in(p.Modalities.Input, providers.ModalityAudio) {
		return invalid("speech to text needs the audio input modality")
	}
	if p.Voice.TTS && !in(p.Modalities.Output, providers.ModalityAudio) {
		return invalid("text to speech needs the audio output modality")
	}
	for _, c := range []struct {
		m       providers.Modality
		handled bool
	}{
		{providers.ModalityImage, p.Media != nil && p.Media.Images != nil},
		{providers.ModalityFile, p.Media != nil && p.Media.Files != nil},
		{providers.ModalityAudio, p.Media != nil && p.Media.Audio != nil},
	} {
		if (in(p.Modalities.Input, c.m) || in(p.Modalities.Output, c.m)) && !c.handled {
			return invalid("a media modality needs its media handling")
		}
	}
	return nil
}

// staleness reports whether the profile's policy review is stale at now. A profile reviewed on
// date D is fresh until 00:00 UTC of D plus its interval (never more than MaxReviewIntervalDays)
// and stale from then on. A malformed date or a non-positive interval is stale: fail closed.
func staleness(p providers.ProviderProfile, now time.Time) (bool, time.Time) {
	d, err := parseDate(p.LastPolicyReview)
	if err != nil || p.ReviewIntervalDays < 1 {
		return true, time.Time{}
	}
	due := d.AddDate(0, 0, int(min(p.ReviewIntervalDays, MaxReviewIntervalDays)))
	return !now.Before(due), due
}
