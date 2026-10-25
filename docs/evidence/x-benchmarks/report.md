# Connected-mode benchmark and provider comparison (X-23)

Written by `go run ./tools/providercompare`. The 4 GB/lightweight profile is benchmarked separately in local and connected modes.

## 0. Read this first

- **No provider was called.** There is no real key, no real money and no external network in this report. Real provider numbers are owed to X-H02 (human-gated, `//go:build live`).
- **Two labels only.** `recorded` is read from an artefact in this repository (a shipped provider profile, a recorded wire fixture replayed through the real adapter, or a benchmark result of an earlier task). `emulated` is produced here: by the latency-injecting loopback stub, inside the emulated 4 GiB container, or computed from recorded inputs on an assumed turn. Every table row ends in its label; a figure that does not exist is written `not measured`.
- **Local mode is reported first and separately, and its verdicts are unchanged.** Connected-mode numbers qualify nothing: they do not make the Lite profile or the 4 GB profile certifiable and they do not offset a failed local gate (external mode must never be used to hide that the local mode has failed a hardware gate).
- **Quality of the hosted models is `not measured`.** What the replay measures is each adapter's wire path, never a model.

## 1. Local mode (recorded; unchanged by this task)

Source: `benchmarks/results/m5/` (M5-09), 80 cases of routing-v0 per file, real LocalAI v4.10.0 runs in cgroup-capped containers (the files themselves say `emulated`). Two files per model and profile are two runs (one without and one with the deterministic scope gate); the files do not say which, see `docs/evidence/m5-models/report.md`.

| Model | Profile | Pass (of 80) | Routing | Refusal | Injection | Total p50 ms | Total p95 ms | Peak RSS MiB | Label |
|---|---|---|---|---|---|---|---|---|---|
| granite-4.0-1b-q4_k_m | lite | 48/80 | 48/54 | 10/16 | 14/16 | 12153.5 | 22541.2 | 1511.4 | recorded |
| granite-4.0-1b-q4_k_m | lite | 47/80 | 45/54 | 13/16 | 15/16 | 9700.9 | 21134.0 | 1523.7 | recorded |
| granite-4.0-350m-q4_k_m | experimental-4g | 3/80 | 4/54 | 0/16 | 2/16 | 2485.3 | 3291.0 | 541.4 | recorded |
| granite-4.0-350m-q4_k_m | experimental-4g | 14/80 | 4/54 | 10/16 | 4/16 | 2169.1 | 3252.2 | 549.7 | recorded |
| granite-4.0-350m-q4_k_m | lite | 3/80 | 4/54 | 0/16 | 2/16 | 3011.1 | 4845.4 | 562.3 | recorded |
| granite-4.0-350m-q4_k_m | lite | 14/80 | 4/54 | 10/16 | 4/16 | 2837.9 | 7794.2 | 571.6 | recorded |
| minicpm5-1b-q4_k_m | lite | 20/80 | 26/54 | 1/16 | 12/16 | 15540.3 | 24250.0 | 782.9 | recorded |
| minicpm5-1b-q4_k_m | lite | 27/80 | 24/54 | 10/16 | 12/16 | 13817.9 | 22625.1 | 784.6 | recorded |
| qwen3.5-0.8b-q4_k_m | experimental-4g | 31/80 | 51/54 | 0/16 | 13/16 | 6625.3 | 13073.8 | 1466.4 | recorded |
| qwen3.5-0.8b-q4_k_m | experimental-4g | 36/80 | 47/54 | 10/16 | 13/16 | 6403.0 | 11069.3 | 1489.9 | recorded |
| qwen3.5-0.8b-q4_k_m | lite | 31/80 | 51/54 | 0/16 | 13/16 | 12117.7 | 27712.3 | 1481.7 | recorded |
| qwen3.5-0.8b-q4_k_m | lite | 36/80 | 47/54 | 10/16 | 13/16 | 8907.2 | 20932.5 | 1501.2 | recorded |
| qwen3.5-2b-q4_k_m | balanced | 44/80 | 47/54 | 9/16 | 15/16 | 9089.7 | 13843.1 | 1843.2 | recorded |
| qwen3.5-2b-q4_k_m | balanced | 46/80 | 44/54 | 12/16 | 15/16 | 9806.7 | 15646.7 | 1859.6 | recorded |

**Verdicts, quoted from the tasks that reached them (not re-derived here):**

- **M0-17** and **M0-21** (`docs/efficiency/M0-17.md`, `docs/efficiency/M0-21.md`): with a grammar-constrained gateway and then with the deterministic out-of-scope gate, no Lite controller candidate cleared the pre-registered bar.
- **M5-09** (`docs/efficiency/M5-09.md`, `docs/evidence/m5-models/report.md`): "No candidate passes the Lite bar, for the fourth time, and the bar was not lowered." Lite stays uncertified and serves the deterministic path; the 4 GB profile is not promoted (Qwen3.5-0.8B has the only usable routing and sits at 2047 MiB against a 2048 MiB line).
- **No Lite controller qualified. Nothing in sections 2 and 3 changes that.**

## 2. Provider comparison (connected mode)

Four real adapters (`adapters/external/{openai,anthropic,gemini,meta}`), built the way the composition root builds them (egress allowlist dialer, placeholder credential), each replaying recorded wire fixtures from a loopback stub. The fixtures were written from each vendor's documented shapes; they are not recordings of a live call.

### 2.1 Capability (recorded: shipped provider profiles)

| Provider | Model | Streaming | Structured output | Tools | Strict args | Parallel | Max tools | Remote MCP | Native features | Image gen | Voice | Max context toks | Max output toks | Label |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| openai | gpt-5-mini | yes | strict_schema | yes | yes | yes | 32 | yes | agents, file_search | no | none | 400000 | 128000 | recorded |
| anthropic | claude-haiku-4-5 | yes | strict_schema | yes | yes | yes | 20 | yes | agents, hosted_memory, server_tool_loops | no | none | 200000 | 32000 | recorded |
| gemini | gemini-3.5-flash-lite | yes | strict_schema | yes | yes | yes | 32 | no | file_search, server_tool_loops | no | none | 1048576 | 65536 | recorded |
| meta | muse-spark-1.3 | yes | schema_best_effort | yes | no | yes | 32 | no | server_tool_loops | no | none | 1048576 | 32768 | recorded |

Data handling, as the profiles record it (all four are `unknown` until a human reads the terms, X-H01):

| Provider | Retention | Training use | Residency | Requests per min | Retries (max attempts) | Label |
|---|---|---|---|---|---|---|
| openai | unknown | unknown | unknown | 500 | 2 | recorded |
| anthropic | unknown | unknown | unknown | 1000 | 2 | recorded |
| gemini | unknown | unknown | unknown | unspecified | 2 | recorded |
| meta | unknown | unknown | unknown | 3000 | 2 | recorded |

### 2.2 Cost

Prices are the recorded price tables of the shipped profiles (micro-units of the currency per million tokens), which are themselves "as summarised, not verified" in the efficiency reviews of X-19 to X-21. The costs below are **computed by the adapters** from those tables on an assumed turn, so they are `emulated`.

| Provider | Model | Currency | Input micro per 1M toks | Output micro per 1M toks | Price table effective on | Label |
|---|---|---|---|---|---|---|
| openai | gpt-5-mini | USD | 250000 | 2000000 | 2026-10-01 | recorded |
| anthropic | claude-haiku-4-5 | USD | 1000000 | 5000000 | 2026-10-07 | recorded |
| gemini | gemini-3.5-flash-lite | USD | 300000 | 2500000 | 2026-10-08 | recorded |
| meta | muse-spark-1.3 | USD | 1250000 | 4250000 | 2026-10-08 | recorded |

| Provider | One turn (1500 in, 200 out) | routing-v0 replay | conversation-v1 replay | rag-questions replay | Label |
|---|---|---|---|---|---|
| openai | 0.000775 USD | 0.012623 USD (23079 in, 3413 out toks) | 0.018259 USD (41475 in, 3928 out toks) | 0.001584 USD (2484 in, 475 out toks) | emulated |
| anthropic | 0.002500 USD | 0.040144 USD (23079 in, 3413 out toks) | 0.061115 USD (41475 in, 3928 out toks) | 0.004859 USD (2484 in, 475 out toks) | emulated |
| gemini | 0.000950 USD | 0.015512 USD (23079 in, 3413 out toks) | 0.022326 USD (41475 in, 3928 out toks) | 0.001959 USD (2484 in, 475 out toks) | emulated |
| meta | 0.002725 USD | 0.043405 USD (23079 in, 3413 out toks) | 0.068608 USD (41475 in, 3928 out toks) | 0.005148 USD (2484 in, 475 out toks) | emulated |

Token counts of the replays are `providers.EstimateTokens` of the text sent and of the reply (what the stub reports as usage), so they are an estimate and the cost an estimate on top of it. The lowest cost is not the best provider: no quality figure exists to weigh it against.

### 2.3 Quality: not measured

Hosted-model quality on routing-v0, conversation-v1 and the RAG questions is `not measured` for every provider: it needs a real key (X-H02). The stub plays the corpus's own expected answer, so the replay below measures whether an adapter **carries a reply through unchanged** (wire fidelity: request accepted, reply decoded, JSON equal), not whether any model would write that reply.

| Provider | Corpus | Cases | Sent | Not sent (decided locally) | Decoded unchanged | Refused by the adapter | Label |
|---|---|---|---|---|---|---|---|
| openai | routing-v0 | 80 | 80 | 0 | 80 | 0 | emulated |
| openai | conversation-v1 | 84 | 84 | 0 | 84 | 0 | emulated |
| openai | rag-questions | 50 | 35 | 15 | 35 | 0 | emulated |
| anthropic | routing-v0 | 80 | 80 | 0 | 80 | 0 | emulated |
| anthropic | conversation-v1 | 84 | 84 | 0 | 84 | 0 | emulated |
| anthropic | rag-questions | 50 | 35 | 15 | 35 | 0 | emulated |
| gemini | routing-v0 | 80 | 80 | 0 | 80 | 0 | emulated |
| gemini | conversation-v1 | 84 | 84 | 0 | 84 | 0 | emulated |
| gemini | rag-questions | 50 | 35 | 15 | 35 | 0 | emulated |
| meta | routing-v0 | 80 | 80 | 0 | 80 | 0 | emulated |
| meta | conversation-v1 | 84 | 84 | 0 | 84 | 0 | emulated |
| meta | rag-questions | 50 | 35 | 15 | 35 | 0 | emulated |

conversation-v1 contributes the turns its own expectations assign to the model (`decided_by: model`); the other turns are decided by the client, a rule or the router and never reach a provider. For comparison, the same corpus on a real local model is in section 1 (routing-v0 only; conversation-v1 and the RAG answer evaluation were gated out there by protocol rule G).

### 2.4 Latency

Provider latency is `not measured` (X-H02). What can be said is the cost the adapter and the egress dialer add on this development box against a loopback stub with **no** injected delay; it is the floor under any network latency and it is the same order for every provider because each adapter does the same kind of work.

| Provider | Corpus | Adapter overhead p50 ms | Adapter overhead p95 ms | Mean request bytes | Label |
|---|---|---|---|---|---|
| openai | routing-v0 | 4.570 | 6.323 | 1198.9 | emulated |
| openai | conversation-v1 | 4.327 | 5.425 | 1926.5 | emulated |
| openai | rag-questions | 4.583 | 6.362 | 490.8 | emulated |
| anthropic | routing-v0 | 4.518 | 5.831 | 1128.9 | emulated |
| anthropic | conversation-v1 | 4.058 | 5.175 | 1856.5 | emulated |
| anthropic | rag-questions | 3.902 | 4.688 | 414.8 | emulated |
| gemini | routing-v0 | 4.715 | 5.939 | 1118.9 | emulated |
| gemini | conversation-v1 | 4.692 | 6.271 | 1846.5 | emulated |
| gemini | rag-questions | 4.401 | 5.388 | 388.8 | emulated |
| meta | routing-v0 | 4.109 | 5.025 | 1106.9 | emulated |
| meta | conversation-v1 | 3.906 | 4.883 | 1834.5 | emulated |
| meta | rag-questions | 4.292 | 5.387 | 367.8 | emulated |

Request bytes differ because each wire format wraps the same prompt differently (the Responses, Messages, generateContent and Chat Completions shapes).

### 2.5 RAG excerpt handling

An excerpt travels only as an untrusted user-role part (never in the system instruction) and only for the cases that are answered; the 15 `decline` cases of the corpus are decided locally before generation and send nothing. This table checks that the excerpt reached the stub inside the request body. It does **not** test the data-egress policy itself: a `local_only` source sends nothing by `internal/dataegress`, tested in `internal/dataegress` and `tests/security/extprovider`, and the excerpts here are stand-ins built from each case's expected facts.

| Provider | Answer cases sent | Declines not sent | Excerpt in request body | Excerpt as untrusted user part | Reply states every fact | Label |
|---|---|---|---|---|---|---|
| openai | 35 | 15 | 35 | 35 | 35 | emulated |
| anthropic | 35 | 15 | 35 | 35 | 35 | emulated |
| gemini | 35 | 15 | 35 | 35 | 35 | emulated |
| meta | 35 | 15 | 35 | 35 | 35 | emulated |

### 2.6 Failure behaviour (recorded fixtures replayed through the real adapter)

`Result` is `ok` or the typed error code the adapter returned. `Requests` is how many requests reached the stub (more than one means the adapter retried); `Backoffs` is how many waits it asked for. `Leak` is whether the error text carried the upstream body, a marker planted in it, or the key. No scenario has a second endpoint to fall back to: a request count above one is a retry of the same call, never another provider. Which codes the controller then maps to a Local fallback is the operator's policy (X-22), not the adapter's.

| Provider | Scenario | Result | Requests | Backoffs | Leak | Label |
|---|---|---|---|---|---|---|
| openai | text | ok | 1 | 0 | no | recorded |
| openai | tool_call_proposal | ok | 1 | 0 | no | recorded |
| openai | json_schema_answer | ok | 1 | 0 | no | recorded |
| openai | provider_native_tool | schema_invalid | 1 | 0 | no | recorded |
| openai | active_media_in_reply | schema_invalid | 1 | 0 | no | recorded |
| openai | malformed_reply | schema_invalid | 1 | 0 | no | recorded |
| openai | auth_rejected | permission_denied | 1 | 0 | no | recorded |
| openai | quota_exhausted | resource_exhausted | 1 | 0 | no | recorded |
| openai | rate_limited_then_ok | ok | 2 | 1 | no | recorded |
| openai | server_error_then_ok | unavailable | 1 | 0 | no | recorded |
| openai | server_error_persistent | unavailable | 1 | 0 | no | recorded |
| openai | connection_dropped | unavailable | 1 | 0 | no | recorded |
| anthropic | text | ok | 1 | 0 | no | recorded |
| anthropic | tool_call_proposal | ok | 1 | 0 | no | recorded |
| anthropic | json_schema_answer | ok | 1 | 0 | no | recorded |
| anthropic | provider_native_tool | schema_invalid | 1 | 0 | no | recorded |
| anthropic | active_media_in_reply | schema_invalid | 1 | 0 | no | recorded |
| anthropic | malformed_reply | schema_invalid | 1 | 0 | no | recorded |
| anthropic | auth_rejected | permission_denied | 1 | 0 | no | recorded |
| anthropic | quota_exhausted | resource_exhausted | 1 | 0 | no | recorded |
| anthropic | rate_limited_then_ok | ok | 2 | 1 | no | recorded |
| anthropic | server_error_then_ok | unavailable | 1 | 0 | no | recorded |
| anthropic | server_error_persistent | unavailable | 1 | 0 | no | recorded |
| anthropic | connection_dropped | unavailable | 1 | 0 | no | recorded |
| gemini | text | ok | 1 | 0 | no | recorded |
| gemini | tool_call_proposal | ok | 1 | 0 | no | recorded |
| gemini | json_schema_answer | ok | 1 | 0 | no | recorded |
| gemini | provider_native_tool | schema_invalid | 1 | 0 | no | recorded |
| gemini | active_media_in_reply | schema_invalid | 1 | 0 | no | recorded |
| gemini | malformed_reply | schema_invalid | 1 | 0 | no | recorded |
| gemini | auth_rejected | permission_denied | 1 | 0 | no | recorded |
| gemini | quota_exhausted | resource_exhausted | 1 | 0 | no | recorded |
| gemini | rate_limited_then_ok | ok | 2 | 1 | no | recorded |
| gemini | server_error_then_ok | unavailable | 1 | 0 | no | recorded |
| gemini | server_error_persistent | unavailable | 1 | 0 | no | recorded |
| gemini | connection_dropped | unavailable | 1 | 0 | no | recorded |
| meta | text | ok | 1 | 0 | no | recorded |
| meta | tool_call_proposal | ok | 1 | 0 | no | recorded |
| meta | json_schema_answer | ok | 1 | 0 | no | recorded |
| meta | provider_native_tool | schema_invalid | 1 | 0 | no | recorded |
| meta | active_media_in_reply | schema_invalid | 1 | 0 | no | recorded |
| meta | malformed_reply | schema_invalid | 1 | 0 | no | recorded |
| meta | auth_rejected | permission_denied | 1 | 0 | no | recorded |
| meta | quota_exhausted | resource_exhausted | 1 | 0 | no | recorded |
| meta | rate_limited_then_ok | ok | 2 | 1 | no | recorded |
| meta | server_error_then_ok | unavailable | 1 | 0 | no | recorded |
| meta | server_error_persistent | unavailable | 1 | 0 | no | recorded |
| meta | connection_dropped | unavailable | 1 | 0 | no | recorded |

Reading the matrix: no adapter retries a 5xx answer or a dropped connection (the call may be commit-capable, so a blind repeat is unsafe); a rate limit that names a wait is retried once after that wait; a rejected key and an exhausted quota are not retried; a provider-native tool, active media or a malformed body is refused as `schema_invalid` and never reaches the caller as data.

Streaming is not replayed by this tool; each adapter's own conformance suite covers it (`go test ./adapters/external/...`).

## 3. Connected-mode footprint under the experimental 4 GB profile (emulated)

Separate from section 1: no local model is loaded here. The question is what the connected-mode control-plane pieces cost on a 4 GiB / 4 CPU machine when the model is remote.

**What was run.** The four real adapters, the egress dialer and four loopback stubs that add an injected network delay, in one process; optionally `alixai serve` (idle core: built-in defaults, no model, no provider configured) beside it in the same container. **What was not run:** a full `alixai serve` with external mode enabled, SQLite sessions, RAG indexes, the credential service or the audit log under load; those are outside this harness.

| Item | Value | Label |
|---|---|---|
| Idle `alixai serve` working set, Windows dev box (M0-15; not emulated, not a Linux RSS) | 23.1 MiB | recorded |
| Idle `alixai serve` private bytes, same sample | 55.0 MiB | recorded |
| `alixai` binary size, same build | 25.5 MiB | recorded |

Profile `experimental-4g`: 4096 MiB, 4 CPUs (`profile-definition`, as `tools/bench` defines it). This run: OS `linux/amd64`, cgroup v2 present: yes. Injected network delay per call: 120 ms plus up to 80 ms of jitter (an **input** of the emulation, not a result). Providers in the run: openai, anthropic, gemini, meta. Core process started in this run: yes.

| Container fact | Value | Label |
|---|---|---|
| memory.max | 4096.0 MiB | emulated |
| memory.swap.max (0 means no swap) | 0.0 MiB | emulated |
| memory.swap.current at the end | 0.0 MiB | emulated |
| memory.peak at the end | 66.1 MiB | emulated |
| oom_kill count | 0 | emulated |
| cpu.max | 400000 100000 | emulated |

| Phase | Calls | Errors | Concurrency | Wall ms | Per-call p50 ms | Per-call p95 ms | Per-call max ms | Added by AlixAI p50 ms | Added by AlixAI p95 ms | Label |
|---|---|---|---|---|---|---|---|---|---|---|
| sequential routing-v0 (4 providers in turn) | 80 | 0 | 1 | 13631.9 | 181.646 | 201.528 | 212.039 | 4.521 | 15.402 | emulated |
| concurrent load (4 providers) | 400 | 0 | 8 | 8006.5 | 155.435 | 199.651 | 211.259 | not measured | not measured | emulated |

Per-call time is the injected delay plus what AlixAI adds. "Added by AlixAI" is each call's time minus the delay the stub injected into that very call, known only when calls arrive one at a time (the concurrent phase does not attribute a delay to a call, so it is `not measured`). It says nothing about a provider's real latency.

| Memory metric (MiB) | Baseline | Peak | Label |
|---|---|---|---|
| Harness process resident set (Linux VmRSS) | 16.0 | 18.0 | emulated |
| Core process `alixai serve` resident set (Linux VmRSS) | 40.5 | 40.7 | emulated |
| Whole container, cgroup memory.current | 63.7 | 66.1 | emulated |
| Go heap in use (harness) | - | 6.8 | emulated |
| Go runtime memory obtained from the OS (harness) | - | 17.9 | emulated |

Peak goroutines in the harness: 61 (90 samples).

Headroom at the container's highest reading: 4029.9 MiB of 4096 MiB, above the 1024 MiB headroom floor of [the resource budgets](../../operations/resource-budgets.md) (`max(15%, 1 GB)`). **This is the connected-mode control-plane pieces alone; it is not the 4 GB profile's headroom.** The operating system, the SQLite and RAG working sets under load, the audit log and the host application are not in it, and the 4 GB aspiration stays an aspiration (master plan 7, additive plan section 12: network latency, local RAG and simultaneous application work still need measurement).

- cgroup memory.current and memory.peak count page cache (the mapped binaries and the files the run read) as well as resident memory, so they read higher than the sum of the resident sets; the headroom below is taken from the cgroup figure, the larger one (M0-15 judged the larger of the resident-set and cgroup views for the same reason)

The same run, in the bench result envelope: `benchmarks/results/x/` (provider `fake`, `synthetic: true`, profile `experimental-4g`, `emulated: true`; checked by `go run ./tools/bench check benchmarks/results/x`).

## 4. What this report does not show

- Hosted-model quality, latency under real network conditions, real cost, rate-limit behaviour of a live account: `not measured`, owed to X-H02.
- Any certified figure. Every container number is a cgroup cap on a larger machine (`emulated`), never `certified`.
- Any change to the Local-mode verdicts of section 1.
- The data-egress policy, budgets and kill switch under load; the full `alixai serve` in External or Hybrid mode.

## 5. Reproduce

```
go run ./tools/providercompare --footprint docs/evidence/x-benchmarks/footprint-4g.json --out docs/evidence/x-benchmarks/report.md
sh tools/providercompare/run-4g.sh      # the emulated 4 GiB container run: writes benchmarks/results/x/ and footprint-4g.json
go run ./tools/bench check benchmarks/results/x
go test ./tools/providercompare/...
```

The container is `--memory 4g --memory-swap 4g --cpus 4 --network none`, not privileged, removed on exit. Nothing is pulled: the image is one already on the machine and the binaries are bind-mounted. No LocalAI is started, so no LocalAI lock is taken.
