# Architecture decision records

Material architecture changes are recorded here. An ADR states the context,
the options considered with their security and resource impact, the decision, its consequences and
how to back out. Write it before or with the change, not after.

## How to add one

1. Copy [TEMPLATE.md](TEMPLATE.md) to `NNNN-short-title.md` using the next free number.
2. Fill every section. Measurements are numbers from a run, or the exact measurement still owed and
   the task that will take it. "N/A" needs a reason.
3. Set `- **Status:**` to one of `proposed`, `provisional`, `accepted`, `superseded by NNNN`.
   `provisional` means a default has been chosen and a named later task will confirm or overturn it.
4. Add a row to the index below in the same change.
5. Never edit an accepted ADR to change its decision. Write a new one and mark the old one
   `superseded by NNNN`. Typo and link fixes are fine.

Numbers 0021-0028 hold the external-provider-mode decisions below, 0029 the decision layer, 0030 and 0031 its
deployment and default-enable policy (written provisional by D-01, finalised by D-08), 0032-0038 are taken,
0039 amends voice VAD rules for dictation, 0040 is the Parakeet local-STT provisional, 0041–0042 are
per-principal LLM accounts and long-term memory (1.2.0), 0043 is external TTS with per-role provider
choice (1.3.0), 0044 is opt-in external realisation of live facts with verified tables and charts
(1.3.0), 0045 is private self-hosted LLM endpoints (1.4.0), and 0046 is verified incremental
streaming (1.4.0). The next free number is 0047.

## Index

### Settled (accepted)

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-go-core-service.md) | Go for the core service | accepted |
| [0002](0002-sqlite-wal-core-store.md) | SQLite (WAL, FTS5) as the core store | accepted |
| [0003](0003-localai-behind-runtime-provider.md) | LocalAI as the default runtime, behind `AIRuntimeProvider` | accepted |
| [0004](0004-headless-service.md) | Headless service, no AlixAI UI | accepted |
| [0005](0005-mcp-first-adapters.md) | MCP-first application adapters, policy stays in AlixAI | accepted |
| [0006](0006-prepare-confirm-commit.md) | Prepare, confirm, commit for every consequential action | accepted |
| [0007](0007-sqlite-job-queue.md) | SQLite-backed bounded job queue | accepted |
| [0008](0008-apache-2-licence.md) | Apache License 2.0 for AlixAI-owned code | accepted |
| [0009](0009-pure-go-sqlite-driver.md) | Pure-Go SQLite driver: `modernc.org/sqlite` | accepted |

0001-0008 record the founding design decisions; 0009 is the first decision added during the build.

### The open decisions of the core design

Numbers 0010-0020 hold one ADR per core design decision. Most are `accepted`. A row that is still
`provisional` keeps a default so the build never stalls; until it is accepted, the numbers in that ADR are
indicative or "owed", and no document may present that default as settled.

| ADR | Open decision | Status | Item | Provisional default | Finalised by |
| --- | --- | --- | --- | --- | --- |
| [0010](0010-lite-model-selection.md) | Final Lite model, including sub-1B candidates | accepted | 1 | Smallest candidate that passes the pre-registered bar: Granite 4.0 350M, then Qwen3.5-0.8B, then Granite 4.0 1B; deterministic tier first | M5-09 |
| [0011](0011-embedded-vector-index.md) | Embedded vector-index implementation and corpus ceiling | accepted | 2 | Exact int8 flat scan in the core store behind `VectorIndexProvider`; ceiling 100k chunks per workspace; sqlite-vec benchmarked head to head | M2-07 |
| [0012](0012-lite-install-native-vs-container.md) | Native process versus container as the preferred Lite install | accepted | 3 | Full container stack (rootless Podman and Quadlet first, Compose supported); native packages always supported | M5-02 |
| [0013](0013-application-context-token.md) | Application context token: PASETO, JWT profile or mutually authenticated exchange | accepted | 4 | Strict JWT profile, one algorithm (`EdDSA`; switch to `Ed25519` when golang-jwt and PyJWT support it), in-tree verifier, single-use jti; mutual-auth local IPC as a seam | M1-06 |
| [0014](0014-parser-sandbox.md) | Cross-platform sandbox for parsers and optional adapters | accepted | 5 | Out-of-process worker, one disposable process per document: mandatory process and resource limits everywhere (Windows Job Objects; Linux rlimits + raw Landlock + seccomp), best-effort OS confinement, fail closed; wazero rejected; macOS and linux/arm64 untested | M2-04 |
| [0015](0015-voice-models-lite.md) | Voice models that meet quality and memory gates on CPU-only Lite hardware | accepted (STT default on Balanced superseded in part by [0040](0040-parakeet-local-stt.md); dictation without VAD amended by [0039](0039-dictation-without-vad.md)) | 6 | Optional per-language packs: whisper.cpp tiny STT on Lite (`en-stt`); Italian and TTS Balanced-only / candidate | M4-06 |
| [0039](0039-dictation-without-vad.md) | Dictation without VAD (amendment of 0015 for user-recorded clips) | accepted | — | STT-only packs for dictation; voice conversation still requires VAD; format/length/energy mitigations | 1.1.0 |
| [0040](0040-parakeet-local-stt.md) | Local STT ladder: Parakeet and Whisper large (candidates until voicebench) | provisional | — | Lite tiny; Balanced Parakeet or turbo; Performance large-v3 / Parakeet / turbo — defaults only after gates | 1.1.0 |
| [0016](0016-multi-app-process-model.md) | Several applications per process, or one isolated deployment per security domain | accepted | 7 | Several applications per process only within one security domain; one deployment per domain recommended | M3-15 |
| [0017](0017-policy-expression-format.md) | Stable policy-expression format for host overrides | accepted | 8 | Closed declarative `policy/v1` data format (raise-only by grammar); Cedar as the planned extension | M1-07 |
| [0018](0018-resource-accounting-quotas.md) | Resource accounting and quotas for shared multi-tenant deployments | accepted | 9 | In-process token buckets and semaphores per application and tenant, persisted daily counters, per-tenant job cap | M5-06 |
| [0019](0019-trademark-compatibility-label.md) | Trademark and compatibility-label policy for third-party distributions | accepted | 10 | Nominative use free; modified builds rename; conformance-gated "AlixAI Compatible" label; owner decides | accepted 2026-10-25 |
| [0020](0020-4gb-profile.md) | Whether and when the complete 4 GB profile passes the full gates | accepted | 11 | Not supported; experimental, emulated, text-only profile with explicit promotion criteria | M5-09 |
| [0029](0029-decision-layer-clm-8b.md) | Optional decision layer: CLM-v0.1-8B as an optional, bounded, non-generative, non-authoritative scorer (deterministic rules and a smaller scorer for constrained profiles) | accepted | — | Optional layer behind a provider-neutral contract; supervised sidecar preferred; OFF by default for every profile until a benchmark passes the default-enable gate; revisions pinned (D-08) | D-08 (ADR-0030, ADR-0031) |
| [0030](0030-decision-scorer-deployment.md) | Decision scorer deployment: LocalAI Decisions API, supervised sidecar or native | accepted | — | Pinned LocalAI v4.10.0 has no Decisions API (measured), so the supervised sidecar is the only CLM path, as an opt-in on a higher-memory host; native rejected | D-08 |
| [0031](0031-decision-scorer-default-enable.md) | Decision scorer default-enable policy | accepted | — | No profile enables any scorer by default; CLM-8B balanced is a measured resource_gate, higher-memory not measured; only a recorded benchmark that passes the `tools/decbench` gate on that host changes it | D-08, D-H02 (human) |
| [0032](0032-fts5-scope-rowid-ranges.md) | Authorised full-text retrieval: one FTS5 table with one rowid range per scope | accepted | — | Scope applied in the MATCH, rechecked in SQL and Go; BM25 cost follows the scope, not the corpus | M2-06 |
| [0033](0033-openapi-wire-contract.md) | OpenAPI/HTTP adapter wire contract | accepted | — | One fixed wire contract configured only by header names; requests and replies checked against the document; no redirects and no hidden retry; a sent commit without a usable reply is `uncertain_outcome` | M1-11 |
| [0034](0034-dialogue-context-budget.md) | Dialogue engine context budget | accepted | — | One `dctx.Profile` replaces the engine's six limits; proposals are built through the bounded builder; responses keep their own prompt under the profile's response budget | M3-18 |
| [0035](0035-copilotkit-agui-compatibility.md) | CopilotKit and AG-UI compatibility | provisional | — | AG-UI endpoint in the core, optional `@alixai/copilotkit-adapter`, Go and Python proxy helpers; client tools, state and context are untrusted; approval through interrupts; no cloud or telemetry | M3-24 |

### The open decisions of external provider mode (milestone X)

One ADR per open decision, all `provisional`, written in
X-01 on 2026-10-03 with provider facts dated that day. External mode is opt-in and off by default; none of these
changes Local mode. The architecture they sit in is [external-provider-mode.md](../architecture/external-provider-mode.md).

| ADR | Open decision | Status | Item | Provisional default | Finalised by |
| --- | --- | --- | --- | --- | --- |
| [0021](0021-first-external-provider.md) | First external provider adapter | provisional | 1 | OpenAI (owner's choice; manual API key first because no OAuth for API access is documented), adapter X-10; Anthropic, Gemini, Meta or hosted Llama follow in X-19, X-20, X-21 | X-16, X-H02 |
| [0022](0022-external-provider-packaging.md) | Core distribution or separate optional packages | provisional | 2 | All adapters in the one binary under `adapters/external/<provider>`, disabled by default, plus a `noexternal` build tag that removes all external code | X-10 |
| [0023](0023-hybrid-default-egress-content.md) | What Hybrid sends by default | provisional | 3 | Structured task state and deterministic facts only; RAG excerpts only from sources marked `allow_excerpt` | X-22 |
| [0024](0024-provider-side-audio-image-file.md) | Provider-side audio, image and file processing in the first release | provisional | 4 | OFF: text and structured output only; each modality enabled later by X-15 behind explicit policy | X-15 |
| [0025](0025-cost-estimation-and-budget-semantics.md) | Cost estimation and budget semantics across billing units | provisional | 5 | Integer micro-units from a per-profile price table over nine meter units; estimates are upper bounds and drive the hard stop; actuals reconcile | X-09 |
| [0026](0026-data-residency-and-region-policy.md) | Provider-region and data-residency policy | provisional | 6 | Profiles record region attributes, policy has `allowed_regions`, unknown or `global` region denied unless listed; jurisdiction rules are the owner's | X-04, X-H03 (human) |
| [0027](0027-external-language-realization.md) | External natural-language realisation while the local model is the controller | provisional | 7 | Hybrid only, from a verified `DialogueDirective` with the facts-only envelope, final text must pass fact verification, template fallback | X-22 |
| [0028](0028-provider-change-and-terms-policy.md) | Support policy when a provider changes model behaviour or terms | provisional | 8 | Pin model version ids; 30-day review ceiling; quarantine on stale review or a changed terms hash, version or region; a changed version keeps the profile id and resets qualification | X-05 (implemented), schema follow-ups |
| [0037](0037-integration-readiness-discovery.md) | Integration readiness discovery: operator-launched, model-assisted, evidence-bound, advisory | accepted | 1 | Deterministic scan plus a model that only proposes; every finding cites evidence a verifier re-reads; never certifies or activates; CLI first | M8 |
| [0038](0038-popup-oauth-and-provider-catalog.md) | Popup OAuth and the provider catalog (SIWC, Google, OpenRouter) | accepted | — | Popup + postMessage + flow poll; SIWC loopback-only; Google operator client; OpenRouter issued key; Anthropic/xAI/Kimi key-only | 1.1.0 |
| [0041](0041-per-principal-llm-accounts.md) | Per-principal LLM accounts and the provider catalog | accepted | — | Operator catalog (`account_mode: per_principal`); bind per (application, principal); reserved tenant bucket for keys; resolve owner check for all kinds | 1.2.0 |
| [0042](0042-long-term-memory.md) | Long-term memory (principal × tenant × workspace) | accepted | — | Consent-gated items; TurnEvent `memory_proposal` → AG-UI CUSTOM `alixai.memory_proposal`; no silent writes | 1.2.0 |
| [0043](0043-external-tts-and-per-role-provider-choice.md) | External TTS and per-role provider choice | accepted | — | OpenAI / ElevenLabs / Google TTS / Polly; attested text only; `characters_in` metering; each role resolves independently | 1.3.0 |
| [0044](0044-external-directive-class-and-result-blocks.md) | Opt-in external realisation of live facts, with verified tables and charts | accepted | — | `external_providers.directive_class`; verified table/chart blocks; conversation-contract `presentation.chart` | 1.3.0 |
| [0045](0045-private-self-hosted-llm-endpoints.md) | Private self-hosted LLM endpoints (per principal and per tenant) | accepted | — | Tenant pin → principal private / external binding → deployment default; `self_hosted` egress; no external fallback when pin/binding not ready | 1.4.0 |
| [0046](0046-verified-incremental-streaming.md) | Verified incremental streaming | accepted | — | Opt-in `dialogue.stream_verified` (default off); sentence segments verified before emit; TurnEvent `text_replaced` → AG-UI `alixai.text_replaced` | 1.4.0 |

Related: [architecture overview](../architecture/overview.md), [external provider mode](../architecture/external-provider-mode.md) and [module map](../architecture/module-map.md).
