# @alixai/copilotkit-adapter

The optional [CopilotKit](https://copilotkit.ai) and [AG-UI](https://docs.ag-ui.com) integration of AlixAI (ADR 0035). CopilotKit's chat, sidebar, popup and headless components talk to AlixAI's `POST /v1/agui` endpoint through
`AlixAIAgent`, an AG-UI agent that runs in **your backend**, inside CopilotKit's own runtime. AlixAI stays the authority layer;
CopilotKit is only a client and a transport. Nothing here needs CopilotKit Cloud, and the adapter sets the switches that keep
CopilotKit's telemetry off.

`@alixai/sdk` does not depend on CopilotKit or AG-UI, and this package is not needed to use it. `@copilotkit/*`, `@ag-ui/*`,
`react`, `react-dom` and `rxjs` are optional peer dependencies: install the ones the entry points you use import.

| Entry point | For | Needs |
| --- | --- | --- |
| `@alixai/copilotkit-adapter` | `AlixAIAgent`, the secret store, the telemetry switches, parsers for AlixAI's AG-UI payloads | `@ag-ui/client`, `@ag-ui/core`, `rxjs` |
| `@alixai/copilotkit-adapter/runtime` | a `CopilotRuntime` with the agent and `AlixAIRunner`, and handlers for a Next.js route or `node:http` | `@copilotkit/runtime` |
| `@alixai/copilotkit-adapter/runtime/express` | the same as an Express router | `@copilotkit/runtime`, `express` |
| `@alixai/copilotkit-adapter/react` | the confirmation card, citation list, attachment preview, block renderers, suggestions, the pending-goal state renderer | `react`, `@copilotkit/react-core` |

## Use with CopilotKit

### 1. The route in your backend (Next.js)

```ts
// app/api/copilotkit/[[...path]]/route.ts
import { createAlixAIFetchHandler, createAlixAIRuntime } from "@alixai/copilotkit-adapter/runtime";

const alixai = createAlixAIRuntime({
  baseUrl: process.env.ALIXAI_URL!, // your AlixAI gateway
  // Called once for every request AlixAIAgent makes: mint a fresh, single-use context token for this user and this thread.
  getContextToken: async ({ sessionId, forwardedHeaders }) => {
    const user = await authenticate(forwardedHeaders); // the browser's Authorization / cookie headers, as CopilotRuntime forwards them
    // Mint only for a session this authenticated user owns, and refuse otherwise: the thread id comes from the browser's
    // input, and the gateway enforces the session check too, but your callback is the first line of defence.
    if (!(await userOwnsSession(user, sessionId))) throw new Error("not your session");
    return mintContextToken({ user, sessionId }); // your signing code; the SDK never mints tokens
  },
});

const handler = createAlixAIFetchHandler(alixai, { basePath: "/api/copilotkit" });
// Authenticate the WHOLE route in front of the handler (your session middleware): see "What the host must still do" below.
export const GET = handler;
export const POST = handler;
```

Express: `app.use(createAlixAIExpressRouter(alixai, { basePath: "/copilotkit" }))` from `@alixai/copilotkit-adapter/runtime/express`.
`node:http`: `http.createServer(createAlixAINodeHandler(alixai, { basePath: "/copilotkit" }))`.

`createAlixAIRuntime` builds the runtime with **`AlixAIRunner`**, and takes no runner of yours (a runner of your own would silently bring back what it fixes): CopilotRuntime's
default runner answers the browser's `connect` (the page joining a thread) from its own process-wide in-memory log, and its local thread routes (`GET /threads`,
`/threads/:id/events|messages|state`, `POST /threads/clear`) list and serve that log, none of it asking your token callback or the gateway. `AlixAIRunner` sends `connect`
through the agent instead (the callback is asked with `operation: "agui.connect"`, and the gateway's connect run answers from the AlixAI session), keeps no history of its own,
has no thread routes, never writes to CopilotRuntime's process-wide in-memory store (which every other default `CopilotRuntime` of the process reads), and cancels the gateway run when
the browser disconnects before the run has ended.

The **thread is the AlixAI session**: AlixAI refuses a run whose `threadId` is not the session id of the context token. Create the
session with `@alixai/sdk` (`createSession()`), give its id to the page, and pass it as `threadId`. `sessionId` in the token
callback is that same id.

### 2. The page (React)

```tsx
import { CopilotChat, CopilotKitProvider } from "@copilotkit/react-core/v2";
import { alixaiProviderProps, useAlixAIConfirmation, useAlixAISuggestions } from "@alixai/copilotkit-adapter/react";

const AlixAI = ({ children }) => {
  useAlixAIConfirmation(); // the confirmation card in the chat; Approve and Decline answer the interrupt
  useAlixAISuggestions(); // the follow-up suggestions of the last run, as CopilotKit's suggestion pills
  return children;
};

export const Assistant = ({ sessionId }) => (
  // alixaiProviderProps: no cloud keys, inspector / dev console / Intelligence indicator off, and the renderers for
  // tables, charts, diagrams, action cards, clarification forms, citations and attachments.
  <CopilotKitProvider {...alixaiProviderProps({ runtimeUrl: "/api/copilotkit", agentId: "default" })}>
    <AlixAI>
      <CopilotChat agentId="default" threadId={sessionId} />
    </AlixAI>
  </CopilotKitProvider>
);
```

`CopilotSidebar` and `CopilotPopup` of `@copilotkit/react-core/v2` are used the same way. The v1 components of
`@copilotkit/react-ui` (`CopilotChat`, `CopilotSidebar`, `CopilotPopup`) and the headless `useCopilotChat` of
`@copilotkit/react-core` work with the same route and stream the verified answer; they render no activity messages, so use the v2
components for the structured blocks and the confirmation card (`compat/matrix.json` has the details per release).

Helpers: `AlixAIConfirmationCard` (the card behind `useAlixAIConfirmation`, usable on its own with the render props of
`useInterrupt`), `AlixAICitationList`, `AlixAIAttachmentPreview` (you supply `renderPreview`: the adapter never builds a file URL),
`AlixAITable`, `AlixAIChart`, `AlixAIDiagram`, `AlixAIActionCard`, `AlixAIChoices` (a clarification: buttons that run the agent with
`forwardedProps.alixai.choiceId`, plus free text when the block allows it) and `createAlixAIActivityRenderers({ components })` to
replace any of them with your own component. They are unstyled and render every value as text.

**State rendering** (the `useCoAgentStateRender` row; v2 has no such hook): the pending goal of the conversation (what it waits for: a confirmation, an answer to a question, the
fields it has and still needs) lives in the shared state under `alixai`. `useAlixAIPendingState()` returns it parsed through an allow-list (undefined when there is none),
`<AlixAIPendingState />` draws it anywhere inside the provider, `AlixAIPendingCard` draws a value you hold, and `alixaiProviderProps(props, { showState: true })` draws it in the
chat under the last message (off by default). Like every other helper it renders text only.

### 3. Human approval

A consequential action ends the run with an interrupt. The card shows the **exact effects** AlixAI projected from the prepared
action; Approve answers `{ "approved": true }`, Decline `{ "approved": false }`, and CopilotKit resumes the run. The answer as a tool
message for `alixai_confirm_<preparation id>` (the `renderAndWaitForResponse` form) is accepted as well.

## What the adapter guarantees

- **The confirmation secret never reaches the browser.** The gateway returns it in the `X-AlixAI-Confirmation-Token` header of the
  run that prepared the action. `AlixAIAgent` keeps it per thread and preparation in a server-side store (`createMemorySecretStore()`
  by default; pass your own `secrets` store for several processes), attaches it to the approving request only and deletes it on use.
  It is not an event, not a message, not a header of the runtime's response; an event that carries it is withheld. An approval with
  no secret held fails before a token is spent. `test/ui.v2.test.tsx` records everything the page receives and searches it for the
  secret, which the gateway side visibly carried.
- **Nothing of the browser's reaches AlixAI except the AG-UI input.** CopilotRuntime forwards `authorization` and custom `x-*`
  headers of the browser request to the agent; the agent hands them to your token callback (`forwardedHeaders`, so you can tell who
  is asking) and never sends them on. `forwardedProps` is reduced to `alixai.choiceId` and `alixai.language`.
- **Client tools and readable context are data.** `useFrontendTool` declarations, `useAgentContext` / `useCopilotReadable` items,
  the shared state and earlier messages are passed to AlixAI as they are; AlixAI fences them as untrusted data, never as a
  capability. A UI tool call the host registered is run by your page and its result goes back as data; the adapter never lists
  client tools or context as AlixAI capabilities.
- **Joining a thread is authenticated like a run.** `connect` goes through your token callback (`operation: "agui.connect"`, a value that is new in this release: a callback that
  switches on `operation` must handle it, and one that refuses unknown operations refuses page loads until it does; the thread as `sessionId`, the caller's
  forwarded headers) and the gateway's connect run, whose scope check requires the token's session to be the thread. A refusal ends the stream with one `RUN_ERROR` and no event
  of any thread; nothing is read from the runtime's memory (the gate in `tests/copilotkit` runs the same probe against CopilotKit's default runner first, where it reads a thread with no callback and no gateway).
- **A browser that goes away cancels the run.** Closing the connection before the run ends cancels it at the gateway, as the runtime's stop route does.
- **Single-use tokens, safe retries.** A context token is asked for once per request (the run, a reconnect, a cancel). A request
  that never got an answer is sent again under the same `runId` (AlixAI answers a recorded run, never a second turn); a cut stream
  is resumed from its cursor (`Last-Event-ID`) without losing or repeating an event; `abortRun()` cancels locally and tells the gateway.
- **No telemetry, no cloud.** See below.

## What the host must still do

**Authenticate the whole runtime route** (every method and path) with your own middleware, in front of the handler or the router. `AlixAIRunner` makes `connect` ask your
callback and the gateway, but the runtime's `info` route and its `stop` route are answered without the caller reaching the runner or the agent: `stop` takes a thread id and a
run id and aborts that run, so anyone who can reach the route and knows both can end it. The callback and the gateway remain the authority for what a thread may show or do.

## Telemetry

CopilotKit's runtime sends anonymous `oss.runtime.*` events to `telemetry.copilotkit.ai` unless told not to, and its packages pull in
`@scarf/scarf` (install-time analytics). Read from the pinned releases, the switches are `COPILOTKIT_TELEMETRY_DISABLED=true`,
`DO_NOT_TRACK=1`, `SCARF_ANALYTICS=false` and `SCARF_NO_ANALYTICS=true`. The runtime reads the first two **once, when it loads**, so
the `/runtime` entry points import `./telemetry-off` first, which sets all four; if your code imports `@copilotkit/runtime` before the
adapter, set them in the process environment instead (`applyTelemetryOff()` and `telemetryIsOff()` are exported). pnpm 12 refuses to
finish an install that leaves the scarf build undecided: put `allowBuilds: { '@scarf/scarf': false }` in `pnpm-workspace.yaml` (this
repository does). In the browser, `withoutCloud(props)` / `alixaiProviderProps(props)` drop `publicApiKey`, `publicLicenseKey` and
`licenseToken` and set `enableInspector`, `showDevConsole` and `showIntelligenceIndicator` to `false`.

The test suite installs a guard (`test/helpers/network-guard.ts`) before any CopilotKit module loads: every connection and name lookup
that is not loopback is refused and fails the test that made it, even when the code that tried swallowed the error.

## Versions

`compat/matrix.json` lists the CopilotKit and AG-UI releases the suite is tested against (AG-UI `@ag-ui/core` 1.0.1, protocol 1.0, the
release the Go core implements). `pnpm --filter @alixai/copilotkit-adapter compat` runs the suite **and the compatibility gate** (`tests/copilotkit`) for every entry (the
workspace's own in place, the others in a scratch install); see `compat/README.md`. The `@alixai/sdk` import resolves to the SDK's sources in the
suite and to its build when published, so build the SDK first (`pnpm --filter @alixai/sdk build`) before building this package.

## Tests

`pnpm --filter @alixai/copilotkit-adapter test` and `typecheck`. The suite replays the golden streams of the gateway
(`tests/agui/testdata`) through `AlixAIAgent` and the real `@ag-ui/client`, and drives the real `@copilotkit/react-core`,
`@copilotkit/react-ui` and `@copilotkit/runtime` in jsdom against a loopback stand-in of the gateway.
