# 0006 — Prepare, confirm, commit for every consequential action

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-03

## Context
A language model can misread, be talked into things, or be fed hostile text through documents and
tool responses. If its output can directly cause a write, one bad generation is a bad commit. The
host application owns the data and the invariants, so the host must validate and commit; AlixAI
must coordinate that without ever letting the model hold authority.

## Options
- **Execute directly from model output.** Simplest. Violates rules 5, 7 and 8. Rejected.
- **Confirm only (ask the user, then run the proposal).** The user confirms text the model wrote,
  not the exact effects, and nothing guards against stale data between confirmation and run.
- **Prepare / confirm / commit with idempotency and expiry.** The host validates and returns the
  exact effects and the data versions used. The user confirms those. Commit is deterministic and
  idempotent. More round trips and more state to store.

## Decision
Every consequential action is prepared, then confirmed when its level requires it, then committed
by deterministic code through the adapter. The model may propose a `TaskProposal` and verbalize a
result; it cannot create or alter a `PreparedAction`, a confirmation or an `ActionReceipt`.

- `PreparedAction` carries an opaque preparation ID, canonical summary, exact effects and targets,
  risk level, data versions used in validation, expiry and invalidation conditions, confirmation
  requirements and an idempotency key.
- Safety levels (§11.4): 0 read-only, executes after authorization; 1 reversible low-impact,
  concise summary, the application may allow immediate commit; 2 consequential or external side
  effect, prepare, show exact effects, explicit confirmation; 3 high-risk, privileged or legally
  sensitive, authenticated application UI or stronger step-up, the AI cannot complete it alone.
- The host may raise a level; it cannot silently lower a platform-enforced minimum.
- Fail closed: a missing permission, uncertain identity, invalid schema, stale or expired
  preparation, replayed confirmation or unavailable adapter never results in execution.
- Uncertain commit response: query the idempotency key or status before any retry. An interrupted
  action stays uncommitted unless a receipt proves otherwise.
- `ActionReceipt` records committed or rejected, stable record IDs, canonical facts, queued
  follow-ups, an audit reference and any partial-failure state.

The host owns risk-classification overrides (§4.3) and may only raise a level (§11.4). The one
open point is the policy expression format for those overrides (§25 item 8); it does not change
this protocol.

## Consequences
- Easier: the safety argument is local to one component (`internal/actions`) and testable with
  replay, stale-state and uncertain-response cases.
- Easier: the same protocol covers text and voice, since both reach the same pipeline.
- Harder: every consequential host operation needs a prepare and an idempotent commit; hosts that
  cannot provide them fail the integration audit rather than being given a shortcut.
- Harder: preparations and receipts are durable state with expiry and invalidation to maintain.
- Forbidden: a code path from model output to an adapter write that does not pass through prepare;
  a level 3 action completed without the host's authenticated step-up.

## Implementation notes (M1-12, internal/actions)
- Confirm and Commit are separate steps; Commit is idempotent — once an outcome exists every call returns the same receipt and
  sends nothing. Confirmation is a 256-bit single-use token; only an HMAC bound to preparation, principal, session and data-version
  digest is stored. Level 3 gets no token and returns `step_up_required` with zero host calls.
- The write-ahead `committing` state and its audit event are one transaction; the adapter is called only after it commits.
- There is NO automatic re-dispatch, in Commit or Recover. After Status shows no record the preparation returns to its prior state
  with the same idempotency key and Commit returns `unavailable`; the caller retries with a live scope and every check reruns.
  Recovery holds only the stored scope, so it never sends a Commit. Only `adapters.StatusShowsNoRecord` proves nothing was applied;
  every other Status answer (including a denied Status call) ends `uncertain`.
- A Commit or Status result may name only records the prepared action targets, and the data version is the one stored at prepare
  (SEC-050). Receipts form a ledger (Reconcile adds one). "Reopen" audit events reuse `action.confirmed`/`action.prepared` with
  `reopened=true`, so consumers must not count `action.confirmed` as "confirmation consumed".
- Contract requirement on hosts and adapters: a `Validate` on UNCHANGED data must report the version stored from `Prepare`
  (freshness.version is the record snapshot version, not a per-response ETag and not a tenant-wide counter), otherwise every commit
  is stale or every write invalidates all open preparations.

## Implementation notes (M1-14, internal/dialogue)
- Confirmation is a STRUCTURED turn input (`Confirmation{PreparationID, Token}`) supplied by the client UI, never a model reading of
  "yes"; the token appears only in the turn `Result`. A confirmation mode of `none` commits in the same turn after `Prepare` through the
  coordinator. Level 3 returns `request_step_up` and the AI path never completes it.
- A malformed model proposal is not repaired or retried: one provider call, then `schema_invalid`. An explicit unsupported language
  tag fails the turn (`validation_failed`); an empty tag defaults to the package language, else `en`.
- Acts that default to a deterministic template with no model call: refuse, report_failure, request_step_up, acknowledge_cancel,
  report_no_evidence. Text that matches no capability is a router out-of-scope refusal with no model call.
- The `dialogue.turn` audit event fails closed: it is appended before state is saved, and a turn that cannot be audited is not
  acknowledged (if a commit already happened, `Turn` returns the Result with the receipt plus the error).
- The proposal call may be skipped by code only for a decisive, level-0 capability with no required or visible field whose Expose
  decision needs no confirmation.

## Reversal plan
This is a safety invariant, not a tunable. Relaxing it for a risk level requires a superseding ADR
with an updated threat model and security tests. Within the protocol, level 0 and level 1 shortcuts
are configuration of the existing states (no confirmation required), not an exception to them, so
they need no reversal.
