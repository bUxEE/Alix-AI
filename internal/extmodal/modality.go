package extmodal

import (
	"context"
	"errors"
	"sync"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Modality is one optional capability of an external provider.
type Modality string

// The modalities. All are OFF until a Policy enables them (ADR-0024).
const (
	// ModVision is image input to a vision-capable model.
	ModVision Modality = "vision"
	// ModFileInput is document input (PDF, text) by reference.
	ModFileInput Modality = "file_input"
	// ModSTT is external speech to text on raw audio.
	ModSTT Modality = "stt"
	// ModImageGeneration is image output.
	ModImageGeneration Modality = "image_generation"
	// ModTTS is external text to speech.
	ModTTS Modality = "tts"
)

// All lists the modalities in report order.
func All() []Modality {
	return []Modality{ModVision, ModFileInput, ModSTT, ModImageGeneration, ModTTS}
}

// Valid reports whether m is known.
func (m Modality) Valid() bool {
	for _, x := range All() {
		if x == m {
			return true
		}
	}
	return false
}

// unsupportedName is the feature a model lacks when it cannot serve m, the value
// providers.IsUnsupported reports.
func (m Modality) unsupportedName() string {
	switch m {
	case ModVision:
		return "image_input"
	case ModFileInput:
		return "file_input"
	case ModSTT:
		return "audio_input"
	case ModImageGeneration:
		return "image_output"
	case ModTTS:
		return "audio_output"
	}
	return string(m)
}

// role is the provider-profile role that serves m.
func (m Modality) role() pv.Role {
	switch m {
	case ModVision, ModFileInput:
		return pv.RoleVision
	case ModImageGeneration:
		return pv.RoleImageGeneration
	case ModSTT:
		return pv.RoleSTT
	case ModTTS:
		return pv.RoleTTS
	}
	return ""
}

// Reason is a fixed code that explains a refusal. It never holds content.
type Reason string

// The reasons.
const (
	ReasonModalityOff          Reason = "modality_off"
	ReasonPolicyUnavailable    Reason = "policy_unavailable"
	ReasonUnsupported          Reason = "unsupported_modality"
	ReasonRetentionUndisclosed Reason = "retention_undisclosed"
	ReasonRawAudioNotPermitted Reason = "raw_audio_not_permitted"
	ReasonAudioNotSegmented    Reason = "audio_not_vad_segmented"
	ReasonTooLarge             Reason = "media_too_large"
	ReasonTextNotVerified      Reason = "text_not_verified"
	ReasonTextDenied           Reason = "text_egress_denied"
	ReasonAudioDenied          Reason = "audio_egress_denied"
	ReasonFileUnresolved       Reason = "file_unresolved"
	ReasonFileExpired          Reason = "reference_expired"
	ReasonFileNotApproved      Reason = "file_not_approved"
	ReasonPrivateFile          Reason = "private_file_needs_egress_permission"
	ReasonExcerptOnly          Reason = "file_approved_as_excerpt_only"
	ReasonMediaType            Reason = "media_type_not_allowed"
	ReasonOutputInvalid        Reason = "provider_output_invalid"
	ReasonStoreRefused         Reason = "local_store_refused"
	ReasonBadRequest           Reason = "invalid_request"
	// ReasonNoBudget and ReasonNotEstimable refuse an external call before it is made, as the text router does:
	// no provider budget is configured, or the call has no upper bound to reserve. A budget that refuses a
	// reservation carries the reason of internal/providerbudget.
	ReasonNoBudget     Reason = "no_budget"
	ReasonNotEstimable Reason = "estimate_failed"
)

// refuse builds the typed denial for a policy or data-handling refusal. It is
// permission_denied; the reason and the modality are in the details.
func refuse(m Modality, r Reason, msg string) error {
	return contracts.New(contracts.ErrPermissionDenied, "extmodal: "+msg).
		WithDetails("reason", string(r)).WithDetails("modality", string(m))
}

func invalid(m Modality, msg string) error {
	return contracts.New(contracts.ErrValidationFailed, "extmodal: "+msg).
		WithDetails("reason", string(ReasonBadRequest)).WithDetails("modality", string(m))
}

// unsupported is the typed result for a model that cannot serve m.
func unsupported(m Modality) error {
	err := providers.UnsupportedError(m.unsupportedName())
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		return err
	}
	return ce.WithDetails("reason", string(ReasonUnsupported)).WithDetails("modality", string(m))
}

// ReasonOf returns the refusal reason of an error from this package, or "".
func ReasonOf(err error) Reason {
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		return ""
	}
	s, _ := ce.Details["reason"].(string)
	return Reason(s)
}

// Level is one level's switches. The zero Level enables nothing.
type Level struct {
	// Modalities are the modalities this level turns on.
	Modalities map[Modality]bool
	// RawAudio is the explicit permission for raw audio to leave. Enabling ModSTT does not imply it.
	RawAudio bool
}

// Policy is the provider level and the tenant level for one provider and scope. A modality, and raw
// audio, is permitted only when BOTH levels say so (the most restrictive wins).
type Policy struct {
	Provider Level
	Tenant   Level
}

// Enabled reports whether both levels enable m.
func (p Policy) Enabled(m Modality) bool { return p.Provider.Modalities[m] && p.Tenant.Modalities[m] }

// RawAudioPermitted reports whether both levels permit raw audio to leave.
func (p Policy) RawAudioPermitted() bool { return p.Provider.RawAudio && p.Tenant.RawAudio }

// PolicySource returns the policy for a provider in a scope. An error denies.
type PolicySource interface {
	ModalityPolicy(ctx context.Context, scope contracts.Scope, providerID string) (Policy, error)
}

// MemoryPolicy is an in-memory PolicySource. Nothing set means everything off.
type MemoryPolicy struct {
	mu        sync.Mutex
	providers map[string]Level
	tenants   map[string]Level
}

// NewMemoryPolicy returns an empty (all off) policy source.
func NewMemoryPolicy() *MemoryPolicy {
	return &MemoryPolicy{providers: map[string]Level{}, tenants: map[string]Level{}}
}

func tenantKey(s contracts.Scope) string {
	return s.DeploymentID + "\x00" + s.ApplicationID + "\x00" + s.TenantID
}

// SetProvider sets the provider level.
func (p *MemoryPolicy) SetProvider(providerID string, l Level) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.providers[providerID] = cloneLevel(l)
}

// SetTenant sets the tenant level of the scope's application and tenant.
func (p *MemoryPolicy) SetTenant(scope contracts.Scope, l Level) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tenants[tenantKey(scope)] = cloneLevel(l)
}

// ModalityPolicy implements PolicySource.
func (p *MemoryPolicy) ModalityPolicy(_ context.Context, scope contracts.Scope, providerID string) (Policy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return Policy{Provider: cloneLevel(p.providers[providerID]), Tenant: cloneLevel(p.tenants[tenantKey(scope)])}, nil
}

// Enable is a Level with the given modalities on.
func Enable(rawAudio bool, ms ...Modality) Level {
	l := Level{Modalities: map[Modality]bool{}, RawAudio: rawAudio}
	for _, m := range ms {
		l.Modalities[m] = true
	}
	return l
}

func cloneLevel(l Level) Level {
	out := Level{RawAudio: l.RawAudio, Modalities: make(map[Modality]bool, len(l.Modalities))}
	for k, v := range l.Modalities {
		out.Modalities[k] = v
	}
	return out
}
