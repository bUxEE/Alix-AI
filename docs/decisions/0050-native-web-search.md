# 0050. Model-native web search and the `web_query` egress setting

## Status

Accepted (1.5.1).

## Context

1.5.0 shipped analyst web search (Brave / Tavily / SearXNG) and a `web_query` send
category, but gated search with a Disclose “internal class” heuristic while docs claimed a
per-category egress switch. Operators also want the routed model’s own search tool as a
`search` step, with AlixAI still fetching cited pages to verify.

## Decision

1. **Egress setting `web_query`.** Optional on `egress-policy` at deployment / application /
   tenant / workspace (not source): `{ enabled, allowed_providers }` where providers are
   `provider_native` | `searxng` | `brave` | `tavily`. Absent or disabled = no search.
   Effective = every level enables it; `allowed_providers` is the intersection. Enabling it
   does not need confidential-class approval. `dataegress.WebQuery` is the only serve gate.

2. **`provider_native` search.** A `search` step may call the routed controller in a
   **separate, minimal** request: fixed instruction + vetted query only (no datasets, no
   conversation, no turn system prompt), with the provider’s official search tool enabled.
   Hits are parsed into `SearchHit`; hosts outside the merged allowlist are dropped; AlixAI
   fetches allowed URLs itself.

3. **Catalog flag `native_web_search`.** Optional on provider profiles (default false) and
   copied onto catalog entries. Private/self-hosted endpoints stay false.

4. **Meter `web_searches`.** Provider-reported search tool calls; priced when the price table
   has a per-call price, else count-only. Budget reserve treats a native search like any
   provider call.

5. **Fallback.** `web.fallback`: `""` | `searxng` | `brave` | `tavily`. Order: native when
   profile + egress allow → else fallback when configured and allowed → else
   `web_search_unavailable`.

## Provider verification (API shapes shipped)

| Provider | Verified | Doc | Wire |
| --- | --- | --- | --- |
| OpenAI | yes | https://platform.openai.com/docs/guides/tools-web-search | Responses API `tools:[{type:"web_search", filters:{allowed_domains}}]`; citations via `url_citation` annotations; `web_search_call` counted |
| Anthropic | yes | https://docs.anthropic.com/en/docs/agents-and-tools/tool-use/web-search-tool | Messages API `tools:[{type:"web_search_20250305", name:"web_search", max_uses, allowed_domains}]` |
| Google Gemini | docs verified; **not offered** | https://ai.google.dev/gemini-api/docs/google-search | Grounding chunks use `vertexaisearch.cloud.google.com/…` redirector URIs (host allowlist drops every hit, or AlixAI would fetch through Google's redirector). Grounding with Google Search terms also require displaying Search Suggestions (`searchEntryPoint`) to the end user, which an embedded analyst search step does not do. `native_web_search` stays false; adapter refuses SearchWeb. |
| xAI | docs verified; client unsupported | https://docs.x.ai/developers/tools/web-search | Official tool is on Responses API; AlixAI’s xAI adapter is Chat Completions only → `native_web_search` stays false until Responses is allowlisted |
| Mistral | unsupported | https://docs.mistral.ai/agents/connectors/websearch | `web_search` is Conversations/Agents only; Chat Completions (what we use) has no result references → false |

Native search uses the same controller route selection as the turn (`RoleController` / principal bindings / private pins), runs `dataegress.Evaluate` (one `web_query` part, class internal) so provider/model/region allowlists, kill switch and audit apply, and reserves/settles the provider budget fail-closed (no soft-ignore).

## Consequences

- Additive schemas only; frozen 1.0 contracts unchanged.
- Existing Disclose-based web gate removed from analyst wiring.
- `web.origin` accepts `http://` and `https://` (operator-configured endpoint).
