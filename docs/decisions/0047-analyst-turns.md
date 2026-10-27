# 0047 — Analyst turns: multi-step reads, core-computed facts, model-chosen presentation

- **Status:** accepted
- **Date:** 2026-10-27
- **Decided in:** release 1.5.0

## Context

AlixAI 1.4 keeps a trustworthy dialogue path: one TaskProposal → one capability → one
DialogueDirective → verified response (see `internal/dialogue/doc.go`). ADR 0044 lets an
external model word a reply from verified facts and emit table/chart blocks only when every
number matches a directive fact; otherwise the template wins. ADR 0046 streams verified
sentence segments with `text_replaced` on failure.

That path cannot answer requests that need several reads, arithmetic over many rows,
forecasts, domain explanations, drafts, or web-backed eligibility rules. The owner’s goal for
1.5 is an analyst companion that **thinks and answers inside the host app’s domain**, while
staying fail-closed: the model plans, reasons and explains; the **core** reads host data and
does the arithmetic; numbers and named entities stay traceable; writes stay prepare → confirm
→ commit. Contracts are additive on the frozen 1.4 set. Local mode and the single-capability
path stay the default when analyst abilities are off.

## Options

### How the model acts across steps

- **A. Native provider tool-calling.** Rejected for 1.5: behaviour differs across OpenAI,
  Anthropic, Gemini, vLLM and LocalAI; grammar and parallel-tool semantics are not identical;
  AlixAI already strips non-model fields from TaskProposal for constrained JSON.
- **B. Constrained-JSON step proposals (TaskProposal style) per loop iteration.** Taken. One
  schema, every provider; the core validates, executes, and feeds the next prompt.

### Where arithmetic and forecasts run

- **A. Sandboxed code execution (Python/JS in a jail).** Rejected for 1.5: resource, escape and
  supply-chain risk on Lite; possible later ADR.
- **B. Deterministic core compute engine (fixed op catalog, no code exec).** Taken. Ops return
  datasets / derived facts with provenance; insufficient data → typed refusal.

### Domain questions that match no capability

- **A. Keep scopegate as the only gate (word overlap → out_of_scope).** Rejected: refuses
  in-domain questions that need analysis, not a capability.
- **B. Scopegate remains a cheap hard refusal for clearly unrelated text; an enabled domain
  classifier (model-judged, typed) decides in-domain vs out-of-domain when the analyst path is
  active; out-of-domain → polite typed refusal with no search/read.** Taken.

### Web and general knowledge

- **A. Always on when analyst is on.** Rejected: egress and hallucination risk.
- **B. Separate ability toggles (`knowledge`, `web_search`, `web_fetch`), off by default;
  domain declared by the application package; web queries are egress category `web_query` and
  must not carry confidential/restricted app values; claims carry source labels.** Taken.

### Ability enablement vs model strength

- **A. Strong models silently unlock abilities.** Forbidden.
- **B. Every ability has its own switch at deployment, application and tenant (most
  restrictive wins). Model catalog/route flag `analyst_capable` only decides what *can* be
  enabled; off at any level = off for every model; non-capable models cannot enable analyst
  abilities even when switched on (typed reason).** Taken.

### Presentation

- **A. Model inlines numbers in block JSON.** Rejected: re-opens invention.
- **B. Model chooses block plan referencing dataset ids; core materialises verified blocks;
  new types only when the client announced support; else table/text fallback.** Taken.

## Decision

Take constrained-JSON step loop (B), core compute (B), combined scopegate + domain classifier
(B), per-ability toggles with `analyst_capable` gate (B), dataset-referenced presentation (B).

### Step loop (opt-in)

When the effective ability set includes `loop` (`dialogue.analyst.enabled` and
`abilities.loop`, application and tenant not narrowing them off) and the resolved controller
is `analyst_capable`, a turn may enter a **bounded read-only step loop** instead of the
single-capability path. Each step is one constrained-JSON **AnalystStepProposal**:

| kind | Payload | Effect |
| --- | --- | --- |
| `read` | `capability`, `arguments` | Shortlist + policy + tenant scope + adapter Read → **dataset** |
| `compute` | `op`, `inputs` (dataset ids), `params` | Core compute → new dataset / derived facts |
| `search` | `query` | Web search provider (ability `web_search`) → result set (untrusted, fenced) |
| `fetch` | `url` from a prior search hit | Fetch page text (ability `web_fetch`) → fenced document + citation |
| `answer` | text plan, block plan, source spans | End loop; verify and emit |
| `clarify` | question / choices | Hand off like today’s clarification |
| `act` | capability fields | Hand off to normal write path (prepare → confirm → commit); **never** commits inside the loop |

No write inside the loop. Caps (configurable, safe defaults): max steps, reads, records per
dataset, total records, tokens, wall time, turn cost. Progress TurnEvents use stable stages
`analyst_read`, `analyst_compute`, `analyst_search`, `analyst_fetch`, `analyst_answer`.

### Datasets and compute

A **dataset** has id, field schema (name, type, unit/currency, data class), rows, and
provenance (read: capability + filters; or compute: op + params + input ids). Compute ops
(deterministic): `filter`, `sort`, `group_by`, `aggregate` (count, sum, avg, min, max, median,
percentile), `ratio`, `percent_change`, `difference`, `rank` / top-n, `join`, `moving_average`,
`cumulative`, `date_bucket` (day/week/month/quarter/year), forecasts `forecast_linear`,
`forecast_holt`, `forecast_seasonal_naive` (method, horizon, interval; insufficient data →
`insufficient_data`). Null handling, unit/currency match-or-refuse, and rounding are fixed in
`schemas/analyst/compute-op.schema.json`. Output fields inherit the most restrictive data class
of contributing inputs; aggregates of restricted fields stay restricted for egress even when
local compute used them.

### Verifier and templates

Every number and every named record/entity in answer text must trace to a read fact or derived
fact under the formatting rules (exact digit forms, grouping, compact forms like `12.4k`,
percentages). Forecasts must be labelled as estimates with method. Qualitative reasoning
without numbers is allowed. Untraceable → `text_replaced` (streaming) or template. Analyst
template: a **table of the final dataset** plus a **provenance** block. Source labels on
spans/blocks: `app`, `computed`, `web`, `knowledge` — enforced as in
`docs/integration/domain-scope.md`.

### Presentation and rich blocks (additive)

Blocks reference `dataset_id` (core fills values). New chart marks: `scatter`, `heatmap`,
`combo_bar_line`; bar variants via `bar_layout` (`grouped`|`stacked`|`horizontal`); donut via
`mark: pie` + `pie_style: donut` (frozen pie if/then unchanged). New blocks: `kpi`, `timeline`,
`record_card`, `provenance`, `draft`.

### Client announcement and 1.4 emit surface (mandatory)

A 1.4.x SDK validates TurnEvents strictly against `schemas/_frozen/v1`. Emitting a new progress
`stage` (`analyst_*`), a new block `type`, a new chart `mark`, or a new member such as
`bar_layout` / `pie_style` / `dataset_id` / `source` (objects are `additionalProperties: false`)
makes that client reject the event — the same failure class as unannounced `text_replaced` in
1.4.0.

**Announcement.** Optional `client_capabilities` (`schemas/analyst/client-capabilities.schema.json`)
on `POST /v1/sessions/{id}/turns` and on AG-UI `forwardedProps.alixai.clientCapabilities`. The SDK
of a release fills it with exactly what it can parse; kits copy that. **Absent = 1.4.**

**Mapping when unannounced.** The server emits ONLY items the client announced; otherwise it maps
to the 1.4 form before send:

| 1.5 item | 1.4 fallback |
| --- | --- |
| `analyst_read` / `analyst_search` / `analyst_fetch` | `retrieving` |
| `analyst_compute` | `generating` |
| `analyst_answer` | `verifying` |
| `kpi`, `timeline`, `record_card`, `provenance`, `draft` | `table` or `markdown` |
| `scatter`, `heatmap`, `combo_bar_line` | closest 1.4 mark (`point` / `bar`+`line`) or `table` |
| `bar_layout`, `pie_style`, `dataset_id`, `source` | omitted |
| source labels / web citation chrome | omitted |

Acceptance for implementation: every TurnEvent a 1.5 server sends to a client that announced
nothing validates against `schemas/_frozen/v1`.

### Egress and budget

Datasets sent to an external model obey tenant egress at `directive_class`. Restricted fields
may feed local compute; only permitted aggregates/fields leave. `web_query` is a new send
category: the core checks the query against data classes of datasets read in the turn — no
record values beyond policy. Web results are untrusted and fenced; web numbers must appear in
the cited fetch. Budget: reservation per model call plus `analyst.max_turn_cost` / tokens;
exhaustion mid-loop → answer from computed-so-far if an `answer` can be verified, else typed
`resource_exhausted` / analyst reason. Search/fetch have their own caps.

### Prompt injection

Retrieved record text and web pages are fenced as data. The loop is read-only: worst case is
extra reads within the caller’s scope. Injection must not cause writes, foreign-tenant reads,
or out-of-domain answers. Required tests listed in Consequences.

### Worked examples (acceptance)

1. **“Is a Persian cat big?”** → out of domain → typed polite refusal; no model answer body
   beyond the refusal template, no search, no read.
2. **“Should my company have applied for the Italian 2026 company discount XX?”** → in domain
   → read company data via capabilities; search official eligibility (allow-listed hosts);
   compare criteria; answer with verdict, criteria table (rule+web citation · company
   value+app · met?), deadlines from cited pages, assessment-not-legal-advice note. Company
   figures `app`/`computed`; rules/deadlines `web` and present in cited text; search query
   names the incentive, not confidential company values.

## Consequences

- Integrators enable analyst abilities explicitly; defaults remain off; weak models cannot
  silently gain the loop. Levels: deployment config, package `assistant`, tenant ops
  (`/v1/ops/tenants/{id}/analyst`); effective view `GET /v1/me/analyst`.
- Clients announce parse support via `client_capabilities`; a 1.4 client that announces
  nothing never receives a 1.5-only member (mapped before emit).
- Schemas under `schemas/analyst/`, additive rich-block / turn-event / egress / manifest /
  provider-profile / OpenAPI fields, and docs `docs/integration/analyst-turns.md`,
  `docs/integration/domain-scope.md`, `docs/operations/analyst.md`. Freeze at release prep
  only — not mid-design.
- Implementation is split across later tasks with disjoint ownership (see
  `docs/integration/analyst-turns.md` §10): compute engine; step loop (incl. storing
  `client_capabilities`); domain+web+settings; verifier; **emit mapping** + blocks + kits +
  SDKs. Each ships the acceptance tests below.
- Forbidden in 1.5: sandboxed code execution; native tool-calling as the step transport;
  writes inside the analyst loop; hidden ability enablement by model strength; emitting
  unannounced 1.5 contract members to a 1.4 client.

### Acceptance tests (implementation tasks)

- Invented number → replaced; computed number survives; forecast labelled.
- Cross-tenant read refused; step/record/budget caps hold.
- Injection in a record → no write, no foreign read.
- Old client (empty announcement) → every TurnEvent validates against `schemas/_frozen/v1`;
  new blocks/marks/stages/members mapped or omitted; out-of-domain refused; in-domain / no
  capability answered.
- Web query with confidential value refused; web number not in source replaced; knowledge
  figure posing as tenant data replaced; injection in a web page → no out-of-domain answer,
  no write.
- Each ability off at each level removes it for a strong model.

## Reversal plan

Set all `dialogue.analyst` abilities false (defaults). Clients that ignore new block types
already get fallbacks when support is not announced. New schemas remain additive; turning
abilities off restores the 1.4 single-capability path with no data migration.
