# Long-term memory (ADR 0042)

Per principal × tenant × workspace. Consent-gated. No silent writes.

## Enable

```yaml
memory:
  enabled: true                 # default false
  max_items_per_scope: 200
  max_retrieve: 8
  max_proposals_per_turn: 1
  max_proposals_per_day: 20
  proposal_ttl: 168h            # confirm after this → memory_proposal_expired
```

With `enabled: false` (default), every `/v1/memory` route answers `unavailable`.

## API

Scope is always the verified context token. Capability ids (exact match):

| Capability | Operations |
| --- | --- |
| `memory.read` | `GET /v1/memory` |
| `memory.write` | `POST /v1/memory`, `PATCH /v1/memory/{id}` |
| `memory.delete` | `DELETE /v1/memory/{id}` |
| `memory.confirm` | `POST …/confirm`, `POST …/reject` |
| `memory.export` | included when history export is authorised and memory is wired |

- **Host create** (`POST /v1/memory`): origin `user_explicit`, status `active`. Use only when the user asked to remember something.
- **List / search**: `GET /v1/memory?q=&status=&cursor=&limit=`.
- **Patch / delete**: token scope only; foreign id → not found (delete is idempotent).
- **Confirm / reject**: structured calls only. Typed chat text ("yes", "ok") never activates a proposal. An expired or non-proposed item is `validation_failed` with `details.reason` = `memory_proposal_expired`.

## Consent flow (assistant proposals)

1. When memory is enabled, the model may emit an optional structured `memory_proposal` in its task output (schema-validated; never free text).
2. The engine stores it as status `proposed` and emits TurnEvent `memory_proposal`.
3. AG-UI maps that to CUSTOM event `alixai.memory_proposal` (payload = the memory item).
4. The host UI calls `POST /v1/memory/{id}/confirm` or `/reject`. Nothing else activates the item.

## Context and privacy

Active items of the exact scope are retrieved into the proposal prompt (recency + lexical match, no embeddings). Each item is fenced as `<memory_data>` untrusted data, counted in the context budget, and carries its `data_class` (default `internal`) for the egress guard. When the controller is external and the tenant policy does not allow the class, the item is not sent (logged without content). Memory items never become instructions.

Retention and `alixai retention erase` remove memory with history (ADR 0036). History export appends memory rows (`{"kind":"memory","item":…}`) when memory is enabled.

## Related

- [ADR 0042](../decisions/0042-long-term-memory.md)
- [AG-UI mapping](agui.md)
- [Light integration](guides/light-integration.md)
