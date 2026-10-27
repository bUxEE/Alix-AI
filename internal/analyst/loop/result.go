package loop

import (
	"github.com/buxee/alix-ai/pkg/contracts"
	an "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"
	"github.com/buxee/alix-ai/pkg/contracts/gen/conversation"
)

// OutcomeKind classifies how the loop ended.
type OutcomeKind string

const (
	OutcomeAnswer   OutcomeKind = "answer"
	OutcomeClarify  OutcomeKind = "clarify"
	OutcomeAct      OutcomeKind = "act"
	OutcomeRefuse   OutcomeKind = "refuse"
	OutcomeFallback OutcomeKind = "fallback" // single-capability path should run instead
)

// Outcome is the loop result handed back to dialogue.Engine.Turn.
type Outcome struct {
	Kind OutcomeKind
	Code contracts.ErrCode
	// Details are optional structured context for KindRefuse (e.g. reason=confidential_value).
	Details map[string]any

	// Answer fields (KindAnswer).
	Text string
	// ModelText is the verified answer with {{ref:…}} tokens still present (ADR 0049).
	// Empty means Text has no refs (or was the template). Persist ModelText into the
	// model window; Text is the resolved form for the client / transcript.
	ModelText        string
	Blocks           []conversation.RichBlock
	CoreMaterialised []bool // parallel to Blocks; set only by the analyst materialiser
	Spans            []an.SourceSpan

	// Clarify fields.
	Clarify *an.StepProposalClarifyStep

	// Act hand-off (never committed inside the loop).
	Act *an.StepProposalActStep

	// Facts collected (for templates / audit).
	Facts FactSet

	// Steps is how many proposals were executed.
	Steps int
	// WritesAttempted is always false inside the loop; tests assert it.
	WritesAttempted bool
	// CapHit names a exceeded cap when Kind is Refuse with analyst_cap_exceeded /
	// resource_exhausted.
	CapHit CapKind
	// Prior is a human-readable step log (no secrets).
	Prior []string
}

// Input is one analyst-loop turn request.
type Input struct {
	TurnID         string
	Utterance      string
	AnalystCapable bool
	ClientCaps     an.ClientCapabilities
	HasClientCaps  bool
	StreamVerified bool
	Language       string // BCP-47; empty → verifier default English
}
