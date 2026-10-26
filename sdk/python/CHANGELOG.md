# Changelog: alixai (Python SDK)

All notable changes to this package. The format follows [Keep a Changelog](https://keepachangelog.com/); versions follow semantic versioning once 1.0.0
ships, with the rules of [compatibility.md](../../docs/policies/compatibility.md).

## Unreleased

### Added

- Generated models for popup OAuth flow status and popup result message; credential kind `oauth_issued_key`.
- Dictation models and client: `client.dictations.capabilities()` / `client.dictations.create(session_id, wav, …)` for record-to-text (`GET /v1/dictations/capabilities`, `POST /v1/sessions/{session_id}/dictations`).
- `providers.get_oauth_flow` / `cancel_oauth_flow` and optional `start_oauth` members `presentation`, `callback`, `opener_origin` (popup OAuth, 1.1.0).

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
