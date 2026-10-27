# Private self-hosted LLM endpoints

Status: written for the 1.4.0 additive wave ([ADR 0045](../decisions/0045-private-self-hosted-llm-endpoints.md)).

A **private LLM** is an OpenAI-compatible model server the deployment owner runs (vLLM, LocalAI, or similar). Prompt and reply data stay on infrastructure the host operates; AlixAI does not hold cloud credentials for provisioning.

## Attachment points

| Kind | Who sets it | Effect |
| --- | --- | --- |
| **Tenant pin** | Deployment operator, or a tenant admin of that tenant with `ops.private_llm.write` | Inside that tenant, every bindable role the endpoint serves uses it. Personal external catalog bindings are overridden for those roles. |
| **Principal private binding** | Deployment operator (`PUT /v1/ops/principals/{id}/private-llm`) or the principal (`PUT /v1/me/llm/{role}` with `private_endpoint_id`) | Follows the person into every tenant. In a pinned tenant the pin wins. |

Resolution per role (External mode only): tenant pin → principal private binding → principal external catalog binding (ADR 0041) → deployment default → `llm_not_configured`.

## Lifecycle

`provisioning` → `warming` → `ready` (or `failed` / `stopped` / `draining`).

| State | Turn outcome |
| --- | --- |
| `provisioning`, `warming` | `unavailable` with `details.reason` `private_llm_starting` — no external provider call |
| `failed`, `stopped`, `draining` | `private_llm_unavailable` — no fallback to a third-party provider |
| `ready` | Serves through the `self_hosted` adapter |

The host provisioner sets lifecycle on register/update. AlixAI health probes (health URL, or `{origin}/v1/models`) may move `ready` ↔ `failed`.

## Operator routes

Capabilities: `ops.private_llm.read`, `ops.private_llm.write`.

- `PUT|GET|DELETE /v1/ops/private-llm/endpoints/{endpoint_id}`
- `PUT|GET|DELETE /v1/ops/tenants/{tenant_id}/private-llm`
- `PUT|GET|DELETE /v1/ops/principals/{principal_id}/private-llm`

Endpoint body: `origin`, `api_dialect: openai-compatible`, `model_ids`, `roles`, optional `cred_ref`, `owner_kind` (`principal` \| `tenant` \| `shared`), optional `health_url`, optional internal prices.

## Security

- Egress classifies the destination as **self_hosted**: classes up to `confidential` may leave when the synthesized profile allows them; `restricted` never leaves. Operators still list `self_hosted` (and model ids) on egress allowlists.
- Ownership: a principal-owned endpoint cannot be used by another principal; a tenant-owned endpoint only in that tenant; shared endpoints keep per-tenant context isolation (scope on every request).
- Secrets stay in the credentials service. Erasure removes bindings and pins; the endpoint holds no AlixAI conversational state.
- Never a silent fallback to OpenAI (or any catalog provider) when a pin or private binding is active and the endpoint is not ready.

## Smooth replies

Set `dialogue.stream_verified: true` (ADR 0046) so verified sentences reach the client as the private endpoint streams. Default is **false** (1.3 whole-reply behaviour). Example:

```yaml
dialogue:
  stream_verified: true
```

## Host responsibilities

1. Provision and run the model server ([deploy/private-llm](../../deploy/private-llm/README.md) kit / Performance compose).
2. Register the endpoint and set lifecycle as it warms.
3. Allowlist `self_hosted` and the model ids in egress policy; set budgets if you want spend caps (cost defaults to zero unless you set internal prices). Shared multi-tenant processes should send a per-tenant `cache_salt` (see Performance [hardening](../../deploy/container/performance/hardening.md)).
4. Pin tenants and/or activate principal bindings for the paid feature.
5. Mode External at the levels that should use the private path.

## Related

- [ADR 0045](../decisions/0045-private-self-hosted-llm-endpoints.md)
- [Per-principal LLM accounts](../decisions/0041-per-principal-llm-accounts.md)
- [Light integration](../integration/guides/light-integration.md)
- [External mode](external-mode.md)
