# External mode for integrators

Status: written in task X-24, 2026-10-08. Operators: see `docs/operations/external-mode.md`. Object definitions: `docs/integration/provider-schemas.md`. Design: [External provider mode](../architecture/external-provider-mode.md).

**External mode is optional, off by default, network-dependent and billed by the provider.** Nothing in this page applies to a deployment that has not enabled it.

## 1. What changes in your integration: nothing

The mode (Local, Hybrid, External) is deployment, application and tenant policy set by an operator. It is never a package field and never a request field, so switching it changes none of:

- the application package, its capabilities, schemas, prompts, policies and signatures;
- the `/v1` routes, request and response shapes, error codes, the turn event stream, blocks, citations and the confirmation flow (including the AG-UI mapping in `docs/integration/agui.md`);
- the context token your application issues, scope handling, or anything your adapter implements.

A model, local or hosted, only proposes. A tool-call proposal from an external model becomes a normal `TaskProposal` and goes through the same schema, shortlist, policy, entity and host-reauthorisation path, then prepare, confirm and commit. A provider can never reach your adapter directly, and every external answer is still subject to fact and citation verification. Your application keeps working in Local mode if the operator turns external mode off or the network is down.

## 2. New package fields

All are optional and can only **restrict**. Declaring one never grants anything: the deployment, tenant and workspace policy decide whether data may leave, and the most restrictive setting wins. All are ignored while external mode is off. They are part of the existing schemas under `schemas/application-package/`.

| Where | Member | Meaning |
| --- | --- | --- |
| manifest | `external.forbid` (bool, default false) | `true`: nothing of this application is ever sent to an external provider. The loader refuses a package that sets it and also declares a RAG source with `egress_mode` `allow_excerpt` or `allow_metadata`. |
| RAG source | `egress_mode` | `local_only` (default when absent), `allow_metadata` (titles, ids, status only), `allow_excerpt` (bounded authorised excerpts; needs an explicit `classification`; refused for a restricted source), `deny` (excluded from the AlixAI capability entirely). This is DATA egress, a different thing from the source's `egress` list of network destinations. |
| RAG source | `classification` | `public`, `internal` (default when absent), `confidential` or `restricted`. A restricted source never leaves. |
| capability field | `classification` | same four values; a field above `internal` is listed by the integration audit as data that could leave. |
| capability field | `local_only` (bool, default false) | `true`: the value never leaves the deployment, whatever the policy says, and cannot be placed in a prompt template. |

Guidance: leave sources at the default (`local_only`) unless a document set is safe to show a hosted provider; classify honestly (an unknown class is treated as restricted); mark identifiers, contact data and secrets `local_only`. `alixai audit run` reports what a package declares and what could leave.

## 3. What your host can read and show

All provider routes are operator-only: the caller is an operator named in the deployment's configuration (`external_providers.deployment_operators` as `<application id>/<principal id>`, or `tenant_admins` as `<application id>/<tenant id>/<principal id>`), never an end user and never someone a token merely says is an admin, and never the same principal id signed by another application's issuer. They take the same verified context token as the rest of `/v1`; the scope is the token's, and a path or query cannot name another tenant. The routes are in `schemas/api/paths/providers.yaml` and `oauth.yaml`; when external mode is off or the build is `noexternal` every one answers `unavailable`.

| You want to show | Route | Object (schema in `schemas/providers/`) |
| --- | --- | --- |
| connection state of each provider | `GET /v1/providers` | `connection-status` |
| which provider and model, which data categories, whether RAG excerpts go, stated retention and training terms, upper-bound cost, budget status, fail or fall back | `GET /v1/providers/{provider}/disclosure` | `egress-disclosure` |
| the mode in force and who decided it | `GET /v1/providers/mode` | effective, configured, decided_by, kill_switch, levels |
| spend, quota, warning and exhaustion | `GET /v1/providers/budgets` | `budget`, `budget-state` |
| usage and cost export | `GET /v1/providers/usage/export` | `usage-record`, JSON or CSV |

The disclosure is what plan section 6.3 requires before data leaves; show it when the user is about to use a provider. `connection-status` carries no reference id, fingerprint or scope list; a provider key or token is never returned to any client, and `PUT /v1/providers/{provider}/credential` is write-only.

Setup flows your admin settings can drive: billing acknowledgement (`POST /v1/providers/{provider}/billing-notice/ack`, both confirmations true; the text to show is in section 9 of the operator guide), OAuth (popup or redirect — see below; without an enabled catalog entry `start` answers `validation_failed` with `details.reason` `manual_fallback`), and the manual-key fallback (send it only from your server, never from browser code, and never log it).

### Popup OAuth (1.1.0)

Connecting a provider from a popup must not navigate the host page. Call `start` with `presentation: "popup"` and `opener_origin` set to the exact origin of the host page (scheme://host[:port]). That origin is validated against the operator allowlist `external_providers.oauth.popup_origins` (never `*`) and is used only as the `targetOrigin` of the callback page's `postMessage`. The start response includes `flow_id` and `poll_interval_ms`. Poll `GET /v1/oauth/flows/{flow_id}` as the source of truth: provider login pages often send `Cross-Origin-Opener-Policy`, which nulls `window.opener`. The callback page may post `{ "type": "alixai.oauth.result", "flow_id", "status" }` (schema `oauth-popup-result`) as a fast path only; it never carries a code, state, token or error text. `DELETE /v1/oauth/flows/{flow_id}` cancels a pending flow when the user closes the popup (idempotent).

With `callback: "loopback"`, AlixAI binds an ephemeral `127.0.0.1` listener and completes the exchange itself. `redirect_uri` stays required on the wire for compatibility; send the placeholder `http://127.0.0.1/oauth/loopback`. A connection that stores a provider-issued API key from browser login (for example OpenRouter PKCE) reports `kind: "oauth_issued_key"` so status can say connected via browser login without claiming access/refresh tokens.

## 4. Errors you may see

They use the standard error body and codes of `common/error`; external mode adds reasons, not codes.

| When | Code | Detail |
| --- | --- | --- |
| egress policy denies the request | `permission_denied` | `rule`, `check`, `reason_code`, `decision_id`; no content. The model cannot override it. |
| budget exhausted, quota or concurrency limit, or no budget | `resource_exhausted` | `reason` `budget_exhausted`, `quota_exhausted`, `concurrency_limit` or `no_budget`, plus `local_fallback_available`; no alternate provider is chosen. |
| provider rate limit | `rate_limited` | with `retry_after_ms` |
| provider down or slow | `unavailable` or `timeout` | falls back only if a policy names it, and the hop is recorded |
| network lost after bytes may have been sent on a request that could commit | `uncertain_outcome` | do not retry; reconcile first |
| the evidence needed is in a source that may not leave | knowledge answer `no_supported_evidence` | reason `egress_restricted`; nothing was sent |
| invalid credential or unqualified profile | `permission_denied` (reason `credential_invalid`, `credential_missing`, `not_qualified`) | never retried on another provider |
| stale or quarantined profile | `quarantined` | `details.reason` is the quarantine reason; never retried on another provider |

## 5. SDK usage

The generated provider types exist: Go `github.com/buxee/alix-ai/pkg/contracts/gen/providers`, TypeScript `sdk/typescript/src/gen/providers`. **The SDKs have no helpers for these routes yet** (`sdk/typescript/src/providers/index.ts` is empty and the route set is not in the TypeScript operation table; the helpers are task X-14). Until then call the routes with your HTTP client and a context token, and decode responses with the generated types, which reject unknown fields and enum values. The existing conversation SDK calls (sessions, turns, confirmations, streams) are unchanged and work in every mode.

## 6. Testing

Never test against a real provider. The repository tests use recorded fixtures and a local mock server; run the conformance tool with `go run ./tools/providerconf run` (recorded fixtures, no network) and `go run ./tools/providerconf check report.json`. A live run spends real money and is a separate, human-gated step.
