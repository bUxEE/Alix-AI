# Guide: a CopilotKit or AG-UI chat in front of AlixAI

Status: written in task M6-05. Decision: ADR [0035](../../decisions/0035-copilotkit-agui-compatibility.md).

AlixAI streams to a CopilotKit page or any AG-UI client, and stays the authority layer: CopilotKit is an optional client and transport, needs no CopilotKit Cloud and sends no telemetry. This guide says which
route to take for which host, then points at the pages that hold the code and the proof. It repeats neither the version matrix nor the quickstarts, because a copy has no test holding it to the data.

**Prerequisite.** An active, audited application ([getting started](getting-started.md)). The thread of the chat is the AlixAI session: create the session with an SDK, hand its id to the page as `threadId`, and
mint the context token for that session.

## Which route

| Your host | What serves AG-UI to the browser | Package | Quickstart |
| --- | --- | --- | --- |
| Next.js | CopilotKit's runtime in a route handler, with `AlixAIAgent` and `createAlixAIFetchHandler` | `@alixai/copilotkit-adapter` (`sdk/copilotkit-adapter`) | [Next.js](../copilotkit-compatibility.md#nextjs) |
| Express (Node) | the same runtime, mounted as an Express router | `@alixai/copilotkit-adapter` | [Express](../copilotkit-compatibility.md#express) |
| Go | the AG-UI proxy, a `net/http` handler; no CopilotKit runtime | `pkg/sdk-go/agui` | [Go host](../copilotkit-compatibility.md#go-host) |
| Python | the AG-UI proxy, an ASGI app or a FastAPI router; no CopilotKit runtime | `alixai.agui` (`sdk/python`) | [Python host](../copilotkit-compatibility.md#python-host) |

The page side is the same for every host: the **v2** components of `@copilotkit/react-core` (the structured blocks, the confirmation card and the suggestions need them) and `alixaiProviderProps`,
`useAlixAIConfirmation` and `useAlixAISuggestions` from `@alixai/copilotkit-adapter/react` (the adapter's README has the full page). For a Go or Python host the page uses a self-managed `HttpAgent` pointed at the proxy
instead of a runtime URL. The Quickstarts section of the compatibility page shows both.

## What every host must do

1. **Authenticate your own user before AlixAI sees the request.** The token callback you give the adapter or the proxy receives the thread and the caller's headers; check that the user owns the thread, then mint a
   **fresh single-use context token** for it. The runtime's `info` and `stop` routes of CopilotKit do not call your callback, so put your own authentication in front of the whole runtime route (limit 1 on the
   compatibility page). The Go and Python proxies have no such routes.
2. **Approve with `resume`.** The confirmation card answers the AG-UI interrupt with a resume. The single-use confirmation secret stays on the server (the adapter's runner and the proxies keep it under the thread and
   preparation) and reaches AlixAI only with a resume that explicitly approves that preparation; the browser never holds it, and text can never approve anything.
3. **Treat client-supplied tools, state, context and messages as untrusted data.** They are never capabilities or authority; a UI tool the page declares comes back as one tool call whose result is data.
4. **Keep telemetry off.** The adapter sets CopilotKit's telemetry switches before the runtime loads and drops cloud keys from the browser props; the proxies contact only the AlixAI URL you give them.

## What is proven, against which releases

The compatibility page, [copilotkit-compatibility.md](../copilotkit-compatibility.md), is the published contract and the only place to read the version matrix (which `@copilotkit/*` releases are claimed, against
which `@ag-ui` release, and what was run for each). It lists, row by row, the CopilotKit features shown by a test, the ones shown with a limit, and the ones not emitted by design (predictive state, reasoning,
sub-agents, raw events). **Full-duplex voice is not claimed**: the stream carries text and transcripts only.
compatibility page does; read the page, which is generated from the tested matrix.)

The AG-UI event schemas (`schemas/_frozen/v1/agui`) and the OpenAPI document, which includes `GET /v1/agui/capabilities` (what the gateway supports), are in the set that M6-01 froze for v1 and that
`tools/compat` checks. The pinned AG-UI release is part of the contract; moving it follows the rules of [compatibility](../../policies/compatibility.md), section 4.

To rerun the proof yourself:

```bash
pnpm install
pnpm --filter @alixai/copilotkit-gate test        # the gate, loopback only
pnpm --filter @alixai/copilotkit-adapter test      # the adapter's own suite
pnpm --filter @alixai/copilotkit-adapter compat    # both, for every entry of the version matrix
go test ./pkg/sdk-go/agui/...                      # the Go proxy
```

The commands are those of the compatibility page; the Python proxy is `pytest sdk/python/tests/test_agui.py` from `sdk/python`.
