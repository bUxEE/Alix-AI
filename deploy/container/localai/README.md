# Private LocalAI runtime (container)

LocalAI packaged as AlixAI's private inference runtime: pinned by digest,
published on loopback only, API key required, everything AlixAI does not use turned off, resource-limited
profiles for hardware emulation. Models and backends are fetched by the LocalAI service itself at start, from
sha256-pinned definitions in `config/`; there is no init container.

## Pinned versions

Resolved on **2026-10-02**. Re-resolve with the commands under "Bumping".

| Component | Pin | Notes |
| --- | --- | --- |
| LocalAI release | `v4.10.0`, published 2026-09-17 | latest release on 2026-10-02; MIT; source revision `7ad0cbf259f0c7bf9920fe2438fc3630ecd6c672` |
| CPU image | `docker.io/localai/localai:v4.10.0` | multi-arch index digest `sha256:0632c21ddbe440bb71bc7aa744796c227a51658641b216290e86d6894e1c705b` (also on `quay.io/go-skynet/local-ai`). amd64 `3ebbbe17...`, arm64 `825ce737...`. Image built 2026-09-17T20:06Z, about 280 MiB compressed. `latest` pointed at the same digest. |
| Backend `cpu-llama-cpp` | `v4.10.0-cpu-llama-cpp` | digest `sha256:d7b0d9c69ecce93c2e7f7500ac3d99aba28648dab1ab29b698e0cc4538662579` (`quay.io/go-skynet/local-ai-backends`, mirror `docker.io/localai/localai-backends`), about 47 MiB compressed. Installed on first boot into `data/backends`. |
| Smoke model | IBM Granite 4.0 350M, `granite-4.0-350m-Q4_K_M.gguf` | Apache-2.0, 236,985,760 bytes, sha256 `771c588a49607f274a2bba3185733607ebe6f74b996ab90e2d6bee0d98bcec52`, from `ibm-granite/granite-4.0-350m-GGUF` at commit `b8208a86a58427e1739265318028eb5895b74bf2`. A plumbing/smoke model, not the model registry's choice for any profile. |

## Files

| File | Purpose |
| --- | --- |
| `compose.yaml` | The service: pinned image, `127.0.0.1:8080:8080`, bind mounts, hardening settings, healthcheck. |
| `compose.lite.yaml` | Override: 8 GB / 4 CPU. |
| `compose.balanced.yaml` | Override: 16 GB / 8 CPU. |
| `compose.4g.yaml` | Override: 4 GB / 4 CPU (experimental, text-only). |
| `compose.stt-only.yaml` | Override: STT alone (no LLM, no embedder). Default Parakeet; Lite Whisper tiny via env. |
| `compose.readonly-models.yaml` | Optional override: `/models` read-only (a mismatching model file can no longer be deleted and replaced at start). Needs the guard switch below. |
| `.env.example` | Copy to `.env`. Holds `LOCALAI_API_KEY` and a few tunables. |
| `config/models.yaml` | Model definitions with pinned source and sha256. Mounted read-only. |
| `config/models.stt-only.yaml` | STT-only model list: Parakeet TDT 0.6B v3 Q4_K (ADR 0040). |
| `config/models.stt-only-lite.yaml` | STT-only model list: Whisper tiny (Lite / `en-stt`). |
| `config/models.stt-only-turbo.yaml` | STT-only model list: Whisper large-v3-turbo q5_0 (`whisper-turbo-stt`). |
| `config/models.stt-only-large-v3.yaml` | STT-only model list: Whisper large-v3 q5_0 (`whisper-large-v3-stt`). |
| `config/backend-gallery.yaml` | Digest-pinned backend catalogue (LLM only), the gallery LocalAI sees by default. Mounted read-only **at `/backends/alixai-backend-gallery.yaml`**: LocalAI v4.10.0 reads a `file://` gallery only from inside its backends directory. |
| `stt-only/backend-gallery.yaml` | STT backends only (`cpu-parakeet-cpp`, `whisper`); mounted by `compose.stt-only.yaml`. |
| `entrypoint.sh` | Guard that runs before LocalAI: refuses a weak or placeholder key, unwritable data dirs (a read-only `/models` only with `ALIXAI_MODELS_READONLY=true`), a writable config dir, and a `debug` or `trace` log level (unless `ALIXAI_ALLOW_VERBOSE_LOG=true`). |
| `smoke.sh` | Start, wait for readiness, check posture, one chat completion, tear down. |
| `hardening.md` | Every LocalAI admin, agent, gallery, p2p and web-UI surface, and how it is handled. |

## Run

Needs Docker Compose v2 (validated with Docker 29.7.2 / Compose 5.5.1) or Podman with `podman compose`.

```bash
cd deploy/container/localai
cp .env.example .env
#   edit .env: set LOCALAI_API_KEY to the output of  openssl rand -hex 32
mkdir -p data/models data/backends data/state       # Podman does not create bind-mount sources
docker compose -f compose.yaml -f compose.lite.yaml up -d
docker compose -f compose.yaml -f compose.lite.yaml logs -f localai
```

The first start downloads the backend image (about 50 MiB) and the model file (about 237 MB) before the API
binds, so `/readyz` is unreachable for a few minutes. The healthcheck allows 60 minutes. Later starts reuse
`data/backends` and `data/models`; the download is skipped when the file's sha256 matches. If the container
cannot reach the registries (a TLS-intercepting security product, a closed network), pull on the host instead:
[Backend and model pull from the host](#backend-and-model-pull-from-the-host).

Docker creates an empty `data/backends/alixai-backend-gallery.yaml` on the host: it is the mount point of the catalogue
next to it, not a backend. Leave it.

```bash
curl -fsS http://127.0.0.1:8080/readyz                                   # no key needed
curl -H "Authorization: Bearer $LOCALAI_API_KEY" http://127.0.0.1:8080/v1/models
```

Use `compose.yaml` alone for an unconstrained run. `./smoke.sh lite` runs the whole sequence and cleans up.

**Stop:**

```bash
docker compose -f compose.yaml -f compose.lite.yaml down       # keeps ./data
rm -rf data                                                    # also forgets models, backends, state
```

Podman: set `COMPOSE="podman compose"` for `smoke.sh`, or run the same commands with `podman compose`.

## Profiles (hardware emulation)

Override files add `mem_limit`, `memswap_limit` and `cpus`, plus `LOCALAI_THREADS` equal to the CPU cap and the default of
`LOCALAI_MAX_ACTIVE_BACKENDS`:

| File | Memory (swap off) | CPUs | Resident backends | Profile |
| --- | --- | --- | --- | --- |
| `compose.lite.yaml` | 8 GB | 4 | 2: controller, embedder | Lite certified target |
| `compose.balanced.yaml` | 16 GB | 8 | 4: controller, embedder, speech to text, text to speech | Balanced |
| `compose.4g.yaml` | 4 GB | 4 | 1: the controller (retrieval is lexical) | experimental 4 GB |
| `compose.stt-only.yaml` | 4 GB default (`STT_MEM_LIMIT`: 1g Lite, 8g large-v3) | 2 (raise with `STT_CPUS`) | 1: speech to text only | STT-only (no LLM) |
| `compose.yaml` alone | none | none | 2, the Lite shape | unconstrained run |

### STT-only profile and local STT ladder

Operators choose the local STT pack per deployment (`voice.active_pack` / install). Ladder (ADR 0040); all
larger options are **candidates** until voicebench passes — no default flip without gates:

| Profile | Pack | Engine | Notes |
| --- | --- | --- | --- |
| Lite | `en-stt` | whisper.cpp tiny | Qualified-emulated English input only |
| Balanced | `parakeet-stt` **or** `whisper-turbo-stt` | parakeet.cpp / whisper.cpp | Both candidate; CPU; GPU optional |
| Performance | `whisper-large-v3-stt`, `parakeet-stt`, or `whisper-turbo-stt` | whisper.cpp / parakeet.cpp | large-v3 is Performance-only (weights ~1.01 GB) |

Parakeet does **not** need NVIDIA hardware: `parakeet.cpp` is a ggml port that runs on CPU; CUDA/Metal/Vulkan
are optional acceleration (parakeet.cpp README). Whisper.cpp is the same story.

For speech recognition without a controller or embedder:

```bash
cd deploy/container/localai
cp .env.example .env   # set LOCALAI_API_KEY
# Parakeet (pack parakeet-stt) — default of compose.stt-only.yaml
LOCALAI_MAX_ACTIVE_BACKENDS=1 \
  docker compose -f compose.yaml -f compose.stt-only.yaml up -d
# Whisper large-v3-turbo (pack whisper-turbo-stt)
STT_MEM_LIMIT=4g LOCALAI_EXTERNAL_BACKENDS=whisper \
  STT_MODELS_CONFIG=/configuration/models.stt-only-turbo.yaml \
  LOCALAI_MAX_ACTIVE_BACKENDS=1 \
  docker compose -f compose.yaml -f compose.stt-only.yaml up -d
# Whisper large-v3 (pack whisper-large-v3-stt, Performance)
STT_MEM_LIMIT=8g STT_CPUS=4 LOCALAI_EXTERNAL_BACKENDS=whisper \
  STT_MODELS_CONFIG=/configuration/models.stt-only-large-v3.yaml \
  LOCALAI_MAX_ACTIVE_BACKENDS=1 \
  docker compose -f compose.yaml -f compose.stt-only.yaml up -d
# Lite Whisper tiny (en-stt)
STT_MEM_LIMIT=1g LOCALAI_EXTERNAL_BACKENDS=whisper \
  STT_MODELS_CONFIG=/configuration/models.stt-only-lite.yaml \
  LOCALAI_MAX_ACTIVE_BACKENDS=1 \
  docker compose -f compose.yaml -f compose.stt-only.yaml up -d
```

Pass `LOCALAI_MAX_ACTIVE_BACKENDS=1` on the command line (one STT worker). Do not put it in `.env` (that
would override every profile). The base image digest is unchanged; STT backends come from
`stt-only/backend-gallery.yaml`, not the default LLM catalogue. Validate with
`docker compose -f compose.yaml -f compose.stt-only.yaml config -q`.

- `memswap_limit` equals `mem_limit`, so the container gets no swap ("avoid swap").
- **Anything measured under these files is labelled `emulated`**, never `certified`. Each profile also sets the
  label `ai.alixai.evidence=emulated`. They cap this container, not a machine: a real 8 GB box also hosts the
  AlixAI core, SQLite and the OS, so M0-13 should judge headroom (at least 15% or 1 GB) against the cap
  minus those, not against the cap itself.
- `LOCALAI_MAX_ACTIVE_BACKENDS` is never lower than the models a profile keeps resident (`docs/operations/resource-budgets.md`
  rules R2 and R3). LocalAI evicts the least recently used backend when a request needs one that is not resident, so with
  fewer slots than resident models **every call reloads a model**: M0-15 measured 15.2 s for a chat after an embedding,
  6.2 s for an embedding after a chat and about 70 page faults a second on the 4 GiB profile with one slot. Do not set the
  variable in `.env` (a value there overrides every profile); to raise it, for a reranker or a decision scorer, check the
  memory projection of R3.2 first. `alixai serve` refuses a value below the number of models its own configuration keeps
  resident when the variable is in its environment (see "AlixAI readiness" below).
- `LOCALAI_THREADS` matches `cpus`; under a CPU quota llama.cpp would otherwise size its thread pool to the
  host's cores and be throttled.
- `docker compose up` fails if `cpus` exceeds the host's cores. Rootless Podman only enforces limits on cgroup v2
  with the memory and cpu controllers delegated to the user; swap limiting needs the swap controller.

## What is disabled, and why

Full table with exact settings and upstream links in [`hardening.md`](hardening.md). In short:

| Disabled | Setting | Why |
| --- | --- | --- |
| Web UI (React admin/runtime GUI) | `LOCALAI_DISABLE_WEBUI=true` | Not required (15.3); no admin UI exposed in production. Removing it also removes the `/api/settings` runtime-settings API. |
| Model and backend gallery, auto-install, remote warm-up | `LOCALAI_DISABLE_GALLERY_ENDPOINT=true`, `LOCALAI_AUTOLOAD_GALLERIES=false`, `LOCALAI_AUTOLOAD_BACKEND_GALLERIES=false`, `LOCALAI_GALLERIES=[]`, backend gallery = one local pinned file | No runtime download of unapproved code or models (16.1, 16.3). |
| MCP, CORS proxy, MCP jobs | `LOCALAI_DISABLE_MCP=true` | No arbitrary MCP registration or open proxy (16.1). |
| Agent pool, agent hub, LocalAI Assistant | `LOCALAI_DISABLE_AGENTS=true`, `LOCALAI_DISABLE_ASSISTANT=true` | AlixAI owns dialogue and actions; the runtime must not run agent loops (rules 5, 7). |
| P2P, federation, distributed mode | `LOCALAI_P2P=false`, `LOCALAI_FEDERATED=false`, `LOCALAI_DISTRIBUTED=false` | Off by default; set explicitly so a default change cannot enable them. |
| LocalAI user system, tracing, CORS | `LOCALAI_AUTH=false`, `LOCALAI_ENABLE_TRACING=false`, `LOCALAI_CORS=false` | One caller, one key; no body capture; no browser access. |
| Runtime writes to config | `./config` mounted `:ro` | LocalAI hot-applies `runtime_settings.json`, `api_keys.json`, `external_backends.json` from its config dir. |

Not removable in v4.10.0 and accepted (loopback, key-only, inert without a matching backend): fine-tuning and
quantization APIs, the stores/face/voice/TTS/video routes, and the anonymous discovery routes (`/swagger`,
`/api/instructions`, `/.well-known/localai.json`). A legacy LocalAI key is full admin, so the key is the only
barrier: keep it secret and the port on loopback.

## Models and backends

- **How a model arrives.** `config/models.yaml` lists models; LocalAI's own preload downloads each
  `download_files` entry into `data/models` before the API binds and verifies its sha256.
- **How a backend arrives.** `LOCALAI_EXTERNAL_BACKENDS` (default `cpu-llama-cpp`) names entries of
  `config/backend-gallery.yaml`; LocalAI pulls the digest-pinned image into `data/backends` at first boot. The catalogue is
  read from `file:///backends/alixai-backend-gallery.yaml`: v4.10.0 refuses a `file://` gallery outside its backends
  directory (`path is outside of trusted root`), so a gallery URL under `/configuration` never loads and no backend is
  installed (M0-13 finding F7). Seeing `Failed to download backend ... x509` in the log instead means the gallery loaded and
  only the network failed: use the host-side pull below.
- **Adding a model.** Pin the source revision (a commit, not `main`) and compute the file's sha256, then add an
  entry. For a Hugging Face file:

  ```bash
  curl -s https://huggingface.co/api/models/<owner>/<repo> | grep -o '"sha":"[0-9a-f]*"'      # repo commit
  curl -sI https://huggingface.co/<owner>/<repo>/resolve/<commit>/<file>.gguf | grep -i x-linked-etag   # sha256
  ```

  Restart. An entry without `sha256` is a policy violation (16.3); the file would never be re-verified.
- **Adding a backend.** Resolve the image digest, add an entry to `config/backend-gallery.yaml`, append its name to
  `LOCALAI_EXTERNAL_BACKENDS` in `.env`, restart. Registry lookup without a credential helper:

  ```bash
  T=$(curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:localai/localai-backends:pull" | sed -E 's/.*"token":"([^"]+)".*/\1/')
  curl -sI -H "Authorization: Bearer $T" -H "Accept: application/vnd.oci.image.index.v1+json" \
    https://registry-1.docker.io/v2/localai/localai-backends/manifests/<tag> | grep -i docker-content-digest
  ```
- Preload and backend-install errors are only logged by LocalAI, and `/readyz` still turns 200. If a model is
  listed but chat fails, search the log for `error downloading models` and `error installing external backend`.
  `smoke.sh` catches this by running a real completion.
- The backend's signature is not verified by LocalAI, only pinned by digest; the reason is in `hardening.md` section 4.

## Backend and model pull from the host

LocalAI pulls the backend image and the model files itself. That fails where the container's TLS does not trust the
network's certificate authority: on the M0-13 and M0-15 dev machine a TLS-intercepting security product re-signs every
HTTPS connection, and the log shows `Failed to download backend ... x509: certificate signed by unknown authority` (the
gallery itself loaded; only the pull failed). The fallback is to fetch on the host, whose daemon and curl trust what the
host trusts, and let LocalAI find the files already in place. Nothing is relaxed: the backend is the same digest-pinned
image, and every model file is the same sha256-pinned file; LocalAI re-checks both pins at every start.

**Backend.** Run from `deploy/container/localai` in a POSIX shell with Docker (on Git Bash for Windows write `$(pwd -W)`
for `$PWD` and prefix the `docker` commands that take a path with `MSYS_NO_PATHCONV=1`). Verified on Docker Desktop
(M0-18); the unpack runs inside a Linux container so the symlinks of `lib/` are created as symlinks:

```bash
BACKEND='quay.io/go-skynet/local-ai-backends@sha256:d7b0d9c69ecce93c2e7f7500ac3d99aba28648dab1ab29b698e0cc4538662579'  # uri of config/backend-gallery.yaml
docker pull "$BACKEND"                                   # the host's daemon; it verifies the content against the digest
docker create --name alixai-backend-extract "$BACKEND" true   # the image has no shell; the command is never run
mkdir -p data/backends/cpu-llama-cpp
docker export alixai-backend-extract | docker run --rm -i -v "$PWD/data/backends:/out" alpine:latest \
  tar -x -C /out/cpu-llama-cpp --exclude .dockerenv --exclude dev --exclude etc --exclude proc --exclude sys
docker rm alixai-backend-extract
printf '{"alias":"llama-cpp","name":"cpu-llama-cpp","gallery_url":"file:///backends/alixai-backend-gallery.yaml","installed_at":"%s","version":"v4.10.0","uri":"%s","digest":"%s"}\n' \
  "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$BACKEND" "${BACKEND#*@}" > data/backends/cpu-llama-cpp/metadata.json
```

The `--exclude` list leaves out what a container export adds (`.dockerenv`, `dev/`, `etc/`, `proc/`, `sys/`); the
directory must end up holding `run.sh`, `llama-cpp-grpc`, `llama-cpp-cpu-all`, `llama-cpp-rpc-server`, `lib/` and
`metadata.json`. `metadata.json` has the exact fields LocalAI writes itself
(recorded during the M0 spike); `uri` and `digest` must equal the catalogue entry, and
`gallery_url` is the trusted path. LocalAI skips installing a backend that is already present, so the first start
goes straight to the model preload. When the pin moves (see "Bumping LocalAI"), delete `data/backends/cpu-llama-cpp` and
repeat with the new digest.

**Model.** Download the commit-pinned URL of the entry in `config/models.yaml` and compare the file's hash with its pin
before the first start (`tests/live/spike/localai/fetch-models.sh` does this for every file of the spike):

```bash
mkdir -p data/models
curl --fail --location --retry 5 -o data/models/granite-4.0-350m-Q4_K_M.gguf \
  https://huggingface.co/ibm-granite/granite-4.0-350m-GGUF/resolve/b8208a86a58427e1739265318028eb5895b74bf2/granite-4.0-350m-Q4_K_M.gguf
echo '771c588a49607f274a2bba3185733607ebe6f74b996ab90e2d6bee0d98bcec52  data/models/granite-4.0-350m-Q4_K_M.gguf' | sha256sum -c -
```

A file whose hash differs from its pin is **deleted** by LocalAI at start and downloaded again (hardening-live.md section
1 item 2b), which fails here; so leave in place only a file that matches. `compose.readonly-models.yaml` makes the
deletion impossible.

## Guards in `entrypoint.sh`

Each refusal is exit 78 with a message that names the fix. Two switches exist; both default off, both are guard
settings and not LocalAI settings.

| Guard | Refuses when | Switch |
| --- | --- | --- |
| API key | empty, under 32 characters, whitespace, a comma, or the `.env.example` placeholder | none |
| Data directories | `/backends` or `/data` is not a writable directory | none |
| Models directory | `/models` is not a writable directory | `ALIXAI_MODELS_READONLY=true` makes a **read-only** `/models` the required state: it is refused when it is writable. `compose.readonly-models.yaml` sets it together with the `:ro` mount. A read-only `/models` without the switch is still refused. |
| Config directory | `/configuration` is writable | none |
| Log level | `LOCALAI_LOG_LEVEL` is `debug` or `trace`, or `LOCALAI_DEBUG` / `DEBUG` is on: at those levels LocalAI logs the model's completion in full (the prompt never), see HC-34 | `ALIXAI_ALLOW_VERBOSE_LOG=true`, for a throw-away development run; it prints a warning. A debug compose override has to set it beside `LOCALAI_LOG_LEVEL`. |

## AlixAI readiness: the warm-up, the first call and the resident backends

`alixai serve` adds three things in front of this runtime (`internal/cli/cmd_serve_readiness.go`,
`internal/gateway/readiness.go`; M0-13 findings F4 to F6, M0-15):

- **`/readyz` says whether the model answered.** LocalAI's own `/readyz` answers 200 with no backend installed and with a
  model that cannot load. AlixAI's `/readyz` has a check `inference_warmth`: the first evaluation sends one short constant
  chat to the controller (a one-word prompt, 4 output tokens, no tenant data) and waits up to 1.5 s for it. A model that is
  still loading is reported `warming up` (503, with Retry-After) until the call returns; one that cannot answer is reported
  by error code and retried every 10 s. A warm model is not called again by `/readyz`, so an anonymous caller cannot make
  the runtime load or answer repeatedly. Point the AlixAI service's healthcheck at `/readyz` and let anything that needs
  inference wait for `service_healthy`. The warm-up is driven by readiness polls, not by start: nothing generates before
  something asks.
- **The first call has a longer budget.** A user turn has 60 s. The warm-up call has 3 minutes (the 60 s plus 2 minutes
  for a cold load): M0-13 measured cold loads of 8 to 50 s, 50.4 s for the first request after a start, and the first
  end-to-end attempt timed out at 60 s. A runtime that went unavailable and came back has restarted and unloaded the model;
  it is warmed again. After the runtime's **idle unload** (`LOCALAI_WATCHDOG_IDLE_TIMEOUT`, 15 minutes) the next user turn gets the
  same cold-load budget as the warm-up: the service remembers when the controller last answered and, once that is older than two thirds of
  the idle timeout (10 minutes by default), gives the next turn the 3 minute budget instead of 60 s (a reload measured 5.8 to 14.6 s with a
  warm host cache, M0-15; M0-19).
  LocalAI's idle watchdog is the only unloader: AlixAI's resource governor does not unload models until the adapter can report memory and unload (M3-30).
- **`localai.models.controller` must be a verified registry id.** The name AlixAI sends must be one of `inference.models`
  (verified against the registry's hash) that serves a controller role. Free text, another registry entry that was not
  verified, or an embedding model is refused at start. So **a model that `alixai serve` is to use has a LocalAI model
  definition named by its registry id** (`tests/live/spike/localai/models.yaml` is the template: one entry per registry id,
  the registry's URL and sha256). The shipped `config/models.yaml` carries only the smoke model `alixai-smoke`, which
  exists for `smoke.sh`; it is not a registry id, so `alixai serve` cannot address it, on purpose. Do not add a second
  entry for the same file: LocalAI hashes every listed model at each start, so list only the models a profile uses.
- **`LOCALAI_MAX_ACTIVE_BACKENDS` must cover the resident models.** `alixai serve` counts the distinct model names of
  `localai.models` (controller, router, embedder) and refuses to start when the variable is in its own environment and
  lower; a negative or non-integer value is refused too; `0` is LocalAI's unlimited and passes with a warning, because
  HC-29 wants a bound. When the variable is not visible to AlixAI (it belongs to the LocalAI container) and more than one model
  is resident, it logs that it cannot check. Pass the same `.env` value to both services.

## Bumping LocalAI

1. Read the release notes (`gh api repos/mudler/LocalAI/releases/latest --jq .body`) and diff `core/cli/run.go` and
   the route registrations between the tags (`hardening.md` section 9).
2. Resolve the new image index digest (registry `manifests/<tag>`, header `docker-content-digest`) and the matching
   `<tag>-cpu-llama-cpp` backend digest; confirm the same digest on both registries.
3. Update `compose.yaml`, `config/backend-gallery.yaml`, the table above (tag, digest, date).
4. Delete `data/backends/cpu-llama-cpp` (LocalAI skips installing a backend that is already present) and restart; where the
   container cannot pull, repeat the host-side pull above with the new digest.
5. Run `./smoke.sh` and the M0-13 checks.

Offline config check (what `plan verify` runs):

```bash
docker compose --env-file .env.example -f compose.yaml -f compose.lite.yaml config -q
```

## Docker and Podman

Only plain Compose-spec keys are used: top-level `name`, and per service `image`, `container_name`, `entrypoint`,
`command`, `ports` (short syntax), `environment`, `volumes` (short syntax), `healthcheck`, `restart`,
`stop_grace_period`, `security_opt`, `cap_drop`, `cap_add`, `labels`, `mem_limit`, `memswap_limit`, `cpus`. No `deploy:`, no `x-` extensions, no `profiles:`, no `depends_on` ordering, no `init:`.
Validated with Docker Compose only; **Podman was not available to test**, so these are the points to check there:

- bind-mount sources must exist (`mkdir -p data/models data/backends data/state`); the catalogue is a file mounted inside the
  `data/backends` bind mount, for which the runtime creates a placeholder file;
- `image: name:tag@sha256:...` pulls by digest and ignores the tag (Docker does; confirm on the Podman version in use);
- on SELinux hosts (Fedora, RHEL) add `:z` to the bind mounts (an override file is enough) or the container gets
  permission errors;
- `cap_drop: [ALL]` with `cap_add: [CHOWN, DAC_OVERRIDE, FOWNER]` is plain Compose spec; under rootless Podman
  container root maps to your user, so the added capabilities should not be needed for `./data`.

Production on Linux is intended to be rootless Podman under Quadlet/systemd; translating this compose file is a
later task, not part of M0-08.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `required variable LOCALAI_API_KEY is missing a value` | No `.env`. `cp .env.example .env`, set the key. |
| Container exits at once with `alixai-localai: refusing to start: ...` | The guard in `entrypoint.sh`. Fix what the message names: key under 32 characters, placeholder, whitespace or comma; data dir not writable; config dir writable. |
| `... is not a writable directory` on Linux | The host directory is not writable by the container's root. `DAC_OVERRIDE` is already added, so check for SELinux (add `:z` to the bind mounts) or a read-only or network filesystem under `./data`. |
| `/readyz` unreachable for minutes | First-boot download (backend, then model). Watch `logs -f localai`. |
| `gallery source unreachable ... path is outside of trusted root`, then chat answers `500 backend not found: llama-cpp` | The catalogue is not under `/backends`. The shipped files mount it at `/backends/alixai-backend-gallery.yaml` and point `LOCALAI_BACKEND_GALLERIES` there; an override that changes either must keep the two together. |
| `Failed to download backend ... x509: certificate signed by unknown authority` | The container's TLS does not trust the network's CA (a TLS-intercepting product). Pull on the host: "Backend and model pull from the host". |
| `refusing to start: /models is not a writable directory ... ALIXAI_MODELS_READONLY=true` | `/models` is read-only and the switch is off. Use `compose.readonly-models.yaml`, or make the directory writable. |
| `refusing to start: ALIXAI_MODELS_READONLY=true but /models is writable` | The switch is on without the `:ro` mount. Add the mount (`compose.readonly-models.yaml`) or unset the switch. |
| `refusing to start: LOCALAI_LOG_LEVEL=debug would put every model completion in the container log` | HC-34. Set the level back to `info`; for a throw-away development run also set `ALIXAI_ALLOW_VERBOSE_LOG=true`. |
| AlixAI `/readyz` is 503 with `inference_warmth` `warming up` | The first call is loading the model (8 to 50 s measured). It turns ready when the call returns; a reason that says `did not answer` names the error code, and the container log has the detail. |
| `alixai serve` refuses to start: `LOCALAI_MAX_ACTIVE_BACKENDS=1 is lower than the 2 models` | More models are configured resident than the runtime may keep loaded: every call would reload one. Raise the value (the profile default is right) or drop the embedder from `localai.models`. |
| `alixai serve` refuses to start: `localai.models.controller ... is not a verified registry id` | Set it to the registry id listed in `inference.models`. |
| `/v1/models` lists `alixai-smoke` but chat returns an error | The model file did not download or its hash did not match; see `error downloading models` in the log. |
| `401` on every call | Missing or wrong `Authorization: Bearer` key. Only `/readyz` and `/healthz` are anonymous. |
| `404` on `/app`, `/models/available`, `/api/settings` | Expected: those surfaces are disabled. |

## What has been run live

- **M0-13** ran the stack against the pinned release on Docker Desktop (lite and balanced caps, x86-64, emulated) and
  re-verified every item of `hardening.md` section 8 and the live rows of the hardening checklist:
  [`docs/evidence/m0-spike/hardening-live.md`](../../../docs/evidence/m0-spike/hardening-live.md). It found that `smoke.sh`
  failed as shipped because of the gallery path (finding F7).
- **M0-18** fixed that. With the catalogue mounted at `/backends/alixai-backend-gallery.yaml` the log shows
  `installing OCI backend without signature verification` for the pinned digest (the gallery loads; the pull then fails on
  the TLS interception of that machine, which the host-side pull above avoids), and `./smoke.sh lite` passes with the backend
  installed from the host: ready after 16 s, 401 without a key, 404 for the disabled surfaces, the model listed, one chat
  completion with content. `compose.readonly-models.yaml` and every refusal and switch of `entrypoint.sh` were run in
  throw-away containers of the pinned image.
- **Not run:** native Linux, rootful Docker and rootless Podman on Linux, ARM64, another OS user's access to the port,
  strict backend integrity with a real verification policy, a verified in-container model download, the 6-hourly
  upgrade check. They are listed in `hardening-live.md` section 6.
