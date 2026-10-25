# Repeating the published benchmark results

Results are published by hardware, model, quantization, backend, context and configuration, and they are independently
repeatable. This package lets a third party check that the published numbers come from the
inputs we say they do, repeat the measurement on their own machine with one command, and see which numbers moved and by how much.

What is published is the M5-09 set: 14 runs of the 80-case `routing-v0` corpus (five models, three resource profiles, two
configurations) under LocalAI v4.10.0. Everything a run depends on is pinned in [`manifest.json`](manifest.json):

| Pinned | Where it is checked |
| --- | --- |
| Runtime image, by digest (`localai/localai:v4.10.0@sha256:...`) and the digest-pinned backend catalogue | `deploy/container/localai/compose.yaml`, `config/backend-gallery.yaml` |
| Profile caps (`lite` 8 GiB / 4 CPUs, `balanced` 16 GiB / 8 CPUs, `4g` 4 GiB / 4 CPUs) | the shipped `compose.<profile>.yaml` |
| Corpus SHA-256 and case count | `tests/performance/corpus/routing-v0.jsonl` |
| Model file: name, size, SHA-256 | the model registry (`models/registry/*.yaml`), the LocalAI model definition (`benchmarks/repro/models/*.yaml`) and the file itself |
| Prompt (`bench-prompt-v1` + its SHA-256), constraint (`grammar`), context (4096), `max_toks` (256), warm-up, timeout, greedy decoding | the 14 published result files |
| The 14 published results, by SHA-256 | `manifest.json` (the summary is the [model report](../../docs/evidence/m5-models/report.md); the raw result files are not part of this repository) |

Every number is **emulated**: a cgroup cap on one container on a larger host, never certified hardware.

## 1. Check the inputs (no model runs, no container, no network)

```bash
go run ./benchmarks/repro/cmd/verify-inputs            # exits 0 only when every pinned input matches
go run ./benchmarks/repro/cmd/verify-inputs -matrix    # also lists the published results by hardware, model, quantization,
                                                       #   backend, context and configuration
go run ./benchmarks/repro/cmd/verify-inputs -models-dir ./benchmarks/tmp/repro/data/models   # also hash the GGUF files you downloaded
```

It fails closed: a corpus byte changed, a model definition or registry entry that disagrees with the manifest, an image without a
digest, a profile with other limits, a published result that was edited, or one that was measured on another prompt, corpus,
model file or runtime version, is a `FAIL` line and exit status 1. With `-models-dir`, a model file that is present must match
its pinned SHA-256 and size; an absent one is skipped (LocalAI downloads and verifies it at the first start) unless you add
`-require-models`.

## 2. Reproduce

Needs Go, curl, and Docker or Podman with the compose plugin. The first run downloads the pinned backend and the
SHA-256-pinned model files (about 3.8 GB for all five models) into `--data`; after that it is offline. Nothing else leaves the machine.

```bash
benchmarks/repro/reproduce.sh --profile 4g                      # one profile (lite | balanced | 4g)
benchmarks/repro/reproduce.sh --runs qwen3.5-0.8b-q4_k_m-lite-A # chosen runs (ids are in manifest.json)
benchmarks/repro/reproduce.sh                                   # everything: about 4.5 hours of wall clock, one container at a time
benchmarks/repro/reproduce.sh --dry-run                         # verify the inputs and print every command; start nothing
```

It runs `verify-inputs`, builds `tools/bench`, then for each model and profile starts the pinned container under the profile's caps with
[`compose.repro.yaml`](compose.repro.yaml) (it mounts only that model's definition), runs `tools/bench` once per configuration
(`A`: the model alone; `G`: with the deterministic out-of-scope gate `scopegate-v1` in front, `--gate`), stops the container,
and finally compares. Results go to `benchmarks/tmp/repro/<timestamp>/runs/<run id>/` (git-ignored), the comparison to `compare.txt`.
The container is the only thing it starts and the only thing it stops; it refuses to run while a container named `alixai-localai` exists.
Exit status: 0 within tolerance, 1 a run failed or a result deviates, 2 a missing prerequisite or bad flag.

Close other heavy work first: the published latencies were taken on a shared host and are indicative (see below).

## 3. Compare

```bash
go run ./benchmarks/repro/cmd/compare -reproduced benchmarks/tmp/repro/<timestamp>/runs [-runs id,id] [-allow-missing]
go run ./benchmarks/repro/cmd/compare <published.json> <reproduced.json>      # two result files
```

The tool first refuses to compare results that were not measured on the same inputs (corpus, prompt, constraint, role, `max_toks`,
context, model file hash, profile, backend and runtime version, emulated flag; a synthetic or partial run never reproduces anything):
those rows are `FAIL` and no metric is compared, because a difference of setup would be reported as a difference of behaviour.
Then, with the tolerances of the manifest (override with `-tol-quality`, `-tol-latency`, `-tol-memory`):

| Class | What is compared | Tolerance | Beyond it |
| --- | --- | --- | --- |
| quality | the nine pass rates (`schema_valid`, `capability_routing`, ..., `pass`; the denominators must be identical) and the error rate | 0.05 absolute | `FLAG`, exit 1 |
| latency | total p50 and p95, time to first output p50, decode tokens/s | 50 % relative | `note`; `FLAG` and exit 1 only with `-strict-perf` |
| memory | peak and steady working set | 25 % relative | `note`; `FLAG` and exit 1 only with `-strict-perf` |

Quality is the repeatability criterion. Latency and memory depend on the host, so they are reported but do not fail a reproduction
on different hardware unless you ask. Greedy decoding is not bit-exact across runs: in M0-17, 14 of 80 outputs differed between two
identical runs on one machine without changing the verdict, which is why quality has a tolerance and not a zero. A reproduction that
lands outside it is a finding about the published result, the build or your environment: report it with `compare.txt`, the
`bench.log` of the run and your `docker inspect` output.

## 4. Publish your own results

Run `tools/bench` as in [`benchmarks/README.md`](../README.md), keep the result file, and state hardware (the result records CPU,
threads, RAM, OS and architecture), model and quantization, backend and runtime version, context, and configuration (gate or not:
the gate flag is not in the result file, so the run name must say it). `bench check` validates a result; `compare` shows how it
differs from ours.

## Maintainers: publishing a new set

`manifest.json` is edited together with the results it describes: add the result files, their SHA-256, the registry pins and the
model definitions, run `verify-inputs`, and commit all of it together. Never edit a published corpus or result in place (a new
corpus gets a new id); `verify-inputs` is the gate that notices a file that no longer matches what was published. The code is in
`internal/repro` (manifest, verification, comparison) with its tests; `cmd/runplan` prints the run plan that `reproduce.sh` reads.
