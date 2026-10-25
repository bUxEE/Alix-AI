# Guide: server

Status: written in task M5-13, 2026-10-03. Read the [guides index](README.md) first for the legend (**Run**, **Static**, **Not run**) and the
known gaps.

A machine you administer, on premises or a cloud VM on a private network, that runs AlixAI as a **system service** for host applications on the same machine or the same
network. AlixAI itself stays on loopback; this guide covers installing it, its service identity, its configuration, monitoring and routine operation. When the internet can
reach the machine, do everything here and then add [the private VPS guide](private-vps.md).

## 1. Choose the install shape

| Shape | What it is | Pick it when | Source |
| --- | --- | --- | --- |
| **A. Container stack, Docker Compose** | `alixai`, LocalAI and two tiny forwarders as four hardened containers on internal networks; one published loopback port. | You already run Docker. | `deploy/container/stack/README.md` |
| **B. Container stack, rootless Podman with Quadlet** | The same topology as systemd user units. The preferred Linux production path (ADR 0012). | Linux with Podman 5.2 or later. | `deploy/container/quadlet/` |
| **C. Native service** | The `alixai` binary under systemd (a Windows service, a launchd daemon on the other platforms); LocalAI as a container beside it or, on Linux, as the separate `localai.service` unit. | You want no container runtime for AlixAI. | `deploy/native/README.md` |

The profile files cap LocalAI exactly as `deploy/container/localai/compose.<profile>.yaml` does. Anything measured under them is **emulated**, never certified.

| Profile | Memory (no swap) | CPUs | Resident backends | Role |
| --- | --- | --- | --- | --- |
| `lite` | 8 GB | 4 | 2: controller, embedder | the Lite target |
| `balanced` | 16 GB | 8 | 4 | Balanced |
| `4g` | 4 GB | 4 | 1: the controller | **experimental**: an aspiration until the full workload passes its gates; it runs the Lite context budgets |

Budgets and their basis: `docs/operations/resource-budgets.md`. The AlixAI container itself is capped at 512 MB; the core measured 96 to 160 MB there.

## 2. Prepare the host

- Shape A: Docker Compose v2 (validated with Docker 29.7.2 and Compose 5.5.1, `deploy/container/localai/README.md`); `podman compose` takes the same files.
- Shape B: Podman 5.2 or later (`.pod` and `.build` units), systemd user services, `loginctl enable-linger` for start at boot, and the `memory` and `cpu` cgroup
  controllers delegated to the user for limits to take effect.
- Shape C: systemd 247 or later for the unit (`LoadCredential`, `ProtectProc`).
- A **LocalAI backend and a model** for answers. The stacks have no route out, so LocalAI cannot download anything: you pull the digest-pinned backend and the
  sha256-pinned model file on the host first (`deploy/container/localai/README.md`, "Backend and model pull from the host"), and AlixAI re-hashes every configured model
  before it serves a request. At the time of writing no controller model in `models/registry` is activatable ([local machine](local-machine.md), section 1), so a fresh
  install runs with the **inference path off**.

## 3. Install

### A. Container stack with Docker Compose

```bash
cd deploy/container/stack
cp .env.example .env                       # set LOCALAI_API_KEY (openssl rand -hex 32)
mkdir -p data/alixai data/localai/models data/localai/backends data/localai/state
chmod 700 data/alixai
# Linux with rootful Docker: the data directories belong to the container users. Docker Desktop ignores owners.
sudo chown 65532:65532 data/alixai
sudo chown -R 10001:10001 data/localai

docker compose -f compose.yaml -f compose.lite.yaml up -d --build      # or compose.balanced.yaml, compose.4g.yaml
curl -fsS http://127.0.0.1:7470/healthz
curl -sS  http://127.0.0.1:7470/readyz
docker compose -f compose.yaml -f compose.lite.yaml down                # keeps ./data
```

Source: `deploy/container/stack/README.md`, "Prepare the host". **Run** on Docker Desktop for Windows (**emulated**): the images build, the four containers start in the gated
order and report healthy with a stand-in for LocalAI, `/healthz` answers through the published loopback port and `/readyz` reports the inference path off. **Not run:** the real
LocalAI image inside the stack, rootful Docker on Linux, `podman compose`, ARM64.

Provision the backend and models into `data/localai/backends` and `data/localai/models` **before** the first start. The configuration is `config/alixai.yaml`, mounted read-only:
edit the host file and recreate the service to change it. It has **no `identity:` section**, so the `/v1` API refuses every request until you add `identity.deployment_id`
(section 7).

### B. Container stack with rootless Podman and Quadlet

```bash
cd deploy/container/quadlet
Q=~/.config/containers/systemd; mkdir -p "$Q" ~/.config/alixai/localai ~/.local/share/alixai/{alixai,localai/{models,backends,state}}
cp *.container *.pod *.network *.build "$Q"/                       # edit the two paths in alixai*.build to your checkout
cp -r profiles/lite/* "$Q"/                                        # or balanced, 4g
install -m 0644 localai.env.example ~/.config/alixai/localai.env
install -m 0600 alixai.env.example  ~/.config/alixai/alixai.env     # set LOCALAI_API_KEY
install -m 0644 ../stack/config/alixai.yaml ~/.config/alixai/alixai.yaml
install -m 0644 ../localai/config/*.yaml ~/.config/alixai/localai/
install -m 0644 ../localai/entrypoint.sh ~/.config/alixai/localai-entrypoint.sh
podman unshare chown 65532:65532 ~/.local/share/alixai/alixai; chmod 700 ~/.local/share/alixai/alixai
podman unshare chown -R 10001:10001 ~/.local/share/alixai/localai
systemctl --user daemon-reload
systemctl --user start alixai-relay.service                         # pulls the whole chain in order
```

Source: `deploy/container/stack/README.md`, "Rootless Podman with Quadlet". **Static** (`TestQuadletHardening`, `TestQuadletStartOrderIsHealthGated`): written to
`podman-systemd.unit(5)` and **not run on a Linux host**; Podman is not installed on the dev box. The start order is `alixai-localai`, `alixai-anchor`, `alixai`, `alixai-relay`,
each gated on the previous one's health check (`Notify=healthy`). Enable linger and delegate the cgroup controllers as the README says.

### C. Native service

Linux with systemd (`deploy/native/README.md`):

```bash
sudo install -m 0755 alixai alixai-parser /usr/local/bin/         # the parser worker must sit beside the service binary
sudo install -d -m 0755 /etc/alixai /var/lib/alixai-models      # models: root-owned, read-only to the service
sudo install -m 0644 systemd/alixai.yaml /etc/alixai/alixai.yaml
openssl rand -hex 32 | sudo install -m 0600 /dev/stdin /etc/alixai/localai-api-key   # give LocalAI the same key
sudo install -m 0644 systemd/alixai.service /etc/systemd/system/alixai.service
sudo systemctl daemon-reload && sudo systemctl enable --now alixai
curl -fsS http://127.0.0.1:7470/healthz
systemd-analyze security alixai.service                          # the exposure score of the sandbox
```

LocalAI as the separate unit, on Linux (skip it when LocalAI is a container published on `127.0.0.1:8080`; `alixai.yaml` attaches to either the same way):

```bash
sudo install -d -m 0755 /opt/localai                              # then put the pinned LocalAI binary there as local-ai, root-owned, mode 0755
sudo install -m 0644 systemd/localai.yaml /etc/alixai/localai.yaml
sudo install -m 0644 systemd/localai.service /etc/systemd/system/localai.service
sudo systemctl daemon-reload && sudo systemctl enable --now localai
systemd-analyze security localai.service
```

The document parser worker, `alixai-parser`, is in every release archive beside `alixai` and in the service image at `/usr/local/bin/alixai-parser`; the native steps install it next to the
service binary (macOS the same; Windows `C:\Program Files\AlixAI\alixai-parser.exe`) and `alixai serve` finds it there, so RAG over files needs no configuration. It runs inside the unit's
sandbox and counts against its `MemoryMax=512M` and `TasksMax` (`deploy/native/README.md`, "The document parser worker"; [index](README.md), gap 2). Without it parse jobs fail closed.

Windows (elevated PowerShell) and macOS (launchd) have their own sequences in the same file. **Static** (`go test ./deploy/native`, including `localai_unit_test.go`): the units, the plist and the Windows
service are checked but have not run on a real host, and the LocalAI unit and its supervisor were never run against a real LocalAI ([`docs/operations/localai-supervisor.md`](../localai-supervisor.md)). The AlixAI
unit caps memory at 512 MB with no swap and one CPU, the caps of the stack's `alixai` service. A Windows service and a launchd daemon have **no memory or CPU cap at all** (ADR 0012). The LocalAI binary is not
part of these packages: pin it yourself, and provision its backends into `/var/lib/alixai-localai/backends` before the first start, because the unit may not reach the network. Whatever runs LocalAI runs it under its own
identity and with the same API key that `/etc/alixai/localai-api-key` holds. `alixai serve` only **attaches** to LocalAI (`supervisor.runtime: unit` in the shipped `systemd/alixai.yaml`): it health-checks it, backs off while it is
unhealthy and settles the actions in flight when it dies, and it never starts it. If the supervisor gives up, the model path stays unavailable until `alixai` itself is restarted.

## 4. Service identity

AlixAI and LocalAI never share an operating-system identity.

| Shape | AlixAI runs as | LocalAI runs as | Other hardening (checked statically) |
| --- | --- | --- | --- |
| Container stack | uid 65532; forwarders 65534 | uid 10001 | read-only root, `cap_drop: [ALL]`, `no-new-privileges`, PID and memory limits, bind-mounted data, no init container (`TestComposeHardening`) |
| systemd | `DynamicUser` (a transient user that owns nothing else) | its container, or `localai.service`, a `DynamicUser` of its own with no capability and a loopback-only IP filter | `ProtectSystem=strict`, `PrivateTmp`, `NoNewPrivileges`, empty capability set, `IPAddressDeny=any` with `IPAddressAllow=localhost`, `MemoryMax=512M`; the LocalAI key arrives through `LoadCredential`, never the environment |
| Windows service | the virtual account `NT SERVICE\AlixAI` (or `NT AUTHORITY\LocalService` with `-account`); LocalSystem and named users are refused | its container | ACL on `C:\ProgramData\AlixAI` cut to the service, Administrators and SYSTEM (`icacls` lines of `deploy/native/README.md`) |
| launchd | the hidden account `_alixai`, umask 0077 | its container | restart on failure; no sandbox profile and no resource cap |

The data directory is owner-only whatever the shape, and key files are refused when other users can read them.

## 5. Configuration

The file is the one each package ships (`deploy/container/stack/config/alixai.yaml`, `deploy/native/*/alixai.yaml`); all four pass `alixai config check` (run in M5-13). Any key can
be set from the environment as `ALIXAI_<SECTION>_<FIELD>`; the profile files do that for `profile`. Nothing secret goes in the file: `localai.api_key_ref` takes `env:NAME` or
`file:PATH`. The sections an operator touches:

| Section | Keys | Notes |
| --- | --- | --- |
| `data_dir` | path | Absolute for a service. Database, keys, audit log and file trees live here. |
| `server` | `listen` | Default `127.0.0.1:7470`. `unix_socket` is **refused** by this build (`server.unix_socket is not served by this build of the gateway yet; unset it`). |
| `gateway` | `read_header_timeout` 5s, `read_timeout` 30s, `write_timeout` 2m, `idle_timeout` 60s, `shutdown_timeout` 10s, `max_body_bytes` 65536, `max_header_bytes` 16384, `tls.*` | Defaults in `internal/gateway/config.go`. `tls` is for a listener beyond loopback ([private VPS](private-vps.md)). |
| `identity` | `deployment_id`, `audience`, `max_token_lifetime` (never above 15m) | **Required** for `/v1`. [Key provisioning](../key-provisioning.md). |
| `keys` | `checkpoint_key_file`, `checkpoint_key_id`, `checkpoint_every` 100, `checkpoint_interval` 5m | Audit checkpoint signing. |
| `integration_audit` | `signing_key_id`, `signing_key_file`, `accept_operators`, `max_report_age_hours` | [Operator commands](operator-commands.md), section 2. |
| `profile` | `lite`, `balanced`, `performance`, `experimental-4g` | The context budgets and limits; an unknown name stops the start. |
| `localai`, `inference` | `endpoint`, `api_key_ref`, `qualified_versions`; `models_dir`, `models` | [Local machine](local-machine.md), section 6. |
| `supervisor` | `enabled` (default false), `runtime` (`unit`; `exec` is for the LocalAI unit's own configuration and `serve` refuses it), optional restart tuning | The shipped `systemd/alixai.yaml` sets `enabled: true`, `runtime: unit`. The container installs stay at `false`: compose or Quadlet restarts LocalAI. [`docs/operations/localai-supervisor.md`](../localai-supervisor.md). |
| `metrics` | `enabled` (default false), `listen` (loopback only) | Section 9. |
| `log` | `level`, `redact_keys` | JSON lines; secrets redacted by key name and by shape. |

`alixai config check -config FILE` validates a change before you restart anything. `alixai serve` refuses to start on anything it can refuse from the configuration alone, **before**
it creates a file or opens a port: a listener beyond loopback without TLS, an unknown profile, an unusable checkpoint key, a half-wired `/v1` area.

## 6. Listener and network posture

- AlixAI listens on `127.0.0.1:7470`. A host application on the same machine calls it there. In the container stack AlixAI listens on loopback **inside** the anchor container's
  network namespace and the `relay` container publishes it on the host's `127.0.0.1:${ALIXAI_PORT:-7470}`; do not change that `ports` entry to another address
  (`TestPortsLoopbackOnly` and the repository's compose gate refuse it). The two hops inside the stack are plaintext on bridge networks that nothing else joins; AlixAI sees the relay
  as the peer of every request.
- LocalAI has no published port and, in the stack, no route out: both networks it shares are `internal: true`. **Outbound traffic is denied by default.** External provider mode is an
  explicit opt-in (`compose.external.yaml`, `deploy/container/quadlet/external`, or a systemd drop-in that widens `IPAddressAllow`) that makes the deployment network-dependent;
  it is outside this guide.
- A caller on **another machine** needs a TLS listener: either AlixAI's own (`gateway.tls`, native installs only) or a reverse proxy on the host that terminates TLS and forwards
  to the loopback port. Both are in the [private VPS guide](private-vps.md); the same rules apply on a private network. Do not expose the plaintext port.

## 7. First-run provisioning

In this order, with the commands of [operator commands](operator-commands.md):

1. Set `identity.deployment_id` (and `audience`) in the configuration and restart. Until then every `/v1` request answers 503 `the /v1 API is not enabled`.
2. `alixai keys init -config FILE`: the checkpoint signing key.
3. Provision the audit report signer, then per application: `trust add-key`, `identity add-issuer-key`, `package install`, `audit run`.

**For a packaged install, where these commands run is an open point.** They must open the service's data directory, as an identity that may write it, and on the same
host (`audit run` reaches the running server through `<data_dir>/admin-endpoint.json`). No earlier task ran them against the container stack's or the unit's data
directory, so test them on a scratch install first:

- *Container stack.* The image's entrypoint is the `alixai` binary, the container runs as uid 65532 (the owner of `./data/alixai`) and has the admin interface in its own
  namespace, so running the command in the existing `alixai` container with `-config /etc/alixai/alixai.yaml` gives the right identity and paths. The container has only the
  data directory, the configuration and the models mounted, so stage a package under the data directory to give the command something to read.
- *systemd with `DynamicUser`.* The state directory belongs to the unit's transient user. A command run as root creates root-owned files in it; a key file the service cannot read stops
  `serve`. Check the ownership of what the command made before starting the service again.
- *Windows service.* Files created inside `C:\ProgramData\AlixAI\data` take the directory's ACL, which grants the service identity modify; confirm it after the first command.

## 8. Operate

| Task | How |
| --- | --- |
| Start, stop, restart | Compose: `docker compose -f compose.yaml -f compose.lite.yaml up -d` and `down` (keeps `./data`); Quadlet: `systemctl --user start alixai-relay.service`; native: `systemctl enable --now alixai`, `Start-Service AlixAI`, `launchctl bootstrap`. Stop is graceful: in-flight requests get `gateway.shutdown_timeout` and a final audit checkpoint is signed. The containers restart `unless-stopped`; the unit restarts `on-failure`. |
| Back up | `alixai backup create`, signed, while the service runs. Schedule it yourself, and copy the backup and `backup.pub` off the machine: [backup and restore](backup-restore.md). |
| Upgrade or roll back | [Upgrades](upgrades.md). |
| Rotate or revoke a key | [Key provisioning](../key-provisioning.md) and [operator commands](operator-commands.md), section 4. |
| Logs | JSON lines on stderr. Compose keeps three files of 10 MB per container (`json-file`); the unit logs to journald under `alixai`; the Windows service logs to the Application event log, source `AlixAI`. |
| Disk | The data directory grows with the database, the file trees and the audit log. There is no automatic pruning of backups: delete old backup directories yourself after verifying a newer one. |

## 9. Monitoring

AlixAI sends **no telemetry** and pushes nothing anywhere. Everything below is read by you.

| Signal | Where | What it tells you |
| --- | --- | --- |
| Liveness | `GET /healthz` | The process serves. Anonymous. |
| Readiness | `GET /readyz` | Store, inference path (with the model roles and their status), ingestion and, when the supervisor is on, `runtime_supervisor` (it fails with the supervisor's state while LocalAI is not running); 503 with `Retry-After` while something is not ready, and the body names it. Anonymous; it reuses the last provider probe for `gateway.health_ttl` (5s), so it cannot be turned into load on the runtime. |
| Prometheus text | `GET http://127.0.0.1:9464/metrics` when `metrics.enabled: true` | Families `alixai_queue_*`, `alixai_provider_*`, `alixai_resource_*`, `alixai_process_*`, `alixai_build_info`, `alixai_scrape_collector_up`. Loopback only: the listener refuses any non-loopback address and any request that is not from loopback. |
| Diagnostics bundle | `GET http://127.0.0.1:9464/diagnostics` with header `X-Alixai-Admin-Token` | Versions, redacted configuration, health, queue, resource use and recent warnings and errors, for a bug report. The token is the `token` member of `<data_dir>/admin-endpoint.json` and changes at every start; without it the answer is 403. |
| Host-visible operations | `GET /v1/ops/queue`, `GET /v1/ops/providers`, `POST /v1/ops/audit/export` | The calling application's own queue counts, the model roles and its own audit events, each behind an exact capability in the context token. Nothing deployment-wide. |

**Run:** `internal/metrics` and `internal/diag` tests (`TestNoOutboundTelemetry` among them), and by hand in M5-13 (the family names and the 403 above). Exporting is your decision: point
your own collector at the loopback listener from the same machine. **The container stack publishes only the API port**, so the loopback metrics listener inside AlixAI's namespace is not
reachable from the host; there is no scrape path for the stack yet ([index](README.md), gap 6).

## 10. Check the installation

- `/healthz` answers 200 on `127.0.0.1:7470` and `/readyz` answers with the state you expect (503 naming the inference path while no model is configured).
- Nothing but the API port is bound outside the machine: LocalAI's `8080`, the metrics port and the admin port are not reachable from another host.
- `alixai identity list` shows the issuer keys you provisioned and none marked `TEST-ONLY`; `alixai trust list -app alixai.platform` shows the checkpoint key and the audit signer.
- `alixai backup create` succeeds and `alixai backup verify` verifies it with the public key you copied off the machine.
