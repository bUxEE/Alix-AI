# M0-15 first 4 GB feasibility experiment (x86-64, emulated) - report

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

Status: **complete**, 2026-10-03. The method and the bar in "What this experiment is" and "Pass bar for the 4g footprint" were committed before any 4g
result existed (`git log -- docs/evidence/m0-4gb/report.md`, first commit) and are reproduced here unchanged; everything else was written after the runs.

## Verdict

**The 4 GB profile is not certifiable today, and nothing measured here changes that.** It is an aspiration (ADR 0020) and this document
says what blocks it instead of passing it. What was measured, all **emulated** (a 4 GiB cgroup on a 16-CPU Docker VM on a Windows host), is below.

1. **One small controller fits.** Qwen3.5-0.8B alone peaks at **1634 MiB** of the 4096 MiB cap over the 80-case run: 2462 MiB are free, 1438 MiB after
   the 1 GiB assumed for the operating system and the AlixAI core (7.4 asks for 1024 MiB). No swap (`memory.swap.max` is 0, `memswap_limit` equals
   `mem_limit`, 0 bytes of swap in every sample), no out-of-memory kill, no eviction (0.0 major page faults per second over 717 s, the backend never
   unloaded), call p95 14.4 s and time to first token p95 4.1 s on the bench prompt. Granite 4.0 350M peaks at 848 MiB. Lines B1 to B4 of the bar pass for both.
2. **The next two rungs do not fit.** Granite 4.0 1B peaks at 2642 MiB and Qwen3.5-2B at 2714 MiB against the 2048 MiB the container may use, so they
   fail B1 (430 and 358 MiB are left after the allowance, 1024 are needed); the 2B also fails the time-to-first-token line (6.7 s).
3. **It fits only without the rest of the product.** A resident embedder beside the 0.8B is projected at 1900 to 2020 MiB of the 2048 (no margin), and
   with the shipped one active backend a RAG turn reloads a model on every call: **15.2 s to answer after an embedding, 6.2 s to embed after an answer,
   70 page faults per second**. Voice, a reranker and a decision scorer cannot be resident.
4. **No model that fits is qualified**, and the ones that come close do not fit (blockers P1 and P2). The Lite quality bar is M0-13's and was not moved.

The twelve things that currently prevent 4 GB certification, with evidence and what would clear each, are in "What currently prevents 4 GB
certification" below. The budgets every later task has to respect are in `docs/operations/resource-budgets.md`.

## What this experiment is, and is not

It is a **footprint feasibility** test of the experimental 4 GB profile (ADR 0020): LocalAI v4.10.0, llama-cpp backend,
under `deploy/container/localai/compose.4g.yaml` (`mem_limit` 4 GiB, `memswap_limit` equal to `mem_limit` so the container has no swap,
4 CPUs, `LOCALAI_THREADS=4`, one resident backend, no embedding and no voice worker), driven by `tools/bench` over the v0 corpus. It
measures memory headroom, swap, eviction (page faults) and latency. It is **not** a qualification of any model: M0-13 rejected every
controller, so no qualified text model exists and no 4 GB profile can be certified whatever these numbers say (that is blocker 1 in the
list at the end).

Everything is **emulated**: a cgroup cap on one container on an i7-10700K host (8 cores, 16 threads, 64 GB, Windows 11, Docker Desktop
WSL2 VM of 16 CPUs and 32 GB, no GPU used). Other agents ran Go tests on the same host, so latency is indicative; the host and VM CPU
load of each run is sampled and reported next to it. Model files sit on a Windows-drive bind mount seen through Docker Desktop, so the
page-cache and fault behaviour of a native ext4 Linux host can differ.

## Subjects (rule fixed before the runs)

| Role | Model | Why |
|---|---|---|
| **Primary** | Qwen3.5-0.8B Q4_K_M (533 MB) | the smallest of M0-13's candidates that routes meaningfully (52/54); Granite 350M routes 0/54, so a footprint measured on it says nothing about a model that can do the controller's job. Rejected in M0-13 on refusal and injection; used here for its footprint only |
| Floor | Granite 4.0 350M Q4_K_M (237 MB) | the smallest model that loads: the lowest footprint any controller could have |
| Ceiling | Granite 4.0 1B Q4_K_M (1.02 GB) and Qwen3.5-2B Q4_K_M (1.28 GB) | the next two rungs, to show where 4 GiB stops being enough |
| Co-residency probe | Granite Embedding 97M Q4_K_M (105 MB) beside the primary | separate from the text-only measurement, to measure what a resident embedder costs |

## Pass bar for the 4g footprint (written before the results)

Definitions. The container's 4 GiB cap stands for the whole machine. The headroom rule: headroom is at least `max(15% of system memory, 1 GB)`;
for 4096 MiB that is `max(614, 1024)` = **1024 MiB**, so the 1 GB floor binds (ADR 0020 does the same arithmetic). A real 4 GB machine also runs
the operating system, the AlixAI core and its SQLite cache outside the LocalAI container; nobody has measured that, so the same **1024 MiB
allowance** the M0-13 Lite bar used (line 6 there) is assumed and is itself listed as an unknown in the blockers. The memory the LocalAI
container may therefore use at steady state is 4096 - 1024 - 1024 = **2048 MiB**.

Memory is read three ways, because `docker stats` alone hides what a memory-mapped model needs: **R** = summed resident set of the LocalAI processes
(anonymous plus file-backed mapped pages, `VmRSS`), **W** = cgroup `memory.current` minus `inactive_file` (the figure `docker stats` shows),
**C** = cgroup `memory.current` (includes all page cache). The judged figure is **U = max(R, W)** in every sample (never the smaller of them).

| # | Line | Bar |
|---|---|---|
| B1 | Steady-state headroom | peak of U over the measured window (80-case run, warm model, one request at a time) at most **2048 MiB**; the median is reported too. Headroom reported is `4096 - U` (with and without the allowance) |
| B2 | No swap | `memory.swap.max` is 0 inside the container, `memory.swap.current` is 0 in every sample, `memory.events` `oom_kill` is 0 |
| B3 | No eviction and reload on ordinary turns | during the measured window the backend's resident set never drops below half of its peak (no unload), the mean rate of major page faults is at most 1 per second and `workingset_refault_file` grows by at most 100 pages. A memory-mapped model that does not fit shows up here before it shows up in RSS (ADR 0020 criterion 2) |
| B4 | Latency, provisional | per proposal call p95 at most 20 s and time to first token p95 at most 5 s on the bench prompt with at most 256 output tokens; first request after a cold start at most 60 s (the gateway's own call budget, M0-13 finding F4). These three numbers are **proposed here** (M0-13 left the latency gate to this task) from the 60 s call budget and M0-13's measured lite figures; they are not user research and are open to an ADR |
| B5 | Quality | **not judged here.** The quality bar is M0-13's (report.md there); it is not relaxed and not re-run. A footprint pass is "fits the 4g footprint", never "qualified" or "certified" |

Recorded, not gated: cgroup `memory.events` `max` and `high` counts, CPU quota throttling (`nr_throttled`, `throttled_usec`), VM and host CPU
load, anonymous versus file memory, KV and compute buffer sizes where the runtime logs them, decode speed, cold load time.

The measured window starts after a separate cold-start request (timed by itself) and a 20 s settle, and ends with the last bench case; an
idle window of 90 s follows with the model still resident. If the primary model fails B1 the report says which line failed and by how much;
it does not move the bar.

## Results

Every table of this section is generated from `summary/*.json` and `samples/` by `scripts/tables.mjs`, and every row traces to a file there. Tags:
`<model>-4g` is the 80-case bench run, the other tags are probes (`scripts/chain2.sh`, `scripts/chain3.sh`).

### What was actually in force

`config/<tag>.txt` records the container's own view for every run. All of them: `HostConfig.Memory` and `MemorySwap` both 4294967296,
`NanoCpus` 4000000000; inside the container `memory.max` 4294967296, **`memory.swap.max` 0**, `memory.swap.current` 0, `cpu.max` 400000 100000;
kernel 6.18.33.2 (WSL2); the Docker VM itself has 32 GiB and 8 GiB of swap, which the cgroup cannot reach. LocalAI v4.10.0 (image
`sha256:0632c21d...`) with the backend `cpu-llama-cpp` (`sha256:d7b0d9c6...`), `LOCALAI_THREADS=4`, one slot (`parallel` 1), `mmap` on, context 4096,
`n_batch` 512, one active backend unless a probe says otherwise, the 15-minute idle watchdog, models and corpus exactly those of M0-13 (artefact hashes
checked by `bench run --artifact`). Each run restarts the stack, so each model starts cold; the Docker VM's page cache is dropped first (`caches_dropped`
in the marks), the Windows host's file cache is not. The sampler reads the cgroup every 3 to 5 seconds (a `docker exec` takes about 3 s here), 160 to 265
samples per bench run.

### The four controllers under the 4g profile

| Model | File | Idle, no model | Loaded, idle | Peak U (bar 2048) | Median U | R backend anon / file | Peak W (docker stats) | Headroom at peak: no allowance / with 1 GiB allowance (needs 1024) | B1 |
|---|---|---|---|---|---|---|---|---|---|
| Qwen3.5-0.8B (primary) | 533 MB | 125 | 1012 | **1634** | 1610 | 964 / 525 | 1067 | 2462 / 1438 | pass |
| Granite 4.0 350M (floor) | 237 MB | 121 | 641 | **848** | 815 | 456 / 248 | 551 | 3248 / 2224 | pass |
| Granite 4.0 1B (ceiling) | 1.02 GB | 122 | 2147 | **2642** | 2578 | 1504 / 995 | 1606 | 1454 / 430 | **fail** |
| Qwen3.5-2B (ceiling) | 1.28 GB | 124 | 2056 | **2714** | 2641 | 1330 / 1238 | 1442 | 1382 / 358 | **fail** |

| Model | Swap max (bytes) | oom_kill | Major faults / s | Refaults | Backend RSS min / peak | CPU cores used | Throttled periods (s) | VM CPU busy median % | Host CPU median / p95 % | B2 | B3 |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Qwen3.5-0.8B (primary) | 0 | 0 | 0.0 | 0 | 884 / 1489 | 3.8 | 123 (0.8) | 24.8 | 79 / 100 | pass | pass |
| Granite 4.0 350M (floor) | 0 | 0 | 0.0 | 0 | 517 / 704 | 3.5 | 36 (0.3) | 23.9 | 86 / 100 | pass | pass |
| Granite 4.0 1B (ceiling) | 0 | 0 | 0.0 | 0 | 2022 / 2498 | 3.8 | 167 (1.1) | 24.9 | 76 / 100 | pass | pass |
| Qwen3.5-2B (ceiling) | 0 | 0 | 0.0 | 0 | 1927 / 2568 | 3.9 | 212 (1.0) | 24.8 | 69 / 99 | pass | pass |

| Model | Cold start to ready (s) | Cold first request (s) | p50 / p95 per call (s) | TTFT p50 / p95 (s) | Decode (tok/s) | Output tokens mean | B4 (call p95 <= 20 s, TTFT p95 <= 5 s, cold <= 60 s) |
|---|---|---|---|---|---|---|---|
| Qwen3.5-0.8B (primary) | 10 | 8.6 | 8.3 / 14.4 | 2.7 / 4.1 | 15.9 | 95 | pass |
| Granite 4.0 350M (floor) | 7 | 5.8 | 2.7 / 5.3 | 0.7 / 1.2 | 28.9 | 65 | pass |
| Granite 4.0 1B (ceiling) | 17 | 11.0 | 8.1 / 14.0 | 3.1 / 4.9 | 9.4 | 59 | pass |
| Qwen3.5-2B (ceiling) | 20 | 14.6 | 12.0 / 17.2 | 5.6 / 6.7 | 8.3 | 63 | **fail** |

The quality rows of the same runs are in the last table of this section; they are a by-product, not the point, and not judged (B5).

### Verdict against the pre-registered bar

| Model | B1 steady-state headroom (peak U <= 2048) | B2 no swap, no OOM kill | B3 no eviction and reload | B4 latency (call p95 <= 20 s, TTFT p95 <= 5 s, cold <= 60 s) | Fits the 4g footprint |
|---|---|---|---|---|---|
| Qwen3.5-0.8B (primary) | pass: 1634 MiB, 2462 MiB free of 4096 (1438 after the 1 GiB allowance) | pass | pass: backend never below 884 MiB, 0.0 faults/s | pass: p95 14.4 s, TTFT p95 4.1 s, cold 8.6 s | **yes** |
| Granite 4.0 350M (floor) | pass: 848 MiB, 3248 MiB free of 4096 (2224 after the 1 GiB allowance) | pass | pass: backend never below 517 MiB, 0.0 faults/s | pass: p95 5.3 s, TTFT p95 1.2 s, cold 5.8 s | **yes** |
| Granite 4.0 1B (ceiling) | **fail**: 2642 MiB, 1454 MiB free of 4096 (430 after the 1 GiB allowance) | pass | pass: backend never below 2022 MiB, 0.0 faults/s | pass: p95 14 s, TTFT p95 4.9 s, cold 11.0 s | no |
| Qwen3.5-2B (ceiling) | **fail**: 2714 MiB, 1382 MiB free of 4096 (358 after the 1 GiB allowance) | pass | pass: backend never below 1927 MiB, 0.0 faults/s | **fail**: p95 17.2 s, TTFT p95 6.7 s, cold 14.6 s | no |

The bar was not moved after the results. B1 is judged on `U = max(R, W)` at its peak over the measured window: for every model here `R` is the larger
figure, `W` (what `docker stats` shows) is 53 to 65 percent of it, because the weights are memory-mapped file pages that the cgroup keeps on its inactive
list while the backend uses them. A `docker stats` reading would have under-reported the 0.8B's peak by about 570 MiB (1067 against 1634).

### Where the primary model's memory goes (Qwen3.5-0.8B, MiB)

| State | Summed resident set `R` | of which the backend, anonymous / file-backed | Source |
| --- | ---: | ---: | --- |
| LocalAI up, no model loaded | 125 | none | `idle_before_load` |
| Model loaded, idle (after the cold request) | 1012 | 360 / 524 | `after_load_idle_settle` |
| During the 80-case run (median / peak) | 1610 / **1634** | 940 / 525 | `measured` |
| 90 s idle after the run, model still resident | 1610 | 940 / 525 | `idle_after`: nothing is given back until the backend unloads |
| A 3,046-token prompt, no grammar (probe A) | **1942** | 1269 / 525 | `qwen3.5-0.8b-q4_k_m-4g-probe-A` |

The weights are about 524 MiB of file-backed pages (the 508 MiB file plus the backend's mapped libraries), the LocalAI service is 125 to 146 MiB, and the rest, 360 MiB at load and 940 MiB after the
first requests, is anonymous: the KV cache (11 to 12 KiB per token on this hybrid model, so about 46 MiB at 4096) plus compute buffers and sampler state.
The growth from 360 to 940 MiB happens on the first bench requests and stays. What causes it (prompt length, or the grammar sampler over this model's
248,000-token vocabulary: the bench uses a grammar, the probes do not) was **not isolated**; the 3,046-token probe without a grammar reached 1269 MiB of
anonymous memory, the 80-case grammar run 940 MiB at prompts of about 330 tokens. What the budget needs from it is the bound: the peak follows the largest
prompt the runtime is given, which is why the prompt cap of `resource-budgets.md` section 6 is a memory control as well as a latency one.

### Context size and the other levers (Qwen3.5-0.8B)

| Configuration | Loaded, idle U (MiB) | Peak U in the probe (MiB) | Backend anon peak (MiB) | Backend file (MiB) | Probe error |
|---|---|---|---|---|---|
| default (context 4096, batch 512) | 1011 | 1466 | 803 | 524 | none |
| context 2048 | 989 | 1297 | 635 | 524 | none |
| context 8192 | 1059 | 1757 | 1092 | 524 | none |
| context 16384 | 1156 | 2069 | 1404 | 525 | none |
| context 4096, batch 128 | 1011 | 1467 | 803 | 524 | none |
| context 4096, 8-bit KV cache, flash attention | 990 | 1421 | 758 | 524 | none |

- Context size costs about **11.6 KiB per token** of idle memory (989 MiB at 2048, 1012 at 4096, 1059 at 8192, 1156 at 16384). Going from 4096 to 2048
  saves 22 MiB and a 2048-token prompt then no longer fits, as it should. The native 262,144-token window would need about 3 GiB of KV alone (arithmetic from this slope), the kind of load that M0-13's `m0-oom` probe
  saw the cgroup kill on another model.
- **Batch size is not a lever** here: batch 128 and 512 peaked within 1 MiB of each other (1467 and 1466), the idle memory is identical. An 8-bit KV cache with flash
  attention saves 21 MiB idle and 45 MiB at the peak, about 3 percent, and its quality effect was not tested.
- The probes' peaks differ because their largest prompts differ (the 2048-token prompt is rejected at context 2048), so compare idle memory across the
  rows and use the peaks only as "what this configuration did under 512 to 2048 token prompts".
- Prompt-prefix reuse is on: the same prompt answered its first token in 0.14 to 0.34 s instead of 4 to 18 s (`measured-prompts-*.json`). LocalAI also passes
  `cache_reuse:256` to the backend, which the backend disables for this hybrid model ("cache_reuse is not supported by this context",
  `logs/localai-qwen3.5-0.8b-q4_k_m-4g-debuglog.log`).

### Controller and embedder together (probe C, `scripts/coresidency.mjs`)

| Active backends | Chat after embed median / max (s) | Chat after chat median (s) | Embed after chat median / max (s) | Failures | Peak U (MiB) | Backend processes | Major faults in window |
|---|---|---|---|---|---|---|---|
| 1 | 15.2 / 17.0 | 2.7 | 6.23 / 6.99 | 0 | 1090 | 0,1 | 9676 |
| 2 | 3.1 / 3.2 | 2.7 | 0.06 / 6.09 | 0 | 1333 | 0,1,2 | 57 |

With one active backend every call reloads the model it needs; nothing fails, it is slow and the weights are read again each time (70 page faults per
second, `U` dropping to 149 MiB between calls). With two both stay resident and the second backend adds about 250 MiB of summed resident set. The peak of 1333
MiB is for small prompts; the controller alone reached 1634 at the bench prompts and 1942 at 3,046 tokens, so a resident embedder is projected at 1900 to
2020 MiB at the bench prompts (the sum of two measurements, not a run) and over the line at long prompts.

### Prefill, decode and concurrency (probe A and `concurrency`)

| Prompt tokens (real) | TTFT median (s) | TTFT max (s) | Prefill (tok/s) | Decode (tok/s) |
|---|---|---|---|---|
| 128 | 1.2 | 1.3 | 106 | 17.5 |
| 273 | 2.2 | 2.5 | 123 | 18.4 |
| 508 | 4.1 | 4.2 | 122 | 16.4 |
| 1025 | 9.2 | 12.0 | 112 | 12.3 |
| 1541 | 16.9 | 25.4 | 91 | 13.4 |
| 2012 | 18.3 | 18.9 | 110 | 12.2 |
| 3046 | 27.0 | 32.0 | 113 | 13.6 |

| Requests at once | Per-request total median (s) | max (s) | Wall (s) | Failures |
|---|---|---|---|---|
| 1 | 7.1 | 7.2 | 7.1 | 0 |
| 2 | 9.1 | 15.6 | 14.7 | 0 |
| 4 | 15.2 | 31.7 | 29.4 | 0 |

The first version of the concurrency probe sent the same prompt every time and measured the prompt cache (1 request 2.0 s, 4 at once 3.2 s each); it was
replaced by this one with a unique first line per request and its output deleted. With one slot, concurrent requests queue: two at once cost the second one
the first one's whole duration.

### Real prompts of the dialogue engine (the estimates of M1-14, measured)

| Prompt | Estimated (providers.EstimateTokens) | Real, Qwen tokenizer | Real, Granite tokenizer | Real / est (Qwen) | Output tokens as sent today (Qwen3.5-0.8B, no grammar, cap 256) |
|---|---|---|---|---|---|
| proposal-1caps-nohistory-en | 787 | 391 | 368 | 0.50 | 79 (stop) |
| proposal-1caps-goal-and-4-turn-rows-en | 982 | 562 | 528 | 0.57 | 84 (stop) |
| proposal-2caps-nohistory-en | 887 | 436 | 413 | 0.49 | 104 (stop) |
| proposal-2caps-goal-and-4-turn-rows-en | 1083 | 606 | 572 | 0.56 | 128 (stop) |
| proposal-3caps-nohistory-en | 971 | 483 | 459 | 0.50 | 118 (stop) |
| proposal-3caps-goal-and-4-turn-rows-en | 1168 | 653 | 618 | 0.56 | 118 (stop) |
| proposal-5caps-nohistory-en | 1076 | 530 | 505 | 0.49 | 133 (stop) |
| proposal-5caps-goal-and-4-turn-rows-en | 1272 | 700 | 664 | 0.55 | 156 (stop) |
| proposal-8caps-nohistory-en | 1292 | 644 | 617 | 0.50 | 151 (stop) |
| proposal-8caps-goal-and-4-turn-rows-en | 1488 | 814 | 776 | 0.55 | 175 (stop) |
| proposal-12caps-nohistory-en | 1618 | 787 | 759 | 0.49 | 235 (stop) |
| proposal-12caps-goal-and-4-turn-rows-en | 1814 | 957 | 918 | 0.53 | 248 (stop) |
| proposal-5caps-nohistory-it | 1090 | 537 | 570 | 0.49 | 128 (stop) |
| proposal-5caps-goal-and-4-turn-rows-it | 1286 | 707 | 729 | 0.55 | 132 (stop) |
| demo-app-call-01-proposal | 763 | 385 | 364 | 0.51 | 81 (stop) |
| demo-app-call-02-response | 334 | 252 | 237 | 0.75 | 9 (stop) |
| demo-app-call-03-proposal | 871 | 476 | 450 | 0.55 | 57 (stop) |
| demo-app-call-04-response | 369 | 278 | 261 | 0.75 | 22 (stop) |
| demo-app-call-05-response | 330 | 249 | 234 | 0.76 | 10 (stop) |

The estimated sizes are those of `docs/efficiency/M1-14.md` and `docs/efficiency/M3-03.md`; the real ones are the runtime's `prompt_tokens` for the same
messages. The last column is what the model wrote when asked the way the gateway asks today (a `json_schema` the runtime silently drops, M0-13 F1):
57 to 156 tokens for one to five capabilities and 235 to 248 for twelve, close to the 256 cap, where a grammar run of the 80-case corpus gave a mean of 95.

### Greedy decoding is not repeatable on the Granite models (observation for M0-17)

The four 4g bench runs reproduce M0-13 exactly for Qwen3.5-0.8B (the 80 outputs are byte-identical) and only approximately for the others: Granite 350M
56/80 schema-valid against 55/80; Granite 1B 76/80 against 74/80, refusal 13/16 against 12/16, injection 15/16 against 14/16 (routing 37/54 both times); Qwen3.5-2B
schema 78/80 against 80/80, routing 46/54 against 48/54, injection 14/16 against 16/16 (it ran at 8 threads in M0-13, 4 here). Output text differs in 65 of 80 cases for the 350M, 43 for the 1B and 30 for the 2B.
`scripts/determinism.mjs` shows why it is not a harness difference: five identical requests at temperature 0 to one running Granite 350M gave **two
different answers** (also when another prompt was interleaved), and five to Qwen3.5-0.8B gave one. The cause was not found; LocalAI's default
`cache_reuse:256` is a candidate (untested). The consequence is that a quality verdict on a Granite model is a measurement with noise of one to two
cases of 80, and M0-13's verdicts stand because none was within reach of its bar.

### Quality of the same runs

| Model | schema valid | routing | refusal | injection | errors |
|---|---|---|---|---|---|
| Qwen3.5-0.8B (primary) | 76/80 | 52/54 | 0/16 | 13/16 | 0 |
| Granite 4.0 350M (floor) | 56/80 | 0/54 | 0/16 | 8/16 | 0 |
| Granite 4.0 1B (ceiling) | 76/80 | 37/54 | 13/16 | 15/16 | 0 |
| Qwen3.5-2B (ceiling) | 78/80 | 46/54 | 7/16 | 14/16 | 0 |

## What currently prevents 4 GB certification

Each item: what it is, the evidence, what would clear it, who owns the next step. "Feature" means something the product needs on a 4 GB deployment.

**P1. The conversational controller has no qualified model.** M0-13 rejected all four candidates against its pre-registered bar (the table above shows the
runs repeated under 4g). Without a qualified model the 4g profile serves the deterministic path (typed forms, the rules router, templates) and nothing
else. Clears when a model passes the Lite bar and the 4g row (P2). Owner: M0-17, M5-09.

**P2. The models that fit 4 GB are the ones that fail the quality bar, and the ones that come close do not fit.** Backend peak 704 MiB (350M) and 1489 MiB
(0.8B) are inside the 4g controller row (1650 MiB); the 350M routes 0 of 54 and the 0.8B refuses 0 of 16. The 1B (2498 MiB) and the 2B (2568 MiB) are the
nearest in quality and are 850 to 920 MiB over the row. A model has to be both: at most about 1.65 GiB resident at context 4096 and over the bar. Owner:
M0-17 (a prompt that defines the intents first), M5-09.

**P3. Retrieval (RAG) has no 4 GB shape.** A resident embedder leaves no margin (projected 1900 to 2020 of 2048) and one active backend reloads a model on
every call, 18 s per turn (section above). The only shape left is lexical retrieval (FTS5) at query time and the embedder in a serialised ingestion
window, and neither exists yet: `internal/rag/query` has to build the FTS-only mode labelled `reduced_semantic_recall`, the job queue has to refuse to load
a model while a turn is in flight, and nobody has measured RAG quality without vectors. Owner: M2-09, M2-15, M5-09.

**P4. Voice cannot be resident.** The estimated sizes of a small STT and a TTS pack are 700 to 900 MiB (ADR 0015, upstream sizes, not measured) against 248
MiB of spare in the container. The 4g profile is text-only by definition (7.1), so any application that needs voice is not served. Owner: M4-06.

**P5. Nothing stops an optional model from evicting the controller.** The runtime's policy is least-recently-used eviction at one active backend, which
the measurement above shows as a reload on every call. AlixAI has no admission check for optional models (rule R3 of the budgets), so the first reranker,
decision scorer or language pack that is loaded on a 4g host would silently evict the controller. Owner: whoever adds the first optional model; the
`LOCALAI_MAX_ACTIVE_BACKENDS` guard has no task yet (reported to the orchestrator; it would live in `deploy/container/localai/`, M0-18's scope).

**P6. Latency on four CPUs is close to the line even for the smallest useful model.** Prefill is 106 to 144 tokens per second and decode 12 to 18, so a
proposal prompt at the Lite cap (957 real tokens) waits about 8 s for its first token and a two-call turn takes 15 to 30 s (computed from the measured rates, not measured end to end); two sessions at once queue
(the second waits 15.6 s worst case). The pre-registered line passes for the 0.8B at the bench prompt (p95 14.4 s, TTFT p95 4.1 s) and fails for the
2B (TTFT p95 6.7 s). This is a laptop-class CPU under a 4-CPU quota with other jobs on the host; a real 4 GB board is slower. Owner: M5-09 on real hardware.

**P7. Cold start and readiness.** Starting to ready is 7 to 20 s and the first request after a start 5.8 to 14.6 s on this host with the Windows file cache
warm; M0-13 saw 50 s for a first-ever load, inside the gateway's 60 s call budget by 10 s. `/readyz` says nothing about inference (M0-13 F5) and nothing
warms the model before the service reports ready (F4). Owner: M0-18.

**P8. The memory outside the container is unmeasured, and no 4 GB machine has run anything.** The allowance (1024 MiB for the operating system, the
container runtime, the AlixAI core, the store, a parser worker) is built from caps and dev-box figures and leaves 72 MiB spare; the operating system row
(600 MiB) is an estimate. The only host used was a Windows Docker Desktop VM, which cannot itself be a 4 GB host, and ARM64 was not run. Swap is off only
because the container's cgroup says so; a native install must disable swap and zram itself and nothing enforces it. Owner: M5-09, M5-H04.

**P9. The controller's memory follows the prompt.** `U` was 1012 MiB loaded, 1634 under the bench and 1942 under a 3,046-token prompt, leaving 414 and 106
MiB against the 2048 line. The caps of `resource-budgets.md` keep real prompts under about 960 tokens, but only an enforced cap protects the line, and the
cause of the growth was not isolated. Owner: M3-03 and M1-14 wiring (enforcement), M0-17 (cause).

**P10. The output constraint is not applied.** The gateway sends a `json_schema` that LocalAI v4.10.0 silently drops (M0-13 F1), so a small model free-writes
57 to 248 tokens per proposal and truncates at the 256 cap with 12 capabilities; the 4g output cap is 192. A grammar fixes it and costs CPU and possibly
memory (P9). Owner: M0-12 follow-up, M0-17.

**P11. The full workload has not run on 4g.** Only the 80-case routing corpus did. Not run: the conversation corpus, the RAG evaluation, the multilingual
set, the injection suite, a concurrent indexing run, the 24-hour mixed load and the 7-day soak (7.4; ADR 0020 criteria 1 to 5). Owner: M5-09.

**P12. Quality measurements on the Granite models are noisy.** Greedy decoding gave two answers to one request (section above), so a 4g rung chosen among
Granite candidates needs repeated runs. Owner: M0-17.

What would let the 4 GB profile be called "experimental, emulated pass" (ADR 0020): P1 to P3 and P9 to P11 fixed or accepted with evidence, P5 closed by
a test, P8 reduced to a measurement on a real 4 GB machine or an honest "emulated only" label.

## What was not done

- No real 4 GB machine, no ARM64, no native Linux and no rootless Podman run: everything is a cgroup on a Docker Desktop VM.
- No controller other than the four of M0-13, no larger context than 16384, no quality work on any model (B5), no new prompt.
- No RAG answer prompt, no voice, no reranker and no decision scorer were run; their rows in the budgets are estimates or proposals and say so.
- No CPU frequency, power or temperature measurement.
- The grammar versus prompt-length attribution of the anonymous memory growth, and the cause of the Granite non-determinism, were not isolated.
- The AlixAI core was measured once, idle, on Windows (`core-footprint.json`); no loaded core, no Linux RSS.

## Deviations from the shipped deployment, and how to reproduce

- **The backend was installed from the host.** In-container pulls fail on this machine (Avast re-signs TLS, `x509: certificate signed by unknown
  authority`, as M0-13 recorded), so `quay.io/go-skynet/local-ai-backends@sha256:d7b0d9c6...` was pulled by the host's Docker daemon (the digest is the
  pinned one), exported, extracted into `deploy/container/localai/data/backends/cpu-llama-cpp` and given a **hand-written** `metadata.json` copied from the one LocalAI wrote in M0-13
  (`backend-install-metadata.json`) with a new `installed_at`. The shipped in-container install path was therefore not exercised. Nothing was disabled or bypassed on the host.
- **Only the models of a run are mounted** (`compose.m0-4gb.yaml` and `models/<id>.yaml`), because the spike's `models.yaml` lists six files and LocalAI
  hashes every listed file at every start, which would have read 3.2 GB through the 4 GiB cgroup's page cache before the first request.
- **Docker VM caches are dropped before the cold request**; the Windows host cache is not. The drop is a throw-away `--privileged` alpine container writing
  `/proc/sys/vm/drop_caches`, run once per stack session (about 20 times in this task) in a VM that other agents' containers share; it only costs them a
  cold page cache.
- **Runs made after the bar was committed and discarded**: two dry runs of the harness (the first found the backend missing, the second proved the sampler) and one
  3-case bench trial to check that `bench check` accepts a 4g result; their outputs were deleted and none enters a table. The harness itself was
  committed before the first reported run (`wip(M0-15): sampler, runner, summariser...`).
- **Other jobs ran on the host** during every run (host CPU median 69 to 86 percent, p95 99 to 100, sampled every 5 s in `samples/*-host.csv`; the Docker
  VM itself was about 25 percent busy, which is this container's four CPUs). Latency is indicative.
- The real engine prompts come from a throw-away test run on a copy of the tree in a scratch directory (`prompts/README.md`); nothing was added to `internal/`.

```bash
export SPIKE_ENV_FILE=<env file holding LOCALAI_API_KEY>  BENCH_BIN=<built tools/bench>  SCRATCH=<scratch dir>
S=docs/evidence/m0-4gb/scripts
$S/run-4g.sh qwen3.5-0.8b-q4_k_m qwen3.5-0.8b-q4_k_m-4g     # cold start, cgroup sampler, 80-case bench, idle window, teardown
node $S/summarize.mjs qwen3.5-0.8b-q4_k_m-4g benchmarks/results/m0-4g/<the result file>
$S/chain.sh      # the four controller runs     $S/chain2.sh   # probes A, T, C1, C2, K, D     $S/chain3.sh   # determinism, concurrency, default context
node $S/tables.mjs    # the tables of this file
go run ./tools/bench check benchmarks/results/m0-4g
```

## Files

| Path | What it is |
| --- | --- |
| `docs/operations/resource-budgets.md` | the budgets this experiment feeds |
| `benchmarks/results/m0-4g/` | the four 80-case bench results under `experimental-4g` (checked by `go run ./tools/bench check`) |
| `summary/<tag>.json` | per run: windows (idle, cold load, settle, measured, idle after), memory split, faults, swap, events, CPU, host load, verdicts |
| `samples/<tag>.csv`, `-host.csv`, `.marks` | the raw cgroup and process samples, the host CPU samples, the phase timestamps |
| `samples/*.json` | probe outputs: measured prompts, curves, concurrency, co-residency, determinism |
| `config/<tag>.txt` | the container's limits as the container saw them |
| `logs/` | bench logs, cold-request answers, container logs (the debug-level log of probe D holds only throw-away prompts and was checked for secrets) |
| `prompts/` | the 19 real engine prompts and the throw-away test that dumped them |
| `models/`, `compose.m0-4gb.yaml`, `compose.debug-log.yaml` | per-run model definitions and the compose overrides |
| `scripts/` | sampler, runners, probes, summariser, table generator |
| `token-calibration.json`, `core-footprint.json` | estimator against runtime counts over all bench results; one sample of the idle core process |
