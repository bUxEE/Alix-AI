// Package actions is the prepare / confirm / commit coordinator: the heart of
// master plan rule 8 ("consequential actions are validated and prepared before
// confirmation and execution") and of the safety levels of 11.4. It is module
// "actions" of the store (ADR 0006).
//
// The model proposes; this package decides. A TaskProposal is a set of
// candidate values. Everything authoritative in a PreparedAction or an
// ActionReceipt (the preparation ID, idempotency key, state, expiry, risk level,
// confirmation requirement, receipt status, audit reference) is minted or decided
// here, and everything the host knows (summary, effects, data versions, record
// IDs, canonical facts) is copied from the host's typed results. Neither has a
// path from model output.
//
// # Operations
//
//	Read       level 0: policy allows, the host reads. No preparation.
//	Prepare    policy, then the host validates and stages; a PreparedAction is stored.
//	Confirm    consumes the single-use confirmation: prepared -> confirmed.
//	Commit     the only path to the adapter's Commit; returns the receipt.
//	Reject     the user declines: prepared | confirmed -> rejected.
//	Invalidate the session or event layer voids a preparation.
//	Reconcile  asks Status again about an uncertain outcome.
//	Recover    resolves rows left in committing by a dead process (service path).
//	ExpireDue  marks lapsed preparations expired (service path).
//
// Confirm and Commit are separate steps. A confirmation is used exactly once:
// the second presentation is replayed and reaches no adapter. A commit is
// idempotent: once a preparation has an outcome, every further Commit returns
// the same receipt and sends nothing.
//
// # States
//
//	prepared -> confirmed -> committing -> committed | rejected | invalidated | uncertain
//	prepared ------------------^           (mode none: levels 0 and 1 without a
//	prepared | confirmed -> rejected | expired | invalidated   confirmation rule)
//
// committing may fall back to the state it came from when the host proves the
// commit was never applied (the same idempotency key is then used again). A
// host result of queued or partial leaves the preparation committed; the receipt
// carries the distinction. Terminal states are never left.
//
// # Policy, twice
//
// The policy gateway decides at prepare and again at commit. Both decisions
// are bound to the application, tenant and workspace of the stored session or
// preparation, not copied from the presented scope, and the presented scope must
// be the same authenticated context (identity.CheckContinuity: a session
// cannot change principal, tenant, workspace or session). The commit decision
// carries the Evidence obtained: Confirmed only for a confirmed preparation,
// StepUp never (the AI path cannot complete a step-up), and the zero Evidence is
// a denial. The package is resolved again at commit, so a quarantine between
// prepare and commit denies. If the policy now assigns a higher risk level than
// the preparation was made with, the preparation is invalidated.
//
// The effective risk level is the highest of the policy's (platform floor of the
// effect class, declared level, host overrides and rules), the level the host
// declares for this preparation and 2 when any effect is external. Level 2 and
// above need an explicit confirmation; level 3 needs a step-up the AI path
// cannot give, so Confirm and Commit return step_up_required and nothing reaches
// the adapter. The requirement is stored with the preparation and enforced at
// commit in addition to the policy's, because the host may raise a level at
// prepare beyond what the policy computes from the declaration.
//
// # Confirmation
//
// Prepare returns a ConfirmationToken for an action that needs a confirmation:
// 256 random bits that print and marshal as "[redacted]" and are meant for the
// client UI only, never for the model. Only HMAC-SHA256(token, preparation ID,
// principal, session, data-version digest) is stored, so the token verifies only
// for exactly that preparation, principal, session and set of data versions and
// a copy of the database cannot confirm anything. Another principal, another
// session or another preparation is refused and does not use the token up.
//
// # Commit
//
// Nothing reaches the adapter's Commit unless, in order: the preparation exists
// in the caller's scope and belongs to the caller's principal and session; it
// is unexpired; policy allows; the confirmation its level requires is present;
// the host, asked to Validate the stored fields again, still reports the data
// version stored at prepare (SEC-042); and the committing state has been
// written (SEC-087, below).
//
// The adapter is never called inside a store transaction: the store re-runs a
// transaction on a busy database, which would send the commit twice.
//
// # Write-ahead and crash safety (SEC-087, SEC-046)
//
// The committing state, the idempotency key it will be sent with and the
// "committing" audit event are written in ONE transaction, and the adapter is
// called only after that transaction committed. If the audit append fails the
// transaction rolls back, the state never became committing and the adapter is
// not called: a consequential commit does not happen without its audit record.
// A row found in committing after a restart is resolved by Recover, which asks
// the host's Status and never commits again.
//
// After the adapter was called the coordinator works on a context detached from
// the caller's, with its own deadline, so a client that disconnects cannot strand
// a dispatched commit. The outcome (audit event, receipt, state) is persisted in
// one transaction before it is announced; if that fails, nothing is announced and
// the row stays committing for Recover.
//
// # What a failed or unclear commit becomes (SEC-041, SEC-047)
//
//	unavailable, resource_exhausted, rate_limited       never delivered: back to where it was
//	scope or validation refusal before sending          not applied: rejected
//	host result rejected (code stale: invalidated)      not applied: rejected
//	host result ok, queued, partial, for this action    committed (queued, partial receipts)
//	anything else, or a result about other records      ask Status
//
//	Status no-record (adapters.StatusShowsNoRecord)     proven not applied: back to where it was
//	Status ok, queued, partial, for this action         the recorded outcome
//	anything else, including an error or a denial       uncertain (uncertain_outcome)
//
// Only the no-record marker proves nothing was applied. A rejection of the
// Status call itself says nothing about the commit. After an uncertain outcome
// nothing is retried; Reconcile asks again. The list of definite failures is
// this package's own and stricter than the adapter wrapper's.
//
// # Binding results to the request (SEC-050)
//
// A DeterministicResult has no echo fields, so the coordinator binds the result
// to what it asked for (bind.go): a Commit or Status result may name only
// records the prepared action targets, and the data version the commit is
// checked against is the one stored at prepare, never one a later reply claims.
// A result that fails the binding is not a decision input; after a commit was
// sent it ends uncertain, never committed.
//
// # Audit
//
// Every state transition appends an event of the action.* family
// (internal/audit): prepared, confirmed, committing, committed, rejected,
// expired, invalidated, uncertain. They hold identifiers, codes and states, never
// a field value, an effect or a confirmation secret. Policy decisions are
// recorded by the gateway's own sink.
//
// # Retention and erasure
//
// Finished preparations (committed, rejected, expired, invalidated) are removed with
// their receipts by EraseFinished, for a tenant, a principal or past a retention
// cutoff, and the removal is an audit event in the same transaction. A pending or
// uncertain preparation, its receipts and the audit events that name it
// (PendingGuard) are never removed: a request that touches one is refused whole.
// See ADR-0036.
//
// # What is not here
//
// Nothing in this package calls a model, opens a network connection, reads the
// environment or reaches a concrete adapter: the adapter and the package come
// from the AdapterSource and PackageSource the composition root provides.
package actions
