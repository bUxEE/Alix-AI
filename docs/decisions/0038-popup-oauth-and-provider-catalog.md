# 0038 — Popup OAuth and the provider catalog (SIWC, Google, OpenRouter)

- **Status:** accepted
- **Date:** 2026-10-26
- **Decided in:** release 1.1.0

## Context

Host applications connect providers from a settings UI without navigating the host page away.
Some providers officially offer third-party browser OAuth; others do not. AlixAI must never
impersonate another product's OAuth client. OpenAI launched Sign in with ChatGPT (SIWC) for
open-source / locally hosted apps (2026-09-29): loopback-only, dynamic client registration,
ChatGPT plan usage of the signed-in person. Google OAuth needs an operator-registered client.
OpenRouter PKCE issues an API key. Anthropic, xAI and Moonshot have no official third-party
browser OAuth.

## Options

- **A. Redirect-only OAuth (1.0).** Host navigates to the IdP. Rejected for settings UIs that must
  keep the opener page.
- **B. Popup + postMessage + server-side flow status.** Host opens a popup; AlixAI completes the
  exchange (host relay or ephemeral 127.0.0.1 listener); the callback page posts
  `{type, flow_id, status}` to an exact `opener_origin` and the host polls `GET /v1/oauth/flows/{id}`
  as source of truth (COOP often nulls `window.opener`). Taken.
- **C. Embed another product's client id for Anthropic / xAI / Kimi.** Forbidden: terms, suspension
  risk, and the project's "never impersonate" rule.

## Decision

Option B for every provider that officially offers third-party OAuth; manual API key (settings /
`ALIXAI_SECRET_*` env / secret file / `alixai providers credential add-ref`) otherwise.

- Catalog: `external_providers.oauth.providers.<id>` with built-in presets for `openai` (SIWC),
  `gemini` / `google-stt` (Google), `openrouter`. Off unless enabled. Unknown keys refused.
  `popup_origins` allowlist (exact origins, never `*`).
- SIWC: loopback only; bind connection to the connecting principal; require
  `chatgpt.tokens.use.direct` before inference; always `store:false`. Remote deployments stay on
  the manual key with a clear reason.
- OpenRouter: store the issued key as `oauth_issued_key`; bind the flow by loopback port / path
  (no `state`).
- Anthropic / xAI / Moonshot: key-only; models remain reachable through OpenRouter when that is
  configured. A future official program is config, not code.
- Chat Completions providers openrouter / xai / moonshot share `adapters/external/chatcompat`;
  slash-bearing wire ids map through `openrouter_models` / `xai_models` / `moonshot_models` like
  `meta_models`. The frozen `modelId` pattern is not relaxed.
- CLI: `alixai providers login <provider>` runs the same engine with loopback; no browser login for
  key-only providers (exit 2).

## Consequences

Hosts get a popup connect path without navigating away. Operators must register Google clients and
list popup origins. SIWC is same-machine only. Credential kind `oauth_issued_key` appears in status
and schemas (1.1.0 additive). New adapters stay behind `external_providers.enabled` and the
`noexternal` build tag.

## Reversal plan

Disable `external_providers.oauth.providers.*` and `popup_origins`; hosts fall back to redirect or
manual key. Retire chatcompat packages behind `noexternal`. Credential rows of kind
`oauth_issued_key` remain readable as API keys until revoked.
