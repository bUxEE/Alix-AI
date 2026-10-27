# 0046 — Verified incremental streaming

- **Status:** accepted
- **Date:** 2026-10-27
- **Decided in:** release 1.4.0

## Context

Smoothness of chat matters: today a reply is shown only after the whole model text passes
`internal/verify`, so users wait the full generation (often 3–8 s). The safety rule must stay
exact: nothing unverified is ever shown. CopilotKit / AG-UI compatibility (ADR 0035) must stay
intact. Contract changes after 1.0 are additive only.

## Options

### When to stream

- **A. Always stream when a provider offers `ChatStream`.** Changes 1.3 behaviour by default.
- **B. Opt-in `dialogue.stream_verified` (default off).** Taken. Off keeps 1.3 whole-reply behaviour.
  When on, the engine uses `ChatStream` where the provider supports it (LocalAI, external chatcompat
  paths once they stream, and a future self-hosted kind); otherwise it falls back to `Chat` and
  one verified emit. Operators turn the key on for smoother UX once the provider streams.

### Segment boundaries

- **A. Token / word fragments.** Too fine: verification of facts and numbers is unreliable on
  partial numbers; layout jumps.
- **B. Language-aware sentence / block segments (en, it).** Taken. Abbreviations, decimals, lists,
  fenced code and Markdown tables stay whole; each ready segment is verified before emit. Rich
  table/chart blocks stay whole after verification (unchanged from ADR 0044).

### Failure after some segments were shown

- **A. Append an error and leave the verified prefix.** Leaves a half answer that is no longer the
  authorised reply.
- **B. Typed replace, then the deterministic template.** Taken. TurnEvent `text_replaced` maps to
  AG-UI `CUSTOM` `alixai.text_replaced`; kits clear the superseded assistant text; token events of
  the template follow. Nothing unverified is ever emitted.

## Decision

Option B for all three. Default off. Incremental verifier API in `internal/verify` applies the same
disallow rules as a whole reply (numbers, entities, injection-shaped claims, completion without
receipt, sanitising) to each segment / cumulative prefix; `must_state` completeness is checked only
when the stream finishes successfully. First failing segment stops the stream and replaces shown
text with the template.

## Consequences

Operators who set `dialogue.stream_verified: true` (YAML section `dialogue`, env
`ALIXAI_DIALOGUE_STREAM_VERIFIED`) get first visible text near the first verified sentence while
the safety bar stays fail-closed. Kits and the TypeScript SDK learn the replace CUSTOM event.
Gateway composition must copy `dialogue.Loaded().StreamVerified` into `dialogue.Config`, publish
mid-turn segments from `OnVerifiedSegment` (`stream.PublishVerifiedSegment` /
`PublishTextReplaced`), and skip a duplicate final `PublishResponse` when `verify.Outcome.Streamed`
is set.

## Reversal plan

Set `dialogue.stream_verified` false (or omit it). Clients that ignore `alixai.text_replaced` still
receive the template as later token events; older clients that fail-closed on unknown TurnEvent
types should upgrade before enabling the flag on a core that emits `text_replaced`.
