# AlixAI threat model

- **Version:** 0.1, initial (task M0-14, milestone 0, written before the first line of product code)
- **Companion documents:** [test-backlog.md](test-backlog.md) (every threat below is a numbered `SEC-NNN` entry there), [localai-hardening-checklist.md](localai-hardening-checklist.md)
- **Status:** living document. It is re-opened when a new transport, source type, provider, voice feature or UI kit is added (see section 9).

This document describes what AlixAI protects, where trust changes, who attacks it, and what can go
wrong in each component of the §5 reference architecture. It does not repeat the tests. Each threat
points to the backlog entries that carry the required test, owner and severity.

## 1. Security objectives

The objectives are the project's non-negotiable rules restated as things an attacker would have
to break.

| # | Objective | Broken by |
| --- | --- | --- |
| O1 | **Tenant isolation.** Every request, row, index, cache entry and job is bound to application, tenant, workspace, principal and scope, and nothing crosses | Cross-tenant disclosure, scope confusion |
| O2 | **Deterministic authority.** Only a prepared, validated, confirmed (where required), idempotent and reauthorized action reaches an application. The model proposes and verbalizes; it never decides | Unauthorized, replayed, stale or duplicated commits; model-set authority fields |
| O3 | **Verified facts.** Every fact the user hears comes from a deterministic result, a receipt or a cited authorized source | Fabricated or unverified facts, uncited answers |
| O4 | **Secrecy of credentials.** The model never sees bearer credentials or reusable secrets; logs and errors never carry them | Secret leakage |
| O5 | **Local-only operation.** No required cloud, no telemetry, no outbound traffic except an explicit per-application allowlist, LocalAI reachable only by AlixAI | Exfiltration, SSRF, exposed runtime |
| O6 | **Interactive availability.** Authentication, authorization, commits and the active turn are served first and stay within bounded resources | Denial of service, resource exhaustion |
| O7 | **Accountability.** Decisions, preparations, confirmations and commits leave tamper-evident records | Audit tampering or loss |
| O8 | **Integrity of what runs.** Only audited application packages, verified models and backends, and signed releases are active | Malicious package, model or build substitution |

## 2. Assets

| ID | Asset | Where it lives | Main concern |
| --- | --- | --- | --- |
| A1 | Context tokens and issuer keys | Host to gateway; identity key registry in SQLite | Forgery, replay, theft (O1, O2) |
| A2 | Secrets: LocalAI API key, adapter credentials, signing keys | OS secret store or approved secret provider, referenced by opaque id | Disclosure (O4) |
| A3 | Tenant business data in flight: adapter results, session and goal state, transcripts, attachments | Memory, SQLite, `data_dir/files` | Cross-tenant disclosure, tampering (O1) |
| A4 | RAG corpus: source originals (owned by the provider), chunks, FTS index, vectors, embeddings | Source providers, SQLite, index directory | Cross-tenant disclosure, poisoning, stale content (O1, O3) |
| A5 | Prepared actions, confirmations, receipts | SQLite (module `actions`) | Replay, forgery, loss (O2) |
| A6 | Audit log and signed integration-audit reports | SQLite (module `audit`), report files | Tampering, truncation, loss (O7) |
| A7 | Application packages and publisher keys | `data_dir` content-addressed store, trust store | Tampering, activation of unaudited code (O8) |
| A8 | Models, backends, model registry | Bind-mounted models and backends directories, `models/registry/` | Substitution, unapproved install (O8) |
| A9 | Policy: manifest policies, host policy, operator configuration, settings | Package, config file, SQLite | Silent widening, unsafe settings (O2, O5) |
| A10 | Durable state: SQLite database, job queue, derived indexes, backups | `data_dir` | Corruption, loss, permission exposure (O6, O7) |
| A11 | Build and release pipeline, release signing keys, SBOM | CI, release tooling, offline keys | Supply-chain compromise (O8) |
| A12 | Compute: CPU, RAM, disk, the loaded model slot | The host | Exhaustion (O6) |
| A13 | System prompt and prompt templates | Core code | Integrity only; the prompt is not a secret (O2) |
| A14 | Voice audio and transcripts (optional) | Memory; transcripts in SQLite | Privacy, retention (O4) |

## 3. System model and trust boundaries

### 3.1 The reference architecture with boundaries

The diagram is the §5 architecture. `TBn` marks a place where trust changes. The model is drawn
inside the AlixAI box because AlixAI feeds it and consumes its output, but the model's output is
untrusted data and crosses TB3 on its way back.

```text
 Host application UI / mobile / voice client                  [untrusted client code]
                     │
             AlixAI client SDK
                     │ ═══ TB1: context token, TLS when remote ═══════════════════
                     ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                               AlixAI Service                                │
│                                                                             │
│  C1 API Gateway ─ C2 Identity Context ─ Rate/Resource Limits                │
│       │                                                                     │
│       ▼                                                                     │
│  C3 Session + Structured Goal State                                         │
│       │                                                                     │
│       ├── C4 Capability Router ─ Policy Gateway                             │
│       │                         │                                           │
│       │                         ▼                                           │
│       │              C5 Prepare / Confirm / Commit                          │
│       │                         │                                           │
│       │                         ▼   ═══ TB2: egress allowlist, minimal ═══  │
│       │               C6 Application Adapters       identity context        │
│       │              MCP / OpenAPI / Native / Events ────────► host app and │
│       │                                                        MCP servers  │
│       ├── C7 Authorized RAG Service                                         │
│       │      sources ═ TB5 ═► parser sandbox ► FTS + vectors + RRF          │
│       │                                                                     │
│       └── C8 Prompt Builder + Response Verifier                             │
│                              │ ═══ TB3: prompt out, output in ═════════════ │
│                              ▼                                              │
│                 C9 AI Runtime Provider (+ the model's output)               │
│                                                                             │
│  C11 SQLite + job queue    C12 Audit log + integration audit                │
│  C13 Package loader + trust store          C17 Operator plane (config/CLI)  │
└──────────────────────────────┬──────────────────────────────────────────────┘
                               │ ═══ TB4: private socket or loopback, API key, ═══
                               │          fixed endpoint allowlist
                               ▼
                      C10 LocalAI by default
           LLM / STT / TTS / embedding / reranker  (+ C15 voice workers)

 TB6: AlixAI process and data directory versus other OS users and processes
 TB7: operator plane (install, config, models) versus the request plane
 TB8: build and distribution (C16) versus what runs
 TB9: tenant versus tenant inside one AlixAI
```

### 3.2 Boundary table

| ID | Boundary | What crosses | What the receiving side must not assume |
| --- | --- | --- | --- |
| TB1 | Host application or SDK to AlixAI gateway | Context token, user text, attachments, scope hints | That any identity or scope field outside the verified token is true. Client code is untrusted |
| TB2 | AlixAI adapters to application back ends and MCP servers | Typed requests with minimal identity, results, events | That a result is well-formed, in scope, honest or non-instructional. That a server's tool list or schema is stable |
| TB3 | Prompt builder and verifier to the model | Prompts out, generated text and proposals in | That model output is valid, authorized, true or non-malicious. It is data to be validated |
| TB4 | AlixAI to LocalAI | Inference, embedding, audio, health on a private endpoint | That LocalAI enforces any authorization. Its API key is a single full-admin credential |
| TB5 | Source documents and API data to the parser sandbox and index | Bytes, names, metadata | That content is benign, bounded, correctly named or in the declared root |
| TB6 | AlixAI and LocalAI processes to the rest of the operating system | Files, sockets, ports, environment | That other local users or processes are friendly |
| TB7 | Operator plane to request plane | Package install, model install, configuration, secrets | That operator input is safe: it can widen exposure by mistake |
| TB8 | Build, release and model distribution to the running system | Binaries, images, models, backends, dependencies | That an artifact is what it claims to be without a verified hash and signature |
| TB9 | Tenant to tenant, workspace to workspace, principal to principal | Nothing, by design | That any shared structure (cache, index, dedupe key, job, session) is safe to share |

### 3.3 Who is trusted for what

| Party or component | Trusted to | Not trusted to |
| --- | --- | --- |
| AlixAI core (after TB1 authentication) | Enforce scope, policy, confirmation, idempotency, verification | n/a. It is the reference monitor, so it is kept small and tested |
| Host application | Authenticate users, own record-level permissions, reauthorize every read, prepare and commit, own business facts | Be bug-free. A compromised host holds valid tokens, so blast radius is bounded by capability scopes, risk floors and explicit confirmation |
| The model | Nothing. Output is a proposal or phrasing | Authority, permissions, facts, validation, commits (§3 rule 5) |
| LocalAI | Run inference and report health | Authenticate, authorize, or keep tenants apart. It is a compute backend |
| Application package | Declare capabilities, documentation and sources once signed and audited | Anything while in quarantine, or after a change until re-audited |
| MCP and API servers | Return typed results for mapped operations inside the egress allowlist | Honest schemas, stable schemas, non-instructional text |
| Documents, records, API text | Be evidence, with provenance | Be instructions (§12.6) |
| SDK and browser | Present state and collect input | Hold authority or credentials beyond memory |
| Operator | Install, configure, approve models and packages | Always configure safely. Defaults and checks assume mistakes |

### 3.4 Entry points for untrusted data

Every place where attacker-controlled bytes can reach a decision or a prompt.

| Entry point | Sink | Control | Entries |
| --- | --- | --- | --- |
| User text and finalized transcripts | Prompt builder | Fenced and labelled, bounded, policy-limited tool list | SEC-001, SEC-002, SEC-076 |
| Retrieved passages and document text | Evidence package, prompt | Fenced as untrusted data, scope applied before exposure, claims checked against spans | SEC-003 to SEC-006, SEC-012, SEC-018 |
| Adapter results and record fields | Dialogue state, prompt | Typed `DeterministicResult`, fenced, authoritative fields only from core and adapter | SEC-007, SEC-009, SEC-050 |
| MCP tool descriptions and text results | Registry, prompt | Mapped tools only, text-only refused for decisions, schema hash pinned | SEC-008, SEC-048, SEC-049 |
| Attachments and file names | Files, parsers, previews | Opaque FileRef, MIME sniffing, sandboxed parsing | SEC-013 to SEC-016, SEC-019 |
| Archives and package tarballs | Parser, package loader | Ratio, count, depth and path checks | SEC-014, SEC-015, SEC-059 |
| Model output | Schema validator, coordinator, verifier | Constrained decoding, schema validation, verifier, template fallback | SEC-009, SEC-010, SEC-011 |
| Application events | Session invalidation | Signed, scoped, deduplicated, never a commit trigger | SEC-045 |
| Context tokens | Identity middleware | One algorithm, audience, expiry, replay cache | SEC-031 to SEC-035 |
| HTTP bodies, SSE and WebSocket frames, Markdown | Gateway, block validator | Size limits, schema validation, sanitization, fuzzing | SEC-019, SEC-076, SEC-097 |
| Package manifest and contents | Loader, audit | Signature, manifest hash, quarantine, deterministic audit | SEC-057 to SEC-062 |
| LocalAI responses and errors | Provider boundary | Constrained output, error mapping, no raw pass-through | SEC-010, SEC-065 |
| Configuration and environment | Config loader | Unknown keys rejected, non-loopback without TLS refused | SEC-065, SEC-068, SEC-093 |
| Models, backends, dependencies, images | Runtime, build | Hash verification, digest pinning, SBOM, signatures | SEC-063, SEC-064, SEC-089, SEC-090 |

## 4. Attacker models

| ID | Attacker | Capabilities | Goals | Cannot (assumed) |
| --- | --- | --- | --- | --- |
| AM1 | **Authorized end user** (malicious, or an account taken over) | A valid token and session in one tenant. Can type or speak anything, upload files, replay requests | Act beyond their permission, commit without confirmation, read others' records, extract prompts or secrets, make the assistant lie | Mint tokens, edit packages, reach LocalAI directly |
| AM2 | **User of another tenant** | A valid token in a different tenant or workspace. Knows or guesses ids | Read or influence tenant A's sessions, documents, caches, receipts | Obtain tenant A's token |
| AM3 | **Content author** | Can place text in a document, record, ticket, email or API field that the application or a source later exposes. No direct access to AlixAI | Inject instructions, hidden text, bombs, look-alike content, stale or traversal-bearing files | Choose which tenant's model sees the text, other than through the content itself |
| AM4 | **Package publisher or host-app developer** (malicious or compromised) | Supplies a package, adapter config and documentation. May hold a publisher key | Activate an over-broad or lying integration, widen scope, hide operations, replace a signed package | Bypass the audit if the audit is correct. Sign as another publisher |
| AM5 | **Malicious or compromised MCP or API server** | Controls tool results, descriptions and schemas for mapped operations, and may change them later. Can crash or stall | Inject instructions, forge results, add hidden tools, drift schemas, exhaust AlixAI | Receive anything outside the egress allowlist and the minimal identity context |
| AM6 | **Local unprivileged OS user or co-resident process** | Runs code on the same host. Can read world-readable files, connect to loopback and sockets | Read the database, keys or logs, call LocalAI or AlixAI directly, tamper with model files | Become root |
| AM7 | **Network attacker** (LAN, or the internet for remote or VPS deployments) | Can connect to any exposed port, observe or alter unprotected traffic | Reach LocalAI or AlixAI, replay tokens, intercept traffic, SSRF through AlixAI | Break TLS, break signatures |
| AM8 | **Supply-chain attacker** | Can poison a dependency, a model repository, a container image, a backend or an update channel | Ship backdoored code, substituted weights or a trojaned backend | Forge signatures made with keys held offline |
| AM9 | **Careless operator** | Edits compose files, enables convenience settings, installs models by hand | Not an adversary. Exposes LocalAI, leaves the API key unset, enables the gallery or agents | n/a |

### 4.1 Assumptions and out of scope

- A compromised operating system, root user, hypervisor or physical access is out of scope. Disk
  encryption at rest is the operator's responsibility (see open question Q1).
- A compromised host application's own authentication is out of scope for prevention. The design
  bounds the damage (token capability scopes, risk floors, explicit confirmation, host reauthorization)
  but cannot make a legitimate identity safe.
- The model may be wrong, adversarially steered or tampered with. The design treats every output as
  untrusted and does not rely on model behaviour for any security property.
- Side channels beyond the ones listed (for example the shared prompt-cache timing channel, SEC-030)
  are not modelled in 0.1.
- LocalAI is an upstream dependency with its own vulnerability history. The design limits its
  exposure instead of relying on it being bug-free.

## 5. STRIDE per component

Categories: **S** spoofing, **T** tampering, **R** repudiation, **I** information disclosure,
**D** denial of service, **E** elevation of privilege. `n/a` rows say why a category does not
apply. Components follow §5 and the process model in §5.1, plus the supporting parts the diagram
leaves implicit (store, audit, package trust, operator plane, packaging).

### C1 API gateway (listener, routing, limits) — M0-12, M1-16, M5-05

| | Threat | Controls and tests |
| --- | --- | --- |
| S | A caller without a valid identity reaches `/v1`; a remote listener is impersonated | Identity middleware on every `/v1` route (SEC-031 to SEC-035); TLS and service identity for any non-loopback listener (SEC-093) |
| T | Request or response altered in transit on a remote deployment; malformed bodies | TLS requirement (SEC-093); body limits and schema validation (SEC-076, SEC-097) |
| R | A principal disputes a request | Request ids, audit events for decisions and actions (SEC-087) |
| I | Verbose errors, stack traces or secrets in responses; permissive CORS; diagnostics or metrics exposed | Typed error envelope (SEC-083); no CORS proxy or wildcard origin (SEC-092); loopback-only metrics (SEC-093) |
| D | Floods, oversized bodies, slow clients | Body limits, timeouts, quotas, admission control (SEC-076, SEC-077) |
| E | Undeclared routes (shell, agent, MCP registration, runtime admin) or the dev spike endpoint exposed | Route inventory against the OpenAPI spec and a deny-list (SEC-092, SEC-054); spike endpoint off by default (SEC-092) |

### C2 Identity context (tokens, scope propagation) — M1-06, M3-14

| | Threat | Controls and tests |
| --- | --- | --- |
| S | Forged, substituted, replayed or stolen tokens; another local process impersonating the host over local IPC | Single accepted algorithm, audience and issuer checks, expiry, replay cache (SEC-031 to SEC-034); issuer key rotation (SEC-037); peer authentication on local IPC (SEC-038) |
| T | Claims or scope overridden by request fields; missing levels defaulting | Scope only from the verified token, no defaults (SEC-035) |
| R | A principal denies an action | Principal, session and `jti` recorded in audit events (SEC-087) |
| I | Tokens in logs, errors or prompts; key files readable | Redaction and canary log inspection (SEC-082, SEC-083); owner-only key files (SEC-081) |
| D | Verifier crash on malformed tokens; replay cache growth | Token fuzzing (SEC-031); replay entries expire with the token (SEC-034) |
| E | A token for one capability exposes others; tenant switch inside a session | Capability scopes limit exposure (SEC-036); tenant change needs a new token (SEC-025) |

### C3 Session and structured goal state — M1-14, M3-01, M3-02

| | Threat | Controls and tests |
| --- | --- | --- |
| S | Session hijack by guessing or reusing an id | Ids are identifiers, not credentials; every access re-checks the token scope (SEC-025) |
| T | Slot values or entity ids altered by the model, a document or an event | Slot origins tracked, authoritative fields from core only, invalidation on revision, correction, permission change (SEC-009, SEC-042) |
| R | Transcript edits or loss disputes what was said | Transcript stored separately from model context; history cleanup never touches receipts or audit (SEC-087) |
| I | Another tenant reads sessions, history or exports | SEC-025, SEC-026 |
| D | Unbounded history and goal growth | Bounded context and compaction (SEC-076) |
| E | Goal state used to enable a capability the policy would deny | Policy decides on every call from scope, never from goal state (SEC-001) |

### C4 Capability router and policy gateway — M1-07, M1-08

| | Threat | Controls and tests |
| --- | --- | --- |
| S | One application claims another's capability ids | Ids namespaced per application (SEC-053) |
| T | Host policy or a setting lowers a platform risk floor or confirmation | Floors raise only (SEC-043) |
| R | A decision cannot be reconstructed | Every decision returns a complete record for the audit log (SEC-087) |
| I | The model sees fields it does not need | Field projection, least capability (SEC-036) |
| D | Huge shortlists or policy evaluation cost | Bounded shortlist (SEC-076). Policy expressions must be bounded in evaluation cost (open question Q3) |
| E | Model-driven tool or capability expansion; quarantined package serving | Deny by default, shortlist and policy before exposure, quarantine exposes nothing (SEC-001, SEC-004, SEC-060) |

### C5 Prepare, confirm, commit coordinator — M1-12

| | Threat | Controls and tests |
| --- | --- | --- |
| S | A confirmation used by another principal or session | Binding to principal, session, preparation id (SEC-040) |
| T | Model sets authoritative fields or claims confirmation; data changes between prepare and commit | SEC-009; data-version checks and expiry (SEC-042) |
| R | Disputed commit; double commit after uncertainty | Idempotency keys, receipts, status-before-retry (SEC-041) |
| I | Receipts or preparations visible across scopes | SEC-025, SEC-026 |
| D | Backlog of unconfirmed preparations | Expiry and cleanup (SEC-042) |
| E | Skipping confirmation or step-up; commit without host reauthorization; half-done action after a crash | SEC-043, SEC-044, SEC-046, SEC-047 |

### C6 Application adapters: MCP, OpenAPI, native, events, and egress control — M1-09 to M1-11, M3-04, M3-14

| | Threat | Controls and tests |
| --- | --- | --- |
| S | A server or endpoint impersonates the application; forged or replayed events | Egress allowlist to exact host and port (SEC-055); signed, scoped, deduplicated events (SEC-045) |
| T | Tool schema drift after audit; forged or malformed results | Schema hash pinning and re-audit (SEC-049); typed results only (SEC-050) |
| R | A call cannot be tied to a decision | Idempotency keys and audit events on every prepare and commit (SEC-041, SEC-087) |
| I | Credentials in errors or logs; SSRF or exfiltration to unlisted destinations | SEC-083; default-deny egress including redirects and DNS rebinding (SEC-055, SEC-056) |
| D | Crash, hang or giant response | Timeouts and size limits, crash handling (SEC-051) |
| E | Unmapped or over-broad operations; tool-name or capability confusion; arbitrary registration | SEC-048, SEC-052, SEC-053, SEC-054 |

### C7 Authorized RAG service: source providers, ingestion, parser workers, indexes, query and answer — M2-02 to M2-10

| | Threat | Controls and tests |
| --- | --- | --- |
| S | A document API returns another tenant's records; a file masquerades by name or extension | Scope parameters always sent; MIME sniffed, not trusted from the extension (SEC-013, SEC-018) |
| T | Poisoned documents steer answers; stale or deleted content stays live; chunks altered after indexing | Untrusted-data fencing and claim verification (SEC-003 to SEC-006); immutable versions and hashes; tombstones, final recheck (SEC-017, SEC-028) |
| R | An answer cannot be traced to a source | Mandatory citations with version and span (SEC-011) |
| I | Cross-tenant retrieval, cache leakage, dedupe existence oracle, FTS syntax abuse, foreign text in prompts | SEC-018, SEC-020 to SEC-024, SEC-029 |
| D | Archive bombs, oversized files, parser hangs, ingestion floods | SEC-013, SEC-014, SEC-016, SEC-079 |
| E | Parser escape, path traversal, symlinks, zip-slip | Sandboxed out-of-process parsers, canonical-path checks (SEC-015, SEC-016) |

### C8 Prompt builder and response verifier — M1-14, M1-13

| | Threat | Controls and tests |
| --- | --- | --- |
| S | Untrusted text posing as a system, tool or assistant message | Roles and fences fixed by the builder, untrusted content labelled (SEC-007, SEC-003) |
| T | Injected instructions in user text, documents, records or tool results alter behaviour | SEC-001 to SEC-008. Containment does not depend on the model obeying: policy, schema and coordinator are deterministic |
| R | Which prompt produced which action cannot be shown | Provider trace id on the proposal; prompts are not logged with secrets (SEC-082) |
| I | Tokens, secrets, system prompt or foreign-tenant text in a prompt | SEC-002, SEC-005, SEC-029 |
| D | Oversized prompts and evidence | Token budgets enforced before the call (SEC-076) |
| E | A response claims an unverified fact or completed action | Verifier with deterministic fallback (SEC-011) |

### C9 AI runtime provider boundary and the model's output — M0-07, M0-09

The model is an untrusted component. Its output crosses TB3 and is treated as data to validate.

| | Threat | Controls and tests |
| --- | --- | --- |
| S | AlixAI talks to an impostor runtime | Loopback or private socket, non-loopback needs TLS (SEC-065) |
| T | Output malformed, partial or adversarial; model or backend replaced | Constrained decoding plus schema validation, fail closed (SEC-010); hash verification (SEC-063, SEC-064) |
| R | Which model and profile answered | Model id, backend and profile recorded with results and audit reports (SEC-088) |
| I | Prompts sent to the wrong endpoint; API key logged | SEC-065, SEC-082 |
| D | Forced model loads, evictions, giant outputs | Max output tokens, request budgets, warm period (SEC-076, SEC-078) |
| E | Model output treated as authority; AlixAI calls runtime admin endpoints | SEC-009; fixed endpoint allowlist (SEC-067) |

### C10 LocalAI runtime, its models and backends — M0-08, M5-01, M5-07

LocalAI's own API key grants full administration with no roles, and its default feature set
includes a web UI, a model gallery, agents, MCP execution and a CORS proxy. The hardening
requirements are in the checklist; the tests are SEC-066 to SEC-070.

| | Threat | Controls and tests |
| --- | --- | --- |
| S | Anyone who can reach the port or holds the shared key acts as the administrator | Loopback or private socket, API key, separate OS identity (SEC-066, SEC-080) |
| T | Model files, backends or runtime settings changed; models installed through gallery or apply endpoints; an upgrade leaves backends in an unqualified state | Digest pinning, hash checks, gallery and runtime-settings disabled, runtime and backend versions compared with the qualified set (SEC-063, SEC-064, SEC-068, SEC-069, SEC-072) |
| R | Prompts retained in runtime logs | Log level and retention rules in the checklist, verified statically (SEC-068) |
| I | Prompts, metrics or error details exposed; KV cache timing across tenants | Opaque errors, metrics off or loopback, accepted low risk with a measurement (SEC-068, SEC-030) |
| D | Model-load storms, uploads, concurrency | Max active backends, watchdog, upload and concurrency limits (SEC-078) |
| E | Agent pool, server-side MCP stdio commands, CORS proxy, web UI used as a way in; container or unit running as root | Agents, MCP and UI disabled; hardened units (SEC-070, SEC-068, SEC-080) |

### C11 SQLite store, job queue and files — M0-05, M0-06, M3-06

| | Threat | Controls and tests |
| --- | --- | --- |
| S | A job runs with an identity that no longer exists | Jobs keep the initiating scope and are re-checked (SEC-027) |
| T | Direct edits to the database; hostile migrations; a tampered backup restored; SQL built from user input | Migration checksums, hash-chained audit (SEC-073, SEC-085); signed backup manifests (SEC-075); scoped, parameterised helpers (SEC-020, SEC-089) |
| R | n/a: the store is not an actor. Accountability lives in C12 | n/a |
| I | Files readable by other users; cross-scope reads; content-hash reuse across tenants | Owner-only permissions (SEC-081); SEC-020, SEC-026; scope in reuse keys (SEC-023) |
| D | Disk pressure, queue floods, corruption | Bounded queues, disk-pressure pause, integrity checks (SEC-079, SEC-074) |
| E | A FileRef or path reaching the model or the filesystem | Opaque FileRefs resolved with scope and expiry on every use (SEC-026) |

### C12 Audit log and integration audit — M1-15, M1-17, M1-18, M5-10

| | Threat | Controls and tests |
| --- | --- | --- |
| S | A forged audit event or report | Signed reports, chained events (SEC-088, SEC-085) |
| T | Modified, removed, reordered or truncated records | SEC-085, SEC-086 |
| R | Events missing for decisions or action transitions; audit lost with history cleanup | SEC-087 |
| I | Sensitive fields in events or exports; unscoped export | Redaction at write, scoped export (SEC-083, SEC-026) |
| D | Audit growth, or an append failure used to block or bypass | Bounded retention policy; write-ahead append and fail-closed on append failure (SEC-087, gap G10) |
| E | Activation without a clean audit; warnings widening scope | SEC-060, SEC-061, SEC-062 |

### C13 Package loader and trust store — M1-04, M1-05

| | Threat | Controls and tests |
| --- | --- | --- |
| S | Unknown, revoked or expired publisher key accepted | SEC-057, SEC-058 |
| T | Package changed or partially replaced | Manifest hash, detached signatures (SEC-057) |
| R | Who signed and activated what | Package lifecycle audit events (SEC-087) |
| I | n/a for the package content itself; credentials are referenced, never embedded. A package that embeds secrets is a finding | Static package validation (SEC-057) |
| D | Archive bombs and giant manifests | SEC-059, SEC-097 |
| E | Path traversal on install; activation through the loader | SEC-059, SEC-060 |

### C14 SDKs, host integration and rich blocks — M1-19, M3-05, M3-09 to M3-11

| | Threat | Controls and tests |
| --- | --- | --- |
| S | A page forges scope fields in SDK calls | Server ignores them, token only (SEC-035) |
| T | Rich blocks or Markdown carry script or active content | Sanitization and schema validation in core, host chooses renderers (SEC-019) |
| R | n/a: the SDK holds no authority | n/a |
| I | Tokens or credentials stored beyond memory | The SDK keeps the token in memory only and never logs it (SEC-082) |
| D | Reconnect storms and slow consumers | Bounded backpressure, resumable cursors (SEC-077) |
| E | A client enables a tool or lowers confirmation through settings | SEC-043, SEC-054 |

### C15 Optional voice service — M4

| | Threat | Controls and tests |
| --- | --- | --- |
| S | Speaker recognition or a spoofed voice used as authorization | Speaker recognition never authorizes (SEC-094) |
| T | Transcript altered before confirmation; unverified facts spoken | Correction before consequential confirmation; TTS only after verification (SEC-094) |
| R | Disputed spoken confirmation | Finalized transcript visible to the host and audited (SEC-087) |
| I | Raw audio retained | Not retained by default (SEC-094) |
| D | Audio floods | SEC-077 |
| E | Realtime pipeline answers or calls tools on its own | Tool calling disabled in the session (SEC-070, SEC-094) |

### C16 Packaging, release and supply chain — M0-01, M0-08, M5-01 to M5-03

| | Threat | Controls and tests |
| --- | --- | --- |
| S | A look-alike image, binary or model published as official | Digest pinning, signed releases and manifests (SEC-064, SEC-090) |
| T | Dependency, base image, model or backend poisoned or swapped | Reproducible builds, SBOM, vulnerability and licence checks, hash verification (SEC-063, SEC-064, SEC-089, SEC-090, SEC-091) |
| R | Build provenance unknown | Release manifest and SBOM (SEC-090) |
| I | Secrets baked into images or the repository | SEC-084 |
| D | n/a for the build. Availability of the pipeline is not a product security property | n/a |
| E | Over-privileged containers or units | SEC-080 |

### C17 Operator plane: configuration, CLI, secrets, settings — M0-02, M3-07

| | Threat | Controls and tests |
| --- | --- | --- |
| S | A local user runs operator commands | OS permissions on the CLI, data directory and keys (SEC-081) |
| T | Config edited to widen exposure; unknown keys silently ignored | Unknown keys rejected, non-loopback without TLS refused (SEC-065, SEC-093) |
| R | Configuration changes without a record | Settings changes audited (SEC-087) |
| I | Secrets in config files or environment dumps | Secret references only; redaction (SEC-082, SEC-083) |
| D | A bad setting starves the host | Resource limits are explicit configuration with bounded defaults (SEC-076, SEC-077, SEC-078) |
| E | Settings that enable undeclared tools, enlarge scope, bypass confirmation or expose runtime administration | SEC-043, SEC-054, SEC-067 |

## 6. Top risks and how the design answers them

| Risk | Why it matters | Primary defences | Entries |
| --- | --- | --- | --- |
| Prompt injection through documents and tool data steering the model | The model cannot reliably tell instructions from data | Nothing the model says is authority: policy before exposure, schema validation, coordinator, verifier, scope applied before text reaches the model | SEC-001 to SEC-011 |
| Cross-tenant leakage through retrieval, caches or reuse | Breaks the product's central promise and gate 5 | Scope in SQL and before similarity (SEC-020, SEC-021), scoped cache keys (SEC-022), prompt capture tests on every path | SEC-018 to SEC-029 |
| Unauthorized or repeated commits | Real-world side effects | Prepare, single-use bound confirmations, data-version checks, host reauthorization, idempotency | SEC-039 to SEC-047 |
| LocalAI as a full-admin side door | One shared key, wide default feature set | Loopback or socket, hardening checklist enforced by static checks, fixed endpoint allowlist, separate identity | SEC-066 to SEC-070 |
| Hostile content in parsers | Highest-risk code path | Out-of-process, resource-limited, no-network parsers | SEC-013 to SEC-016 |
| Malicious package or MCP server | Third-party code shapes the model's tool list | Signature, quarantine, deterministic audit, schema pinning, minimal wrapped surface | SEC-048 to SEC-062 |
| Supply chain | Substituted weights, backends or builds | Hash and digest pinning, signed reproducible releases | SEC-063, SEC-064, SEC-089 to SEC-091 |
| Silent loss of evidence | Tampering with or truncating the audit trail | Chain, signed checkpoints, write-ahead append | SEC-085 to SEC-088 |
| Unapproved outbound traffic or runtime code fetching | Breaks the self-hosted and no-telemetry rules and enables exfiltration | Default-deny egress, no runtime fetch path, nothing phones home under a failing dialer | SEC-055, SEC-056, SEC-071 |
| Weaknesses the planned tests do not anticipate | A backlog only covers threats someone thought of | Grey-box pentest of the running stack, then an external review, with no open Critical or High finding at release | SEC-095, SEC-096 |

## 7. Residual risks accepted in 0.1

| Risk | Reason accepted | Revisit |
| --- | --- | --- |
| Prompt-cache timing across tenants inside LocalAI (SEC-030) | Low severity, requires fine timing and prefix knowledge; measured in M5-12 and cache disabled if it matters | M5-12 |
| A legitimately authenticated but compromised host application can drive permitted capabilities | Authentication belongs to the host (§4.3) | Risk floors and confirmation stay mandatory; M6-H01 |
| Model errors that the verifier cannot detect (a plausible but wrong phrasing of a correct fact) | Facts are checked deterministically; wording quality is a product concern | M3-16 corpus, M5-09 |
| Windows parser sandbox is weaker than Linux | Mechanism chosen in ADR-0014 (M2-04) | M2-04, M5-12 |

## 8. Open questions

| ID | Question | Where it will be settled |
| --- | --- | --- |
| Q1 | Is encryption at rest for the SQLite database, files and backups part of the core or the operator's responsibility? The plan is silent. Transcripts and attachments can be sensitive | An ADR before M5-04 (backup) |
| Q2 | Does the first stable release recommend several applications per process or one deployment per security domain? Shared process means shared caches, queues and memory | ADR-0016 (M3-15) |
| Q3 | Policy expressions (ADR-0017) need bounded evaluation cost, or a hostile package can make the policy gateway a denial-of-service vector | ADR-0017 (M1-07) |
| Q4 | Token format and local IPC authentication (ADR-0013): algorithm confusion, key distribution and rotation for each SDK language | ADR-0013 (M1-06) |
| Q5 | How are operator secrets provisioned on each platform when no OS secret store is available (headless Linux server)? | M5-02, M5-13 |

## 9. When to update this document

- A new adapter transport, source type, provider role or API area is added (new component or boundary).
- Voice moves from optional to default for any profile.
- The M7 UI kits (built, opt-in) render untrusted Markdown, citations and previews in a browser: a client-side attack surface this version does not model yet. Model it before the kits are recommended for production.
- A pentest, review, CVE or incident produces a finding that has no backlog entry. Add the entry, then the threat here.
- An ADR in the range 0010 to 0020 is finalized and changes a control described above.
