package hybrid

import (
	"context"

	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// TransitionKind says what the turn changed to.
type TransitionKind string

// The transitions.
const (
	// TransitionLocalFallback: the external route did not serve and the policy named the local
	// provider for the condition.
	TransitionLocalFallback TransitionKind = "local_fallback"
	// TransitionTemplateFallback: the text the turn shows is the deterministic template.
	TransitionTemplateFallback TransitionKind = "template_fallback"
	// TransitionProfileFallback: the profile's own named fallback to another external profile.
	TransitionProfileFallback TransitionKind = "profile_fallback"
)

// Transition is the data of the hybrid.transition audit event: a fixed-code record of where a turn
// went instead of the external route. It holds no text, no prompt and no secret.
type Transition struct {
	Kind TransitionKind `json:"kind"`
	// Reason is a fixed lower_snake code: verification_failed, a router reason or an outcome kind.
	Reason string `json:"reason"`
	// From and To are a profile id, "local" or "template".
	From string `json:"from"`
	To   string `json:"to"`
	// Condition is the failure condition the policy named, when there is one.
	Condition string `json:"condition,omitempty"`
	// Outcome is the router outcome kind of the turn.
	Outcome string `json:"outcome"`
	Mode    string `json:"mode"`
	Act     string `json:"act"`
	// Violations are the kinds of the verification failures (never the offending text).
	Violations []string `json:"violations,omitempty"`
	// DataSent is true when an external request may have left the deployment.
	DataSent bool `json:"data_sent"`
}

// EventTransition is the registered audit event type of a Transition.
var EventTransition = audit.MustRegister[Transition](audit.TypeSpec{
	Name:        "hybrid.transition",
	Description: "a Hybrid-mode turn used the local provider or the deterministic template instead of the external route; codes only, never content",
	Levels:      audit.LevelTenant,
	Required:    []string{"kind", "reason", "from", "to", "outcome"},
})

// Recorder records a transition. A recorder must not retain or log more than the Transition holds.
type Recorder interface {
	Record(ctx context.Context, scope contracts.Scope, t Transition) error
}

// AuditRecorder appends transitions to the audit log of the scope's tenant.
type AuditRecorder struct{ Log audit.Appender }

// Record implements Recorder.
func (a AuditRecorder) Record(ctx context.Context, scope contracts.Scope, t Transition) error {
	loc, err := audit.ForScope(scope)
	if err != nil {
		return err
	}
	_, err = EventTransition.Append(ctx, a.Log, loc, t)
	return err
}
