# 0010 — Final Lite model, including candidates below 1B

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-16 (provisional default and ladder); finalised in M5-09 (2026-10-08)

## Context
§25 item 1 leaves the Lite conversational-controller model open until the complete
multilingual/tool benchmark has run. The Lite target is 8 GB, 4 cores, no GPU, one interactive
session, INT4/GGUF (§7.1), and every certified profile must keep at least `max(15%, 1 GB)` of
memory free in steady state (§7.4). Rule 16 says to try the smallest practical method first, and
rule 17 says no decision may make the 4 GB direction impossible.

Two things narrow what the model has to do. First, authority never sits in the model (rule 5):
the controller maps an utterance to a capability, extracts typed fields into a schema-constrained
`TaskProposal` (M0-07/M0-12 constrain output with a JSON Schema or GBNF grammar), and realises
already-verified facts as text. Second, the work is bounded by the application contract, not
open-domain. A model that is weak at open chat can still be adequate here, and the reverse is
not guaranteed, so only the project's own suite can decide.

Task M0-13 measures the candidates under the emulated Lite profile, fixes the pass bar in
writing before looking at results, and records the provisional qualified default in
`docs/evidence/m0-spike/report.md` and `models/registry/`. M5-09 re-surveys, re-runs everything
under lite, balanced and 4g (emulated) and finalises this ADR. This ADR does not pre-empt either
result; it fixes the ordering rule, the fallback and the screens so the build never stalls.

## Options
Facts are from the model cards and Hugging Face file listings, checked 2026-10-02. KV figures are
**computed from `config.json`, not measured**: per token `2 x kv_layers x kv_heads x head_dim x 2
bytes` at f16, for one sequence slot.

0. **No model (deterministic tier).** Exact identifiers, enumerations, rules and templates, with
   response text from verified templates. Zero RAM/CPU, strongest security, and the mandatory
   first rung in every turn (§7.5, rule 16). It cannot map arbitrary multilingual phrasing to
   typed fields, so it does not replace a model; it makes the model the fallback for what code
   cannot decide.
1. **Granite 4.0 350M** (IBM, 2025-10-28, Apache-2.0). Dense decoder-only transformer, 28 layers,
   hidden 1024, 16 heads / 4 KV heads, 32K context, 12 languages (including English and
   Italian). BFCL v3 39.32, IFEval 55.4 (card). GGUF Q4_K_M 237 MB, Q8_0 378 MB. KV about
   28 KiB/token (112 MiB at 4,096 tokens). A dense model needs no hybrid-kernel support from the
   LocalAI llama.cpp backend. Smallest viable footprint; tool-calling accuracy is the open
   question. The hybrid `H-350M` variant exists and is revisited in the M5-09 re-survey.
2. **Qwen3.5-0.8B** (Alibaba, 2026-02, Apache-2.0). Hybrid Gated DeltaNet: 18 linear-attention
   and 6 full-attention layers, hidden 1024, 201 languages, non-thinking by default. BFCL-V4 25.3,
   IFEval 52.1 (card; a different BFCL version from IBM's, so **not comparable**). Q4_K_M 533 MB,
   Q8_0 812 MB. KV only on the 6 full-attention layers (2 KV heads x 256) about 12 KiB/token (48
   MiB at 4,096) plus a fixed recurrent state. Risks: the card warns the 0.8B variant is prone to
   thinking loops, so it must run non-thinking under a hard `max_tokens`; the hybrid architecture
   depends on backend support that M0-13 must confirm; a 248K-entry vocabulary and an integrated
   vision encoder make the file larger than the text path needs.
3. **Granite 4.0 1B** (IBM, 2025-10-28, Apache-2.0). Despite the name the card lists about 1.6B
   parameters: dense, 40 layers, hidden 2048, 128K context. BFCL v3 50.21, IFEval 78.53 (card;
   press coverage quotes 54.8 for BFCL, the card is used here). Q4_K_M 1.02 GB, Q8_0 1.74 GB. KV about
   80 KiB/token (320 MiB at 4,096). The §7.1 Lite fallback: about 4x the weights of option 1.
4. **Found but blocked by the §8.4 licence review.** LFM2.5-350M (LFM Open License v1.0: free
   commercial use only below a USD 10M revenue threshold) and Gemma 3 270M / FunctionGemma
   (Gemma Terms of Use: use restrictions that must be flowed to downstream users). Neither is
   OSI-permissive, so neither enters the default registry; an operator may add them knowingly,
   and the registry records a licence class per entry. Granite 4.1 (2026-05) ships only 3B, 8B
   and 30B, so nothing below 2B for Lite; it is a Balanced candidate for M5-09.
5. **Qwen3.5-2B** is the Balanced baseline (§7.2) and outside this decision.

Security impact per option: no option holds authority, so model choice changes *availability and
quality*, not the permission model. A weaker model fails closed through schema validation,
`ambiguous` / `missing_fields` clarification and the injection-resistance gate. A larger model
has more surface for prompt-injection compliance, which is why injection resistance is a hard
pass criterion and not a score.

## Decision
**No candidate qualifies, so no model is the Lite default and Lite is not certified. The bar was not lowered.**

M5-09 re-surveyed current open models (2026-10-07, first-party repositories, licence screen first) and ran the pre-registered suite
(`docs/evidence/m5-models/protocol.md`, written before any result) on every candidate of the ladder plus the one new candidate that fits the Lite size class,
MiniCPM5-1B (Apache-2.0, 688 MB Q4_K_M, declares English and Chinese only). Bar unchanged from M0-13: routing-v0 schema-valid 80/80, routing at least 49/54,
refusals 16/16, injections 16/16, no errors, peak memory inside the profile. Each model ran on its profile twice over, without the scope gate (A, the shipped path) and with it
(G, the M0-21 lever). Result (emulated, bench-prompt-v1, grammar):

| Model (profile) | Config A: schema / routing / refusal / injection | Config G: schema / routing / refusal / injection | Verdict |
| --- | --- | --- | --- |
| Granite 4.0 350M (lite) | 16 / 4 / 0 / 2 | 35 / 4 / 10 / 4 | rejected |
| Qwen3.5-0.8B (lite) | 76 / 51 / 0 / 13 | 79 / 47 / 10 / 13 | rejected |
| Granite 4.0 1B (lite) | 74 / 48 / 10 / 14 | 78 / 45 / 13 / 15 | rejected |
| MiniCPM5-1B (lite) | 56 / 26 / 1 / 12 | 58 / 24 / 10 / 12 | rejected |
| Qwen3.5-2B (balanced) | 80 / 47 / 9 / 15 | 80 / 44 / 12 / 15 | rejected |

These repeat M0-13, M0-17 and M0-21 within one to three cases (greedy decoding is not exactly repeatable): the verdict is stable under a fresh runtime session and a new candidate.
The lines that fail are the same ones: refusal of out-of-scope requests (no model reaches 16/16, the best is Granite 1B with the gate at 13/16) and injection resistance (no model reaches
16/16); the smallest rungs also fail schema validity and routing. Because no candidate cleared routing-v0, rule G of the protocol gated out conversation-v1 and the RAG answer
evaluation (not run; conversation-v1 also has no live loader, see the report). A smaller model replaces a larger one only if the whole suite passes (8.3), so nothing replaces anything.

**Ladder and order, unchanged.** The order stays Granite 4.0 350M, Qwen3.5-0.8B, Granite 4.0 1B (smallest first, deterministic tier always in front); registry `status` stays `rejected` for all
four and the new MiniCPM5-1B entry is `rejected` too; `default_for` is empty everywhere and there is no model fallback. The Lite profile serves the deterministic path (typed forms, rules router,
templates) until a model passes the bar. Context is capped at 4,096 tokens and one slot. Embeddings are a separate role (Granite Embedding 97M Multilingual R2, [0011](0011-embedded-vector-index.md)).

**Decision-scorer profiles (docs/architecture/decision-layer.md section 7).** No milestone D evidence existed yet at that time. No registry entry, profile or
compose file defaults to CLM-8B, and the Lite profile never requires it (the smallest-profile row of the decision-layer table keeps it disabled); enabling it anywhere needs recorded evidence under the section 7 gate.

**Overturn criteria (the re-evaluation path).** Re-run this suite when any of these happens: a sub-2B instruction model with a permissive licence is released (the 2026-10-07 survey found none besides
MiniCPM5-1B); a new revision or quantisation of a rejected candidate; an engine-level refusal rule that takes the out-of-scope and injection lines off the model (the cheapest lever, the scope gate,
fixes 10 of 16 refusals and costs 3 to 5 routing cases, and is not wired); or a prompt that is not tuned to the 80 cases. Move up a rung when the smaller one misses a bar. Drop a candidate whose
licence changes. Adopt a hybrid (`H`) variant only if it lowers measured RSS at equal quality. Next scheduled review: the first minor release after 1.0, or 2027-01-15, whichever is first.

## Consequences
- Easier: the pass bar was fixed before the results were seen and held; the registry default is data, not code; swapping a rung needs no host-application change
  (§8.4). The pass bar is fixed before results are seen, so the choice cannot be argued backwards.
- Easier: the deterministic tier shrinks model load in practice, which helps the 4 GB direction.
- Harder: each rung needs its own prompt/grammar tuning, and the smallest models will refuse or
  ask for clarification more often. That is the correct failure mode, but it costs UX.
- Forbidden: certifying Lite by weakening deterministic, schema, scope or injection requirements;
  shipping a model whose licence has not been screened and recorded.

## Reversal plan
The model is reached only through `AIRuntimeProvider` and a registry entry (model id, hash,
quantisation, licence class, measured memory). Reversal is a registry edit plus a re-run of the
M5-09 suite; no API, schema or SDK change. Prompts and grammars live with the capability
contract, not the model, so they survive a swap.

## Sources (checked 2026-10-02; MiniCPM5 and the 2026-10-07 survey in docs/evidence/m5-models/report.md)
- Granite 4.0 350M and GGUF: <https://huggingface.co/ibm-granite/granite-4.0-350m>, <https://huggingface.co/ibm-granite/granite-4.0-350m-GGUF>
- Granite 4.0 1B and GGUF: <https://huggingface.co/ibm-granite/granite-4.0-1b>, <https://huggingface.co/ibm-granite/granite-4.0-1b-GGUF>
- Qwen3.5-0.8B and GGUF: <https://huggingface.co/Qwen/Qwen3.5-0.8B>, <https://huggingface.co/unsloth/Qwen3.5-0.8B-GGUF>
- Granite 4.1 collection: <https://huggingface.co/collections/ibm-granite/granite-41-language-models>
- LFM Open License: <https://www.liquid.ai/lfm-license>; Gemma/FunctionGemma terms: <https://ai.google.dev/gemma/docs/functiongemma>
- MiniCPM5-1B and GGUF: <https://huggingface.co/openbmb/MiniCPM5-1B>, <https://huggingface.co/openbmb/MiniCPM5-1B-GGUF>
