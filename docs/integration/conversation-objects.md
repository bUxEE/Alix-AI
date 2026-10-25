# Conversation and action objects

Design: [ADR 0006](../decisions/0006-prepare-confirm-commit.md) (prepare / confirm / commit). Where this page and the
schemas disagree, the schemas win and this page is the bug. The schemas under `schemas/conversation/` and
`schemas/actions/` are the contract; this page explains them. Go and TypeScript types are generated
from the schemas (M1-02), never written by hand.

The rule behind every object here: **the model can propose and verbalise
these objects; it cannot manufacture, authorise or alter their authoritative fields.** The schemas
encode that rule in a machine-readable annotation, `x-alixai-authority`, on every property.

## 1. What is defined

| Object | Schema |
| --- | --- |
| TaskProposal | `schemas/conversation/task-proposal.schema.json` |
| DialogueDirective | `schemas/conversation/dialogue-directive.schema.json` |
| PreparedAction | `schemas/actions/prepared-action.schema.json` |
| ActionReceipt | `schemas/actions/action-receipt.schema.json` |
| DeterministicResult | `schemas/actions/deterministic-result.schema.json` |
| TurnEvent (union of 10 event types) | `schemas/conversation/turn-event.schema.json` |
| RichBlock (union of 9 block types) | `schemas/conversation/rich-block.schema.json` |
| Citation | `schemas/conversation/citation.schema.json` |
| FileRef | `schemas/conversation/file-ref.schema.json` |
| Shared building blocks (fact, canonical value, data version, effect, choice, issue, IDs, ...) | `schemas/actions/shared.schema.json` |

Every schema is JSON Schema 2020-12 with `$id` `urn:alixai:schema:<area>:<name>:v1`. Examples live in
`<schema dir>/examples/<name>/{valid,invalid}/`. Fixed-shape objects use `additionalProperties:
false`; open maps (`fields`, `uncertainty.fields`, chart records) constrain their keys with
`propertyNames` and their values with `additionalProperties`.

## 2. The authority annotation

`x-alixai-authority` is a JSON Schema annotation (it constrains nothing by itself) with exactly three
values. It names **who may originate the value**:

| Value | Meaning | Examples |
| --- | --- | --- |
| `model` | The value comes from the language model. It is untrusted: validated, verified or sanitised before anything relies on it. | Every TaskProposal property except `trace_id`; the text of a streamed `token`; the text of a markdown block; chart data; a clarification prompt. |
| `alixai` | The value is minted or decided by the AlixAI core. It is never read from a model output, a client request or an adapter reply. | IDs (`preparation_id`, `receipt_id`, `audit_ref`), `expires_at`, `idempotency_key`, the effective `risk_level`, the dialogue `act`, the receipt `status`, event envelopes, `trace_id`. |
| `host` | The value is set only from a validated adapter result: it is a fact the host application reported. The core relays it verbatim. | Canonical facts and values, record IDs, `data_versions`, the canonical action summary and effects, `freshness`, the host's `status` and `errors` in a DeterministicResult. |

### How to read it

1. **Every property is annotated.** Every property defined in any fixed-shape object in
   `schemas/actions/` and `schemas/conversation/`, including properties inside `$defs`, carries one
   value. A map's annotation covers all of its entries; an array's covers all of its items.
   Constraint-only keywords (`if`, `then`, `else`, `not`, `contains`, `allOf` overlays) define no
   properties and carry no annotation.
   Objects referenced from `schemas/common` (`error`, `scope`, `ids`, `timestamps`) carry no
   annotation of their own: they take the authority of the property that references them (for
   example `error` in an `error` event is `alixai`, so the whole common error object is).
2. **A container labels who assembles it; its members label who originated them.**
   - Inside a `model` container every member is `model`.
   - Inside a `host` container every member is `host`.
   - An `alixai` container may hold members of any authority. For example a `directive` event is
     assembled by the core (`alixai`), its `facts` are `host`, its `act` is `alixai`.
3. **A model-settable value is one whose property and every ancestor are `model`.** Anything else a
   model emits is ignored or rejected. Model-originated data enters the system in exactly two ways:
   a constrained TaskProposal, and verbalised response text that the core then verifies and wraps.
4. **Adapter replies may populate `host` properties only.** An `alixai` property in an adapter reply
   (for example `operation` in a DeterministicResult) is discarded or the reply is `schema_invalid`.
   Client requests never supply authority-labelled fields.
5. **One def, one authority.** A shared def is used only where its authority applies. Where two
   authorities need the same shape there are two defs: `allowedNextAction` (what the host will
   accept, `host`) and `permittedNextAction` (what the core permits after policy, `alixai`).
6. **The label is about origin, not secrecy.** An `alixai` or `host` field can be shown to the user.
   `model` is the safe label for free content: core-templated text is still labelled `model` and is
   verified and sanitised like any model text.

### Who reads it

| Task | Use |
| --- | --- |
| M1-02 schemagen | May carry the annotation into generated types as metadata. |
| M1-09 adapter contract | Normalise adapter replies into DeterministicResult; accept `host` properties only. |
| M1-12 prepare/confirm/commit | Build PreparedAction and ActionReceipt; a proposal that tries to set `alixai` or `host` fields is ignored (`TestModelCannotAlterAuthoritativeFields`). |
| M1-13 response verifier | The facts a response may state are the `host` values of the DialogueDirective; text properties labelled `model` are the part under verification. |
| M1-14 turn pipeline | Derive the model's grammar from TaskProposal by keeping only `model` properties, then attach `trace_id` after the inference call. |
| M3-05 | Sanitise every `model` text property before emitting a block. |

## 3. TaskProposal

What the model may propose for a user turn: capability ID, candidate typed fields, candidate entity
references, an intent, explicit uncertainty and a trace ID. It is a proposal only: every value is
re-validated deterministically, and a value being present never makes it a fact.

| Property | Authority | Required | Meaning |
| --- | --- | --- | --- |
| `capability_id` | `model` | yes | The proposed capability, or null when none applies. |
| `fields` | `model` | yes | Candidate typed field values keyed by field name. |
| `entity_refs` | `model` | yes | Candidate references to application entities mentioned in the turn. |
| `intent` | `model` | yes | What the user is doing this turn: a new task, a correction of earlier values, cancelling the current goal, asking for help, or something outside the application's scope. |
| `uncertainty` | `model` | yes | Explicit uncertainty: the model states what it is not sure of instead of guessing. |
| `trace_id` | `alixai` | yes | Trace ID of the inference call that produced this proposal. |

Rules the schema enforces:

- `capability_id` is required but nullable. It must be non-null for intent `task` and `correction`,
  null for `out_of_scope`, and may be either for `help` and `cancel`. An `out_of_scope` proposal also
  carries no `fields` and no `entity_refs`.
- `fields` is a map keyed by lower snake_case field names (`propertyNames`); each value is a scalar,
  a flat list of scalars or a flat object of scalars. Nothing deeper.
- `trace_id` is `alixai`: the core assigns it from the provider call. The grammar handed to a
  constrained decoder is this schema with every non-`model` property removed from both `properties`
  and `required` (otherwise `additionalProperties: false` plus a required property that was removed
  is unsatisfiable); the core re-attaches `trace_id` before the proposal is validated and stored.

## 4. DialogueDirective

What the core tells the model, and the response verifier, to say this turn. Built by deterministic
code from validated state and adapter results. No property is `model`.

| Property | Authority | Required | Meaning |
| --- | --- | --- | --- |
| `turn_id` | `alixai` | yes | The turn this directive belongs to. |
| `capability_id` | `alixai` | yes | The capability the active goal concerns, or null when there is none (greeting, help, refusal before any capability was chosen). |
| `act` | `alixai` | yes | The dialogue act the response must perform. |
| `facts` | `host` | yes | The authoritative facts the response may state, from adapter results. |
| `must_state` | `alixai` | yes | Names of the facts (their field) the response must state. |
| `missing_fields` | `alixai` | yes | Required fields the user has not supplied yet, from the capability contract. |
| `invalid_fields` | `alixai` | yes | Supplied values that failed deterministic validation, with the typed reason. |
| `next_question` | `alixai` | no | The one question the response may ask next. |
| `allowed_suggestions` | `host` | yes | Suggestions the response may offer, taken from host data (for example alternative slots). |
| `confirmation` | `alixai` | yes | Whether the response must ask for confirmation, and how. |
| `permitted_next_actions` | `alixai` | yes | What the user may do next. |
| `sources` | `alixai` | yes | Sources the response may cite, where the act is grounded in them. |
| `preparation_id` | `alixai` | no | The PreparedAction a request_confirmation or request_step_up concerns. |
| `receipt_id` | `alixai` | no | The ActionReceipt a report_outcome states. |
| `reason_code` | `alixai` | no | The typed reason behind a refuse, report_failure, request_step_up or report_no_evidence: an ErrCode. |

The `act` drives which other members are required:

| `act` | Required by the schema |
| --- | --- |
| `ask_missing` | `next_question`, at least one `missing_fields` entry |
| `ask_invalid` | `next_question`, at least one `invalid_fields` entry |
| `disambiguate` | `next_question` with `answer_kind` `choice` and at least two `choices` |
| `request_confirmation` | `preparation_id`, `confirmation.mode` `explicit` |
| `request_step_up` | `preparation_id`, `reason_code`, `confirmation.mode` `step_up` |
| `report_outcome` | `receipt_id` |
| `refuse`, `report_failure`, `report_no_evidence` | `reason_code` |
| `present_result`, `offer_help`, `acknowledge_cancel` | nothing further |

`must_state` lists the `field` names of facts the response must state; each must name an entry of
`facts` (the verifier checks this, JSON Schema cannot). The response may not state any number, date,
time, amount or ID that is not in `facts`.

## 5. PreparedAction

A consequential action that the host validated and the core prepared, waiting for confirmation.
Nothing reaches an adapter's Commit unless a PreparedAction exists, is unexpired and still current.
No property is `model`.

| Property | Authority | Required | Meaning |
| --- | --- | --- | --- |
| `preparation_id` | `alixai` | yes | Opaque preparation ID. |
| `capability_id` | `alixai` | yes | The registered capability this action was prepared for. |
| `state` | `alixai` | yes | Lifecycle state (internal/actions): prepared, confirmed, committing, then committed, rejected, expired, invalidated or uncertain. |
| `summary` | `host` | yes | Canonical action summary from the host application's Prepare result. |
| `effects` | `host` | yes | The exact effects and their targets, from the host's Prepare result. |
| `risk_level` | `alixai` | yes | Effective risk level: the platform minimum for the capability, raised (never lowered) by the host's declared level. |
| `data_versions` | `host` | yes | The versions of everything the validation relied on, from the host's Prepare result. |
| `prepared_at` | `alixai` | yes | When the core created the preparation. |
| `expires_at` | `alixai` | yes | When the preparation stops being valid: the earlier of the platform limit and the host's valid_for_seconds, from the core's clock. |
| `invalidation_conditions` | `alixai` | yes | Events that void the preparation before it expires. |
| `confirmation` | `alixai` | yes | What must happen before the action may commit. |
| `idempotency_key` | `alixai` | yes | Key sent with Commit and Status so a repeated or recovered commit produces the same outcome. |

Rules the schema enforces:

- `risk_level` is a required integer 0 to 3. A missing level is never read as 0.
- Level 2 and 3 require `confirmation.mode` `explicit` or `step_up`; level 3 requires `step_up`.
- Any effect with `external: true` forces level 2 or higher.
- `data_versions` has at least one entry, so staleness is always detectable.
- `invalidation_conditions` always contains `data_version_changed`.
- `confirmation.bindings` is empty for mode `none` and lists all four bindings (`preparation_id`,
  `principal`, `session`, `data_versions`) otherwise.
- `state` mirrors the lifecycle of `internal/actions` (M1-12): `prepared`, `confirmed`,
  `committing`, `committed`, `rejected`, `expired`, `invalidated`, `uncertain`.

The effective `risk_level` is computed by the core as the platform minimum for the capability raised
by the host's `declared_risk_level` from the Prepare result: the host may raise a level and cannot
silently lower a platform-enforced minimum.

## 6. ActionReceipt

The durable outcome of an action. Built by the core from the adapter's Commit or Status result and
persisted before it is announced. A verified response may claim completion only when a receipt with
status `committed` exists. No property is `model`.

| Property | Authority | Required | Meaning |
| --- | --- | --- | --- |
| `receipt_id` | `alixai` | yes | ID of this receipt. |
| `preparation_id` | `alixai` | yes | The PreparedAction this receipt answers. |
| `capability_id` | `alixai` | yes | The capability that was committed or rejected. |
| `status` | `alixai` | yes | The outcome of the action. |
| `records` | `host` | yes | Stable application record IDs the action created, changed or cancelled, from the host's result. |
| `facts` | `host` | yes | Canonical result facts from the host's result: the only numbers, dates, times, amounts and IDs a response about this outcome may state. |
| `errors` | `alixai` | yes | Why the action was rejected, partial or uncertain: issues relayed from the host's result plus any the core adds itself (for example uncertain_outcome, expired). |
| `follow_up` | `host` | no | Status of queued follow-up work the host reported, where applicable. |
| `audit_ref` | `alixai` | yes | Reference to the audit record of the commit attempt. |
| `issued_at` | `alixai` | yes | When the core persisted the receipt. |

Rules the schema enforces:

| `status` | Schema requirement |
| --- | --- |
| `committed` | `errors` empty |
| `rejected` | at least one error, `records` empty (a rejection never suggests a record was created) |
| `queued` | `errors` empty, `follow_up` present |
| `partial` | at least one error |
| `uncertain` | at least one error (for example `uncertain_outcome`); the action is not retried blindly |

`status` is `alixai`: the core derives it from the host's DeterministicResult and its own recovery,
and `committed` is never inferred from anything but a host `ok`. `errors` is an `alixai` array because
the core may add its own issues (`uncertain_outcome`, `expired`) to the ones the host reported.

## 7. DeterministicResult

The normalised form of every adapter result: status, identifiers,
canonical values, errors, freshness and allowed next actions. No critical decision may depend on model
prose, so an adapter result that does not normalise into this object is a `schema_invalid` error and
is never partially trusted. Failures the core detects itself (adapter unreachable, timeout, malformed
reply) are typed errors (`common/error`), not DeterministicResults.

| Property | Authority | Required | Meaning |
| --- | --- | --- | --- |
| `operation` | `alixai` | yes | The adapter operation that produced this result. |
| `capability_id` | `alixai` | yes | The capability the call was made for. |
| `status` | `host` | yes | The outcome as the host reports it. |
| `identifiers` | `host` | yes | Stable IDs of the records the result concerns (read, created, changed or cancelled). |
| `values` | `host` | yes | Canonical values: the typed facts the host reports. |
| `errors` | `host` | yes | Typed issues (ErrCode plus the field concerned). |
| `freshness` | `host` | no | When and against which data version the result was produced. |
| `allowed_next_actions` | `host` | yes | The next steps the host will accept for this object. |
| `follow_up` | `host` | no | Queued or deferred work the host reports. |
| `preparation` | `host` | no | What the host validated and would do, present exactly when operation is prepare and status is ok. |

The `preparation` member (present exactly when `operation` is `prepare` and `status` is `ok`) is
what the core builds a PreparedAction from:

| Property | Authority | Required | Meaning |
| --- | --- | --- | --- |
| `summary` | `host` | yes | Canonical one-sentence summary of the action. |
| `effects` | `host` | yes | The exact effects and their targets. |
| `data_versions` | `host` | yes | The versions of everything the validation relied on. |
| `declared_risk_level` | `host` | yes | The risk level the host application assigns. |
| `valid_for_seconds` | `host` | no | How long the host will honour the preparation. |

Rules the schema enforces ("no success-looking generic message"):

- `status` `ok` and `queued` have an empty `errors`; every other status has at least one error.
- `ok`, `queued` and `partial` carry `freshness`; `queued` carries `follow_up`.
  `freshness` is optional for `rejected`, `uncertain` and `unavailable`, where the host may have been
  unable to read its data; the integration audit requires freshness for every result, so a probe that wants it
  everywhere (M1-18) must treat these three statuses as the documented exception.
- `errors` entries are `{code, field?, message}` where `code` is the ErrCode enum of `common/error`
  (referenced, not copied, so it cannot drift from `pkg/contracts`).

### Canonical values and facts

A fact is `{field, value, display?}`. The value is a typed `canonicalValue`, discriminated by `kind`,
so the verifier can compare normalised numbers, dates, times, amounts and IDs instead of matching
prose.

| `kind` | `value` | Notes |
| --- | --- | --- |
| `text` | string up to 4096 characters | |
| `integer` | integer within the JSON-safe range | |
| `decimal` | string, `-?digits(.digits)?`, up to 9 fraction digits | Strings keep binary floating point out of money-like values |
| `boolean` | boolean | |
| `date` | `YYYY-MM-DD` | No time zone |
| `time` | `HH:MM` or `HH:MM:SS` | No date, no time zone |
| `datetime` | RFC 3339 UTC instant with `Z` | Optional `zone` is the IANA zone the host displays it in |
| `money` | decimal string | `currency` is an ISO 4217 code and is required |
| `identifier` | the common identifier grammar | A stable application ID |

## 8. TurnEvent

One event of a streamed turn, discriminated by `type`. Every event repeats the envelope
`type`, `turn_id` and `seq`. `seq` starts at 0 and rises by one per turn; it is also the resume cursor
(M3-05). Events are emitted only by the core.

| `type` | Members beyond the envelope (authority) | Meaning |
| --- | --- | --- |
| `token` | `text` (model) | A fragment of the response text, in order. |
| `directive` | `directive` (alixai) | The DialogueDirective the response is realised from, for clients that show or log what the core decided. |
| `clarification` | `clarification` (alixai) | A structured clarification the user answers by selecting a choice. |
| `prepared_action` | `prepared_action` (alixai) | A PreparedAction awaiting confirmation. |
| `receipt` | `receipt` (alixai) | The ActionReceipt of a committed, rejected, queued, partial or uncertain action. |
| `citation` | `citation` (alixai) | One source supporting the answer, emitted as the answer streams. |
| `progress` | `stage` (alixai), `state` (alixai), `percent` (alixai), `capability_id` (alixai), `job_id` (alixai) | Progress of the turn: pipeline stage, queued work, a capability call or source retrieval. |
| `error` | `error` (alixai) | A typed failure of the turn or of one stage of it. |
| `done` | `finish_reason` (alixai), `preparation_id` (alixai) | The last event of a turn. |
| `block` | `block` (alixai) | One rich block of the response: markdown, code, table, chart, diagram, citation_list, action_card, clarification_choices or confirmation_panel. |

Notes:

- The plan lists nine event types; `block` is a tenth added here so that every RichBlock has a place in
  the stream. A client that ignores `block` still gets the text through `token` events.
- `token` text is emitted only after the response verifier accepted it, so streamed text never states
  a fact the directive does not allow.
- A `failed` stage or turn is always accompanied by an `error` event; an `error` is never replaced by
  a success-looking message.
- `done` with `finish_reason` `awaiting_confirmation` carries the `preparation_id`; any other reason
  must not.
- `error` carries the common typed error, including its `code`/`retryable` consistency rule.

### Delivery and resume

The events of a turn are delivered by the stream hub (`internal/stream`, M3-05), through two routes
of the public API:

- `POST /v1/sessions/{session_id}/turns` runs the turn and answers with its events, as one JSON
  document or, with `Accept: text/event-stream`, as server-sent events. The events are published
  into the hub first and read back from it, so what this response carries and what a resume carries
  are the same bytes.
- `GET /v1/sessions/{session_id}/turns/{turn_id}/events` reads the turn again from a cursor, as server-sent
  events or (with `Upgrade: websocket`) a WebSocket. It only reads: the turn is never run again because
  a client reconnected.

What a client can rely on:

- **Cursor.** The cursor is the `seq` of the last event received, in the `Last-Event-ID` header or the
  `cursor` query parameter (the later wins); the stream continues with the next event, each exactly once
  and in order. A cursor that is malformed or ahead of the stream is `400`, one behind the retained
  window is `410` (the stream is never resumed with a gap), and one at the end of a finished turn is
  `204`: stop reconnecting. The hub keeps a bounded number of events per turn and keeps a finished turn
  for a few minutes.
- **Sequence numbers are assigned when an event is published.** `seq` runs from 0 without gaps. An
  event the hub refuses (it fails validation, or is too large) is not sent and does not use up its
  number, and it is not skipped over: the turn ends with an `error` event and a `done` event with
  `finish_reason: failed`, at the next numbers, and the confirmation secret is withheld.
- **`token` text is sanitised as a whole, then cut.** The core sanitises the whole response (raw HTML and
  images removed, links limited to the allowed schemes) and only then cuts it into `token` events of at
  most 4096 characters, so no fragment boundary can split a construct into two harmless halves that
  become active when joined. Accumulate the fragments and render the accumulated text as Markdown; see
  section 9 for how to render it.
- **A turn id is no oracle.** An unknown turn, another principal's turn and a turn past its retention
  are the same `403`.
- **Every connection needs a fresh context token.** Tokens are single use, so a reconnect mints a new
  one. A browser `EventSource` cannot set the `Authorization` header and reconnects on its own, so it
  cannot be used under header authentication; read the stream with a fetch-based reader (the SDK's) and
  send `Last-Event-ID` yourself.
- **The confirmation secret is never in a stream.** It is not a TurnEvent member: it is the
  `X-AlixAI-Confirmation-Token` header of the turn's own event-stream response, or `confirmation` in its
  JSON response, shown once to the client that made the turn. A resume does not repeat it.

The wire format, the limits and the WebSocket close codes are in the documentation of `internal/stream`
and in `schemas/api/paths/sessions.yaml`.

## 9. RichBlock

One block of a rich message, discriminated by `type`. Blocks are data: no block has a URL, a path, an
expression, a script, a callback or an HTML member, and every fixed-shape object forbids additional
properties. The host application decides which renderers are enabled; a client that does not support a
type shows the text alternative.

**Rendering these blocks, and the text of `token` events, safely is the host's side of the bargain.** The
core sanitises and bounds what it sends, but it cannot know what your page does with it. The
Content-Security-Policy to serve the page with, the recommended renderer defaults (which block types to
enable, which to leave off, how to render each) and the rules for a safe renderer are in
[Rich blocks in a host application: renderers and content-security policy](../../internal/blocks/HOSTS.md).

| `type` | Members (authority; `?` optional) | Meaning |
| --- | --- | --- |
| `markdown` | `text` (model) | Markdown text. |
| `code` | `language`? (model), `code` (model) | A code listing, shown verbatim and never executed. |
| `table` | `caption`? (model), `columns` (model), `rows` (model) | A table of plain values. |
| `chart` | `title`? (model), `description` (model), `mark` (model), `data` (model), `encoding` (model) | A restricted, declarative chart: the data is inline, the mark comes from a fixed enum and encodings name data fields. |
| `diagram` | `language` (alixai), `requires_host_renderer` (alixai), `title`? (model), `description` (model), `source` (model) | A diagram as typed text. |
| `citation_list` | `citations` (alixai) | The sources supporting an answer, one Citation each. |
| `action_card` | `title` (host), `capability_id`? (alixai), `facts` (host), `actions` (alixai), `preparation_id`? (alixai), `receipt_id`? (alixai) | A structured application result or an offer of next steps: a title, canonical facts and the actions the user may take. |
| `clarification_choices` | `field` (alixai), `prompt`? (model), `answer_kind` (alixai), `allow_free_text` (alixai), `choices` (host) | A clarification the user answers by selecting one choice. |
| `confirmation_panel` | `preparation_id` (alixai), `summary` (host), `effects` (host), `risk_level` (alixai), `expires_at` (alixai), `confirmation` (alixai) | The confirmation of a PreparedAction: the canonical summary, the exact effects, the risk level, the expiry and how it may be confirmed. |

### Markdown, code and table

Markdown blocks carry text only. Sanitisation happens in the core (M3-05) before a block is emitted;
the schema does not try to sanitise Markdown. The same goes for the response text of a turn: it is
sanitised as a whole and then cut into `token` events (section 8). Renderer rules and the
Content-Security-Policy are in [HOSTS.md](../../internal/blocks/HOSTS.md). Code blocks are shown verbatim and never executed. A
table's cells are plain scalars and are rendered as text; every row has one cell per column (checked
by the core).

### Chart

A restricted, declarative spec in the spirit of a minimal Vega-Lite, with everything executable
removed:

- data is **inline**: an array of flat records of scalars keyed by field name, at most 1000 records of
  16 fields; there is no way to name a URL;
- `mark` is one of `bar`, `line`, `area`, `point`, `pie`;
- `encoding` maps channels (`x`, `y`, `color`, `theta`) to `{field, type, title?}`; a cartesian mark
  requires `x` and `y` and forbids `theta`, a pie requires `theta` and `color` and forbids `x` and `y`;
- there are no expressions, transforms, aggregates, calculated fields or scales: the data is
  pre-aggregated, so the chart is exactly reproducible from what it declares;
- `description` (the text alternative) is required;
- every string in titles and data is plain text: a `<` may not start an HTML tag (`<script>` is
  rejected, `<18` is fine). This is defence in depth, not a replacement for sanitisation and
  text-only rendering.

That every encoded `field` exists in the data, and that figures stating application facts match the
DialogueDirective, are checked by the core and the verifier: JSON Schema cannot express them. A chart
must not hide an unverified application fact.

### Diagram

Typed text flagged as needing a host renderer: `language` is `mermaid`, `requires_host_renderer` is
the constant `true`, and AlixAI ships no renderer. As defence in depth the schema rejects Mermaid
front matter, `%%{ ... }%%` directives (which can loosen the renderer's security level), `click` and
`link` statements (callbacks and URLs), `javascript:` text and HTML tag starts. The host's renderer
must still run Mermaid in its strict security mode.

### Action card, clarification choices and confirmation panel

These present application state, so they are assembled by the core from PreparedActions,
ActionReceipts, citations and adapter results. A confirmation panel exists only when confirmation is
required (`mode` `explicit` or `step_up`): the user confirms the effects the host validated, not the
model's description of them. A `clarification_choices` block is also the payload of the
`clarification` event; selecting a choice sends its `choice_id`.

## 10. Citation

Every successful RAG result exposes a Citation: source ID, authorised display name,
source version or content hash, a locator (page, section, record or span) when known, availability and
staleness, a permission-checked open reference and the answer claims the source supports. It is
produced by the core after the final authorisation and version recheck; a model cannot cite a source
the retrieval did not return. When no authorised evidence supports an answer the result is the
typed `no_supported_evidence` result (`schemas/rag/no-supported-evidence`; the same string is also a common error
code), not an empty citation.

| Property | Authority | Required | Meaning |
| --- | --- | --- | --- |
| `source_id` | `alixai` | yes | ID of the registered source. |
| `display_name` | `alixai` | yes | The source's name as the requesting principal is authorised to see it. |
| `version` | `alixai` | yes | Source version or content hash the answer was grounded on. |
| `locator` | `alixai` | no | Where in the source the evidence sits, when known: a page, a section, a record or a character span. |
| `availability` | `alixai` | yes | Whether the source can still be opened. |
| `staleness` | `alixai` | yes | Whether the cited version is the latest. |
| `open_ref` | `alixai` | yes | Permission-checked way to open or preview the source. |
| `claims` | `alixai` | yes | The answer claims this source supports, one short statement each, as verified by the core's claim/source check. |

`open_ref` is a typed, opaque reference (`file`, `record` or `source`), never a URL or a path;
resolving it re-checks scope and expiry every time.

## 11. FileRef

A scoped, opaque reference to an attachment or generated file. It has no path, no URL and no
storage key: the model never receives a filesystem path, and a client resolves the file only through
an authorised API call. The MIME type is sniffed from the bytes by the core and must match the declared
type. Every property is `alixai`.

| Property | Authority | Required | Meaning |
| --- | --- | --- | --- |
| `file_id` | `alixai` | yes | Opaque file ID. |
| `name` | `alixai` | yes | Display name, sanitised by the core: a bare file name with no path separators and no control characters. |
| `mime_type` | `alixai` | yes | The sniffed media type, lower case, for example image/png. |
| `size_bytes` | `alixai` | yes | Size in bytes. |
| `content_hash` | `alixai` | no | SHA-256 of the content, as sha256: followed by 64 lower-case hex digits. |
| `owner` | `alixai` | yes | The principal that owns the file, inside the same tenant and workspace as the request. |
| `origin` | `alixai` | yes | Where the file came from. |
| `treatment` | `alixai` | no | How the host's attachment policy treats the file: ephemeral turn context, a registered RAG source, or an application object. |
| `permissions` | `alixai` | yes | What the requesting principal may do with the file. |
| `previewable` | `alixai` | yes | True only when a safe previewer exists for the sniffed type. |
| `expires_at` | `alixai` | yes | When the reference stops resolving. |

### Application events: the notice and the session-level announcement

An application event (`POST /v1/events`, signed by the host; `adapters/events`) can only invalidate: it voids the
preparation a session waited on, clears what was derived from stale facts and tells the session. It prepares, confirms
and commits nothing. The session is told with a **notice**: a turn of its own, in the hub under the session's scope,
made of core data only: an `error` event (`code: stale`, `retryable: false`, the fixed text "...Nothing was
committed.", the event id, type and kind and what was cleared in `details`) at `seq` 0, then `done` with
`finish_reason: failed` at `seq` 1. The schema has no "state changed" `TurnEvent` variant and none was added: a typed
stale error is the honest existing event, and a failed turn is never replaced by a success-looking message.

The hub is keyed per turn and a client never learns the id of a turn it did not start, so the notice alone reaches
nobody. The decision (task M3-22) is a **session-level announcement**:

- `GET /v1/sessions/{session_id}/events` is a server-sent event stream of the session's announcements (`id` = the
  announcement's `seq`, numbered from a time-based value (the Unix time in seconds when the session's feed began) and
  only increasing; `event: state_changed`; `data` = `{seq, type, event_id, event_type, kind, turn_id, at}` on one
  line). It replays the retained announcements after the cursor (`Last-Event-ID` or `cursor`), then follows live,
  with `retry:` and `: keep-alive` like the turn stream. The scope is the verified token's, whole; the session in the
  path must be the token's; a cursor older than the feed (kept from before a restart) replays it from its start, a
  cursor ahead of the feed is `400`; a session has at most four open streams.
- The client then reads the notice with `GET /v1/sessions/{session_id}/turns/{turn_id}/events`, under a fresh token,
  exactly like any turn.
- The announcement carries identifiers only (no text, no state). It is a **hint**: kept in memory for as long as the
  notice turn is (the hub's retention, five minutes by default), at most 16 per session, 4096 sessions, lost with the
  process. `GET /v1/sessions/{session_id}/pending` stays the authority on what is pending.

Rejected: a field on the next turn response (it needs a client that sends a turn first, which is the client that
lost its confirmation without knowing, and a change to `schemas/api` and the sessions route), a new `TurnEvent`
variant (a schema change to every client of every turn stream), and a long-lived hub turn per session (the hub's turns
end with `done`).

**AG-UI (ADR 0035).** A CopilotKit client is on `POST /v1/agui`, not on the route above, and sees the change as
**state**: every run and every connect run starts from the client-safe projection of the session (`agui.ProjectState`),
which after an event no longer shows the voided confirmation, so the next run emits a `STATE_DELTA` on `/alixai` (a
`STATE_SNAPSHOT` when the client sent no object state) that removes it (`TestEventsChangeTheStateAGUIProjects`). That
needs nothing new in `internal/agui`. The reason ("the application reported a change", which event) is the
announcement; carrying it into AG-UI as a `CUSTOM` event named `alixai.state_changed` right after `RUN_STARTED` of the
next run and of a connect run is a **proposal** for `internal/agui` and `routes_v1_agui.go`, which read
`EventsFeed` for the run's scope; it is not implemented by M3-22 and no `ACTIVITY_SNAPSHOT` is used.

## 12. Decisions made in M1-03

- **`TaskProposal.capability_id` is nullable**, with the per-intent rules in section 3, so help,
  cancel and out-of-scope turns do not have to invent a capability. It stays in `required`.
- **`trace_id` is `alixai`.** The model never writes it; the core attaches it after the inference
  call. The model's grammar is the `model`-only subset of the schema.
- **Authority is annotated at every level** and containers follow the nesting rules in section 2.
- **A tenth event type, `block`**, carries RichBlocks. The nine listed types are all present.
- **Canonical values are typed** (section 7) rather than free scalars, because the verifier (M1-13)
  needs to normalise numbers, dates, times, amounts and IDs.
- **`DeterministicResult.operation` and `capability_id` are `alixai`**: the core knows which call it
  made, so it never trusts the reply for them.
- **`ActionReceipt.status` is `alixai`**: the core decides it from the host result and its own
  recovery; `committed` still requires a host `ok`.
- **ErrCode is referenced**, not copied: issues and `reason_code` use
  `urn:alixai:schema:common:error:v1#/properties/code`.
- **Open maps use `propertyNames`** to constrain keys. `common/error` `details` remains an unconstrained
  open map (`schemas/common/` is outside M1-03's scope).

## 13. Working with the schemas

```bash
go run ./tools/schemacheck schemas/conversation schemas/actions       # compile and check every example
go run ./tools/schemacheck -v schemas/conversation schemas/actions    # also show why each invalid example fails
```

Every `valid/` example must pass and every `invalid/` example must fail; the checker reports any
mismatch. An invalid example should fail for exactly the reason in its name, so derive it from a valid
example by changing one thing. The examples are also fixtures for the Go and TypeScript conformance
tests of later tasks.

When adding a property: annotate it with `x-alixai-authority`, respect the nesting rules, add a valid
example that uses it and an invalid example that breaks its constraint. When two authorities need the
same shape, add a second def instead of reusing one.
