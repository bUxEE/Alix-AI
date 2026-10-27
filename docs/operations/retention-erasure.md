# Retention and erasure

Status: written for 1.5.0. Design: [ADR 0036](../decisions/0036-audit-retention-and-erasure.md), [ADR 0041](../decisions/0041-per-principal-llm-accounts.md). Production checklist: [production.md](production.md) §8.

**Fixed (1.5.0):** workspace-level erasure (`level: workspace`) no longer removes other workspaces' conversation history or memory — it is scoped to the named `workspace_id`.

## What erasure removes

On request (`alixai retention erase` or `POST /v1/ops/erasure`), AlixAI removes personal data of a **tenant**, **workspace**, or **principal** in one application:

| Kind | What goes |
| --- | --- |
| Audit payloads | Replaced by a signed tombstone; envelope and chain hashes stay |
| Finished preparations and receipts | Removed when no pending/uncertain action depends on them |
| Conversation history | Transcript turns, session windows, session rows (active sessions ended first) |
| Long-term memory | Memory items of the target |
| Files | File rows of the target (including generated exports); best-effort |
| Principal LLM credentials/bindings | **Only** on application-wide principal erasure (see below) |

The audit chain still verifies after erasure. RAG source registrations use their own deletion path and are not removed here. Backups and the host application's own records are out of scope (ADR 0036).

## Two principal erasures (ADR 0041)

Principal-owned LLM credentials and bindings belong to the **person across every tenant**. A tenant administrator of company A must not wipe credentials that also serve company B.

| Kind | Who | What |
| --- | --- | --- |
| **Tenant-scoped** | Tenant admin of that tenant, or deployment operator | Principal's data **inside that tenant only** (sessions, history, memory, files, audit payloads). Never credentials/bindings. |
| **Application-wide (GDPR)** | Deployment operator only | Principal's data in **every tenant** of the application, then revoke principal-owned credentials (at the provider where supported) and remove bindings. |

## CLI

```text
# Whole tenant
alixai retention erase -application APP -tenant TENANT -reason "tenant erasure request" -operator OPERATOR

# GDPR: erase this person everywhere in the application (+ credentials)
alixai retention erase -application APP -tenant TENANT -principal PRINCIPAL -reason "principal erasure request" -operator OPERATOR

# Tenant-local only (no credentials): e.g. user left one company
alixai retention erase -application APP -tenant TENANT -principal PRINCIPAL -tenant-scoped -reason "left tenant" -operator OPERATOR
```

`-principal` without `-tenant-scoped` is application-wide (the `-tenant` value is still required for the command and the report; every tenant that holds the principal's data is erased). `-dry-run` counts without writing. After a real run, `alixai audit verify-chain` must still pass (`chains_failed` = 0).

## HTTP (1.3.0)

Deployment operators and tenant administrators (same lists as the providers area: `external_providers.deployment_operators` / `tenant_admins`) may start erasure over `/v1`. Application-wide principal erasure is **deployment operator only**.

```http
# Tenant-scoped (default when scope is omitted): tenant admin OK
POST /v1/ops/erasure
{ "level": "principal", "principal_id": "usr_alice", "reason": "left company A" }
→ 202 { "job_id": "erj_…", "status": "queued" }

# GDPR application-wide: deployment operator only
POST /v1/ops/erasure
{ "level": "principal", "scope": "application", "principal_id": "usr_alice", "reason": "gdpr request" }
→ 202 { "job_id": "erj_…", "status": "queued" }

GET /v1/ops/erasure/{job_id}
→ { "job_id", "status": "queued|running|completed|failed", "scope", "counts": { … }, "finished_at": … }
```

Levels: `principal` (requires `principal_id`; optional `scope` `tenant`|`application`|`workspace`), `tenant`, `workspace` (requires `workspace_id`, defaults to the token's workspace). With `level: principal` and `scope: workspace`, only that principal's data in the named workspace is erased (credentials/bindings untouched). Idempotent: a second request for the same target while one runs returns the running job. The implementation is shared with the CLI.

SDKs (server-side, operator token): TypeScript `client.ops.erasure.start` / `status`; Python `client.ops.start_erasure` / `erasure_status`; Go `client.Ops().StartErasure` / `ErasureStatus`.
