# 0042 — Long-term memory (principal × tenant × workspace)

- **Status:** accepted
- **Date:** 2026-10-26
- **Decided in:** release 1.2.0

## Context

Hosts need durable facts the assistant may recall across sessions, separate from turn history. Memory must follow the same scope as sessions and RAG: per principal × tenant × workspace. No silent writes. No core cross-tenant read. Context assembly already fences untrusted data (ADR 0034); retention and erasure follow ADR 0036. CopilotKit / AG-UI compatibility (ADR 0035) must stay intact. 1.2 ships the contract and stubs only.

## Options

### Storage shape

- **A. Fold into history segments.** Rejected: history is a transcript; memory is curated and user-controlled.
- **B. Memory item:** id; scope (application, tenant, workspace, principal); bounded text and optional structured key/value; origin (`user_explicit`, `assistant_proposed`); status (`proposed`, `active`, `rejected`); created/updated; optional expiry; data class (default `internal`). Taken.

### Writes

- **A. Silent model writes.** Forbidden.
- **B. Consent required.** Assistant proposals are a typed TurnEvent (`memory_proposal`); the UI confirms or rejects with `POST /v1/memory/{id}/confirm` or `/reject` (typed text never confirms). The host may `POST /v1/memory` on the user's explicit request. Taken.

### Reads and egress

- Bounded retrieval of **active** items into context assembly, fenced as untrusted data (never instructions), counted in the context budget, egress-guarded by the item's class; local-only when the tenant policy says so.

### AG-UI

- **A. Reuse confirmation interrupt.** Different lifecycle (memory may outlive the turn); rejected as the primary path.
- **B. CUSTOM event `alixai.memory_proposal`** (same pattern as `alixai.directive`), with confirm/reject on the memory HTTP routes; optional later interrupt if a host wants run-blocking UX. Taken for 1.2.

## Decision

Option B everywhere above. Routes: `GET|POST /v1/memory`, `PATCH|DELETE /v1/memory/{id}`, `POST …/confirm` and `…/reject`. Typed reason `memory_proposal_expired` when a proposal can no longer be confirmed. List/search/edit/delete and export-with-history are user control; audit every change; erasure hooks per ADR 0036.

## Consequences

Hosts get a consent-gated memory surface without mixing tenants. Context and egress stay as for other untrusted data. Until the memory service is wired, every route answers `unavailable`.

## Reversal plan

Leave routes registered answering `unavailable`; drop feature flag / Want wiring. Existing items (once stored) export then delete via erasure. Remove the TurnEvent variant only in a major (additive today).
