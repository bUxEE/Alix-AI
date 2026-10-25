package decision

import "github.com/buxee/alix-ai/internal/audit"

// The decision layer's audit events. Payloads hold identifiers, codes, counts and
// numbers only: never a candidate label or text, never bounded state, never an
// utterance. All are tenant-chain events appended with audit.ForScope.

type selectedAudit struct {
	CandidateID string `json:"candidate_id"`
	// Per-mille integers: the audit log refuses non-integer JSON numbers.
	ScorePermille      int `json:"score_permille"`
	ConfidencePermille int `json:"confidence_permille"`
}

// InvokedAudit is the data of decision.invoked: the scorer is about to be called (or
// its cached answer used).
type InvokedAudit struct {
	TraceID         string `json:"trace_id"`
	DecisionType    string `json:"decision_type"`
	ProviderID      string `json:"provider_id"`
	ModelID         string `json:"model_id"`
	ModelRevision   string `json:"model_revision"`
	ContractVersion string `json:"contract_version"`
	Candidates      int    `json:"candidates"`
	CacheStatus     string `json:"cache_status"`
}

// AcceptedAudit is the data of decision.accepted: the result passed every gate. It is
// advisory; what was finally done is a decision.final_action event.
type AcceptedAudit struct {
	TraceID         string          `json:"trace_id"`
	DecisionType    string          `json:"decision_type"`
	ProviderID      string          `json:"provider_id"`
	ModelID         string          `json:"model_id"`
	ModelRevision   string          `json:"model_revision"`
	ContractVersion string          `json:"contract_version"`
	CacheStatus     string          `json:"cache_status"`
	LatencyMS       int64           `json:"latency_ms"`
	Selected        []selectedAudit `json:"selected"`
}

// FallbackAudit is the data of decision.fallback: no scorer result was accepted, and
// why. Invoked says whether the provider was reached.
type FallbackAudit struct {
	TraceID         string `json:"trace_id"`
	DecisionType    string `json:"decision_type"`
	Reason          string `json:"reason"`
	RejectCode      string `json:"reject_code,omitempty"`
	Invoked         bool   `json:"invoked"`
	ProviderID      string `json:"provider_id"`
	ModelRevision   string `json:"model_revision,omitempty"`
	ContractVersion string `json:"contract_version"`
	OodStatus       string `json:"ood_status"`
	LatencyMS       int64  `json:"latency_ms"`
}

// FinalAudit is the data of decision.final_action: what the deterministic side did
// with the advice.
type FinalAudit struct {
	TraceID  string   `json:"trace_id"`
	Action   string   `json:"action"`
	Reason   string   `json:"reason,omitempty"`
	Selected []string `json:"selected,omitempty"`
}

var (
	eventInvoked = audit.MustRegister[InvokedAudit](audit.TypeSpec{
		Name:        "decision.invoked",
		Description: "the decision scorer was invoked on a bounded authorised candidate set (ids and counts only)",
		Levels:      audit.LevelTenant,
		Required:    []string{"trace_id", "decision_type", "provider_id"},
	})
	eventAccepted = audit.MustRegister[AcceptedAudit](audit.TypeSpec{
		Name:        "decision.accepted",
		Description: "an advisory scorer result passed every gate; it authorises nothing",
		Levels:      audit.LevelTenant,
		Required:    []string{"trace_id", "decision_type", "selected"},
	})
	eventFallback = audit.MustRegister[FallbackAudit](audit.TypeSpec{
		Name:        "decision.fallback",
		Description: "no scorer result was accepted; the deterministic path continues with a typed fallback reason",
		Levels:      audit.LevelTenant,
		Required:    []string{"trace_id", "decision_type", "reason"},
	})
	eventFinal = audit.MustRegister[FinalAudit](audit.TypeSpec{
		Name:        "decision.final_action",
		Description: "what the deterministic side finally did with the advice (selection resolved, refused, or fallback routing)",
		Levels:      audit.LevelTenant,
		Required:    []string{"trace_id", "action"},
	})
)
