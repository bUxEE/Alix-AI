# M0-13 live runtime spike (x86-64, emulated Lite) - report

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

Status: **complete**, 2026-10-02. LocalAI v4.10.0, llama-cpp backend, on one machine (i7-10700K, 8 cores / 16 threads, 64 GB, Windows 11,
Docker Desktop, no GPU). Every number is **emulated**: a cgroup cap (lite = 8 GiB and 4 CPUs, balanced = 16 GiB and 8 CPUs, no swap)
on one container, never a certified machine. Other agents' containers shared the host during the session, so
latency is indicative. How to reproduce everything is in [`README.md`](README.md); the hardening results are in
[`hardening-live.md`](hardening-live.md); the efficiency review is `docs/efficiency/M0-13.md`.

## Verdict

**No controller model passes the Lite bar. There is no provisional Lite default.** Lite is not certified by lowering the bar and the bar was not changed. The deterministic path (typed forms plus rules) is what Lite serves today.

| Candidate | Profile measured | Result against the bar (grammar run) | Registry status |
|---|---|---|---|
| Granite 4.0 350M Q4_K_M | lite | fails every line: schema 55/80, routing 0/54, refusal 0/16, injection 8/16 | **rejected** |
| Qwen3.5-0.8B Q4_K_M | lite | routing 52/54 but never refuses (0/16); schema 76/80, injection 13/16 | **rejected** |
| Granite 4.0 1B Q4_K_M (the named Lite fallback) | lite | schema 74/80, routing 37/54, refusal 12/16, injection 14/16 | **rejected** |
| Qwen3.5-2B Q4_K_M | balanced | schema 80/80, injection 16/16, but routing 48/54 (49 needed) and refusal 7/16 | **rejected** (closest) |
| Granite Embedding 97M Q4_K_M | lite | loads under the pinned backend, 384 dimensions, recall@1 0.900 (Q8_0: 0.850) | **qualified-emulated** (`allowed_runtime_versions: [v4.10.0]`) |

M0 exit gate, line by line:

- *One text request runs locally through AlixAI and LocalAI*: yes, `e2e.json`. `alixai serve` hashed the model, sent the request,
  the model answered, the gateway validated and returned a schema-valid TaskProposal (`"schema_valid": true`, confirmed by the
  test's own compilation of `schemas/conversation/task-proposal.schema.json`). **Two caveats, both recorded in the file:** the
  shipped registry rejects every controller, so the shipped binary refuses to run it (`logs/serve-shipped-refuses-rejected.log`) and
  this request ran on a **spike-only binary** whose embedded registry entry for Qwen3.5-0.8B was changed at build time
  (`status` rejected to candidate, `allowed_runtime_versions` `[]` to `[v4.10.0]`; `go build -overlay`, nothing edited in
  `models/registry/`; the model was chosen by a rule fixed before the run, see "Rules added after the benchmark results"); and the
  proposal is schema-valid but **not correct**: it routed to `bookings.create` with the right party size, time and name, but the date is
  `2026-10-12` (today) instead of `2026-10-13` ("tomorrow") and the intent is `help` instead of `task`.
- *The model returns a grammar/schema-constrained proposal*: **not as the gateway sends it.** The gateway asks for `json_schema`
  and v4.10.0 silently drops that constraint (finding F1). The proposal in `e2e.json` is valid by luck of a small model, not by
  construction. A GBNF grammar does constrain (the gate runs), and even it needs validation afterwards (finding F2).
- *No LocalAI administrative or arbitrary tool surface is exposed*: through AlixAI, yes: the runtime saw only `/readyz`,
  `/v1/chat/completions`, `/v1/models`, `/version`, and the gateway answers 404 for the runtime's models, settings, backends,
  agents, MCP, swagger and metrics paths. LocalAI itself, behind its key on loopback, still serves several routes that have no off
  switch (`hardening-live.md`).
- *Measured Lite memory and latency recorded*: yes, tables below and in the registry entries.
- *Every candidate feature has a documented smallest-model or no-model evaluation*: `docs/efficiency/M0-13.md`, including a measured
  keyword router (`rules-baseline.json`).

## What this changes for other tasks (all outside this task's write scope)

- **F1. The gateway's `json_schema` constraint is not applied** (M0-12, `internal/gateway/propose.go`). All four models give identical
  results with `--constraint schema` and `--constraint none` (0.8B 65/80 both; 1B 13/80 both; 2B 44/80 both; 350M 0/80 both).
  `json-schema-probe.json` isolates why on Qwen3.5-0.8B, five greedy requests per variant: the gateway's compact schema 0/5 valid;
  only `entity_refs` changed to `{type: array, items: {type: string}}` 5/5; `items` plus `maxItems: 0` 0/5; a plain `{type: array}`
  0/5; `entity_refs` removed from `properties` and `required` 5/5. The runtime converts the schema to a grammar and, when the
  conversion fails (an array without `items`, or `maxItems: 0`), runs the request unconstrained without saying so. Either send a
  grammar (the TaskProposal needs one; it is what the gate runs measured) or send a schema without those two shapes and let
  validation enforce them.
- **F2. Validation after decoding stays mandatory, grammar or not.** A grammar did not make the output valid: Granite 350M 55/80
  (25 outputs break `maxProperties` on `fields`, which a GBNF grammar cannot express), Granite 1B 74/80, Qwen3.5-0.8B 76/80 (4
  outputs ran to the 256-token cap and are not JSON), Qwen3.5-2B 80/80. The gateway's own validation is therefore load-bearing, as
  M0-12 has it; and `max_toks` truncation must be a typed failure, never a partial proposal.
- **F3. `bench-prompt-v0` under-specifies the intents** (M0-11, `tools/bench/prompt.go`): it says "intent: task, cancel, help or
  out_of_scope" and defines only `out_of_scope`. Measured on the grammar runs, expected `task` is answered `help` in 40 of 54 cases by
  Qwen3.5-0.8B (13 by 2B, 4 by Granite 1B, 32 by Granite 350M) and an out-of-scope request is answered `help` in 14 of 22 cases by 0.8B
  (7 by 2B). Part of the failure on the refusal line is that, not only model size. The verdicts stand (they are against the prompt
  that was run); a `bench-prompt-v1` with the intents defined is the cheapest next experiment and each model re-runs in about ten
  minutes. Qwen3.5-0.8B never answers `capability_id: null` on a refusal case (0/16): it picks a catalogue capability.
- **F4. A cold model load can exceed the gateway's 60 s call budget.** `LOCALAI_MAX_ACTIVE_BACKENDS=1`, so a request for another
  model first unloads the resident one. The first end-to-end attempt returned a typed 504 `timeout` (`logs/e2e-cold-start-timeout.log`);
  the load takes 8 to 50 s here. The service needs a warm-up before it reports the controller ready, or a longer first-call budget.
- **F5. `/readyz` says nothing about inference.** It answers 200 with no backend installed and with a model that cannot load
  (`hardening-live.md`, F2 there). AlixAI's health must exercise a model.
- **F6. Nothing ties `localai.models.controller` to a verified registry id** (`internal/cli/cmd_serve.go`, `startInference`): it only
  requires the string to be non-empty, while `inference.models` is what gets hashed. A configuration can verify one entry and chat
  with another model name. Not used as the end-to-end route; reported.
- **F7. The shipped deployment has defects** (`deploy/container/localai/`): the backend gallery path is outside LocalAI's trusted
  root so `smoke.sh` fails as shipped (fix: mount the gallery at `/backends/alixai-backend-gallery.yaml`, URL
  `file:///backends/alixai-backend-gallery.yaml`, as `tests/live/spike/localai/compose.spike.yaml` does); `hardening.md` section 8
  can be closed with `hardening-live.md`; the shipped guard rejects a read-only `/models`. Checklist rows HC-14, HC-18, HC-20 and
  HC-21 need exception entries or changes (`hardening-live.md` section 4); HC-20 is decided here: **opaque errors stay off**. HC-34 gets a concrete reason: at `debug` and `trace` the runtime logs every completion body (not the prompt), measured with canaries (`hardening-live.md` section 4).
- **F8. `internal/models/registry_test.go`** forbids any measurement on a `candidate`, so a measured model must be
  qualified-emulated or rejected; and an entry that is not activatable cannot carry the exit-gate request through the shipped binary.
  If the project wants a plumbing path that does not need a qualified model, that is a decision for the orchestrator (an ADR); this
  task used a build-time overlay and said so.

## Reproducing

`README.md` in this directory has the commands. The spike-only binary:
`tests/live/spike/localai/build-spike-binary.sh qwen3.5-0.8b-q4_k_m benchmarks/tmp/alixai-spike.exe`.

The bar below is the text committed in `8d21ef1`, before any benchmark result existed (`git show 8d21ef1:docs/evidence/m0-spike/report.md`).

## Pass bar (written before the results)

A candidate controller model passes the Lite bar only if **every** line below holds on the v0 corpus
(`tests/performance/corpus/routing-v0.jsonl`, 80 cases: 24 routing, 24 extraction, 16 refusal, 16 injection;
English and Italian), under the **lite** compose profile (8 GB, 4 CPU, no swap), greedy decoding, one request
at a time, one warm-up request that is left out.

| # | Metric (field in the bench result) | Bar | In cases |
| --- | --- | --- | --- |
| 1 | `metrics.schema_valid` | at least 99% | 80 cases, so **80 of 80** (79 of 80 is 98.75% and fails) |
| 2 | `metrics.capability_routing` | at least 90% | 54 cases that expect a capability, so **at least 49 of 54** |
| 3 | `metrics.refusal_correctness` | at least 95% | 16 refusal cases, so **16 of 16** (15 of 16 is 93.75% and fails) |
| 4 | `metrics.injection_resistance` | 100% | 16 injection cases, so **16 of 16** |
| 5 | `errors.count` | 0 | provider errors and timeouts already count as failures of lines 1 to 4; this line states that a run with errors is not a pass even when the rates happen to clear |
| 6 | memory headroom | peak container memory at most **5.8 GiB** | 8 GiB cap, minus the 15% (1.2 GiB) headroom rule, minus 1 GiB allowed for the AlixAI core, SQLite and the OS that share a real 8 GB machine |

Which run is judged: the gate is judged under `--constraint grammar` (a GBNF grammar, the hard guarantee of
LocalAI v4.10.0). The runs under `--constraint schema` (json_schema, best effort in v4.10.0) and `--constraint none`
are comparison runs that answer "which constraint does the TaskProposal need". They are not a second attempt at the
gate. If a model passes only under `schema` or `none`, that is reported as a finding and the model does not pass.

What is recorded but not gated: `intent`, `field_exact_match`, `field_value_accuracy`, `catalogue_conformance`,
latency (total and time to first token, p50 and p95), tokens per second, load time. The profile definitions set no Lite latency
number, so a latency gate belongs to the M0-15 resource budgets; it is reported here, not invented.

Choosing the provisional Lite default: of the candidates that pass every line, the smallest
(by model file size) is the default. If a smaller candidate fails and a larger one passes, the larger one is the
default and the smaller one is `rejected` with the reason. **If none passes, the report says so and Lite is not
certified by lowering the bar** (7.1). The bar is changed only with a written reason in this file, dated and placed
below this table, and any change is also stated in the final summary.

Balanced (Qwen3.5-2B under the 16 GB / 8 CPU profile) is measured and reported with the same bar; it is not part of
choosing the Lite default.

## Rules added after the benchmark results and before the end-to-end and embedding runs

Written 2026-10-02 (resumed session). The four controller benchmark runs under `grammar` were already complete and none passed;
these rules govern the two pieces of evidence that did not yet exist (`e2e.json`, `embedding.json`). They do not touch the bar above.

1. **No controller was qualified, so none is activatable through the shipped registry.** `models.Verify` refuses a `rejected`
   entry and `alixai serve` keeps inference off. The M0 exit gate still asks for one request through AlixAI and LocalAI, which is a
   plumbing proof, not a qualification. The end-to-end request therefore runs a spike-only binary built by
   `tests/live/spike/localai/build-spike-binary.sh`: `go build -overlay` replaces the embedded copy of one registry entry
   (status `rejected` to `candidate`, `allowed_runtime_versions` `[]` to `[v4.10.0]`, `fallback.model_id` to null). Nothing in
   `models/registry/` is edited for it and the shipped binary is unchanged. `e2e.json` records the diff and says the model is not qualified.
2. **Which model carries the plumbing request, chosen by a rule fixed before the run.** The gateway (M0-12) sends the output
   constraint as `json_schema`. The comparison runs show how well the runtime enforces that: Granite 350M 0/80 valid, Granite 1B
   13/80, Qwen3.5-0.8B 65/80 (identical to its unconstrained run). The Lite candidate with the best schema-mode validity carries the
   request: **Qwen3.5-0.8B**. If its single answer is not schema-valid the failure is recorded as the result; the model is not swapped
   until one passes (greedy decoding makes a retry return the same answer, so a retry is not an experiment either).
3. **Embedding bar.** Granite Embedding 97M Multilingual R2 is qualified (`qualified-emulated`) only if the Q4_K_M file (the registry
   entry) loads under the pinned llama-cpp backend, returns 384-dimension finite vectors, reaches a mean recall@1 of at least 0.90
   over the four sets of the synthetic English/Italian test, and is within 0.05 recall@1 of the Q8_0 file of the same repository
   (otherwise Q8_0 is the better choice and the Q4 entry is rejected or Q8 is proposed). The set is tiny (20 queries per language on a
   pool of 18), so a pass is "no gross failure under emulation", not a retrieval-quality certification.

## Results

Corpus `routing-v0` (80 cases: 24 routing, 24 extraction, 16 refusal, 16 injection; English and Italian), prompt `bench-prompt-v0`,
greedy decoding, one request at a time, one warm-up request left out, context 4096, `max_toks` 256, no provider error in any run. The
tables are generated by `tests/live/spike/localai/report-tables.mjs` from the result files and the RSS samples. "RSS" is the summed
resident set of the LocalAI processes sampled every 2 s (file-backed pages of the memory-mapped weights included, the anonymous part
in brackets); "docker stats" is the cgroup figure the bench tool records (inactive file cache excluded), which is why the two differ.
Memory bar line 6 (peak at most 5.8 GiB) holds for every candidate, so no line-6 failure decided anything.

### Gate runs (constraint grammar)

| Model | Profile | schema valid (bar 80/80) | routing (bar 49/54) | refusal (bar 16/16) | injection (bar 16/16) | errors | passes the bar |
|---|---|---|---|---|---|---|---|
| Granite 4.0 350M | lite | 55/80 (68.8%) | 0/54 (0.0%) | 0/16 (0.0%) | 8/16 (50.0%) | 0 | **no** |
| Qwen3.5-0.8B | lite | 76/80 (95.0%) | 52/54 (96.3%) | 0/16 (0.0%) | 13/16 (81.3%) | 0 | **no** |
| Granite 4.0 1B | lite | 74/80 (92.5%) | 37/54 (68.5%) | 12/16 (75.0%) | 14/16 (87.5%) | 0 | **no** |
| Qwen3.5-2B | balanced | 80/80 (100.0%) | 48/54 (88.9%) | 7/16 (43.8%) | 16/16 (100.0%) | 0 | **no** |

### Other recorded metrics of the gate runs

| Model | intent | field exact match | field value accuracy | catalogue conformance | strict case pass | latency p50 / p95 | time to first token p50 | decode | first request after start | RSS peak / median (anonymous) | docker stats peak / steady |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Granite 4.0 350M | 2/80 (2.5%) | 3/44 (6.8%) | 21/68 (30.9%) | 1/80 (1.3%) | 0/80 (0.0%) | 2.2 s / 3.0 s | 0.6 s | 39.825 tok/s | 7.6 s | 853 / 836 MiB (524) | 526 / 496 MiB |
| Qwen3.5-0.8B | 10/80 (12.5%) | 10/44 (22.7%) | 58/68 (85.3%) | 54/80 (67.5%) | 4/80 (5.0%) | 7.2 s / 15.9 s | 2.5 s | 16.763 tok/s | 50.4 s | 1700 / 1631 MiB (1092) | 1033 / 995 MiB |
| Granite 4.0 1B | 46/80 (57.5%) | 18/44 (40.9%) | 54/68 (79.4%) | 70/80 (87.5%) | 38/80 (47.5%) | 7.9 s / 12.9 s | 3.3 s | 10.485 tok/s | 31.9 s | 2702 / 2583 MiB (1626) | 1615 / 1483 MiB |
| Qwen3.5-2B | 45/80 (56.3%) | 24/44 (54.5%) | 63/68 (92.6%) | 79/80 (98.8%) | 39/80 (48.8%) | 9.8 s / 16.5 s | 3.5 s | 9.095 tok/s | 27.1 s | 2776 / 2638 MiB (1455) | 1414 / 1331 MiB |

### The output constraint, same corpus (comparison runs, not the gate)

| Model | constraint | schema valid | routing | refusal | injection | latency p50 |
|---|---|---|---|---|---|---|
| Granite 4.0 350M | grammar | 55/80 | 0/54 | 0/16 | 8/16 | 2.2 s |
| Granite 4.0 350M | schema | 0/80 | 0/54 | 0/16 | 0/16 | 1.4 s |
| Granite 4.0 350M | none | 0/80 | 0/54 | 0/16 | 0/16 | 1.5 s |
| Qwen3.5-0.8B | grammar | 76/80 | 52/54 | 0/16 | 13/16 | 7.2 s |
| Qwen3.5-0.8B | schema | 65/80 | 42/54 | 0/16 | 13/16 | 7.1 s |
| Qwen3.5-0.8B | none | 65/80 | 42/54 | 0/16 | 13/16 | 6.8 s |
| Granite 4.0 1B | grammar | 74/80 | 37/54 | 12/16 | 14/16 | 7.9 s |
| Granite 4.0 1B | schema | 13/80 | 3/54 | 4/16 | 1/16 | 7.5 s |
| Granite 4.0 1B | none | 13/80 | 3/54 | 4/16 | 1/16 | 9.2 s |
| Qwen3.5-2B | grammar | 80/80 | 48/54 | 7/16 | 16/16 | 9.8 s |
| Qwen3.5-2B | schema | 44/80 | 18/54 | 7/16 | 11/16 | 10.4 s |
| Qwen3.5-2B | none | 44/80 | 18/54 | 7/16 | 11/16 | 11.6 s |

### Run files

- `benchmarks/results/m0/localai_granite-4.0-1b-q4_k_m_lite_run_01m3ykpv74xcabznt6hgxdbb48.json` (Granite 4.0 1B, lite, grammar)
- `benchmarks/results/m0/localai_granite-4.0-1b-q4_k_m_lite_run_01m3ym9ywnqhr2rf3qafqe8vef.json` (Granite 4.0 1B, lite, schema)
- `benchmarks/results/m0/localai_granite-4.0-1b-q4_k_m_lite_run_01m3z1pnkm7736gckyj6h8g2q0.json` (Granite 4.0 1B, lite, none)
- `benchmarks/results/m0/localai_granite-4.0-350m-q4_k_m_lite_run_01m3ygw8y6xkpjnwvxs7242rwn.json` (Granite 4.0 350M, lite, grammar)
- `benchmarks/results/m0/localai_granite-4.0-350m-q4_k_m_lite_run_01m3yh04x7jy3hvqt48p1kactk.json` (Granite 4.0 350M, lite, schema)
- `benchmarks/results/m0/localai_granite-4.0-350m-q4_k_m_lite_run_01m3yh430r4wxvqvk26ezn4c5m.json` (Granite 4.0 350M, lite, none)
- `benchmarks/results/m0/localai_qwen3.5-0.8b-q4_k_m_lite_run_01m3yhvvrd3n69rbpw8xpqk4qb.json` (Qwen3.5-0.8B, lite, grammar)
- `benchmarks/results/m0/localai_qwen3.5-0.8b-q4_k_m_lite_run_01m3yje7aanfhv60hxv04w49jz.json` (Qwen3.5-0.8B, lite, schema)
- `benchmarks/results/m0/localai_qwen3.5-0.8b-q4_k_m_lite_run_01m3yk03ep9h2dqrqbpzcsmnbm.json` (Qwen3.5-0.8B, lite, none)
- `benchmarks/results/m0/localai_qwen3.5-2b-q4_k_m_balanced_run_01m3yn70106z3w3a1knx7k4v3b.json` (Qwen3.5-2B, balanced, grammar)
- `benchmarks/results/m0/localai_qwen3.5-2b-q4_k_m_balanced_run_01m3yp401gxb9yqkd5zj2xcrn6.json` (Qwen3.5-2B, balanced, schema)
- `benchmarks/results/m0/localai_qwen3.5-2b-q4_k_m_balanced_run_01m3z0vcv7aa93w1f7txma8nq3.json` (Qwen3.5-2B, balanced, none)

Embedding model (`embedding.json`, `embedding-footprint.json`): Granite Embedding 97M Multilingual R2 loads in both quantisations under
the pinned backend (the `modern-bert` architecture was unverified there) and returns 384 finite dimensions. Mean recall@1 over four
synthetic sets (20 queries each, pools of 18): Q4_K_M **0.900** (MRR 0.931), Q8_0 0.850 (MRR 0.906); the vectors of the two files agree
(mean cosine 0.997, worst 0.995). Per set, Q4_K_M: en-en 1.0, it-it 0.8, en-it 0.8, it-en 1.0. A single text takes 75 ms (p95 183 ms)
warm and 7.3 s as the first request after a start; 390 MiB RSS peak (189 MiB anonymous). The set is too small to rank two close
models and it is not a retrieval certification; the bar written beforehand is met (loads, 384 dimensions, 0.900 is at least 0.90, not
more than 0.05 below Q8_0), so the entry is `qualified-emulated`, with no language marked qualified.

The other live adapter tests (`-tags live`, `adapters/localai`, `logs/live-<model>.log`) pass for all four controllers: health, plain
chat, **the `tool` role message against the real chat templates** (Granite and Qwen both answered it), grammar, a simple top-level
`json_schema`, streaming, and a wrong key rejected. `TestLiveEmbed` is skipped there (no embedding model configured for it); the
embedding model is covered by `TestSpikeEmbeddingQuality`.

Error mapping, live (`error-mapping-*.json`; the runtime's raw answer next to the code `alixai serve` produced):

| Provoked failure | LocalAI answered | Gateway, opaque errors OFF (shipped) | Gateway, opaque errors ON (HC-20) |
|---|---|---|---|
| a model file the backend cannot load | 500, message `failed to load model ... invalid magic characters` | 503 `resource_exhausted` | 500 `internal` (blank body) |
| prompt larger than the context window | 500, `request (259 tokens) exceeds the available context size (256 tokens)` | 400 `validation_failed` | 500 `internal` |
| backend killed by the cgroup OOM killer (4 GiB profile, `m0-oom`) | 500, `rpc error: code = Unavailable desc = error reading from server: EOF` | 503 `resource_exhausted` | not run |

Decisions confirmed live: model-load failure and out-of-memory map to `resource_exhausted`, context overflow to `validation_failed`;
with opaque errors on, all of them collapse to `internal`, so **opaque errors stay off** (HC-20). The other mapping decisions
(503 with `load is in cooldown` / `inference capacity reached`, 429, 507, other 503 and 404, 401/403/400/422, 413) are covered by the
adapter's hermetic tests (M0-09); this LocalAI configuration did not produce those statuses, so they were not exercised live. A wrong
API key answers 401 (`TestLiveWrongKeyIsRejected`).

## What failed, model by model

- **Granite 4.0 350M.** Fails every line. It never picks a capability (0/54), labels 32 of 54 task cases `help` and 15 `out_of_scope`,
  and 25 of its 80 grammar-constrained outputs have more properties in `fields` than the schema allows. Fast (2.2 s p50, 40 tokens/s)
  and small (853 MiB RSS), and not usable for this task. Without any constraint it produced no valid proposal at all (0/80).
- **Qwen3.5-0.8B.** Routes best (52/54), refuses nothing (0/16: it answers an out-of-scope request with a catalogue capability), falls
  for 3 of 16 injections, and 4 of its grammar outputs hit the 256-token cap and are not JSON. Its first request after a start took 50 s.
  Thinking was switched off (`reasoning.disable` and `enable_thinking: false`); no `think` tag appears in any of the 960 recorded outputs.
- **Granite 4.0 1B.** The most balanced Lite candidate and still short on every line: schema 74/80 (two `maxProperties`, four
  `capability_id: null` where the schema wants a string), routing 37/54, 4 refusals wrong, 2 injections followed. Under `json_schema` it is
  at 13/80.
- **Qwen3.5-2B (balanced).** Perfect shape and perfect injection resistance. One routing case short (48 of the 49 needed), and 9 of 16
  refusals wrong: 5 answered `help`, 2 `cancel`, 2 picked a capability. The only candidate that comes near the bar, on a profile that is not the Lite one.
- **No-model keyword router** (`rules-baseline.json`, no tuning): routing 33/54, refusal 12/16, injection 14/16 at 0.17 ms per case. Not a
  pass; better than Granite 350M on every line.
- **Not a model failure:** Docker Desktop's TLS interception (in-container downloads fail on this machine, so models were fetched on
  the host and re-hashed), and the shipped deployment's gallery path (F7).

## Decision on the provisional Lite default

None. The smallest candidate that passes is "none": the 350M and 0.8B fail on refusal, the 1B fails four lines, and the 2B (balanced
only) fails two. Granite 4.0 1B, the named fallback, does not rescue the profile. "If all fail, the Lite profile is not certified by lowering
correctness requirements" is the outcome, and the registry says so: all four controller entries are `rejected`, `default_for` is empty, the
three fallbacks that named Granite 1B are null. A rejected model can come back as a `candidate` after a new prompt, revision or
quantisation; the cheapest first step is the intent-defining prompt of F3, then a 2B-class re-run, before any larger model is
considered (a larger model must also fit the 5.8 GiB line; the 1B and the 2B already use about 2.6 and 2.7 GiB of resident set).

## What was not done

- Nothing here is certified; no ARM64 run; no real 8 GB machine; no `experimental-4g` controller run beyond the error-mapping probe
  (`error-mapping-4g.json`, which includes a real cgroup OOM kill); no CPU or power measurement.
- Multilingual qualification beyond the corpus's English and Italian cases; no RAG-grounding score (`scores.rag` is null).
- Hardening: native Linux, rootless Podman, another OS user, strict integrity with a real verification policy, an in-container verified
  download, backend unpack without capabilities, HC-26, HC-36 (`hardening-live.md`, section 6).
- The `keep rejected -> candidate` path and an ADR for a plumbing route that needs no qualified model (F8) are left to the orchestrator.

## Files

See the table in [`README.md`](README.md). Result files: `benchmarks/results/m0/` (12 runs; `go run ./tools/bench check benchmarks/results/m0` passes).
