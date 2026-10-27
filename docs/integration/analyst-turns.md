# Analyst turns

Design: [ADR 0047](../decisions/0047-analyst-turns.md). Operations: [analyst.md](../operations/analyst.md).
Domain and source labels: [domain-scope.md](domain-scope.md). Where this page and the schemas disagree, the schemas win.

## 1. What it is

An **analyst turn** is an opt-in dialogue path where the model may propose several **read-only**
steps in one user turn: read host data, compute, optionally search/fetch the web, then answer
with verified text and rich blocks. The model plans, reasons and explains; the **core** reads
and does the arithmetic. Local mode and the single-capability path are unchanged when analyst
abilities are off (the default).

## 2. Enabling

1. Mark the controller profile `analyst_capable: true` (catalog / private route).
2. Set deployment config (all abilities default false):

```yaml
dialogue:
  analyst:
    enabled: true
    abilities:
      loop: true
      compute: true
      forecasts: true
      presentation: true
      drafts: true
      knowledge: true
      web_search: true
      web_fetch: true
    max_steps: 12
    max_reads: 8
    max_records_per_dataset: 500
    max_total_records: 2000
    max_turn_tokens: 32000
    max_turn_wall_ms: 60000
    web:
      provider: brave          # or tavily, searxng, provider_native
      cred_ref: cred_brave_1   # opaque credentials-service ref (never a secret in YAML)
      max_searches: 3
      max_fetches: 3
      allow_hosts: [gov.it, europa.eu]
```

`alixai serve` wires this through `internal/cli` (composition root): compute engine, domain
classifier, web search/fetch, settings ability resolver, verify streaming, and the stream hub
progress stages. With `enabled: false` (default) the runner is not attached and Local / 1.4
behaviour is unchanged.

3. Declare `assistant.domain` (and optional ability ceilings) in the application package.
4. Tenant settings may only **narrow** abilities further.
5. Read the effective set: `GET /v1/me/analyst` (or optional `analyst` on
   `GET /v1/agui/capabilities`). Tenant ceilings:
   `GET|PUT /v1/ops/tenants/{tenant_id}/analyst` (`ops.analyst.read|write`; tenant may only narrow).

A disabled ability is removed from the step grammar and the prompt. A step that uses it anyway
is refused with `ability_disabled`. A non-`analyst_capable` model cannot enable analyst
abilities even when switches are on (`model_not_analyst_capable`).

## 2b. Client announcement (1.4-safe emit)

Send `client_capabilities` on each turn (sessions API) or `forwardedProps.alixai.clientCapabilities`
(AG-UI). Schema: `schemas/analyst/client-capabilities.schema.json`. The 1.5 SDKs and kits fill it
automatically. **Absent = 1.4:** the server maps analyst stages, new blocks/marks/layouts and
source labels to the frozen 1.4 form so a 1.4.x client never sees an unknown member. See ADR 0047.

A host that relays AlixAI turn events to its own frontend must announce **only** the
blocks/stages/marks that frontend actually renders. Over-announcing unlocks shapes the host
cannot show; under-announcing is safe (server maps down). The gateway copies the announcement
into the dialogue turn and onto the stream hub before the first publish (including mid-turn
verified segments and AG-UI reconnect replay).

## 3. What the model may do

Constrained-JSON **AnalystStepProposal** (`schemas/analyst/step-proposal.schema.json`), not
native tool-calling:

| kind | Needs | Effect |
| --- | --- | --- |
| `read` | `loop` | Capability Read → dataset |
| `compute` | `compute` (+ `forecasts` for forecast ops) | Deterministic op → dataset / derived facts |
| `search` | `web_search` | Web search (egress `web_query`) |
| `fetch` | `web_fetch` | Fetch allow-listed URL from a search hit |
| `answer` | — | End loop; verify text + materialise blocks |
| `clarify` | — | Clarification hand-off |
| `act` | — | Hand-off to prepare → confirm → commit (**no write in the loop**) |

## 4. How numbers are verified

- Every number and named record/entity in the answer must equal a **read fact** or **derived
  fact** under the formatting rules (plain, grouped space/comma/dot, compact `k`/`m`, percent,
  currency).
- Forecast values must be labelled as estimates and name the method (`linear_trend`, `holt`,
  `seasonal_naive`) with interval.
- Qualitative reasoning without numbers is allowed.
- Source labels: `app`, `computed`, `web`, `knowledge` — see [domain-scope.md](domain-scope.md).
- Failure → `text_replaced` (if streaming) then the analyst template: **table of the final
  dataset** + **provenance** block.

## 5. Forecasts

Ops `forecast_linear`, `forecast_holt`, `forecast_seasonal_naive` in
`schemas/analyst/compute-op.schema.json`. Insufficient series → `insufficient_data` /
`forecast_unavailable`. Ability `forecasts` must be on.

## 6. Blocks

The model chooses a **block plan** that references `dataset_id`s; the core fills values.
Additive chart marks (`scatter`, `heatmap`, `combo_bar_line`), optional `bar_layout` /
`pie_style`, and blocks `kpi`, `timeline`, `record_card`, `provenance`, `draft` are in
`schemas/conversation/rich-block.schema.json`. Emit a new type only if the client announced
support; otherwise table or markdown fallback.

## 7. Egress and budget

Datasets obey tenant egress at `directive_class`. Restricted fields may feed local compute;
only permitted fields/aggregates leave. `web_query` must not carry confidential/restricted
record values. Turn-level `max_turn_cost` / tokens / wall; exhaustion → answer from what is
already computed when verifiable, else typed refusal (`resource_exhausted` or
`analyst_cap_exceeded`).

## 8. Example: eligibility question

User: “Should my company have applied for the Italian 2026 company discount XX?”

1. Domain classifier → `in_domain`.
2. `read` company capability (size, sector, region, revenue, …).
3. `search` query about the incentive (no company confidential values) → `fetch` official page.
4. `compute` / compare criteria.
5. `answer` with verdict, criteria table (rule+web · company value+app · met?), deadlines from
   cited text, assessment-not-legal-advice note; provenance block.

## 9. Example: out of domain

User: “Is a Persian cat big?” → `out_of_domain` refusal; no search, no read, no model essay.

## 10. Implementation (1.5.0)

Shipped in this release (ADR 0047; everything off until the operator enables abilities):

| Area | Location |
| --- | --- |
| Compute engine | `internal/analyst/compute` — fixed op catalog, exact decimals, typed refusals, labelled forecasts |
| Step loop | `internal/analyst/loop` + `internal/dialogue` analyst path; config under `dialogue.analyst`; progress stages `analyst_*`; `client_capabilities` on the turn |
| Domain + web | `internal/analyst/domain`, `internal/analyst/web` (SearXNG-compatible / Brave / Tavily), `web_query` egress; tenant/ops analyst settings |
| Verifier | `internal/verify` analyst path — invented numbers replaced; materialised table/chart/provenance; CoreMaterialised trust for publish |
| Emit + kits + SDKs | 1.5 → 1.4 emit mapping when unannounced; Vue/React/demo-mock blocks; Go/TS/Python SDK announcements |

Cross-cutting: a 1.5 server with a client that announced nothing never emits a member outside the 1.4 freeze.
