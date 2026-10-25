// Package audit is AlixAI's tamper-evident local audit log (master plan 4.1,
// 16.4 "audit tampering", 19). It is module "audit" of the store.
//
// # Events and chains
//
// An Event is an append-only record: who (principal, session), where
// (application, tenant, workspace), when, what (a registered EventType) and a
// small JSON object of data. Events form hash chains, one chain per
// (application, tenant) pair:
//
//	hash = sha256( canonical(event without hash and prev_hash) || prev_hash )
//
// canonical is trust.CanonicalJSON (RFC 8785 restricted to strings and
// integers) of every stored column: format, id, application_id, tenant_id,
// workspace_id, chain_seq, type, occurred_at_ms, principal_id, session_id and
// data. prev_hash is appended as 64 lower-case hex characters. The first event
// of a chain has prev_hash = GenesisHash(application, tenant), which binds a
// chain to its owner so events cannot be spliced into another tenant's chain.
// The workspace is part of every hash but not of the chain key: a chain spans
// the workspaces of its tenant, and moving an event to another workspace (which
// would defeat export scoping) breaks its hash.
//
// Two levels of chain exist. A tenant chain is keyed by a real application and
// tenant. An application chain has an empty tenant_id and holds events that belong
// to no tenant: package lifecycle, integration-audit runs and, under the
// application trust.PlatformApplicationID, the platform's own events (a policy
// decision for a request that carried no usable scope, for instance). Real
// tenant ids are never empty, so the two cannot collide, and a Locator can only
// be built through ForScope, ForIDs, ForApplication or Platform. Tenant deletion
// is a tombstone event on the tenant's chain (EventDeletionTenant); the chain
// is never cut and no event is ever deleted. Clearing conversation history is
// EventDeletionHistoryCleared: it does not touch audit events.
//
// # Append, and write-ahead use
//
// Log.Append and Log.AppendAt run in their own transaction; a failure is an
// error and nothing is written. Log.AppendTx appends inside the caller's
// transaction and is what makes a write-ahead step real (SEC-087): the action
// coordinator records its "committing" state and the EventActionCommitting
// event in ONE store transaction, commits it, and only then calls the
// adapter's Commit. If the append fails, the transaction rolls back, the state
// never became "committing" and the adapter is not called. Callers must treat
// every error from an append as "do not proceed" (fail closed); there is no
// best-effort mode. The policy gateway already does this with its Sink.
//
// Appends to one chain are serialised by the store's single writer connection
// (and, across processes, by the database write lock): the chain head is read
// and the new event inserted in one transaction, and UNIQUE constraints on
// (application, tenant, chain_seq) and (application, tenant, prev_hash) refuse
// a fork even if that were ever violated.
//
// # Event types
//
// An event type is registered before it can be appended; an unregistered type
// is refused. This package registers the core types (policy decisions, action
// lifecycle, package lifecycle, audit runs, settings changes, deletions).
// Any other package registers its own from its init() without editing this one:
//
//	var EventEgressDecision = audit.MustRegister[EgressDecision](audit.TypeSpec{
//		Name:        "dataegress.decision",
//		Description: "a data-egress decision for an external provider",
//		Required:    []string{"decision_id"},
//	})
//
//	...
//	EventEgressDecision.Append(ctx, log, loc, EgressDecision{...})
//
// Registration governs appending only. Verification never consults the
// registry, so a binary can verify a chain that contains types it does not know.
//
// # Redaction at write time
//
// Before an event is hashed, its data is redacted with the same rules as the
// log redactor (internal/log): values under keys that look like a token,
// secret, password, authorization header, API key, cookie, credential or
// private key are replaced by "[REDACTED]", and bearer tokens, JWTs, PEM
// private keys, URL userinfo and key=value secrets inside strings are scrubbed.
// What reaches the table, an export and a checkpoint is the redacted form; the
// original is never stored. Event data is limited to strings, integers (at most
// 2^53-1), booleans, null, arrays and objects, at most MaxDataBytes and 32 levels
// deep: a float or an oversized payload fails the append (money is integer
// micro-units elsewhere in AlixAI too).
//
// # Verification and signed checkpoints
//
// VerifyChain recomputes every hash from the stored columns and checks that
// positions are contiguous from 1, that each prev_hash is the previous hash,
// that rows were inserted in chain order, and that the head matches the latest
// signed checkpoint. A changed event, a removed or reordered event and a
// spliced event are all reported with the position at which they occur.
//
// A hash chain has no secret, so on its own it cannot see the removal of the
// newest events or a rewrite of the whole chain with fresh hashes (SEC-086).
// A Checkpoint states "this chain had head_seq events and head hash H", signed
// with a platform key from internal/trust (Log.Checkpoint, periodically through
// Options.CheckpointEvery or CheckpointAll from a job). VerifyChain accepts the
// signature only from a key registered for trust.PlatformApplicationID, requires
// the checkpoint to name the chain being verified, and then requires the chain
// to be at least head_seq long with exactly H at that position. The signed body
// repeats created_at and expires_at, so the unauthenticated timestamps of the
// signature record are bound to the signed bytes.
//
// Limits, deliberately stated:
//
//   - Events appended after the newest checkpoint are not protected: someone
//     with write access can truncate or forge them, because the hash chain is
//     unkeyed. The window is Options.CheckpointEvery events (VerifyReport
//     reports EventsAfterAnchor).
//   - Someone with write access can also delete checkpoint rows. The defence is
//     a checkpoint held elsewhere: ExportCheckpoint is meant to travel with
//     integration-audit reports and signed backup manifests, and
//     VerifyOptions.Expected makes VerifyChain require the chain to extend it.
//   - A checkpoint signature expires (trust.SignPayload requires an expiry); the
//     default is ten years. VerifyChain checks the latest stored checkpoint and
//     reports an expired one as a problem rather than ignoring it; Checkpoint
//     signs a fresh one over an unchanged head.
//   - Revoking the signing key makes every checkpoint signed with it fail.
//   - The store runs with synchronous=NORMAL: a committed WAL transaction survives
//     a process crash but the last commits can be lost on power failure. The
//     write-ahead guarantee therefore holds against failures of the append and of
//     the process, not against sudden loss of power on the host.
//
// # Export
//
// Export streams the events of the caller's own application, tenant and
// workspace as JSON lines to an io.Writer. The caller's scope must be valid and
// its token must permit the capability CapabilityExport; an optional
// Options.ExportAuthorizer adds a further check (typically the policy gateway).
// Anything else is permission_denied and nothing is written. Each export is
// itself recorded (EventAuditExported) before any data leaves, and a failed
// append refuses the export. Exports carry redacted data only. Each line holds
// its hashes, so a recipient can check it with Event.HashValid.
//
// # Erasure and retention (ADR-0036)
//
// Nothing is ever deleted from the chain, but a payload can be erased. Log.EraseTx
// replaces the data of an event by the fixed marker ErasedMarker and, in the same
// transaction, stores a signed Tombstone (audit_tombstones): the whole envelope and
// the hash the event had, why, by whom and when, and which audit.erased event of the
// chain records it. The row keeps its position, envelope, hash and prev_hash, so the
// links around it and every checkpoint still match.
//
// VerifyChain cannot recompute the hash of an erased event; it requires instead that
// the row is exactly what its tombstone signed and that the tombstone's signature
// verifies under a platform key (ProblemTombstone...). Whether an event is erased is
// decided by its tombstone row, never by the shape of data. The database enforces the
// same: the append-only trigger lets a payload become the marker only when its
// tombstone exists and nothing else about the row changes.
//
// EraseTx refuses without the signing key (never a payload without its tombstone),
// refuses an event that no longer hashes to what is stored (erasing it would launder
// the tampering), and asks a Guard which events must keep their payload: the package
// that owns a dependency provides it (actions.PendingGuard keeps the events of a
// pending or uncertain action). SelectEvents picks the events of a tenant, of a
// principal or past a retention cutoff. Limits, stated plainly: the envelope keeps
// principal_id and session_id; the kept hash lets someone who can reconstruct a
// low-entropy payload confirm it; tombstones stop verifying if their key is revoked,
// like checkpoints.
//
// # Scope of queries
//
// Every table carries application_id, tenant_id and workspace_id. Reads on
// behalf of a caller (Export) filter on all three through store.ScopedWhere.
// The operator paths address a whole chain, which spans workspaces, and so
// filter on application and tenant only: Append, VerifyChain, Checkpoint,
// ExportCheckpoint and Chains. They are for the service, the CLI and jobs, not
// for a request handler to expose with a caller-supplied chain key.
package audit

import (
	"embed"

	"github.com/buxee/alix-ai/internal/store"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("audit", migrations) }
