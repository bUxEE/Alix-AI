# 0031 — Decision scorer default-enable policy

- **Status:** accepted
- **Date:** 2026-10-08
- **Decided in task:** D-01 (provisional); finalised by D-08 on this host (emulated); per-profile decision on real hardware by D-H02

## Context

[ADR 0029](0029-decision-layer-clm-8b.md) makes the decision layer optional, bounded, non-generative and non-authoritative. CLM-v0.1-8B is a higher-memory candidate, and its vendor-reported latency and accuracy figures are not evidence for AlixAI. Efficiency is a permanent objective (§3 rule 16): a larger model has to earn its cost on the target host. D-08 ran the `tools/decbench` tooling on this host; evidence in [`docs/evidence/d-decision/report.md`](../evidence/d-decision/report.md).

## Decision

**No profile enables any scorer by default; CLM-v0.1-8B is OFF on every profile.** In the wording the exit gate checks: no profile enables CLM-8B by default, the Lite profile never requires CLM-8B (the sentence is: The Lite profile never requires CLM-8B), and a change for a profile needs a recorded benchmark. Per profile, on this host, emulated:

| Profile | Deterministic rules | Sub-1B scorer | CLM-v0.1-8B | Default |
| --- | --- | --- | --- | --- |
| lite (8 GiB / 4 CPU) | measured (configuration 1) | not measurable: the pinned LocalAI has no Decisions API (`unsupported_runtime`) and no other runtime here serves Laya or GLiNER2.5 | never required by design | OFF |
| balanced (16 GiB / 8 CPU) | measured (configuration 1) | as lite | `resource_gate`: the encoder container reached 13.4 GiB of its 14 GiB cap during load and the host's free RAM fell to 2.3 GiB; the load never completed | OFF |
| higher-memory (30 GiB of caps) | not run | not run | not measured: this host could not safely run it | OFF |

The default-enable gate (code in `tools/decbench`, D-07) refuses without a candidate run, so no CLM or sub-1B run could be gated. The one comparison that could be run is deterministic rules alone against direct controller routing on lite (the D-08 run, summarised in the [decision report](../evidence/d-decision/report.md)): rules avoid 23 of 86 controller calls (26.7%) and answer in under 1 ms, but complete 25 of 86 tasks against the controller's 86 of 86, so the gate answers do not enable (regressed: citation correctness, multilingual quality, task completion). The controller in that comparison is the harness's scripted controller with a measured latency (Granite 4.0 1B, 592 ms p50); its correctness is assumed, not measured. Rules stay in front of the controller as a free first step and nothing replaces the controller.

A change for a profile needs a recorded benchmark on that host, under concurrent RAG, voice and application-tool load, that passes the gate in separate baseline and candidate runs, each with its own measured peak, resident memory and controller latency; a new ADR then supersedes this one for that profile. A smaller scorer follows the same rule. The lite profile never requires CLM-8B.

## Consequences

Easier: no document or build can imply a production benefit before evidence exists, because the gate is code. Harder: operators opt in explicitly. Forbidden: an unconditional CLM-8B default and weakening a gate to make a candidate pass (doclint rule CLM-DEFAULT).

## Reversal plan

If evidence later passes the gate for a profile, a new ADR supersedes this one for that profile with the benchmark attached. Setting a profile back to OFF is a configuration change.

## Amendment (2026-10-25, 1.0)

Confirmed for 1.0 without change to the decision: the real-hardware gates for CPU, ARM64, CUDA, TensorRT and VPS were not measured, so CLM-v0.1-8B and every other scorer stay OFF on every profile. See [docs/verification/hardware.md](../verification/hardware.md), section 4.
