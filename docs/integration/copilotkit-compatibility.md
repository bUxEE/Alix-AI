# CopilotKit compatibility

AlixAI stays the authority layer; CopilotKit is an optional client and transport ([ADR 0035](../decisions/0035-copilotkit-agui-compatibility.md)).
This page publishes what is proven, against which releases, and how to wire a host. The AG-UI mapping itself is in
[agui.md](agui.md); the adapter's own guide is `sdk/copilotkit-adapter/README.md`.

**The claim is tested, not asserted.** `tests/copilotkit` (package `@alixai/copilotkit-gate`) drives the real `@ag-ui/client` and the real
`@copilotkit/runtime`, with `AlixAIAgent` from `@alixai/copilotkit-adapter`, over the 22 golden streams the Go gateway recorded
(`tests/agui/testdata`), and asserts every row of the feature matrix of [agui.md section 12](agui.md#12-feature-matrix-against-copilotkit):

```text
@ag-ui/client HttpAgent  ->  @copilotkit/runtime (CopilotRuntime v2 + AlixAIRunner)  ->  AlixAIAgent  ->  gateway (replays the goldens)
                                                                                                          (the Go gateway has its own tests)
```

```bash
pnpm install
pnpm --filter @alixai/copilotkit-gate test        # the gate: 100+ tests, about fifteen seconds, loopback only
pnpm --filter @alixai/copilotkit-adapter test      # the adapter's own suite (jsdom pages, wire rules, every cut point)
pnpm --filter @alixai/copilotkit-adapter compat    # both suites, for every entry of the version matrix below (scratch installs)
```

## Versions

| Entry | `@copilotkit/*` | `@ag-ui/client` and `@ag-ui/core` | Gate (`tests/copilotkit`) | Adapter suite |
| --- | --- | --- | --- | --- |
| `copilotkit-1.76.0` (primary, pinned in the workspace) | 1.76.0 | 1.0.1 (protocol 1.0) | passes (119 of 119) | passes (117 of 117) |
| `copilotkit-1.77.0` | 1.77.0 | 1.0.1 (protocol 1.0) | passes (119 of 119; scratch install, `node compat/run.mjs`) | passes (117 of 117; scratch install, `node compat/run.mjs`) |

**What `copilotkit-1.77.0` has run through.** Both suites: `node compat/run.mjs` runs the adapter suite and then the gate for every entry (it did only the adapter suite until M3-39).
The last run, in M6-08 (2026-10-08), passed both suites for both entries, so 1.77.0 is claimed as tested. Two defects of the repository, neither a 1.77.0 finding, had made the M6-07 run fail:

* **Gate, 1 failure:** the `predictive-state` row of the gate said `not claimed` while this page, `agui.md` and ADR 0035 (addendum) say `not emitted by design`. The row now agrees (it failed for `copilotkit-1.76.0` too).
* **Adapter suite, 3 failures (4 in a fresh M6-08 workspace, where the headless hook lost the same race):** `test/ui.v1.test.tsx` (the v1 `@copilotkit/react-ui` chat, sidebar and popup, and the headless hook) sent before the page had joined its thread.
  Since M3-39 the page asks the gateway for the thread's history (`agui.connect`), and a connect answered after the send replaces the messages the run set, so the answer never showed. It failed the same way on 1.76.0 in a fresh workspace;
  the tests then waited for the join, as the v2 tests did. M9-07 fixed the cause in the adapter (`AlixAIRunner` holds a connect that a run overlaps and reads it again after the run, see "What AlixAIRunner changes"), and the v1 `@copilotkit/react-ui` and headless tests and the late-connect tests send without waiting for the join (the v2 adapter tests and the gate's state tests still join: the stand-ins keep no session state, so their connect answers carry no pending goal where the real gateway's connect reads the state the run left, and a held connect must not be cut under the page when a stack is closed).

`sdk/copilotkit-adapter/compat/matrix.json` is the data; the gate's `package.json` pins and the installed packages are tested against its
primary entry (`test/matrix.test.ts`), so the two cannot drift. `node compat/run.mjs` (from `sdk/copilotkit-adapter`) runs the adapter suite **and** the gate for every
entry: the primary entry in place, every other entry in a scratch install laid out like the repository, with `ALIXAI_COMPAT_ENTRY` naming the entry so that both
suites check what is installed against it. `@copilotkit/*` before 1.76.0 depends on `@ag-ui/client` 0.0.59, which predates the
interrupt outcome and resume of the AG-UI release AlixAI implements (`@ag-ui/core` 1.0.1): not tested, not claimed.

## The feature matrix

Status: **shown** by a test of the gate through the real client and runtime; **shown with a limit** (the limit is pinned by a test and listed below);
**not emitted by design** and **not claimed** (a statement, with a test that nothing implies the opposite).
`ui.*` and `agent.*` are the adapter's suites (`sdk/copilotkit-adapter/test`); `Go` names a test of the gateway.

| Row | CopilotKit feature | Status | Proven by | Note |
| --- | --- | --- | --- | --- |
| `chat` | Chat, sidebar, popup and headless components (`useCopilotChat`) | shown | gate `chat.test.ts`, `ui.test.tsx` (v2 sidebar, v2 popup, a page with no runtime); adapter `ui.v2` (v2 chat), `ui.v1` (v1 chat, sidebar, popup, headless hook) | the structured blocks, the confirmation card and the suggestions need the v2 components: the v1 components of `@copilotkit/react-ui` stream the answer text and render no interrupt and no activity |
| `streaming` | Streaming messages | shown | gate `chat.test.ts` | a fragment reaches the page while the gateway is still producing the rest; the runtime does not buffer the stream |
| `lifecycle` | Run lifecycle, steps, errors | shown | gate `chat.test.ts`, `golden.test.ts` | a refused request shows its explaining text, then `RUN_ERROR`; never a success-looking finish |
| `history` | Message history on load (`connect`) | shown | gate `control.test.ts` (connect through the agent, a restarted host, a thread nobody spoke in), `ui.test.tsx` (a v2 page loads the history when it opens the thread; `AlixAIAgent.connectAgent` on a synthetic stream); Go `TestAGUIConnectReplaysTheThread` | the runtime's `connect` route goes through `AlixAIRunner` and `AlixAIAgent`: the host's callback is asked (operation `agui.connect`) and the gateway's connect run answers from the AlixAI session; see "What AlixAIRunner changes". A page with a self-managed `HttpAgent` has no `connect` (note below) |
| `frontend-actions` | Frontend actions and tools the model calls (`useCopilotAction`) | shown | gate `tools.test.ts`; Go `TestAGUIUIActionIsEmittedAndItsResultIsUntrusted` | a declared and registered UI tool comes back as one tool call; the result is data and never an approval; unregistered, undeclared and capability-named proposals fail whole |
| `backend-tool-calls` | Backend tool calls rendered in the UI | shown | gate `tools.test.ts` | the confirmation call carries the exact effects the host's data projected |
| `tool-results` | Deterministic tool results | shown | gate `tools.test.ts` | the receipt, byte for byte, is the result of the confirmation call; rejected and failed are results too |
| `approval` | Human-in-the-loop approval | shown | gate `tools.test.ts`, `control.test.ts`; adapter `ui.v2` (the card), `agent.golden` | **limit 3**; the confirmation secret is nowhere a page can read it |
| `generative-ui` | Generative UI | shown | gate `generative.test.ts` | every structured thing is an activity message of type `alixai.*` |
| `blocks` | Tables, charts, diagrams, forms | shown | gate `generative.test.ts`; adapter `ui.v2`, `react` (renderers) | table, chart and form from golden streams; **the diagram is shown on a synthetic stream: no golden holds a diagram block** |
| `citations` | Citations and source references | shown | gate `generative.test.ts` | opaque references only; the client applies the delta of each further source |
| `attachments` | Attachments and previews | shown | gate `generative.test.ts` | the refusal of inline bytes and addresses is the gateway's (Go intake tests, `internal/agui/input_test.go`), not shown through CopilotKit |
| `shared-state` | Shared state (`useCoAgent`) | shown | gate `generative.test.ts` | the run adds `/alixai` and never replaces the page's members; only the allow-list reaches the page |
| `readable-context` | Readable context (`useCopilotReadable`) | shown | gate `generative.test.ts`, `golden.test.ts`; Go `TestAGUIClientContextReachesTheEngineAsFencedData` | the gate shows the data reaches the gateway in its own slots and is never a header, a resume or a tool; the fencing in the prompt is shown by the Go tests |
| `state-render` | Agent state rendering (`useCoAgentStateRender`) | shown | gate `ui.test.tsx`, `generative.test.ts`; adapter `state.test.tsx` | v2 has no `useCoAgentStateRender`: the adapter ships `useAlixAIPendingState` and `AlixAIPendingState` (a page reads the pending goal), `AlixAIPendingCard`, and a chat renderer (`alixaiProviderProps(props, { showState: true })`, off by default) that draws it under the last message; an allow-list parser, text only |
| `predictive-state` | Predictive state updates | not emitted by design | gate `streams.test.ts` | the engine runs a turn to completion before its events exist, so there are no streamed tool arguments to predict from; state comes from the session's deterministic state and an unconfirmed prepared action must never look like state |
| `suggestions` | Chat suggestions | shown | gate `generative.test.ts`; adapter `ui.v2` (the pills) | same stream as the answer, no extra model call; picking one is an ordinary message |
| `run-control` | Stop, regenerate, retry, reconnect | shown | gate `control.test.ts`; adapter `agent.wire` (every cut point) | stop through the runtime's stop route and a bare browser disconnect both cancel the gateway run (**limit 1** for who may call stop); reconnect at five cut points, including inside the confirmation call |
| `reasoning` | Reasoning display | not emitted by design | gate `streams.test.ts` | model internals stay inside the core |
| `subagents` | Sub-agents | not emitted by design | gate `streams.test.ts` | one agent |
| `raw` | Raw provider events | not emitted by design | gate `streams.test.ts` | provider output is untrusted data the core re-expresses, never relays |
| `voice` | Voice | not claimed | gate `streams.test.ts` | text and transcripts only; **full duplex voice is not claimed until M4 proves it with a test** |
| `no-cloud` | Copilot Cloud, telemetry, public API key | shown | gate `telemetry.test.ts`, `streams.test.ts` (static scan); adapter `telemetry` | see "No cloud, no telemetry" |
| `transports` | Transports | shown with a limit | gate `control.test.ts`; Go `TestAGUIWebSocketMatchesSSE` | SSE is shown; **limit 4** |
| `thread-isolation` | Thread isolation (thread A cannot resume thread B's interrupt) | shown | gate `control.test.ts` | task row, not in agui.md; runs, resumes, **connect** and the runtime's thread routes: another user, an anonymous caller and a callback that mints for the wrong session reach nothing of the thread (the tests run the same probe against a runtime with CopilotKit's default runner first, where it reads Alice's confirmation effects, and against such a runtime living beside the AlixAI one, where it reads nothing) |

### What AlixAIRunner changes (found by the first gate, fixed in M3-39)

`CopilotRuntime`'s default runner answered the browser's `connect` from a process-wide in-memory log of the runs it had served: a request that named a
thread read that thread's events, the confirmation card's effects included, with no call to `AlixAIAgent`, to the host's token callback or to the gateway, and a
restarted host showed an empty history. Its local thread routes (`GET /threads`, `/threads/:id/messages`, `/threads/:id/events`, `/threads/:id/state`,
`POST /threads/clear`) listed every thread of the process, served what was recorded of any of them and wiped them, also with no authentication. The adapter now
builds the runtime with its own runner (`createAlixAIRuntime` ignores a runner of the host's: a runner of its own would silently bring the hole back):

* **`connect` goes through the agent.** The host's callback is asked for the caller whose headers came with the request (`operation: "agui.connect"`,
  `sessionId` = the thread, `forwardedHeaders` = what the runtime forwards), and the gateway's connect run answers from the AlixAI session, so the host's own user
  authentication and the gateway's scope check (the token's session must be the thread) apply to joining a thread exactly as they do to a run. There is no store of
  its own and no fallback: a refusal by the callback or the gateway ends the stream with `RUN_STARTED` and one `RUN_ERROR` (the typed error's own message and code)
  and not one event of any thread. History survives a restart because it is the gateway's.
* **The runtime's thread routes are off.** The runner does not advertise them, so they answer an error and carry no thread data.
* **A browser that disconnects cancels the gateway run.** When the response is closed before the run has ended, the runner stops the run exactly as the stop route does,
  and `AlixAIAgent` asks the gateway to cancel it (a run that ended on its own is never cancelled; a receipt that already exists stands either way). A page reload during
  a run therefore ends that run; a page that joins a thread gets the settled history of the gateway, not a rejoin of the run in flight.
* **A connect that a run overlaps is read again after the run.** CopilotKit's page does not wait for its connect before it lets the person send, and a connect's `MESSAGES_SNAPSHOT` and state event replace what the page holds.
  A connect read before the run was recorded and answered after it would therefore wipe the run's messages (and reset the pending-goal state): the answer sent in the instant after the page loads never showed. The runner
  keeps a flag per connect in flight (no event): when a run is, or becomes, in flight on the connect's own thread, the answer is dropped, the connect waits for the run to end and is read again through the agent, with a fresh
  token from the host's callback and the gateway's scope check, and the page gets the history that includes the run (the gateway settles the turn and reads the session state after it before it writes the run's first event, `internal/gateway/routes_v1_agui.go`, and a snapshot's message ids are the live stream's, so the second read holds the run's messages under the ids the page already has). A connect no run overlaps (a reload, a second tab, a genuine reconnect) is answered at once with its
  history and state, as before; a run on another thread never holds it; a connect the browser abandons while it waits is not read again; a refused connect still ends at once with `RUN_ERROR`. After three reads the answer
  carries no history or state, only `RUN_STARTED` and `RUN_FINISHED` (it never replaces a run's messages). A connect opened during a long run therefore answers when the run ends; a message sent in the few milliseconds between the end of that run and the delivery of the held answer is the one window left. Pinned by `ui.lateConnect.test.tsx` (the real page sends before the join and the second read is checked to have been made and applied,
  on every matrix entry) and `runner.lateConnect.test.ts` (including a genuine reconnect, answered at once with one read, and the three-read limit).
* **It writes nothing to CopilotRuntime's in-memory store.** That store is process-wide: every other `CopilotRuntime` of the same process with the default runner reads it. The adapter's
  runner keeps only the runs in flight (the agent and the run id of each, to stop it) and drops them when a run ends, so a co-resident default runtime reads nothing of an AlixAI thread (a test runs
  that probe).

### Limits that stay

These are pinned by tests, so a fix makes the test fail and this page is updated with it.

1. **The runtime's `info` and `stop` routes do not pass the caller to the runner or the agent, so the host's token callback is not asked about them.** `info` lists the
   agent's name and description; `stop` takes a thread id and a run id and aborts that run in the runtime, after which the gateway cancel is made with the run's own forwarded
   headers and scope-checked by the gateway: someone who can reach the route and knows both ids can end a run (a denial of service at worst: nothing is read, nothing is
   approved). **Put your own authentication in front of the whole runtime route** (see the quickstarts), whatever the runner does for `connect`.
2. **A request the gateway refuses before any stream (400, 403, 409, 428) reaches the page as `RUN_ERROR` with the gateway's message and CopilotKit's own code**, not
   the AlixAI error code. Show the message; do not branch on the code. (A refused `connect` is different: the adapter's runner writes the `RUN_ERROR` itself, with the AlixAI code.)
3. **Approve with `resume`.** `@ag-ui/client` 1.0.1 refuses a run on a thread with an interrupt that `resume` does not address, so the tool-message form of
   approval (`renderAndWaitForResponse`, accepted by the gateway) is not sendable from the same client while the interrupt is pending. `useInterrupt`, which the
   adapter's card uses, answers with `resume`.
4. **WebSocket cannot be shown with a CopilotKit or AG-UI client of the pinned releases**: `@ag-ui/client` 1.0.1 has no WebSocket agent or transport. The gateway serves
   it (Go `TestAGUIWebSocketMatchesSSE`, which covers the transport); the SDK proxies and the adapter use SSE.

Two further notes. `RUN_STARTED` reaches a CopilotKit page with an `input` member (the runtime echoes the browser's own request into it); the gateway never sends
one, and it holds nothing the page did not send. The gateway stand-in of the gate infers a scenario from the request text, which several goldens share, so every
run in the gate names its golden: that is a property of the stand-in, not of the gateway.

## No cloud, no telemetry

* The gate installs a guard before any CopilotKit module loads (`test/helpers/network-guard.ts`): every TCP and TLS connection (`fetch`, `http`, WebSocket), every
  name lookup and every datagram that is not loopback is refused and recorded, and the test that was running fails, even when the code that tried swallowed the
  refusal; a final check runs after the last test. Tests show the guard refuses CopilotKit's telemetry host and a public address, and that the whole chain's own
  connections go through it. **It covers the Node process of the suite only**: the Go and Python proxies are held to the same rule by their own SDK tests
  (`TestProxy...`, `sdk/python/tests/test_agui.py`).
* The switches that keep CopilotKit's runtime telemetry off (`COPILOTKIT_TELEMETRY_DISABLED`, `DO_NOT_TRACK`, `SCARF_ANALYTICS`, `SCARF_NO_ANALYTICS`) are set by
  `@alixai/copilotkit-adapter/runtime` before the runtime loads; the browser props drop `publicApiKey`, `publicLicenseKey` and `licenseToken` and turn the
  inspector, the dev console and the Intelligence indicator off (`alixaiProviderProps`). `pnpm-workspace.yaml` denies the install script of `@scarf/scarf`.
* A static scan of the SDK and adapter sources finds no analytics, tracing or cloud client.
* **OpenTelemetry.** In the JavaScript tree the only package is `@opentelemetry/api` (a peer of LangChain and OpenAI, which `@copilotkit/runtime` depends on). It is a
  no-op until an SDK registers a provider; the gate checks that none is registered in the process that served the runs, and the guard would stop any export. For a
  Python host, M3-27 noted that a FastAPI install brings OpenTelemetry; the repository's Python environment (`sdk/python/.venv`) has neither FastAPI nor any
  OpenTelemetry package, so the gate cannot name where it enters. The rule for such a host: the AlixAI SDK imports none and only calls the AlixAI base URL
  (the gate's static scan covers `sdk/python/src` and `pkg/sdk-go/agui`; the proxy only calls the base URL it was given, validated by `normalize_base_url`), an installed OpenTelemetry API does nothing until the host installs an SDK and an
  exporter and points them at an endpoint, and the standard switch `OTEL_SDK_DISABLED=true` keeps the SDK off.

## Quickstarts

A page needs the **v2** components (`@copilotkit/react-core/v2`) for the structured blocks, the confirmation card and the suggestions. The page code is the same for
every host (`sdk/copilotkit-adapter/README.md` has the full version); what changes is what serves it.

```tsx
import { CopilotChat, CopilotKitProvider } from "@copilotkit/react-core/v2";
import { alixaiProviderProps, useAlixAIConfirmation, useAlixAISuggestions } from "@alixai/copilotkit-adapter/react";

const AlixAI = ({ children }) => {
  useAlixAIConfirmation(); // the confirmation card; Approve and Decline answer the interrupt
  useAlixAISuggestions(); // the follow-up pills
  return children;
};

// A Next.js or Express host (CopilotKit's runtime in your backend):
const props = alixaiProviderProps({ runtimeUrl: "/api/copilotkit", agentId: "default" });
// A Go or Python host (the proxy serves AG-UI at POST /agui/, no CopilotKit runtime):
//   import { HttpAgent } from "@ag-ui/client";
//   const props = alixaiProviderProps({ agentId: "default", selfManagedAgents: { default: new HttpAgent({ url: "/agui/", threadId: sessionId }) } });

export const Assistant = ({ sessionId }) => (
  <CopilotKitProvider {...props}>
    <AlixAI>
      <CopilotChat agentId="default" threadId={sessionId} />
    </AlixAI>
  </CopilotKitProvider>
);
```

The thread is the AlixAI session: create the session with the SDK, give its id to the page as `threadId`, and mint the context token for that session.

### Next.js

```ts
// app/api/copilotkit/[[...path]]/route.ts
import { createAlixAIFetchHandler, createAlixAIRuntime } from "@alixai/copilotkit-adapter/runtime";

const alixai = createAlixAIRuntime({
  baseUrl: process.env.ALIXAI_URL!,
  // Asked once per gateway request. Authenticate the user from forwardedHeaders, check they own sessionId, mint a fresh single-use token for it.
  getContextToken: async ({ sessionId, forwardedHeaders }) => mintContextTokenFor(await userOf(forwardedHeaders), sessionId),
});
const handler = createAlixAIFetchHandler(alixai, { basePath: "/api/copilotkit" });

// Authenticate the WHOLE route, not only the runs: see limit 1 (the runtime's info and stop routes do not call getContextToken; connect does).
const guarded = async (request: Request): Promise<Response> =>
  (await userOf(Object.fromEntries(request.headers))) === undefined ? new Response("unauthenticated", { status: 401 }) : handler(request);
export const GET = guarded;
export const POST = guarded;
```

### Express

```ts
import express from "express";
import { createAlixAIExpressRouter } from "@alixai/copilotkit-adapter/runtime/express";
import { createAlixAIRuntime } from "@alixai/copilotkit-adapter/runtime";

const app = express();
app.use("/copilotkit", requireSignedInUser); // your middleware: before the router, for every method and path
app.use(createAlixAIExpressRouter(createAlixAIRuntime({ baseUrl: process.env.ALIXAI_URL!, getContextToken }), { basePath: "/copilotkit" }));
```

### Go host

The proxy (`pkg/sdk-go/agui`) lets the host serve the page from its own origin without CopilotKit's runtime: it validates the thread and run ids, calls your
callback for a fresh single-use token (session = thread), forwards to `POST /v1/agui`, keeps the confirmation secret server-side and streams the events back.

```go
proxy, err := agui.NewProxy(agui.Options{
	BaseURL: "http://127.0.0.1:8080",
	GetContextToken: func(ctx context.Context, req sdkgo.TokenRequest) (string, error) {
		// ctx carries the user your middleware authenticated; check they own req.SessionID (the thread), then mint a token for it.
	},
})
mux.Handle("/agui/", http.StripPrefix("/agui", requireSignedInUser(proxy.Handler()))) // POST /agui/ runs; /capabilities, /threads/{t}/runs/{r}/events and /cancel
```

### Python host

```python
from alixai.agui import AguiProxy

async def get_context_token(request, caller):
    # request.session_id is the thread; caller.headers are the incoming request's headers. Authenticate, check ownership, mint.
    ...

proxy = AguiProxy("http://127.0.0.1:8080", get_context_token)
app.mount("/agui", proxy)                              # any ASGI framework
# app.include_router(proxy.router(), prefix="/agui")   # FastAPI (imports FastAPI only when called)
```

**What proves which part.** The Next.js and Express wiring is the code the gate runs (the route handler of `createAlixAIFetchHandler` over `node:http`; the Express router
is built from the same runtime and is not served by the gate). The Go and Python proxies are proven by their own suites (`go test ./pkg/sdk-go/agui/...`,
`pytest sdk/python/tests/test_agui.py`: the golden streams unchanged, the secret kept server-side, resume and cancel, refusals before a token is spent); the gate
shows the page side of that wiring, a CopilotKit v2 page with no runtime reaching an AG-UI endpoint directly through `selfManagedAgents` (`ui.test.tsx`), against the
gateway stand-in, not against the proxies. Limit 1 does not apply to the proxies, which have no CopilotKit runtime and so no `info`, `stop` or `connect` route; for the same reason a page with a self-managed `HttpAgent` loads no history on its own (`@ag-ui/client` 1.0.1 does not implement `connect`: `connectAgent` answers with nothing and sends nothing; the gate pins it), so a Go or Python host that wants one replays the session history itself.
