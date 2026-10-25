# 0024 — Provider-side audio, image and file processing in the first external release

- **Status:** provisional
- **Date:** 2026-10-03
- **Decided in task:** X-01; finalised by X-15

## Context

External providers can lower local hardware needs for voice, vision, image generation and TTS, but audio, images
and files carry more than text does: a recording or a scanned document holds data no field-level redaction sees,
and providers keep media under different rules. As fetched on 2026-10-03 (summarised by a fetch tool, not
verified by AlixAI): OpenAI keeps files until deleted unless `expires_after` is set, generated images get 30 days
of abuse monitoring, audio outputs are stored for 1 hour for multi-turn use; Anthropic's Files API accepts
requests up to 500 MB and its code-execution containers retain data up to 30 days; Gemini terms differ between
unpaid and paid use (ADR-0021 has the table). External mode requires every modality to be optional and
provider-capability-gated, and §18 item 4 asks whether the first release enables them.

What already exists: the request envelope has `image`, `file` and `audio` part types with the categories
`image_content`, `file_content` and `audio_content`; the profile records `modalities`, `media.*` (with
`retention`), `voice` (with `audio_retention`) and `image_generation`; and `providerreg.Check` refuses a request
that needs a modality the profile does not declare.

## Options

| Option | Security | Resource and cost | Verdict |
| --- | --- | --- | --- |
| A. Enable every declared modality as soon as an adapter supports it | media leaves on a capability flag alone | cheapest to build | rejected |
| B. Text and structured output only in the first release; each modality OFF until X-15 enables it behind explicit policy | media egress is a separate, recorded decision per modality | media features arrive later | **default** |
| C. Never provider-side media | strongest privacy | gives up the main hardware saving of External mode (voice, vision) | not chosen; B keeps it reachable |

## Decision

**Provider-side audio, image and file processing is OFF in the first external release; text and structured
output only.** Concretely:

1. The first profiles (X-10 OpenAI) declare text input and output only. A profile that declares a media
   modality is not enough by itself.
2. X-15 (`internal/extmodal`) adds a per-modality enable that defaults to off and is turned on only by explicit
   provider and tenant policy (`TestModalityOffByDefault`, planned). The shape of that switch is X-15's to design;
   this ADR fixes the default and the preconditions.
3. Preconditions to enable a modality: the profile declares it and its `media` handling; the egress policy
   allows the data class; the retention and recording behaviour of that modality is disclosed to the operator at
   enablement and in the egress disclosure; and the modality passed its conformance checks (they are the only
   checks that may be `not_applicable`, and only when the profile does not declare the modality).
4. Data rules that hold once enabled: raw audio only under explicit provider and tenant policy,
   a finalised local transcript preferred; external TTS receives only verified response text; images and files
   go as scoped, expiring references and only approved files or transformed excerpts, application-private files
   need explicit egress permission; generated images and files are stored locally first and handed to the host as
   AlixAI-managed references; the provider can never write into application storage; provider Markdown, HTML,
   images and charts are untrusted and become validated response blocks.

## Consequences

Easier: the first release has a text-only egress surface that the conformance suite and the security suite can
cover completely. Harder: voice and vision under External mode wait for X-15, and the local 4 GB profile keeps its
local STT and TTS in the meantime. Forbidden: enabling a modality because an adapter happens to support it.

## What confirms or overturns this (owed)

- X-15 records, per modality, bytes sent, latency and price against the local alternative in
  `docs/efficiency/X-15.md`, and the retention text each provider states, dated. Live calls are X-H02's.
- If a modality's retention cannot be disclosed accurately for a provider, that modality stays off for that
  provider.

## Reversal plan

The default is a policy default plus the profile's declared modalities: switching a modality on or off is a
policy and profile change, with no schema or data migration. Removing `internal/extmodal` leaves text-only
External mode intact.
