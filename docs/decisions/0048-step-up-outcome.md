# 0048 — Host reports step-up outcome into the session

- **Status:** accepted
- **Date:** 2026-10-27
- **Decided in:** release 1.5.0

## Context

Level 3 capabilities use confirmation mode `step_up` (ADR 0006). The AI path
cannot complete them: Confirm and Commit return `step_up_required` and never
reach the adapter's Commit. The effect happens only when the user finishes the
step-up in the **application's own authenticated interface**.

That interface includes a dialog the host opens from its embedded chat panel —
it is still the host's authenticated UI, not AlixAI's confirm path. Today the
host can only tell AlixAI that something changed via `POST /v1/events`, which
**invalidates** preparations (sessions go stale). The assistant never learns
"the step-up is done" as a durable `ActionReceipt`, so the open turn stream /
AG-UI cannot show a receipt and the next turn has no committed outcome.

## Options

### How the host reports completion

- **A. Overload `POST /v1/events` with a new kind that commits.** Rejected:
  events are invalidate-only by contract (ADR 0006 / sessions.ApplyEvent); mixing
  commit into that path would break the guarantee.
- **B. Let the AI Confirm path accept a host-signed proof.** Rejected: Confirm
  stays reserved for MAC tokens of mode `explicit`; level 3 must never be
  completable on the AI path.
- **C. New host-backend route that records an outcome after verifying Status.**
  Taken. `POST /v1/sessions/{session_id}/preparations/{preparation_id}/step-up-outcome`
  with a context token of the **same principal and session**, body
  `{outcome: completed|declined|failed, reference?}`.

### Trust model

- **A. Trust the body.** Forbidden: a forged or premature `completed` would
  invent a receipt; a forged `declined` would hide a real effect.
- **B. Verify with the adapter's Status for the preparation's idempotency key.**
  Taken for every outcome. `completed` is accepted only when Status shows the
  effect (same binding as commit recovery). No effect → typed refusal
  (`step_up_effect_missing`). `declined` / `failed` are refused with
  `step_up_effect_present` when Status shows the effect (the host must report
  `completed`); when Status has no record or is undecided/unavailable they
  record a rejected receipt with provenance that the step-up ended in the
  host's interface.

## Decision

Add the session-scoped step-up-outcome route (C) with Status verification (B).

Rules:

1. Preparation must belong to this session, mode `step_up`, state `prepared`
   (or already terminal with a matching receipt — idempotent replay).
2. Expiry: allowed until `expires_at` plus a fixed grace window (24 h); beyond
   that → `expired`. Non-`step_up` preparations → `validation_failed`.
3. Other principal or session → continuity / `scope_violation` (same as Confirm).
4. On a recorded receipt: durable `ActionReceipt` (committed or rejected), audit
   in the same family as Commit, provenance reason `host_step_up_*`. The session
   store then completes the active goal that still references the preparation
   the same way an in-turn commit does (`sessions.ApplyStepUpReceipt`):
   committed / queued → goal capsule `success` with `ReceiptID` and a
   `ResultCommit` when records exist; partial → capsule `failure` with receipt;
   declined → capsule `cancelled` with receipt; failed → capsule `failure` with
   receipt. If the goal no longer references that preparation, the receipt still
   stands and goal state is unchanged. Optimistic save, one retry on version
   conflict.
5. **Delivery to the client.** The stream hub indexes turns by turn id
   (`Hub.Lookup`); this route has no turn id without new cross-package coupling,
   so it does **not** publish a mid-stream `receipt` event. The next turn sees
   the completed (or cancelled) goal / capsule and must not re-propose the
   action; the receipt is also readable at
   `GET /v1/actions/{preparation_id}/receipt`. Do not invent a new event type.
6. AlixAI Confirm/Commit of `step_up` stay refused forever.

## Consequences

Hosts can close the loop after their own step-up UI without invalidating the
session. Integrators must call Status-backed completion from the **backend**
with a fresh context token. Packages that apply the effect only in-app (no
Status record) cannot use `outcome: completed`; they keep the invalidate-or-
approve-then-commit designs already documented for step-up capabilities.

## Host linkage after `alixai_continue_in_app` (1.5.0 behaviour)

The AG-UI `alixai_continue_in_app` view **strips the idempotency key on purpose**
(the browser must not see it; see [agui.md](../integration/agui.md) §7). The host
backend recovers the full `PreparedAction` and closes the loop as follows:

1. With a context token of the **same principal and session**, call
   `GET /v1/actions/{preparation_id}` → `PreparedAction` including
   `idempotency_key` and effects.
2. Apply the effect in the host's own authenticated UI under that
   `idempotency_key`. The host may link its own staged payload via a **data
   version** it returned from Prepare — data versions round-trip exactly.
3. Its Status operation answers:
   - `ok` | `queued` | `partial` with identifiers inside the prepared effects
     when the effect is done; or
   - `rejected` with exactly one `validation_failed` error on field
     `idempotency_key` and **no** identifiers when there is no such effect.
4. Then `POST /v1/sessions/{session_id}/preparations/{preparation_id}/step-up-outcome`
   with `{outcome: completed|declined|failed, reference?}` (this ADR).

Sequence:

```text
AG-UI alixai_continue_in_app (no idempotency_key in the browser)
  → host backend GET /v1/actions/{preparation_id}  (context token)
  → PreparedAction { idempotency_key, effects, data_versions… }
  → host UI applies effect under that key (optional staged payload ↔ data version)
  → host Status: ok|queued|partial + identifiers  OR  rejected / validation_failed on idempotency_key
  → host backend POST …/step-up-outcome {outcome}
  → AlixAI Status-checks → ActionReceipt → goal completed / cancelled → next turn sees it
```

## Reversal plan

Remove the route and `RecordStepUpOutcome` / `ApplyStepUpReceipt`; leave
existing receipts. No schema removals — the addition is additive on the frozen
1.4 OpenAPI set.
