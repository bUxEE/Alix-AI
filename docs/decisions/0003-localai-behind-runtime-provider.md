# 0003 — LocalAI as the default runtime, behind `AIRuntimeProvider`

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-03

## Context
AlixAI needs local text generation, embeddings, reranking and optionally STT, TTS and VAD, with
constrained (schema or grammar) output. Writing and maintaining inference backends is not our
product. Models and runtimes change quickly, so no domain code may depend on one of them (rule 11).
The runtime must not be reachable by anything except AlixAI (§16.1).

## Options
- **LocalAI behind a provider interface.** Open source (MIT), OpenAI-compatible HTTP API, backends
  fetched on demand, one runtime for LLM, embeddings, reranking, STT, TTS and VAD, hardware
  acceleration selection. Costs a second resident process, and its admin and model-install surface
  must be kept away from production.
- **llama.cpp server directly.** Smaller and text-focused; the speech roles would need separate
  runtimes and we would own backend packaging.
- **Ollama.** Easy model management, but it carries the same model-pull and management surface
  concern as LocalAI with no advantage that outweighs the plan's single multi-role runtime. Not
  evaluated further.
- **In-process bindings (cgo).** Lowest latency, no second process. Breaks `CGO_ENABLED=0`
  ([0001](0001-go-core-service.md), [0009](0009-pure-go-sqlite-driver.md)), couples model crashes
  to the service and removes the loopback boundary.
- **Hosted API.** Violates rules 1 and 2. Not an option for the core.

## Decision
LocalAI is the default runtime, accessed **only** through the role interfaces in
`internal/providers` (`TextGenerator`, `Embedder`, `Reranker`, voice roles later). Only
`adapters/localai` knows LocalAI exists; everything else uses the interfaces and, in tests, the
deterministic fake. The adapter calls a fixed allowlist of paths (inference, health, model list);
install, apply and delete endpoints are never callable. LocalAI binds loopback or a Unix socket and
runs under a separate least-privilege OS identity. Non-loopback endpoints need TLS. Model
selection is not decided here (open decision §25.1).

## Consequences
- Easier: swapping or adding a runtime is one new adapter; tests and the M0 gate need no model.
- Easier: degradation is explicit per role (§18); the registry reports `ok`, `degraded` or
  `unavailable`.
- Harder: LocalAI is a second process whose RSS counts against the 4 GB direction; the adapter must
  map runtime failures to `ErrCode` and never return a success-looking result.
- Forbidden: importing LocalAI code outside `adapters/localai` and the composition roots (`cmd/`,
  `internal/cli/`, `tools/`), enforced by `TestNoLocalAIImportsOutsideAdapterAndCompositionRoots`;
  exposing LocalAI's UI or admin API; model or agent installation outside an operator-approved
  process.

## Reversal plan
Write a new adapter implementing the same interfaces and wire it in the composition root. No
domain code, schema or stored data changes. Delete `adapters/localai` last, together with the
deployment files under `deploy/` that pin LocalAI.
