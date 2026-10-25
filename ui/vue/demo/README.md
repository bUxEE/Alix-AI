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
| Chat | `AlixChatPanel` driven by the host's own `useAlixChat` controller, with a "Try these" rail from `demoPrompts`: streamed Markdown, bar and line charts, table, diagram (drawn by the demo's renderer through the `diagram` slot), image attachments (drawn through the `attachments` slot with the files API), citations, clarification choices, the risk 2 confirmation (sheet, then receipt or decline), the risk 3 step-up, progress with its percentage, an unavailable service, a refusal, a long multi-block answer and a cut stream that resumes. |
| Conversations | `AlixConversationList` on `/demo/conversations`: new, remove, refresh (overlay over the rows shown), a failed load with retry, and a skeleton shaped like the rows on first load. |
| Voice | `AlixVoiceMode` on the mock voice socket (`/v1/sessions/{id}/voice`) with a simulated microphone unless you turn the real one on, speech through Web Audio, plus `AlixVoiceButton` in each state and `AlixTranscriptEditor` on its own. |
| Uploads and files | `AlixAttachmentComposer` with the host's uploader (`POST /v1/files`), the size and type refusals, every file the mock holds and a preview: images through the safe preview route, CSV as text, PDF from its content. |
| Settings and status | `AlixSettingsPanel` over the effective settings (saved with `PUT /v1/settings/user` and its version), `AlixSourceHealthPanel` (refresh re-audits first) and `AlixIntegrationAuditPanel` with the providers' health. |
| Blocks gallery | Every block on its own with fixed data: charts, table, diagram, Markdown, clarification choices, action card, receipt, confirmation sheet, citations, file preview. |

Language (English, Italian) and theme (auto, light, dark) switch from the sidebar.
