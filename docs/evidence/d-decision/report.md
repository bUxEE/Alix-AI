# D-08 report: live decision-layer benchmark on this host (emulated)

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

(Written as `report.md` because the worker harness refuses to create a file named `report.md`; the orchestrator renames it to `report.md`, the name the task's `verify` expects.)

The decision layer is optional, bounded, non-generative and non-authoritative; CLM-v0.1-8B is a candidate scorer that ranks supplied authorised candidates and never decides. Everything below is **emulated** (container caps on one development host), indicative, and no benefit of any scorer is claimed: that needs evidence this host could not produce.

## Result in one paragraph

The pinned LocalAI v4.10.0 does not serve the Decisions API, so it can serve neither CLM nor a sub-1B scorer. The supervised CLM sidecar fetched and verified (every pinned sha256 matched) but, on the balanced profile, the 8B encoder did not fit: it climbed to 13.4 GiB of its 14 GiB cap while loading, the host's free RAM fell to 2.3 GiB, and it never became healthy (`resource_gate`). The higher-memory profile was not run because the host could not safely hold 30 GiB of caps. The host crashed minutes after the balanced attempt (cause not established). Consequently no CLM or scorer run exists to gate, and ADR 0031 keeps every scorer OFF on every profile.

## Host and setup

i7-10700K (8 cores, 16 logical), 64 GB RAM (Windows free RAM during the work 17 to 35 GiB; a security product held about 16 GiB), Windows 11, Docker Desktop 29.7.2 with a 31.3 GiB, 16 CPU VM, no GPU used. Repository HEAD of the worktree: see the commit log (`decbench` records the git sha in every result). Images by digest: `localai/localai:v4.10.0@sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b`, `vllm/vllm-openai-cpu:v0.31.0@sha256:495c84fe30f3572b83a78100a1e8ba423dbb77fc532ecefebe114e6b77327bd9`. Models: Granite 4.0 1B Q4_K_M (controller stand-in, 1,023,645,440 bytes), CLM head `e939398d...` and Qwen3-8B `b968826d...` (pins in ADR 0029). Container limits: lite 8 GiB / 4 CPU, balanced 16 GiB / 8 CPU (LocalAI) and encoder 14 GiB / 6 CPU plus CLM service 2 GiB / 2 CPU (`compose.balanced.yaml` of the sidecar). Host-side pulls only; the LocalAI model directory was junctioned, not copied. Registry-pinned files were fetched with `deploy/container/clm/fetch.sh` (16.4 GB, about 9 minutes, all hashes matched).

## 1. Does the pinned LocalAI serve the Decisions API? No

`localai-decisions-probe.txt`, `logs/localai-adapter-live-test.txt`: with a valid key, `POST /v1/systemone` (and `/permute`, `/separate`, `/v1/decisions`) answer 404 `Resource not found`, like an invented route; the binary holds no `systemone` string; `adapters/decision/localai` reports `unsupported_runtime` ("the runtime does not provide this scoring route"). The `v4.10.0-cpu-vllm-cpp` backend image exists on quay (index `sha256:d7f00abd...`, 23 MB amd64 layer) but is not in the pinned backend catalogue and was not installed.

## 2. Baseline and deterministic runs (`benchmarks/results/d/`, all pass `decbench check`)

Non-synthetic (`--scorer localai`, no scorer call is made for these configurations), 86-case EN/IT corpus, controller latency supplied from a live measurement, memory supplied from `docker stats`.

| Profile | Config | Result |
| --- | --- | --- |
| lite | 1 rules only (`run_01M4D5F1B1...`) | task 25/86 (29.1%), controller calls avoided 23/86, first response p50/p95/p99 0.0/0.0/0.5 ms |
| lite | 4 direct controller (`run_01M4D5F2017...`) | task 86/86 by construction, p50 = p95 = 592 ms (supplied), peak 467 MB + resident model 976 MB, reserve 6,749 MB |
| balanced | 1 (`run_01M4D5K8JM5...`) | same counts as lite |
| balanced | 4 (`run_01M4D5K97REM...`) | p50 = p95 = 525 ms (supplied), peak 456 MB + 976 MB, reserve 14,952 MB |

Limits: the harness controller is an oracle with a supplied latency (task 86/86 is by construction, not a model quality measurement); the controller latency is the Granite 4.0 1B stand-in warm p50 over 15 calls (`controller-granite-1b-*.json`); no concurrent RAG, voice or tool load; the memory reserve is the profile cap minus the container's total memory (incl. page cache). Configurations 2, 3 and 5 have no result: config 2 needs the Decisions API (absent), and 3 and 5 need the CLM sidecar (never healthy). A keyed live run was also impossible with the current tool: `tools/decbench` builds its adapters without an `api_key_ref` and a keyless LocalAI was not started (auth is not weakened). `decbench gate`, lite, config 1 against config 4 (`logs/gate-lite-rules-vs-controller.txt`): do not enable (regressed task completion, citation correctness, multilingual quality; improved latency and capacity).

## 3. Deployment options

| Option | Result |
| --- | --- |
| LocalAI Decisions on v4.10.0 | unsupported (section 1) |
| Sidecar, balanced (14 GiB encoder) | `resource_gate`: re-hash of the 16.4 GB took about 3.5 min; load then climbed 0.2 -> 13.4 GiB (13,742 of 14,336 MiB) in 166 s; process gone 6 s later (13,742 -> 1,413 -> 844 -> 35 MiB); host free RAM 19 GiB -> 2.3 GiB (`rss/balanced-sidecar.csv`). Exit code and logs were lost in the host crash that followed, so an out-of-memory kill is inferred, not recorded; vLLM's dtype line was never seen. Not repeated: it drove the host to 2.3 GiB free. |
| Sidecar, higher-memory (26 + 4 GiB) | not measured: free host RAM 17 to 35 GiB, bf16 weights 15.3 GiB before activations, 30 GiB of caps do not fit the 31.3 GiB VM beside everything else |
| Resident vs demand-loaded (LocalAI, Granite 1B stand-in, lite, 2 active backends) | cold load 13.9 s (balanced 15.4 s), warm 0.35 to 0.74 s, reload after LRU eviction 15.2 s (three models in turn, `rss/lite-eviction.csv`, peak container 1,902 MiB), restart to ready 6.1 s then 12.0 s first call. The sidecar's demand-loaded cost would be the re-hash plus the load on every start. |

## 4. Registry, ADRs

`clm-v0.1-8b` and `qwen3-8b` stay `candidate` (the status is not per profile; `rejected` would also block the higher-memory profile, which has no evidence either way, and the sidecar entrypoint refuses rejected entries) with a note recording the `resource_gate` and the evidence paths (their `measurement` block stays null because `TestCLM8BEntryStartsAsCandidateAndUnpinned` in `internal/models/scorer_test.go`, outside this task's scope, pins it null for a candidate); `last_efficiency_review` points at `docs/efficiency/D-08.md`; `default_for` empty. ADR 0030 and 0031 are accepted; ADR 0029 carries the pinned revisions and the outcome. The sub-1B scorers keep their registry state: they could not be run, which says nothing against the models.

## 5. Incident and cleanup

The host crashed during the balanced attempt (about 4 minutes after the sidecar had already failed once and restarted by its `on-failure` policy); Docker Desktop was down afterwards. Docker Desktop was restarted by the worker to remove the container, which it had resurrected; the sidecar container and network were removed with `docker compose down`, no container of this task remains, and no process was killed by name. The 16.4 GB of model files stay in the worktree's gitignored `deploy/container/clm/data` and disappear with the worktree; the junction `deploy/container/localai/data` points at the shared LocalAI data and must be removed with `rmdir`, never `rm -r`.

## 6. Needs outside this task's scope

- `tools/decbench`: an `--api-key-ref` flag (a keyed LocalAI or CLM service cannot be benchmarked without it); the `high` profile caps (32768 MB, 8 CPU) differ from `compose.highmem.yaml` (30 GiB, 16 CPU), so a reserve computed under `--profile high` uses the wrong cap.
- A CLM measurement needs a host with free RAM well above 30 GiB (D-H02) or a quantised encoder that fits Balanced.
