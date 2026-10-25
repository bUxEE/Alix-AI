# Optional CLM sidecar (container): vLLM encoder + CLM service

The supervised sidecar of the decision layer (ADR 0029 option 4; ADR 0030 provisional preference): a vLLM server that
serves the frozen Qwen3-8B encoder, and the CLM service (CLM-v0.1-8B heads) that calls it. It is **optional** (AlixAI
runs without it, and nothing here is needed by the Lite profile), **bounded** (at most 32 authorised candidates and
2048 state tokens per call), **non-generative** (it ranks supplied candidates and writes no text) and
**non-authoritative** (an advisory score; the deterministic controller re-resolves every selected id and decides).
It follows the pattern of [`../localai`](../localai/README.md): pinned digest, loopback only, resource-limited
profiles, a guard entrypoint, no one-shot setup container.

> **Untested.** The vLLM server has never been started here. The vLLM CPU path for an 8B pooling model, the memory it
> needs, the time to load it, the CLM service on CPU, and the behaviour under Docker Desktop on Windows are all
> untested until D-08 runs them. The balanced profile is expected not to fit (section "Profiles"). Every number below
> is a cap or a pin, not a measurement. No result obtained under a resource-limited profile is a certification: results
> from these files are labelled **emulated**.

## Pinned versions

Read on 2026-10-07. Digests and hashes come from the registries' own metadata, not from memory.

| Component | Pin | Source |
| --- | --- | --- |
| vLLM CPU image | `docker.io/vllm/vllm-openai-cpu:v0.31.0@sha256:495c84fe30f3572b83a78100a1e8ba423dbb77fc532ecefebe114e6b77327bd9` (index; amd64 `sha256:8024248339dc...`, arm64 `sha256:045fb648a61c...`) | Docker Hub tag of 2026-10-04; digest from the registry's `docker-content-digest` header |
| Encoder | `Qwen/Qwen3-8B` commit `b968826d9c46dd6066d109eabc6255188de91218` (2025-07-26), Apache-2.0, bf16, five safetensors of 16,381,516,776 bytes (~15.3 GiB) | Hugging Face API, LFS oids |
| CLM head | `Contrastive-LM/CLM-v0.1-8B` commit `e939398d4556fcd9400c76fa8c5a513202f42b0a` (2026-09-24), `CLM_v0.1-8B.pt` 75,557,149 bytes, sha256 `b2b4a8c9c2d39263eff78a351eb909a342ce9b3bf21a3f07c1d1bf15f1c4eda5`, Apache-2.0 | Hugging Face API, LFS oid |
| CLM service | `contrastive-lm` 0.1.0, wheel sha256 `7f3eed12d3aa10173f71bac7e50fc387565b485a2a2fc4c4323af32b1950055f` (uploaded 2026-09-24), Apache-2.0 | PyPI metadata |
| Image contents (checked 2026-10-07) | Python 3.12.15, torch 2.13.0+cpu, fastapi 0.136.3, uvicorn 0.54.0, numpy 2.3.5, requests 2.34.2, vLLM 0.31.0 | `docker run --network none` import check |
| Serving instructions read | `github.com/Contrastive-LM/CLM` at `d5f9ef0fd9bde185df0ceaad4f4ecc6cfe8c34f6` (2026-10-05): vLLM on 8090 with `--runner pooling --max-model-len 2048`, the CLM service on 8700, `--device cpu` | repository README |

Every file's sha256 is in [`config/pins.txt`](config/pins.txt). The encoder revision recorded by the CLM repository's
instructions is `Qwen/Qwen3-8B` without a revision; the pin above is the newest commit at the review date and is what the
registry (D-11) must record. Vendor-reported numbers (for example about 28 ms for a cold call with three actions on one
RTX 4090) are a vendor claim on other hardware and are not evidence for AlixAI.

## Files

| File | Purpose |
| --- | --- |
| `compose.yaml` | the two services, digest-pinned, `127.0.0.1`-only publish, internal encoder network, health-gated order |
| `compose.balanced.yaml`, `compose.highmem.yaml` | resource profiles (override files), every number emulated |
| `compose.fetch.yaml` | optional override: the two services fetch their own pinned files at first start |
| `entrypoint.sh` | guard for both services: registry and pin checks, hashing, then `exec` |
| `config/pins.txt` | the pins: revision, sha256, path and url of every file |
| `fetch.sh` | host-side fetch of the pinned files (the working path behind a TLS-intercepting product) |
| `smoke.sh` | start, posture check, one rank request, tear down (not run by `plan verify`; the live run is D-08) |
| `.env.example`, `hardening.md` | placeholders only; the posture, the disabled surfaces and the outbound calls |

## Run

```bash
cd deploy/container/clm
cp .env.example .env            # edit CLM_API_KEY: openssl rand -hex 32  (see "Authentication gap")
./fetch.sh                      # ~16.4 GB into ./data/encoder, ~76 MB into ./data/clm; resumes, verifies sha256
docker compose --env-file .env -f compose.yaml -f compose.highmem.yaml config -q     # validates only
docker compose --env-file .env -f compose.yaml -f compose.highmem.yaml up -d         # nothing starts before this line
```

The CLM service starts only after the encoder reports healthy. The encoder first re-hashes about 16 GB, then loads
the model; `start_period` is 60 minutes for it. The CLM service answers on `http://127.0.0.1:8700`; AlixAI points
`adapters/decision/clm` at that endpoint with the model name the service lists (`clm-latest`) and the pinned revision.

The sidecar needs the registry entries of the CLM head and the encoder (`models/registry/<id>.yaml`, ids in `.env`):
D-11 adds them. Until then the entrypoints refuse to start with `model registry entry ... not found`, which is the intended
fail-closed behaviour. The entries must pin `artifact.revision`, `artifact.filename` and `artifact.sha256` equal to
the line of `config/pins.txt` for that file; for the encoder the entry names one of the five safetensors files, for
the head `CLM_v0.1-8B.pt`. Status `rejected` or `retired` refuses the start.

## Profiles (hardware emulation)

| File | Encoder | CLM service | Total | Evidence |
| --- | --- | --- | --- | --- |
| `compose.balanced.yaml` | 14g, 6 CPU, 6 threads | 2g, 2 CPU | 16g / 8 CPU | emulated; **expected not to fit** |
| `compose.highmem.yaml` | 26g, 12 CPU, 12 threads | 4g, 4 CPU | 30g / 16 CPU | emulated; fit untested |
| none (`compose.yaml` alone) | unlimited | unlimited | host | unconstrained, not a profile |

There is no lite or 4 GB file on purpose: the smallest profile never requires the sidecar. The bf16 weights alone are
~15.3 GiB, larger than the encoder cap of the balanced file, so that profile exists to record the resource-gate
outcome (`resource_gate`), not to promise a run. `mem_limit` equals `memswap_limit`, so the containers cannot swap.
Threads equal the CPU share because a CPU quota does not shrink the cores a thread pool sees.
Whether the CPU bf16 path needs vector instructions the host lacks, and what throughput it reaches, is a hypothesis
until D-08 measures it.

## What is disabled, and why

Details in [`hardening.md`](hardening.md). In short: no published encoder port and no route out for it, no playground,
no CORS, no head download, no vLLM or Hub telemetry, no GPU of any kind (an AMD GPU is never used), no package
install, no one-shot setup container, no extra model files.

## Fetching: in the services, or on the host

The multi-GB download belongs to the main services, not to a setup container, so there are two ways, both through the
same checks:

- **In the services.** `docker compose ... -f compose.fetch.yaml up -d` sets `ALIXAI_FETCH_MODELS=true`, mounts the data
  directories writable and gives the encoder a route out for that run. `entrypoint.sh` downloads only the exact urls
  of `config/pins.txt` into `.part` files and renames a file after its sha256 matched, then verifies everything as at
  every start. Drop the override afterwards.
- **On the host.** `./fetch.sh` does the same with the host's `curl`, which trusts what the host trusts. In-container
  TLS fails where a TLS-intercepting security product re-signs HTTPS (the M0 dev machine): the service log then shows an
  `x509: certificate signed by unknown authority` or SSL verification error. This is the working path there, and the
  image pull must likewise be done by the host's Docker (`docker pull` of the pinned reference), which the compose file
  then finds locally. Nothing is relaxed in either path.

## Guards in `entrypoint.sh`

Every failed check stops the container with exit 78 and serves nothing: the registry entry exists and is not rejected or
retired, its revision and sha256 equal the pins, the model directory holds exactly the pinned files (no extra file, no
symlink, no `.part`), every file has its pinned sha256, a GPU selector other than `-1` is refused, telemetry
stays off, and (CLM service) the key is present, not a placeholder and at least 32 characters. The encoder
entrypoint checks the encoder; the CLM entrypoint checks the head, the wheel, and that the registry's encoder entry
equals the encoder pins.

## Authentication gap

The CLM service supports a bearer key (`CLM_API_KEY`); the encoder cannot use one because the service sends none to it,
and is protected by the internal network instead. `adapters/decision/clm` (D-14) sends the service key as `Authorization: Bearer` from its `api_key_ref` (`env:` or `file:`), so a run driven by AlixAI uses the key and needs no stopgap. See `hardening.md` section 7.

## Update and rollback

- **Update.** Resolve the new references from the registries: the image digest with
  `T=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:vllm/vllm-openai-cpu:pull" | sed -E 's/.*"token":"([^"]+)".*/\1/')`
  then `curl -sI -H "Authorization: Bearer $T" -H "Accept: application/vnd.docker.distribution.manifest.list.v2+json" https://registry-1.docker.io/v2/vllm/vllm-openai-cpu/manifests/<tag> | grep -i docker-content-digest`;
  the model commits and LFS hashes with `https://huggingface.co/api/models/<repo>/tree/<commit>`; the wheel digest from
  `https://pypi.org/pypi/contrastive-lm/<version>/json`. Change `compose.yaml` (both `image:` lines), `config/pins.txt` and the
  registry entries in one commit, move the old data aside (`mv data data.prev`), run `./fetch.sh`, `./smoke.sh`.
- **Rollback.** `docker compose ... down`, check out the previous commit of this directory and of the registry,
  restore the old data (`mv data.prev data`), start again. The old image is pulled by its old digest; nothing floats.
- **Pre-flight.** Re-run the audit in `hardening.md` section 8 and record new vLLM release notes on outbound calls.

## Stop and remove

```bash
docker compose --env-file .env -f compose.yaml -f compose.highmem.yaml down --remove-orphans   # stops and removes both containers and the networks
rm -rf data                                                                                       # ~16.5 GB of model files (bind mounts, nothing else to clean)
docker image rm docker.io/vllm/vllm-openai-cpu:v0.31.0@sha256:495c84fe30f3572b83a78100a1e8ba423dbb77fc532ecefebe114e6b77327bd9
```

Removing the sidecar needs no AlixAI data change: with `decision.enabled=false` or the provider unset, the pipeline
routes deterministically (ADR 0029 reversal plan).

## Docker and Podman

The files use plain Compose syntax. Podman (`podman compose`) honours `internal: true` networks and `read_only` with
`tmpfs`; rootless Podman cannot publish a port below 1024, which is not used here. Both are untested for this stack.

## Troubleshooting

- `model registry entry ... not found`: D-11 has not added the entry, or the id in `.env` differs from the file name.
- `does not hold exactly the pinned files`: an interrupted download left a `.part` file, or a file from another revision
  is present. Remove it, or `mv data data.prev` and fetch again.
- vLLM exits with `Read-only file system`: a cache path is not on the tmpfs. Note the path and add it as a `tmpfs:` entry;
  do not make the root writable.
- The encoder is killed with exit code 137 or 9: out of memory under the cap. Compare with the weights size above.
- `get_mempolicy: Operation not permitted`: expected, see `hardening.md` section 5.
- The CLM service answers 401 to AlixAI: the authentication gap above.

## What has been run live

Only configuration-level checks, on 2026-10-07, host Docker 29.7.2 on Windows 11, with the pinned image pulled by the host:

- the image holds Python 3.12.15, torch 2.13.0+cpu, fastapi 0.136.3, uvicorn 0.54.0, numpy 2.3.5, requests 2.34.2 and vLLM 0.31.0,
  plus `bash`, `awk` (mawk), `sha256sum` and `curl`, and runs as root;
- `entrypoint.sh vllm` against a fake registry and a one-file pins file refused a revision mismatch, a tampered file, an extra file,
  a `rejected` entry, a missing entry and `HIP_VISIBLE_DEVICES=0`, each with exit 78;
- `entrypoint.sh clm` with the real head and wheel (fetched by `./fetch.sh clm`, both hashes matched), a fake registry matching
  the pins, `--network none`, `--read-only`, `--cap-drop ALL`: both registry checks and the hashing passed, the head loaded on CPU
  (`models ['clm-latest', 'clm-raw'] on cpu`, `auth on`, a 268 MB bounded vector cache) and the service listened on 8700 with the
  encoder reported `NOT REACHABLE`, as expected without it. No request was sent; the container was removed.

Everything involving the encoder is untested. D-08 owns the first full run, the load time, the cold and warm latency, the
memory peak and the comparison with the LocalAI Decisions path, all labelled emulated.
