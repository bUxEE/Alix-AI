# @alixai/demo-mock

A scripted, offline stand-in for the AlixAI gateway, for the demo apps of the Vue and React kits. It is a Vite plugin: `pnpm dev` serves the page and the mock from the same origin. There is no model, no network access and no telemetry. Every reply is scripted, in English and Italian, about a fictional company, Northwind Facilities.

## Wiring

```ts
// vite.config.ts of a demo app
import { alixDemoMock } from "../demo-mock/src/plugin";

export default defineConfig({ plugins: [vue(), alixDemoMock()] }); // options: { basePath?, pace?, maxFileMb?, voice?, dictation? }
```

The browser code imports `../demo-mock/src/index` for `demoPrompts` (the "try these" buttons), `DEMO_PATHS`, `DEMO_THREAD_ID` and a few helpers: `demoUploader`, `fetchDemoPreview` (returns a `blob:` URL), `demoVoiceOptions`, `demoDictationClient`, and `fetchDemoJson`. That file is browser safe.

- **Vue kit:**
  - `createAguiClient({ endpoint: DEMO_PATHS.agui })`, and `threadId` (start with `DEMO_THREAD_ID`).
  - `uploader: demoUploader()`, `language: "en" | "it"`.
  - The panels take the JSON of the `/v1/settings/*` and `/v1/rag/sources` routes, and `conversations` from `GET /demo/conversations`.
  - `AlixFilePreview` takes `src` from `fetchDemoPreview`.
  - `AlixChatPanel` does not forward `AlixMessageStream`'s slots, so it shows attachments as file cards. To draw the images inline, build the chat from `useAlixChat` and `AlixMessageStream`, and give it an `attachments` slot.
- **React kit:**
  - Use `<AlixChatPanel runtimeUrl={DEMO_PATHS.copilotRuntime} providerProps={{ selfManagedAgents: { default: new HttpAgent({ url: DEMO_PATHS.agui }) } }} />`. The runtime URL only answers CopilotKit's `/info` probe, and it lists no agents.
  - Pass `renderPreview` (fed by `fetchDemoPreview`) to draw image attachments. Pass `showProgress` to draw the progress line.
- **Voice (both kits):**
  - `voice.createVoiceSession(demoVoiceOptions(threadId, { language }))`. The mock asks for no token header.
- **Dictation (both kits):**
  - `demoDictationClient()` for `voice: "dictation"` on the chat panel. Import the button from `@alixai/vue/dictation` or `@alixai/react/dictation` when the host wants record-to-text without conversation mode.

## What it serves

| Route | What |
| --- | --- |
| `POST /v1/agui` | AG-UI over SSE, as the gateway maps a turn. It sends:<ul><li>`RUN_STARTED`</li><li>state as a snapshot, or as a delta when the client sent state</li><li>steps</li><li>`alixai.directive`</li><li>blocks as `alixai.block.*` activities</li><li>`alixai.citations` as a snapshot plus deltas</li><li>`alixai.progress`, `alixai.receipt` and `alixai.attachments`</li><li>text in fragments, with `id:` cursors</li><li>the `alixai_confirm_action` call and its interrupt</li><li>`alixai.suggestions`</li><li>`RUN_FINISHED` or `RUN_ERROR`</li></ul>It also handles resume, `forwardedProps.alixai.choiceId`, and connect runs (`messages: []` replays the thread). |
| `GET /v1/agui/threads/{t}/runs/{r}/events`, `POST …/cancel`, `GET /v1/agui/capabilities` | Read a run again from `Last-Event-ID`/`cursor`; cancel a run; the capabilities document. |
| `POST /v1/files`, `GET\|DELETE /v1/files/{id}`, `/content`, `/preview` | Multipart upload. The type is sniffed, uploads are limited to 8 MB, and SVG and HTML are refused. Raster and text files have previews. Fixtures (PNG floor plan and chart, PDFs, CSV) are generated at start. |
| `GET\|PUT /v1/settings/user`, `GET /v1/settings/{effective,profiles,capabilities,integration-audit,providers}`, `GET /v1/rag/sources`, `POST /v1/rag/sources/reaudit`, `/healthz`, `/readyz` | Panel data of the shapes in `schemas/api`. |
| `WS /v1/sessions/{id}/voice` | The voice protocol, in order:<ol><li>`session.started`</li><li>`capture.started`</li><li>interim transcripts</li><li>`transcript.final`</li><li>`turn.completed`</li><li>`response`</li><li>`playback.started`</li><li>the reply's own words as PCM chunks, recorded once with the machine's own voice (Windows speech, macOS `say`, `espeak-ng`) into the git-ignored `.voice-cache`, or silence where there is no voice</li><li>`playback.done`</li></ol>It also handles corrections, cancel and barge-in, and `?speak=false`. |
| `GET /v1/dictations/capabilities`, `POST /v1/sessions/{id}/dictations` | Record-to-text (not the WebSocket). Capabilities report local STT; create accepts `audio/wav` PCM s16le mono 16 kHz and returns a scripted transcript. Silent or too-short clips answer `validation_failed` (`details.reason: silent`). Option `dictation: "disabled"` hides the control; `dictation: "unavailable"` answers 503. |
| `GET\|POST /demo/conversations`, `DELETE /demo/conversations/{id}`, `GET /demo/files`, `/demo/copilotkit/info` | Mock-only helpers. |

## Prompts

Typed messages are matched by keyword in English or Italian; anything else gets the help reply.

| Scenario | Prompt |
| --- | --- |
| help | What can you do? |
| markdown | Give me an overview of our maintenance services |
| chart_bar | Show open work orders per site as a bar chart |
| chart_line | Plot monthly energy use at Harbor Point and Westfield |
| table | List the open work orders |
| diagram | Draw the work-order process as a diagram |
| image | Show me the floor plan of Harbor Point level 3 |
| citations | What does the maintenance policy say about HVAC filters? |
| clarification | Schedule a fire-safety inspection at Riverside Depot. A choice leads to a confirmation. |
| confirmation | Replace the HVAC filters at Harbor Point. This is a risk 2 action: a card, then the confirmation sheet, then a receipt or a decline. |
| step_up | Terminate the cleaning contract with Brightline. This is risk 3, so the assistant hands it to the application to continue. |
| progress | Export the Q3 maintenance report |
| unavailable | Check the live sensor data for Westfield Data Hall |
| refusal | Export everyone's salary details |
| long | Give me the full Q3 facilities report |
| reconnect | Simulate a dropped connection. The stream is cut once and the Vue client resumes it. |

The mock does not keep or check the confirmation secret. A demo page talks to it directly, without the host relay that would hold the secret.

`pnpm --filter @alixai/demo-mock test` checks every stream with the kits' own clients and parsers (`createAguiClient`, the reducer, `useAlixChat`, the voice controller, and `@ag-ui/client`'s `HttpAgent`). It also validates every document against `schemas/`.
