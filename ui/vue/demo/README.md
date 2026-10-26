# @alixai/vue demo

An interactive showcase of every part of the Vue kit, running against the scripted mock backend in `ui/demo-mock` on the same
origin. No model, no cloud, no telemetry, no network beyond the local dev server.

## Run

```sh
pnpm install --frozen-lockfile --offline   # once, at the repository root
cd ui/vue
pnpm dev                                    # http://localhost:5173
pnpm demo:build                             # optional: a static build in ui/vue/demo-dist (needs `vite preview` for the mock)
```

`vite.config.ts` mounts `alixDemoMock()`, so the kit talks to `/v1/agui`, `/v1/files`, the settings routes and the voice socket
exactly as it would to a host's gateway. The demo is not published (`files` lists only `src`).

## Sections

| Section | What it shows |
| --- | --- |
| Chat | `AlixChatPanel` driven by the host's own `useAlixChat` controller, with a "Try these" rail from `demoPrompts`: streamed Markdown, bar and line charts, table, diagram (drawn by the demo's renderer through the `diagram` slot), image attachments (drawn through the `attachments` slot with the files API), citations, clarification choices, the risk 2 confirmation (sheet, then receipt or decline), the risk 3 step-up, progress with its percentage, an unavailable service, a refusal, a long multi-block answer, a cut stream that resumes, and a memory proposal (`memory: true`, host `onMemoryConfirm` / `onMemoryReject` against `/v1/memory/{id}/confirm|reject`). Fullscreen toggle on the panel header (same instance / draft survives). Download menus on table/chart blocks and answers (`exports` + a local fake `exporter` until the demo-mock export route exists). The voice control switches `voice` among `off` (chat only), `dictation` (record-to-text via `demoDictationClient`) and `conversation` (talk button). |
| Conversations | `AlixConversationList` on `/demo/conversations`: new, remove, refresh (overlay over the rows shown), a failed load with retry, and a skeleton shaped like the rows on first load. |
| Memory | `AlixMemoryProposal` (standalone) and `AlixMemoryList` on `/v1/memory`: edit, delete, refresh. The kit never calls AlixAI; the host supplies data and handlers. No per-user LLM account UI (that stays on the host server). |
| Voice | `AlixVoiceMode` on the mock voice socket (`/v1/sessions/{id}/voice`) with a simulated microphone unless you turn the real one on, speech through Web Audio, plus `AlixVoiceButton` in each state and `AlixTranscriptEditor` on its own. |

## Voice mode on the chat panel

Pass one prop: `voice: "off" | "dictation" | "conversation"` (default `off`, unchanged for 1.0 hosts). For dictation, also pass `dictationClient` from `voice.createDictationClient` (the demo uses `demoDictationClient()`). The default `AlixDictationButton` emits `transcript` with the recognised text (listen with `@transcript`); it never sends a turn. Import the control alone from `@alixai/vue/dictation` when you want record-to-text without the conversation orb or session driver.
| Uploads and files | `AlixAttachmentComposer` with the host's uploader (`POST /v1/files`), the size and type refusals, every file the mock holds and a preview: images through the safe preview route, CSV as text, PDF from its content. |
| Settings and status | `AlixSettingsPanel` over the effective settings (saved with `PUT /v1/settings/user` and its version), `AlixSourceHealthPanel` (refresh re-audits first) and `AlixIntegrationAuditPanel` with the providers' health. |
| Blocks gallery | Every block on its own with fixed data: charts, table, diagram, Markdown, clarification choices, action card, receipt, confirmation sheet, citations, file preview. |

Language (English, Italian) and theme (auto, light, dark) switch from the sidebar.
