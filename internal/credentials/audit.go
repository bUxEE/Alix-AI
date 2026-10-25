package credentials

import "github.com/buxee/alix-ai/internal/audit"

// useAudit is the data of the audit event of one use of a credential. It names the
// reference (opaque), the provider, why the credential was asked for and the
// outcome. It never carries a value, a fingerprint or anything a provider said.
// The keys avoid the substrings the log and audit redactors blank (token, secret,
// credential, ...), or the trail would hold "[redacted]" in place of the reference.
type useAudit struct {
	Ref      string `json:"ref"`
	Provider string `json:"provider_id"`
	Purpose  string `json:"purpose"`
	Part     string `json:"part"`
	Outcome  string `json:"outcome"` // allowed | denied
	Reason   string `json:"reason"`  // "" when allowed
	Backend  string `json:"backend"`
}

// lifecycleAudit is the data of the audit event of a change to a reference.
type lifecycleAudit struct {
	Ref      string `json:"ref"`
	Provider string `json:"provider_id"`
	Action   string `json:"action"` // created | rotated | revoked | flagged | rejected
	From     string `json:"from_status"`
	To       string `json:"to_status"`
	Reason   string `json:"reason"`
}

// EventUse is the audit event of every use of a credential, allowed or denied
// (additive plan 11.1: credential use is audited without logging secret values).
var EventUse = audit.MustRegister[useAudit](audit.TypeSpec{
	Name:        "credentials.use",
	Description: "a credential reference was resolved to a secret for a provider call, or the attempt was refused; no secret value",
	Required:    []string{"ref", "provider_id", "outcome"},
})

// EventLifecycle is the audit event of a change to a credential reference: created,
// rotated, revoked, flagged (invalid, quarantined, reauthorization required) or a
// manual key that the provider rejected.
var EventLifecycle = audit.MustRegister[lifecycleAudit](audit.TypeSpec{
	Name:        "credentials.lifecycle",
	Description: "a credential reference was created, rotated, revoked or flagged; no secret value",
	Required:    []string{"ref", "action"},
})
