# Changelog: @alixai/vue

All notable changes to this package. Versions follow semantic versioning with the rules of
[compatibility.md](../../docs/policies/compatibility.md).

## 1.4.0 — 2026-10-27

### Added

- Handle AG-UI `CUSTOM` `alixai.text_replaced` (ADR 0046): discard superseded streamed assistant text; quiet caret while a message is streaming (respects reduced motion).

## 1.3.0 — 2026-10-27

### Added

- Fullscreen overlay z-index: CSS custom property `--alix-fullscreen-z` (default **2400**, above typical Vuetify/MUI overlays at ~2000) and prop `fullscreenZIndex` to override it. Body-teleported dialogs/menus opened from the panel must use a higher z-index than the overlay. npm publish readiness.

### Notes (core)

- Core 1.3.0: external TTS (OpenAI, ElevenLabs, Google, Polly, Mistral) and per-role provider choice (ADR 0043); `characters_in`; directive_class + verified tables/charts (ADR 0044); me OAuth callback/flows; ops erasure; credential_ref / application_credentials; me usage/budget; Mistral; browser-URL hardening; light External hardware docs.

## 1.2.0 — 2026-10-26

### Added

- `AlixMemoryProposal` and `AlixMemoryList` for long-term memory (ADR 0042). The kit never calls AlixAI: the host provides confirm/reject and list data.
- Chat panel opt-in prop `memory` (default `false` for 1.1 compatibility): when true, `alixai.memory_proposal` CUSTOM events render as proposal cards.
- i18n keys for memory (en + it).
- Chat panel fullscreen overlay: header toggle ("Full screen" / "Exit full screen"), `v-model:fullscreen` (default false), Teleport to `body` with the same panel instance (draft, scroll and running turn survive), Esc to exit, focus trap/restore, body scroll lock, and a short enter transition that respects `prefers-reduced-motion`. The `header-actions` slot stays available beside the toggle.
- Opt-in answer downloads: prop `exports` (default `false` for 1.1 compatibility) with host `exporter` and `download`. Table and chart blocks get "Download CSV" / "Download XLSX"; an assistant answer gets "Download report (HTML)" / "Download Markdown". The kit never calls AlixAI; actions stay hidden without an exporter. Control-level loading and error states.
- i18n keys for fullscreen and export (en + it).

## 1.1.0 — 2026-10-26

### Added

- Dictation (record-to-text): `AlixDictationButton`, `useAlixDictation`, `insertAtCaret`, and the `@alixai/vue/dictation` entry so hosts can ship dictation without pulling voice-conversation modules.
- Chat panel voice mode `off` | `dictation` | `conversation`: dictation inserts editable text into the composer; it never submits a turn.

## 1.0.0

First public release of the optional Vue 3 UI kit.
