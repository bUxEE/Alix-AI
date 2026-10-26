# Roadmap (after 1.0.0)

Short list of planned work. Nothing here is a commitment of date or version; items land only when measured
and reviewed. See [Project status](../README.md#project-status) and
[docs/verification/README.md](verification/README.md) for what 1.0 left undone.

## Local and external layers (LLM, STT, TTS)

- **Balanced/Performance local STT defaults** after voicebench on real LocalAI
  ([ADR 0040](decisions/0040-parakeet-local-stt.md)): candidates today are `parakeet-stt` and
  `whisper-turbo-stt` (Balanced), plus `whisper-large-v3-stt` (Performance). Lite keeps Whisper tiny (`en-stt`).
  All run on CPU; GPU is optional.

## Shipped in 1.3.0

- **External TTS and per-role provider choice** ([ADR 0043](decisions/0043-external-tts-and-per-role-provider-choice.md)):
  OpenAI, ElevenLabs, Google Cloud TTS, Amazon Polly and Mistral behind `routing.tts` / principal `tts` bindings;
  attested verified text only; `characters_in` metering; each of controller, realisation, STT and TTS may use
  the same provider or different ones with one credential per (principal or tenant, provider).
- **Opt-in external realisation of live facts** ([ADR 0044](decisions/0044-external-directive-class-and-result-blocks.md)):
  `external_providers.directive_class`; verified table/chart blocks; conversation-contract `presentation.chart`.
- **Per-user OAuth callback/flows** and **ops erasure** (tenant vs application scope) over HTTP.
- **Outbound `credential_ref`** for OpenAPI, MCP and RAG api (`application_credentials`); optional RAG
  `scope_params.include_principal_id`.
- **Per-user usage and principal budgets** (`GET /v1/me/usage`, `GET /v1/me/budget`, `principal_application`
  subject kind; operator usage export `principal_id` filter).
- **Mistral (EU) chat + STT + TTS**: `adapters/external/mistral`, `adapters/external/stt/mistral` (Voxtral
  transcription) and `adapters/external/tts/mistral` (Voxtral TTS). Default data residency EU; register chat with
  `-template mistral`.
- **UI kit fullscreen z-index**: configurable `--alix-fullscreen-z` / `fullscreenZIndex` (default 2400); npm
  publish readiness for `@alixai/*`.
- **Browser-URL hardening** for CLI OAuth; **light External hardware** guidance.

## Provider auth

- **OAuth for Anthropic / xAI / Kimi / Mistral** if and when those vendors offer third-party OAuth for API access. Today:
  API key, or their models through OpenRouter's OAuth where available. Never impersonate another product's OAuth client.

## Shipped in 1.2.0

- **Per-principal LLM accounts** ([ADR 0041](decisions/0041-per-principal-llm-accounts.md)): operator-approved catalog;
  each user binds provider/model and their own credential; companies keep egress policy (catalog CLI, `/v1/me/…`,
  reserved-tenant credentials, router resolution).
- **Long-term memory** ([ADR 0042](decisions/0042-long-term-memory.md)): per principal × tenant × workspace, consent-gated
  proposals, fenced into context (`memory.enabled`, default off); see [integration/memory.md](integration/memory.md).
- **Downloadable exports**: `POST /v1/sessions/{session_id}/exports` builds CSV, XLSX, HTML or Markdown from a turn's
  already verified rich blocks (`exports.create`); durable turn-block store; SDK clients and demo-mock route.
- **UI kit fullscreen and downloads**: Vue/React chat panel fullscreen overlay; opt-in export menu on tables, charts and
  answers (host exporter; kits never call AlixAI).

## Verification still owed (from the 1.0 "not done" list)

- **Real hardware certification** of the Lite (and later Balanced) profile on a real 8 GB x86-64 machine;
  ARM64 has never been run.
- **24-hour and 7-day soaks** (analysis only so far).
- **Independent external security review** (maintainer self-review is done; not independent).
