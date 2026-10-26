# @alixai/react demo

An interactive showcase of every part of the React kit, running against the scripted mock backend in `ui/demo-mock` on the same
origin. No model, no cloud, no telemetry, no network beyond the local dev server.

## Run

```sh
pnpm install --frozen-lockfile --offline   # once, at the repository root
cd ui/react
pnpm dev                                    # http://127.0.0.1:5174
pnpm demo:build                             # optional: a static build in ui/react/dist-demo (needs `vite preview` for the mock)
```

`demo/vite.config.ts` mounts `alixDemoMock()` and resolves `@alixai/sdk`, the CopilotKit adapter and the kit itself from source, so
nothing has to be built first. `@vitejs/plugin-react` compiles the TSX and hot-swaps edited components (Fast Refresh). The demo is not published (`files` lists only `dist` and `src/styles`, and
`tsconfig.build.json` covers only `src`); `pnpm run typecheck` checks it with `demo/tsconfig.json`.

## Sections

| Section | What it shows |
| --- | --- |
| Chat | `AlixChatPanel` (CopilotKit's chat) with an `HttpAgent` on `/v1/agui`, as `CopilotChat`, `CopilotSidebar` or `CopilotPopup`, and a "Try these" rail from `demoPrompts`: streamed Markdown, bar and line charts, table, diagram, image attachments drawn by `renderPreview`, citations, the clarification form, the risk 2 confirmation (sheet, then receipt or declined line), the risk 3 step-up (prepared-action panel and "continue in the application" card), the progress line (`showProgress`), an unavailable service, a refusal, a long answer, and a memory proposal (`memory: true`, host `onMemoryConfirm` / `onMemoryReject` against `/v1/memory/{id}/confirm|reject`). Fullscreen toggle on the panel chrome (same node / draft survives). Download menus on table/chart blocks and answers (`exports` + a local fake `exporter` until the demo-mock export route exists). Host callbacks (`onAction`, `onOpenCitation`, `onContinueInApp`) show as notes at the bottom of the page. The voice control switches `voice` among `off` (chat only), `dictation` (record-to-text via `demoDictationClient`) and `conversation` (talk button). |
| Conversations | `AlixConversationList` on `/demo/conversations`: switch, create, delete; "Open in chat" loads the thread's history. |
| Memory | `AlixMemoryProposal` (standalone) and `AlixMemoryList` on `/v1/memory`: edit, delete, refresh. The kit never calls AlixAI; the host supplies data and handlers. No per-user LLM account UI (that stays on the host server). |
| Voice | `AlixVoiceMode` on the mock voice socket (a simulated microphone by default, so it works without one), plus `AlixVoiceButton` (toggle and push-to-talk) and `AlixTranscriptEditor` on their own. |

## Fullscreen on the chat panel

Fullscreen is a CSS state on the panel node (`position: fixed` with safe-area insets). The kit does **not** move the node in the DOM, so draft, scroll and the running turn survive by construction. A `transform` or `filter` on an ancestor creates a containing block for `position: fixed`: render the panel outside such ancestors, or wrap it in a portal of your own if the host layout needs that.

**Z-index.** Default is `2400` so the overlay sits above common UI-framework layers (Vuetify/MUI overlays start around 2000). This kit’s token prefix is `--alixai-*`, so the primary variable is **`--alixai-z-fullscreen`** (aliased as `--alix-fullscreen-z` for hosts that copy Vue docs). The Vue kit’s native name is **`--alix-fullscreen-z`**. Override with prop `fullscreenZIndex` or by setting the CSS variable on an ancestor. Menus and dialogs that stay in the panel’s DOM stack inside its stacking context; body-teleported overlays (typical framework dialogs) must use a z-index **higher** than the fullscreen value (raise the framework’s overlay base, or pass a lower `fullscreenZIndex`).

## Voice mode on the chat panel

Pass one prop: `voice: "off" | "dictation" | "conversation"` (default `off`, unchanged for 1.0 hosts). For dictation, also pass `dictationClient` from `voice.createDictationClient` (the demo uses `demoDictationClient()`). Recognised text is inserted at the composer caret (replacing any selection) and never sent. Import the control alone from `@alixai/react/dictation` when you want record-to-text without the conversation orb or session driver.
| Files | `AlixAttachmentComposer` uploading to `/v1/files`, sample files made in the page (PNG, CSV, and an SVG and an HTML page the mock refuses), `AlixFileList` with image and text previews, and a large preview: images and text from `/preview`, PDFs from `/content`. |
| Settings | `AlixSettingsPanel` as the editor of the user's own document (`PUT /v1/settings/user` with `expected_version`, conflict handling) or read only. |
| Source health | `AlixSourceHealthPanel` on `/v1/rag/sources` with refresh and re-audit, and its first-load and failed states. |
| Integration audit | `AlixIntegrationAuditPanel` on `/v1/settings/integration-audit` and `/v1/settings/providers`. |
| Blocks | Every block on its own with fixed data: chart (all five marks), table, confirmation sheet, prepared-action panel, step-up card, receipt (committed, queued, partial), action card, clarification form, citations, diagram, progress line, file list. |

Language (English, Italian) and theme (auto, light, dark) switch from the sidebar and apply to the kit and to CopilotKit's chat.

## Not in the demo

- **The reconnect scenario.** CopilotKit's `HttpAgent` does not resume a cut stream (the Vue kit's client does), so the React rail
  leaves it out; the mock marks it `kits: ["vue"]`. CopilotKit is not patched for it.
- **A new run while a confirmation is open.** `@ag-ui/client` refuses one until the interrupt is answered, so the rail is disabled
  and says so while a confirmation is pending.
- **An unavailable service and a refusal** end the run with `RUN_ERROR` after their text: the answer shows, and CopilotKit also logs
  the run error in the browser console. That is CopilotKit's behaviour, not a demo fault.
- **PDF previews of uploaded files.** The mock has no safe preview for PDFs and lets only its seeded files be downloaded, so only
  those open in the large preview.
- **The confirmation secret.** The page talks to the mock directly; a real host puts its CopilotRuntime with `AlixAIAgent` in
  between, which holds the secret. CopilotKit warns that `selfManagedAgents` is an Enterprise feature in production; the demo uses
  it only to reach the local mock.
