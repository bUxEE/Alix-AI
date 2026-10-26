# Changelog: @alixai/demo-mock

All notable changes to this package. Versions follow semantic versioning with the rules of
[compatibility.md](../../docs/policies/compatibility.md).

## 1.2.0 — 2026-10-26

### Added

- Scripted `/v1/providers/catalog`, `/v1/me/*` and `/v1/memory*` routes for per-principal LLM accounts and long-term memory.
- AG-UI scenario `memory_proposal` (`alixai.memory_proposal` CUSTOM event) and capabilities `state.memory: true`.
- Scripted `POST /v1/sessions/{session_id}/exports` returning an `OriginGenerated` `FileRef` for report/download demos.

## 1.1.0 — 2026-10-26

### Added

- Offline dictation HTTP stand-in for the Vue and React demos: `GET /v1/dictations/capabilities` and `POST /v1/sessions/{session_id}/dictations` return scripted capabilities and transcript text (no model, no network).

## 1.0.0

First public release of the offline demo mock used by the Vue and React kit demos.
