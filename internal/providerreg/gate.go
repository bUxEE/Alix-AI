package providerreg

import (
	"context"
	"slices"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Requirement is what one request needs from a provider, stated before any data is sent. It
// carries sizes, classes and capability needs: never content. The zero value of each need means
// "not needed". DataClass and Role are always required.
type Requirement struct {
	// Role is the job the model is asked to do.
	Role providers.Role
	// DataClass is the most restrictive classification of anything in the request. An empty or
	// unknown value is treated as restricted, and restricted never leaves.
	DataClass providers.DataClassification

	// TypedActionProposals is true when the capability needs the model to propose typed actions
	// (tool or function calls). It needs structured tool calls with strict arguments; AlixAI
	// has no constrained adapter yet, so a provider without them is never selected.
	TypedActionProposals bool
	// ToolCount is the number of tools the request offers.
	ToolCount int64
	// StructuredOutput is the weakest structured-output guarantee the capability accepts
	// (none, json_mode, schema_best_effort, strict_schema). Empty means none needed.
	StructuredOutput providers.ProviderProfileStructuredOutput
	// Streaming is true when the caller needs a streamed response.
	Streaming bool
	// Input and Output are the modalities the request sends and expects.
	Input, Output []providers.Modality
	// ImageGeneration, SpeechToText, TextToSpeech and RealtimeVoice name the voice and image
	// capabilities the request needs.
	ImageGeneration, SpeechToText, TextToSpeech, RealtimeVoice bool
	// ContextToks, OutputToks and RequestBytes are upper bounds for the request.
	ContextToks, OutputToks, RequestBytes int64
}

// Admission is the proof that a request passed every gate. Only Check and Route produce a
// valid one; the zero value is not valid. The sender of a request takes one.
type Admission struct {
	// Profile is the profile the request may use, as it was when the gates ran.
	Profile providers.ProviderProfile
	passed  bool
}

// Valid reports whether the admission came from the gates.
func (a Admission) Valid() bool { return a.passed }

// structuredRank orders the structured-output guarantees from weakest to strongest.
var structuredRank = map[providers.ProviderProfileStructuredOutput]int{
	providers.ProviderProfileStructuredOutputNone:             0,
	providers.ProviderProfileStructuredOutputJSONMode:         1,
	providers.ProviderProfileStructuredOutputSchemaBestEffort: 2,
	providers.ProviderProfileStructuredOutputStrictSchema:     3,
}

// gateCapabilities lists every reason the profile cannot serve the requirement. It is pure: it
// reads the profile and the requirement and nothing else, so it runs before anything is sent.
func gateCapabilities(p providers.ProviderProfile, req Requirement) []Reason {
	var out []Reason
	add := func(r Reason) { out = append(out, r) }

	if !slices.Contains(p.Roles, req.Role) {
		add(ReasonRoleUnsupported)
	}
	if req.TypedActionProposals {
		if !p.ToolCalling.Supported {
			add(ReasonToolCallingUnsupported)
		} else if !p.ToolCalling.StrictArguments {
			add(ReasonToolArgumentsNotStrict)
		}
	}
	if req.ToolCount > 0 {
		if !p.ToolCalling.Supported {
			if !req.TypedActionProposals {
				add(ReasonToolCallingUnsupported)
			}
		} else if p.ToolCalling.MaxTools != nil && req.ToolCount > *p.ToolCalling.MaxTools {
			add(ReasonTooManyTools)
		}
	}
	if req.StructuredOutput != "" {
		need, ok := structuredRank[req.StructuredOutput]
		have := structuredRank[p.StructuredOutput] // an unknown guarantee ranks as none
		if !ok || have < need {
			add(ReasonStructuredOutputWeak)
		}
	}
	if req.Streaming && !p.Streaming {
		add(ReasonStreamingUnsupported)
	}
	for _, m := range req.Input {
		if !slices.Contains(p.Modalities.Input, m) {
			add(ReasonInputModalityMissing)
			break
		}
	}
	for _, m := range req.Output {
		if !slices.Contains(p.Modalities.Output, m) {
			add(ReasonOutputModalityMissing)
			break
		}
	}
	if req.ImageGeneration && !p.ImageGeneration.Supported {
		add(ReasonImageGenerationMissing)
	}
	if req.SpeechToText && !p.Voice.STT {
		add(ReasonSpeechToTextMissing)
	}
	if req.TextToSpeech && !p.Voice.TTS {
		add(ReasonTextToSpeechMissing)
	}
	if req.RealtimeVoice && !p.Voice.Realtime {
		add(ReasonRealtimeVoiceMissing)
	}
	if req.ContextToks < 0 || req.OutputToks < 0 || req.RequestBytes < 0 || req.ToolCount < 0 {
		add(ReasonRequirementInvalid)
	}
	if req.ContextToks > p.Limits.MaxContextToks {
		add(ReasonContextTooLarge)
	}
	if req.OutputToks > p.Limits.MaxOutputToks {
		add(ReasonOutputTooLarge)
	}
	if req.RequestBytes > p.Limits.MaxRequestBytes {
		add(ReasonRequestTooLarge)
	}
	return out
}

// classAllowed reports whether the profile may receive the data class at all. Restricted, empty
// and unknown classes are never allowed: an unknown classification is the most restrictive.
func classAllowed(p providers.ProviderProfile, c providers.DataClassification) bool {
	switch c {
	case providers.DataClassificationPublic, providers.DataClassificationInternal, providers.DataClassificationConfidential:
		return slices.Contains(p.AllowedDataClasses, providers.EgressEligibleClass(c))
	}
	return false
}

// usable runs the gates that do not depend on the request: the profile must be permitted for the
// scope, qualified, internally consistent and fresh. A stale profile is quarantined on the spot,
// so a profile nobody has swept is still refused. It returns the (possibly just quarantined)
// profile.
func (r *Registry) usable(ctx context.Context, scope contracts.Scope, p providers.ProviderProfile) (providers.ProviderProfile, error) {
	if !permits(p.PermittedScopes, scope) {
		return p, fail(contracts.ErrPermissionDenied, ReasonScopeNotPermitted, "the provider profile is not permitted for this application, tenant and workspace")
	}
	switch p.Lifecycle {
	case providers.ProfileLifecycleQualified:
	case providers.ProfileLifecycleQuarantined:
		reason := ReasonOperatorHold
		if p.QuarantineReason != nil {
			reason = Reason(*p.QuarantineReason)
		}
		return p, fail(contracts.ErrQuarantined, reason, "the provider profile is quarantined pending review")
	case providers.ProfileLifecycleRetired:
		return p, fail(contracts.ErrPermissionDenied, ReasonRetired, "the provider profile is retired")
	default:
		return p, fail(contracts.ErrPermissionDenied, ReasonNotQualified, "the provider profile is not production-qualified")
	}
	if stale, _ := staleness(p, r.clk.Now()); stale {
		q, err := r.mutate(ctx, scope, p.ProfileID, systemActor, func(e *edit) error {
			if e.p.Lifecycle != providers.ProfileLifecycleQualified {
				return nil
			}
			e.quarantine(providers.ProviderProfileQuarantineReasonStaleReview)
			return nil
		})
		if err != nil {
			return p, err
		}
		return q, fail(contracts.ErrQuarantined, ReasonStaleReview, "the provider profile's policy review is stale")
	}
	// Defence in depth: a qualified profile must still hold what qualification needs.
	if blockers := qualificationBlockers(p, r.clk.Now(), false); len(blockers) > 0 {
		return p, fail(contracts.ErrPermissionDenied, ReasonNotQualified, "the provider profile no longer meets the qualification conditions").
			WithDetails("missing", reasonStrings(blockers))
	}
	return p, nil
}

// credentialUsable asks the credential service whether the profile's credential works. Anything
// but an active credential, including an error from the service, is a refusal that never
// selects another provider.
func (r *Registry) credentialUsable(ctx context.Context, scope contracts.Scope, p providers.ProviderProfile) error {
	if p.CredRef == nil || !validCredRef(*p.CredRef) {
		return fail(contracts.ErrPermissionDenied, ReasonCredentialMissing, "the provider profile has no credential bound")
	}
	status, err := r.creds.CredentialStatus(ctx, scope, *p.CredRef)
	if err != nil || status != providers.CredentialRefStatusActive {
		return fail(contracts.ErrPermissionDenied, ReasonCredentialInvalid, "the provider credential is not valid; no other provider is selected")
	}
	return nil
}

// Check runs every gate for a request that would use the profile, and sends nothing. The
// profile must exist in the caller's application and be permitted for the caller's tenant and
// workspace, be qualified and fresh, be allowed to receive the data class, offer every
// capability the requirement needs, and hold a credential the credential service reports
// active. The first failing gate decides the error; an impossible request lists every missing
// capability in Details["reasons"].
func (r *Registry) Check(ctx context.Context, scope contracts.Scope, id providers.ProfileID, req Requirement) (Admission, error) {
	p, err := r.Get(ctx, scope, id)
	if err != nil {
		return Admission{}, err
	}
	return r.checkProfile(ctx, scope, p, req)
}

func (r *Registry) checkProfile(ctx context.Context, scope contracts.Scope, p providers.ProviderProfile, req Requirement) (Admission, error) {
	p, err := r.usable(ctx, scope, p)
	if err != nil {
		return Admission{}, err
	}
	if !classAllowed(p, req.DataClass) {
		return Admission{}, fail(contracts.ErrPermissionDenied, ReasonDataClassDenied, "the provider profile may not receive this data class")
	}
	if missing := gateCapabilities(p, req); len(missing) > 0 {
		return Admission{}, fail(contracts.ErrValidationFailed, ReasonCapability, "the provider profile cannot serve this request").
			WithDetails("reasons", reasonStrings(missing))
	}
	if err := r.credentialUsable(ctx, scope, p); err != nil {
		return Admission{}, err
	}
	return Admission{Profile: p, passed: true}, nil
}

// Route runs every gate and only then calls send with the admission. When a gate fails, send is
// never called, so no data is built, sent or even looked at for a request that cannot be served.
// send's own error is returned unchanged.
func (r *Registry) Route(ctx context.Context, scope contracts.Scope, id providers.ProfileID, req Requirement, send func(context.Context, Admission) error) error {
	if send == nil {
		return contracts.New(contracts.ErrInternal, "providerreg: Route needs a send function")
	}
	adm, err := r.Check(ctx, scope, id, req)
	if err != nil {
		return err
	}
	return send(ctx, adm)
}

func reasonStrings(rs []Reason) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}
