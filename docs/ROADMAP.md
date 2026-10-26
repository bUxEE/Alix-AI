# Roadmap (after 1.0.0)

Short list of planned work. Nothing here is a commitment of date or version; items land only when measured
and reviewed. See [Project status](../README.md#project-status) and
[docs/verification/README.md](verification/README.md) for what 1.0 left undone.

## Local and external layers (LLM, STT, TTS)

- **External TTS adapters** (Amazon Polly, OpenAI, Google Cloud TTS, ElevenLabs) behind the existing `tts`
  routing role, so each of the three layers (LLM, STT, TTS) can be local or external under the same data-egress
  and budget rules. Off by default; same posture as other external providers.
- **Balanced/Performance local STT defaults** after voicebench on real LocalAI
  ([ADR 0040](decisions/0040-parakeet-local-stt.md)): candidates today are `parakeet-stt` and
  `whisper-turbo-stt` (Balanced), plus `whisper-large-v3-stt` (Performance). Lite keeps Whisper tiny (`en-stt`).
  All run on CPU; GPU is optional.

## Provider auth

- **OAuth for Anthropic / xAI / Kimi** if and when those vendors offer third-party OAuth for API access. Today:
  API key, or their models through OpenRouter's OAuth. Never impersonate another product's OAuth client.

## Shipped in 1.2.0

- **Per-principal LLM accounts** ([ADR 0041](decisions/0041-per-principal-llm-accounts.md)): operator-approved catalog;
  each user binds provider/model and their own credential; companies keep egress policy (catalog CLI, `/v1/me/…`,
  reserved-tenant credentials, router resolution).
- **Long-term memory** ([ADR 0042](decisions/0042-long-term-memory.md)): per principal × tenant × workspace, consent-gated
  proposals, fenced into context (`memory.enabled`, default off); see [integration/memory.md](integration/memory.md).

## Verification still owed (from the 1.0 "not done" list)

- **Real hardware certification** of the Lite (and later Balanced) profile on a real 8 GB x86-64 machine;
  ARM64 has never been run.
- **24-hour and 7-day soaks** (analysis only so far).
- **Independent external security review** (maintainer self-review is done; not independent).
