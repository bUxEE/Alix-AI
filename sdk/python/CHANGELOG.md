# Changelog: alixai (Python SDK)

All notable changes to this package. The format follows [Keep a Changelog](https://keepachangelog.com/); versions follow semantic versioning once 1.0.0
ships, with the rules of [compatibility.md](../../docs/policies/compatibility.md).

## 1.5.0 — 2026-10-27

### Added

- Analyst contracts (ADR 0047): generated models under `alixai.gen.analyst`; rich-block and turn-event extensions; `web_query` egress category; new error codes for analyst refusals.
- `DEFAULT_CLIENT_CAPABILITIES` and optional `client_capabilities` on turn inputs: the SDK announces what it parses; pass a subset to announce less, or `None` to omit (server = 1.4).
- Typed private-LLM ops: `ops.put_private_llm_endpoint` / tenant pin / principal binding use `alixai.gen.providers` models (`PrivateLLMEndpoint`, `TenantPrivateLLMPin`, …); responses still support 1.4 `res["key"]` via mapping compat.
- `report_step_up_outcome` — host-backend level-3 step-up outcome into the session (ADR 0048).
- Erasure `scope: "workspace"` for principal×workspace erase.
- Documented `me.put_role` with `private_endpoint_id` (bind a role to a private endpoint; `profile_id` must be that endpoint's `profile_id`).

### Notes

- Core: analyst companion (ADR 0047, off by default), step-up outcome (ADR 0048), workspace-scoped principal erasure (workspace-level erase no longer removes other workspaces' conversation history), llmbench quality mode, SSRF hardening, web search via SearXNG-compatible / Brave / Tavily.

## 1.4.1 — 2026-10-27

### Fixed

- `text_replaced` (ADR 0046): accepted in `TURN_EVENT_TYPES` (also `memory_proposal`); `turn_text` resets on replacement so joined text is the template only.

## 1.4.0 — 2026-10-27

### Added

- Ops private LLM routes (ADR 0045): endpoint CRUD, tenant pin, principal private binding under `/v1/ops/…`.
- `me.llm.put_role` accepts optional `private_endpoint_id` (mutually exclusive with `profile_id`).

### Notes (core)

- Private self-hosted OpenAI-compatible endpoints (`self_hosted`); tenant pin overrides personal external bindings inside that tenant; typed `private_llm_starting` / `private_llm_unavailable` with no third-party fallback.

## 1.3.0 — 2026-10-27

### Added

- Principal LLM role `tts` and optional `voice` / `language` on binding update bodies (ADR 0043). Meter unit `characters_in` in generated provider types.
- `client.me.oauth_callback` / `oauth_flow_status` / `oauth_cancel` for per-user host-callback OAuth.
- `client.ops.start_erasure` / `erasure_status` for GDPR erasure over HTTP (operator only).
- Optional `scope` on erasure start (`tenant` default / `application` for deployment-operator GDPR erase).
- `client.me.usage()` / `client.me.budget()`; optional `principal_id` on `providers.export_usage`.

### Notes (core)

- External TTS adapters (OpenAI, ElevenLabs, Google Cloud TTS, Amazon Polly, Mistral) behind `routing.tts` / principal `tts` bindings; attested verified text only; per-role provider choice (ADR 0043).
- Per-user usage view and application-wide principal budgets across tenants; Mistral chat + Voxtral STT/TTS (EU-default residency).
- CLI OAuth system-browser opener only follows validated `https` or loopback `http` URLs; light External hardware guide (4 vCPU / 8 GB).
- Opt-in `external_providers.directive_class` (live facts to the external model), verified table/chart blocks, and additive conversation-contract `presentation.chart` (ADR 0044).
- Outbound `credential_ref` for OpenAPI, MCP and RAG api (`application_credentials`); optional RAG `scope_params.include_principal_id`. Kit fullscreen z-index default 2400; npm publish readiness. No Python SDK API change beyond the surfaces above.
- Dev cores report `1.3.0-dev`; packages for this line use `compatibility.alixai_core` `[1.0.0, 2.0.0)`.

## 1.2.0 — 2026-10-26

### Added

- `client.me` / async `me` (catalog, LLM bindings, credential, billing ack, OAuth start) and `client.memory` / async `memory` (list/create/patch/delete/confirm/reject) for ADR 0041 / 0042.
- **Host only:** `submit_credential` submits a write-only key. Call it from the host server, never from browser code.
- Migration note: upgrade the SDK before a core that emits `memory_proposal` turn events.
- Core (1.2): `/v1/me/…` and `GET /v1/providers/catalog` answer when external mode is enabled; otherwise `unavailable` / `llm_not_configured`. Memory routes answer when `memory.enabled` is true.
- `client.exports.create(session_id, {turn_id, format, block_ids?})` → `FileRef` (`origin: generated`) for downloadable files from verified turn blocks (`exports.create`).

### Notes (core)

- Dev cores report `1.2.0-dev`; packages for this line use `compatibility.alixai_core` `[1.0.0, 2.0.0)`.

## 1.1.0 — 2026-10-26

### Added

- Generated models for popup OAuth flow status and popup result message; credential kind `oauth_issued_key`.
- Dictation models and client: `client.dictations.capabilities()` / `client.dictations.create(session_id, wav, …)` for record-to-text (`GET /v1/dictations/capabilities`, `POST /v1/sessions/{session_id}/dictations`).
- `providers.get_oauth_flow` / `cancel_oauth_flow` and optional `start_oauth` members `presentation`, `callback`, `opener_origin` (popup OAuth).

### Notes (core)

- Dev cores report `1.1.0-dev`; packages for this line use `compatibility.alixai_core` `[1.0.0, 2.0.0)`. See `docs/integration/guides/light-integration.md`.

## 1.0.0

First public release. Tested against the core in the same repository commit.

### Stable surface (what 1.0 freezes)

- The wire documents of `schemas/` that the generated models in `alixai.gen` come from, frozen for v1 in `schemas/_frozen/v1` and checked by `tools/compat` (M6-01).
- The public API operations of `schemas/api/openapi.yaml` that the SDK speaks (the `OPERATIONS` table, checked against the document by `tests/test_openapi_contract.py`).
- The error codes (`ERROR_CODES`) and their retry classes.

### Added

- Sync (`AlixAIClient`) and async (`AlixAIAsyncClient`) clients over `httpx`: sessions, streamed and JSON turns with sequence checks that fail closed, the single-use
  confirmation secret read from the response header, actions (get, confirm, reject, receipt), applications and capabilities, health probes (M3-11).
- Redirects are never followed, even through a host-supplied `httpx` client built with `follow_redirects=True`: a 3xx answer is an error
  (`details.reason: redirect`; `502 unavailable`, reason `upstream_redirect`, from the AG-UI proxy), so the token, a confirmation secret and the body
  never reach another origin.
- Context tokens as a per-request callback (a coroutine function is accepted by the async client); the SDK never caches or retries with a token.
- File ids on a turn (`attachments`, up to four) (M3-29).
- Models generated from the JSON Schemas (`alixai.gen`), with a drift check.
- `FakeAlixAIServer` (`alixai.testing`): an `httpx` transport that enforces the protocol and injects faults.
- `get_decision_status()`: the read-only decision-layer status (D-10). Advisory only.
- `client.providers`: modes, egress policy and disclosure, budgets in integer micro-units, usage export, the kill switch, credentials (write-only `Secret`) and the
  browser OAuth flow (X-14). External provider mode is opt-in and off by default.
- Readiness types (M9-06): `ReadinessReport`, `ReadinessFinding`, `ReadinessRequirement`, `ReadinessStatusView`, generated from `schemas/readiness`, so a host can
  read a stored report. No client method: readiness discovery is CLI-only and advisory.
- `alixai.agui`: an ASGI AG-UI proxy and a FastAPI-compatible router for CopilotKit and AG-UI frontends, AG-UI event types of the pinned release, an SSE encoder, and a
  server-side `SecretStore` for the confirmation secret (M3-27). It imports no CopilotKit or AG-UI library and sends no telemetry.

### Not in this package

- Resume from a cursor (`streamTurnEvents`) and a WebSocket transport. A lost stream is `uncertain_outcome`.
- Uploading, downloading or previewing files; conversation helpers; history, privacy and settings; the RAG client. These exist in the TypeScript SDK only (the Go SDK has RAG).
- Voice (TypeScript only). **Full-duplex voice is not claimed** by any SDK.
- The operator-only decision configuration routes.
- The Lite profile is uncertified; no SDK behaviour depends on it.

### Changed

- README rewritten for the 1.0 surface: every code sample is copied from a test that runs, and the stale statement that resuming a turn is missing "as in the
  TypeScript SDK" is gone (the TypeScript SDK has it; this one does not).
