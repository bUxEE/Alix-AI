# LocalAI hardening (pinned v4.10.0)

Scope: every LocalAI admin, agent, gallery, p2p and web-UI surface, and what this deployment does
about it.

Everything below was established by reading the LocalAI **v4.10.0** source and docs (links are pinned to
that tag), validating the compose files with `docker compose config`, and then **running the pinned release**:
task M0-13 re-verified it live on Docker Desktop ([`docs/evidence/m0-spike/hardening-live.md`](../../../docs/evidence/m0-spike/hardening-live.md)),
and M0-18 fixed what that run found. Section 8 records the outcome of each live item and what was not run.
`smoke.sh` asserts the checkable parts (401 without the key, disabled routes answer 404, the model is listed,
one chat completion returns content) and passes under `lite` since M0-18.

## 1. Posture

- One caller. Only the AlixAI core talks to LocalAI. LocalAI is the private runtime, not an AlixAI interface.
- Published as `127.0.0.1:8080:8080` only (`compose.yaml`). No other port, no UDP, no host networking.
  Inside the container LocalAI binds `:8080` (`LOCALAI_ADDRESS`), which is required for the port mapping.
- One shared key in `LOCALAI_API_KEY`, sent as `Authorization: Bearer <key>`. **A legacy LocalAI API key is full
  admin; there is no role separation** ([authentication](https://localai.io/features/authentication/)). That is
  why loopback-only, a single key, and disabling the admin surfaces below all matter: the key is the only barrier.
- LocalAI's own user system (`LOCALAI_AUTH`, GitHub/OIDC) is not enabled: it would add a database, OAuth
  callbacks and registration endpoints for a runtime with exactly one caller. The OAuth/OIDC variables are not
  forwarded into the container.
- `entrypoint.sh` (guard in front of the image's own entrypoint) refuses to start, exit 78, when: the key is
  missing, shorter than 32 characters, contains whitespace or a comma (a comma makes LocalAI accept several
  keys), or is the `.env.example` placeholder; when `/models`, `/backends` or `/data` is not writable (`/models`
  alone may be read-only, and only with the explicit switch `ALIXAI_MODELS_READONLY=true`, which in turn refuses a
  `/models` that is writable); when `/configuration` is writable; or when the log level is `debug` or `trace`
  (`LOCALAI_LOG_LEVEL`, or `LOCALAI_DEBUG` / `DEBUG`) without the explicit development switch
  `ALIXAI_ALLOW_VERBOSE_LOG=true`: at those levels LocalAI logs every completion in full (section 3, HC-34).
- Compose interpolation fails closed too: `${LOCALAI_API_KEY:?...}` aborts `docker compose` when no key is set.
- Constant-time key comparison: `LOCALAI_SUBTLE_KEY_COMPARISON=true`.
- Never set: `LOCALAI_DISABLE_API_KEY_REQUIREMENT_FOR_HTTP_GET` (anonymous GETs),
  `LOCALAI_ALLOW_INSECURE_PUBLIC_BIND`, `LOCALAI_HTTP_GET_EXEMPTED_ENDPOINTS`. Compose forwards only the
  variables it lists, so a stray variable on the host cannot reach LocalAI.
- Public by upstream design even with a key (anonymous, loopback-only here): `GET /healthz`, `GET /readyz`,
  `GET /swagger/*`, `GET /api/instructions[/name]`, `GET /.well-known/localai.json`
  (see "Public discovery APIs" in [authentication](https://localai.io/features/authentication/)) and
  **`GET /api/auth/status`**, which `core/http/auth/public_routes.go` also lists as public (it answers
  `authEnabled:false` while `LOCALAI_AUTH` is off; M0-13 saw it answer 200 without a key). The same file lists
  `/api/branding`, `/branding/asset/`, `/login`, `/invite/` and `/favicon.svg`, which exist only with the web UI
  on. They expose the API shape, the version and the auth mode, not data. Everything else, including
  `GET /version` and generated-content URLs, requires the key. Measured: of 33 probed routes every one except
  these answers 401 without the key (the raw probe output is not published).

## 2. Surfaces disabled

Legend: **off** = a setting removes it; **not enabled** = off by default and set explicitly so a default change
cannot turn it on. Source column links are pinned to tag v4.10.0.

| Surface | What it is | Setting (in `compose.yaml`) | Upstream reference |
| --- | --- | --- | --- |
| Web UI | React SPA at `/app`, `/browse`, `/explorer`, `/` redirect, `/assets`, `/locales`, plus the UI JSON API and legacy views. Off: the runtime GUI is not required and this is a production profile. | `LOCALAI_DISABLE_WEBUI=true` | [CLI reference](https://localai.io/reference/cli-reference/) · [run.go L79](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L79) · [app.go L500, L596](https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/app.go#L596) |
| Runtime settings API | `GET/POST /api/settings`: change watchdog, CORS, API keys, p2p token, galleries, agent pool at runtime and persist to `runtime_settings.json`. It is registered inside the UI API block, so it is already gone with the web UI; the flag is a second lock. See the file-watcher caveat in section 3. | `LOCALAI_DISABLE_RUNTIME_SETTINGS=true` | [runtime settings](https://localai.io/features/runtime-settings/) · [ui_api.go L1906](https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/routes/ui_api.go#L1906) |
| Model and backend gallery endpoints | `POST /models/apply`, `/models/import`, `/models/import-uri`, `/models/edit/:name`, `/models/delete/:name`, `/models/reload`, `PUT /models/toggle-*`, `GET /models/available`, `/models/galleries`, `POST /backends/apply`, `/backends/delete/:name`, `/backends/upgrade/:name`, `GET /backends*`, `GET /api/aliases`. An API-key holder could install arbitrary models and backends (arbitrary code) at runtime. | `LOCALAI_DISABLE_GALLERY_ENDPOINT=true` | [model gallery](https://localai.io/models/) · [backends](https://localai.io/backends/) · [localai.go L45](https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/routes/localai.go#L45) · [run.go L113](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L113) |
| Gallery auto-install and warm-up | Loads the public model gallery at boot and pre-warms remote size and VRAM estimates (remote GGUF probes); auto-installs backends from the public backend gallery. | `LOCALAI_AUTOLOAD_GALLERIES=false`, `LOCALAI_AUTOLOAD_BACKEND_GALLERIES=false`, `LOCALAI_GALLERIES=[]`, `LOCALAI_BACKEND_GALLERIES` = only `file:///backends/alixai-backend-gallery.yaml` (the catalogue is mounted inside the backends directory, the only place v4.10.0 reads a `file://` gallery from), `LOCALAI_AUTO_UPGRADE_BACKENDS=false`; `LOCALAI_PRELOAD_MODELS`, `LOCALAI_PRELOAD_MODELS_CONFIG`, `LOCALAI_MODELS` unset | [CLI reference](https://localai.io/reference/cli-reference/) · [startup.go L468](https://github.com/mudler/LocalAI/blob/v4.10.0/core/application/startup.go#L468) · [run.go L53-L63](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L53) |
| MCP | `POST /v1/mcp/chat/completions` (+ `/mcp/...` aliases), `/v1/mcp/servers\|prompts\|resources/*`, `/api/cors-proxy` (an authenticated open HTTP proxy), and the MCP "CI jobs" API `/api/agent/tasks`, `/api/agent/jobs`. Network posture: no arbitrary MCP registration, CORS proxy or agent hub in production. | `LOCALAI_DISABLE_MCP=true` | [MCP](https://localai.io/features/mcp/) (section on disabling) · [localai.go L457, L502](https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/routes/localai.go#L457) · [run.go L114](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L114) |
| Agent pool and agent hub | `/api/agents/*` (agents, skills, collections, actions, per-agent chat/SSE), the built-in knowledge-base store, and the agent hub link (`LOCALAI_AGENT_HUB_URL`, `https://agenthub.localai.io`, used by the UI only). With the pool disabled the routes are not registered. | `LOCALAI_DISABLE_AGENTS=true` | [agents](https://localai.io/features/agents/) · [agents.go L13](https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/routes/agents.go#L13) · [run.go L128, L147](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L128) |
| LocalAI Assistant | In-process admin MCP server exposed as a chat modality ("manage this LocalAI by chatting"). | `LOCALAI_DISABLE_ASSISTANT=true` | [assistant](https://localai.io/features/localai-assistant/) · [run.go L125](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L125) |
| P2P | Libp2p/DHT networking, token generation, llama.cpp RPC worker sharing. `GET /api/p2p` and `/api/p2p/token` stay registered but return empty lists and an empty string while no token is set. | not enabled: `LOCALAI_P2P=false`; `LOCALAI_P2P_TOKEN` and `LOCALAI_P2P_NETWORK_ID` unset. Hazard: a bare `TOKEN` environment variable is an alias of the p2p token ([run.go L93](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L93)); compose does not forward it, and `runtime_settings.json` cannot set it (section 3). | [distribute](https://localai.io/features/distribute/) · [CLI reference P2P flags](https://localai.io/reference/cli-reference/) · [localai.go L274](https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/routes/localai.go#L274) |
| Federation | Federated instance mode load-balancing across p2p nodes. Also the `federated`, `explorer` and `p2p-worker` subcommands, which are never run: the container command is `run`. | not enabled: `LOCALAI_FEDERATED=false` | [distribute](https://localai.io/features/distribute/) · [run.go L112](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L112) |
| Distributed mode | Multi-node control plane (PostgreSQL + NATS), `/api/node/*` registration, `/api/nodes/*` admin. Routes are only registered when a node registry exists. | not enabled: `LOCALAI_DISTRIBUTED=false`; no NATS, storage or registration-token variables | [distributed mode](https://localai.io/features/distributed-mode/) · [nodes.go L40, L65](https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/routes/nodes.go#L40) |
| LocalAI user system | Accounts, sessions, GitHub/OIDC login, per-user API keys, `/api/auth/*` registration and login. | not enabled: `LOCALAI_AUTH=false`; `GITHUB_CLIENT_ID`, `LOCALAI_OIDC_*` not forwarded | [authentication](https://localai.io/features/authentication/) · [run.go L150](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L150) |
| Tracing | In-memory capture of request and response bodies, `GET /api/traces`, `/api/backend-traces`. Prompts and tenant data must not be retained by the runtime. | not enabled: `LOCALAI_ENABLE_TRACING=false`; log level `info` (at `debug` and `trace` the completion is logged in full, section 3) | [tracing](https://localai.io/features/tracing/) · [run.go L117](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L117) |
| Browser CORS | Cross-origin browser access to the API. AlixAI calls server to server. | not enabled: `LOCALAI_CORS=false` (also the default) | [CLI reference](https://localai.io/reference/cli-reference/) · [run.go L72](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L72) |
| Cloud-proxy MITM listener | TLS-intercepting HTTP proxy on a second port for cloud-model traffic. | not enabled: `LOCALAI_MITM_LISTEN` unset (empty means disabled) | [run.go L202](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L202) · [startup.go L511](https://github.com/mudler/LocalAI/blob/v4.10.0/core/application/startup.go#L511) |
| Realtime WebRTC UDP port | Extra UDP port for `/v1/realtime` WebRTC ICE traffic. | not enabled: `LOCALAI_WEBRTC_UDP_PORT` unset; no UDP published | [run.go L41](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L41) |
| Ollama root endpoint | Replaces `/` with an Ollama-style health response. | not enabled: `LOCALAI_OLLAMA_API_ROOT_ENDPOINT` unset | [run.go L80](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L80) |
| Private-source credentials | Registry/gallery/HTTP credentials file and `HF_TOKEN`. Not needed: the pinned sources are public. | not enabled: `LOCALAI_CREDENTIALS_FILE` and `HF_TOKEN` not forwarded | [private sources](https://localai.io/advanced/private-sources/) |
| Opaque errors | Replaces every error body with a blank 500. Deliberately **not** enabled, and measured live (M0-13, HC-20): with opaque errors a model that cannot load and a prompt over the context window are both a blank 500, which `alixai serve` can only map to `internal`; without them they map to `resource_exhausted` (503) and `validation_failed` (400). The gateway needs real status codes (401, 404, 429, 503 with Retry-After) to fail closed correctly. Recorded as an exception in the hardening checklist. | `LOCALAI_OPAQUE_ERRORS` unset | [run.go L84](https://github.com/mudler/LocalAI/blob/v4.10.0/core/cli/run.go#L84) |

## 3. Kept, loopback-only and behind the key

| Surface | Why it stays |
| --- | --- |
| OpenAI-, Anthropic-, Ollama-, Open Responses-, Jina- and ElevenLabs-compatible inference routes (`/v1/*`, `/api/chat`, ...) | The product. AlixAI uses the OpenAI-compatible chat, embeddings and (later) audio routes. The other dialects cannot be switched off and give a key holder no extra capability. |
| `/readyz`, `/healthz` | Readiness and liveness. Anonymous by upstream design. `/readyz` returns 503 while the startup preload is in progress, and **says nothing about inference**: it answers 200 with no backend installed and with a model that cannot load (M0-13). AlixAI's own `/readyz` has the check that exercises the model (README "AlixAI readiness"). |
| `GET /metrics` (Prometheus), `GET /system`, `GET /backend/monitor`, `/api/backend-logs`, `/ws/backend-logs/:model` | Local-only operational telemetry and resource reporting. Behind the key. `LOCALAI_DISABLE_METRICS_ENDPOINT=true` removes `/metrics` if an operator wants it gone; kept on purpose, recorded as an exception of HC-21 in the hardening checklist. |
| `POST /backend/load`, `POST /backend/shutdown` | Warm a model into memory or unload one. Useful for the M0-13 warm/cold measurements. Admin by upstream design, i.e. any key holder. |
| `/swagger/*`, `/api/instructions`, `/.well-known/localai.json` | Anonymous API discovery, unremovable. Loopback-only here. |

**Config directory is read-only.** LocalAI watches `/configuration` and hot-applies `runtime_settings.json`,
`api_keys.json` and `external_backends.json` placed there ([startup.go `startWatcher`](https://github.com/mudler/LocalAI/blob/v4.10.0/core/application/startup.go#L564),
[config_file_watcher.go](https://github.com/mudler/LocalAI/blob/v4.10.0/core/application/config_file_watcher.go#L29)).
Those files can add API keys, add external backends, enable p2p (`p2p_token`), or replace galleries. The settings
documentation also states that an environment variable set to its default value (for example `LOCALAI_P2P=false`)
loses to the file ([runtime settings](https://localai.io/features/runtime-settings/#known-limitations)).
`LOCALAI_DISABLE_RUNTIME_SETTINGS` does **not** prevent this: its help text says it stops loading
`runtime_settings.json`, but in v4.10.0 the only code that reads it skips the `/api/settings` routes
([ui_api.go L1906](https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/routes/ui_api.go#L1906)); the watcher
and the boot loader ([startup.go `loadRuntimeSettingsFromFile`](https://github.com/mudler/LocalAI/blob/v4.10.0/core/application/startup.go#L593))
do not consult it. So `./config` is mounted `:ro`, `entrypoint.sh` refuses to start if it is writable, and with the
web UI off there is no HTTP path that writes it.

## 4. Supply chain

| Artifact | Control |
| --- | --- |
| LocalAI image | Pinned by digest in `compose.yaml` (multi-arch index `sha256:0632c21d...`, same digest on Docker Hub and quay.io). The tag in the image reference is informational. See README for tag, digest, date and the bump procedure. |
| Backend (`cpu-llama-cpp`) | Pinned by digest in `config/backend-gallery.yaml` (`sha256:d7b0d9c6...`, same on quay.io and Docker Hub, which is listed as the mirror). It is the only backend gallery configured, so nothing else can be resolved. The upstream index points at mutable `latest-*` tags; the local catalogue does not. LocalAI's fallback that rewrites `latest-` to `master-` only fires for a `latest-` tag, so a digest reference is never silently swapped. |
| Model file | `download_files` entry in `config/models.yaml`: HF commit-pinned URL plus `sha256`. LocalAI re-downloads when the hash differs and errors on a mismatch after download ([uri.go](https://github.com/mudler/LocalAI/blob/v4.10.0/pkg/downloader/uri.go)). The hash was recomputed independently on the full file, not only read from the Hub header. |
| Backend signature verification | **Not enforced, on evidence.** LocalAI's strict mode (`LOCALAI_REQUIRE_BACKEND_INTEGRITY=true`) needs a per-gallery `verification` policy and a Sigstore **bundle** referrer (`artifactType application/vnd.dev.sigstore.bundle.*`); the verifier skips anything else ([bundle.go](https://github.com/mudler/LocalAI/blob/v4.10.0/pkg/oci/cosignverify/bundle.go), [backend signing](https://github.com/mudler/LocalAI/blob/v4.10.0/.agents/backend-signing.md)). The pinned v4.10.0 backend image has, on both registries, exactly one referrer for the index and one for each per-arch manifest, and every one is a legacy cosign signature manifest (config type `application/vnd.dev.cosign.artifact.sig.v1+json`, layer `application/vnd.dev.cosign.simplesigning.v1+json`), not a Sigstore bundle. The signing certificate does identify the producer: SAN `https://github.com/mudler/LocalAI/.github/workflows/backend_merge.yml@refs/tags/v4.10.0` (checked with `openssl x509` on the Docker Hub signature). Strict mode would therefore refuse the install, and merely adding a `verification` block makes every install attempt verification and fail even without strict mode. Integrity control is the immutable digest. **Confirmed live in M0-13** (`hardening-live.md` item 4): with `LOCALAI_REQUIRE_BACKEND_INTEGRITY=true` on an empty backends directory the install is refused (`strict integrity: gallery "alixai-pinned" has no verification policy for OCI backend "cpu-llama-cpp"`), no backend installs, and a chat answers `500 backend not found`. Without it LocalAI logs `installing OCI backend without signature verification`. If upstream publishes bundle-format signatures, add the `verification` block and flip `LOCALAI_REQUIRE_BACKEND_INTEGRITY=true`. |
| Provenance scan | LocalAI's HF pre-download malware scan runs on the gallery install path only ([models.go](https://github.com/mudler/LocalAI/blob/v4.10.0/core/gallery/models.go)), which is disabled here. The commit-pinned URL plus sha256 is the stronger control for `download_files`. `LOCALAI_DISABLE_PREDOWNLOAD_SCAN` is left at its default. |
| Runtime installs | None possible: gallery endpoints are off, the config dir is read-only, backends and models come only from the two committed files. Adding either is a reviewed change to `config/` plus a restart. |

## 5. Residual surfaces with no upstream off-switch

None of these can be removed in v4.10.0 (no flag found in `core/cli/run.go`, none in the route registration).
Each needs the key and loopback access, and each is inert without a backend or model that this deployment never
installs.

| Surface | Why it is acceptable here |
| --- | --- |
| `/api/fine-tuning/*`, `/api/quantization/*` ([fine-tuning](https://localai.io/features/fine-tuning/), [quantization](https://localai.io/features/quantization/)) | Registered unconditionally ([app.go L541, L563](https://github.com/mudler/LocalAI/blob/v4.10.0/core/http/app.go#L541)). They drive training and quantization backends, which are not in the catalogue and cannot be installed (galleries empty, gallery endpoints off, no `external_backends.json`). Watch for these in the v4.x changelog. |
| `/api/score`, `/stores/*`, `/v1/face/*`, `/v1/voice/*`, `/api/voice-profiles`, `/tts`, `/video`, `/3d/*`, `/vad`, ... | Inference or data endpoints that need a model plus a matching backend. Only `llama-cpp` is installed. |
| Legacy key = admin | Any holder of the key reaches every kept route in section 3. Mitigations: one key, loopback-only, no browser surface. Rotate by changing `.env` and restarting. |
| Periodic backend upgrade check | Runs every 6 hours whenever any backend gallery is configured ([upgrade_checker.go L61](https://github.com/mudler/LocalAI/blob/v4.10.0/core/application/upgrade_checker.go#L61)): an OCI digest lookup on the pinned reference. Report-only (`LOCALAI_AUTO_UPGRADE_BACKENDS=false`); a failure is logged at debug. It is the only periodic outbound call. |

## 6. Outbound network calls

The network posture says outbound access is denied by default and downloads happen only through an
operator-approved process. Compose cannot deny egress while also publishing a port, so egress is a host or
network-policy control. What LocalAI will try to reach with these settings:

| When | Destination | Bound by |
| --- | --- | --- |
| First boot, or if `/backends/cpu-llama-cpp` is missing | `quay.io`, then `docker.io` (mirror), image by digest | digest pin |
| First boot, or if the model file is missing or its hash differs | `huggingface.co` and the CDN hosts it redirects to | commit-pinned URL, sha256 |
| Every 6 hours | the backend registry (digest lookup, no download) | report-only |

No telemetry or update-check client exists in the LocalAI server code (a search of the v4.10.0 tree finds none;
the desktop launcher has an updater, which is not part of this image). After the first successful start the model
and backend are on disk and every call above can be blocked at the host firewall (for example the Docker
`DOCKER-USER` chain, or a Podman `pasta`/`slirp4netns` policy) without affecting inference. M0-13 captured what actually leaves the
container (section 8, item 6): the list above plus one undocumented contact with `huggingface.co` during the preload.

## 7. Container-level

- `security_opt: no-new-privileges:true`, `cap_drop: [ALL]`, then `cap_add: [CHOWN, DAC_OVERRIDE, FOWNER]`. A CPU
  workload on port 8080 needs no network, device or setuid capability, so NET_RAW, MKNOD, SETUID, SETGID, SETFCAP,
  SYS_CHROOT and the rest of Docker's default set stay dropped. The three added are file-access ones: `DAC_OVERRIDE`
  because the host's `./data/*` directories are normally owned by the invoking user and the container's root could
  not write them on rootful Linux Docker without it; `CHOWN` and `FOWNER` as a margin for ownership and mode changes
  while staging models and unpacking backend images. Backend images are unpacked with containerd
  `archive.Apply(..., WithNoSameOwner())` ([image.go L611](https://github.com/mudler/LocalAI/blob/v4.10.0/pkg/oci/image.go#L611)),
  so ownership is not preserved and CHOWN is likely unused, and M0-13 found it unused in every path that ran; it is kept as a margin.
  **Tested live (M0-13, Docker Desktop, which presents Windows bind mounts as root-owned and world-writable, so this
  holds for that platform only):** LocalAI starts and answers under this set, under **no** capability at all, under
  `DAC_OVERRIDE` alone, as the non-root `user: 1000:1000`, with a read-only root filesystem, with `/models` read-only
  and with `/configuration` read-only throughout. With Linux file-permission semantics (the ext4 matrix of
  `linux-permission-matrix.sh`): root with no capability cannot write a directory owned by uid 1000, `DAC_OVERRIDE`
  alone can, `FOWNER` only allows `chmod` of foreign files and `CHOWN` only `chown`. `CHOWN` and `FOWNER` were
  unused in every path that ran and are unproven for the one that might need them (unpacking a backend image, which
  could not run without the container's network). They are kept as a margin; a non-root container with directories
  owned by that user needs none. Rootful Docker and rootless Podman on Linux: not tested.
- Bind mounts for all four paths the image declares as `VOLUME` (`/models`, `/backends`, `/data`, `/configuration`),
  so no anonymous volume is ever created. No named volumes.
- Resource caps are applied by the profile override files (`mem_limit`, `memswap_limit` = `mem_limit`, `cpus`).
- Run live in M0-13 and **not applied** in these files (the production stack of M5-01 applies the checklist's HC-33):
  `read_only: true` with `tmpfs` for `/tmp` and `/run/localai` (`docker diff` then shows only mount points), a PID limit
  of 512, a 10 MB log cap, and a non-root user. A read-only `/models` is available as an override
  (`compose.readonly-models.yaml`).

## 8. Confirmed live (M0-13, closed by M0-18)

Run against the pinned release on Docker Desktop (lite profile unless stated; **emulated**, and not Linux). Every row has
its evidence in [`docs/evidence/m0-spike/hardening-live.md`](../../../docs/evidence/m0-spike/hardening-live.md) section 1
(the raw run logs are not published).

| # | Item | Result |
| --- | --- | --- |
| 1 | `smoke.sh` passes under `lite` | **Failed as shipped, passes now.** The shipped `file:///configuration/backend-gallery.yaml` was refused by v4.10.0 (`path is outside of trusted root`), so no backend installed and the first chat was `500 backend not found: llama-cpp`. The catalogue now sits at `/backends/alixai-backend-gallery.yaml` (`compose.yaml`, URL `file:///backends/alixai-backend-gallery.yaml`). M0-18 re-ran it: ready after 16 s, 401 without a key, 404 for `/app`, `/models/available`, `/api/settings` and `/v1/mcp/servers/x`, the model listed, one chat with content. |
| 2 | Backend install from the `file://` gallery with an OCI digest; the preload verifies the sha256 | **Yes.** LocalAI wrote `metadata.json` with the gallery URL, the digest URI and the digest (`backend-install-metadata.json`) and logs `installing OCI backend without signature verification`. An install failure is only logged (`error installing external backend`) and the service still reports ready. At debug level each model file logs `File already exists and matches the SHA. Skipping download`; a file whose hash differs is **deleted** and downloaded again (fail closed, destructive), and every start hashes every listed model (about 35 s for 3.2 GB there), so `config/models.yaml` lists only the models a profile uses. A verified in-container download could not be observed (TLS interception on that machine); the files were fetched on the host and re-hashed against their pins (README "Backend and model pull from the host"). |
| 3 | `/configuration` read-only and the reduced capability set work | **Yes on Docker Desktop**, for every variant in section 7. Nothing was written to `/configuration`; `/data` got `.local_user_id`, `traces/` and `voice-profiles/` and no `runtime_settings.json`. Rootful Docker and rootless Podman on Linux: **not tested**. |
| 4 | Strict backend integrity refuses the pinned image | **Yes**, section 4. Strict mode is unusable for this gallery until a verification policy and a Sigstore bundle exist for the image. |
| 5 | Only `127.0.0.1:8080` is exposed | **Yes.** `docker port` shows `127.0.0.1:8080`, the host's LAN address refuses, and inside the container the listeners are LocalAI's `:8080`, the backend's gRPC on a random loopback port and Docker's resolver. |
| 6 | The egress list of section 6 matches a capture | **Mostly, plus one undocumented contact.** First boot without the backend: `quay.io`, `index.docker.io` and `huggingface.co`. A boot with backend and models present: only `huggingface.co` during the preload (16 DNS lookups, 8 TCP connections in about 40 s), which section 6 does not list and which is not needed (an `internal: true` boot works); its trigger was not identified. No telemetry or other destination; every DNS query went to 8.8.8.8. The 6-hourly upgrade check was not observed (captures were 75 to 90 s). A network-level block is the control, as section 6 says. |

What the live run changed in this deployment: the gallery path (item 1), the guard switch for a read-only `/models` and the
log-level guard (section 1), the per-profile `LOCALAI_MAX_ACTIVE_BACKENDS` defaults (M0-15 rule R3), and the exception
entries of the hardening checklist (HC-14, HC-18, HC-20, HC-21; HC-11 gains `/api/auth/status`; HC-34 gains its measured reason).

**Not run:** native Linux, rootful Docker on Linux, rootless Podman, another OS user's access to the port, strict integrity
with a real verification policy, a verified in-container model download, backend unpack without capabilities, the 6-hourly
upgrade check, HC-26 (remote-code trust field) and HC-36 (prompt-cache timing channel, M5-12).

## 9. Re-audit on every LocalAI bump

Diff `core/cli/run.go` (every `env:` tag) and the route registrations (`core/http/app.go`, `core/http/routes/*.go`)
between the old and new tag. A new `LOCALAI_DISABLE_*` flag, a new always-on route, or a change to the public-route
list in `core/http/auth/public_routes.go` means this document and `compose.yaml` need an update before the new
digest is adopted.
