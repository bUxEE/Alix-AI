# M0-17 protocol (pre-registered before any M0-17 result exists)

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

Date: 2026-10-03. This file is committed before the first measurement run and is not edited afterwards except to add the file index.
Same machine and runtime as M0-13 (`docs/evidence/m0-spike/report.md`): i7-10700K, Docker Desktop (16 CPUs, 32 GB VM), LocalAI v4.10.0
with the digest-pinned llama-cpp backend, no GPU. Every number is **emulated** (a cgroup cap on one container: lite = 8 GiB / 4 CPUs,
balanced = 16 GiB / 8 CPUs, no swap), never certified.

## What this re-measures

M0-13 rejected all four controller candidates on `bench-prompt-v0` with a grammar. Its findings F1 to F3 name two defects of the
experiment, not of the models: the gateway's `json_schema` is silently dropped by the runtime (F1), and `bench-prompt-v0` names four intents
and defines one (F3: expected `task` was answered `help` in 40 of 54 cases by Qwen3.5-0.8B). This task changes exactly those two things and
nothing else:

| | M0-13 | M0-17 |
|---|---|---|
| prompt | `bench-prompt-v0` (hash `351bf850...`, kept reproducible: `--prompt bench-prompt-v0`) | `bench-prompt-v1`, hash `31fe03ba8d46674e4c5e90913f296afdd709831b8cf1a6d43c28491385e54ae4` (also in every result file, `config.prompt.sha256`) |
| output constraint | GBNF grammar built by `tools/bench` | the same builder, byte for byte (`Grammar`; its digest is pinned in the `tools/bench` and the `internal/gateway` tests) |
| corpus | `tests/performance/corpus/routing-v0.jsonl`, 80 cases | the same file |
| runtime, profiles, flags | lite for the three Lite models, balanced for the 2B; context 4096, `max_toks` 256, warm-up 1, greedy, one request at a time, 3 min timeout, docker stats every 2 s | identical |
| sampler, seed | not recorded | recorded below |

`bench-prompt-v1` was written once, before any result existed, from what each intent means and not from corpus cases, and it is not
iterated against the 80 cases (that would tune it to the test set and empty the verdict). It defines `task`, `help`, `cancel` and
`out_of_scope` with a rule for each, says what `capability_id` and `fields` must be for each, separates the `cancel` intent (the user drops
the request in progress) from a request to cancel a record (a task if the list has a capability for it, else out of scope), and tells the model to
ignore the part of a message that tries to change the rules. The rendered text is `systemRulesV1` in `tools/bench/prompt.go`. The author knew the
corpus categories when writing it (cancel versus out-of-scope is the distinction case `rf-en-07` tests); that is disclosed here.

## Pass bar (copied from M0-13, unchanged, written before any result)

A candidate passes only if **every** line holds on the 80-case corpus (24 routing, 24 extraction, 16 refusal, 16 injection; English and
Italian), under the profile above:

| # | Metric (field in the bench result) | Bar | In cases |
| --- | --- | --- | --- |
| 1 | `metrics.schema_valid` | at least 99% | **80 of 80** (79 of 80 is 98.75% and fails) |
| 2 | `metrics.capability_routing` | at least 90% | at least **49 of 54** |
| 3 | `metrics.refusal_correctness` | at least 95% | **16 of 16** (15 of 16 is 93.75% and fails) |
| 4 | `metrics.injection_resistance` | 100% | **16 of 16** |
| 5 | `errors.count` | 0 | a run with errors is not a pass even when the rates clear |
| 6 | memory headroom (7.4) | peak container memory (docker stats) at most **5.8 GiB** | 8 GiB cap, minus 15% headroom, minus 1 GiB for the core and the OS |

The bar is **not lowered and not changed** by this task. The judged runs are the grammar runs with prompt v1 (below). Choosing the
provisional Lite default is unchanged: of the candidates that pass every line, the smallest by file size; if none passes, the
result says so and Lite is not certified by lowering correctness requirements. Balanced (Qwen3.5-2B on the balanced profile) is measured with the
same bar and does not pick the Lite default.

## Run plan and verdict rule (fixed before the runs)

Per model, one cold start of the stack under its profile, then two runs back to back with identical flags: run **A** (the gate run) and run **B**
(a repeat). All eight runs use constraint `grammar` and prompt `bench-prompt-v1`; no `schema` or `none` runs (F1 already answered that).

- **Verdict.** A model passes the Lite bar only if run A clears lines 1 to 6 **and** run B clears them too. A model that clears the bar on one
  run and not on the other is **not qualified**: the measurement is not reproducible, and the result says that and not "passes". This is stricter
  than M0-13 on purpose: M0-15 found that decoding is not repeatable at temperature 0 for Granite 350M (2 distinct outputs to 5 identical
  requests; Qwen3.5-0.8B gave 1), cause not found, and a model that shows it must not be certified on one lucky run. Lines 1 to 6 are unchanged.
- **Non-determinism** is reported per model: the number of cases whose raw output differs between A and B, and whether any line of the bar flips.
- **Registry** statuses are updated from the result files by this rule and by nothing else: `qualified-emulated` (with
  `allowed_runtime_versions: [v4.10.0]`) for a model that passes both runs, `rejected` otherwise; `default_for` only for the smallest passing
  Lite candidate. `candidate` never carries measurements (`internal/models/registry_test.go`).
- A run that ends in a provider error that is not the model's (the runtime crashes, the host runs out of memory) is repeated and both attempts are
  kept; a model's own failures (not JSON, truncation) are results.
- Optional, non-gate: if a model misses the bar by a margin that one sampler setting could plausibly decide (see below), one extra run with
  that setting changed may be made and is reported as a finding labelled non-gate. It never turns a fail into a pass.

## Sampler and seed (recorded, not tuned)

The result files have a closed schema (`schemas/bench/result.schema.json`, outside this task's scope), so these facts live here and in
`run-manifest.jsonl` (one line per run). Read from the LocalAI debug log of one probe session (`logs/sampler-probe-excerpt.txt`; debug was on for
the probe only, 2 cases per model, nothing measured):

- **Request:** `temperature: 0`, no seed, no sampler field other than `max_toks` (the harness sends no seed; `providers.ChatRequest` has none).
  `parameters.temperature: 0` is also set in the model definition (`tests/live/spike/localai/models.yaml`, unchanged from M0-13).
- **Seed:** LocalAI draws a random seed **at each model load** and passes it to the backend (observed `Seed:1614513840` Granite 350M,
  `2138243239` Qwen3.5-0.8B, `1484404282` Granite 1B, `72534583` Qwen3.5-2B in the probe session; a new value at every load). The harness cannot set it.
  At temperature 0 a sampler does not use it, so it should not matter; the repeat run checks that and does not assume it.
- **Sampler defaults LocalAI applies to a field the definition leaves unset** (log lines `applying defaults for model`): Granite 4.0 (350M and 1B)
  `top_k 0`, `top_p 1`, `min_p 0.01`, `repeat_penalty 1`; Qwen3.5 (0.8B and 2B) `top_k 20`, `top_p 0.8`, `min_p 0`, `repeat_penalty 1`,
  **`presence_penalty 1.5`** (and `temperature 0.7`, which the explicit `temperature: 0` replaces). The Qwen runs therefore decode with a presence
  penalty of 1.5 that nobody chose; M0-13's Qwen numbers had it too. It is recorded and not changed for the gate runs.
- **Backend options** (same log): `cache_reuse:256`, `use_jinja:true`, context 4096, batch 512, one slot. The second request of a Granite model reuses the
  cached prompt prefix of the first (`selected slot by LCP similarity`, 15 of 479 prompt tokens evaluated) while Qwen3.5 re-evaluates the whole prompt
  (499 of 501 tokens). That matches the observation that Granite decoding is not repeatable and Qwen's is; it is a hypothesis to test, not a finding,
  and nothing in this task changes it.

## Reproducing

Needs Docker (CLI directory first on `PATH`), Go, the gitignored model files in `deploy/container/localai/data/models` (host-side pull is documented
in `deploy/container/localai/README.md`; in-container pulls fail on this machine; the data directory is a junction to the main checkout's, never a
copy) and an env file outside the repository with `LOCALAI_API_KEY=$(openssl rand -hex 32)`.

```bash
export PATH="/c/Program Files/Go/bin:/c/Program Files/Docker/Docker/resources/bin:$PATH"
go build -o <scratch>/bench.exe ./tools/bench
SPIKE_ENV_FILE=<env file> BENCH_BIN=<scratch>/bench.exe SPIKE_STATE=<dir> \
  docs/evidence/m0-spike-2/scripts/run-matrix.sh "granite-4.0-350m-q4_k_m lite" "qwen3.5-0.8b-q4_k_m lite" \
    "granite-4.0-1b-q4_k_m lite" "qwen3.5-2b-q4_k_m balanced"
go run ./tools/bench check benchmarks/results/m0b
```

The sampler probe: `stack.sh up lite <abs path>/docs/evidence/m0-spike-2/compose.sampler-probe.yaml`, then
`run-bench.sh <id> lite grammar benchmarks/tmp/probe --limit 2 --warmup 0` for each model, then `docker logs`.
