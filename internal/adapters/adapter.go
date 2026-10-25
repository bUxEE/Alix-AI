package adapters

import (
	"context"

	"github.com/buxee/alix-ai/pkg/contracts"
	"github.com/buxee/alix-ai/pkg/contracts/gen/actions"
)

// Operation names one call of the AppAdapter contract. The six operations that
// answer with a DeterministicResult use the same spelling as the schema's
// operation enum.
type Operation string

const (
	OpDescribe Operation = "describe"
	OpRead     Operation = "read"
	OpValidate Operation = "validate"
	OpPrepare  Operation = "prepare"
	OpCommit   Operation = "commit"
	OpStatus   Operation = "status"
	OpCancel   Operation = "cancel"
	OpHealth   Operation = "health"
	OpInspect  Operation = "inspect"
)

// ResultOperations returns the operations that return a DeterministicResult.
func ResultOperations() []Operation {
	return []Operation{OpRead, OpValidate, OpPrepare, OpCommit, OpStatus, OpCancel}
}

// AllOperations lists every operation of the contract.
func AllOperations() []Operation {
	return []Operation{OpDescribe, OpRead, OpValidate, OpPrepare, OpCommit, OpStatus, OpCancel, OpHealth, OpInspect}
}

// Valid reports whether o is one of the nine operations.
func (o Operation) Valid() bool {
	for _, v := range AllOperations() {
		if o == v {
			return true
		}
	}
	return false
}

// Result returns the DeterministicResult operation that o produces, and false
// for describe, health and inspect, which do not produce one.
func (o Operation) Result() (actions.DeterministicResultOperation, bool) {
	for _, v := range ResultOperations() {
		if o == v {
			return actions.DeterministicResultOperation(o), true
		}
	}
	return "", false
}

// String returns the wire spelling.
func (o Operation) String() string { return string(o) }

// Identity is the minimal identity context that crosses to the host
// application (master plan 10.1: "AlixAI passes the minimum identity context to
// the application adapter. The application reauthorizes every read, prepare and
// commit"). It is the only part of a contracts.Scope a transport may put on the
// wire: the deployment ID and the capability scopes of the context token are
// AlixAI's own business and the host decides access from the principal, roles,
// tenant and workspace it receives.
type Identity struct {
	ApplicationID string   `json:"application_id"`
	TenantID      string   `json:"tenant_id"`
	WorkspaceID   string   `json:"workspace_id"`
	PrincipalID   string   `json:"principal_id"`
	Roles         []string `json:"roles"`
	SessionID     string   `json:"session_id"`
}

// IdentityFrom extracts the Identity of a scope. It copies the roles. It does
// not validate: the Guard has already refused an invalid scope.
func IdentityFrom(s contracts.Scope) Identity {
	return Identity{
		ApplicationID: s.ApplicationID,
		TenantID:      s.TenantID,
		WorkspaceID:   s.WorkspaceID,
		PrincipalID:   s.PrincipalID,
		Roles:         append([]string(nil), s.Roles...),
		SessionID:     s.SessionID,
	}
}

// ReadRequest asks the host for a deterministic read (capability risk level 0).
type ReadRequest struct {
	CapabilityID string
	// Fields are the capability's field values after the core validated and
	// normalised them: JSON scalars or flat lists of scalars, keyed by field
	// name. The host validates them again; nothing here is trusted by the host.
	Fields map[string]any
}

// ValidateRequest asks the host to check candidate field values without
// staging anything.
type ValidateRequest struct {
	CapabilityID string
	Fields       map[string]any
}

// PrepareRequest asks the host to validate and stage a consequential action.
type PrepareRequest struct {
	CapabilityID string
	Fields       map[string]any
}

// CommitRequest asks the host to execute a prepared action. Nothing reaches
// Commit unless a PreparedAction exists (master plan 3 rule 8); the host gets it
// whole so it can re-check the data versions it validated against, and the
// idempotency key so a repeat does not apply the effect twice.
type CommitRequest struct {
	CapabilityID string
	// IdempotencyKey must equal Prepared.IdempotencyKey.
	IdempotencyKey actions.IdempotencyKey
	Prepared       actions.PreparedAction
}

// StatusRequest asks what became of the commit sent with an idempotency key.
// It is how an uncertain commit is resolved before anything is retried.
type StatusRequest struct {
	CapabilityID   string
	IdempotencyKey actions.IdempotencyKey
}

// CancelRequest withdraws work the host holds for an idempotency key (a staged
// preparation or queued commit) that has not taken effect. It never reverses an
// effect that was applied: that is a capability of its own.
type CancelRequest struct {
	CapabilityID   string
	IdempotencyKey actions.IdempotencyKey
}

// AppAdapter is the one interface every application transport (MCP, OpenAPI,
// native) implements, so the rest of the core never cares which one is in use
// (master plan 9.3: transports are not the policy layer). It is not a policy
// layer: authorisation, risk, confirmation and idempotency decisions belong to
// internal/policy and internal/actions, and the host reauthorises every request.
//
// # Contract for implementers
//
// Callers do not call a transport directly. They call Wrap(transport, ...),
// which validates the scope, bounds time and concurrency, re-validates every
// result and maps ambiguous commit failures to uncertain_outcome. An
// implementation therefore does these things and no more:
//
//   - send only Identity (IdentityFrom(scope)) and the request fields;
//   - never retry: a retry of Commit is the coordinator's decision, taken after
//     Status;
//   - read replies through ReadLimited and turn them into results with
//     Normalise, so anything that is not a DeterministicResult is a
//     schema_invalid error;
//   - honour ctx: return promptly once it is done;
//   - report a failure that happened before the request left the process or
//     proved that nothing was delivered (connection refused) as unavailable, and
//     one that happened after the request may have been delivered (reset, timeout
//     or garbled reply to a Commit) as uncertain_outcome via UncertainOutcome;
//   - report what the host said as a DeterministicResult, and what the adapter
//     itself found out (unreachable, timeout, malformed reply) as a typed error.
//     A host refusal is a result with status rejected, never an error.
//
// All methods are safe for concurrent use.
type AppAdapter interface {
	// Describe reports what the adapter and the application it fronts speak and
	// expose. It is how the contract version is negotiated and how the
	// integration audit compares the declared control surface with the real one.
	Describe(ctx context.Context) (Description, error)

	// Read performs a deterministic read.
	Read(ctx context.Context, scope contracts.Scope, req ReadRequest) (actions.DeterministicResult, error)
	// Validate checks candidate field values without staging anything.
	Validate(ctx context.Context, scope contracts.Scope, req ValidateRequest) (actions.DeterministicResult, error)
	// Prepare validates and stages an action. A result with status ok carries
	// the preparation: summary, exact effects, data versions and declared risk.
	Prepare(ctx context.Context, scope contracts.Scope, req PrepareRequest) (actions.DeterministicResult, error)
	// Commit executes a prepared action, exactly once for one idempotency key.
	Commit(ctx context.Context, scope contracts.Scope, req CommitRequest) (actions.DeterministicResult, error)
	// Status reports what the host recorded for an idempotency key. Status ok,
	// queued or partial mean the host recorded that outcome (identifiers and
	// values are those of the commit reply). The only result that proves
	// nothing was applied for the key is the no-record marker, rejected with
	// one validation_failed error on field idempotency_key (see
	// StatusShowsNoRecord); it is also the only thing that makes retrying the
	// Commit safe. Every other result leaves the outcome unknown and nothing may
	// be retried: a rejection for any other reason (a reauthorisation denial or
	// a scope error on the Status call itself says nothing about the commit),
	// uncertain, and unavailable.
	Status(ctx context.Context, scope contracts.Scope, req StatusRequest) (actions.DeterministicResult, error)
	// Cancel withdraws pending work for an idempotency key.
	Cancel(ctx context.Context, scope contracts.Scope, req CancelRequest) (actions.DeterministicResult, error)

	// Health probes the application. A returned error means the probe itself
	// failed; a report with status unavailable means the probe ran and the
	// application is down.
	Health(ctx context.Context) (HealthReport, error)
	// Inspect returns what the application exposes for the runtime and system
	// audit (master plan 4.5). Anything not exposed is reported as unsupported
	// and is therefore unverified; AlixAI does not look for it by itself.
	Inspect(ctx context.Context, scope contracts.Scope, req InspectRequest) (Inspection, error)
}
