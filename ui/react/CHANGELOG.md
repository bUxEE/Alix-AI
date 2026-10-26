# Changelog: @alixai/react

All notable changes to this package. Versions follow semantic versioning with the rules of
[compatibility.md](../../docs/policies/compatibility.md).

## 1.2.0 — 2026-10-26

### Added

- `AlixMemoryProposal` and `AlixMemoryList` for long-term memory (ADR 0042). The kit never calls AlixAI: the host provides confirm/reject and list data.
- Chat panel opt-in prop `memory` (default `false` for 1.1 compatibility) plus `onMemoryConfirm` / `onMemoryReject`; listens for `alixai.memory_proposal` CUSTOM events.
- i18n keys for memory (en + it).
- Chat panel fullscreen overlay: header toggle ("Full screen" / "Exit full screen"), controlled `fullscreen` + `onFullscreenChange` or uncontrolled `defaultFullscreen`, portal to `body` keeping the same panel node (draft, scroll and running turn survive), Esc to exit, focus trap/restore, body scroll lock, and a short enter transition that respects `prefers-reduced-motion`. `headerActions` stays available beside the toggle.
- Opt-in answer downloads: prop `exports` (default `false` for 1.1 compatibility) with host `exporter` and `download` / `onDownload`. Table and chart blocks get "Download CSV" / "Download XLSX"; assistant answers get "Download report (HTML)" / "Download Markdown". The kit never calls AlixAI; actions stay hidden without an exporter. Control-level loading and error states.
- i18n keys for fullscreen and export (en + it).

## 1.1.0 — 2026-10-26

### Added

- Dictation (record-to-text): `AlixDictationButton`, `useAlixDictation`, `insertAtCaret` / `insertIntoCopilotTextarea`, and the `@alixai/react/dictation` entry so hosts can ship dictation without pulling voice-conversation modules.
- Chat panel voice mode `off` | `dictation` | `conversation`: dictation inserts editable text into the composer; it never submits a turn.

## 1.0.0

First public release of the optional React UI kit.
