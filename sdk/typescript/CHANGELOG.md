# Changelog: @alixai/sdk

All notable changes to this package. The format follows [Keep a Changelog](https://keepachangelog.com/); versions follow semantic versioning once 1.0.0
ships, with the rules of [compatibility.md](../../docs/policies/compatibility.md).

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
