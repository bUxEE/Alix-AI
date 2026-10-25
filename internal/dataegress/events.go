package dataegress

import (
	"github.com/buxee/alix-ai/internal/audit"
	"github.com/buxee/alix-ai/pkg/contracts/gen/providers"
)

// EventDecision is the registered audit event type of an egress decision. The
// payload is the providers.EgressDecision itself: provider, model and version,
// the policy decision, the data categories, the checks that ran. By schema it holds
// no content (every string is an ID, a fixed code or an enum), so it is stored and
// exported as it is. One is appended for every decision, allow or deny, BEFORE the
// filtered request is handed back (additive plan section 6.2 steps 7 and 8); an
// append that fails denies the request.
var EventDecision = audit.MustRegister[providers.EgressDecision](audit.TypeSpec{
	Name:        "dataegress.decision",
	Description: "a data-egress decision (allow or deny) for one request to an external provider: provider, model, outcome, checks and data categories, never content",
	Levels:      audit.LevelTenant,
	Required:    []string{"decision_id", "outcome", "reason", "provider", "categories"},
})

// PolicyChanged is the data of the dataegress.policy_changed event: a policy was
// created or replaced. It says which policy and revision, whether it enables
// external processing and the digest of the canonical document, never more.
type PolicyChanged struct {
	PolicyID         string `json:"policy_id"`
	Level            string `json:"level"`
	Revision         int64  `json:"revision"`
	PreviousRevision int64  `json:"previous_revision"`
	Enabled          bool   `json:"enabled"`
	SourceMode       string `json:"source_mode"`
	SHA256           string `json:"sha256"`
	ChangedBy        string `json:"changed_by"`
}

// EventPolicyChanged is the registered audit event type of a policy write. A
// scope-bound write goes to the tenant chain of the writer's scope, a
// deployment-level write (an operator path) to the platform chain. It is appended in the
// same transaction as the write, so a change that cannot be recorded does not happen.
var EventPolicyChanged = audit.MustRegister[PolicyChanged](audit.TypeSpec{
	Name:        "dataegress.policy_changed",
	Description: "a data-egress policy was created or replaced; carries the level, revisions and the digest of the new document",
	Levels:      audit.LevelTenant | audit.LevelApplication,
	Required:    []string{"policy_id", "level", "revision"},
})

// KillSwitchChanged is the data of the dataegress.kill_switch event.
type KillSwitchChanged struct {
	DeploymentID string `json:"deployment_id"`
	Engaged      bool   `json:"engaged"`
	Reason       string `json:"reason"`
	ChangedBy    string `json:"changed_by"`
}

// EventKillSwitch is the registered audit event type of the global external
// disable switch being engaged or released, on the platform chain.
var EventKillSwitch = audit.MustRegister[KillSwitchChanged](audit.TypeSpec{
	Name:        "dataegress.kill_switch",
	Description: "the global emergency switch that disables every external provider request was engaged or released",
	Levels:      audit.LevelApplication,
	Required:    []string{"deployment_id", "engaged"},
})
