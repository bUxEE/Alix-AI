# 0043 — External TTS and per-role provider choice

- **Status:** accepted
- **Date:** 2026-10-26
- **Decided in:** release 1.3.0

## Context

Operators want hosted speech synthesis (OpenAI, ElevenLabs, Google Cloud TTS, Amazon Polly) under the same
external-mode gates as chat and STT, and want **each role** — conversational controller, language realisation,
STT, TTS — to resolve to its own provider when desired. One credential per (principal or tenant, provider) is
shared by every role that uses that provider; roles never share another principal's credential and never
silently switch provider. ADR 0041 left TTS reserved; ADR 0024 / `internal/extmodal` already require attested
verified reply text before any external synthesis. Contracts stay additive on the frozen 1.2 set.

## Options

### External TTS availability

- **A. Keep the 1.1 roadmap refusal.** Rejected: the owner requires external TTS.
- **B. Four adapters behind `voice.TTS`, modality/egress/budget gated, local fallback only when the role policy
  names it.** Taken. Adapters live under `adapters/external/tts/{openai,elevenlabs,googletts,polly}`; the
  composition root registers them the same way as STT.

### What may be spoken

- **A. Any model text.** Forbidden by ADR 0024.
- **B. Only attested verified reply text (or the deterministic degraded template), session-scoped, with
  `user_message` / internal class at the speech purpose.** Taken (existing `extmodal.Attest` / `NewTTS`).

### Metering

- **A. Continue estimating TTS as `toks_in` only.** Incomplete for character-priced speech APIs.
- **B. Additive meter unit `characters_in` on the price table, budgets and usage; reserve characters of the
  spoken text plus `requests` before the call; refuse profiles that price only `audio_seconds_out` (length
  unknown before the call).** Taken.
- **Pricing rule (explicit):** prefer `characters_in` when the profile prices it. Else, if the profile
  prices `toks_in`, estimate with that (legacy compatibility). Else refuse as not estimable — the same
  fail-closed rule as STT without `audio_seconds_in`. Never invent a silent tok estimate when neither unit
  is priced.

### Per-role provider choice

- **A. One provider for all roles.** Rejected by the owner.
- **B. Each role resolves independently: principal binding → deployment `external_providers.routing.<role>` →
  local (when a pack / model provides it) → typed refusal. Same provider or different providers; one credential
  per (principal or tenant, provider).** Taken. TTS joins the principal-bindable roles; optional `voice` and
  `language` on the binding update apply to TTS (and language to STT when useful).

### Audio class and playback

PCM s16le mono at the provider's native rate (OpenAI 24 kHz; ElevenLabs / Polly 16 or 24 kHz; Google configurable)
is returned with that rate on `voice.AudioFormat`; the voice session already emits `PlaybackStarted` from the
stream format, so no kit change is required.

## Decision

Option B for availability, spoken text, metering and per-role resolution. Principal bindings and the catalog
accept role `tts`. Deployment routing of `tts` is accepted once an adapter has registered
(`RegisterExternalTTSAdapter`). Layer status (`config check` / `providers layers`) prints
`tts: external (profile …)` when routed.

## Consequences

Hosts can bind chat and TTS to different providers for the same user, or the same provider with one key.
Cross-principal credential use remains refused. External TTS stays off until the operator enables external mode,
admits the TTS modality, routes or binds a profile, and has an active credential. Local packs remain the default
when nothing is routed.

## Threat model (notes)

| Threat | Mitigation |
| --- | --- |
| Unverified model text spoken externally | Attest + Verified gate; pieces must be substrings of attested text |
| Cross-principal / cross-tenant key use | Resolve and ActivePrincipalRef owner checks; self-only `/v1/me` |
| Silent provider switch on denial | Same selection loop as 1.1: typed error, no soft-skip |
| Budget surprise | Reserve before call; character (or legacy toks) estimate; no call if unpriced / exhausted |
| Secret leakage | Credentials only in the credentials service; adapters never put keys in errors or logs |

## Reversal plan

Stop calling `RegisterExternalTTSAdapter`; routing `tts` is refused again. Clear principal `tts` bindings;
deployment routes empty. Adapters remain in the binary behind `noexternal` exclusion. No frozen schema to restore
beyond freezing the additive fields already recorded.
