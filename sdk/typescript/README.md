# @alixai/sdk

The TypeScript SDK for the AlixAI public API (`/v1`). ESM, strict TypeScript, **zero runtime dependencies** (it uses `fetch` and a small
server-sent-events reader over `ReadableStream`), so it runs in Node 20 and later and in browsers. An in-process fake of the service for your own
tests ships as `@alixai/sdk/testing`.

**Status: 1.1.0.** The contracts this SDK speaks are frozen for v1 (`schemas/_frozen/v1`, checked by `tools/compat`).
`package.json` reads `1.1.0`. Compatibility and support rules are in
[compatibility](../../docs/policies/compatibility.md) and [migration and support](../../docs/policies/migration-and-support.md).
The version this SDK is tested against is the core in the same repository commit. See [CHANGELOG.md](CHANGELOG.md).

## What is in it, and what is not

| Module | State |
| --- | --- |
| Core: sessions, streamed turns with cursors, confirmations, actions, applications, capabilities, health | Complete. `createClient` |
| Messages with attachments, scoped files, conversations (stop, regenerate, edit-and-resubmit, switching), history, privacy and settings | Complete. TypeScript only: the Go and Python SDKs do not have these |
| RAG: sources, search, grounded answers with citations | Complete. `rag` namespace |
| Voice | Complete as a client of the voice WebSocket (M4-07). `voice` namespace. TypeScript only. **Full-duplex voice is not claimed**; this is push to talk plus an optional endpointer |
| Providers, modes, budgets (External provider mode) | Complete for the providers and OAuth operations of the API (X-14). `providers` namespace. **Opt-in and off by default**; a deployment without it answers `unavailable` |
| Per-principal LLM account (ADR 0041) | Complete (1.2.0): `client.me` (`catalog`, `llm.*`, `providers.*`). `me.providers.submitCredential` is **host-server only** — never call it from browser code |
| Long-term memory (ADR 0042) | Complete (1.2.0): `client.memory` (`list` / `create` / `patch` / `delete` / `confirm` / `reject`) |
| Decision status | Read-only (D-10). `decision` namespace. Advisory information, never an authorisation |
| AG-UI and CopilotKit | Not in this package, on purpose: this SDK does not depend on CopilotKit. The optional integration is `@alixai/copilotkit-adapter` (`sdk/copilotkit-adapter`); see [the CopilotKit guide](../../docs/integration/guides/copilotkit.md) |
| Operator-only decision configuration (`GET`/`PUT /v1/decision/config`) | Not in any SDK |
| Lite profile | Uncertified. Nothing in this SDK depends on it; figures elsewhere in the repository are labelled emulated |

The same feature-by-SDK table, for all three SDKs, is in [the integration guides](../../docs/integration/guides/README.md).

## Quick start

Every code sample in this README (except the commands under Development) is copied from a test that runs, and names it. The host below is the one the SDK's own tests use: a fake service and a client
whose token provider mints one fresh token per call. In production, drop `fetch` and `fake`, set `baseUrl` to your AlixAI and make `getContextToken` call
your backend.

From `test/helpers/host.ts` (`createHost`):

```ts
const fake = createFakeServer(options);
const client = createClient({
  baseUrl: fake.baseUrl,
  fetch: fake.fetch,
  getContextToken: (request) => {
    tokenRequests.push(request);
    return fake.issueToken({ session_id: "ses-1", ...claims });
  },
});
```

From `test/turn.test.ts` ("is an async iterable that narrows by event type"):

```ts
let text = "";
for await (const event of client.sendTurn("ses-1", { text: "narrow" })) {
  if (event.type === "token") text += event.text; // `text` exists only on a token event
}
```

## Context tokens

A context token is the application's proof of who the user is (ADR 0013). It is **single use**: every request, and every event-stream open, needs a fresh
one, and a second presentation is `replayed`.

- You supply `getContextToken(request)`. It is called **once per request**, after the arguments have been validated (a malformed argument never costs a
  token), and receives `{ operation, sessionId?, turnId?, preparationId?, applicationId?, fileId?, ... }` so you can mint the right one. Return the bare token.
- The SDK **never mints, decodes, caches or retries with** a token. It holds one in memory for the length of one call and sends it only as
  `Authorization: Bearer`; redirects are an error, cookies are never sent.
- `/healthz` and `/readyz` are unauthenticated: no token is requested for them.
- The service verifies `EdDSA` tokens (ADR 0013); signing is your backend's job, never the browser's.

From `test/client.test.ts` ("calls getContextToken once per request, with what the request concerns"):

```ts
await client.createSession();
await client.getSession("ses-1");
await client.getPendingState("ses-1");
await client.listCapabilities();
await client.getHealth();
await client.endSession("ses-1");
// tokenRequests is now, in order:
//   { operation: "createSession" }, { operation: "getSession", sessionId: "ses-1" }, { operation: "getPendingState", sessionId: "ses-1" },
//   { operation: "listCapabilities" }, { operation: "getHealth" }, { operation: "endSession", sessionId: "ses-1" }
```

## Turns

`sendTurn(sessionId, input)` sends the request immediately and returns a `TurnStream`: an async iterable of typed `TurnEvent`s (`token`, `directive`,
`clarification`, `prepared_action`, `receipt`, `citation`, `progress`, `error`, `done`, `block`). Turns are POST requests, so the stream is read with
`fetch`, not `EventSource`. `input` is exactly one of `{ text }`, `{ choice_id }`, `{ confirmation: { preparation_id, token } }` and
`{ reject: { preparation_id } }`; `text` can also carry `language` and `attachments` (file ids). A confirmation is a structured input from your UI: text can never
confirm anything. `sendTurnJson` returns the same as one JSON document, without streaming.

The stream is checked as it arrives and **fails closed**: one `turn_id`, `seq` from 0 without gaps, `done` last, each frame's `event`/`id` agreeing with the
event it carries. A stream that ends before `done` throws `uncertain_outcome` (what the turn did is unknown: read `getPendingState` before sending
anything again); a gap, a malformed event or an unknown event type throws `internal`. A turn that ended in a typed failure or a refusal is **delivered as
events** (an `error` event, then `done` with `finish_reason: "failed"`), not thrown. A turn that could not be run at all (no valid scope, a rate limit,
an ended session) throws before any event.

### Reconnecting: cursors, SSE and WebSocket

Every stream knows its `turnId` and its `cursor` (the `seq` of the last event it delivered). `streamTurnEvents(sessionId, turnId, { cursor })` reads the
turn again from there, with a fresh context token per connection. It only reads: the turn is never run again, and the confirmation secret is not part of
it. The gateway keeps a bounded replay buffer: a cursor behind it is `expired` (`details.reason: cursor_expired`), a turn that is not yours is the same
`scope_violation` as one that does not exist, and a cursor at the end of a finished turn is an empty stream.

From `test/resume.test.ts` ("continues after the cursor, each event exactly once and in order", "makes progress count"):

```ts
const turn = await host.client.sendTurnJson("ses-1", { text });
const stream = host.client.streamTurnEvents("ses-1", turn.turn_id, { cursor: 1 });
const events = await collect(stream);
// events[0].seq === 2: the stream continues after the cursor
```

and, to reconnect by itself:

```ts
const events = await collect(host.client.sendTurn("ses-1", { text: "x" }, { resume: { maxAttempts: 1, backoffMs: 0 } }));
```

Reconnecting is **off by default** (a lost stream is `uncertain_outcome`). Switch it on per call (`{ resume: { maxAttempts, backoffMs } }`) or with
`createClient({ resume })`. The same stream can arrive over a **WebSocket**. The gateway authenticates with the `Authorization` header and never accepts a
credential in a URL, and a browser WebSocket cannot set a header, so the SDK opens a socket only through a `webSocket` factory you supply (see `WebSocketFactory`
in `src/ws.ts`; one line on a runtime whose WebSocket takes headers: Node 22 and later, Deno, Bun). With a factory, `transport: "auto"` tries the WebSocket and
**falls back to SSE with a fresh token** when it cannot be opened. `stream.transport` says which one was used. Close codes: 1000 is the end, 1013 (slow
consumer) and a lost connection are resumed from the cursor, 1011 (turn abandoned) and 1008 (protocol) end the stream with a typed error. The test is
`test/resume.test.ts`, "the WebSocket transport", using `createHost({}, {}, { webSocket: true })`.

### Messages with attachments, files

A turn request carries only the ids of files already uploaded. `sendMessage` does the two steps in order: each attachment is uploaded as a scoped file of
the session, then the text turn is sent. From `test/files.test.ts` ("uploads each attachment first, then sends the text turn, and reports the references in order"):

```ts
const stream = host.client.sendMessage("ses-1", {
  text: "what is in these?",
  attachments: [png("one.png"), { data: text("ID3"), name: "voice.mp3", mimeType: "audio/mpeg" }, { data: text("....ftypmp42"), name: "clip.mp4", mimeType: "video/mp4" }],
});
const result = await stream.result();
const refs = await stream.attachments;
```

The whole message is checked before the first upload (no HTML, SVG, XML or script types, no empty file, text required). If an upload fails nothing is sent and the
files already stored are deleted. `alixai.files` is the scoped-file API: `upload`, `get`, `download`, `preview`, `delete`. A file is known only by its
`FileRef`, never a path or a URL; every use re-checks scope, owner, session, expiry and permission.

### Confirmations

When a turn prepares an action that needs the user's explicit confirmation, its `done` event says `awaiting_confirmation` and the **single-use confirmation
secret** is not in any event: it is the `X-AlixAI-Confirmation-Token` response header of the stream (or the `confirmation` member of the JSON turn). The SDK
reads it for you and pairs it with the preparation. From `test/client.test.ts` (`describe("actions")`):

```ts
host.fake.onTurn(() => prepareAction(BOOK));
const grant = (await host.client.sendTurn("ses-1", { text: "book" }).result()).confirmation!;

const outcome = await client.confirmAction(grant.preparation_id, grant.token);
// outcome.receipt.status === "committed"; outcome.preparation.state === "committed"
expect(await client.getActionReceipt(grant.preparation_id)).toEqual(outcome.receipt);
expect(await client.getPendingState("ses-1")).toBeNull();

const failure = await caught(client.confirmAction(grant.preparation_id, grant.token));
// { code: "replayed", status: 409, retryable: false }, and no receipt on the error
```

- Show the secret to the user's client only, present it once. It is never part of `getPendingState`.
- A commit the host refused (409) or whose outcome is partial or unknown (500) throws an `AlixAIError` that carries the `receipt` and `preparation`; a wrong
  secret is `permission_denied` and does not use the right one up; a level 3 action returns a `request_step_up` directive and cannot be confirmed through the API
  (`step_up_required`): the user continues in your application's own interface.
- In a browser on another origin, the service must expose the header (`Access-Control-Expose-Headers: X-AlixAI-Confirmation-Token, X-Request-ID`).

## Conversations

`alixai.conversation(sessionId)` is a small state holder over one session: `send`, `choose`, `stop`, `regenerate`, `editAndResubmit`, `resume`, with every turn
recorded in `turns` and `view(record)` for rendering. From `test/conversation.test.ts`:

```ts
const conversation = host.client.conversation("ses-1");
const stream = conversation.send("hello");
const iterator = stream[Symbol.asyncIterator]();
await iterator.next();
await iterator.next();
conversation.stop();
const rest = await collect(conversation.resume());
const record = conversation.turns[0]!;
```

The API has no route that rewinds or re-runs a turn, so a regenerate or an edit is **a new text turn** on an append-only session. The guards keep it from
doing anything twice: a turn that committed an action is never sent again; a turn whose prepared action still waits for the user is sent again only with
`discardPending: true`, which first **rejects** that preparation; a turn that was not read to its end is not sent again (resume it first); only the latest turn can be
edited. **Stop does not halt server work**: it releases the connection, and `resume()` reads the rest while the turn is in the replay buffer. Switching conversations is
opening another one (`alixai.conversation(id)`); find one with `alixai.history.search`.

## Rendering: blocks, citations, receipts

`viewTurn(events)` folds the events of a turn into one object: `text`, `blocks` (Markdown, code, table, chart, diagram, citation list, action card,
clarification, confirmation panel; `blocksOfType(view.blocks, "table")` narrows them), `clarification` with its choices, `preparedAction` and its
`confirmationPanel`, `receipt`, `directive`, `citations`, `errors` and `finishReason`. `citationLink(citation)` is the data to render a citation as a source link
(label, place in the source, available or stale, and an opaque `open` reference to resolve with `rag.resolveCitation`, never a URL). The SDK renders nothing.

## History, privacy and settings

`alixai.history`: `segments`, `search`, `export` (JSON lines; whole only if it ends with the trailer and its count, otherwise a retryable `export_incomplete`),
`clear` and `cleanup`. Each needs its capability in the context token (`history.read`, `history.search`, `history.export`, `history.clear`, `history.cleanup`).
`alixai.settings`: user, tenant and deployment levels, `getEffective`, `listProfiles`, `getCapabilities`, `getIntegrationAudit`, `getProviderHealth`. A write
replaces the whole document and names the version it read: a stale one is `conflict` with `details.current_version` (the SDK never retries a write). A setting can
only select among what the operator approved, switch something off or ask for more confirmation. `FAKE_HISTORY_SCOPES` and `FAKE_SETTINGS_SCOPES` list the
capabilities the fake needs in a token (`test/history-settings.test.ts`).

## RAG

`rag.createRagClient({ baseUrl, getContextToken })` is the RAG area: sources, indexing, search, and grounded answers. An answer is either grounded with
citations or `no_supported_evidence` (`rag.isAnswered`, `rag.isNoSupportedEvidence`); never an uncited answer. Tests: `test/rag.test.ts`.

## Voice

`voice.createVoiceSession({ baseUrl, sessionId, getContextToken, webSocket })` is the client of the voice WebSocket of a session (M4-07): push-to-talk capture,
interim and final transcripts, transcript correction, verified speech playback, barge-in, reconnect with a fresh token. **Voice never confirms anything**: the
session object has no confirm, reject or choose; a prepared action is confirmed on the host's screen through the main client, with the secret that
`turn.completed.confirmation` carries. **Full-duplex voice is not claimed**; a speech failure leaves text and application control working. From
`test/voice.test.ts`:

```ts
const { session, transport } = setup({ language: "it", speak: false });
const info = await session.connect();
```

```ts
await session.connect();
session.startCapture();
expect(session.pushAudio(pcm(1200))).toBe(true); // 2400 bytes
session.endCapture();
```

```ts
session.correctTranscript("book tuesday");
```

`voice.createFakeVoiceTransport` is the test double (`@alixai/sdk` exports it; the browser microphone and speaker are yours).

## Providers, modes, budgets (External provider mode)

External provider mode is **opt-in and off by default**; `local` is the effective mode everywhere until an operator changes it, and a deployment without the
feature answers every method `unavailable`. The client is operator-only: modes, egress policy and disclosure, budgets (integer micro-units), usage export,
the kill switch, credential submission and the browser OAuth flow. A provider credential is write-only and prints as a placeholder everywhere. From
`test/providers.test.ts` ("local is the default, the most restrictive level decides, the kill switch wins"):

```ts
const { client } = setup();
await client.getMode();
await client.setMode("deployment", "external");
const mode = await client.setMode("tenant", "hybrid");
expect(await client.setKillSwitch(true, "incident_42")).toMatchObject({ engaged: true, reason: "incident_42", changed_by: "principal-1" });
expect(await client.getMode()).toMatchObject({ effective: "local", configured: "hybrid", decided_by: "kill_switch", kill_switch: true });
```

An external model is an interchangeable inference backend only; it never executes a tool or owns permissions.

## Decision status

`decision.createDecisionClient({ baseUrl, getContextToken }).getStatus()` reads the status of the decision layer: provider, model, mode, fallback counters.
The decision layer is optional, bounded, non-generative and non-authoritative: it scores candidates that were already authorised and never authorises,
validates, prepares, confirms or commits anything, and with it off or failing the controller produces the same deterministic outcomes. The status is
advisory operational information (it needs the capability `decision.status.read`), and no member of it is permission to do anything. From
`test/decision.test.ts`:

```ts
const fake = decision.createFakeDecisionServer();
fake.setStatus(decision.decisionStatusFixture("advisory", { accepted: 7, fallbacks: { timeout: 1, low_confidence: 2 } }));
const status = await client.getStatus();
```

## Sessions, actions, applications, health

| Method | Operation (OpenAPI `operationId`) |
| --- | --- |
| `createSession()` | `createSession`: opens the session the token names; always 200 |
| `getSession(id)`, `getPendingState(id)`, `endSession(id)` | `getSession`, `getPendingState`, `endSession` |
| `sendTurn(id, input)`, `sendTurnJson(id, input)`, `streamTurnEvents(id, turnId)` | `sendTurn`, `streamTurnEvents` |
| `getAction(id)`, `confirmAction(id, token)`, `rejectAction(id)`, `getActionReceipt(id)` | the action routes |
| `getApplication(id)`, `listCapabilities()`, `registerApplication(archive)` | the application routes (a registered package lands in quarantine) |
| `getHealth()`, `getReadiness()`, `getLiveness()` | `/v1/health`, `/readyz`, `/healthz` |

`getHealth` and `getReadiness` return the report for 200 **and** 503. Every method takes `{ signal, timeoutMs }`. `OPERATIONS` (plus `rag.RAG_OPERATIONS`,
`providers.PROVIDERS_OPERATIONS`, `decision.DECISION_OPERATIONS`) is the table of methods and paths the SDK speaks, checked against `schemas/api/openapi.yaml` by tests.

From `test/client.test.ts` ("inspects the caller's application", "a quarantined application serves no capabilities"):

```ts
expect(await client.getApplication("fake-app")).toMatchObject({ application_id: "fake-app", status: "active", health: { status: "ok" } });
expect(await caught(client.listCapabilities())).toMatchObject({ code: "quarantined", status: 403 });
```

An application is served only while its package is active, and a package becomes active only through a clean integration audit run by the operator
([getting started](../../docs/integration/guides/getting-started.md)).

## Errors

Every failure is an `AlixAIError` carrying one of the `ErrCode` strings (`ERROR_CODES`) in `code`, plus `retryable`, `status`, `details`, `requestId` and
`retryAfterMs`. Failures that never reached the server use the same strings: a bad argument is `validation_failed`, a token provider that threw is
`unauthenticated`, an unreadable response is `internal`. A request that may have been processed and whose outcome is unknown (a network error or timeout on a
POST or DELETE) is `uncertain_outcome`, never retryable. **The SDK does not retry anything**: tokens are single use and an uncertain outcome must not be re-sent
blindly; use `retryable` and `retryAfterMs` to decide. `baseUrl` must be https (plain http is accepted for loopback only; `allowInsecureHttp` overrides).

## Testing your application: `@alixai/sdk/testing`

An in-process fake of the API: a `fetch` handler, no port, no network. It enforces the protocol, so a host that gets it wrong fails the way it would against the
real service: single-use tokens (`replayed`), capability scopes as exact ids with default deny, the session in the path must be the token's (`scope_violation`),
turns as exact SSE or JSON, the secret in the header only, a secret that works once, level 3 `step_up_required`, a receipt that is `conflict` until there is an
outcome, and a package that is not active serves nothing (`quarantined`). It keeps a replay buffer of every turn, implements files, history and settings, and
`failNext` injects faults. Script a turn with `onTurn` and the plans `reply`, `askChoice`, `prepareAction`, `failTurn`, `rejectTurn`. What it does not do: verify
signatures (its tokens are unsigned and labelled `alixai-fake+jwt`), run a model, or reach an application. A real model is not needed to test a host; none of
the controller models is activatable yet, so a live conversation needs a model the operator has qualified.

## Types

The wire types generated from `schemas/` (do not edit `src/gen`; `go run ./tools/schemagen` regenerates them) are available under the `types` namespace, with
the common ones also exported directly (`TurnEvent`, `PreparedAction`, `ActionReceipt`, `ErrorCode`, ...). The `/v1` types that only the OpenAPI document defines
(`Session`, `PendingState`, `Health`, `Capability`, ...) are hand-written in `src/api-types.ts`.

## Development

```bash
pnpm install
pnpm --filter @alixai/sdk run typecheck
pnpm --filter @alixai/sdk test
pnpm --filter @alixai/sdk run build      # tsc: dist/ with .d.ts and source maps
```

Arrow functions only (a test enforces it, and forbids runtime imports and Node-only globals outside `src/testing`).
