# 0049 — Reference tokens in analyst prose

- **Status:** accepted
- **Date:** 2026-10-27
- **Decided in:** release 1.5.1

## Context

Analyst turns (ADR 0047) withhold `confidential` / `restricted` / `local_only`
cell values from third-party models (egress at most `internal`). The model may
still plan blocks by `dataset_id` (core materialises values). It cannot name
those values in prose ("The assembly was attended by …") without inventing them,
which verification replaces with the analyst template.

Hosts need natural-language answers that include the withheld values for the
user, without ever sending those values to the provider — including on later
turns that replay assistant history into the prompt.

## Options

### How the model names a withheld cell

- **A. Invent the value; core substitutes by fuzzy match.** Rejected: ambiguous
  and re-opens invention of neighbouring numbers.
- **B. Opaque reference token in `text_plan` only.** Taken.
  `{{ref:<dataset_id>:<row>:<field>}}` (e.g. `{{ref:ds_example:0:attendee_name}}`).
  The syntax cannot collide with Markdown emphasis or ordinary user braces; row
  is the 0-based index visible from non-withheld columns. Tokens are forbidden
  in titles and block plans (blocks already bind by dataset).

### When values are filled in

- **A. Before verification (expand then verify).** Rejected: the model would be
  credited with stating secrets it never saw; streaming could leak mid-token.
- **B. After verification, server-side, before the client.** Taken. Invalid or
  unknown tokens → same template replacement as any other violation. Resolved
  values count as `app`-sourced facts; numbers inside them are trusted because
  they come from the dataset. Formatting follows the field type and the turn
  language (`en` / `it`: dates, money, numbers).

### Token vs resolved form

- **A. One stored string (resolved) for everyone.** Rejected: the next turn's
  provider request would re-egress confidential values from session history.
- **B. Keep the two forms apart.** Taken. Model window / prompt assembly /
  memory proposals keep the token form (or refuse tokens in memory). Client
  stream, AG-UI text, and user-visible transcript/exports show resolved values.
  A token split across verified stream segments (ADR 0046) is held until
  complete so a half-resolved token is never emitted.

### Client-resent AG-UI history

CopilotKit and AG-UI clients resend the whole thread on every run. The client's
copy of an assistant message holds the **resolved** text. Those assistant-role
messages must never become prompt "hints": only earlier **user** messages from
the client may enter `buildContext`. Prior assistant turns for the model come
only from the server's `session_turns` (token form). A host that copies displayed
answers into `useCopilotReadable` / shared state is sending them as client data
(readable context / shared state); the AlixAI kits never do that.

## Decision

Take opaque tokens in `text_plan` (B), resolve after verify (B), dual form (B),
drop client assistant hints (AG-UI).

Rules:

1. Step prompt lists withheld fields with the exact syntax and an example.
2. `CheckAnalystRefs` / `VerifyAnalyst` require an existing dataset of this turn,
   a row in range, and a field of that dataset; anything else → template.
3. `ResolveAnalystRefs` runs after a verified answer (loop / stream) before
   publish; `AnalystStream` resolves each emitted segment and keeps tokens in
   the accumulator for `ModelText`.
4. Dialogue persists `Assistant` (token / model-window form) into `session_turns`
   and `AssistantDisplay` (resolved) into `transcript_turns`. Prompt assembly
   reads only `session_turns`. History API / session view / exports / AG-UI
   reconnect read the transcript (display form). `Response.Text` is resolved.
5. Memory proposals must not contain `{{ref:…}}` (turn-local placeholders).
6. AG-UI `buildContext` drops assistant-role client messages.

## Consequences

Prose can name withheld attendees and similar fields without provider egress,
including on later AG-UI runs that resend the thread. Schema description of
`answer.text_plan` documents the syntax (additive).

## Reversal plan

Stop teaching tokens in the prompt; treat leftover `{{ref:…}}` as unknown
identifiers → template. No frozen-schema removal.
