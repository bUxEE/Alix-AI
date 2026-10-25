# 0029 — Optional decision layer: CLM-v0.1-8B as a bounded, non-generative, non-authoritative scorer

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** planning integration (an owner decision); finalised by D-08 (ADR-0030 deployment, ADR-0031 default-enable)

## Context

AlixAI's turn pipeline calls the conversational controller for every turn. Many turns are
bounded decisions: which declared capability a request maps to, whether a message is a confirmation, rejection,
correction, cancel or help request, which one of a short authorised candidate list the user means, and whether a
request is out of scope or an injection attempt. A generative controller is slow and memory-hungry for these.

An optional decision/router scorer can take those bounded decisions before the controller is invoked. The layer is
**optional, bounded, non-generative and non-authoritative** throughout this ADR. The owner
adopted **CLM-v0.1-8B** as the primary candidate for that role (JEV was only ever an optional proposed System One
accelerator — it was never required, never the controller and never a source of application truth; it does not
appear in any AlixAI plan or code, and it is not a candidate in this ADR). Laya and
other small scorers remain benchmark candidates and the constrained-profile fallback.

### Facts, with sources (reviewed 2026-10-02)

Fetched on 2026-10-02 and summarised by a fetch tool; **none of these facts has been verified by AlixAI yet** and the
exact checkpoint revision was not shown, so D-05/D-11 must pin the Hugging Face commit and hash before any use.

| Fact | Source |
| --- | --- |
| CLM is a "System One" model trained with a contrastive objective connecting states and actions: two small projection heads (state head, action head) on top of a **frozen Qwen3-8B encoder**, last-token pooling | model card https://huggingface.co/Contrastive-LM/CLM-v0.1-8B |
| It ranks supplied candidates or answers typed questions with probabilities; **no generation**; "encoder-locked" | model card, repo https://github.com/Contrastive-LM/CLM |
| Licence Apache-2.0 (model, repo); Qwen3-8B encoder Apache-2.0 | model card, repo (licences still need human review: D-H01) |
| Reference release `CLM-v0.1-8B`, 2026-09-19; repository had 8 commits at review | repo |
| Serving: vLLM serves Qwen3-8B (default port 8090), the CLM service runs beside it (default 8700); endpoints `POST /v1/systemone` and `POST /v1/rank`; Python `Engine.rank/answer`; `pip install contrastive-lm`; PyTorch for the heads; GPU recommended, `--device cpu` fallback mentioned | repo |
| States longer than 2048 tokens are truncated (default max 2048) | repo |
| Vendor-reported latency and accuracy claims (up to 9x lower latency than Jev, 2.8x faster for cached revisited states, ~28 ms cold with 3 actions on an RTX 4090, SOTA verifier results) — **vendor claims on other hardware and workloads; not evidence for AlixAI** | model card, repo |
| LocalAI Decisions API: `POST /v1/systemone` (also `/permute`, `/separate` for NER models), typed questions (choice, score, noul), request body capped at 64 KiB, at most 64 questions, letter-based models at most 26 options; models declare `known_usecases: [decisions]`; the vllm.cpp backend is documented as supporting Laya, GLiNER2.5-Decide, Tev1, kev, Nimble and CLM | https://localai.io/docs/features/decisions/ |

Consequences of these facts that this ADR relies on: the 8B frozen encoder makes CLM-8B a **higher-memory
candidate** that is not automatically suitable for the smallest deployments; LocalAI documents Decisions support but
**whether the pinned LocalAI v4.10.0 ships that backend, and whether it can serve CLM on CPU within a profile's
memory, is untested**; the CLM service depends on a separate vLLM encoder process.

## Options

1. **No decision model: deterministic rules + the controller.** Smallest, always correct, slowest for ambiguous
   natural language. Remains the baseline and the Lite default.
2. **A qualified sub-1B scorer** (Laya or another small non-autoregressive scorer; Granite/Qwen small controllers
   used as classifiers) for Lite and constrained hosts. Unknown quality; to be benchmarked.
3. **CLM-v0.1-8B as scorer, served through LocalAI Decisions** (if the pinned release supports it).
4. **CLM-v0.1-8B as a supervised sidecar** (vLLM encoder + CLM service on loopback or a Unix socket) behind the
   AlixAI provider contract.
5. **A native in-process provider.** Rejected for now: Python/PyTorch/vLLM stack, no pure-Go path, violates the
   single-binary baseline.
6. **CLM-8B as the conversational controller.** Rejected: it does not generate text.

## Decision (provisional)

Adopt a **provider-neutral, optional, removable decision layer** with this authority order (full flow in
`docs/architecture/decision-layer.md`):

- Deterministic scope, permission, freshness and exact-case rules run first and may skip the scorer.
- A `DecisionModelProvider` scores a **bounded authorised candidate set**. Its result is *advisory*: it can select or
  rank a permitted interpretation path and nothing else. It never authorises, validates, prepares, confirms, commits,
  sends, deletes, discloses or changes anything, never lowers risk, never decides who is authorised, and never
  generates user-facing prose or invents an action, tool, file, API call or permission.
- Confidence, calibration, candidate-size and out-of-distribution gates decide whether the result is *accepted*.
  Every selected candidate id is re-resolved by the deterministic adapter before any tool call.
- Anything else falls back, with an explicit typed fallback reason, to deterministic routing or the conversational
  controller.

Deployment profiles (benchmark-gated, **no profile enables CLM-8B by default in this ADR**):

| Profile | Decision layer |
| --- | --- |
| Lite / smallest host | deterministic routing first; a qualified sub-1B scorer only if useful and measured; CLM-8B disabled unless the complete workload proves it fits; never evicts the controller, retrieval or speech silently |
| Balanced local machine or VPS | CLM-8B evaluated resident and demand-loaded; deterministic and smaller-scorer fallback retained; eviction, latency spikes and lost concurrency measured |
| Higher-memory host | CLM-8B may be active if conformance, latency, quality, security, thermal and reliability gates pass; resident, demand-loaded and sidecar deployments compared |

Provisional deployment preference, to be decided by ADR-0030 after testing: **supervised sidecar** (Option 4) unless
the pinned LocalAI release serves CLM through its Decisions API within the profile's memory (Option 3). The scorer
runs under the existing provider registry as the `router` role and is invoked only through the contract; nothing
provider-specific leaks into the application contract.

## Benchmark evidence required before CLM-8B may be enabled by default for any profile

Same versioned multilingual (en, it) fixtures and signed conversation contracts for all five configurations —
(1) deterministic rules only, (2) qualified sub-1B scorer, (3) CLM-8B scorer, (4) direct controller routing,
(5) a scorer followed by the controller when needed. Enable only if it improves measured latency, capacity or safety
**without** reducing task success, citation correctness, multilingual quality, fail-closed behaviour or the required
memory reserve, on the target host and under concurrent RAG, voice and application-tool load. The metric list and
gates are in `docs/architecture/decision-layer.md` §7. Otherwise keep the smaller scorer or the deterministic route and
record the rejection in the model registry. No document may state that CLM-8B is faster, safer or better in
production before that evidence exists (vendor claims are quoted and labelled as such).

## Consequences

- A new optional provider contract, schemas, a dialogue stage, a benchmark and security suite (milestone D).
- The smallest profile does not silently require CLM-8B; the 4 GB direction is unaffected.
- The frozen Qwen3-8B encoder is a real memory cost (and an extra serving process); it must be measured, not assumed.
- External conversational providers do not receive decision-scoring requests in this release; the
  decision layer is local unless a future provider is explicitly qualified for the same bounded contract.

## Reversal plan

The layer is optional: setting `decision.enabled=false` (or building without the decision providers) returns the
pipeline to deterministic routing plus the controller with no change to application data, contracts or the public
API. Removal criteria: failed security or conformance gate, supply-chain finding, licence change, a benchmark that
shows no net benefit, or a resource-gate rejection on the target profile. Registry status moves to `rejected` with the
evidence path; the provider package stays behind the interface until deleted.

## Open items

Licence and supply-chain sign-off for CLM, Qwen3-8B, vLLM and any quantisation (D-H01); real-hardware gates for CPU,
ARM64, CUDA, TensorRT, container and VPS (D-H02); a CLM measurement on a host that can hold the encoder (D-08 could not
run the higher-memory profile here).

## Pinned revisions and D-08 outcome (2026-10-08)

The pins below are fetched from the registries' own metadata and verified by `deploy/container/clm/fetch.sh` (all
sha256 matched on this host); they equal `deploy/container/clm/config/pins.txt` and `models/registry/`.

| Component | Pin |
| --- | --- |
| CLM head | `Contrastive-LM/CLM-v0.1-8B` commit `e939398d4556fcd9400c76fa8c5a513202f42b0a`, `CLM_v0.1-8B.pt`, 75,557,149 bytes, sha256 `b2b4a8c9c2d39263eff78a351eb909a342ce9b3bf21a3f07c1d1bf15f1c4eda5` |
| Frozen encoder | `Qwen/Qwen3-8B` commit `b968826d9c46dd6066d109eabc6255188de91218`, five bf16 safetensors, 16,381,516,776 bytes |
| CLM service | `contrastive-lm` 0.1.0, wheel sha256 `7f3eed12d3aa10173f71bac7e50fc387565b485a2a2fc4c4323af32b1950055f` |
| vLLM CPU image | `docker.io/vllm/vllm-openai-cpu:v0.31.0@sha256:495c84fe30f3572b83a78100a1e8ba423dbb77fc532ecefebe114e6b77327bd9` |
| LocalAI | `localai/localai:v4.10.0@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b`; backend `v4.10.0-cpu-vllm-cpp` index `sha256:d7f00abd7636e2b2b6bddca5f8e15c4148b9c1b101f5491aa327864e5783a327` (exists, not installed) |

Outcome, all emulated on the development host: the pinned LocalAI does not serve the Decisions API (HTTP 404, adapter
reports `unsupported_runtime`); CLM-8B on the balanced profile hit the `resource_gate` (the encoder reached 13.4 GiB of a
14 GiB cap while loading and never became healthy); the higher-memory profile was not measured; no scorer is enabled
by default. See [ADR 0030](0030-decision-scorer-deployment.md), [ADR 0031](0031-decision-scorer-default-enable.md)
and `docs/evidence/d-decision/report.md`. The layer stays optional, bounded, non-generative and non-authoritative.
