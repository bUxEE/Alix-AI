# Changelog: @alixai/react

All notable changes to this package. Versions follow semantic versioning with the rules of
[compatibility.md](../../docs/policies/compatibility.md).

## 1.1.0 — 2026-10-26

### Added

- Dictation (record-to-text): `AlixDictationButton`, `useAlixDictation`, `insertAtCaret` / `insertIntoCopilotTextarea`, and the `@alixai/react/dictation` entry so hosts can ship dictation without pulling voice-conversation modules.
- Chat panel voice mode `off` | `dictation` | `conversation`: dictation inserts editable text into the composer; it never submits a turn.

## 1.0.0

First public release of the optional React UI kit.
