# 0027 — External natural-language realisation while the local model is the controller

- **Status:** provisional
- **Date:** 2026-10-03
- **Decided in task:** X-01; finalised by X-22

## Context

The open decision: may an external provider write the natural-language answer after the deterministic facts are
produced, while the local model stays the controller? Step 14 of the turn pipeline (overview §5) already splits
the work: the model verbalises, deterministic code verifies, and a failed verification falls back to the
deterministic template for the same directive (`internal/verify`, M1-13). The local small model can be weak at
fluent multilingual prose, which is the one thing a hosted model does clearly better, so the question is
worth asking. What must not change: the facts and the decision are already made locally, and the model owns no
authority.

## Options

| Option | Security | Quality and cost | Verdict |
| --- | --- | --- | --- |
| A. Never: realisation is always local or template | nothing extra leaves | fluency limited by the local model | safe baseline, always available |
| B. Hybrid only, from a verified `DialogueDirective` (and `ActionReceipt`), with the facts-only envelope of ADR-0023, and the final text must pass fact verification | only facts that already passed local verification leave; the output is untrusted and checked before the user sees it | better prose, one external call per realised turn | **default** |
| C. Realisation from anything, including raw user text and excerpts, in any mode | the provider sees unverified material | best fluency | rejected |

## Decision

**External language realisation is allowed in Hybrid mode only (option B).** The rules:

1. **Input.** The input is a `DialogueDirective` or `ActionReceipt` that already passed local verification, built
   into the facts-only envelope of ADR-0023 (categories `dialogue_directive`, `action_receipt`,
   `deterministic_fact`, `structured_state`, and `user_message` where the language or phrasing needs it). No
   `rag_excerpt` is added by this ADR; excerpts follow ADR-0023 per source. The role is
   `language_realization` in the profile's `roles`.
2. **The controller stays local.** The local controller proposes the `TaskProposal`; the external provider never
   proposes a tool call in this path, and any tool-call-shaped output in a realisation response is dropped as
   malformed.
3. **Output is untrusted data.** The realised text goes through the deterministic response verifier (facts,
   claims, source references and citations, `internal/verify`) and provider Markdown, HTML or charts become
   validated blocks. The user sees only text that passed.
4. **Failure falls back to the template**, for the same directive, with no second external attempt and no
   alternate provider. The turn still returns the correct facts. The transition and the verification failure are
   recorded (audit event shape owned by X-22).
5. **External TTS** receives only the verified response text (ADR-0024).
6. **Privacy-sensitive turns stay local.** A turn that involves a `local_only` source, a `restricted` class or a
   tenant whose policy forbids external realisation is realised locally; the policy chain, not the model, decides.
7. **Outage.** If the provider is unavailable, times out or is rate-limited, the turn is realised locally and the
   transition is recorded; this is the policy-named fallback to the local provider, never a hidden one.

How a RAG answer is realised from facts without excerpts when its sources are `local_only` is X-22's design
question. If the facts-only envelope cannot carry it, the turn uses the local model or the template; it never
widens the envelope.

## Consequences

Easier: Hybrid gets better prose without moving any authority, and a verification failure can never produce a
wrong fact. Harder: each realised turn is one more external call (latency, cost, budget) and needs the verifier to
be strict enough that good prose is not rejected. Forbidden: realisation in External mode from unverified
material, using the realised text before verification, retrying elsewhere on a verification failure.

## What confirms or overturns this (owed)

- X-22: `TestExternalRealizationUsesVerifiedDirectiveOnly`, `TestRealizedTextFailingVerificationFallsBackToTemplate`,
  `TestOutageFallsBackToLocalAndIsRecorded`, `TestSensitiveSourceStaysLocalInHybrid` (all planned).
- X-22 or X-23, before the run: write the bar (the fraction of realised turns that may fail verification and the
  fluency gain that justifies the call), then measure on recorded fixtures: verification-failure rate, latency and
  cost per realised turn against local realisation. Labelled recorded or emulated, never live.
- If the failure rate is high or the gain is not measurable, option A stays the recommended setting and this
  ADR's default is overturned.

## Reversal plan

Realisation is one provider role (`language_realization`) routed by `internal/modes` (X-11) and orchestrated in
`internal/modes/hybrid` (X-22). Removing the role from a profile, or from the policy, returns every turn to local
realisation with no data migration.
