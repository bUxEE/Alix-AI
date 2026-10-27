# Changelog: @alixai/sdk

All notable changes to this package. The format follows [Keep a Changelog](https://keepachangelog.com/); versions follow semantic versioning once 1.0.0
ships, with the rules of [compatibility.md](../../docs/policies/compatibility.md).

## 1.5.0 — 2026-10-27

### Added

- Analyst contracts (ADR 0047): generated types under `gen/analyst/*`; rich-block types `kpi`, `timeline`, `record_card`, `provenance`, `draft` and extended chart marks; progress stages `analyst_*`; egress category `web_query`; ErrCodes for domain/ability/web/forecast refusals.
- `DEFAULT_CLIENT_CAPABILITIES` and optional `client_capabilities` on turn inputs: the SDK announces exactly what it parses; pass a subset to announce less, or `null` to omit (server treats absent as 1.4).
- Typed private-LLM ops: `ops.privateLlm.*` takes/returns `gen/providers/private-llm-*` types (no more `Record<string, unknown>`).
- Erasure `scope: "workspace"` (with `level: "principal"`) for principal×workspace GDPR erase.
- `reportStepUpOutcome` — host-backend level-3 step-up outcome into the session (ADR 0048).
- Documented `me.llm.putRole` with `private_endpoint_id` (bind a role to a private endpoint; `profile_id` must be that endpoint's `profile_id`).

### Notes

- Private-LLM ops keep 1.4 `Record<string, unknown>` call sites type-compatible (overload); response guards accept extra members.
- Core: analyst companion (ADR 0047, off by default), step-up outcome (ADR 0048), workspace-scoped principal erasure (workspace-level erase no longer removes other workspaces' conversation history), llmbench quality mode, SSRF hardening, web search via SearXNG-compatible / Brave / Tavily.

## 1.4.1 — 2026-10-27

### Fixed

- `text_replaced` (ADR 0046): `viewTurn` / `turnText` discard superseded token text; `TurnView.textReplaced` is set when a replacement happened.

## 1.4.0 — 2026-10-27

### Added

- Ops private LLM routes (ADR 0045): `putPrivateLlmEndpoint` / `getPrivateLlmEndpoint` / `deletePrivateLlmEndpoint`, tenant pin and principal private binding operations under `/v1/ops/…`.
- `me.llm.putRole` accepts optional `private_endpoint_id` (mutually exclusive with `profile_id`); binding types carry optional `source` / `lifecycle`.
- TurnEvent type `text_replaced` / `TextReplacedEvent` (ADR 0046 verified incremental streaming). Fail-closed decode: upgrade the SDK before a core that emits it when `dialogue.stream_verified` is on.

### Notes (core)

- Private self-hosted OpenAI-compatible endpoints (`self_hosted`); tenant pin overrides personal external bindings inside that tenant; no silent fallback when warming/failed (`private_llm_starting` / `private_llm_unavailable`).

## 1.3.0 — 2026-10-27

### Added

- Principal LLM role `tts` and optional `voice` / `language` on `me.llm.putRole` / binding types (ADR 0043). Meter unit `characters_in` in generated provider types.
- `client.me.oauth.callback` / `flowStatus` / `cancel` for per-user host-callback OAuth (`POST /v1/me/oauth/callback`, flow status/cancel).
- `client.ops.erasure.start` / `status` for GDPR erasure over HTTP (`POST|GET /v1/ops/erasure…`, operator only).
- Optional `scope` on erasure start (`tenant` default / `application` for deployment-operator GDPR erase).
- `client.me.usage()` (`GET /v1/me/usage`) and `client.me.budget()` (`GET /v1/me/budget`); optional `principal_id` on `providers.exportUsage`.

### Notes (core)

- External TTS adapters (OpenAI, ElevenLabs, Google Cloud TTS, Amazon Polly, Mistral) behind `routing.tts` / principal `tts` bindings; attested verified text only; per-role provider choice (ADR 0043).
- Per-user usage view and application-wide principal budgets across tenants; Mistral chat + Voxtral STT/TTS (EU-default residency).
- CLI OAuth system-browser opener only follows validated `https` or loopback `http` URLs; light External hardware guide (4 vCPU / 8 GB).
- Opt-in `external_providers.directive_class` (live facts to the external model), verified table/chart blocks, and additive conversation-contract `presentation.chart` (ADR 0044).
- Outbound `credential_ref` for OpenAPI, MCP and RAG api (`application_credentials`); optional RAG `scope_params.include_principal_id`. Kit fullscreen z-index default 2400; npm publish readiness. No TypeScript SDK API change beyond the surfaces above.
- Dev cores report `1.3.0-dev`; packages for this line use `compatibility.alixai_core` `[1.0.0, 2.0.0)`.

## 1.2.0 — 2026-10-26

### Added

- Operation tables `me.ME_OPERATIONS` and `memory.MEMORY_OPERATIONS` for per-principal LLM accounts (ADR 0041) and long-term memory (ADR 0042); TurnEvent type `memory_proposal` / `MemoryProposalEvent`.
- Typed clients on `createClient`: `client.me` (`catalog`, `llm.get` / `putRole` / `deleteRole`, `providers.submitCredential` / `deleteCredential` / `acknowledgeBillingNotice` / `oauthStart`) and `client.memory` (`list` / `create` / `patch` / `delete` / `confirm` / `reject`). Fake-server routes for the same.
- **Host only:** `me.providers.submitCredential` submits a write-only key. Call it from the host server, never from browser code.
- Migration note: upgrade the SDK before a core that emits `memory_proposal` events (fail-closed decode).
- Core (1.2): `/v1/me/…` and `GET /v1/providers/catalog` answer when external mode is enabled; otherwise `unavailable` / `llm_not_configured`.
- `client.exports.create(sessionId, { turnId, format, blockIds? })` → `FileRef` (`origin: generated`) for downloadable files from a turn's verified blocks (`POST /v1/sessions/{session_id}/exports`, capability `exports.create`). Fake-server route and `FAKE_EXPORTS_SCOPES`.

### Notes (core)

- Dev cores report `1.2.0-dev`; packages for this line use `compatibility.alixai_core` `[1.0.0, 2.0.0)`.

## 1.1.0 — 2026-10-26

### Added

- Generated types for popup OAuth (`OAuthFlowStatus`, `OAuthPopupResult`) and optional start/response members (`presentation`, `callback`, `opener_origin`, `flow_id`, `poll_interval_ms`); credential kind `oauth_issued_key`.
- Generated types for dictation: `TranscriptionCapabilities`, `DictationTranscript` (record-to-text; not the voice WebSocket).
- Dictation HTTP client (`voice.createDictationClient` / `voice.dictations`): `capabilities()` and `create(sessionId, wav, opts)` for `GET /v1/dictations/capabilities` and `POST /v1/sessions/{session_id}/dictations`; helper `encodeWav16kMono`.
- Providers popup OAuth: poll/cancel flow helpers that speak `GET`/`DELETE /v1/oauth/flows/{flow_id}` and the optional start members above.

### Notes (core)

- Dev cores report `1.1.0-dev`; packages for this line use `compatibility.alixai_core` `[1.0.0, 2.0.0)`. See `docs/integration/guides/light-integration.md`.

## 1.0.0

First public release. Tested against the core in the same repository commit.

### Stable surface (what 1.0 freezes)

- The wire documents of `schemas/` that this SDK's generated types come from, frozen for v1 in `schemas/_frozen/v1` and checked by `tools/compat` (M6-01).
- The public API operations in `schemas/api/openapi.yaml` that this SDK speaks: the `OPERATIONS`, `rag.RAG_OPERATIONS`, `providers.PROVIDERS_OPERATIONS` and
  `decision.DECISION_OPERATIONS` tables, each checked against the document by a test.
- The error codes (`ERROR_CODES`) and their retry classes.

### Added

- Core client (`createClient`): sessions, streamed turns over server-sent events with sequence checks that fail closed, JSON turns, the single-use
  confirmation secret read from the response header, actions (get, confirm, reject, receipt), applications and capabilities, health probes (M1, M3-09).
- Context tokens as a per-request callback; the SDK never mints, caches or retries with one.
- Resumable streams: `streamTurnEvents` with a cursor, opt-in automatic reconnect, an opt-in WebSocket transport through a host-supplied factory with fallback
  to SSE.
- Messages with attachments, scoped files (`upload`, `get`, `download`, `preview`, `delete`), and file ids on a turn (M3-20, M3-29).
- Conversations (stop, regenerate, edit-and-resubmit, resume, switching), history (segments, search, export, clear, cleanup), privacy and settings.
- Rendering helpers: `viewTurn`, `blocksOfType`, `citationLink`.
- `rag` namespace: sources, indexing control, search and grounded answers with citations (M2-11).
- `voice` namespace: a client of the voice WebSocket with push to talk, an optional endpointer, transcript correction, verified speech playback with
  cancellation, and reconnect with a fresh token (M4-07). Voice never confirms an action.
- `providers` namespace: modes, egress policy and disclosure, budgets in integer micro-units, usage export, the kill switch, credentials (write-only) and the
  browser OAuth flow (X-14). External provider mode is opt-in and off by default.
- `decision` namespace: the read-only decision-layer status (D-10). Advisory only.
- `@alixai/sdk/testing`: an in-process fake of the API that enforces the protocol, plus fakes for voice, providers and decision status.

### Not in this package

- **Full-duplex voice is not claimed.** The voice client is push to talk with an optional endpointer; full duplex is not proven by any test.
- The operator-only decision configuration routes are not part of any SDK.
- AG-UI and CopilotKit are not here by design. `@alixai/copilotkit-adapter` is the optional integration, with its own version.
- The Lite profile is uncertified; no SDK behaviour depends on it.

### Changed

- README rewritten for the 1.0 surface: every code sample is copied from a test that runs, and the `voice`, `providers` and `decision` namespaces are no longer
  described as placeholders.
