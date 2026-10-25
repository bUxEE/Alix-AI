# 0030 — Decision scorer deployment: LocalAI Decisions API, supervised sidecar or native

- **Status:** accepted
- **Date:** 2026-10-08
- **Decided in task:** D-01 (provisional); finalised by D-08 on this host (emulated profiles)

## Context

[ADR 0029](0029-decision-layer-clm-8b.md) adds an optional, bounded, non-generative and non-authoritative decision scorer behind `DecisionModelProvider`. The scorer, CLM-v0.1-8B, is a frozen Qwen3-8B encoder plus two small heads; it only ranks supplied authorised candidates and never authorises, writes prose or commits. D-08 tested the deployment options on the development host (i7-10700K, 16 logical CPUs, 64 GB RAM, Docker Desktop with a 31.3 GiB VM, no GPU). Every number is **emulated** (cgroup caps), never certified. Evidence: [`docs/evidence/d-decision/report.md`](../evidence/d-decision/report.md).

## Options and what was measured

1. **LocalAI Decisions API (`POST /v1/systemone`) on the pinned release.** Tested against `localai/localai:v4.10.0@sha256:0632c21d...` (lite profile, valid key): `/v1/systemone`, `/v1/systemone/permute`, `/v1/systemone/separate` and `/v1/decisions` all answer HTTP 404 `Resource not found`, exactly like an invented route; the 160,913,144-byte binary holds no `systemone` string; `adapters/decision/localai` reports `unsupported_runtime` against it (raw probe and live-test logs recorded under D-08, not published; summary in the [decision report](../evidence/d-decision/report.md)). The `v4.10.0-cpu-vllm-cpp` backend image exists on quay (index `sha256:d7f00abd...`, one amd64 layer of 23 MB), but the pinned stack's backend catalogue lists only `cpu-llama-cpp`, and a backend cannot add a route the core does not have. **The pinned LocalAI does not serve the Decisions API, so it cannot serve CLM or a sub-1B scorer.** Nothing is claimed for a later release.
2. **Supervised sidecar** (vLLM CPU image `v0.31.0` serving the encoder, plus the CLM service; `deploy/container/clm`). Balanced profile (encoder cap 14 GiB, 6 CPU): the pinned files fetched and verified (about 16.4 GB, 9 minutes on this host, every sha256 matched); the entrypoint re-hash took about 3.5 minutes; then the encoder container's memory climbed from 0.2 GiB to 13.4 GiB (cap 14 GiB = 14,336 MiB) in 166 s while the host's free RAM fell from 19 GiB to 2.3 GiB, and the process was gone 6 s later (container memory 13,742, 1,413, 844, then 35 MiB). The exit code and logs were lost when the host crashed minutes later, so an out-of-memory kill is the most likely cause, not a recorded fact. The encoder never became healthy: no load time, latency or dtype was measured. Higher-memory profile (encoder cap 26 GiB, total 30 GiB): **not run**. The host's free RAM was 17 to 35 GiB with a security product holding about 16 GiB, the balanced attempt had already driven the host to 2.3 GiB free, and the bf16 weights alone are 15.3 GiB before activations, so 30 GiB of caps do not fit a 31.3 GiB Docker VM next to the rest of the stack.
3. **Native in-process provider**: rejected, as before (no pure-Go encoder; it would break the single-binary baseline).

Resident versus demand-loaded was measured for the LocalAI runtime with the Granite 4.0 1B controller stand-in (lite, 8 GiB / 4 CPU, two active backends): resident warm call p50 592 ms, cold load 13.9 s, reload after LRU eviction 15.2 s, restart to ready 6.1 s then 12.0 s for the first call; container memory 1.4 GiB with the model resident and 1.9 GiB peak with three loaded in turn. For the sidecar, demand loading would cost the 3.5-minute re-hash plus the load on every start, so the design assumes a resident encoder.

## Decision

**The supervised sidecar (option 2) stays the only deployment path for CLM-v0.1-8B**, as an operator opt-in on a higher-memory host, because the pinned LocalAI cannot serve it. It is not part of the Lite or Balanced profiles: on Balanced the encoder does not fit (`resource_gate`), and the Lite profile never requires it. Option 1 stays behind the same provider contract for a future LocalAI release that serves `/v1/systemone`, and is re-tested on every LocalAI bump. Both are optional and neither is enabled by default ([ADR 0031](0031-decision-scorer-default-enable.md)).

## Consequences

Easier: the choice does not leak into application contracts; with the layer off, unavailable or rejected by a resource gate, AlixAI is correct through the deterministic controller. Harder: the sidecar needs a 16 GB download, a Python/vLLM stack and a host with well over 20 GiB of free RAM; whether the vLLM CPU path loads bf16 weights on an AVX2-only CPU is still unmeasured. Forbidden: assuming LocalAI hosts CLM-8B, a one-shot init container for the download, and a non-loopback endpoint without TLS. Needed next and outside D-08's scope: `tools/decbench` cannot send an API key to a scorer, so a live run against a keyed LocalAI or CLM service needs an `--api-key-ref` flag.

## Reversal plan

`decision.enabled=false` or a `nodecision` build removes the layer. Switching option is a configuration change of the provider id; the other adapter stays behind the interface until deleted.
