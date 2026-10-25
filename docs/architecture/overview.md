# Architecture overview

This page is the short version every contributor reads before touching code. The decisions behind it are the
[ADRs](../decisions/README.md); the schemas under `schemas/` are the contract. Where this page and the schemas
disagree, the schemas win and this page is the bug.

AlixAI is a **headless**, strictly self-hosted service. A host application embeds it through the
SDK or the HTTP API. The language model interprets and communicates; the host application and the
deterministic AlixAI core hold all authority ([ADR 0004](../decisions/0004-headless-service.md),
[ADR 0006](../decisions/0006-prepare-confirm-commit.md)).

## 1. Reference diagram

```text
Host application UI / mobile / voice client
                     │
             AlixAI client SDK
                     │
                     ▼
┌─────────────────────────────────────────────────────────────┐
│                        AlixAI Service                       │
│                                                             │
│  API Gateway ─ Identity Context ─ Rate/Resource Limits      │
│       │                                                     │
│       ▼                                                     │
│  Session + Structured Goal State                            │
│       │                                                     │
│       ├── Capability Router ─ Policy Gateway                │
│       │                         │                           │
│       │                         ▼                           │
│       │              Prepare / Confirm / Commit             │
│       │                         │                           │
│       │                         ▼                           │
│       │               Application Adapters                  │
│       │              MCP / OpenAPI / Native                 │
│       │                                                     │
│       ├── Authorized RAG Service                            │
│       │      FTS + vectors + RRF + provenance               │
│       │                                                     │
│       └── Prompt Builder + Response Verifier                │
│                              │                              │
│                              ▼                              │
│                     AI Runtime Provider                     │
└──────────────────────────────┬──────────────────────────────┘
                               │ private local API/socket
                               ▼
                      LocalAI by default
           LLM / STT / TTS / embedding / reranker
```

Box to package mapping (details in [module-map.md](module-map.md)):

| Box | Package |
| --- | --- |
| API Gateway | `internal/gateway` |
| Identity Context | `internal/identity` |
| Rate/Resource Limits | `internal/resources`, `internal/quotas` |
| Session + Structured Goal State | `internal/sessions` |
| Capability Router | `internal/capabilities` |
| Policy Gateway | `internal/policy` |
| Prepare / Confirm / Commit | `internal/actions` |
| Application Adapters | `internal/adapters` (contract) + `adapters/{mcp,openapi,native}` (implementations) |
| Authorized RAG Service | `internal/rag` |
| Prompt Builder + Response Verifier | `internal/dialogue` (prompt building), `internal/verify` |
| AI Runtime Provider | `internal/providers` (interfaces) + `adapters/localai` (the only LocalAI code) |
| Durable queue, store, audit | `internal/jobs`, `internal/store`, `internal/audit` |

## 2. Process model

Minimum deployment:

| Piece | What it is | Notes |
| --- | --- | --- |
| `alixai` | One Go binary (`CGO_ENABLED=0`, [ADR 0001](../decisions/0001-go-core-service.md)) | Service and CLI in one executable. Default listener `127.0.0.1:7470`, optional Unix socket. |
| LocalAI | One private runtime process, reached only through `AIRuntimeProvider` ([ADR 0003](../decisions/0003-localai-behind-runtime-provider.md)) | Loopback or Unix socket. Separate least-privilege OS identity. Only inference, health and model-list paths are callable. |
| SQLite | One database file in WAL mode ([ADR 0002](../decisions/0002-sqlite-wal-core-store.md)) | Metadata, sessions, preparations, receipts, audit, jobs, FTS5 index. Driver: `modernc.org/sqlite` ([ADR 0009](../decisions/0009-pure-go-sqlite-driver.md)). |
| Data directory | Local document and index directory | Source originals stay owned by their provider unless explicitly imported. |
| Models | Installed models and only the LocalAI backends they need | One active LLM in the Lite profile. |

Optional, started only when installed or active: OCR, parsing, embedding and large reranking
workers as isolated processes with strict memory and time limits; voice components (VAD, STT, TTS)
installed independently so a text-only deployment pays nothing for them.

Not required in the baseline: PostgreSQL, Redis, Kubernetes, an external vector database. Background
work (ingestion, embeddings, OCR, reindexing, maintenance) uses the SQLite-backed bounded queue
([ADR 0007](../decisions/0007-sqlite-job-queue.md)). Larger deployments may swap these behind provider
contracts when measured scale requires it.

Network posture (§16.1): AlixAI binds loopback by default; a non-loopback listener requires TLS and
explicit configuration. Outbound traffic is denied by default; every application, API or MCP
connector gets an explicit destination allowlist. No telemetry. No shell tools, code execution,
arbitrary MCP registration or LocalAI administration surface.

Resource priority (§17.1), highest first: (1) authentication, authorization and commits; (2) the
active text or voice turn; (3) the current RAG query; (4) ordinary application reads;
(5) ingestion and embeddings; (6) OCR, reindexing and maintenance. Lower classes yield to higher
ones; every queue has bounded depth, deadlines, cancellation and retry with backoff.

## 3. Ownership boundaries

### 3.1 AlixAI owns (§4.1)

- Application registration and manifests; integration contracts and SDKs.
- Tenant, workspace and principal context propagation.
- Session and structured goal state; context assembly and bounded memory.
- Capability routing; model prompts and constrained output schemas.
- Policy evaluation before any tool is exposed to a model.
- Task proposal validation; prepare/confirm/commit coordination.
- RAG source registration, ingestion and query orchestration; retrieval authorization and provenance.
- Answer and response verification.
- Local conversation and audit records.
- Provider selection, health and graceful degradation.
- Conformance, evaluation and security test tooling.

### 3.2 LocalAI owns by default (§4.2)

- Local model loading and inference; backend selection and hardware acceleration.
- Text generation; STT, TTS and VAD execution where qualified.
- Embedding and reranking execution.
- Realtime WebSocket/WebRTC pipeline support and, optionally, typed Decisions/SystemOne inference for the decision scorer (see [decision-layer.md](decision-layer.md)), where the selected backend supports it.
- Model runtime health and resource reporting.

LocalAI's own authentication protects its private endpoint. It does not replace AlixAI or
host-application authorization.

### 3.3 The integrated application owns (§4.3)

- User identity and login; tenant and workspace membership; record-level permissions.
- Business data, transactions and invariants.
- Current availability, conflicts and calculations (the deterministic results the model may only quote).
- Authoritative documentation and terminology.
- Action risk classification overrides. The host may raise a safety level; it cannot silently lower a
  platform-enforced minimum (§11.4).
- Commit, rollback and idempotency behavior.
- Final application audit and compliance records.

### 3.4 Non-goals (§4.4)

No unrestricted chatbot, no arbitrary browsing, no shell or code execution, no direct writes to
host databases, no automatic trust in application documentation or retrieved text, no fully
autonomous consequential actions, no hosted model service, no replacement for host authentication,
no single mandatory model.

### 3.5 What the model may and may not do

| The model may | The model may never |
| --- | --- |
| Propose a `TaskProposal` (capability ID, candidate fields, candidate entity references, uncertainty) | Own permissions, business facts, validation or conflict resolution |
| Verbalize a verified `DialogueDirective` or `ActionReceipt` | Create or alter an authoritative field of a directive, preparation or receipt |
| Embed, rerank and summarize **authorized** text it was handed | Choose which tenant, workspace or source it can see |
| Ask a clarification the directive permits | See bearer credentials, reusable secrets or tools the policy gateway did not expose |
| | Turn natural language directly into a database write, shell command or external side effect |

Retrieved text, application documentation, tool responses and file contents are **untrusted
data**. They are fenced in prompts and never act as instructions (§12.6).

## 4. Action safety levels (§11.4)

| Level | Meaning | Default behavior |
| --- | --- | --- |
| 0 | Read-only, low sensitivity | Execute after authorization |
| 1 | Reversible low-impact change | Present concise action summary; application policy may allow immediate commit |
| 2 | Consequential or external side effect | Prepare, show exact effects, require explicit confirmation |
| 3 | High-risk, privileged or legally sensitive | Require authenticated application UI or stronger step-up; AI cannot complete alone |

## 5. Data flow of one turn

The turn pipeline. "Det." means deterministic code with no model in the decision.
Every row fails closed: a failure never maps to a success-looking result (rule 14).

| # | Step | Who decides | Package | On failure |
| --- | --- | --- | --- | --- |
| 1 | Authenticated text or finalized transcript arrives; context token verified into a `contracts.Scope` (application, tenant, workspace, principal, roles, session) | Det. | `gateway`, `identity` | Reject. No scope, no work. There is never a default scope. |
| 2 | Scope and rate checks; admission under the priority class | Det. | `gateway`, `resources`, `quotas` | Reject or queue before memory is exhausted. |
| 3 | Active goal and state lookup | Det. | `sessions` | Expired or invalidated state is discarded, not reused. |
| 4 | Capability shortlist, deterministic rules first; the policy gateway removes anything the principal may not use | Det. | `capabilities`, `policy` | Empty shortlist means a refusal or clarification, not a guess. |
| 5 | Bounded context assembly: goal state, relevant turns, authorized RAG evidence if needed | Det. | `dialogue`, `rag` | Over-budget context is truncated by rule, never silently widened. RAG scope is applied before text reaches a model. |
| 6 | **Model call 1:** constrained `TaskProposal` generation | Model proposes | `providers` -> `adapters/localai` | Runtime unavailable: return a clear local failure. Partial output is never executed. |
| 7 | Schema validation of the proposal | Det. | `contracts`, `dialogue` | Invalid output is discarded. Never a partially trusted object. |
| 8 | Entity resolution against the host application | Host app | `adapters` | Ambiguous or missing entity becomes a clarification. |
| 9 | Host reauthorization and deterministic validation | Host app | `adapters`, `policy` | Denied or invalid fields become a `DialogueDirective` with the missing or invalid fields. |
| 10 | `DialogueDirective` or `PreparedAction` produced (opaque preparation ID, exact effects, risk level, data versions, expiry, idempotency key) | Det. | `actions`, `dialogue` | Nothing is committed at this step. |
| 11 | Confirmation when the level requires it (2: explicit confirmation; 3: authenticated application UI or step-up) | Human via host UI | `actions`, `policy` | Stale, expired, replayed or invalidated preparation is refused. |
| 12 | Idempotent commit through the adapter | Host app | `actions`, `adapters` | Uncertain response: query idempotency or status before any retry. Adapter down: report no commit. |
| 13 | `ActionReceipt` recorded (committed or rejected, stable record IDs, canonical facts, audit reference) | Det. | `actions`, `audit` | Partial failure is recorded as such, not rounded up to success. |
| 14 | **Model call 2:** natural-language response, then verified against the directive or receipt | Model verbalizes, Det. verifies | `dialogue`, `verify`, `providers` | The verifier is code, not a second model. Failed verification falls back to the deterministic template for the same directive. |

Notes:

- The model is called at steps 6 and 14 of the turn path, plus embedding, reranking and grounded
  answer generation inside RAG. It is never in the authorization, validation or commit path.
- A level 0 read skips steps 11 and 12 but still passes 1-9 and 13-14.
- Policy runs twice: before tool exposure (step 4) and again at prepare and commit (steps 10-12).
- The full transcript is stored apart from the model context; old turns are not replayed.
  Application events, permission changes, user changes, timeouts and record revisions invalidate
  pending state; events never trigger a commit.
- A successful RAG answer carries the original authorized sources that support it. With no
  supporting evidence the result is `no_supported_evidence`, never an uncited answer (§12.5).

## 6. Degradation (§18, summary)

| Failure | Behavior |
| --- | --- |
| LLM unavailable | Application and search stay usable; clear local failure; no execution from partial output |
| Embedding model unavailable | Authorized FTS/exact search, labelled as reduced semantic recall |
| Reranker unavailable | Fused retrieval order |
| STT / TTS unavailable | Text input remains; verified text returned |
| Source unavailable | Source marked stale or unavailable, never reported as checked |
| MCP/API adapter unavailable | Session and prepared-state safety preserved; no commit reported |
| Commit response uncertain | Query idempotency/status before retrying |
| SQLite or index corruption | Isolate damage; restore metadata backup or rebuild the derived index |
| LocalAI crash | Supervisor restarts with backoff; in-flight action stays uncommitted unless a receipt proves otherwise |
| Disk pressure | Pause ingestion before interactive or transactional storage is endangered |

## 7. Where decisions are recorded

| Topic | ADR |
| --- | --- |
| Go core service | [0001](../decisions/0001-go-core-service.md) |
| SQLite WAL as core store | [0002](../decisions/0002-sqlite-wal-core-store.md) |
| LocalAI behind the runtime provider | [0003](../decisions/0003-localai-behind-runtime-provider.md) |
| Headless service | [0004](../decisions/0004-headless-service.md) |
| MCP-first adapters | [0005](../decisions/0005-mcp-first-adapters.md) |
| Prepare / confirm / commit | [0006](../decisions/0006-prepare-confirm-commit.md) |
| SQLite-backed job queue | [0007](../decisions/0007-sqlite-job-queue.md) |
| Apache-2.0 licence | [0008](../decisions/0008-apache-2-licence.md) |
| Pure-Go SQLite driver | [0009](../decisions/0009-pure-go-sqlite-driver.md) |

Open decisions (§25) are reserved as 0010-0020 in the [index](../decisions/README.md).
