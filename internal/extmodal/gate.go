package extmodal

import (
	"context"
	"sync"
	"time"

	"github.com/buxee/alix-ai/internal/blocks"
	"github.com/buxee/alix-ai/internal/clock"
	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
	pv "github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// Purpose says why text leaves, for the TextEgress seam.
type Purpose string

// The purposes.
const (
	PurposeSpeech      Purpose = "speech"
	PurposeVisionAsk   Purpose = "vision_prompt"
	PurposeImagePrompt Purpose = "image_prompt"
	PurposeExcerpt     Purpose = "file_excerpt"
)

// TextEgress is the data-egress seam for text that leaves in a multimodal call (a prompt, a file
// excerpt, the text to speak). The composition root backs it with the data-egress guard
// (internal/dataegress.Guard): a nil error means the guard allowed this text for this target. The
// modality gate always runs first, so the guard is never consulted for a modality that is off.
//
// Category and class (M9-02 decision): spoken text, vision questions and image prompts are sent as category
// user_message at class internal; a file excerpt as file_content at class confidential. The composition root maps
// Purpose to these (internal/cli extWireTextEgress); the operator's egress policy decides what leaves (an excerpt
// only under its files setting, which is off by default).
type TextEgress interface {
	AllowText(ctx context.Context, scope contracts.Scope, t Target, p Purpose, text string) (TextDecision, error)
}

// TextDecision names the egress decision that allowed a text (or audio) and the envelope it was checked in. The
// budget reservation of the call that sends it belongs to them.
type TextDecision struct {
	EnvelopeID pv.EnvelopeID
	DecisionID pv.EgressDecisionID
}

// Audio is one utterance of raw audio as the data-egress seam sees it: what the guard decides on, never the bytes.
type Audio struct {
	// Class is the data class the session states for the utterance. Empty is unknown, the most restrictive class:
	// such audio never leaves.
	Class string
	// Bytes is the size of the utterance.
	Bytes int64
}

// AudioEgress is the data-egress seam for raw audio that would go to an external speech recogniser. The composition
// root backs it with the data-egress guard (internal/cli): the utterance is evaluated as an audio part of its class, so
// the kill switch, the policy chain, the audio setting of the egress policy (off by default) and the allowlists
// decide, and the decision is recorded as for text. A nil error means the guard allowed it; the decision it returns is
// the one the budget reservation of the call belongs to.
type AudioEgress interface {
	AllowAudio(ctx context.Context, scope contracts.Scope, t Target, a Audio) (TextDecision, error)
}

// FileResolver resolves an AlixAI file reference from scratch in the caller's scope, enforcing scope
// and expiry. *files.Service satisfies it.
type FileResolver interface {
	Stat(ctx context.Context, scope contracts.Scope, id string) (conversation.FileRef, error)
}

// Event is what Observer receives for each decision and call. It holds no content.
type Event struct {
	Modality      Modality
	Provider      string
	Model         string
	Allowed       bool
	Reason        Reason
	Route         string
	BytesSent     int64
	BytesReceived int64
	Latency       time.Duration
}

// Observer receives events. It must not block. A nil Observer discards them.
type Observer interface{ Observe(Event) }

// Deps are the collaborators of a Service.
type Deps struct {
	// Policy and Egress are required: without them nothing can be admitted (fail closed).
	Policy PolicySource
	Egress TextEgress
	// Audio is the data-egress seam for raw audio. Without it no raw audio leaves (external speech to text refuses).
	Audio AudioEgress
	// Files, Approvals and Store are needed by the operations that use them; without one, that
	// operation refuses.
	Files     FileResolver
	Approvals FileApprovals
	Store     GeneratedStore
	// Budgets is the provider budget every external call is reserved against before it is made
	// (*providerbudget.Service). Without it no external call is made.
	Budgets Budgets
	// Blocks validates provider rich output. Nil means blocks.Default().
	Blocks   *blocks.Validator
	Clock    clock.Clock
	Observer Observer
	// AttestTTL is how long a verified response stays speakable. Zero means 10 minutes.
	AttestTTL time.Duration
}

// Service gates and handles the data of the external multimodal roles. It is safe for concurrent use.
type Service struct {
	d   Deps
	clk clock.Clock
	val *blocks.Validator

	mu      sync.Mutex
	attests map[string][]attested
}

// New returns a Service. It fails closed on a missing dependency.
func New(d Deps) (*Service, error) {
	switch {
	case d.Policy == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "extmodal: a modality policy source is required")
	case d.Egress == nil:
		return nil, contracts.New(contracts.ErrValidationFailed, "extmodal: a text egress guard is required")
	case d.AttestTTL < 0:
		return nil, contracts.New(contracts.ErrValidationFailed, "extmodal: the attestation lifetime cannot be negative")
	}
	s := &Service{d: d, clk: d.Clock, val: d.Blocks, attests: map[string][]attested{}}
	if s.clk == nil {
		s.clk = clock.Real{}
	}
	if s.val == nil {
		s.val = blocks.Default()
	}
	if s.d.AttestTTL == 0 {
		s.d.AttestTTL = 10 * time.Minute
	}
	return s, nil
}

func (s *Service) observe(e Event) {
	if s.d.Observer != nil {
		s.d.Observer.Observe(e)
	}
}

// Admission is the allow of a modality: the policy that allowed it and what to disclose.
type Admission struct {
	Policy     Policy
	Disclosure Disclosure
}

// Admit decides whether modality m may be used with target t in scope. Checks run in a fixed order and
// stop at the first failure, and nothing is sent by Admit itself:
//
//  1. the provider and the tenant level of the policy both enable m (default OFF; a policy source that
//     fails denies);
//  2. the profile declares m with its handling block and role, and the runtime capabilities serve it
//     (otherwise the typed unsupported result);
//  3. the provider's retention for this media is stated (an unknown retention cannot be disclosed, so
//     the modality stays off for that provider).
func (s *Service) Admit(ctx context.Context, scope contracts.Scope, t Target, m Modality) (Admission, error) {
	adm, err := s.admit(ctx, scope, t, m)
	e := Event{Modality: m, Provider: string(t.Profile.ProviderID), Model: string(t.Profile.ModelID), Allowed: err == nil, Reason: ReasonOf(err)}
	s.observe(e)
	return adm, err
}

func (s *Service) admit(ctx context.Context, scope contracts.Scope, t Target, m Modality) (Admission, error) {
	if !m.Valid() {
		return Admission{}, invalid(m, "unknown modality")
	}
	if err := ctx.Err(); err != nil {
		return Admission{}, contracts.Wrap(contracts.ErrUnavailable, err, "extmodal: the request was cancelled")
	}
	if err := scope.Validate(); err != nil {
		return Admission{}, err
	}
	pol, err := s.d.Policy.ModalityPolicy(ctx, scope, string(t.Profile.ProviderID))
	if err != nil {
		return Admission{}, refuse(m, ReasonPolicyUnavailable, "the modality policy could not be read")
	}
	if !pol.Enabled(m) {
		return Admission{}, refuse(m, ReasonModalityOff, "this modality is off for this provider and tenant")
	}
	if !declares(t.Profile, m) || !supports(t.Caps, m) {
		return Admission{}, unsupported(m)
	}
	ret := retentionOf(t.Profile, m)
	if ret == "" {
		return Admission{}, refuse(m, ReasonRetentionUndisclosed, "the provider's retention of this media is not stated, so it cannot be disclosed")
	}
	return Admission{Policy: pol, Disclosure: newDisclosure(t, m, ret)}, nil
}

// allowText runs the egress seam for text that leaves.
func (s *Service) allowText(ctx context.Context, scope contracts.Scope, t Target, m Modality, p Purpose, text string) (TextDecision, error) {
	d, err := s.d.Egress.AllowText(ctx, scope, t, p, text)
	if err != nil {
		return TextDecision{}, refuse(m, ReasonTextDenied, "the data-egress guard refused this text")
	}
	return d, nil
}

// allowAudio runs the egress seam for raw audio that leaves. No seam is a refusal.
func (s *Service) allowAudio(ctx context.Context, scope contracts.Scope, t Target, a Audio) (TextDecision, error) {
	if s.d.Audio == nil {
		return TextDecision{}, refuse(ModSTT, ReasonAudioDenied, "no data-egress guard for audio is configured")
	}
	d, err := s.d.Audio.AllowAudio(ctx, scope, t, a)
	if err != nil {
		return TextDecision{}, refuse(ModSTT, ReasonAudioDenied, "the data-egress guard refused this audio")
	}
	return d, nil
}
