# LocalAI hardening checklist

- **Task:** M0-14 (initial version); M0-18 (exception entries, HC-11, HC-29 and HC-34 corrected from the live run of M0-13)
- **Companion documents:** [threat-model.md](threat-model.md) (component C10), [test-backlog.md](test-backlog.md) (SEC-063 to SEC-070),
  [`deploy/container/localai/hardening.md`](../../deploy/container/localai/hardening.md) and the live results in
  [`docs/evidence/m0-spike/hardening-live.md`](../evidence/m0-spike/hardening-live.md)
- **Provenance:** setting names and defaults below come from the LocalAI documentation fetched on
  2026-10-02 (CLI reference, authentication, runtime settings, agents, MCP and model gallery pages at
  localai.io). They were read through a summarizer, not from the source tree. **M0-08 re-verifies
  every row against the exact release it pins before the checklist is treated as final**, and records
  the release tag, digest and verification date in `deploy/container/localai/README.md`. Defaults are
  quoted only where the documentation stated them. Rows marked *confirm* rest on a statement the
  documentation did not make explicitly.

## 1. What this checklist is

LocalAI is the default model runtime (§4.2). It is also a large upstream product with a web UI, a model
gallery, an agent pool, MCP execution and its own user system. AlixAI uses a small part of it: inference,
embeddings, health and (optionally) audio, called by AlixAI only, over a private endpoint.

Three facts shape every rule below.

1. **A LocalAI API key is a full administrator credential.** The authentication documentation says keys
   grant full admin access with no role separation. Anything that reaches the port with the key can
   install models, change settings and run agents. So the network boundary and the fixed endpoint
   allowlist in the AlixAI adapter (M0-09) are the real controls, not the key alone.
2. **Several defaults are exposure-friendly.** The default address is `:8080` (all interfaces), agents
   and the gallery are on, and the web UI and runtime settings page are on. The production profile turns
   everything it does not use off, explicitly, in environment variables (values set by environment are
   locked against change through the runtime settings UI, which is another reason to set them all).
3. **Upstream has had real vulnerabilities in the surfaces we disable.** CVE-2024-5182 (GHSA-cpcx-r2gq-x893,
   rated High) was a path traversal in the model-delete flow that deleted arbitrary files, affecting
   versions before 2.16.0. Delete and install endpoints must be unreachable and the pinned version must
   be current.

This file states **requirements**. Other tasks implement and check them:

| Task | Role |
| --- | --- |
| M0-08 | Implements the rows in `deploy/container/localai/` and explains each disabled surface in its `hardening.md`. Every row here must appear there |
| M0-09 | Implements the client side: endpoint allowlist, loopback-only, no key in logs |
| M0-13 | Runs the live smoke checks in section 5 once on the real container |
| M5-01, M5-02 | Apply the rows to the production stack, Quadlet units and native services |
| M5-08 | Implements `TestSecLocalAIHardeningFlags` (SEC-068): parses every file that launches LocalAI and checks the `static` rows |
| M5-12 | Probes the running stack live for the `live` rows |

## 2. How to read the table

| Column | Meaning |
| --- | --- |
| ID | `HC-NN`, stable |
| Setting | Flag and environment variable as the documentation names them. `—` where the row is about a deployment property rather than a setting |
| Rule | `require` (must be set to the value), `forbid` (must be absent, empty or false), `bound` (must be set within the stated limit), `manual` (a property checked by the stated method) |
| Check | `static` (M5-08 parses compose, Quadlet, systemd and env files), `live` (probe against a running stack, M0-13 and M5-12), `review` (human or ADR) |
| SEC | Backlog entry whose test relies on the row |

Profile column: **prod** is the production profile of §16.1. **install** is the one-time,
operator-approved install or update step (§16.1 "Model downloads occur only through an
operator-approved install/update process"). **dev** is the M0 spike and local development. A row that
says `prod` also applies to the later profiles unless stated.

Deviations need an entry in the exceptions table (section 6). The static check reads that table.

## 3. Requirements

### 3.1 Network exposure

| ID | Setting | Rule | Value or limit | Profile | Check | SEC | Why |
| --- | --- | --- | --- | --- | --- | --- | --- |
| HC-01 | `--address`, `LOCALAI_ADDRESS` | require | Native installs: `127.0.0.1:<port>`. In a container the process may bind inside the container namespace, but see HC-02. The documented default `:8080` is all interfaces and is forbidden | prod, dev | static | SEC-066 | §16.1 loopback or private socket |
| HC-02 | Compose and Quadlet `ports` | require | Published ports are `127.0.0.1:<host>:<container>` only. No bare `8080:8080`, no `0.0.0.0:`, no `network_mode: host` unless HC-01 holds. In the production stack LocalAI publishes **no** port: it is reachable only on an internal network or shared socket that AlixAI also joins | prod, dev | static | SEC-066, SEC-093 | M0-08 verify, M5-01 `TestPortsLoopbackOnly` |
| HC-03 | Private socket | manual | If the pinned release can listen on a unix domain socket, prefer it in native installs, with the socket file owner-only and owned by the AlixAI identity group. *Confirm* socket support in the pinned release; otherwise use loopback | prod | review | SEC-066 | §16.1 |
| HC-04 | — | manual | No reverse proxy, tunnel, port-forward or firewall rule exposes the LocalAI port. Nothing other than AlixAI has a route to it | prod | live | SEC-066 | Only AlixAI may call LocalAI (§16.1) |
| HC-05 | Container network | require | Production LocalAI runs on a network with no outbound route (compose `internal: true` or firewall deny) once models and backends are provisioned. During an approved install step the route may be opened and closed again | prod | static | SEC-071 | Outbound denied by default (§16.1); LocalAI can fetch from galleries, Hugging Face and OCI registries |

### 3.2 Authentication

| ID | Setting | Rule | Value or limit | Profile | Check | SEC | Why |
| --- | --- | --- | --- | --- | --- | --- | --- |
| HC-06 | `--api-keys`, `LOCALAI_API_KEY` | require | Set to a generated random secret, at least 32 random bytes encoded, supplied from an untracked env file or secret store. The compose file must fail to start if it is unset (`${LOCALAI_API_KEY:?}`). `.env.example` carries a placeholder only. Never a default or shared value | prod, dev | static | SEC-066, SEC-084 | When set, all requests must be authenticated |
| HC-07 | `--use-subtle-key-comparison`, `LOCALAI_SUBTLE_KEY_COMPARISON` | require | `true` (constant-time comparison; documented default `false`) | prod | static | SEC-066 | Timing side channel on key comparison |
| HC-08 | `--disable-api-key-requirement-for-http-get`, `LOCALAI_DISABLE_API_KEY_REQUIREMENT_FOR_HTTP_GET`; `--http-get-exempted-endpoints`, `LOCALAI_HTTP_GET_EXEMPTED_ENDPOINTS` | forbid | Absent or `false` | prod, dev | static | SEC-066 | Removes authentication from GET routes |
| HC-09 | `--auth-enabled`, `LOCALAI_AUTH`; `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`; `LOCALAI_OIDC_ISSUER`, `LOCALAI_OIDC_CLIENT_ID`, `LOCALAI_OIDC_CLIENT_SECRET`; `LOCALAI_REGISTRATION_MODE` | forbid | LocalAI's own user accounts stay off: AlixAI is the only client and its scopes do not map to LocalAI users. Setting OAuth credentials auto-enables auth per the documentation. If an operator needs it for an admin surface, record an exception and set `LOCALAI_REGISTRATION_MODE` to `invite` or `approval`, never `open` (documented default) | prod | static | SEC-068 | An open registration mode would let anyone who reaches the port create an account |
| HC-10 | Key handling | manual | The key is held by AlixAI as an opaque secret reference (OS secret store or approved provider), never placed in a model prompt, log, error or diagnostics bundle. One key per deployment, rotated on suspicion of exposure | prod | review | SEC-082, SEC-083 | §16.2 |
| HC-11 | Known-exempt routes | manual | Per the authentication documentation and `core/http/auth/public_routes.go`, health and readiness, discovery, **`/api/auth/status`** and SPA asset routes are reachable without a key. Treat them as public to anyone who can reach the port and keep the network boundary strict. Measured in M0-13 (33 routes probed, `surface-probe.json`): the ones that answered 200 without a key were `/readyz`, `/healthz`, `/swagger/index.html`, `/api/instructions`, `/.well-known/localai.json` and `/api/auth/status` (it reports `authEnabled:false` while `LOCALAI_AUTH` is off); `/api/branding`, `/branding/asset/`, `/login`, `/invite/` and `/favicon.svg` are in the same file and exist only with the web UI on | prod | live | SEC-066 | Docs list `/healthz`, `/readyz`, `.well-known/localai.json`, `/api/instructions`, SPA routes and CORS preflights as exempt; the source adds `/api/auth/status` |

### 3.3 Surfaces AlixAI does not use

| ID | Setting | Rule | Value or limit | Profile | Check | SEC | Why |
| --- | --- | --- | --- | --- | --- | --- | --- |
| HC-12 | `--disable-webui`, `LOCALAI_DISABLE_WEBUI` | require | `true` | prod | static | SEC-068 | §15.3: the LocalAI GUI is not required and is disabled in production unless the operator's admin policy needs it, then loopback-only and listed as an exception |
| HC-13 | `--disable-runtime-settings`, `LOCALAI_DISABLE_RUNTIME_SETTINGS` | require | `true`. Runtime settings can change API keys, CORS, CSRF, P2P, galleries and the agent pool and are persisted to `runtime_settings.json` in the config directory. The config directory is also mounted read-only where the release allows | prod | static | SEC-068 | A remote configuration-change surface |
| HC-14 | `--disable-gallery-endpoint`, `LOCALAI_DISABLE_GALLERY_ENDPOINT`; `--autoload-galleries`, `LOCALAI_AUTOLOAD_GALLERIES`; `--autoload-backend-galleries`, `LOCALAI_AUTOLOAD_BACKEND_GALLERIES`; `--galleries`, `LOCALAI_GALLERIES`; `--backend-galleries`, `LOCALAI_BACKEND_GALLERIES` | require | Endpoint disabled `true`; both autoload flags `false` (documented default `true`); gallery lists unset or `[]`. The model install endpoint `/models/apply` and backend install are unreachable. **Exception EX-01 (section 6):** the one backend gallery is the local, digest-pinned `file://` catalogue, not unset | prod | static | SEC-069 | Arbitrary model and backend installation (§16.4 bullet 10) |
| HC-15 | `LOCALAI_DISABLE_AGENTS` | require | `true` (agents documented as enabled by default) | prod | static | SEC-070 | The agent pool exposes `/api/agents/*` management, skills, collections and action execution. No agent hub in production (§16.1) |
| HC-16 | `LOCALAI_DISABLE_MCP` | require | `true`. Server-side MCP can run stdio commands and call remote servers from the LocalAI process and loops tool calls automatically. *Confirm* that the setting also removes the browser CORS proxy `/api/cors-proxy` that the documentation attaches to client-side MCP; if it does not, the live probe in section 5 must show that path refused | prod | static, live | SEC-070, SEC-092 | No arbitrary MCP, no CORS proxy (§16.1) |
| HC-17 | `--p2p`, `LOCALAI_P2P`; `--federated`, `LOCALAI_FEDERATED`; `LOCALAI_P2P_TOKEN`; `LOCALAI_P2P_NETWORK_ID` | forbid | Absent or `false` | prod | static | SEC-068 | P2P and federation open a second network surface and share inference with peers |
| HC-18 | `--external-backends`, `LOCALAI_EXTERNAL_BACKENDS`; `--external-grpc-backends`, `LOCALAI_EXTERNAL_GRPC_BACKENDS` | forbid | Absent. **Exception EX-02 (section 6)** for `LOCALAI_EXTERNAL_BACKENDS`, the install mechanism of the shipped deployment; `LOCALAI_EXTERNAL_GRPC_BACKENDS` stays absent | prod | static | SEC-064, SEC-069 | Loads backends from outside at boot, which is unapproved code (§16.3) |
| HC-19 | `--cors`, `LOCALAI_CORS`; `--cors-allow-origins`, `LOCALAI_CORS_ALLOW_ORIGINS`; `--disable-csrf`, `LOCALAI_DISABLE_CSRF` | require | CORS off (documented default `false`) with no allowed origins, because no browser talks to LocalAI. CSRF protection not disabled | prod | static | SEC-068, SEC-092 | §16.1 no browser CORS proxy |
| HC-20 | `--opaque-errors`, `LOCALAI_OPAQUE_ERRORS` | require | `true`, **if** the AlixAI error mapping still distinguishes timeout, resource exhaustion and unavailable (M0-09 `TestErrorMapping`) when every error is a blank 500. **Decided by M0-13: they collapse the mapping, so opaque errors stay off** (a model that cannot load and a context overflow become the same blank 500 and map to `internal`; with them off they map to `resource_exhausted` and `validation_failed`; a cgroup OOM kill of the backend maps to `resource_exhausted`). Off only on a loopback or internal endpoint; exception EX-03 (section 6) | prod | static, review | SEC-068 | Error bodies can reveal paths and internals |
| HC-21 | `--disable-metrics-endpoint`, `LOCALAI_DISABLE_METRICS_ENDPOINT` | require | `true`. AlixAI exposes its own loopback metrics (M5-05) and reads provider health through the allowed health path. **Exception EX-04 (section 6):** `/metrics` is kept for now | prod | static | SEC-068 | One less unauthenticated-by-default surface |
| HC-22 | `--disable-predownload-scan`, `LOCALAI_DISABLE_PREDOWNLOAD_SCAN` | forbid | Absent or `false` | install, dev | static | SEC-063, SEC-069 | Keeps the upstream safety scan before any approved download |
| HC-23 | `--machine-tag`, `LOCALAI_MACHINE_TAG` | forbid | Unset | prod | static | SEC-068 | Adds origin information to response headers |

### 3.4 Models, backends and what loads at start

| ID | Setting | Rule | Value or limit | Profile | Check | SEC | Why |
| --- | --- | --- | --- | --- | --- | --- | --- |
| HC-24 | `--models-path`, `LOCALAI_MODELS_PATH`; `--backends-path`, `LOCALAI_BACKENDS_PATH`; `--data-path`, `LOCALAI_DATA_PATH`; `--localai-config-dir`, `LOCALAI_CONFIG_DIR` | require | Bind mounts under the data directory, never named volumes. Models and backends are provisioned by the install step, then mounted read-only where the release allows. The config directory is read-only in production. Owner-only permissions. A read-only `/models` runs (M0-13) and also stops LocalAI from deleting and re-downloading a model file whose hash differs from its pin at start; the deployment ships it as the optional override `compose.readonly-models.yaml`, guarded by the explicit switch `ALIXAI_MODELS_READONLY=true` of `entrypoint.sh`, and production uses it | prod | static | SEC-063, SEC-081 | A writable models directory lets a runtime compromise persist; hashes verified by AlixAI (SEC-063) |
| HC-25 | `--preload-models`, `LOCALAI_PRELOAD_MODELS`; `--preload-models-config`, `LOCALAI_PRELOAD_MODELS_CONFIG`; `--models`, `LOCALAI_MODELS`; `--load-to-memory`, `LOCALAI_LOAD_TO_MEMORY` | require | **install and dev:** may name remote URIs, but only the approved registry entries, each with its `sha256` pin where the release supports one, and AlixAI re-verifies the hash (SEC-063). **prod:** reference local filenames already provisioned, never a remote URI. No model fetching in a separate init container (M0-08 rule) | install, dev, prod | static | SEC-063, SEC-069 | Model downloads only through an approved process (§16.1) |
| HC-26 | Model configuration files | manual | Operator-authored, reviewed, kept in the repository's model registry flow. They contain no MCP or agent stanza, no `trust_remote_code: true` (*confirm* the field name for the pinned release's transformers backend), and no backend path outside the approved backends directory. User input never reaches a model config | prod | static | SEC-069, SEC-070 | Model YAML can carry tool and backend settings |
| HC-27 | LocalAI image and backends | require | Image pinned by digest (`@sha256:`), tag and digest recorded with the date. Backend versions are listed in the release manifest. If the pinned release verifies signed backends, enable it (the LocalAI 4.3 release notes mention signed backends; *confirm* and record) | prod | static | SEC-064 | §16.3 verified backend hashes |
| HC-28 | LocalAI version | manual | The pinned release has no known unpatched advisory affecting a surface we use. Record the advisories checked (for example GHSA-cpcx-r2gq-x893 fixed in 2.16.0) in the README. Re-check at every upgrade (M5-04) | prod | review | SEC-064, SEC-072 | Upstream vulnerability history |

### 3.5 Resource and availability limits

| ID | Setting | Rule | Value or limit | Profile | Check | SEC | Why |
| --- | --- | --- | --- | --- | --- | --- | --- |
| HC-29 | `--max-active-backends`, `LOCALAI_MAX_ACTIVE_BACKENDS` (the older `LOCALAI_SINGLE_ACTIVE_BACKEND` is deprecated) | bound | **The number of models the profile keeps resident** (`docs/operations/resource-budgets.md` R2 and R3.1): lite **2** (controller, embedder), balanced **4** (adds speech to text and text to speech), experimental-4g **1** (controller only). Below that LocalAI unloads the least recently used backend on every call: M0-15 measured 15.2 s for a chat after an embedding, 6.2 s for an embedding after a chat and about 70 page faults a second with one slot on the 4 GiB profile. Documented default `0` is unlimited, which cannot evict but is not a bound. The compose profiles set these values and `alixai serve` refuses a lower one when it sees the variable (`TestMaxActiveBackendsCoversResidentRoles`) | prod | static | SEC-078 | Resident models, no eviction on ordinary turns, no runaway memory (§17.2, §7.4) |
| HC-30 | `--enable-watchdog-idle`, `LOCALAI_WATCHDOG_IDLE`; `--watchdog-idle-timeout`, `LOCALAI_WATCHDOG_IDLE_TIMEOUT`; `--enable-watchdog-busy`, `LOCALAI_WATCHDOG_BUSY`; `--watchdog-busy-timeout`, `LOCALAI_WATCHDOG_BUSY_TIMEOUT` | bound | Idle unload on, with a timeout not shorter than AlixAI's minimum warm period (M3-17) so ordinary turns do not reload the model. Busy watchdog on, with a timeout above AlixAI's request deadline. Documented defaults are idle `15m`, busy `5m`, both off | prod | static | SEC-078 | Model-load denial of service and eviction thrash (§16.4 bullet 11) |
| HC-31 | `--max-concurrent-backend-requests`, `LOCALAI_MAX_CONCURRENT_BACKEND_REQUESTS` | bound | The profile's measured concurrency from `docs/operations/resource-budgets.md` (M0-15). The documented default is 1024, far above any certified profile | prod | static | SEC-078, SEC-076 | Process-wide ceiling for inference |
| HC-32 | `--upload-limit`, `LOCALAI_UPLOAD_LIMIT` | bound | The smallest value the audio path needs; AlixAI does not upload files to LocalAI otherwise. Documented default 15 MB | prod | static | SEC-076 | Bounded request size |
| HC-33 | Container or service limits | require | `mem_limit` and `cpus` per profile, `memswap_limit` equal to `mem_limit` (no swap), a PID limit, `cap_drop: ALL`, `no-new-privileges`, non-root user, read-only root filesystem with tmpfs where needed, no host mounts beyond the bind mounts above. Native installs get the equivalent systemd hardening | prod | static | SEC-080 | §16.2, §7.4 |

### 3.6 Logging and privacy

| ID | Setting | Rule | Value or limit | Profile | Check | SEC | Why |
| --- | --- | --- | --- | --- | --- | --- | --- |
| HC-34 | `--log-level`, `LOCALAI_LOG_LEVEL`; `--debug`, `LOCALAI_DEBUG`, `DEBUG` (deprecated) | require | `info` or higher in production (documented default `info`), never `debug` or `trace` with real data. **Measured in M0-13 against v4.10.0** (`hardening-live.md` section 4, three boots at `info`, `debug` and `trace`, each handling two chats with canary strings): **at `debug` and `trace` LocalAI logs the model's completion in full**, one `DEBUG Response response="{...\"content\":...}"` line per call (none at `info`). The prompt is not logged at any level (0 matches for a prompt canary in stdout and in `/data`, `/tmp`, `/models`, `/configuration`, `/backends`; `/data/traces` stays empty), and the request line (method, path, status, no body) is present at every level. A completion can hold the user's own data, a proposal's field values or a refusal, and container logs are outside AlixAI's redaction rules (HC-35). **Configuration guard:** `deploy/container/localai/entrypoint.sh` refuses to start with `LOCALAI_LOG_LEVEL` `debug` or `trace` (any case) or `LOCALAI_DEBUG` / `DEBUG` on, unless the development switch `ALIXAI_ALLOW_VERBOSE_LOG=true` is set, which is a throw-away dev run and prints a warning; the static check forbids that switch in every production file | prod | static, live | SEC-068, SEC-082 | Completions hold tenant data (the prompt does not reach the log, the answer does above `info`) |
| HC-35 | Log retention | manual | Container logs rotate with a size limit; LocalAI logs are inside the diagnostics redaction rules if bundled (M5-05) | prod | review | SEC-083 | §16.2 |
| HC-36 | Prompt cache | manual | Measure whether LocalAI's prompt or KV cache can be used for cross-tenant timing inference (SEC-030); disable it if the cost is small, else record the accepted risk | prod | review | SEC-030 | Shared cache side channel |

### 3.7 What AlixAI itself must and must not do toward LocalAI

| ID | Setting | Rule | Value or limit | Profile | Check | SEC | Why |
| --- | --- | --- | --- | --- | --- | --- | --- |
| HC-37 | AlixAI adapter endpoint allowlist | require | Calls only inference (chat, embeddings), health and readiness, model list, and audio paths when voice is installed. Never model or backend apply, delete or install, agent, MCP, CORS proxy, settings, gallery or metrics paths. The whole test suite is checked against the allowlist | prod | static | SEC-067 | M0-09 `TestOnlyAllowlistedEndpointsCalled` |
| HC-38 | AlixAI to LocalAI transport | require | Loopback or private socket. A non-loopback endpoint needs TLS configured and is refused otherwise. The key is sent to the configured host only | prod | static | SEC-065 | M0-09 `TestRejectsNonLoopbackWithoutTLS` |
| HC-39 | Tool calling | require | AlixAI uses schema or grammar-constrained chat and executes nothing LocalAI returns. If the Realtime API is used, the session disables model-side tool calling and autonomous responses (M4-03) | prod | static | SEC-070, SEC-094 | Typed execution (§3 rule 7) |
| HC-40 | Settings API | require | No AlixAI setting or route can enable a LocalAI surface or expose its administration | prod | static | SEC-067 | M3-07 `TestSettingsCannotExposeRuntimeAdmin` |
| HC-41 | Process identity | require | LocalAI runs as an OS identity different from AlixAI's, with no write access to AlixAI's data directory, and the reverse (§16.2). Native installs use a dedicated service user; the supervisor (M5-07) does not run as root | prod | static, live | SEC-080 | §16.2 |

## 4. Machine-readable summary for the static check

`TestSecLocalAIHardeningFlags` (M5-08) reads every compose, Quadlet, systemd and `.env*` file that
launches LocalAI and applies these rules. Environment names are the ones in section 3. A file may set a
variable as an `environment:` entry, an `Environment=` line, an `--flag`, or an env file line.

| Kind | Variables |
| --- | --- |
| Must be present with the value | `LOCALAI_API_KEY` (non-empty, from a required substitution), `LOCALAI_SUBTLE_KEY_COMPARISON=true`, `LOCALAI_DISABLE_WEBUI=true`, `LOCALAI_DISABLE_RUNTIME_SETTINGS=true`, `LOCALAI_DISABLE_GALLERY_ENDPOINT=true`, `LOCALAI_AUTOLOAD_GALLERIES=false`, `LOCALAI_AUTOLOAD_BACKEND_GALLERIES=false`, `LOCALAI_DISABLE_AGENTS=true`, `LOCALAI_DISABLE_MCP=true`, `LOCALAI_DISABLE_METRICS_ENDPOINT=true` unless excepted (EX-04), `LOCALAI_MAX_ACTIVE_BACKENDS` (the resident models of the profile: lite `2`, balanced `4`, 4g `1`), `LOCALAI_OPAQUE_ERRORS=true` unless excepted (EX-03) |
| Must be absent, empty or false | `LOCALAI_DISABLE_API_KEY_REQUIREMENT_FOR_HTTP_GET`, `LOCALAI_HTTP_GET_EXEMPTED_ENDPOINTS`, `LOCALAI_AUTH`, `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET`, `LOCALAI_OIDC_ISSUER`, `LOCALAI_P2P`, `LOCALAI_FEDERATED`, `LOCALAI_P2P_TOKEN`, `LOCALAI_P2P_NETWORK_ID`, `LOCALAI_EXTERNAL_BACKENDS` (unless excepted, EX-02), `LOCALAI_EXTERNAL_GRPC_BACKENDS`, `LOCALAI_CORS`, `LOCALAI_CORS_ALLOW_ORIGINS`, `LOCALAI_DISABLE_CSRF`, `LOCALAI_DISABLE_PREDOWNLOAD_SCAN`, `LOCALAI_MACHINE_TAG`, `LOCALAI_GALLERIES` and `LOCALAI_BACKEND_GALLERIES` (or `[]`; the one local `file://` catalogue of EX-01 is the exception), `ALIXAI_ALLOW_VERBOSE_LOG` (a switch of the deployment's entrypoint guard, not a LocalAI setting) |
| Log level | `LOCALAI_LOG_LEVEL` not `debug` or `trace`; `LOCALAI_DEBUG` and `DEBUG` absent (HC-34) |
| Structure | image reference ends in `@sha256:<64 hex>`; every published port starts with `127.0.0.1:` (or none); no named volumes for models, backends or data; `memswap_limit` equals `mem_limit`; `cap_drop` contains `ALL`; no `network_mode: host`; no service named `*init*` |

The check is a floor, not a ceiling: a rule that a file satisfies textually but defeats by another
route (for example an override file that re-enables a surface) is caught by the live probe.

## 5. Live verification

These run once in M0-13 against the real container and again in M5-12 against the production stack.
They need a running stack and are never part of the offline `verify`.

1. **Listening sockets.** On the host, only loopback (or nothing, for the internal-network stack) lists
   the LocalAI port. Linux: `ss -ltnp`. Windows: `netstat -ano | findstr LISTENING`.
2. **Reachability from elsewhere.** A connection to the host's LAN address on the LocalAI port is
   refused or times out, and so is a connection from another OS user where the port is loopback.
3. **No key, no service.** Requests without the key to model list, model apply, backends, settings,
   agent and CORS-proxy paths return 401, 403 or 404, never 200. Only the documented exempt routes may
   answer (HC-11).
4. **Disabled surfaces are gone.** The web UI root, the gallery endpoints, the runtime settings page,
   `/api/agents/*`, `/api/cors-proxy` and the metrics path do not serve content, even with the key.
5. **Allowlist.** With the key, AlixAI's own run of its test suite produces a request log whose paths
   are a subset of HC-37.
6. **Outbound.** From inside the LocalAI container, a connection to an external host fails in the
   production network (HC-05).
7. **Identity.** `id` inside the container shows a non-root user, AlixAI's data directory is not
   readable from the LocalAI container, and the reverse holds.
8. **Idempotent restart.** After a restart the same checks still pass: no surface is re-enabled by a
   persisted runtime settings file or a changed default.

## 6. Exceptions

An exception is allowed only for a stated reason, with an owner and an expiry, and is visible to the
static check. The four below are the rows the live run of M0-13 showed the shipped deployment cannot meet, recorded by
M0-18 (`hc-static-check.json` lists all 44 settings the checklist names and finds each in v4.10.0). An expiry is the date
by which the entry must be re-examined, or earlier when the condition in it holds.

| ID | Row | Reason | Owner | Expires | Mitigation |
| --- | --- | --- | --- | --- | --- |
| EX-01 | HC-14 | `LOCALAI_BACKEND_GALLERIES` is not unset: it names exactly one gallery, the local `file:///backends/alixai-backend-gallery.yaml` (mounted read-only from `deploy/container/localai/config/backend-gallery.yaml`), which holds the digest-pinned `cpu-llama-cpp` backend. With no gallery LocalAI has no way to install a backend. The rest of HC-14 is met: gallery endpoint disabled, both autoload flags `false`, `LOCALAI_GALLERIES=[]` | deployment, `deploy/container/localai` (M0-18) | 2027-04-03, or when the backend is baked into the image or pre-installed by the install step and the variable is unset (M5-01, M5-03) | No network gallery exists; the catalogue is read-only and pinned by digest (no mutable tag); `/models/apply` and every `/backends*` route answer 404 even with the key (`surface-probe.json`); `LOCALAI_AUTO_UPGRADE_BACKENDS=false`; the config directory is read-only |
| EX-02 | HC-18 | `LOCALAI_EXTERNAL_BACKENDS=cpu-llama-cpp` is the install mechanism of the shipped deployment: LocalAI installs the named backend at first boot. The value is a catalogue **name** resolved only in the gallery of EX-01, not a URI, so it cannot point at code outside the pinned catalogue. `LOCALAI_EXTERNAL_GRPC_BACKENDS` stays absent | deployment, `deploy/container/localai` (M0-18) | 2027-04-03, or when the production stack pre-installs the backend (an installed backend is skipped, so the variable can then be unset; README "Backend and model pull from the host") | A name that is not in the catalogue fails to install; `external_backends.json` cannot be dropped into the config directory (mounted `:ro`, refused otherwise by the entrypoint guard); strict integrity (HC-27) cannot be switched on until the image has a Sigstore bundle (`hardening-live.md` item 4); the digest pin is the integrity control |
| EX-03 | HC-20 | `LOCALAI_OPAQUE_ERRORS` is unset. **Decided by M0-13, measured:** with it the model-load failure and the context overflow are both a blank 500 and `alixai serve` maps both to `internal`; without it they map to `resource_exhausted` (503) and `validation_failed` (400), and a cgroup OOM kill of the backend to `resource_exhausted` (`error-mapping-default.json`, `error-mapping-opaque-errors.json`, `error-mapping-4g.json`). Fail-closed mapping needs the real status | adapter and gateway (M0-09, M0-12); decision M0-13 | 2027-04-03, or when the adapter can classify a blank 500 from another signal | Loopback or an internal network only, behind the key; the adapter turns runtime errors into fixed short reasons and never forwards the runtime's text to a client (`/readyz` shows an error code, nothing else) |
| EX-04 | HC-21 | `LOCALAI_DISABLE_METRICS_ENDPOINT` is unset: `/metrics` answers 200 with the key. `hardening.md` section 3 keeps it as local operational telemetry (metrics stay local), and AlixAI's own metrics (M5-05), which are this row's reason, do not exist yet | deployment (M0-18); M5-05 | M5-05 landing, or 2027-04-03 | Behind the key and loopback or the internal network only; the series are Prometheus counters and gauges, but the body was not inspected in M0-13, so M5-12 reads it for request content before the exception is renewed |

## 7. Open points to settle while implementing

| Point | Settled by |
| --- | --- |
| Does the pinned release support a unix socket listener (HC-03)? | M0-08. **Settled (M0-13, source reading, not run):** v4.10.0 supports systemd socket activation (`run.go` `systemdActivatedListeners`) and has no `unix:` address flag, so a private socket is possible on native installs through systemd |
| Does `LOCALAI_DISABLE_MCP` remove `/api/cors-proxy` (HC-16)? | M0-08, confirmed live in M0-13: **yes**, the path answers 404 with the key |
| Does `--opaque-errors` break AlixAI's error mapping (HC-20)? | M0-09 and M0-13. **Settled: yes**, see EX-03 |
| Exact field name and default for remote-code trust in model configs (HC-26) | M0-08 |
| Does the pinned release verify signed backends (HC-27)? | M0-08, M5-03. **Settled (M0-13):** strict integrity exists and refuses the unsigned-bundle image; it is unusable until the image carries a Sigstore bundle |
| What each log level prints, and whether prompts appear (HC-34) | M0-13. **Settled:** the prompt never; the completion in full at `debug` and `trace` (HC-34, guarded by the entrypoint) |
| Whether the prompt or KV cache is a usable cross-tenant timing channel (HC-36, SEC-030) | M5-12 |
| Is a read-only root filesystem and read-only models mount workable for the pinned image (HC-24, HC-33)? | M0-08, M5-01. **Settled (M0-13, Docker Desktop):** both run, with `tmpfs` for `/tmp` and `/run/localai`; the production stack applies them (M5-01) |

## 8. References

- LocalAI documentation: [CLI reference](https://localai.io/docs/reference/cli-reference/), [authentication](https://localai.io/docs/features/authentication/), [runtime settings](https://localai.io/docs/features/runtime-settings/), [agents](https://localai.io/docs/features/agents/), [MCP](https://localai.io/docs/features/mcp/), [model gallery](https://localai.io/docs/models/)
- CVE-2024-5182 / GHSA-cpcx-r2gq-x893: path traversal in LocalAI model deletion, fixed in 2.16.0
- The LocalAI authentication, agents, MCP and Realtime pages are the primary references
