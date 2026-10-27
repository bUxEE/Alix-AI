# 0045 — Private self-hosted LLM endpoints

- **Status:** accepted
- **Date:** 2026-10-27
- **Decided in:** release 1.4.0

## Context

Hosts that run their own model servers (vLLM, LocalAI, or any OpenAI-compatible chat
completions endpoint) want AlixAI to send turns there so prompt and reply data never reach a
third-party provider. Two attachment points are required: a **per-principal** private endpoint
that follows the person into every tenant, and a **per-tenant pin** that forces every principal
inside that tenant onto one endpoint (data-residency for that company's data). ADR 0041 external
catalog bindings stay; resolution must prefer the pin, then private or external principal
bindings, then the deployment default, then typed `llm_not_configured`. When a pin or private
binding exists and the endpoint is not ready there is **no** silent fallback to a hosted
provider — only typed `private_llm_starting` or `private_llm_unavailable`. Contracts stay
additive on the frozen 1.3 set. AlixAI holds no cloud credentials for provisioning; the host
operates the endpoint with the 1.4 deployment kit.

## Options

### Attachment points

- **A. Per-principal only.** Rejected: cannot guarantee residency for a whole tenant.
- **B. Per-tenant only.** Rejected: loses the personal private-LLM product.
- **C. Both: tenant pin overrides personal bindings inside that tenant; outside it the
  principal's private or external binding applies.** Taken.

### Who may pin a tenant

- **A. Deployment operator only.** Incomplete for hosts that delegate tenancy.
- **B. Deployment operator, or a tenant admin of that tenant with capability
  `ops.private_llm.write`.** Taken. Documented in `docs/operations/private-llm.md`.

### Egress classification

- **A. Treat the private endpoint like any external provider (third-party allowlists only).**
  Rejected: confidential would need the same third-party path as OpenAI.
- **B. Classify the destination as `self_hosted` (provider id `self_hosted`): data classes up to
  `confidential` may leave when the profile allows them; `restricted` never leaves; no silent
  path to a third-party provider when a pin or private binding is active.** Taken. The endpoint
  is operated by the deployment owner (in-deployment), so it is not third-party egress.

### Lifecycle and fallback

- **A. Soft-skip to the catalog when warming/failed.** Forbidden.
- **B. Lifecycle `provisioning|warming|ready|draining|stopped|failed`; health checks drive
  `ready`/`failed`; warming/provisioning → `private_llm_starting`, failed/stopped/draining →
  `private_llm_unavailable`; no external call.** Taken.

### Auth to the endpoint

Credential reference (bearer) required when the host sets one; mTLS remains optional at the
network layer (egress dialer / TLS config). Empty cred_ref is allowed for open loopback
mocks only when the operator records it that way.

## Decision

Option C for attachment, B for pinning authority, B for egress, B for lifecycle. Operator
registers endpoints (`/v1/ops/private-llm/endpoints/{endpoint_id}`), pins tenants
(`…/tenants/{tenant_id}/private-llm`), and activates principal private bindings
(`…/principals/{principal_id}/private-llm`). Principals may also bind via
`/v1/me/llm/{role}` with `private_endpoint_id` (mutually exclusive with catalog `profile_id`)
when the endpoint's owner permits them. Erasure removes bindings and pins; the endpoint holds
no AlixAI state. Metering records tokens; cost is zero unless the operator sets an internal
price. Budgets still apply when configured.

Resolution per role and turn (External mode):

1. Tenant pin for the request tenant
2. Principal private binding, else principal external catalog binding (ADR 0041 / 0043)
3. Deployment default route
4. Typed `llm_not_configured`

## Consequences

Hosts can offer private LLMs without third-party egress for pinned tenants or entitled users.
Cross-principal use of a principal-owned endpoint is refused. Shared endpoints keep per-tenant
context isolation (scope on every request). Local mode and Hybrid stay unchanged for private
resolution (private bindings apply only when effective mode is External, same gate as ADR 0041).

## Threat model (notes)

| Threat | Mitigation |
| --- | --- |
| Silent fallback to OpenAI when private endpoint down | Typed reasons only; no soft-skip |
| Cross-principal use of someone's endpoint | Owner check on bind and on resolve |
| Secret in responses/logs | cred_ref only; redaction; never echo |
| Restricted data to private URL | Restricted still never leaves |
| Host cloud credentials in AlixAI | Not stored; provisioning is the host's job |

## Reversal plan

Stop wiring `PrivatePolicy` / private-LLM gateway area; routes answer `unavailable`. Clear
pins and private bindings; endpoint rows unused. No frozen schema to restore beyond freezing
the additive fields already recorded.
