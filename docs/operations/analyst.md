# Operating analyst turns

Design: [ADR 0047](../decisions/0047-analyst-turns.md). Integrator view:
[analyst-turns.md](../integration/analyst-turns.md). Schema:
`schemas/analyst/config.schema.json`.

## 1. Defaults

Everything is **off**. With `dialogue.analyst.enabled` false (default) the core keeps the 1.4
single-capability path. Each ability is a separate switch; model strength only decides what
*can* be enabled (`analyst_capable` on the profile), never what *is* enabled.

## 2. Config keys

Loaded by the dialogue config section (`dialogue.Loaded().Analyst`) and wired into the
serve composition root (`internal/cli` analyst provider). Caps and ability flags are
validated against `schemas/analyst/config.schema.json`. Unknown `web.provider` values fail
closed at startup when analyst is enabled.

Under `dialogue.analyst` (env prefix `ALIXAI_DIALOGUE_ANALYST_*` where the config loader maps
nested keys):

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled` | false | Master opt-in for the analyst path |
| `abilities.loop` | false | Multi-step read loop |
| `abilities.compute` | false | Compute engine |
| `abilities.forecasts` | false | Forecast ops |
| `abilities.presentation` | false | New chart marks / kpi / timeline / record_card / provenance |
| `abilities.drafts` | false | `draft` blocks |
| `abilities.knowledge` | false | General knowledge in-domain |
| `abilities.web_search` | false | `search` steps |
| `abilities.web_fetch` | false | `fetch` steps |
| `max_steps` | 12 | Step proposals per turn |
| `max_reads` | 8 | Read steps |
| `max_records_per_dataset` | 500 | Rows per dataset |
| `max_total_records` | 2000 | Rows across datasets |
| `max_turn_tokens` | 32000 | Token budget across model calls |
| `max_turn_wall_ms` | 60000 | Wall clock |
| `max_turn_cost` | null | Optional micro-unit ceiling |
| `web.provider` | `""` | `brave` \| `tavily` \| `searxng` \| `provider_native` |
| `web.cred_ref` | — | Opaque credentials-service ref (never a secret in YAML) |
| `web.origin` | — | SearXNG https origin when provider is `searxng` |
| `web.max_searches` / `max_fetches` | 3 | Caps |
| `web.allow_hosts` / `deny_hosts` | [] | Deployment host policy (intersected with tenant allow/deny and package `preferred_web_hosts` when set) |

Application package `assistant.abilities` and tenant settings
(`PUT /v1/ops/tenants/{tenant_id}/analyst`) may only narrow. Most restrictive wins.
Effective set: `GET /v1/me/analyst`. Tenant host allow/deny intersect deployment lists; deny wins.

## 3. Costs

Each model call in the loop reserves against the usual provider budget; settle at end or on
cancel. `max_turn_cost` is an additional turn ceiling. Search/fetch count against their caps
and any provider metering the adapter defines. Exhaustion mid-loop: emit a verified answer
from computed-so-far when possible; otherwise typed `resource_exhausted` /
`analyst_cap_exceeded`.

## 4. Profiles

Set `analyst_capable: true` only on profiles you trust for multi-step constrained JSON and
careful citation. Lite / weak controllers should stay false.

## 5. Egress

Web search (`abilities.web_search` / `web_fetch`) requires **External mode** and an egress
policy chain that permits category `web_query` (the query’s data class must be allowed on
every level). Local-only deployments get no web search: the serve composition fails closed
(`analystWebQueryAllows` / “web search requires External mode” under `-tags noexternal`).
The serve gate probes with egress `Disclose` (no audit send record) before any real
`web_query`; restricted dataset fields still refuse at search time via `QueryLeaksRestricted`.

Example (SearXNG self-host; `cred_ref` optional for searxng, required for brave/tavily;
`allow_hosts` required whenever `web_fetch` is on):

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
    web:
      provider: searxng
      origin: "https://searx.example.internal"
      cred_ref: "cred_01ARZ3NDEKTSV4RRFFQ69G5FAV"
      max_searches: 3
      max_fetches: 3
      allow_hosts: [gov.it]
```

Operators then enable External mode and PUT egress policies that allow `internal` (or
narrower) for the tenant that may search. See [external-mode.md](external-mode.md) for
category `web_query` and dataset field-class propagation. Restricted fields stay local for
compute; permitted aggregates may leave at `directive_class`.
