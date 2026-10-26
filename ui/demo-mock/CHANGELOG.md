# Changelog: @alixai/demo-mock

All notable changes to this package. Versions follow semantic versioning with the rules of
[compatibility.md](../../docs/policies/compatibility.md).

## 1.1.0 — 2026-10-26

### Added

- Offline dictation HTTP stand-in for the Vue and React demos: `GET /v1/dictations/capabilities` and `POST /v1/sessions/{session_id}/dictations` return scripted capabilities and transcript text (no model, no network).

## 1.0.0

First public release of the offline demo mock used by the Vue and React kit demos.
