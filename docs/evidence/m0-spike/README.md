# M0-13 spike evidence: how it was produced

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

Everything here comes from one machine (Windows 11, Docker Desktop with a 16-CPU, 32 GB Linux VM, i7-10700K host, no GPU use) and is
**emulated**: the lite, balanced and 4g profiles are cgroup caps on one LocalAI container, not certified hardware.
[`report.md`](report.md) is the result, [`hardening-live.md`](hardening-live.md) is the hardening re-verification, this file is the
recipe and the file index.

## Reproduce

Run from the repository root in Git Bash. The Docker CLI directory must come first on `PATH`
(`export PATH="/c/Program Files/Docker/Docker/resources/bin:$PATH"`): on this machine the credential helper cannot be found
otherwise. The API key lives in an env file outside the repository. Do not export `MSYS_NO_PATHCONV` for the wrappers: they call
`curl` and `docker compose` with host paths (the scripts that need it set it for `docker exec` only).

```bash
export SPIKE_ENV_FILE=<path to a file holding LOCALAI_API_KEY=$(openssl rand -hex 32)>
S=tests/live/spike/localai

$S/fetch-models.sh                                   # host-side download + sha256 check into deploy/container/localai/data/models
go run ./tests/live/spike/hashmodels -dir deploy/container/localai/data/models \
    -out docs/evidence/m0-spike/model-hashes.json \
    -also granite-embedding-97M-multilingual-r2-Q8_0.gguf=25155b89638e501ac33495fa278d551d7545e1e2f62722a499bba1f064c080f2:115061088

$S/smoke.sh lite                                     # the shipped smoke checks plus the spike overrides
go build -o benchmarks/tmp/bench.exe ./tools/bench
go test -tags live -c -o benchmarks/tmp/localai.live.test.exe ./adapters/localai/
SPIKE_STATE=<dir> $S/matrix.sh "granite-4.0-350m-q4_k_m lite grammar,schema,none" \
    "qwen3.5-0.8b-q4_k_m lite grammar,schema,none" "granite-4.0-1b-q4_k_m lite grammar,schema,none" \
    "qwen3.5-2b-q4_k_m balanced grammar,schema,none"  # one cold start per model; RSS sampled into rss/
# (the later "none" runs of the 1B and the 2B were run with SPIKE_RSS_SUFFIX=-none so their samples did not overwrite the first ones)
node $S/apply-registry.mjs                           # writes the verdicts into models/registry/ from the result files
node $S/report-tables.mjs                            # the tables of report.md

# The shipped registry rejects every controller, so the shipped binary refuses inference. The plumbing request runs on a binary
# whose embedded copy of ONE registry entry is changed at build time (go build -overlay; nothing in models/registry/ is edited):
$S/build-spike-binary.sh qwen3.5-0.8b-q4_k_m benchmarks/tmp/alixai-spike.exe
$S/stack.sh up lite
ALIXAI_LOCALAI_URL=http://127.0.0.1:8080 ALIXAI_LOCALAI_KEY=<key> ALIXAI_SPIKE_BIN=$PWD/benchmarks/tmp/alixai-spike.exe \
  ALIXAI_SPIKE_MODEL=qwen3.5-0.8b-q4_k_m ALIXAI_SPIKE_CONTAINER=alixai-localai \
  ALIXAI_SPIKE_DOCKER='C:\Program Files\Docker\Docker\resources\bin\docker.exe' \
  go test -tags live -count=1 -v ./tests/live/spike/
$S/stack.sh down lite
# variants: stack.sh up lite $PWD/$S/experiments/opaque-errors.yaml + ALIXAI_SPIKE_ERRORS_VARIANT=opaque-errors;
#           stack.sh up 4g + ALIXAI_SPIKE_ERRORS_VARIANT=4g ALIXAI_SPIKE_ERRORS_OOM=1
$S/embedding-footprint.sh                            # cost of the embedding model on a cold lite stack
$S/hardening-experiments.sh baseline restart internal-network no-caps only-dac-override nonroot readonly-rootfs \
    readonly-models strict-integrity wrong-sha no-caps-install info-log debug-log trace-log
$S/linux-permission-matrix.sh                        # capability semantics on a Linux filesystem
$S/egress-capture.sh                                 # sidecar tcpdump during a provisioned boot
node $S/hc-static-check.mjs <run.go,context.go of the pinned tag> <docker compose config output> docs/evidence/m0-spike/hc-static-check.json
node tests/live/spike/baseline/rules-baseline.mjs tests/performance/corpus/routing-v0.jsonl docs/evidence/m0-spike/rules-baseline.json
go run ./tools/bench check benchmarks/results/m0
```

`tests/live/spike/localai/` holds the spike overrides (`compose.spike.yaml`, `models.yaml`), the wrappers and the `experiments/`
overrides used for the hardening checks. Nothing under `deploy/` was edited: `deploy/` is outside this task's scope, so every fix it
needs is listed in `report.md` (F7) and `hardening-live.md` as a change for the orchestrator. `models/registry/*.yaml` is edited
only by `apply-registry.mjs`, from the files listed below.

## Files

| File | What it is |
| --- | --- |
| `report.md` | The result: verdict, pass bar (written before the results), tables, what failed, findings for other tasks |
| `hardening-live.md` | Every item of `deploy/container/localai/hardening.md` section 8 and of the hardening checklist's live section, re-verified against the pinned release |
| `e2e.json` | One request through `alixai serve` to a validated TaskProposal: request, raw model output, proposal, correctness, latency, memory, the registry variant used |
| `model-hashes.json` | Every downloaded GGUF re-hashed with `models.HashFile` and compared with the registry pin |
| `embedding.json`, `embedding-footprint.json` | Granite Embedding 97M Q4 versus Q8 retrieval quality; its cold start, latency and resident set |
| `json-schema-probe.json` | Which `response_format` schemas the pinned runtime actually enforces |
| `surface-probe.json` | Status of 33 LocalAI routes with and without the key |
| `error-mapping-default.json`, `-opaque-errors.json`, `-4g.json` | The runtime's raw failure answers and the code `alixai serve` mapped them to |
| `rules-baseline.json` | The no-model keyword router on the same corpus |
| `hc-static-check.json` | The checklist's machine-checkable rows against the pinned release and the effective compose file |
| `backend-install-metadata.json` | What LocalAI recorded when it installed the digest-pinned backend from the local gallery |
| `egress/` | What left the container: DNS and connection attempts from three packet captures, with `summary.txt` |
| `logs/` | Smoke runs, bench runs, live adapter tests, the live spike suite, hardening experiments (`hardening-*.log`), the cold-start timeout and the shipped binary refusing a rejected model |
| `rss/` | Process RSS of the LocalAI processes, sampled every 2 s during each run (docker stats cannot give RSS) |
