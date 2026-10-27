# AG-UI and CopilotKit compatibility

Source of truth: [ADR 0035](../decisions/0035-copilotkit-agui-compatibility.md) and the project rule that the SDKs and the
streaming API stay compatible with the CopilotKit UI API and the AG-UI protocol. Where this page and the ADR disagree, the
ADR wins and this page is the bug.

AG-UI (Agent-User Interaction Protocol) is the open event protocol CopilotKit speaks to agents. AlixAI implements it as a
second face of the streaming API: the same turn, the same authority rules, expressed as AG-UI events. Nothing about the
TurnEvent API changes, and nothing here needs CopilotKit Cloud or sends telemetry.

| Part | Where | Task |
| --- | --- | --- |
| Pinned schemas of the AG-UI release and the generator | `schemas/agui/`, `tools/aguischema/` | M3-24 |
| The mapping (pure Go, no HTTP): events, intake, interrupts, state, suggestions, capabilities | `internal/agui/` | M3-24 |
| `POST /v1/agui` over SSE and WebSocket, resume cursor, cancel, capabilities route | `internal/gateway/routes_v1_agui.go` | M3-25 |
| The engine seams for readable context and frontend actions (client block of the proposal prompt, `ui_action` of the proposal, UI tool allow-list) | `internal/dialogue/`, `internal/dialogue/context/`, `schemas/conversation/task-proposal.schema.json` | M3-32 |
| Client context and UI tools end to end: the route passes both to the engine, the host's UI tool allow-list (`agui.ui_tools`), the UI action in the run, capabilities that follow the wiring, golden streams | `internal/gateway/routes_v1_agui.go`, `internal/cli/cmd_agui.go`, `internal/agui/`, `tests/agui/` | M3-34 |
| `@alixai/copilotkit-adapter` (AlixAIAgent for CopilotRuntime) | `sdk/copilotkit-adapter/` | M3-26 |
| Go and Python proxy helpers | `pkg/sdk-go`, `sdk/python` | M3-27 |
| Version matrix against real `@ag-ui/client` and `@copilotkit/runtime` | `compat/matrix.json` | M3-28 |

This page describes what `internal/agui` does today. What the gateway and the adapter add on top is listed as notes for
those tasks (section 13); until they land, a row of the CopilotKit matrix (section 12) that depends on them says so.

## 1. The pinned release and the vendored schemas

The pinned release is `@ag-ui/core` **1.0.1**, protocol version **1.0** (`PROTOCOL_VERSION` of the package).

`tools/aguischema/generate.mjs` loads the installed package, registers every exported zod schema under its export name,
converts the registry with zod's own JSON Schema converter (draft 2020-12, input side) and writes:

- `schemas/agui/ag-ui-core.json`: one bundle, every AG-UI schema a member of `$defs` (`Event`, `RunAgentInput`, `Message`,
  `Tool`, `Context`, `JsonPatchOperation`, `ResumeEntry`, `Interrupt`, `AgentCapabilities`, one per event type, ...). 99
  definitions. The file does not end in `.schema.json` on purpose: `tools/schemagen` and `tools/schemacheck` own that
  suffix and the `urn:alixai` id convention, and these are another party's schemas, not AlixAI contracts.
- `schemas/agui/VERSION`: the package and version, the protocol version, the npm `integrity` hash of the tarball, the zod
  version used for the conversion, the number of refinements the converter cannot carry, and the sha256 of the bundle.

```bash
cd tools/aguischema && npm ci && node generate.mjs     # regenerate (writes schemas/agui)
node tools/aguischema/generate.mjs --check             # exit 1 when schemas/agui differs from a fresh run
node tools/aguischema/generate.mjs --from <dir>        # <dir> holds node_modules and package-lock.json of the pinned packages
```

`tools/aguischema` is not part of the pnpm workspace: it is a dev tool with its own `package.json` and `package-lock.json`,
run with npm. To move to a newer release, change the two pins in `package.json`, regenerate, and let the tests tell you what
changed: `TestVendoredSchemasCoverTheWholeProtocol` fails until every new event type is classified as emitted or not.

What validates against them (all in `internal/agui`, hermetic):

- `TestEveryEmittedEventValidatesAgainstTheVendoredAGUISchema`: every event of every scenario (plain answer, clarification,
  citations, prepare and interrupt, resume approve and reject and failure, refusal, progress, every block type, step-up,
  cancel, attachments, state in both modes, connect, abort) is validated against `Event` and its own definition. The
  release's schemas are open (`additionalProperties: {}`), so a misspelled member would pass; the validator therefore also
  checks that every member is declared. The test fails when an emittable event type is never produced by a scenario.
- `TestVendoredSchemasAreThePinnedRelease`, `TestEditedBundleIsRefused`: the bundle must match the sha256 in `VERSION`, and
  `VERSION` must name the release `tools/aguischema/package.json` pins.
- 49 zod refinements are not expressible in JSON Schema. They are `value !== null` / `value !== undefined` guards on members
  typed `any` (`rawEvent`, `metadata`, `payload`, `snapshot`, patch `value`...). The mapping never emits `rawEvent` or
  `metadata`, and a patch always carries its value.

`agui.LoadSchemas(dir)` and `FindSchemaDir()` are exported, with `Schemas.Validate`, `ValidateEvent` and `ValidateInput`, so the golden-stream tests of M3-25 and the SDK
tests validate with the same code.

## 2. TurnEvents to AG-UI events

The input of the mapping is the stream hub's account of the turn (`internal/stream`): events the block validator accepted
and whose Markdown was sanitised. The mapping neither validates nor sanitises again. `agui.Mapper` is a state machine over
one run; it is not safe for concurrent use.

```go
m, _ := agui.NewMapper(runContext)
for _, turnEvent := range hubEvents {        // in seq order, once each
    emitted, err := m.Map(turnEvent)         // []Emitted{Seq, Event, Data}
}
```

Every AG-UI event leaves through `agui.Encode`, which refuses a type that is not emittable, a credential-shaped member name
at any depth, a withheld value (the confirmation secret of the run) and an event over 256 KiB.

| TurnEvent | AG-UI events |
| --- | --- |
| first event of the run | `RUN_STARTED(threadId, runId)`, then `ACTIVITY_SNAPSHOT` `alixai.attachments` if the message had files, then the initial `STATE_SNAPSHOT` (or `STATE_DELTA` in patch mode) |
| `progress` | `STEP_STARTED(stage)` and `STEP_FINISHED(stage)`; stages that have a percentage, a job or a capability, or report `progress`, also an `ACTIVITY_SNAPSHOT` `alixai.progress` updated by `ACTIVITY_DELTA` (`replace /state`, `add /percent`) |
| `directive` | `CUSTOM` `alixai.directive` (the directive as the TurnEvent API sends it) |
| `memory_proposal` | `CUSTOM` `alixai.memory_proposal` (ADR 0042; confirm/reject via `/v1/memory/{id}/confirm` or `/reject`, never typed text) |
| `text_replaced` | `TEXT_MESSAGE_END` if a message is open, then `CUSTOM` `alixai.text_replaced` (`reason`, optional `messageId`) — verified streamed text is superseded (ADR 0046); the deterministic template follows as new `token` / `TEXT_MESSAGE_*` events. Kits discard the superseded assistant message so nothing unverified or half-replaced stays on screen. |
| `token` | `TEXT_MESSAGE_START` (role assistant) once, `TEXT_MESSAGE_CONTENT` per fragment, `TEXT_MESSAGE_END` when anything else arrives or the turn ends |
| `block` markdown, code | a complete assistant text message (code as a fenced block whose fence is longer than any backtick run in it) |
| `block` table, chart, diagram, citation_list, action_card, clarification_choices, confirmation_panel; `clarification` | `ACTIVITY_SNAPSHOT` of type `alixai.block.<type>`, content the validated block as it is |
| `citation` | one `ACTIVITY_SNAPSHOT` `alixai.citations`, then `ACTIVITY_DELTA` `add /citations/-` per further citation |
| `prepared_action`, explicit confirmation | `TOOL_CALL_START(alixai_confirm_action)`, `TOOL_CALL_ARGS` (the exact effects), `TOOL_CALL_END`; the call id is `alixai_confirm_<preparation id>` |
| `prepared_action`, step-up | the same three events for `alixai_continue_in_app`: the AI path cannot complete it, the user continues in the application |
| `receipt` | in a resume run: `TOOL_CALL_RESULT` for the confirmation call, content the receipt (the application's deterministic facts); otherwise an `alixai.receipt` activity |
| `error` | held; the run ends with `RUN_ERROR(message, code)` at the `done` event |
| `done` completed, awaiting_input | open steps closed, state delta, suggestions (`CUSTOM alixai.suggestions`), `RUN_FINISHED` with `outcome: {type: success}` |
| `done` awaiting_confirmation | `RUN_FINISHED` with `outcome: {type: interrupt, interrupts: [...]}` (section 6) |
| `done` cancelled | `RUN_FINISHED` with `outcome: {type: cancelled}` |
| `done` failed, or any run that carried an `error` | `RUN_ERROR`, never a success-looking finish |

`RUN_FINISHED.result` is `{"turnId": ..., "finishReason": ...}`. No event carries `timestamp`, `rawEvent`, `metadata` or the
run's input (`RUN_STARTED.input` is never set: it would reflect the client's state and forwarded properties).

Why an error is held: the engine publishes the `error` event before the response text that explains it, then `done`. AG-UI
allows nothing after `RUN_ERROR` and wants open messages closed, so emitting it on arrival would break the protocol. The
text is shown, then the run ends with the error.

The lifecycle rules of the pinned `@ag-ui/client` (`verifyEvents`: `RUN_STARTED` first, nothing after `RUN_FINISHED` or
`RUN_ERROR`, steps and messages and calls opened before use and closed before the run finishes, never opened twice, a step
finished by name) are encoded as a state machine in the tests (`verifyLifecycle`) and applied to every scenario
(`TestTurnEventsMapToTheAGUILifecycle`). Because that is a copy of the rules, every scenario stream was also replayed once, in a
scratch directory, through the real `@ag-ui/client` 1.0.1: the zod schemas of `@ag-ui/core` parse every event, `verifyEvents`
accepts the order (including a resume run that answers a tool call opened in the previous run), and `defaultApplyEvents` applies
the state snapshots and deltas, the activity deltas and the tool results without error. M3-28 turns that into a recorded,
repeatable gate on the golden streams of M3-25.

### Ids, determinism and the cursor

Every id is derived, never random: `msg_<turn id>_<n>` for assistant text, `msg_<turn id>_r<n>` for a tool result,
`act_<turn id>_<kind>` for activities, `alixai_confirm_<preparation id>` for the confirmation call, `alixai_ui_<turn id>` for the UI tool call of a turn. There is no wall-clock
timestamp. The mapping is a pure function of the `RunContext` and the TurnEvents, so mapping the buffered TurnEvents again
gives the same bytes (`TestMappingIsDeterministicAndResumable`).

Each `Emitted` carries `Seq`, the seq of the TurnEvent it was produced for. That is the SSE `id:` of the event and the
resume cursor: a client that has everything up to cursor `c` is sent the events with `Seq > c`. A cursor is therefore always
a TurnEvent boundary, and a resume re-maps from the first TurnEvent and skips what the client has.

`RunContext` is plain JSON-serialisable data (thread, run, kind, language, state before and after, resolved attachments,
resume reference, the UI action of the turn). The gateway keeps it with the turn's stream. It holds no secret (`Withhold` is `json:"-"`).

## 3. RunAgentInput to a turn request

`agui.ParseRunAgentInput` checks the shape and bounds (1 MiB body, 200 messages, 32 tools, 32 context items, one resume
entry); `agui.Intake(ctx, input, confirmation, env)` applies the rules with the verified scope (`identity.RequireScope`).

| Input | What happens to it |
| --- | --- |
| `threadId` | must equal the session id of the verified scope. The host that mints the context token sets the session to the thread. Anything else is the one `scope_violation` of section 6 |
| `runId` | echoed in `RUN_STARTED` and `RUN_FINISHED`; the gateway indexes it for idempotent retry (section 9) |
| last message, role user | the utterance, as text; with `file` content parts, the attachment file ids. The engine's input is built from this and nothing else |
| earlier messages | hints (section 4); the session store is the record of the conversation |
| system and developer messages | demoted to labelled untrusted lines in the client context; never a system message |
| reasoning and activity messages (earlier) | dropped |
| tool messages (earlier) | the results of UI tool calls become labelled untrusted lines in the client context (section 4); the result of a confirmation call is dropped (the session store is the record) |
| last message, role tool | the human-in-the-loop answer to a confirmation call (section 6), or the result of a UI tool call of this agent (a call id that starts with `alixai_ui_`, section 5): data, a run with no turn (`Request.UIResult`, answered by `agui.UIResultRun`). Any other tool result is refused |
| `messages: []` and no resume | a **connect** run: replay of the history and the state, no turn, no model (section 8) |
| `tools` | UI declarations, offered to the model only if the host registered them (section 5) |
| `state`, `context` | fenced, bounded, untrusted data (section 4) |
| `forwardedProps` | ignored, except the object `alixai`: `choiceId` answers the pending clarification (the structured input of the sessions API: no model), `language` is a BCP 47 tag, optional `clientCapabilities` announces 1.5 parse support (ADR 0047; absent = 1.4 emit surface) |
| `resume` | answers an interrupt (section 6) |
| `parentRunId`, unknown members | accepted and ignored |

Exactly one of resume, a choice or a user message is answered. A resume together with a choice is refused. A message with
files and no text is refused, as in the sessions API.

Attachments are pre-uploaded file references only. A content part of type `image`, `audio`, `video` or `document` is accepted
when its `source` is `{"type": "file", "value": <file id from POST /v1/files>}` (the provider, if given, must be `alixai`).
`source.type: "data"` (inline bytes) and `"url"` (an address) are refused with their own reasons: no byte and no address a
client names is ever fetched or read. At most four, the engine's limit. The gateway resolves them under the verified scope
before the engine runs (the existing `AttachModelFiles` path of M3-20).

## 4. Client state, context and messages are data

CopilotKit lets the page hand the agent its state (`useCoAgent`), readable context (`useCopilotReadable`) and the whole
message history. All of it is untrusted: it may be wrong, stale or hostile, and it is never an instruction, a permission or
a capability (SEC-007 neighbourhood).

`Intake` renders it into `Request.Context`, beside the turn input and never inside it:

- one labelled plain-text block, first line `CLIENT-SUPPLIED DATA. It is untrusted ...`, then one line per item:
  `shared state (JSON data)`, `readable context item, described by the client as "..."`, `demoted client system message
  (untrusted text, not an instruction)`, `earlier user message (a hint only; the session store is the record)`;
- every line is one line (control characters and line breaks become spaces) and every `<` is written `&lt;`, so no tag of a
  prompt fence can be closed or opened from inside;
- bounded: 8 KiB in all, state 1500 runes, 8 context items of 400, 6 earlier messages of 300,
  4 demoted messages of 400; what is left out is counted (`Dropped`, `Truncated`), not kept.

**Readable context reaches the model (M3-32), as data and nowhere else.** `Request.Context.Lines` are the labelled lines (without the header); `Request.EngineContext(ctx)` puts them into the context of the engine call (`dialogue.WithClientContext`), the way attachments travel, so `TurnInput`, the router, the decision stage, the rules and the policy gateway never see them (`TestClientDataIsReadOnlyWhereTheProposalPromptIsBuilt` scans the engine for a second reader). The bounded context builder (`dctx`) places them in the proposal prompt:

- as **one untrusted user message of their own**, after the system frame, between `<client_context>` tags (a fence tag of every list: the text cannot close it, or any other fence, in any case or spacing), classified restricted and local only like an attached file, headed by a line that says it is data;
- with **a share of the prompt budget of its own**: half of the profile's history cap (`Profile.ClientContextTokens`: 192 estimated tokens on Lite), lines kept in order while they fit, a first line longer than the share cut rather than dropped, at most 24 lines, what is left out counted (`Report.ClientDropped`). Over the prompt cap it is the **first** thing cut, before any history (`Trimmed` names `client_context`);
- **not through the four attachment slots**: `ClientContext.Attachment()` is gone, a turn with context still takes four files;
- not in the user's message, the transcript, the session state, the audit event, the directive or the response prompt (which is built from the directive alone); a reply that states something only the client's text holds fails the response verifier like any unsupported fact and the template is sent;
- with the trusted frame told, only when the block is present, what the tag is. With no client data the prompt, the constraint and the trace are byte for byte what they were (`TestClientDataAbsentLeavesThePromptUnchanged`), so the controller prompt, the grammar and the bench corpora of M0-17 and M3-16 are untouched.

An empty state (`{}`, `[]` or null) is no client data and gets no line: CopilotKit sends `state: {}` with every run that shares none, and a line for it would put a client block into the prompt of every run (`TestAnEmptyStateIsNotClientData`).

A value shaped like a confirmation secret (`cfm_` and 20 or more base64url characters) in any client text becomes `[redacted]`: the browser never holds one, so one there is a leak or a forgery, and it does not go on into a prompt. Format characters (zero width, bidirectional overrides) are removed like control characters.

The mutation-style tests (`TestClientStateContextAndMessagesAreFencedUntrustedData`, `TestClientContextReachesTheProposalPromptAsFencedData`) run a table of hostile payloads (tag closers in several spellings, role confusion, line injection) and assert that the utterance is exactly the last user message, that no `<` survives, and that nothing of the data is anywhere in the request or the prompt outside the context.

## 5. Client tools are UI declarations, never capabilities; frontend actions

A tool in `RunAgentInput.tools` is a declaration of something the client renders or executes itself (CopilotKit `useCopilotAction`, generative UI through tool calls). `Intake` validates the name (letters, digits, `_ . : -`, at most 64), refuses a name that starts with `alixai` (the reserved namespace) or equals a capability id of the application (case-insensitively; the refusal is the same message either way, so it does not reveal which capabilities exist), refuses duplicates and oversize declarations, and keeps the **names** in `Request.ClientTools` and the bounded declarations (name, description, parameter schema) in `Request.UITools`. **A UI action is an instruction to the CLIENT's screen, never to the application**:

- the host registers which UI tools exist per application (`dialogue.Deps.UITools`, an allow-list in the composition root's configuration; nil registers none). `alixai serve` reads it from the `agui` section of the configuration, and with no section no tool of any application is ever offered (none is the default):

  ```yaml
  agui:
    ui_tools:
      <application id>: [highlightRow, openPanel]
  ```

  The names are exact and must be well-formed and outside the reserved namespace (`agui.UIToolNameAllowed`); a name that could never be a UI tool, or a duplicate, stops the start. `internal/cli/cmd_agui.go` wraps the builder `wireV1` calls so that the engine is built with the allow-list (unless the composition gave it one of its own) and provides the same value to the gateway as `dialogue.UIToolRegistry`, so the engine and the capabilities document cannot disagree. The list is the operator's decision: the application package has no member for it, and a package is the application's claim. The section is read from `config.Default`; a registry built with `registerServeSections` (tests, tools) does not know it. Nothing is read from the environment: the member is a map; the model is offered a tool only if the client declared it for this run **and** the host registered it, it is a well-formed name that is not reserved, not a capability's and does not name a `prepare` or a `commit`, and its declared parameters are a bounded object schema. At most four tools are offered, and only as many as half of the client block's share holds; if offering them would cost the turn a capability of the shortlist, the turn goes on without them (`not_offered`);
- the model sees only the **bare names** in the trusted frame and the output constraint. The client's description (one line, clipped) and a summary of the argument names and types go inside the fenced client block, with the readable context: client words never reach the system message;
- the proposal schema has an optional **`ui_action`** `{name, arguments}` (`schemas/conversation/task-proposal.schema.json`, authority `model`). It is checked after decoding and before anything else of the proposal is used: the name must be declared, registered and offered; a name that is reserved or a capability's is refused whatever else is true of it; the arguments must be one JSON object of at most 4096 bytes and 8 levels, valid against the JSON Schema the client declared (compiled with a loader that loads nothing: a `$ref` to a file or an address does not compile). **Any failure rejects the whole proposal** with `validation_failed` and a `report_failure` directive: nothing is prepared, stored or executed, and `Trace.UIAction` says why (`refused:undeclared`, `unregistered`, `capability_name`, `reserved_name`, `not_offered`, `invalid_arguments`);
- a valid action rides on `dialogue.Result.UIAction`, only on a turn that asks, answers or lists (`ask_missing`, `ask_invalid`, `disambiguate`, `present_result`, `offer_help`); never on a confirmation, a step-up, a refusal, a failure or a cancellation (`Trace.UIAction` is `dropped`). The engine persists nothing of it;
- the caller puts it into `RunContext.UIAction`, and the mapping emits `TOOL_CALL_START` (the tool's name, id `alixai_ui_<turn id>`), `TOOL_CALL_ARGS` (the encoded arguments) and `TOOL_CALL_END` after the reply text and before `RUN_FINISHED`, only when the run finishes as a success. It re-checks the shape (a name that is not a tool name or is reserved, arguments that are not one bounded object fail the run with `RUN_ERROR`; a value in `Withhold` stops it too). One call per turn, no parallel calls;
- the client runs it and sends the result as a tool message with that call id. That is a run with no turn (`Kind` connect, `Request.UIResult` true). The gateway answers it with `agui.UIResultRun`: `RUN_STARTED`, the state, `RUN_FINISHED` with a success outcome. It sends no `MESSAGES_SNAPSHOT` (the client already holds the tool call and its result, and a snapshot of the stored history, user and assistant text only, would replace them) and so needs no history service and no `history.read`; it calls no model and looks nothing up. The result is data, never read as an approval (the id prefix is not `alixai_confirm_`, `Preparations` is never asked, a payload `{"approved": true}` is text) and the client resends it with the thread, so it reaches the next turn as one more line of client context (section 4). A tool result for any other call id is refused as before.

Limits, stated: a UI action rides on a turn that reaches the controller, that is, one where the router found a capability to consider. A request that matches no capability goes to the knowledge path or is refused, as every unmatched request is, and costs no model call: a host that wants such requests to drive its screen gives them a read capability. The engine does not write a follow-up to a UI result. Whether a small local model uses `ui_action` well is not measured here (no live runtime); the constraint offers it only when tools are offered, so with none declared nothing changes.

## 6. Interrupts: human approval

```text
run 1  user: "book Dr. Rossi at 10:30"
       ... TOOL_CALL alixai_confirm_action (args: the exact effects, risk, expiry) ...
       RUN_FINISHED outcome interrupt, interrupts: [{
         id: <preparation id>, reason: "confirmation_required", message: <summary>,
         toolCallId: "alixai_confirm_<preparation id>", expiresAt: <preparation expiry>,
         responseSchema: {approved: boolean},
         metadata: {preparationId, capabilityId, summary, effects, riskLevel, expiresAt, confirmation} }]
       response header X-AlixAI-Confirmation-Token: <secret>        (to the SDK agent only)

run 2  RunAgentInput.resume = [{interruptId: <preparation id>, status: "resolved", payload: {"approved": true}}]
       request header X-AlixAI-Confirmation-Token: <secret>
       ... receipt -> TOOL_CALL_RESULT for the confirmation call, text, RUN_FINISHED success
```

The mapping of the answer (`TestInterruptResumeMapsToConfirmAndReject`):

| Answer | Engine input |
| --- | --- |
| `status: resolved`, payload exactly `{"approved": true}`, with the secret | `Confirm{preparation id, token}` |
| `status: resolved`, payload exactly `{"approved": false}` | `Reject` |
| `status: cancelled` (the interrupt was dismissed) | `Reject` |
| anything else: no payload, `null`, `"yes"`, `true`, `{"approved": "true"}`, extra members, a payload carrying a token, an unknown status | `validation_failed`; **a confirmation is never inferred** |
| approval without the secret | `confirmation_required`; the engine is not run |
| a tool message whose `toolCallId` is `alixai_confirm_<id>` and whose content is the same `{"approved": bool}` | the same as a resume (the form of clients that answer a tool call). A client's own tool results (`call_...`) are never read as approvals: the prefix is not `call_` on purpose |

Against the pinned release the brief's wording differs in three ways, and the release wins: a resume entry is
`{interruptId, status: resolved | cancelled, payload}` (not `{interruptId, response}`); `RUN_FINISHED` has a third outcome,
`cancelled`, which `done(cancelled)` maps to; an interrupt has `toolCallId`, `expiresAt`, `responseSchema` and `metadata`,
used as above.

### The confirmation secret

The secret is the single-use proof that a confirmation was issued for one preparation to one principal in one session
against these data versions (`internal/actions`). It is never part of an AG-UI event, by construction and by checks:

1. it is not a TurnEvent member, and the mapping reads TurnEvents only (and the closed TurnEvent schema has no place for it);
2. `Intake` takes it as a separate argument, the value of the `X-AlixAI-Confirmation-Token` request header. It is never read
   from the body, and a payload that carries one is refused;
3. the gateway returns it in the same-named response header of the run that prepared the action; the SDK agent in the host
   backend (M3-26) keeps it per thread and preparation, attaches it to the resume and deletes it on use. The browser never
   holds it;
4. `Encode` refuses any event with a credential-shaped member name and any event containing a value of `RunContext.Withhold`,
   where the caller puts the secret of the run: a producer that put it into a summary, a fact or a message by mistake has the
   run ended with `RUN_ERROR`, not the secret sent (`TestConfirmationSecretNeverAppearsInAnEvent` runs the same poison with
   and without the guard);
5. `actions.ConfirmationToken` marshals and prints as `[redacted]`, so a `Request` that is logged or encoded does not carry it.

### A resume for another thread, another principal or an unknown preparation

All of these are the identical `scope_violation` (same code, same message, same JSON, no echo of the request):

- the run's `threadId` is not the session of the token (checked first: the action service is not asked at all);
- the coordinator answers `scope_violation` or `permission_denied` (another tenant, another principal, another session, or no
  such preparation);
- the coordinator answers a preparation with another id than the one asked for.

The coordinator itself finds a preparation only through the verified scope and only for the principal and session that made
it. A preparation that is no longer awaiting a decision is not confirmed: `expired` or `conflict`. Re-declining one that is
already declined is idempotent. `TestResumeForAnotherThreadIsTheIdenticalScopeViolation` compares the marshalled errors of
nine different cases byte for byte.

Level 3 and step-up actions cannot be confirmed here at all: the prepared action becomes a call of
`alixai_continue_in_app` and the run finishes as a success; a confirm that reaches the coordinator for one gets
`step_up_required`.

## 7. What the confirmation card shows

The tool call arguments and the interrupt `metadata` are projected from the PreparedAction: preparation id, capability id,
summary, the exact effects (host data), risk level, expiry and the confirmation requirement. They do not contain the
idempotency key or the data versions. The text the model writes around it is verified by the core as ever; the effects are the
host's, not the model's.

## 8. State, history and snapshots

The shared state of a thread is `{"alixai": ClientState}`:

```json
{"alixai": {"pending": {"kind": "confirmation", "capabilityId": "appointments.book", "phase": "prepared",
  "collectedFields": [{"name": "doctor", "origin": "user_turn"}], "missingFields": [],
  "preparationId": "prep_...", "expiresAt": "2026-10-09T08:10:05Z"}}}
```

`ProjectState` builds it field by field from the session state: names and origins of collected fields, the pending question
with its choices (id and label), the preparation id and expiry. Slot values, entity ids, results, capsules, other goals, the
roles and capability scopes of the token and `allowed_next` are not copied and cannot be added by accident
(`TestSEC_ClientStateIsAnAllowListProjection` puts canaries in all of them and searches the snapshot and the delta).

Two modes, chosen by the shape of the client's own state:

- the client sent no state, or not an object: `STATE_SNAPSHOT {"alixai": ...}` first;
- the client sent an object: `STATE_DELTA` `add /alixai`, so the members the client keeps for itself are never replaced.

At the end of a run `STATE_DELTA` carries the minimal patch from the state at the start to the state at the end (objects
member by member, arrays and scalars replaced whole), and only paths under `/alixai`. A test applies snapshot plus delta and
compares with the final state.

A connect run (`messages: []`, no resume) emits `RUN_STARTED`, `MESSAGES_SNAPSHOT` (the thread's history, user and assistant
text only, ids derived from turn ids, newest messages kept to 192 KiB), the state, `RUN_FINISHED`. No turn runs.

CopilotKit's page joins a thread with `POST /agent/:id/connect` on the runtime. With `@alixai/copilotkit-adapter` that route is answered by `AlixAIRunner`, which
calls this connect run through `AlixAIAgent` (the host's token callback is asked with `operation: "agui.connect"` for the thread and the caller's forwarded headers),
so a thread that does not resolve for the caller is refused by the host or by the gateway's scope check and nothing of it is replayed; CopilotRuntime's own in-memory
replay is not used (`docs/integration/copilotkit-compatibility.md`). The page applies the snapshot and the state as it does any other run's; the state it holds under
`alixai` is rendered by the adapter's state renderer (`useAlixAIPendingState`, `AlixAIPendingState`, or `showState` for the chat).

## 9. Cancel, retry and reconnect

These are the semantics the gateway (M3-25) implements; the mapping provides the determinism they rest on.

- **Reconnect.** The cursor is the `Seq` of the last event received, sent as `Last-Event-ID` (SSE) or `cursor` (WebSocket) like
  the TurnEvent streams. The server re-maps the buffered TurnEvents and skips events with `Seq <= cursor`. A cursor behind the
  retained window is `410`, one ahead of the stream `400`, a turn that is not visible in the caller's scope `403` (unknown,
  foreign and expired are indistinguishable): the hub's rules. A reconnect never runs the turn again.
- **Retry.** A run is identified by `(thread, runId)`. The same `runId` on the same thread returns the recorded run (the
  buffered stream), never a second turn. A new `runId` is a new turn. A retried resume reaches the coordinator only once:
  the confirmation is single use: a second approval under a NEW `runId` is answered `409 conflict` before any streaming starts
  (no `RUN_ERROR`), the same `runId` replays the recorded run and never resends the secret, and while that run is still running
  it is `409`. A missing secret on an approval is `428 confirmation_required`. A reject resume ends with the `cancelled`
  outcome. Nothing is committed twice. (Behaviour of the M3-25 gateway.)
- **Cancel.** The engine runs a turn to completion before its events exist, so a cancel before completion aborts the request
  context and no event is produced. After completion cancel means the client stops reading: a receipt that exists stands, and
  a prepared action left pending can be declined (`resume` with `cancelled`). `done(cancelled)` ends the run with the
  `cancelled` outcome. A confirmation already given is not undone: cancel leaves nothing committed unless the user had already
  confirmed.
- **Abort.** A producer that is abandoned (the hub's idle timeout) ends the stream without a `done`; `Mapper.Abort` closes what
  is open and sends `RUN_ERROR(aborted)`.

## 10. Suggestions

`agui.Suggest(directive, language, catalog, max)`: computed, never generated; no model, no network, no randomness. It offers
only what the directive already allows: the host's own `allowed_suggestions` first, then each capability named by an
`invoke_capability` entry of `permitted_next_actions` (the core built the list from the host's data intersected with policy,
scope and the goal's state), in the directive's order, de-duplicated, at most 4 (6 at most). Display text comes from the
capability catalog the caller passes (title and examples of the application package, which the schema marks `test_only`):
the example in the turn's language, else the first example, else the title. An example is only ever shown; picking one sends
it as an ordinary message, through the same pipeline as anything typed, and the example never routes, ranks or authorises
anything. A capability the directive does not name is never suggested, so a suggestion cannot reveal a capability the caller
may not use. A confirmation, a step-up, a refusal and a failure offer none. They travel as `CUSTOM alixai.suggestions`
`{"suggestions": [{"title", "message", "capabilityId"?}]}` just before `RUN_FINISHED`.

## 11. What is deliberately not emitted, and the voice statement

| Not emitted | Reason |
| --- | --- |
| `REASONING_*` (start, message start, content, end, chunk, end) and `REASONING_ENCRYPTED_VALUE` | model internals never leave the core. The verified response is the only model text a client sees; debug and trace logs already leak completions, and a stream is the last place to add another copy. Nothing in the TurnEvent API carries reasoning either |
| `SUBAGENT_STARTED`, `SUBAGENT_FINISHED`, `SUBAGENT_ERROR` | AlixAI is one agent; there is no delegation to report. The capabilities document says `multiAgent.supported: false` |
| `RAW` | a pass-through of an upstream provider's event. Nothing upstream is forwarded: provider output is untrusted data that the core validates and re-expresses, never relays |
| `TEXT_MESSAGE_CHUNK`, `TOOL_CALL_CHUNK` | the compact forms are a convenience for producers that a client expands. This package always sends the explicit start, content and end forms |

These types are accepted when read (`ParseEvent`, for the proxies of M3-27 and the conformance tests), including chunk forms
and types of a newer release, and are never produced: `Encode` refuses them (`TestReasoningSubagentAndRawAreNeverEmitted`
also checks that no scenario's output carries the members that belong to them: `rawEvent`, `encryptedValue`,
`subagentRunId`, `usage`).

**Voice.** Compatibility is claimed only for what is tested. Text turns carry transcripts like any other message; spoken
replies and the voice pipeline belong to the voice stack. **Full duplex voice is not claimed** and the
capabilities document says nothing that implies it; it will be claimed when a test proves it.

`agui.CapabilitiesFor(version, wiring)` is the document `GET /v1/agui/capabilities` serves: the protocol version, the event types
emitted and not emitted, the identifiers of the features not claimed and the release's `AgentCapabilities` object, validated
against the vendored schema and checked for overclaiming (`TestCapabilitiesAreValidAndNeverOverclaim`).
`agui.Capabilities(version)` is the document of a deployment that has wired nothing, and `agui.NotClaimed` is that baseline.
Two features follow the wiring (`TestAGUICapabilitiesClaimClientContextAndToolCallsOnlyWhenWired`): `client_context` is claimed
because the route hands the client's context to every engine call, and `client_tool_calls` (with `tools.clientProvided`) only
when the host registered at least one usable UI tool for the caller's application: no registry service, an empty one, one for
another application, one that fails, or one holding only names that can never be offered (reserved, malformed, a capability's)
claims nothing. The member `pending` of the published contract is always the empty list: nothing is accepted and left inert any more.

## 12. Feature matrix against CopilotKit

Status words: **core** works in `internal/agui` today and is tested; **gateway** needs `POST /v1/agui` (M3-25);
**adapter** needs `@alixai/copilotkit-adapter` (M3-26); **not claimed** is a statement, not a gap to be hidden. Rows that
depend on CopilotKit's own behaviour are verified against the pinned CopilotKit versions in M3-26 and published as the
version matrix in M3-28; nothing below is claimed from memory of a CopilotKit release.

| CopilotKit feature | AG-UI mechanism | AlixAI | Status |
| --- | --- | --- | --- |
| Chat, sidebar, popup and headless components (`useCopilotChat`) | the event stream over HTTP | `POST /v1/agui`, `AlixAIAgent` for CopilotRuntime | core + gateway + adapter |
| Streaming messages | `TEXT_MESSAGE_START/CONTENT/END` | verified response text, in order | core |
| Run lifecycle, steps, errors | `RUN_STARTED/FINISHED/ERROR`, `STEP_*` | pipeline stages as steps; errors terminal | core |
| Message history on load (`connect`) | `MESSAGES_SNAPSHOT` | connect run from the session history | core + gateway + adapter (`AlixAIRunner` sends the runtime's connect route through the agent) |
| Frontend actions and tools the model calls (`useCopilotAction`) | `tools` in the input, `TOOL_CALL_*` out, a tool message back | the model proposes `ui_action` among the tools the client declared **and** the host registered; arguments checked against the declared schema; emitted as one tool call; the result is data (section 5; `TestUIActionIsEmittedAsToolCallEvents`, `TestUIActionProposalIsAcceptedOnlyForDeclaredAndRegisteredTools`, `TestToolResultMessageIsFencedAndNeverApprovesAnything`) | core + gateway (`TestAGUIUIActionIsEmittedAndItsResultIsUntrusted`; golden streams `ui_tool_*`) |
| Backend tool calls rendered in the UI | `TOOL_CALL_START/ARGS/END/RESULT` | the confirmation call and its result; the continue-in-app call | core |
| Deterministic tool results | `TOOL_CALL_RESULT` | the receipt: the application's facts | core |
| Human-in-the-loop approval | `RUN_FINISHED` outcome `interrupt`, `resume`; tool-result form | confirmation interrupt; approve and reject; secret kept out of the browser | core + gateway + adapter |
| Generative UI | `ACTIVITY_SNAPSHOT/DELTA`, renderers by `activityType` | validated blocks as `alixai.block.<type>`; progress, citations, attachments | core (renderers: adapter) |
| Tables, charts, diagrams, forms | activities | table, chart, diagram blocks; forms are the `clarification_choices` block (a question with choices and optional free text); there is no arbitrary form block, because forms are data, not executable UI | core |
| Citations and source references | activity | `alixai.citations`: opaque, scope-checked references of the RAG answer | core |
| Attachments and previews | content parts with `file` sources; activity | pre-uploaded file references; previews from the files service (`previewable`) | core + gateway; inline bytes and URLs refused |
| Shared state (`useCoAgent`) | `STATE_SNAPSHOT/DELTA` | client-safe pending state under `alixai`; client state is untrusted input | core |
| Readable context (`useCopilotReadable`) | `context` | fenced, bounded, untrusted data placed in one block of the proposal prompt with a budget share of its own (section 4; `TestClientContextReachesTheProposalPromptAsFencedData`) | core + gateway (`TestAGUIClientContextReachesTheEngineAsFencedData`; golden stream `client_context`) |
| Agent state rendering (`useCoAgentStateRender`) | state events | the pending-goal state | core + adapter (`useAlixAIPendingState`, `AlixAIPendingState`, the chat renderer `showState`) |
| Predictive state updates | state events from streamed tool arguments | not produced; the engine runs a turn to completion before its events exist, so there are no streamed tool arguments to predict from, and state arrives as StateSnapshot and StateDelta from deterministic session state | not emitted by design |
| Chat suggestions | `CUSTOM` | deterministic suggestions, no model call | core (mapping: adapter) |
| Stop, regenerate, retry, reconnect | run control, cursor | cancel route, resumable stream with cursor, idempotent retry (section 9) | gateway + adapter (a closed connection cancels the run too) |
| Reasoning display | `REASONING_*` | not emitted (section 11) | not claimed |
| Sub-agents | `SUBAGENT_*` | not emitted | not claimed |
| Raw provider events | `RAW` | not emitted | not claimed |
| Voice | transcripts as messages | text and transcripts; full duplex not claimed | not claimed |
| Copilot Cloud, telemetry, public API key | - | not required, not used, never sent; the adapter documents and tests the switches that keep CopilotKit's own telemetry off | adapter |
| Transports | SSE, WebSocket | both, same events; prefer SSE for server-to-server (WebSocket is server-to-client only) | gateway |

## 13. Notes for the tasks that build on this

1. **M3-25 (gateway).**
   - Take `agui.Intake`'s `Request`. Resolve `AttachmentIDs` through `AttachModelFiles`, run `Input` through the engine, publish
     the TurnEvents into the stream hub as the sessions route does, then map them. Keep `RunContext` (thread, run, kind,
     language, state before and after, resolved `FileRef`s, resume reference, suggestion catalog of the capabilities the policy
     gateway exposes to the caller) beside the turn so a reconnect re-maps identically; set `RunContext.Withhold` to the
     secret of the turn.
   - Return the secret in `X-AlixAI-Confirmation-Token` (the gateway's `HeaderConfirmationToken`) on the run that prepared the
     action and only when its whole account was shown; read it from the same header on the resume and pass it to `Intake`.
   - Client data (M3-32, wired by M3-34): the route calls `req.EngineContext(ctx)` for `Engine.Turn`, puts a validated `Result.UIAction` into `RunContext.UIAction` (persisted
     with the rest, so a reconnect sends the same call) and answers a `Request.UIResult` run with `agui.UIResultRun`. The composition root gives the engine its allow-list
     (`agui.ui_tools`, section 5). The capabilities document claims client context and, for an application with a registered tool, client tool calls (section 11).
   - Serve `agui.Capabilities(version)` and map `Emitted.Seq` to the SSE `id:`.
   - Provide `Preparations` (the action service) and `IsCapability` (the package's capability ids) in `agui.Env`.
2. **M3-26 (adapter).** The browser must never see the secret header: `AlixAIAgent` runs in the host backend, stores the secret
   per thread and preparation, attaches it to the resume and deletes it on use. Map `alixai.*` activities and the two UI tools
   to CopilotKit renderers; map `alixai.suggestions` to CopilotKit's suggestion API; send the thread id as the session id the
   context token carries; send `forwardedProps.alixai.choiceId` for clarification buttons. M3-39 hardens it after the compatibility gate: `AlixAIRunner` answers the
   runtime's connect through the agent (authenticated by the host's callback and the gateway), turns CopilotRuntime's in-memory thread routes off, cancels the gateway run
   when the browser disconnects, and the adapter ships the renderer for the pending-goal state; the host still authenticates the whole runtime route (`info` and `stop`).
3. **M3-27 and M3-28.** `agui.ParseEvent` reads events; the vendored bundle and `VERSION` are the source for the Go and Python
   event types; the version matrix names `@ag-ui/core 1.0.1` as the release implemented.

## 14. Tests

`go test ./internal/agui/...` (hermetic, no network, no model): the nine named tests of the task plus the determinism,
lifecycle, abort, connect, state, capabilities, encode and schema-integrity tests, and, since M3-32, `TestUIActionIsEmittedAsToolCallEvents`,
`TestUIActionOfTheWrongShapeIsNeverEmitted` and `TestToolResultMessageIsFencedAndNeverApprovesAnything`. The security-relevant rules were checked
by mutation: removing the `<` neutralisation, the thread check, the withheld-value check, the credential-member check, the
reserved-name and capability-name checks, the strict approval payload, the token requirement, the allow-list of the state
projection, the directive-only suggestions or the error hold each fails a test.

`go test ./internal/dialogue/...` since M3-32: `TestClientContextReachesTheProposalPromptAsFencedData`, `TestClientContextHasABudgetShareOfItsOwn`,
`TestClientDataAbsentLeavesThePromptUnchanged`, `TestClientDataIsReadOnlyWhereTheProposalPromptIsBuilt`,
`TestUIActionProposalIsAcceptedOnlyForDeclaredAndRegisteredTools`, `TestUIActionCanNeverNameACapabilityOrCommit`,
`TestUIActionResultIsUntrustedDataInTheNextTurn`, `TestUndeclaredUIToolFailsClosed`, `TestUIToolSchemaCannotReferenceAnotherResource`,
`TestUIToolsAreNotOfferedWhenTheyWouldCutTheShortlist` and, in `internal/dialogue/context`, the client block tests. Checked by mutation, each of these fails a test:
dropping the registered check, the declared check, the capability-name check, the reserved-name check, the validation of the arguments against the declared schema,
the schema loader that loads nothing, the `client_context` entry of the fence list (in the builder and in the engine), the rule that a UI action never rides on a
confirmation, a second reader of the client data (a way for routing to see it), the UI call id prefix check of the intake, a UI result that approves, the
neutralisation of `<` in a tool result, the redaction of a confirmation secret and the shape check of the mapping.

`go test ./internal/gateway/...` since M3-34: `TestAGUIClientContextReachesTheEngineAsFencedData`, `TestAGUIUIActionIsEmittedAndItsResultIsUntrusted`,
`TestAGUIUIActionRefusalsFailTheWholeProposal`, `TestAGUIUIActionOfCarriesOnlyWhatTheRequestDeclared` and `TestAGUICapabilitiesClaimClientContextAndToolCallsOnlyWhenWired`;
`go test ./internal/agui/...`: `TestCapabilitiesClaimOnlyWhatIsWired`, `TestUIToolNameAllowedIsTheNameRuleOfIntake`, `TestUIResultRunHasNoHistoryAndNoTurn`,
`TestAnEmptyStateIsNotClientData`; `go test ./internal/cli`: `TestAguiUIToolsAreRegisteredOnlyByTheConfiguredSection`, `TestAguiEngineIsBuiltWithTheUIToolAllowList`;
`go test ./tests/agui/...`: the golden streams `client_context`, `ui_tool_call`, `ui_tool_result`, `ui_tool_undeclared`, `ui_tool_unregistered` and `ui_tool_capability_named`
(the model proposing a capability's id as the action; a client that declares a tool with a capability's name is refused with a 400 before any stream, which the gateway test
covers). Checked by mutation, each of these fails a test: the route not calling `EngineContext`, not copying `Result.UIAction` into the run, answering a UI result run through
the history path, `tools.clientProvided` claimed without a registered tool, the name rule of `UIToolNameAllowed`, and the engine builder not setting the allow-list.
The registration and declaration checks themselves are the engine's (the M3-32 mutations above); the refusal table of the gateway test runs each refusal through the real route.
