# Module map

Every Go package in the repository, what it is responsible for, and what it may import. This file
is the dependency contract. A task that needs an import this map forbids stops and reports it; the
fix is an edit to this file in a reviewed change, not a quiet violation.

Module path: `github.com/buxee/alix-ai`. Pure Go, `CGO_ENABLED=0` ([ADR 0001](../decisions/0001-go-core-service.md),
[ADR 0009](../decisions/0009-pure-go-sqlite-driver.md)).

## 1. Dependency rules

Rules 1 and 2 are enforced by the repository-wide invariant tests run by `go test ./...` (imports parsed with `go/parser`).
Rules 3 to 10 are the standing convention; they are not machine-checked yet, except where a row says a test exists.

| # | Rule | Enforced by |
| --- | --- | --- |
| 1 | **`pkg/contracts` imports nothing internal.** No import path under `github.com/buxee/alix-ai/internal/`. Stated policy is stricter: stdlib and third-party only, plus its own subpackages. | `TestPkgContractsImportsNothingInternal` |
| 2 | **`internal/*` never imports `adapters/localai`**, with one exception: `internal/cli`, a composition root. Only `adapters/localai` itself (including its subpackages) and the composition roots may import it. Applies to every `.go` file, `_test.go` and build-tagged files included. Tests that need a runtime use `internal/providers`' fake, or drive the `alixai` binary. | `TestNoLocalAIImportsOutsideAdapterAndCompositionRoots` |
| 3 | **Only composition roots wire concrete providers and adapters.** The roots are `cmd/`, `internal/cli/` and `tools/`. Everything else receives interfaces from `internal/providers` and `internal/adapters`. | convention |
| 4 | **Imports flow downward through the table in section 2.** A package may import only packages listed above it in that table, and only those named in its "May import" column. No cycles. | convention |
| 5 | **Only `internal/store` imports `modernc.org/sqlite`.** Every other package sees `store.DB`, `store.Tx` and the scoped query helpers. This is the reversal seam of [ADR 0009](../decisions/0009-pure-go-sqlite-driver.md). | convention |
| 6 | **`pkg/sdk-go` and `pkg/bridge` import only `pkg/contracts`** (and stdlib). An embedding host application must be able to depend on them without pulling in the service. | convention |
| 7 | **Outside the composition roots, `internal/*` imports no concrete `adapters/*` package** (`mcp`, `openapi`, `native`, `events`, `localai`, `external/*`). Rule 2 is the enforced subset. | convention |
| 8 | **Concrete adapters never import each other.** `adapters/{mcp,openapi,native,events,localai}` and each `adapters/external/<provider>` are siblings. Shared helpers placed directly under `adapters/external/` are the one allowed exception: the provider packages may import them and nothing else of each other. | convention |
| 9 | **A package may only import what its task's `depends_on` has already merged.** The "May import" column is an upper bound, not an entitlement. If a task needs an allowed package that is not among its dependencies, it stops and reports the missing `depends_on` edge. | convention |
| 10 | **External provider mode (milestone X) is opt-in code, reached only through interfaces and one tagged composition-root file.** The milestone-X packages (the rows marked X-NN in sections 2 and 3) are imported only by each other, by their own tests, by the X test suites (`tests/conformance/providers`, `tests/security/extprovider`, `tests/degradation/ext` and the repository-wide invariant tests), by `tools/` binaries that are not part of `./cmd/alixai`, and by **`internal/cli/providers_external.go`** (`//go:build !noexternal`, created by X-10), which is the only file in the dependency graph of `./cmd/alixai` that may import `internal/providerreg` or any other X package. The core reaches them through interfaces it already owns (the dialogue seams of M1-14, the gateway's `Need`/`Want` by type, the `internal/providers` roles). Types-only imports of `pkg/contracts` and `pkg/contracts/gen/providers` are allowed everywhere. A build with `-tags noexternal` therefore contains none of them ([ADR 0022](../decisions/0022-external-provider-packaging.md)). | planned test `TestXNoexternalBuildHasNoExternalCode` (X-GATE, runs `go list -tags noexternal -deps ./cmd/alixai`); `internal/providerreg` already enforces its own imports with `TestImportsStayWithinRegistryTier` |

Go's own `internal/` rule keeps other modules from importing `internal/*`. The public surface is
`pkg/`, `schemas/`, the HTTP API and the SDKs.

Conventions that cross every package: functions take `ctx context.Context` and a `contracts.Scope`
and return an error when there is no scope; every table carries `application_id`, `tenant_id`,
`workspace_id` and every query filters on them; time comes from an injected `clock.Clock`; IDs come
from `internal/ids`; errors are `contracts.ErrCode` wrapped with `%w`.

## 2. Internal packages

Read top to bottom. **base** means `pkg/contracts`, `internal/clock`, `internal/ids`,
`internal/config`, `internal/log`; any package from tier 2 down may import base without listing
it. `internal/version` is a leaf any package may import. Responsibilities cite the task that built them so
the code and its history can be matched.

### Tier 0 - leaves (stdlib and third-party only)

| Package | Responsibility | May import |
| --- | --- | --- |
| `pkg/contracts` | Public Go types: `Scope`, `ErrCode`, `TaskProposal`, `DialogueDirective`, `PreparedAction`, `ActionReceipt`, generated from `schemas/`. | nothing from this module |
| `internal/clock` | `Clock` interface, `Real`, `Fake` with `Advance()`. Only source of time. | nothing from this module |
| `internal/ids` | Prefixed ULIDs (`ses_`, `prep_`, `rcpt_`, `src_`, `job_`, `aud_`, ...). | nothing from this module |
| `internal/version` | Build version and commit info. | nothing from this module |
| `internal/config` | YAML file plus `ALIXAI_<SECTION>_<FIELD>` overrides; sections registered by their owning module; unknown keys are an error. | nothing from this module |

### Tier 1 - process basics

| Package | Responsibility | May import |
| --- | --- | --- |
| `internal/log` | `slog` JSON handler with redaction of tokens, secrets and configured fields. | `internal/config` |

### Tier 2 - infrastructure

| Package | Responsibility | May import |
| --- | --- | --- |
| `internal/store` | SQLite (WAL, FTS5) open, per-module migrations and checksums, scoped query helpers, `Tx` with busy retry, backup, integrity check. The only importer of the SQLite driver. | base |
| `internal/store/storetest` | Test helpers for other modules: `Open`, `Scope`, `AssertScopedTables`. Imported only from `_test.go` files. | `internal/store`, `pkg/contracts` |
| `internal/providers/voice` | Voice role interfaces (STT batch + streaming, streaming TTS, VAD), audio limits, `voice.Register`/`LookupSTT|TTS|VAD`, deterministic fake in `voice/fake`. Interfaces only. | `internal/providers`, `pkg/contracts` |
| `internal/voice/profile` | Installable voice profile: loads nothing unless enabled and a pack is installed; language-pack registry (one active pack); gated providers. | `internal/config`, `internal/providers`, `internal/providers/voice` |
| `internal/identity/identitytest` | Test support: mints tokens (incl. malformed ones) and builds a full identity Env. Imported only from tests. | `internal/identity`, `internal/store`, `internal/clock`, `pkg/contracts` |
| `internal/adapters/fake` | In-memory host application with injectable faults and an in-process transport, used by the adapter conformance suite. | `internal/adapters`, base |
| `tests/conformance/adapters` | Reusable adapter conformance suite (package `adaptersuite`): `RunAdapterSuite(t, Factory)`. | `internal/adapters`, `internal/adapters/fake` |
| `internal/providers` | Runtime role interfaces (`TextGenerator`, `Embedder`, `Reranker`, voice roles), request budgets, registry with per-role health, deterministic fake. Interfaces only, no HTTP. | base |
| `internal/models` | Model registry schema and loader; candidate entries with hash and licence. | base |
| `internal/egress` | Outbound destination allowlists per connector, enforced by a custom dialer; deny by default. | base |
| `internal/metrics` | Local metrics in Prometheus text format, loopback only, no push. Imported by `diag` and `gateway`; other packages expose plain stats, not metric registrations. | base |

### Tier 3 - platform services

| Package | Responsibility | May import |
| --- | --- | --- |
| `internal/audit` | Tamper-evident local audit log. | base, `store` |
| `internal/jobs` | Durable bounded queue: priority classes, leases, retry/backoff, deadlines, scope retention. | base, `store` |
| `internal/trust` | Package integrity manifests, Ed25519 signatures, trust store. | base, `store` |
| `internal/policy` | Policy gateway: deny by default, tool exposure, risk floors, field projection. Pure decision functions over `contracts` types that return an audit-ready record; it does not touch the store. Runs before exposure and again at prepare and commit. | base |
| `internal/resources` | Resource governor: admission control, background yielding to interactive work, idle model unload through the provider interface. Probes are injected. | base, `jobs`, `providers` |
| `internal/quotas` | Per-tenant quotas with in-memory buckets and persisted daily counters. | base, `store` |
| `internal/settings` | Typed, versioned settings and client preferences, permission-checked. | base, `store`, `policy`, `audit` |
| `internal/dataegress` (X-04, planned) | **Data**-egress policy: what may leave the deployment. Policy store and evaluation across deployment, application, tenant, workspace and source (most restrictive wins, default external disabled), classification rules, request-envelope builder and redaction, RAG-excerpt filter by source mode, region check, disclosure, per-decision audit record, persistent global kill switch. No model, no network. Distinct from `internal/egress`, which decides which network **destinations** may be dialled; neither imports the other. Implements the dialogue `PromptGuard` seam structurally, without importing `dialogue`. | base, `store`, `audit`, `policy`, `pkg/contracts/gen/providers` |
| `internal/providerreg` (X-05, merged) | Persistent registry of provider and model profiles (module `providerreg`, application-scoped), capability gate (`Check`, `Route`) that rejects an impossible request before any data is built, lifecycle candidate / qualified / quarantined / retired, terms and review staleness, billing-notice acknowledgement, fallback chain data. Contacts no provider; takes the credential status through an interface. Only `internal/cli/providers_external.go` (`!noexternal`) may import it from the serve path (rule 10). | `pkg/contracts`, `pkg/contracts/gen/common`, `pkg/contracts/gen/providers`, `internal/clock`, `internal/ids`, `internal/jobs` (X-25, the revalidation job), `internal/store` (tests also `internal/store/storetest`); not `config`, `log`, `net`, `os/exec` or any adapter (`TestImportsStayWithinRegistryTier`) |
| `internal/credentials` (X-07, merged) | Credential service: references bound to provider, deployment, application, tenant; OS secret store, encrypted-file and env/file backends behind one interface; `Resolver`; rotation, revocation, manual-key fallback, masked fingerprint; audit of every use. The only place a provider secret lives. | base, `store`, `audit`, `pkg/contracts/gen/providers` |
| `internal/oauth` (X-08, planned) | OAuth 2.x authorization-code + PKCE engine: state and nonce, exact redirect binding, loopback and registered-HTTPS redirects, consent data, refresh with rotation, revocation. Does not open a browser; stores tokens only through `credentials`; token and revocation endpoints are dialled through `egress`. | base, `credentials`, `egress`, `pkg/contracts/gen/providers` |
| `internal/providerbudget` (X-09, planned) | Provider budgets, quotas, rate and concurrency limits, reserve / settle / release accounting, upper-bound estimates in integer micro-units, usage export. Does not import `providerreg` (the router joins them) or `quotas`. | base, `store`, `jobs`, `pkg/contracts/gen/providers` |

### Tier 4 - application contract and authorization

| Package | Responsibility | May import |
| --- | --- | --- |
| `internal/identity` | Application context token verification, jti replay cache, scope construction and propagation. | base, `store` |
| `internal/apppkg` | Application package loader, validator, quarantine install. | base, `store`, `trust` |
| `internal/dialogue/context` (package `dctx`) | Bounded context assembly and deterministic history compaction with per-profile token budgets; never reads the transcript; imports nothing from `internal/dialogue`. | `pkg/contracts`, `internal/providers`, `internal/sessions` (tests also `internal/clock`, `internal/store/storetest`) |
| `internal/dialogue/decision` (D-04) | The decision-layer stage of the dialogue engine: plugs `internal/decision` into the `dialogue.DecisionStage` seam, resolves candidates through the policy gateway, off by default; only a composition root imports it. | `internal/decision`, `internal/dialogue`, `internal/policy`, `internal/providers`, `pkg/contracts` (tests also `tests/conversation`, `internal/decision/fakescorer`) |
| `internal/capabilities` | Capability registry and deterministic-first router; shortlists only policy-visible capabilities; uses an `Embedder` only when one exists. | base, `store`, `apppkg`, `policy`, `providers` (test files only may also import `trust`, `tools/devkeys`, `store/storetest` and `providers/fake`) |
| `internal/adapters` | `AppAdapter` contract (describe, read, validate, prepare, commit, status, cancel, health, inspect), normalised results, in-memory fake. Interfaces and wrappers only; transports live in `adapters/*`. | base, `egress` |

### Tier 5 - domain engines

| Package | Responsibility | May import |
| --- | --- | --- |
| `internal/actions` | Prepare / confirm / commit coordinator: preparation IDs, expiry, idempotency, uncertain-outcome recovery, receipts. | base, `store`, `jobs`, `audit`, `identity`, `policy`, `adapters` |
| `internal/verify` | Deterministic response verifier and per-language response templates. No model. | base |
| `internal/rag` | Source providers, parsing workers, chunker, FTS5 and vector index, ingestion, authorized hybrid query with RRF, grounded answers, citation resolver. `rag/ingest` defines `ingest.Charger`, the hook that charges the bytes a source sync reads into new versions in the transaction that writes them (M9-01); the composition root fills it from `internal/quotas`, which `rag` does not import. | base, `store`, `jobs`, `audit`, `identity`, `policy`, `providers`, `egress`, `apppkg`, `adapters`, `verify` |
| `internal/blocks` | Rich-block validation. | base |
| `internal/stream` | SSE and WebSocket streaming protocol, resumable. | base, `blocks` |
| `internal/files` | Attachments and scoped file references. Defines `files.Charger`, the hook that charges storage and ingestion bytes in the transaction that stores the file (M9-01); the composition root fills it from `internal/quotas`, so this package does not import `quotas`. | base, `store`, `jobs`, `audit`, `policy` |
| `internal/sessions` | Durable sessions and structured goal state; event handling that invalidates state and never commits. | base, `store`, `audit`, `identity`, `capabilities`, `actions`, `adapters` |
| `internal/history` | Conversation history API and retention. | base, `store`, `sessions` |
| `internal/voice` | Installable voice profile, language packs, voice session service (same session and action state as text; speech only after facts are verified). | base, `store`, `providers`, `models`, `sessions`, `stream`, `blocks`, `verify` |

### Tier 6 - orchestration

| Package | Responsibility | May import |
| --- | --- | --- |
| `internal/analyst/loop` | Bounded read-only analyst step loop (ADR 0047): constrained AnalystStepProposal iterations, caps, ability-filtered grammar, egress-safe dataset summaries, progress stages `analyst_*`. Sibling seams (Computer, Verifier, Web, Domain, AbilityResolver) are interfaces only; the integrator wires them. | base, `pkg/contracts`, `pkg/contracts/gen/analyst`, `pkg/contracts/gen/conversation`, `pkg/contracts/gen/common` |
| `internal/analyst/compute` | Deterministic compute engine for analyst turns: fixed op catalog (filter/sort/group/aggregate/ratio/forecast/…), exact decimal money, typed refusals; no code execution. | base, `analyst/dataset`, `pkg/contracts`, `pkg/contracts/gen/analyst`, `pkg/contracts/gen/common` |
| `internal/analyst/dataset` | Shared dataset helpers (field lookup, row clone, measure alignment) used by compute and the loop. | base, `pkg/contracts/gen/analyst` |
| `internal/analyst/domain` | Model-judged in/out-of-domain classifier for analyst turns; fail-closed typed refusal; utterance + declared domain only. | base, `pkg/contracts`, `pkg/contracts/gen/analyst` |
| `internal/analyst/web` | Web search/fetch seam (Brave/Tavily/SearXNG): credentials via `cred_ref`, SSRF guard, host allow/deny, query egress, fenced untrusted results + citations. | base, `credentials` (via SecretResolver interface only), `clock`, `ids`, `pkg/contracts`, `pkg/contracts/gen/analyst`, `pkg/contracts/gen/common` |
| `internal/analyst/settings` | Tenant analyst ability ceilings and effective-set resolver (deployment ∧ application ∧ tenant); gateway `AnalystService`. | base, `store`, `audit`, `clock`, `pkg/contracts`, `pkg/contracts/gen/analyst`, `pkg/contracts/gen/applicationpackage`, `pkg/contracts/gen/common` |
| `internal/dialogue` | The turn pipeline (section 5 of the [overview](overview.md)): state lookup, shortlist, prompt builder with fenced untrusted content, constrained proposal, directive or preparation, verified response. Long-term memory plugs in through the `MemoryStore` seam (ADR 0042); the engine does not import `internal/memory`. Optional analyst path (ADR 0047) enters `internal/analyst/loop` when abilities include `loop` and the controller is `analyst_capable`; field class / local_only for datasets use `apppkg` helpers. | base, `store`, `audit`, `identity`, `capabilities`, `policy`, `actions`, `sessions`, `rag`, `verify`, `providers`, `adapters`, `apppkg`, `analyst/loop` |
| `internal/memory` | Long-term memory per principal × tenant × workspace (ADR 0042): SQLite store, consent-gated proposals, retrieval for context, erasure/export hooks. | base, `store`, `audit`, `clock`, `ids`, `config`, `pkg/contracts/gen/memory` |
| `internal/integrationaudit` | Integration audit runner, signed report, activation state machine, static and runtime probes, re-audit triggers. | base, `store`, `audit`, `trust`, `apppkg`, `capabilities`, `policy`, `adapters`, `egress`, `actions`, `rag`, `identity`, `providers`, `sessions`, `dialogue` |
| `internal/readiness/catalog` (M8-01) | Integration readiness requirement catalog (embedded data plus loader): the checklist of what a project offers before the integration audit can pass it; every requirement names the audit check it feeds. Advisory, decides nothing. | base, `pkg/contracts/gen/readiness` |
| `internal/readiness/scan` (M8-02) | Deterministic leg of integration readiness discovery ([ADR 0037](../decisions/0037-integration-readiness-discovery.md)): bounded read-only walker over a directory the operator names, rule-based detectors (MCP, OpenAPI, native adapters, deterministic operations, models, data, docs, scope, RAG sources, deployment) and findings with path, line span and file hash. No model, no network, no process execution, no `$ref` resolution; secret-looking files are listed and never opened, secret-looking values are redacted. Standard library only beyond the two listed imports; `TestScannerImportsNoNetworkOrExec` enforces it. | base, `pkg/contracts/gen/readiness`, `readiness/catalog` |
| `internal/readiness/verify` (M8-03) | Evidence verifier of readiness discovery ([ADR 0037](../decisions/0037-integration-readiness-discovery.md)): the one authority on whether a finding stands. Re-opens every cited file under the scanned root (confined with `os.Root`, no link followed), re-hashes it, re-reads the line span with the scanner's line rule and, for a model proposal, checks the quoted text is in the span. Evidence outside the root, outside the scan's own inventory or on a secret file is refused without opening it; a finding that does not confirm is demoted to unverified with a reason from a closed vocabulary; a model proposal that confirms becomes `model_verified`, one that does not `model_unverified`. Origin comes from which slice a finding arrived in, never from its `Source`. Standard library only beyond the listed imports; mutation tests (`TestVerifierMutantsAreCaught`) prove each rule is load-bearing. | base, `pkg/contracts/gen/readiness`, `readiness/catalog`, `readiness/scan` |
| `internal/readiness/report` (M8-03) | Checklist evaluator, report builder and bilingual markdown renderer. The evaluator accepts only `verify.RdyVerified` values, takes each status from the rule's finding and lets a verified model finding raise only a `missing` or `unverified` requirement the catalog allows it to satisfy; `model_unverified` never counts as covered (mutation tests). Cross-source contradictions (two API descriptions that disagree on an operation) are re-read by the verifier like any finding. The report is advisory and its first line, in English and Italian, says it is not an integration audit pass. | base, `pkg/contracts/gen/readiness`, `readiness/catalog`, `readiness/scan`, `readiness/verify` |
| `internal/readiness/store` (M8-03) | Report store in the data directory: module `readiness` migrations through `internal/store`, key-level rows (the application id the operator names, or `prj_` and a hash of the canonical directory path; tenant and workspace empty), history with a cap, latest, diff (newly met, newly missing, changed) and the status view. Never stores file content beyond the bounded, redacted finding text; validates a report before storing it. | base, `internal/store`, `pkg/contracts/gen/readiness`, `readiness/scan` (redactor); tests also `readiness/report` |
| `internal/readiness/assist` (M8-04) | Model-assisted leg of readiness discovery ([ADR 0037](../decisions/0037-integration-readiness-discovery.md)): picks bounded, redacted excerpts of the project's prose (README, docs, API descriptions, tool lists, schema files; never code, never a secret file) through its own `os.Root`-confined reader that re-hashes every file, fences them as untrusted data under a fixed template (digest recorded in the report), withholds instruction-like, fence-breaking and hidden-character lines, and calls a `providers.TextGenerator` with no tools, a closed output schema, temperature 0 and a token and call budget. Every reply is untrusted: only found or partial, only for a requirement no rule has decided, only on lines the model was shown; anything else is dropped and counted (attempted overrides). Survivors go to `report.RdyBuild` as `verify.RdyProposal` values; the verifier decides. No model, an unavailable or failing model, or a refusal leaves the deterministic leg alone and the report says so. The model and the optional egress gate (`AssistEgressGate`) come from the CLI composition root as interfaces; an external model needs external mode enabled, the gate and a project class other than `local_only`. Imports none of `internal/modes`, `internal/dataegress`, `internal/providerreg`; `TestAssistImportsNoExternalModePackages` enforces it. Mutation tests (`TestAssistMutantsAreCaught`) prove each rule is load-bearing. | base, `internal/providers`, `pkg/contracts/gen/readiness`, `readiness/catalog`, `readiness/scan`, `readiness/verify`, `readiness/report`; tests also `providers/fake` |
| `internal/backup` | Backup and restore of metadata; rebuild of derived indexes. | base, `store`, `audit`, `jobs`, `rag` |
| `internal/upgrade` | Staged upgrade with compatibility checks and rollback. | base, `store`, `backup`, `audit`, `rag`, `version` |
| `internal/diag` | Local monitoring and diagnostics bundle (redacted), queue and provider status. | base, `store`, `jobs`, `providers`, `audit`, `metrics`, `version` |
| `internal/supervisor` | Restarts LocalAI with backoff on native installs. Controls the process through the OS and `providers` health, never through LocalAI code. | base, `providers` |
| `internal/modes` (X-11, planned) | Mode resolution (Local default; deployment, application, tenant, most restrictive wins), provider router (egress guard and budget check before every external call, no alternate provider unless policy names one), the additive section 13 outcomes as typed results, feature-locality report, global disable. No HTTP, no model calls. Refuses the decision-scorer role for external providers. | base, `providers`, `dataegress`, `providerreg`, `providerbudget`, `store`, `audit`, `policy` |
| `internal/dialogue/extmode` (X-12, merged) | Implements the dialogue seams for external mode: egress guard in the turn pipeline, provider tool-call proposals validated as `TaskProposal`, allowlisted tool schemas, local-only evidence handling, verification of external responses. Wired only from the `!noexternal` composition root. | base, `dialogue`, `modes`, `dataegress`, `rag`, `verify`, `policy`, `adapters`, `providers`, `audit` |
| `internal/extmodal` (X-15, merged) | Capability-gated external audio, vision, image generation and TTS handling; every modality OFF by default ([ADR 0024](../decisions/0024-provider-side-audio-image-file.md)); scoped expiring references; generated media stored locally first. | base, `modes`, `dataegress`, `providers`, `blocks`, `files`, `voice`, `verify`, `dialogue/extmode`, `audit` |
| `internal/modes/hybrid` (X-22, planned) | Hybrid orchestration: local routing and RAG, external realisation from a verified directive with the facts-only envelope ([ADR 0023](../decisions/0023-hybrid-default-egress-content.md), [ADR 0027](../decisions/0027-external-language-realization.md)), outage fallback to the local provider, template fallback. | base, `modes`, `dataegress`, `providerbudget`, `providers`, `verify`, `dialogue/extmode`, `audit` |

### Tier 7 - edge

| Package | Responsibility | May import |
| --- | --- | --- |
| `internal/gateway` | HTTP/WebSocket API: loopback listener, health and readiness, error envelope, one self-registering `routes_<area>.go` file per area. Receives providers and adapters as interfaces. | base, any tier 2 to 6 package except `supervisor`, `backup`, `upgrade` and except the milestone-X packages (rule 10: `routes_v1_providers.go`, X-13, takes its services by interface type and the `!noexternal` root provides them); never `internal/cli`, never `adapters/*` |

### Tier 8 - composition roots

| Package | Responsibility | May import |
| --- | --- | --- |
| `internal/cli` | CLI subcommands, one `cmd_<name>.go` file each, self-registering via `init()`. Wires config, store, providers, adapters and gateway for `alixai serve`. External provider registration lives in `providers_external.go` (`//go:build !noexternal`) with a stub `providers_noexternal.go` (X-10, planned): the only serve-path files that import the milestone-X packages (rule 10). | anything, including concrete `adapters/*` |
| `cmd/alixai` | `main` only; delegates to `internal/cli`. | `internal/cli`, `internal/version` |
| `cmd/alixai-parser`, `cmd/alixai-conformance` | Isolated parsing worker and the conformance runner. Same root status as `cmd/alixai`. Keep a worker's import set minimal. | anything |
| `tools/<name>` | Developer tools (`schemagen`, `schemacheck`, `bench`, ...; milestone X adds `providerconf` in X-16 and `providercompare` in X-23, planned). | anything, including concrete `adapters/*` |

## 3. Packages outside `internal/`

| Package | Responsibility | May import |
| --- | --- | --- |
| `pkg/contracts` | See tier 0. | nothing internal |
| `pkg/sdk-go` | Go client SDK over the public HTTP API. | `pkg/contracts` only |
| `pkg/bridge` | Go library a host application embeds to expose its operations to the native adapter. | `pkg/contracts` only |
| `internal/voice/packs` | Voice language packs: embedded, schema-validated pack manifests (Whisper base, Piper, Silero VAD, en and it) pinned by size and sha256; Store (Install, Verify, Activate, Deactivate), Catalog and Loader. Install copies from an operator directory, never downloads, needs an explicit approval and never activates; the activation marker lives under the data dir. | base, `store`, `audit` |
| `adapters/localai` | The **only** code that knows LocalAI exists (subpackages such as `voice` and `realtime` included). Implements the `internal/providers` roles over LocalAI's OpenAI-compatible API. The `voice` subpackage (M4-02) has its own 3-route allowlist (`/v1/audio/transcriptions`, `/v1/audio/speech`, `/v1/vad`) and the parent allowlist below is untouched. Fixed endpoint allowlist: inference, health and model-list only; admin paths are never callable. Refuses non-loopback endpoints without TLS. | base, `internal/providers` |
| `adapters/mcp` | MCP transport. Filters and wraps individual tools; never exposes a whole server to a model. | base, `internal/adapters`, `internal/egress` |
| `adapters/openapi` | OpenAPI/HTTP transport from an approved API subset. | base, `internal/adapters`, `internal/egress` |
| `adapters/native` | Native local-IPC transport (framed JSON-RPC over a Unix socket, loopback TCP fallback) with peer authentication. | base, `internal/adapters`, `internal/egress`, `internal/identity` |
| `adapters/events` | Signed event intake that invalidates session state; events never trigger a commit or a tool call. | base, `internal/adapters`, `internal/trust`, `internal/identity`, `internal/sessions` |
| `adapters/external` and `adapters/external/<provider>` (X-10 `openai`, X-19 `anthropic`, X-20 `gemini`, X-21 `meta`, planned) | One Go package per hosted provider implementing the extended provider contract of X-06 over that provider's official API; shared helpers live directly under `adapters/external/`. Compiled into the one binary, DISABLED by default, absent from a `-tags noexternal` build ([ADR 0022](../decisions/0022-external-provider-packaging.md)). They receive an already admitted, filtered request envelope; they never import `internal/providerreg`, `dataegress`, `modes` or `providerbudget`, never `adapters/localai`, and never each other. Keys come from `credentials` only; destinations are dialled through `egress`. | base, `internal/providers`, `internal/egress`, `internal/credentials`, `pkg/contracts/gen/providers` |
| `tests/{conformance,security,performance,degradation,e2e,...}` | Cross-cutting tests, including the repository-wide invariants. May import anything except as limited by rule 2. | anything |
| `examples/reference-apps/*`, `sdk/typescript`, `sdk/python` | Outside the service's Go dependency graph. Go examples import only `pkg/`. | `pkg/*` |

## 4. Non-Go layout that shapes the graph

| Path | Contents |
| --- | --- |
| `schemas/<area>/` | JSON Schema 2020-12, `$id: urn:alixai:schema:<area>:<name>:v1`. Go and TypeScript types are generated from them (`tools/schemagen`) and committed. |
| `schemas/api/openapi.yaml`, `schemas/api/paths/<area>.yaml` | Public API root plus one fragment per area. |
| `schemas/providers/` | External provider mode contracts (profile, policy, envelope, usage, budget, credential reference, conformance report); generated Go in `pkg/contracts/gen/providers`. See [provider-schemas.md](../integration/provider-schemas.md). |
| `internal/<module>/migrations/NNNN_name.sql` | Embedded per-module migrations; no global numbering. |
| `models/registry/` | Model registry entries. |
| `deploy/{native,container,examples}` | Packaging. Loopback binds, bind mounts, no one-shot init containers. |
| `docs/{architecture,integration,operations,security,decisions,efficiency,evidence}` | Documentation. |

## 5. Self-registration points

So parallel tasks never edit the same file:

- DB migrations: `store.RegisterMigrations(module, fs)`, tracked in `schema_migrations(module, version, checksum)`.
- CLI: one `internal/cli/cmd_<name>.go` per subcommand.
- HTTP: one `internal/gateway/routes_<area>.go` per area.
- Config: `config.Register("<section>", &<module>.Config{})` from the owning module.
- External providers: `internal/cli/providers_external.go` (`!noexternal`) is the one registration point for every milestone-X package; nothing else wires them.

## 6. Changing this map

1. Prefer moving the code or adding an interface over adding an import.
2. If an import is genuinely needed, edit the "May import" cell and say why in the commit message.
3. A new package gets a row in the right tier in the same change that creates it.
4. A change to rules 1 to 3 needs an ADR.
