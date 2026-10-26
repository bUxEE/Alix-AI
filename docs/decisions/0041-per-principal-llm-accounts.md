# 0041 — Per-principal LLM accounts and the provider catalog

- **Status:** accepted
- **Date:** 2026-10-26
- **Decided in:** release 1.2.0

## Context

Hosts that serve several companies (tenants) for the same signed-in person need the person — not the company — to choose which hosted LLM and which personal API key or OAuth grant the assistant uses. Companies still decide whether their data may leave to that provider (egress policy). The frozen 1.1 `credential-ref.binding.tenant_id` stays required; SIWC already binds a credential to an `owner_principal_id` but Resolve still refuses a different tenant. Local models stay deployment-wide. External-mode invariants (no hidden fallback, no other person's credential, budgets, audit) must not weaken. Contracts for 1.2 are additive only; business logic lands in a later wave.

## Options

### Catalog

- **A. Tenant-configured profiles (1.1 operator surface).** Rejected: companies would configure LLMs; the owner wants the opposite.
- **B. Operator-approved catalog with `credential_mode: per_principal`.** Profiles carry no secret; users may only pick catalog entries. Taken.
- **C. Free-form provider/model strings from the client.** Rejected: no qualification, terms, prices or billing notice.

### Principal binding and resolution

- **A. Bind per tenant.** Rejected: the same person would re-enter keys in every company.
- **B. Bind per (application, principal) for roles `conversational_controller`, `language_realization`, `stt` (TTS reserved); resolution order principal binding → deployment default route → typed `llm_not_configured`.** Never another principal's or tenant's credential; a policy denial never skips to the next profile (same as the 1.1 selection loop). Taken.
- **C. Soft-skip a denied profile and try the next.** Forbidden by I4 / router behaviour.

### Principal-owned credentials while `binding.tenant_id` stays required

- **A. Relax `credential-ref.binding.tenant_id`.** Forbidden: frozen v1.
- **B. Reserved tenant bucket.** Store principal-owned refs with `binding.tenant_id` = the application-scoped sentinel `alixai-principal-credential` (not a company tenant; never used for sessions or RAG). Every such row sets `owner_principal_id`. Resolve (implementation wave) looks up by application + owner + provider and allows any request tenant of that application when the owner matches; the 1.1 SIWC owner check generalises to `api_key`, `oauth` and `oauth_issued_key`. Public surface is a new principal-credential / `/v1/me/…` document family (status only, never the secret). Taken.
- **C. One synthetic personal tenant per principal as the home for keys.** Heavier; forces every host to mint personal tenants for keys alone. Rejected for 1.2.

### Governance and self-service

- Tenant egress still gates which providers may receive that company's data; a mismatch is the typed egress error in that company only.
- Billing notice acknowledged per principal; metering per principal (still reportable per tenant); optional per-principal budget.
- A principal may read/set/delete only their own binding and credential; the key arrives through the host server (never browser→AlixAI); popup OAuth (1.1) binds to the principal via `/v1/me/oauth/{provider}/start`.

## Decision

Option B for catalog, binding/resolution and credentials (reserved tenant bucket + principal document family). Typed refusal reasons include `llm_not_configured` and `provider_not_in_catalog` in `Error.details.reason`. Routes (stubs in 1.2): `GET /v1/providers/catalog`; `GET|PUT|DELETE /v1/me/llm…`; principal credential and billing-ack under `/v1/me/providers/…`; principal OAuth start under `/v1/me/oauth/…`.

## Consequences

Hosts can expose a per-user provider picker without giving tenant admins the user's key. Operators publish catalog profiles and keep company egress policy. Local routing is unchanged. Cross-tenant Resolve for owner-bound credentials is specified here and implemented later; until then routes answer `unavailable`.

## Threat model (notes)

| Threat | Mitigation |
| --- | --- |
| Credential theft from API responses/logs | Write-only submit; redaction; no secret in errors/URLs/postMessage |
| Cross-principal use | Resolve refuses non-owners; self-only `/v1/me` routes |
| Tenant admin reading a user's key | Operator routes never return principal secrets; admin sees connection status only where authorised |
| Prompt injection asking to switch provider | Binding changes only via structured `/v1/me` API, audited |
| Erasure of a principal | Revoke keys, delete bindings (ADR 0036 hooks) |

## Reversal plan

Disable catalog / principal-LLM feature flag; routes stay `unavailable`. Retire principal rows; reserved tenant bucket unused. Hosts fall back to deployment default routes only. No frozen schema to restore.
