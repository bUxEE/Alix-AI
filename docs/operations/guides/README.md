# Deployment guides

Status: written in task M5-13, 2026-10-03.

These guides are for the person who installs and runs an AlixAI deployment: the **operator**. AlixAI is headless (no dashboard, no admin UI before the
deferred M7 kits), so everything here is a configuration file, a command line or an HTTP probe.

| Guide | Read it when |
| --- | --- |
| [Local machine](local-machine.md) | One person, one machine (workstation, laptop, a test box). Loopback only; nothing is exposed. |
| [Server](server.md) | A machine you administer (on premises, a rack, a cloud VM on a private network) that runs AlixAI as a system service for host applications on the same machine or network. |
| [Private VPS](private-vps.md) | A rented machine that the internet can reach. Adds TLS, firewall, service identity and the listener rules to everything in the server guide. |
| [Browser hosts](browser-hosts.md) | A web application that runs in a browser calls AlixAI. Read before opening any listener to a browser origin. |
| [Operator commands](operator-commands.md) | Reference for every `alixai` command an operator runs, who the operator is, and which key is which. |
| [Backup and restore](backup-restore.md) | Making, verifying and restoring a signed backup; disaster recovery. |
| [Upgrades](upgrades.md) | Staged upgrade, rollback and interrupted-upgrade recovery. |
| [Troubleshooting](troubleshooting.md) | A symptom, its cause and the fix. |
| [Light External (no local LLM)](light-external.md) | Chat (and optional local STT) with External mode, no LocalAI LLM; per-layer routing presets. |
| [Hardware and pricing](../hardware-and-pricing.md) | Dedicated / shared private LLM GPU segments, rents, break-even vs light External. |

Policies that go with them: [compatibility and deprecation](../../policies/compatibility.md), [vulnerability response](../../policies/vulnerability-response.md),
[SECURITY.md](../../../SECURITY.md).

## What runs where

| Part | Address | Who can reach it | Setting |
| --- | --- | --- | --- |
| API gateway: `/healthz`, `/readyz`, `/v1/...` | `127.0.0.1:7470` | The machine. A listener beyond loopback is refused unless TLS is configured (SEC-093). | `server.listen`, `gateway.tls` |
| LocalAI runtime | `127.0.0.1:8080` (the container stack: a private network) | AlixAI only. Never published. | `localai.endpoint` |
| Metrics and diagnostics | `127.0.0.1:9464`, **off by default** | Loopback only; any other address fails the configuration load. | `metrics.enabled`, `metrics.listen` |
| Operator admin interface | `127.0.0.1`, a random port, written with its credential to `<data_dir>/admin-endpoint.json` | The OS user that can read the data directory. Not configurable and never beyond loopback, with or without TLS. | none |

`/healthz` and `/readyz` are the only unauthenticated routes of the gateway; every `/v1` route needs a context token that the host application minted
(`docs/operations/key-provisioning.md`). Sources: `internal/gateway/doc.go`, `internal/metrics/config.go`, `internal/gateway/routes_admin.go`.

## Which install shape

ADR [0012](../../decisions/0012-lite-install-native-vs-container.md) decides it: the **container stack** (AlixAI and LocalAI as separate non-root containers,
rootless Podman with Quadlet first on Linux, Docker Compose supported) is the preferred install, because its limits and hardening are enforced by the kernel
and checked by tests, and it is the shape the measurements were taken in. The **native service** (systemd unit, Windows service, launchd daemon of
[`deploy/native`](../../../deploy/native/README.md)) is always shippable and supported; LocalAI then runs as a container beside it or, on Linux, as the separate `localai.service` unit under the exec supervisor
([`docs/operations/localai-supervisor.md`](../localai-supervisor.md)).

## How to read the commands in these guides

Every command block cites the file it comes from and carries one of three states. A command a task never ran is not given as a command: the guide says what
is required in prose and says that nothing has run it.

| State | Meaning |
| --- | --- |
| **Run** | Run by the cited task on the dev box (Windows 11, Docker Desktop, **emulated**, never certified), or exercised by an automated test of the cited package. Where this task also ran a command by hand, the guide says so. |
| **Static** | Written to the platform's documented syntax and checked only by static tests (`go test ./deploy/native ./tests/deploy`). Not run on that platform. |
| **Not run** | Stated as a requirement; no earlier task ran it. |

Commands are shown for the installed binary, `alixai`. From a source checkout, `go run ./cmd/alixai <command>` is the same program.

## Known gaps

These are true of the repository today. A guide that depends on one says so where it matters.

1. **No release exists yet.** The release workflow (`.github/workflows/release.yml`) has never run (`docs/operations/release.md`, section 4), so there is no signed
   release manifest, archive or image to download. The guides build from source (`CGO_ENABLED=0 go build ./cmd/alixai`) or use the stack's local image build.
2. **The document parser worker is packaged (closed in M5-22), but nothing has run it from a package.** `alixai-parser` (`cmd/alixai-parser`, ADR 0014) is a separate executable
   that `alixai serve` starts once per document, and it must sit **in the same directory as `alixai`** (or be named by `rag_ingest.parser.worker_path`, an absolute path). Where it lives:

   | Install shape | `alixai-parser` | Source |
   | --- | --- | --- |
   | Release archive (every platform) | at the root of the archive, beside `alixai` (`alixai-parser.exe` in the Windows zip) | `.goreleaser.yaml`, second build |
   | Service image and container stack (Docker and Podman) | `/usr/local/bin/alixai-parser`, beside `/usr/local/bin/alixai`; the `netctl` image does not carry it | `deploy/container/alixai/Dockerfile` |
   | Native, Linux and macOS | `/usr/local/bin/alixai-parser` | `deploy/native/README.md` |
   | Native, Windows | `C:\Program Files\AlixAI\alixai-parser.exe` | `deploy/native/README.md` |
   | Source checkout | `go build -o alixai-parser ./cmd/alixai-parser`, next to the `alixai` you built | |

   No unit, plist, compose file or configuration changes for it: the worker confines itself (ADR 0014) inside the service's own sandbox, and the shipped files keep that sandbox's
   limits at their defaults. **Static** (`TestPackagedInstallsShipTheParserWorker`): no archive or image has been built from this change and no packaged install has ingested a file. Without
   the worker `alixai serve` starts, logs `rag: ingestion starts without a ready parser worker` and parse jobs fail closed and are retried.
3. **The native units have not run on a real host.** The systemd unit and the launchd plist are checked statically; the Quadlet units have not run on Linux; Podman
   is not installed on the dev box (ADR 0012, "What was not measured"). Every figure is **emulated**; no profile is **certified**.
4. **Operator commands against a packaged install are not exercised.** The CLI has been run in-process against a data directory, and by hand on the dev box; nobody
   has run it against the data directory of the container stack or of the DynamicUser systemd unit. The guides state the constraint (the files it creates must end
   up owned by the service identity) and leave the procedure to you.
5. **No update metadata.** The release manifest is one signed statement about one release; there are no TUF-style timestamp, snapshot or freeze roles
   (`docs/operations/release.md`, section 4). `alixai upgrade apply` refuses a release that is not newer than the recorded one, which is its only replay protection.
6. **No metrics path from the container stack.** The metrics listener is loopback inside AlixAI's own network namespace and the stack publishes only the API port.
7. **The `-operator` flag is not authentication.** See [Operator commands](operator-commands.md), section 1.
