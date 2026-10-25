# 0020 — Whether and when the complete 4 GB profile passes the full gates

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-16 (provisional default and promotion criteria); finalised in M5-09 (2026-10-08)

## Context
The long-term objective is a 4 GB all-in profile for a bounded, text-first deployment (§7). It is
"deliberately an aspiration rather than a launch promise": it becomes supported only when a
complete implementation proves safe headroom, acceptable latency, reliable RAG and correct
application control. §7.1 allows an *experimental* 4 GB profile for text-only inference with a
smaller model and no resident embedding or voice workers, but not as the dependable minimum
until it keeps safe headroom under the full Lite test. Rule 17 requires every architecture
decision to keep it possible. §25 item 11 asks whether, and when, the complete profile passes
the full application-control, RAG, audit and safety gates.

Related decisions: the Lite model ladder ([0010](0010-lite-model-selection.md)), the vector
corpus ceiling ([0011](0011-embedded-vector-index.md)), the install shape
([0012](0012-lite-install-native-vs-container.md)), parser limits
([0014](0014-parser-sandbox.md)) and quotas ([0018](0018-resource-accounting-quotas.md)).

**The arithmetic of the headroom rule** (§7.4: keep at least `max(15% of system memory, 1 GB)` free
in steady state, no swap). On 4 GB, 15% is about 614 MiB, so the 1 GB floor binds: roughly
**3,072 MiB remain for the operating system, alixai, LocalAI, model weights, KV cache, the embedding
model, the SQLite cache and every worker together.** The sizes below are **measured** (emulated, a 4 GiB cgroup on Docker Desktop) by M0-15
([4 GB report](../evidence/m0-4gb/report.md), `docs/operations/resource-budgets.md`) and M5-09 ([model report](../evidence/m5-models/report.md)). The first version of this
table was card arithmetic and was 1.9 to 2.5 times too low; it is replaced, not kept.

| LLM rung ([0010](0010-lite-model-selection.md)) | Q4_K_M file | Backend peak (R), M0-15 | U peak, all LocalAI processes: M0-15 / M5-09 config A / M5-09 config G | Fits U <= 2048 MiB |
| --- | --- | --- | --- | --- |
| Granite 4.0 350M | 237 MB | 704 MiB | 848 / 851 / 857 MiB | yes, 1.2 GiB spare |
| Qwen3.5-0.8B | 533 MB | 1489 MiB | 1634 / **2047** / **2074** MiB | M0-15 yes; M5-09 no margin (config A passes by 1.4 MiB, config G fails by 26 MiB) |
| Granite 4.0 1B | 1.02 GB | 2498 MiB | 2642 MiB (M0-15 only) | no |
| Qwen3.5-2B | 1.28 GB | 2568 MiB | 2714 MiB (M0-15 only) | no |

U is max(R, W) per sample, R the summed resident set of the LocalAI processes and W the cgroup working set (`docker stats` alone under-reports the memory-mapped weights by about 570 MiB). The KV cache is 11.6 KiB per
token on the hybrid 0.8B (46 MiB at 4,096), which matched the card; what the card missed is the compute buffers, the sampler state and the backend's mapped libraries (the backend resident set grew from about 880 MiB at load
to 1,489 MiB in M0-15 and 1,901 MiB in M5-09 on the first requests, and stays). The M5-09 0.8B peak is 410 MiB above M0-15's on the same files, runtime and prompt; the cause was not isolated (candidates: sampler or
grammar state over the 248,000-token vocabulary, two configurations back to back in one process, host contention). The 0.8B therefore has no safe margin against the 2048 MiB line, which is the finding that matters.

Further known items (arithmetic, not measured here): the embedding model has about 97M parameters (roughly 100 MB at 8-bit,
roughly 195 MB at fp16, arithmetic from parameter count); the SQLite page cache is capped at 8 MB
in M0-05; int8 vectors cost 38.4 MB per 100k chunks if cached resident. **Measured by M0-15 (see the report):** the operating-system baseline, resident size of the LocalAI core and the llama.cpp
backend process, alixai's own RSS, runtime and compute buffers, parser worker peaks, and container
runtime overhead. The table shows the 350M rung leaves most of the budget free and the 1B rung
does not fit at all. Hosts that put a desktop OS or a VM-backed container runtime inside the same 4 GB (Windows, macOS,
Docker Desktop) are expected not to fit, which is why the experimental profile targets a headless Linux host, native or rootless Podman;
every number here was nevertheless taken on such a VM, so none of them is a 4 GB machine.

The plan today has a gap: M5-H04 runs the full suite on a real 8 GB x86-64 machine and on ARM64,
but **nothing runs the 4 GB profile on real 4 GB hardware**. Until a task does, 4 GB can pass only
as *emulated* (a 4 GB container or VM limit on a larger machine), never as certified.

## Options
A. **Promise a supported 4 GB profile at 1.0.** Contradicts §7 (an aspiration, not a launch
   promise) and the evidence rule: no measured full-workload run exists. Rejected.
B. **Drop the 4 GB target and treat 8 GB as the floor.** Simpler, but it contradicts rule 17 and
   lets always-on services, duplicated models and context growth creep in. Rejected.
C. **Keep 4 GB as an experimental, gated direction with explicit promotion criteria**, reviewed at
   fixed points. Costs a standing efficiency review on every feature; keeps the option alive at
   little cost.

Levers available to reach it without lowering any requirement: the smallest qualified LLM rung;
the deterministic tier in front of the model; one resident model with the embedding model loaded on
demand and serialised with it (never both resident); a capped 2,048-4,096 token context and one
slot; quantised KV cache if quality holds; `GOMEMLIMIT` for the Go processes; SQLite cache capped
and mmap off; no reranker, no voice; class-5/6 work yielding to interactive turns (§17.1) and parser
workers bounded by [0014](0014-parser-sandbox.md); no swap or zram, because compressed swap is
still swap for §7.4.

## Decision
**Option C, accepted. The 4 GB profile is not a supported or certified profile and has not reached "experimental, emulated pass"; nothing in the 1.0 documentation may promise it.** It stays the
experimental `experimental-4g` profile (text-only, no resident embedding or voice workers, headless Linux, one session), labelled **emulated** in every result, until it meets *all* of these promotion criteria.
The 4g bar of M0-15 is carried in unchanged, with the turn line added:

1. **Headroom (§7.4).** Peak U = max(resident set of the LocalAI processes, cgroup working set) at most **2048 MiB** over the warm 80-case window and over the full text workload (conversation-v1, RAG eval,
   injection suite, a concurrent indexing run); this is the 1 GiB steady-state headroom plus a 1 GiB allowance for the operating system and the AlixAI core. No swap (`memswap_limit` equal to `mem_limit`) and no OOM kill.
2. **No eviction or reload on ordinary turns.** The backend is never unloaded and major page faults on the model file stay at or below 1 per second (weights are memory-mapped, so RSS alone hides it).
3. **The same pass bar as Lite** on application control, RAG grounding, audit and safety (routing-v0 bar of ADR 0010 and the later suites): no requirement is relaxed and no safety or verification step is dropped to fit.
4. **Latency:** per proposal call p95 at most **20 s**, time to first token p95 at most **5 s**, **turn p95 at most 35 s**, cold start (first request after the container starts) at most **60 s**; background indexing inside
   its CPU, memory and I/O limits without starving interactive work.
5. For *supported* status, a real 4 GB hardware run plus the 24-hour mixed-load and 7-day soak tests of §7.4. Emulated results alone can at most justify "experimental, emulated pass".

**Measured by M5-09** (emulated, routing-v0, `bench-prompt-v1`, grammar, config A = no gate, G = scope gate; `docs/evidence/m5-models/report.md`):

| Model under 4g | Quality (A) | Peak U A / G (bar 2048) | No swap, no OOM | Eviction | Call p95 / TTFT p95 (A) | Cold | Result |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Granite 4.0 350M | 16/80 schema, 4/54 routing, 0/16 refusal, 2/16 injection | 851 / 857 MiB | pass | none (0.1 faults/s) | 3.3 s / 1.3 s | 4.7 s | fits, fails quality |
| Qwen3.5-0.8B | 76/80, 51/54, 0/16, 13/16 | **2047** / **2074** MiB | pass | none (0 faults, 0 refaults) | 13.1 s / 3.9 s | 7.9 s | quality fails; memory passes A by 1.4 MiB and fails G by 26 MiB |

Granite 4.0 1B (2642 MiB) and Qwen3.5-2B (2714 MiB) were not re-measured on 4g: M0-15 measured them on the same stack and files and they failed criterion 1 by 600 to 700 MiB. Turn p95 was not measured: the
conversation harness for conversation-v1 does not exist (`tools/bench` loads single-turn corpora only), so criterion 4's turn line stays open. The embedder-beside-controller result of M0-15 stands (a resident
embedder projects to 1,900 to 2,020 MiB; with one active backend a RAG turn reloads a model on every call, 15.2 s and 6.2 s); no ingestion-swap measurement was repeated.

**Outcome: not promoted. What still blocks it, exactly** (blockers P1 to P12 of `docs/evidence/m0-4gb/report.md`; "fixed" means fixed in code and not re-measured on 4g):

| # | Blocker | Status after M5-09 |
| --- | --- | --- |
| P1 | no qualified controller model | **open**: five candidates rejected on the unchanged bar (ADR 0010) |
| P2 | models that fit do not pass, models that pass do not fit | **open**: 350M fits and routes 4 of 54; 0.8B routes 51 of 54, fails refusal and injection, and has no memory margin |
| P3 | RAG has no 4 GB shape | **open**: the FTS-only mode (`reduced_semantic_recall`) exists in code; no RAG quality or serialised-ingestion measurement on 4g |
| P4 | voice cannot be resident | **accepted**: 4g is text-only by definition; M4-06 found no qualified TTS even on Lite |
| P5 | nothing stops an optional model evicting the controller | **fixed in code** (resource governor, `LOCALAI_MAX_ACTIVE_BACKENDS` readiness guard); not exercised on 4g |
| P6 | latency close to the line | **open**: call and TTFT lines pass for the 0.8B (13.1 s, 3.9 s), turn p95 unmeasured, host was shared |
| P7 | cold start and readiness | **fixed in code** (`inference_warmth` readiness check); cold request 4.7 s and 7.9 s measured |
| P8 | memory outside the container and real hardware unmeasured | **open / accepted as emulated-only**: still a Docker Desktop VM, no 4 GB machine, no ARM64 |
| P9 | controller memory follows the prompt | **open**: prompt caps enforced by the dialogue profile (ADR 0034), but the 0.8B reached 2047 MiB (1634 in M0-15) and the cause is not isolated |
| P10 | output constraint not applied | **fixed** (the gateway applies a GBNF grammar; the bench uses it) |
| P11 | full workload not run on 4g | **open**: only routing-v0 ran; conversation-v1, RAG answers, concurrent indexing, 24 h and 7 d not run |
| P12 | Granite results are noisy | **accepted**: run-to-run differences of 1 to 3 cases again, no verdict within reach of its bar |

**Real-hardware evidence still missing for supported status:** a run on a real 4 GB x86-64 machine and on ARM64 with swap and zram off natively; the 24-hour mixed-load and 7-day soak tests; the operating-system and core
memory outside the container (`resource-budgets.md` 4.2 is an estimate); a non-shared host, so latency is a measurement and not an indication.

The ADR is re-opened at every minor release and after any model, prompt-gate or architecture change. **Overturn criteria.** Promote only on all criteria. Demote or remove `experimental-4g` if no rung can meet
them and the remaining blockers are structural; the 0.8B having no memory margin is a warning toward that, not yet a ground.

## Consequences
- Easier: one honest statement for users and one scoreboard for engineers; every efficiency review
  already records whether a feature preserves, improves or worsens the 4 GB target (§7).
- Harder: a standing check on every feature, model and provider; some convenient designs
  (always-on workers, resident duplicate models, large context) are not allowed.
- Security impact: memory pressure must never be solved by weakening isolation, validation or
  verification; a smaller budget reduces *capability exposed to the model* (least capability,
  smaller context), not deterministic checks. Out-of-memory conditions fail closed (`unavailable`,
  work refused before exhaustion, §17.2), never partially executed.
- Forbidden: calling 4 GB "supported", "certified" or "dependable" before the criteria are met;
  labelling an emulated run as real hardware; lowering the pass bar or dropping a safety step to
  fit; enabling swap or compressed swap to hit a headroom number.

## Reversal plan
Promotion and demotion are changes to documentation, the profile label and compose overrides; the
architecture is the same in every profile (same APIs and contracts, §7.3). Removing
`experimental-4g` deletes an override file and its tests; nothing else depends on it.

## Sources (checked 2026-10-02; measurements in the 4 GB and M5-09 model reports under docs/evidence/)
- Model cards and configs behind the arithmetic: see [0010](0010-lite-model-selection.md) and
  [0011](0011-embedded-vector-index.md) (Granite 4.0 350M and 1B, Qwen3.5-0.8B, Granite Embedding 97M).
- WSL2 defaults (memory 50% of host RAM, swap 25% of host RAM): <https://learn.microsoft.com/en-us/windows/wsl/wsl-config>

## Update 2026-10-03 (M0-15)
Measured emulated 4 GiB runs (docs/evidence/m0-4gb/report.md, docs/operations/resource-budgets.md): the container budget is 2048 MiB; Qwen3.5-0.8B peaks at 1634 MiB and
Granite 350M at 848 MiB and both pass B1-B4, Granite 1B and Qwen3.5-2B fail B1 (2642 and 2714 MiB). The card arithmetic in the table above is 1.9-2.5x too low against
measured backend peaks; M5-09 (2026-10-08) replaced it with the measured table above and accepted the ADR; the profile is still not promoted (no real 4 GB machine, no ARM64, the full workload has not run on 4g).
