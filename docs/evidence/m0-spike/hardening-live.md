# M0-13: LocalAI hardening, re-verified live against the pinned release

> The raw results, logs, scripts and configuration files this report names are not published with the source code; their
> names are kept as a record of how the figures were produced.

Pinned release: LocalAI **v4.10.0** (`docker.io/localai/localai@sha256:0632c21d...`, backend `cpu-llama-cpp` at
`quay.io/go-skynet/local-ai-backends@sha256:d7b0d9c6...`). Everything here ran on one machine: Windows 11, Docker Desktop (Linux
VM), i7-10700K, lite profile (8 GiB, 4 CPUs) unless stated, no GPU. It is **emulated** and it is **not Linux**: Docker Desktop
presents Windows bind mounts as root-owned and world-writable, so every permission and capability result below holds for that
platform only. **A native Linux host and rootless Podman were not tested** (the permission matrix in section 4 reproduces Linux
file-permission semantics, which is all it can do). Scripts: `tests/live/spike/localai/` (`hardening-experiments.sh`,
`linux-permission-matrix.sh`, `egress-capture.sh`, `hc-static-check.mjs`); logs: `docs/evidence/m0-spike/logs/hardening-*.log`.

Avast on this machine intercepts TLS: from inside the container every HTTPS connection fails with `x509: certificate signed by
unknown authority` (huggingface.co and quay.io alike), plain HTTP works. That is a property of this machine, but it decided what
could be observed (items 2 and 3 below).

## 1. `deploy/container/localai/hardening.md` section 8

| # | Item | Result | Evidence |
|---|---|---|---|
| 1 | `smoke.sh` passes under lite | **Fails as shipped, passes with one fix.** The shipped `LOCALAI_BACKEND_GALLERIES=file:///configuration/backend-gallery.yaml` is refused by v4.10.0 (`gallery source unreachable ... path is outside of trusted root`; the downloader only reads a `file://` gallery from inside the backends directory), so the backend never installs and the first chat is `500 backend not found: llama-cpp`. With the gallery file mounted at `/backends/alixai-backend-gallery.yaml` and the URL changed to `file:///backends/alixai-backend-gallery.yaml` (`tests/live/spike/localai/compose.spike.yaml`) the smoke passes: 401 without key, 404 for `/app`, `/models/available`, `/api/settings`, `/v1/mcp/servers/x`, the model listed, one chat with content, ready after 44 s. The fix belongs in `deploy/` (outside this task's scope) | `logs/smoke-shipped-as-is.log`, `logs/smoke-with-spike-overrides.log` |
| 2a | Backend install from the `file://` gallery, OCI digest reference | **Works with the fix above.** LocalAI wrote `data/backends/cpu-llama-cpp/metadata.json` with `gallery_url file:///backends/alixai-backend-gallery.yaml`, `uri quay.io/...@sha256:d7b0d9c6...`, `digest sha256:d7b0d9c6...`. It logs `installing OCI backend without signature verification`. The install error path only logs (`error installing external backend`), and the service still reports ready (see finding F2) | `backend-install-metadata.json`, `egress/capture-no-backend.txt` |
| 2b | Model preload verifies sha256 | **Verified, as enforcement and as cost.** At debug level every model file logs `File already exists and matches the SHA. Skipping download` (source `pkg/downloader/uri.go` L650-L660). A file whose hash differs is **deleted** and re-downloaded: with a wrong pin on a copy of the model, the copy was gone after start, the re-download failed (TLS), the model was unavailable and a chat answered 500: fail closed, but destructive. Cost: the preload hashes every file listed in `models.yaml` on every start (about 35 s for 3.2 GB in this VM, `Preloading models` to ready), so list only the models a profile uses. A successful **verified download** from huggingface.co could not be observed here (TLS interception); the files were fetched on the host and re-hashed with `models.HashFile` (all 7 match their pins: `model-hashes.json`) | `logs/hardening-wrong-sha.log`, `logs/hardening-trace-boot.log` |
| 3 | `/configuration` read-only, reduced capabilities | **Works on Docker Desktop for every variant tried** (all with `/configuration` `:ro`, nothing written there; `/data` gets `.local_user_id`, `traces/`, `voice-profiles/` and no `runtime_settings.json`): shipped set (CapEff `0x0b`), **all capabilities dropped and none added**, `DAC_OVERRIDE` only, non-root `user: 1000:1000` (the image has user `ubuntu`, uid 1000), read-only root filesystem with `tmpfs` for `/tmp` and `/run/localai`, `pids_limit: 512` and a 10 MB log cap (`docker diff` shows only mount points), `/models` mounted read-only (with the image's own entrypoint: the shipped guard refuses a read-only `/models` by design). **What each capability is for, with Linux semantics** (`linux-permission-matrix.sh`, ext4 volume, dir owned by uid 1000 mode 0755): root with no capability cannot write there; `DAC_OVERRIDE` alone can; `FOWNER` only allows `chmod` of foreign files; `CHOWN` only `chown`. LocalAI itself never needed `CHOWN` or `FOWNER` in anything that ran; the one path that might (unpacking a backend image) could not be run without the network (`logs/hardening-no-caps-install.log`: the pull fails on TLS). Verdict: **keep `DAC_OVERRIDE` where the container runs as root on host-owned directories; `CHOWN` and `FOWNER` are unused in every path exercised and unproven for the unpack path; a non-root container with directories owned by uid 1000 needs none**. Rootful Docker and rootless Podman on Linux: not tested | `logs/hardening-no-caps.log`, `...-only-dac-override.log`, `...-nonroot.log`, `...-readonly-rootfs.log`, `...-readonly-models.log`, `...-linux-permission-matrix.log` |
| 4 | Strict backend integrity refuses the pinned image | **Yes.** `LOCALAI_REQUIRE_BACKEND_INTEGRITY=true` on an empty backends directory: `strict integrity: gallery "alixai-pinned" has no verification policy for OCI backend "cpu-llama-cpp" (set verification: in the gallery YAML or disable --require-backend-integrity)`. No backend installs, the service still reports ready, a chat answers `500 backend not found`. So strict mode is **unusable** for this gallery until a `verification:` policy and a Sigstore bundle exist for the image (hardening.md section 4); a run with such a policy was not attempted | `logs/hardening-strict-integrity.log` |
| 5 | Only `127.0.0.1:8080` is exposed | **Yes.** `docker port` shows `8080/tcp -> 127.0.0.1:8080` and the host's only listener on 8080 is `127.0.0.1:8080`; the host's LAN address (192.168.1.36:8080) refuses. Inside the container the listeners are `[::]:8080` (LocalAI, all interfaces of the container namespace, needed for the port mapping), `127.0.0.1:<random>` (the llama-cpp gRPC backend, loopback only) and Docker's resolver `127.0.0.11`; nothing else | `logs/hardening-baseline.log`, `logs/hardening-host-listeners.log` |
| 6 | The egress list matches a capture | **Mostly, with one undocumented contact.** Three captures (a sidecar `tcpdump` in the container's network namespace): first boot without the backend: DNS and TCP 443 to `quay.io`, `index.docker.io` (backend digest pull) and `huggingface.co`; boot with the backend and models present: **only `huggingface.co`** (16 DNS lookups, 8 TCP connections to its CDN in about 40 s, during the preload). No telemetry or other destination appeared; every DNS query went to 8.8.8.8. The Hugging Face contact with all files present and hash-correct is not in hardening.md section 6, is not logged at info level and is not needed (an `internal: true` boot works, section 3); its trigger was not identified (the downloader source skips the network for a matching file). The 6-hourly backend upgrade check was not observed (captures are 75 to 90 s). A network-level block is therefore the control, as section 6 already says | `egress/summary.txt`, `egress/capture-*.txt` |

## 2. `docs/security/localai-hardening-checklist.md` section 5 (live verification)

| # | Check | Result |
|---|---|---|
| 1 | Only loopback lists the port | Yes: `netstat -ano` shows `127.0.0.1:8080` (the Docker backend process) and no other 8080 listener (`logs/hardening-host-listeners.log`) |
| 2 | Not reachable from elsewhere | From the host's LAN address: refused. From another OS user: not tested |
| 3 | No key, no service | Yes: of 33 probed routes every one except the documented exempt ones answers 401 without the key (`surface-probe.json`). Exempt and answering 200: `/readyz`, `/healthz`, `/swagger/index.html`, `/api/instructions`, `/.well-known/localai.json` and **`/api/auth/status`**, which `core/http/auth/public_routes.go` also lists as public (it reports `authEnabled:false`); HC-11's list should name it, and the `/api/branding`, `/branding/asset/`, `/login`, `/invite/`, `/favicon.svg` routes of that file if the web UI were on |
| 4 | Disabled surfaces are gone, even with the key | Yes (404): `/`, `/app`, `/browse`, `/api/settings`, `/models/available`, `/models/apply`, `/models/import-uri`, `/backends*`, `/api/agents`, `/api/agent/tasks`, `/v1/mcp/servers/x`, `/v1/mcp/chat/completions`, **`/api/cors-proxy`** (so `LOCALAI_DISABLE_MCP=true` does remove the CORS proxy: HC-16's open point), `/api/nodes`. Present with the key and recorded, not disabled: `/metrics`, `/system`, `/api/p2p` (empty lists), `/api/fine-tuning/jobs`, `/api/quantization/jobs` (no off switch in v4.10.0), `/stores/get` (405), `/backend/monitor` (500 for an unknown model). `/api/traces` and `/api/backend-traces` exist (200) but return `[]` after a chat request was made: tracing is off, the routes are inert |
| 5 | AlixAI calls a subset of HC-37 | Yes: through `alixai serve` the runtime saw only `/readyz`, `/v1/chat/completions`, `/v1/models`, `/version` (`e2e.json`, `runtime_paths_called_by_alixai`; none outside the allowlist) |
| 6 | Outbound fails on the production network | Yes on an `internal: true` network with nothing published: `Could not resolve host`, no DNS, from both the LocalAI container and a peer; LocalAI booted, loaded a model and answered a chat from a peer container (200 with the key, 401 without). On the shipped default network DNS resolves and plain HTTP to the internet works (HTTPS fails on the Avast certificate) |
| 7 | Identity | The shipped container runs as **root** (`uid=0`); `user: 1000:1000` works (item 3 above). Only the stack's own bind mounts exist (models, backends, data and configuration directories, the entrypoint guard and, in the spike, the gallery and models files; `logs/hardening-baseline.log`, `mounts`); AlixAI's data directory is not mounted. "The reverse" (AlixAI reading LocalAI's data) is a host permission, not tested |
| 8 | Idempotent restart | Yes: after `docker restart` the same status codes (401, 404, 404, 404) and no `runtime_settings.json` or other file appeared in `/configuration` or `/data` (`logs/hardening-restart.log`) |

## 3. Network placement (HC-05)
`experiments/internal-network.yaml` (`internal: true`, no `ports`) works once models and backend are on disk, with the preload
hashing local files only. A first boot on such a network cannot fetch anything: provisioning must happen before the network is
closed, as the checklist says.

## 4. Rows of the checklist against v4.10.0 (`hc-static-check.json`)
`hc-static-check.mjs` reads the effective compose configuration (`docker compose config`, all overrides applied) and the pinned
`core/cli/run.go` and `core/cli/context/context.go`. **All 44 settings the checklist names exist in
v4.10.0** (231 settings declared; none renamed; `LOCALAI_DEBUG` and `DEBUG` live in `context/context.go`, not `run.go`).
Four rows are not met by the deployment as shipped, each for a reason that needs an exception entry (checklist section 6) or a
change:

| Row | Checklist says | Deployment | Verdict |
|---|---|---|---|
| HC-14 | backend gallery list unset or `[]` | one `file://` gallery with the digest-pinned backend | needed to install the backend; record an exception, and fix the path (item 1) |
| HC-18 | `LOCALAI_EXTERNAL_BACKENDS` absent | `cpu-llama-cpp` | it is the install mechanism; record an exception (a shipped backend baked into the image would remove it) |
| HC-20 | `LOCALAI_OPAQUE_ERRORS=true` if the error mapping survives | unset | **keep it off, record the exception.** Measured (`error-mapping-default.json` vs `error-mapping-opaque-errors.json`): with opaque errors the model-load failure and the context overflow are both a **blank 500**, and `alixai serve` maps both to `internal`; with them off the same failures map to `resource_exhausted` (HTTP 503) and `validation_failed` (HTTP 400). Under the 4g profile a real cgroup OOM kill of the backend (`m0-oom`, 131072-token window, `OOMKilled=true`) answered `500 ... code = Unavailable desc = error reading from server: EOF` and mapped to `resource_exhausted` (`error-mapping-4g.json`) |
| HC-21 | `LOCALAI_DISABLE_METRICS_ENDPOINT=true` | unset, `/metrics` answers 200 with the key | either set it or record an exception (hardening.md section 3 keeps it deliberately) |

Open points of checklist section 7 settled here: **HC-03** v4.10.0 supports systemd socket activation (`run.go` L259
`systemdActivatedListeners`, L809 "Using systemd socket activation listener"), no `unix:` address flag was found, so a private
socket is possible on native installs through systemd (source reading, not run). **HC-16** yes, the CORS proxy goes with MCP.
**HC-20** above. **HC-27** strict integrity exists and refuses an unsigned image (item 4). **HC-34** boots at `info`, `debug` and `trace` each handled two chats: one whose prompt carried a canary string, one asking the model to repeat a second canary (`logs/hardening-info-log.log`, `-debug-log.log`, `-trace-log.log`; the log is read with ANSI colour codes stripped, matched case-insensitively because the 350M model answers `Canary-...`). **The prompt is not logged at any level** (0 matches in stdout and in `/data`, `/tmp`, `/models`, `/configuration`, `/backends`; `/data/traces` stays empty), and the request line (`HTTP request method="POST" path="/v1/chat/completions" status=200`, no body) is present at every level as the control. **The model's answer is logged at `debug` and `trace`**: a `DEBUG Response response="{...\"content\":\"Canary-DEBUG-ECHO-0a7e\"...}"` line carries the whole completion (1 matching line at each level; none at `info`). So the HC-34 rule, "never run above info", has a concrete reason: above it, every model output (which can hold the user's own data, a proposal's field values, a refusal) goes to the container log. An earlier version of this probe grepped `chat/completions.*status=200` on the coloured log, matched nothing (the control read 0) and could not support a conclusion; it was replaced. **HC-24** a read-only `/models` runs
(item 3) and, because a mismatching file is deleted at start (item 2b), is worth having; the shipped guard rejects it and needs a
switch. **HC-33** read-only root filesystem, PID limit, log cap, `no-new-privileges` (observed `NoNewPrivs: 1`, `Seccomp: 2`) and a
non-root user all run. Not examined: **HC-26** (remote-code trust field), **HC-36** (prompt-cache timing channel, M5-12).

## 5. Findings that are not in the documents yet
- **F1.** The shipped backend gallery path is outside LocalAI's trusted root: `smoke.sh` fails as shipped (item 1).
- **F2.** `/readyz` answers 200 with no backend installed and with a model that cannot load. It is anonymous and says nothing
  about inference. AlixAI's own health check must exercise a model (the adapter's readiness already lists models; a real
  completion is the only proof).
- **F3.** LocalAI deletes a model file whose sha256 differs from the pin at start, then tries to re-download it (item 2b).
- **F4.** Every start hashes every listed model: about 35 s of startup for 3.2 GB here.
- **F5.** First request after a model switch pays the cold load (`LOCALAI_MAX_ACTIVE_BACKENDS=1`): 9.6 s to 50 s for the 0.8B
  model in this VM, longer than the gateway's 60 s budget on the first attempt (`logs/e2e-cold-start-timeout.log`).
- **F6.** The Hugging Face contact at boot with everything provisioned (item 6).
- **F7.** `LOCALAI_AUTH=false` still serves `/api/auth/status` anonymously (item 3 of section 2).
- **F8.** At `debug` and `trace` LocalAI writes every completion body to the container log (`DEBUG Response response="{...}"`); the prompt is never
  logged. HC-34 should say why the log level stays `info` and that container logs above `info` hold model output (section 4).

## 6. Not done
Native Linux, rootful Docker on Linux, rootless Podman; another OS user's access; `strict` integrity with a real verification
policy; a verified in-container model download (blocked by TLS interception on this machine); backend unpack without
capabilities; the 6-hourly upgrade check; HC-26 and HC-36.
