# 0021 — First external provider adapter

- **Status:** provisional
- **Date:** 2026-10-03
- **Decided in task:** X-01 (default chosen by the project owner); finalised by X-16 (conformance against recorded fixtures) and X-H02 (live)

## Context

Additive phase B says: "Begin with one provider whose API, structured output, streaming and terms are clear. Add
other providers only after the normalized contract works." The first adapter fixes which structured-output
guarantee the control path (typed `TaskProposal`) can rely on, which credential flow ships first, and which
terms the first operators accept. The roadmap order after it is already in the task graph: Anthropic (X-19),
Gemini (X-20), Meta or a hosted Llama endpoint (X-21).

### Facts, with sources (fetched 2026-10-03)

All rows were fetched on 2026-10-03 and summarised by a fetch tool. **AlixAI has not verified them**; the
adapter tasks re-fetch and date what they use, and X-H01 and X-H03 review the terms. Provider documents move
(all four first URLs tried redirected to a new host), so each URL below is the one that answered.

| Topic | OpenAI | Anthropic | Google Gemini | Meta Llama API |
| --- | --- | --- | --- | --- |
| Strict structured output | `json_schema` with `strict: true` on Responses and Chat Completions; needs `additionalProperties: false` and every field in `required`; refusals come in a separate `refusal` field | GA: `output_config.format` JSON outputs and `strict: true` tool use; no recursive schemas, no numeric or string length constraints, `additionalProperties` only `false`, 20 strict tools per request; a refusal may not match the schema | not researched here (X-20) | not applicable (see below) |
| Tool or function calls | model proposes, application executes; strict mode; parallel calls; guidance to keep fewer than 20 functions | strict tool use as above | not researched here (X-20) | not applicable |
| Auth for API access | API keys. The official auth page returned 404 on fetch; a third-party search summary says the platform API has no OAuth authorization endpoint that bills a user's account, and "Sign in with ChatGPT" is identity, not API access (low confidence) | three methods listed on the official page: API keys (personal, service account, legacy workspace), Workload Identity Federation, App Attest for iOS and macOS apps; no per-user OAuth authorization-code flow is listed | API key is the default; OAuth is documented with scopes `https://www.googleapis.com/auth/cloud-platform` and `https://www.googleapis.com/auth/generative-language.retriever`, described by the page as a simplified approach "appropriate for a testing environment", with an unverified-app warning during setup | not applicable |
| Default retention and training | abuse-monitoring logs up to 30 days by default; API data not used for training unless the customer opts in (statement dated March 1, 2023); zero data retention for most endpoints on eligible organisations | conversation content "not retained by default" except designated "Covered Models", which require 30-day retention and are not ZDR-eligible; the standard commercial retention page was not fetched | unpaid services: content may be used to improve products and reviewed by humans; paid services: not used to improve products; abuse-monitoring log retention quoted as 30 days for two grounding features only | not applicable |
| Stateful features to avoid | Responses `store: true` retains data for at least 30 days; Files retained until deleted unless `expires_after`; Assistants, threads, vector stores, Conversations are not ZDR-eligible; generated images 30 days of abuse monitoring; audio outputs stored 1 hour | code-execution containers retained up to 30 days | not researched here (X-20) | not applicable |
| Regions | per-project data residency with 10 named regions (see ADR-0026) | `inference_geo` `global` (default) or `us` (see ADR-0026) | data "may be stored transiently or cached in any country in which Google or its agents maintain facilities"; users in the EEA, Switzerland and the UK may use paid services only | not applicable |
| Availability | live | live | live | The Llama API public preview is **winding down on 2026-07-06** according to Meta's "Upcoming changes to Llama API" page as quoted by a search tool (the page itself sat behind a sign-in when fetched); Meta points developers to third-party hosts |

Sources fetched 2026-10-03: `developers.openai.com/api/docs/guides/your-data`, `.../structured-outputs`,
`.../function-calling`, `.../migrate-to-responses`; `platform.claude.com/docs/en/manage-claude/authentication`,
`.../api-and-data-retention`, `.../build-with-claude/structured-outputs`; `ai.google.dev/gemini-api/docs/oauth`,
`.../terms`; search results for the OpenAI OAuth and Meta Llama API status questions.

Billing boundary: a ChatGPT or Claude consumer subscription is a different
product from API billing; Gemini API access is tied to a Cloud project; consumer Meta AI access is not Llama API
access. Each profile carries its own billing notice and setup cannot complete without the acknowledgement (X-05).

## Options

| Option | Security | Resource and cost | Verdict |
| --- | --- | --- | --- |
| OpenAI | strict structured output is a hard guarantee for `TaskProposal`; API-key auth only | the largest hosted model range, per-token pricing | **default** (owner's choice) |
| Anthropic | strict structured output and strict tool use also GA; stricter schema subset | second adapter, X-19 | follows |
| Gemini | the only one of the three that documents OAuth for API access; terms differ sharply between unpaid and paid use | third adapter, X-20 | follows; first real OAuth exercise |
| Meta Llama API | the hosted API is being retired | no adapter to build as of the research date | superseded by "any hosted Llama endpoint" (X-21) |
| A generic OpenAI-compatible hosted-endpoint adapter first | one adapter that also serves several Llama hosts | weaker guarantees: strict structured output varies by host | not first; it is the X-21 fallback |

LocalAI v4.10.0 lacks strict structured output (task X-10 brief), which is why a hosted provider with a hard
schema guarantee is the first external adapter and why this ADR does not pick a weaker generic endpoint first.

## Decision

**OpenAI is the first external provider; its adapter is `adapters/external/openai` (X-10).** Anthropic, Gemini and
Meta or a hosted Llama endpoint follow in X-19, X-20 and X-21. The adapter implements the extended
`AIRuntimeProvider` contract (X-06), is DISABLED by default (ADR-0022) and is one interchangeable backend: it
never executes a tool, never owns scope or commits.

Consequences the adapter task must carry (flagged for X-10):

1. **The first release authenticates with a protected API key.** OpenAI documents no OAuth flow for API access,
   so the manual-key fallback (X-07) is what ships first. This is allowed by the acceptance criterion
   "OAuth whenever a provider supports a suitable flow" ([release criteria](../release-gates.md), X.12). The OAuth engine (X-08) is built and proven against a
   mock identity provider; the first real provider OAuth consent is Gemini (X-20), live in X-H02. Do not describe
   an OpenAI connection as OAuth.
2. **Send `store: false` and none of the stateful features.** Responses with `store: true` retain data for at
   least 30 days. The adapter sets `store: false`, and never uses Conversations, vector stores, file search,
   built-in tools or remote MCP (test `TestProviderNativeAgentFeaturesNotRequested`). Whether to use
   Responses or Chat Completions is X-10's call; as of 2026-10-03 OpenAI recommends Responses for new projects,
   keeps Chat Completions supported and not deprecated, and both support strict structured output.
3. **Strict mode needs a schema subset.** The `TaskProposal` grammar in `schemas/` uses patterns and bounds; X-10
   must show the grammar is accepted by strict mode and record every keyword it has to strip, without weakening
   the local validation (the proposal is validated again locally either way).
4. **The profile records the provider's stated terms.** `terms.retention` is `limited` with `retention_days`
   unless the operator has an approved zero-retention arrangement; `terms.training_use` and region follow the
   operator's account (ADR-0026, ADR-0028). The operator, not AlixAI, creates the account and the spend caps
   (X-H01).

## Consequences

Easier: one provider with a hard schema guarantee proves the control path end to end. Harder: OAuth-first is not
demonstrated by the first provider; the first operators handle an API key. Forbidden: scraping or reusing a
consumer session, and any provider-native agent or hosted-memory feature on a consequential path.

## What confirms or overturns this (owed)

- X-16: the OpenAI adapter passes the 30-check conformance suite against recorded fixtures (the report is the
  `conformance-report` schema).
- X-H02: live runs with real model ids, including the cheapest hosted tier that passes the routing corpus, with
  measured latency and cost. X-10 records prices as of its research date and states that live measurement is owed;
  no number in this ADR is a measurement.
- If strict mode cannot carry `TaskProposal` without losing a constraint the control path needs, the order
  changes: Anthropic (X-19) becomes the reference adapter.

## Reversal plan

Everything provider-specific is behind the provider contract and `adapters/external/openai`. Retire the profile
(`providerreg.Retire`), remove the package or build with `-tags noexternal` (ADR-0022). No application data,
contract or schema depends on the provider; only provider profiles and usage records name it.
