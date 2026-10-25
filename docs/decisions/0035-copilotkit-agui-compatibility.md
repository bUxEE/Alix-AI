# 0035 - CopilotKit and AG-UI compatibility: a protocol endpoint in the core, an optional adapter package, AlixAI stays the authority

- **Status:** accepted (finalised in M3-24 after a security review of the confirmation handling, below)
- **Date:** 2026-10-03
- **Decided in task:** M3-24 (internal/agui), M3-25 (gateway), M3-26 (adapter), M3-27 (Go and Python), M3-28 (compatibility gate)

## Context
The owner's rule (2026-10-03): the SDKs must be compatible with the CopilotKit UI API and protocol, including all of its features. CopilotKit is a React framework for in-app copilots
(chat, sidebar, popup and headless components; frontend actions and tools; human-in-the-loop; generative UI; shared agent state; readable context; suggestions). Its open protocol is
AG-UI (Agent-User Interaction Protocol): an event stream over HTTP SSE or WebSocket with run lifecycle events (RunStarted, RunFinished with an optional success or interrupt outcome, RunError,
StepStarted, StepFinished), text messages (Start, Content, End and Chunk forms), tool calls (Start, Args, End, Result and Chunk), state (StateSnapshot, StateDelta as JSON Patch, MessagesSnapshot),
activity (ActivitySnapshot, ActivityDelta), reasoning events, sub-agent events, Raw and Custom events; the input is RunAgentInput (threadId, runId, messages, tools, state, context,
forwardedProps and a resume list for interrupts). CopilotKit's runtime connects to agents through AG-UI (HttpAgent or a custom agent).

## Options
- **A. Only a TypeScript adapter that translates our stream.** Every host must run it; Go and Python hosts get nothing.
- **B. An AG-UI endpoint in the core (the protocol) plus an optional adapter package and thin helpers in the Go and Python SDKs.** One mapping, tested once, usable from any host.
- **C. Make AG-UI the only API.** Couples the core to a third-party protocol and its releases.

## Decision
Option B. AG-UI is a transport and UI protocol next to our own; our TurnEvent API stays and nothing existing changes.
- **Core (Go):** `internal/agui` maps TurnEvents to AG-UI events and RunAgentInput to a turn request; the gateway serves it at `POST /v1/agui` (SSE and WebSocket, resumable, cancellable) with the same context-token scope rules as /v1/sessions;
  `GET /v1/agui/capabilities` states honestly what is supported.
- **Optional package `@alixai/copilotkit-adapter`** (sdk/copilotkit-adapter): an `AlixAIAgent` for CopilotRuntime and CopilotKit components (chat, sidebar, popup, headless), typed helpers for confirmation cards, citations,
  attachments and the structured blocks (charts, tables, forms), reconnect and retry. CopilotKit and AG-UI are optional peer dependencies; `@alixai/sdk` never depends on them.
- **Go and Python SDKs:** AG-UI event types and a proxy handler (net/http, ASGI) so a host backend in those languages can serve a CopilotKit frontend through AlixAI.
- **Authority is unchanged.** Client-supplied `tools` are UI-only declarations the client executes; they never become capabilities and a model proposal cannot call anything the host did not register.
  Client `state`, `context` items and earlier `messages` are untrusted data: fenced, bounded, never authority; client `system` or `developer` messages are demoted to untrusted context.
  Capabilities act only through prepare, confirm and commit.
- **Human approval:** a PreparedAction ends the run with `RunFinished` outcome interrupt (reason confirmation_required, the exact effects, the preparation id as interrupt id); `resume` with approve or reject maps to
  confirm or reject. The confirmation secret never appears in an event: it travels in a response header to the SDK agent in the host backend, which keeps it per thread and attaches it to the resume
  (the browser never holds it). Finalised by the security review below.
- **Not emitted by design:** reasoning and encrypted-reasoning events (model internals never leave; debug and trace logs already leak completions), sub-agent events (one agent) and Raw. Also not emitted, added 2026-10-04 by the M3 gate: predictive state updates (the engine runs a turn to completion before its events exist, so there are no streamed tool arguments to predict from, and showing an unconfirmed prepared action as state would blur the prepare, confirm and commit boundary). Each is documented with its reason.
- **Voice:** compatibility is stated only for what is tested; full duplex voice is not claimed until a test proves it (capabilities endpoint says so).
- **No cloud, no telemetry:** nothing in the core or the adapter requires CopilotKit Cloud or sends telemetry; the adapter documents and tests the switches that keep CopilotKit's own telemetry off, and a test fails on any
  non-loopback connection.
- **Versions:** a tested compatibility matrix (CopilotKit and AG-UI versions) is part of the repository and CI; the endpoint declares the AG-UI protocol version it implements.

## Consequences
- Five tasks (M3-24 to M3-28) join milestone M3 and its gate; the optional UI kits (M7) build on the adapter instead of a competing chat UI.
- Every emitted event is validated against schemas vendored from the pinned AG-UI release, so protocol drift fails a test instead of a customer.
- The Go and Python SDKs stay free of CopilotKit; the TypeScript SDK core stays free of it too.

## Security review of the confirmation handling (M3-24)
Reviewed against the pinned release (`@ag-ui/core` 1.0.1, protocol 1.0) and the code of `internal/agui`, `internal/actions` and the
sessions route. Details and test names: `docs/integration/agui.md`, sections 3 to 7.

| Threat | Handling | Residual risk |
| --- | --- | --- |
| The model confirms an action | it has no path: the model never sees the secret, a confirmation is a structured input (`resume` or a tool result), and the mapping makes no tool call from model text | none in the core |
| The confirmation secret reaches the browser, a log or a proxy | it is not a TurnEvent member; it travels only in the `X-AlixAI-Confirmation-Token` response header to the SDK agent in the host backend and back in the same request header, never in a body; `Encode` refuses credential-shaped members and any value in the run's withheld list; `ConfirmationToken` prints as `[redacted]` | the SDK agent (M3-26) must keep the secret per thread and preparation, delete it on use and never put it in an event, state or log: a host-side obligation, tested in the adapter |
| A resume for another thread, principal, tenant or an unknown preparation | the thread must be the session of the verified scope (checked before any lookup), the coordinator finds a preparation only through scope, principal and session, and every miss is one identical `scope_violation` | none |
| An approval inferred from loose input (a "yes", a truthy value, extra members, a payload with a token) | only `{"approved": true}` with `status: resolved` approves; every other shape is `validation_failed`; a dismissed interrupt rejects | none |
| A client tool result taken for an approval | the confirmation call id has its own prefix (`alixai_confirm_`), not the generic `call_` of model clients; any other tool result is refused | none |
| Replay or double approval | the confirmation is single use (`replayed`), a retried run id returns the recorded run, the card is resolved as failed | none |
| A stale or expired preparation is confirmed | expired is `expired`, any other state `conflict`; the coordinator rechecks data versions at commit | none |
| An action that must not be confirmed by the AI path | step-up and level 3 become a call of `alixai_continue_in_app`; the coordinator answers `step_up_required` to a confirm | none |
| The card shows something other than what will happen | the arguments and the interrupt metadata are projected from the PreparedAction (host effects, core risk and expiry), not from model text | none |
| Client state, context, messages, tools or forwarded properties used as authority | data only: fenced, bounded, `<` neutralised, demoted, never in the turn input; tools are names only and cannot take a capability's or a reserved name | the engine has no seam for client context: it reaches a prompt only through the attachment seam, if the gateway opts in |
| The pending state leaks values | allow-list projection of names and origins; canaries tested | none |

Findings that changed the plan during the review:
- the pinned release's resume entry is `{interruptId, status, payload}` and `RUN_FINISHED` has a `cancelled` outcome, so the mapping
  uses those and `done(cancelled)` maps to the outcome;
- the first draft used `call_<id>` for the confirmation call; a client tool result with a model-style `call_` id would have been read
  as an approval attempt, so the prefix became `alixai_confirm_`;
- the thread id is bound to the session id of the token rather than looked up, so that no thread-to-session table exists to get
  wrong.

Voice: nothing changes. Compatibility is claimed only for what is tested, and full duplex is not claimed.

## Reversal plan
Remove the `v1_agui` area and the adapter package; nothing else depends on them.
