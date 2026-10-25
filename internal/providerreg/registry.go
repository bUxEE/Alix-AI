// Package providerreg is the persistent registry of external provider and model profiles
// (additive plan section 5), the capability gate that rejects an impossible request before any
// data is sent, and the lifecycle that decides which profile may be used at all. External mode
// is opt-in: nothing in this package contacts a provider, and nothing outside milestone X
// imports it except the composition roots.
//
// # What a profile is
//
// A profile is the typed record of schemas/providers/provider-profile: one provider, one pinned
// model id and version, a credential REFERENCE (never the secret), the capabilities and terms
// the provider states, the applications, tenants and workspaces that may use it, the data classes
// it may receive, one named fallback and the conformance and billing-notice records. Profiles are
// stored as canonical JSON in the module "providerreg", one application-level row each: every
// statement names application_id, tenant_id and workspace_id, so one application never sees
// another's profiles. permitted_scopes then narrow use to tenants and workspaces inside the
// application; an empty list permits nobody.
//
// # Lifecycle
//
//	candidate -> qualified -> quarantined -> candidate ...      retired is terminal
//
// A profile is production-qualified only through Qualify, which needs, together: a bound
// credential that the credential service reports active, an acknowledged billing notice (the
// current version and text), a policy review that is not stale, and a passing conformance report
// recorded for the exact provider, model id and model version (additive plan section 15: a
// successful basic chat request qualifies nothing). Qualify recomputes the report's verdict from
// its checks and does not trust its outcome field. A changed model version (PinModelVersion)
// clears the report and returns the profile to candidate.
//
// # Staleness (ADR-0028, provisional)
//
// A profile is reviewed by an operator on last_policy_review and goes stale review_interval_days
// later, never more than MaxReviewIntervalDays (30): it is fresh until 00:00 UTC of the day
// that many days after the review date, and stale from then on. A stale profile is quarantined
// (stale_review) by Revalidate and, without waiting for a sweep, by every Check. Observe
// quarantines on a changed terms hash (terms_changed), a drifted model version
// (model_version_changed), a changed processing region (region_changed) and a reported excess of
// credential permissions (excessive_permissions). A quarantined profile is never used; it returns
// to candidate only through the call that belongs to its reason: Review (stale_review,
// terms_changed, operator_hold), Release (region_changed, excessive_permissions),
// RecordConformance (conformance_failed) or PinModelVersion (model_version_changed). A pin never
// ends another reason, and a changed terms hash clears the conformance report.
//
// Revalidate is scheduled by a class 6 maintenance job (JobRevalidate, see jobs.go), so expired
// reviews are quarantined without a request arriving first.
//
// # No hidden fallback
//
// A profile names at most one fallback and the conditions under which it applies (unavailable,
// timeout, rate_limited, unsupported_modality). Fallback follows it only for such a condition,
// only while the primary profile itself is qualified, fresh and holds a valid credential, only
// to a target that passes every gate for the same request, and records the hop. An invalid
// credential, stale terms, denied egress or an exhausted budget never select an alternate
// provider, whatever condition the caller names.
//
// # Errors
//
// Every failure is a *contracts.Error whose Details["reason"] is one of the Reason values (see
// ReasonOf). Credential, terms, permission and fail-closed outcomes are never retryable codes, so
// no caller can mistake them for a reason to try elsewhere.
package providerreg

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/internal/ids"
	"github.com/buxee/alix-ai/internal/store"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("providerreg", migrations) }

// MaxReviewIntervalDays is the longest revalidation interval a profile may state (ADR-0028,
// provisional: 30 days). A shorter interval is stricter and allowed.
const MaxReviewIntervalDays = 30

// maxChainLen bounds a fallback walk. Cycles are refused when a fallback is set, so the bound is
// only a guard against a corrupted store.
const maxChainLen = 16

// Profile is the typed provider profile of schemas/providers/provider-profile.
type Profile = providers.ProviderProfile

// CredentialStatusSource reports the status of a credential reference. The credential service
// (internal/credentials) implements it; the registry never holds, reads or logs a secret. An
// error, or any status but active, means the credential is not usable.
type CredentialStatusSource interface {
	CredentialStatus(ctx context.Context, scope contracts.Scope, ref providers.CredentialRefID) (providers.CredentialRefStatus, error)
}

// Config wires a Registry.
type Config struct {
	// Clock is the source of time (staleness, timestamps). Nil means the wall clock.
	Clock clock.Clock
	// Credentials answers whether a credential reference is usable. Required: the registry
	// cannot tell a valid credential from an invalid one without it, and fails closed.
	Credentials CredentialStatusSource
}

// Registry is the profile registry. It is safe for concurrent use.
type Registry struct {
	db    *store.DB
	clk   clock.Clock
	ids   *ids.Generator
	creds CredentialStatusSource
}

// New returns a Registry over db. The database must have been opened with the "providerreg"
// migrations (the default registry has them once this package is linked in).
func New(db *store.DB, cfg Config) (*Registry, error) {
	if db == nil {
		return nil, contracts.New(contracts.ErrInternal, "providerreg: no store")
	}
	if cfg.Credentials == nil {
		return nil, contracts.New(contracts.ErrInternal, "providerreg: a credential status source is required")
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.Real{}
	}
	return &Registry{db: db, clk: clk, ids: ids.NewGenerator(clk), creds: cfg.Credentials}, nil
}

// Reason is a fixed, machine-readable cause carried in Details["reason"] of every error this
// package returns. It is never free text and never quotes a request, a credential or provider
// output.
type Reason string

// The reasons. Quarantine reasons reuse the schema's own values (stale_review, terms_changed,
// model_version_changed, conformance_failed, region_changed, operator_hold).
const (
	ReasonProfileNotFound   Reason = "profile_not_found"
	ReasonInvalidProfile    Reason = "invalid_profile"
	ReasonScopeNotPermitted Reason = "scope_not_permitted"
	ReasonNotQualified      Reason = "not_qualified"
	ReasonRetired           Reason = "retired"
	ReasonDataClassDenied   Reason = "data_class_denied"
	ReasonCapability        Reason = "capability_unsupported"

	ReasonCredentialMissing Reason = "credential_missing"
	ReasonCredentialInvalid Reason = "credential_invalid"

	ReasonBillingNoticeNotAcknowledged Reason = "billing_notice_not_acknowledged"
	ReasonBillingNoticeVersionMismatch Reason = "billing_notice_version_mismatch"
	ReasonBillingConfirmationsMissing  Reason = "billing_confirmations_missing"

	ReasonConformanceMissing  Reason = "conformance_missing"
	ReasonConformanceMismatch Reason = "conformance_mismatch"
	ReasonConformanceFailed   Reason = "conformance_failed"
	ReasonConformanceStale    Reason = "conformance_stale"

	ReasonFallbackConditionForbidden Reason = "fallback_condition_forbidden"
	ReasonFallbackNotNamed           Reason = "fallback_not_named"
	ReasonFallbackInvalid            Reason = "fallback_invalid"

	ReasonStaleReview          Reason = "stale_review"
	ReasonTermsChanged         Reason = "terms_changed"
	ReasonModelVersionChanged  Reason = "model_version_changed"
	ReasonRegionChanged        Reason = "region_changed"
	ReasonExcessivePermissions Reason = "excessive_permissions"
	ReasonOperatorHold         Reason = "operator_hold"
	ReasonNotReleasable        Reason = "not_releasable"

	// The capability reasons Check lists, in Details["reasons"], for an impossible request.
	ReasonRoleUnsupported        Reason = "role_unsupported"
	ReasonToolCallingUnsupported Reason = "tool_calling_unsupported"
	ReasonToolArgumentsNotStrict Reason = "tool_arguments_not_strict"
	ReasonTooManyTools           Reason = "too_many_tools"
	ReasonStructuredOutputWeak   Reason = "structured_output_insufficient"
	ReasonStreamingUnsupported   Reason = "streaming_unsupported"
	ReasonInputModalityMissing   Reason = "input_modality_unsupported"
	ReasonOutputModalityMissing  Reason = "output_modality_unsupported"
	ReasonImageGenerationMissing Reason = "image_generation_unsupported"
	ReasonSpeechToTextMissing    Reason = "stt_unsupported"
	ReasonTextToSpeechMissing    Reason = "tts_unsupported"
	ReasonRealtimeVoiceMissing   Reason = "realtime_unsupported"
	ReasonContextTooLarge        Reason = "context_too_large"
	ReasonOutputTooLarge         Reason = "output_too_large"
	ReasonRequestTooLarge        Reason = "request_too_large"
	ReasonRequirementInvalid     Reason = "requirement_invalid"
)

// ReasonOf returns the Reason of an error returned by this package, or "".
func ReasonOf(err error) Reason {
	var ce *contracts.Error
	if errors.As(err, &ce) && ce != nil {
		if s, ok := ce.Details["reason"].(string); ok {
			return Reason(s)
		}
	}
	return ""
}

// fail builds the typed error. msg is a fixed sentence; it never carries request content.
func fail(code contracts.ErrCode, reason Reason, msg string) *contracts.Error {
	return contracts.New(code, "providerreg: "+msg).WithDetails("reason", string(reason))
}

// notFound is the one answer for a profile that does not exist in the caller's application,
// whether it exists in another application or nowhere.
func notFound() *contracts.Error {
	return fail(contracts.ErrValidationFailed, ReasonProfileNotFound, "no such provider profile")
}

// appOf validates the whole scope and returns the application the registry rows are keyed on.
// An invalid scope keeps its contracts code (scope_missing, scope_violation): there is no
// default application.
func appOf(scope contracts.Scope) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	return scope.ApplicationID, nil
}

// actorOf is who is recorded for a change made under scope.
func actorOf(scope contracts.Scope) string { return scope.PrincipalID }

// systemActor is recorded for a change the registry makes on its own (a lazy quarantine).
const systemActor = "system"

// timestamp is the schema's RFC 3339 UTC instant.
func timestamp(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
}

// parseDate reads a schema "date" (YYYY-MM-DD) as 00:00 UTC.
func parseDate(s string) (time.Time, error) {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("not a date: %w", err)
	}
	return d.UTC(), nil
}

// wrapStore types a store failure that is not already typed.
func wrapStore(err error, msg string) error {
	if err == nil {
		return nil
	}
	var ce *contracts.Error
	if errors.As(err, &ce) {
		return err
	}
	return contracts.Wrap(contracts.ErrUnavailable, err, msg)
}
