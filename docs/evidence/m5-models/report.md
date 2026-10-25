# M5-09 final Lite model selection and 4 GB re-run - report

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

Written 2026-10-08. Finalises ADR 0010 and ADR 0020. The protocol, the bar and the verdict rule were committed before any result existed (`protocol.md`, first commit of this task; one amendment, below).
Everything is **emulated**: one LocalAI v4.10.0 container capped by cgroup (lite 8 GiB / 4 CPUs, balanced 16 GiB / 8 CPUs, 4g 4 GiB / 4 CPUs, no swap, checked in `config/*.txt`) on an i7-10700K host under Docker
Desktop, no GPU. Never a certified machine (7.4). Other jobs shared the host, so latency is indicative; memory is not affected.

## Verdict

1. **No candidate passes the Lite bar, for the fourth time, and the bar was not lowered.** Five models were run (Granite 4.0 350M, Qwen3.5-0.8B, Granite 4.0 1B, the new MiniCPM5-1B, and Qwen3.5-2B on Balanced), each with and without
   the deterministic scope gate. All are `rejected`; `default_for` is empty; there is no model fallback; **Lite is not certified**. The numbers repeat M0-13, M0-17 and M0-21 within one to three cases.
2. **The 4 GB profile is not promoted.** Granite 350M fits (851 MiB of the 2048 MiB container budget) and cannot route; Qwen3.5-0.8B is the only rung with usable routing (51 of 54), and its memory has no margin in this run
   (2047 MiB without the gate, 2074 MiB with it, bar 2048). ADR 0020 lists the twelve blockers with their status.
3. **Suites gated out.** conversation-v1 and the RAG answer evaluation were not run, by rule G of the protocol (a suite runs only for a candidate that clears routing-v0); conversation-v1 additionally has no live loader (below).
4. **Decision layer.** No milestone D evidence existed at that time, so there was nothing from milestone D to reflect. No registry entry, profile or compose file defaults to CLM-8B and the Lite profile never requires it
   (`docs/architecture/decision-layer.md` section 7; no `default_for` in `models/registry/` names a decision scorer).

## Re-survey (2026-10-07, first-party repositories, licence screen first)

Queried the hub API for the newest repositories of ibm-granite, Qwen, google, openbmb, HuggingFaceTB, microsoft, LiquidAI, allenai, nvidia, meta-llama and apple, and by name for Qwen3.8, Gemma 4, MiniCPM5 and Granite 4.2.

| Model | Size / GGUF | Licence | Decision |
|---|---|---|---|
| Granite 4.0 350M, Qwen3.5-0.8B, Granite 4.0 1B, Qwen3.5-2B (the M0 candidates) | 237 MB, 533 MB, 1.02 GB, 1.28 GB | Apache-2.0 | benchmarked |
| **MiniCPM5-1B** (openbmb, 2026-05, Llama architecture, 1.08B parameters, English and Chinese declared) | Q4_K_M 688 MB | Apache-2.0 | **new, benchmarked** (Lite class) |
| MiniCPM5-2B (openbmb, 2026-09) | Q4_K_M 1.56 GB | Apache-2.0 | not benchmarked: Balanced class, the same size as the Qwen3.5-2B baseline; candidates are not added after results exist (protocol), so it is a Balanced challenger for the next review |
| Gemma 4 E2B and E4B (google, 2026-03; QAT Q4_0 GGUF) | 3.35 GB (E2B) | Apache-2.0 | not benchmarked: more than twice the Qwen3.5-2B file, outside Lite and 4g; a Balanced challenger for the next review |
| Granite 4.1 and 4.2 (ibm-granite) | 3B, 8B, 30B only (3B Q4_K_M 2.24 GB) | Apache-2.0 | not benchmarked: nothing below 3B |
| Qwen3.8 (Qwen, 2026-08) | 27B and larger | Apache-2.0 / other | no sub-2B release found; Qwen3.5-0.8B and 2B remain the newest small Qwen |
| LFM2.5 1.2B / 2.6B (LiquidAI) | small | LFM Open License (revenue threshold) | blocked by the licence review of ADR 0010 |
| SmolLM3 3B, nanowhale 100M (HuggingFaceTB) | 3B / 100M base | Apache-2.0 | 3B is above the class; nanowhale is a base model, not an instruction or tool model |
| Community fine-tunes and quantisations (Qwen2.5-0.5B variants, Temper, decide/ask 0.5B, ...) | n/a | various | not evidence: only first-party repositories and well-known quantisers count |

The one new first-party small instruction model is MiniCPM5-1B; it was pinned (hub commit `3d55fac8...`, sha256 `81b64d05...`, re-hashed locally), added to `models/registry/` and measured. No sub-1B model has been released since the M0 survey.

## Results

`results.json` is generated from `run-manifest.jsonl`, `benchmarks/results/m5/*.json` (80 raw cases each) and `summary/*.json` by `scripts/results.mjs` and `scripts/summarize.mjs`; the tables below are the output of
`scripts/tables.mjs`. Configuration A = no gate (shipped path, the M0-17 configuration); G = the scope gate `scopegate-v1` in front of the model (the M0-21 configuration, not wired into the engine). Latency is over the cases the model
answered (the gate answers 19 of 80 without a call). The bar for each line is in `protocol.md`; "memory" is line 6 (lite 5939 MiB, balanced 12902 MiB, 4g 2048 MiB).

### Quality against the bar (bench-prompt-v1, grammar, routing-v0)

| Profile | Model | Cfg | Schema valid (80) | Routing (49/54) | Refusal (16) | Injection (16) | Errors | Lines missed |
|---|---|---|---|---|---|---|---|---|
| lite | granite-4.0-350m-q4_k_m | A | 16/80 | 4/54 | 0/16 | 2/16 | 0 | schema_valid, capability_routing, refusal_correctness, injection_resistance |
| lite | granite-4.0-350m-q4_k_m | G | 35/80 | 4/54 | 10/16 | 4/16 | 0 | schema_valid, capability_routing, refusal_correctness, injection_resistance |
| lite | qwen3.5-0.8b-q4_k_m | A | 76/80 | 51/54 | 0/16 | 13/16 | 0 | schema_valid, refusal_correctness, injection_resistance |
| lite | qwen3.5-0.8b-q4_k_m | G | 79/80 | 47/54 | 10/16 | 13/16 | 0 | schema_valid, capability_routing, refusal_correctness, injection_resistance |
| lite | granite-4.0-1b-q4_k_m | A | 74/80 | 48/54 | 10/16 | 14/16 | 0 | schema_valid, capability_routing, refusal_correctness, injection_resistance |
| lite | granite-4.0-1b-q4_k_m | G | 78/80 | 45/54 | 13/16 | 15/16 | 0 | schema_valid, capability_routing, refusal_correctness, injection_resistance |
| lite | minicpm5-1b-q4_k_m | A | 56/80 | 26/54 | 1/16 | 12/16 | 0 | schema_valid, capability_routing, refusal_correctness, injection_resistance |
| lite | minicpm5-1b-q4_k_m | G | 58/80 | 24/54 | 10/16 | 12/16 | 0 | schema_valid, capability_routing, refusal_correctness, injection_resistance |
| balanced | qwen3.5-2b-q4_k_m | A | 80/80 | 47/54 | 9/16 | 15/16 | 0 | capability_routing, refusal_correctness, injection_resistance |
| balanced | qwen3.5-2b-q4_k_m | G | 80/80 | 44/54 | 12/16 | 15/16 | 0 | capability_routing, refusal_correctness, injection_resistance |
| 4g | granite-4.0-350m-q4_k_m | A | 16/80 | 4/54 | 0/16 | 2/16 | 0 | schema_valid, capability_routing, refusal_correctness, injection_resistance |
| 4g | granite-4.0-350m-q4_k_m | G | 35/80 | 4/54 | 10/16 | 4/16 | 0 | schema_valid, capability_routing, refusal_correctness, injection_resistance |
| 4g | qwen3.5-0.8b-q4_k_m | A | 76/80 | 51/54 | 0/16 | 13/16 | 0 | schema_valid, refusal_correctness, injection_resistance |
| 4g | qwen3.5-0.8b-q4_k_m | G | 79/80 | 47/54 | 10/16 | 13/16 | 0 | schema_valid, capability_routing, refusal_correctness, injection_resistance, memory |

### English and Italian halves of the same runs (the multilingual suite)

| Profile | Model | Cfg | en: schema / routing / refusal / injection | it: schema / routing / refusal / injection |
|---|---|---|---|---|
| lite | granite-4.0-350m-q4_k_m | A | 7/40 / 1/27 / 0/8 / 1/8 | 9/40 / 3/27 / 0/8 / 1/8 |
| lite | granite-4.0-350m-q4_k_m | G | 13/40 / 1/27 / 5/8 / 1/8 | 22/40 / 3/27 / 5/8 / 3/8 |
| lite | qwen3.5-0.8b-q4_k_m | A | 40/40 / 26/27 / 0/8 / 7/8 | 36/40 / 25/27 / 0/8 / 6/8 |
| lite | qwen3.5-0.8b-q4_k_m | G | 40/40 / 25/27 / 5/8 / 7/8 | 39/40 / 22/27 / 5/8 / 6/8 |
| lite | granite-4.0-1b-q4_k_m | A | 37/40 / 24/27 / 5/8 / 7/8 | 37/40 / 24/27 / 5/8 / 7/8 |
| lite | granite-4.0-1b-q4_k_m | G | 39/40 / 23/27 / 7/8 / 7/8 | 39/40 / 22/27 / 6/8 / 8/8 |
| lite | minicpm5-1b-q4_k_m | A | 27/40 / 14/27 / 0/8 / 5/8 | 29/40 / 12/27 / 1/8 / 7/8 |
| lite | minicpm5-1b-q4_k_m | G | 28/40 / 13/27 / 5/8 / 5/8 | 30/40 / 11/27 / 5/8 / 7/8 |
| balanced | qwen3.5-2b-q4_k_m | A | 40/40 / 23/27 / 5/8 / 8/8 | 40/40 / 24/27 / 4/8 / 7/8 |
| balanced | qwen3.5-2b-q4_k_m | G | 40/40 / 23/27 / 7/8 / 8/8 | 40/40 / 21/27 / 5/8 / 7/8 |
| 4g | granite-4.0-350m-q4_k_m | A | 7/40 / 1/27 / 0/8 / 1/8 | 9/40 / 3/27 / 0/8 / 1/8 |
| 4g | granite-4.0-350m-q4_k_m | G | 13/40 / 1/27 / 5/8 / 1/8 | 22/40 / 3/27 / 5/8 / 3/8 |
| 4g | qwen3.5-0.8b-q4_k_m | A | 40/40 / 26/27 / 0/8 / 7/8 | 36/40 / 25/27 / 0/8 / 6/8 |
| 4g | qwen3.5-0.8b-q4_k_m | G | 40/40 / 25/27 / 5/8 / 7/8 | 39/40 / 22/27 / 5/8 / 6/8 |

### Footprint and latency (emulated; U = max(R, W), model-answered cases only for latency)

| Profile | Model | Cfg | Peak U (MiB) | Median U | Bar (MiB) | B1 | B2 swap/OOM | B3 no eviction | Call p50 / p95 (s) | TTFT p95 (s) | Cold request (s) | Backend RSS min / peak (MiB) |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| lite | granite-4.0-350m-q4_k_m | A | 861.8 | 838.8 | 5939 | pass | pass | pass | 3.0 / 4.8 | 1.9 | 6.9 | 517.2 / 713.6 |
| lite | granite-4.0-350m-q4_k_m | G | 867.6 | 861 | 5939 | pass | pass | pass | 3.2 / 8.0 | 2.9 | 6.9 | 669.1 / 718.2 |
| lite | qwen3.5-0.8b-q4_k_m | A | 2061.8 | 1768.7 | 5939 | pass | pass | **fail** | 12.1 / 27.7 | 9.7 | 12.7 | 884.5 / 1915 |
| lite | qwen3.5-0.8b-q4_k_m | G | 2082 | 1972.1 | 5939 | pass | pass | pass | 11.4 / 22.1 | 8.4 | 12.7 | 1792.9 / 1935 |
| lite | granite-4.0-1b-q4_k_m | A | 2557.1 | 2520.6 | 5939 | pass | pass | pass | 12.2 / 22.5 | 11.7 | 12.9 | 2022.2 / 2420.3 |
| lite | granite-4.0-1b-q4_k_m | G | 2570.1 | 2531.7 | 5939 | pass | pass | pass | 12.4 / 25.1 | 11.9 | 12.9 | 2356.7 / 2426.5 |
| lite | minicpm5-1b-q4_k_m | A | 1518.5 | 1480.3 | 5939 | pass | pass | pass | 15.5 / 24.3 | 7.9 | 61.3 | 1137.7 / 1380 |
| lite | minicpm5-1b-q4_k_m | G | 1518.2 | 1499.9 | 5939 | pass | pass | pass | 15.9 / 22.6 | 7.5 | 61.3 | 1339.6 / 1380.1 |
| balanced | qwen3.5-2b-q4_k_m | A | 3121.4 | 2810.6 | 12902 | pass | pass | pass | 9.1 / 13.8 | 5.3 | 12.2 | 1927.3 / 2974.8 |
| balanced | qwen3.5-2b-q4_k_m | G | 3136.3 | 3121.4 | 12902 | pass | pass | pass | 10.5 / 15.8 | 6.1 | 12.2 | 2840 / 2989.2 |
| 4g | granite-4.0-350m-q4_k_m | A | 850.5 | 832.2 | 2048 | pass | pass | pass | 2.5 / 3.3 | 1.3 | 4.7 | 517.1 / 707.4 |
| 4g | granite-4.0-350m-q4_k_m | G | 856.6 | 851.1 | 2048 | pass | pass | pass | 2.5 / 3.7 | 1.5 | 4.7 | 707 / 712.3 |
| 4g | qwen3.5-0.8b-q4_k_m | A | 2046.6 | 1770 | 2048 | pass | pass | **fail** | 6.6 / 13.1 | 3.9 | 7.9 | 884.2 / 1900.9 |
| 4g | qwen3.5-0.8b-q4_k_m | G | 2073.5 | 1996 | 2048 | **fail** | pass | pass | 6.9 / 12.9 | 5.0 | 7.9 | 1797.6 / 1927.2 |

### Verdicts

| Profile | Model | Configurations run | Verdict |
|---|---|---|---|
| lite | granite-4.0-350m-q4_k_m | A, G | rejected |
| lite | qwen3.5-0.8b-q4_k_m | A, G | rejected |
| lite | granite-4.0-1b-q4_k_m | A, G | rejected |
| lite | minicpm5-1b-q4_k_m | A, G | rejected |
| balanced | qwen3.5-2b-q4_k_m | A, G | rejected |
| 4g | granite-4.0-350m-q4_k_m | A, G | rejected |
| 4g | qwen3.5-0.8b-q4_k_m | A, G | rejected |

## Reading the results

- **Refusals and injection are the lines no model reaches.** Without the gate the best is Granite 1B (10 of 16 refusals, 14 of 16 injections); with it the best is 13 of 16 (Granite 1B) and 12 of 16 (Qwen3.5-2B). The gate lifts refusals to
  10 to 13 of 16 and costs 3 to 5 routing cases; two refusal cases ("Cancel my booking" against a catalogue without cancel) need an action-level rule or a model.
- **Smaller is not cheaper in quality terms.** Granite 350M routes 4 of 54; MiniCPM5-1B (688 MB, English and Chinese declared) reaches 26 of 54 with Italian close to English (14 and 12 of 27), so the missing Italian claim did
  not show up as a gap, but it is far from the bar. Qwen3.5-0.8B routes 51 of 54 and is the smallest model that could be the controller if the refusal and injection lines were taken off it.
- **Multilingual.** The English and Italian halves are close for every model that routes at all (Qwen3.5-0.8B 26 and 25 of 27, Granite 1B 24 and 24); Italian is not the failing language.
- **4g.** See ADR 0020. Qwen3.5-0.8B: peak U 2046.6 MiB (A) and 2073.5 MiB (G) against 2048, no swap, no OOM kill, 0 major faults per second and 0 refaults, call p95 13.1 s, TTFT p95 3.9 s (A) and 5.0 s (G), cold request 7.9 s.
  The literal B3 rule ("backend resident set never below half its peak") fails for A only because the window begins while the backend grows from 884 to 1901 MiB on the first requests; nothing was evicted. M0-15 measured 1634 MiB for the
  same model on the same stack; this run is 410 MiB higher and the cause was not isolated (two configurations back to back in one process, host contention and sampler state are candidates).
- **Footprint ranking** is stable: 350M 851 MiB, MiniCPM5-1B 1519 MiB (lite), 0.8B 2047 to 2082 MiB, Granite 1B 2557 MiB, Qwen3.5-2B 3121 MiB.

## Suites not run, and why

| Suite | State | Reason |
|---|---|---|
| routing-v0, multilingual (en / it halves), injection (line 4) | run, every model, both configurations | the gating suite |
| conversation-v1 (70 cases, 110 turns) | **not run** | rule G (no candidate cleared routing-v0) and no live loader: `tools/bench` and `schemas/bench` load only single-turn corpora and are outside this task's write scope. A multi-turn schema and loader (`schemas/bench/`, `tools/bench/`) plus engine wiring on top of `tests/conversation` (`NewClinicHost`, `Adapter`, `Apply`, `RoleScope`) are needed; the turn p95 of the 4g bar needs the same harness |
| RAG answer evaluation with a controller | **not run** | rule G; the M2-15 retrieval measurement (embeddings only, FTS-only versus hybrid) stands and is not repeated |
| 4g for MiniCPM5-1B | not run | the protocol's condition (lite U at most 2048 MiB) holds (1519 MiB) but the model is rejected on quality, so a 4g footprint would change no decision |
| 4g for Granite 1B and Qwen3.5-2B | not run | measured in M0-15 on the same stack and files: 2642 and 2714 MiB, over the bar |

## Deviations and gaps

- **Amendment 1 (protocol).** MiniCPM5-1B thinks by default; with the Granite-style definition LocalAI returned empty content and retried five times per request. The first start was aborted after 10 of 80 cases with no result, its
  samples were discarded, and the definition was changed to non-thinking (as Qwen3.5 has) before the real run. No other change.
- **A power loss interrupted the Balanced session.** The PC crashed during Qwen3.5-2B; the partial session (no result file) was discarded and the session was redone from a cold start. No result file was edited or removed otherwise.
- **The Docker VM page cache was not dropped** before the cold request (that needs a privileged container, which this task may not use); the cold-request figures are therefore not comparable to M0-15's. Cold request: 350M 4.7 to 6.9 s, 0.8B 7.9 to 12.7 s,
  Granite 1B 12.9 s, Qwen3.5-2B 12.2 s, MiniCPM5-1B 61 s on a busy host (the one figure over the 60 s line, outside the lite bar).
- **Latency in the lite sessions was 1.5 to 2 times M0-17's** on the same models because other jobs shared the host; the 4g sessions ran on a quieter host. Memory figures are not affected.
- **The host-CPU sampler of M0-15 was not run**; the container sampler records the VM's CPU ticks.
- **No repeat run (B)** was made: a repeat is run only for a run that clears the bar, and none did. Run-to-run variation is visible against M0-17 and M0-21 (Granite 1B 75 to 74 schema-valid in A, Qwen3.5-0.8B 78 to 79 in G).
- **No real 4 GB or ARM64 machine, no power or CPU-frequency measurement, no concurrency measurement.**
- `docs/decisions/README.md` still lists ADR 0010 and 0020 as `provisional`; it is outside this task's write scope and needs a one-word update by the orchestrator.

## Files

`protocol.md` (pre-registered, one amendment) · `run-manifest.jsonl` (result file for every run) · `results.json` (generated) · `summary/<tag>-<cfg>.json` (memory, swap, faults, latency per run) · `samples/<tag>.csv` and `.marks` (1 s cgroup sampler and phase marks) ·
`logs/` (bench logs, cold-request replies, LocalAI logs) · `config/<tag>.txt` (the container's own view of its limits) · `models/` (the LocalAI definitions used) · `compose.m5.yaml` · `scripts/` · `benchmarks/results/m5/` (the 14 result files; `go run ./tools/bench check benchmarks/results/m5`).

Reproduce (Git Bash, Docker CLI directory first on `PATH`, API key in an env file outside the repository):
`SPIKE_ENV_FILE=<env> BENCH_BIN=<bench.exe> SCRATCH=<dir> docs/evidence/m5-models/scripts/run-profile.sh <registry-id> <lite|balanced|4g> <tag> A G`, then `node scripts/summarize.mjs <tag> <profile> <cfg> <result>`, `node scripts/results.mjs`, `node scripts/tables.mjs`.
