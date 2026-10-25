# Guide: local machine

Status: written in task M5-13, 2026-10-03. Read the [guides index](README.md) first for the legend (**Run**, **Static**, **Not run**) and the known gaps.

One person, one machine: a workstation, a laptop or a test box. AlixAI listens on the loopback interface only, nothing is exposed, and no certificate is needed. Use
this guide to evaluate AlixAI, to develop a host application against it, or to run a single-user deployment. For a machine other applications or people reach, use the
[server guide](server.md).

## 1. What you need

| Need | Notes |
| --- | --- |
| The `alixai` binary | Built from source today (section 2); a release archive once a release exists. |
| A data directory | Everything AlixAI keeps: database, key files, audit log, file trees. Owner-only. |
| LocalAI plus a model, for answers | Optional to start. Without them the service runs with the **inference path off**: `/healthz` answers and `/readyz` says why the model path is unavailable. |
| Memory | The Lite profile is sized for an 8 GB machine (`docs/operations/resource-budgets.md`, section 3). All figures there are **emulated**, never certified. |

At the time of writing no conversational (controller) model of `models/registry` is activatable: four are `rejected` and the embedding model is
`qualified-emulated`. Keep `inference.models` empty until model qualification (M5-09) says otherwise; the rest of the service, including packages and audits,
works without it.

## 2. Get the binary

```bash
CGO_ENABLED=0 go build -o alixai ./cmd/alixai      # Windows: -o alixai.exe
./alixai version
```

**Run.** `CGO_ENABLED=0 go build ./cmd/alixai` is the build of `.goreleaser.yaml` and of M5-02's verify steps (`docs/decisions/0012-lite-install-native-vs-container.md`);
`alixai version` was run in M5-13. The binary is static and pure Go: no runtime, no container, no C toolchain. It is the **only** file `serve` needs, except for document
ingestion, which also needs the worker `alixai-parser` in the same directory: a release archive carries it, and from a source checkout you build it too
(`CGO_ENABLED=0 go build -o alixai-parser ./cmd/alixai-parser`; `.exe` on Windows; [index](README.md), gap 2).

## 3. Configure

Write `alixai.yaml` next to the binary. This is the smallest useful file; every key not shown keeps its default (`internal/config/core.go`: listen `127.0.0.1:7470`,
data directory `data`, log level `info`).

```yaml
data_dir: ./data              # relative to where you start it; use an absolute path for anything that runs as a service
server:
  listen: 127.0.0.1:7470      # loopback only; the gateway refuses another address without TLS
log:
  level: info
profile: lite                 # lite, balanced, performance or experimental-4g: the context budgets and limits
identity:
  deployment_id: dep_local    # without it the /v1 API refuses every request
  audience: alixai            # what context tokens must carry as aud; empty means deployment_id
inference:
  models: []                  # empty keeps the inference path off
```

```bash
./alixai config check -config alixai.yaml
```

**Run** (`cmd_config_check.go`; this exact file, with the data directory elsewhere, was checked in M5-13). Every key can also be set from the environment as
`ALIXAI_<SECTION>_<FIELD>` (`ALIXAI_PROFILE`, `ALIXAI_SERVER_LISTEN`); `ALIXAI_CONFIG` names the file. `config check` reads no key or certificate file.

## 4. Provision the keys

```bash
./alixai keys init -config alixai.yaml
```

**Run** (`cmd_keys_test.go`; M5-13). It creates `<data_dir>/keys/audit-checkpoint.key` (0600, directory 0700) and registers the public half, so `serve` can sign audit
checkpoints. Without it `serve` starts and warns `audit checkpoints are not signed`. On Windows, restrict `data\keys` to your own account (the command says so).
The other keys (issuer, publisher, audit signer) are provisioned per application: [operator commands](operator-commands.md), sections 3 and 4.

## 5. Run

```bash
./alixai serve -config alixai.yaml
```

```bash
curl -fsS http://127.0.0.1:7470/healthz        # {"status":"ok"}
curl -sS  http://127.0.0.1:7470/readyz          # 503 until the model path is up; the body names the failing check
```

**Run** (`cmd_serve_test.go`, `deploy/native/README.md`; M5-13 started it, probed both routes and stopped it). With `inference.models: []`, `/readyz` answers 503 with
`inference path is off: no model is configured for verification`: that is the designed state, not a fault. Logs are JSON lines on stderr, with secrets redacted.
Stop it with Ctrl-C or SIGTERM: in-flight requests get `gateway.shutdown_timeout` (10 s) and a final audit checkpoint is signed. A process that is killed hard
(on Windows, Task Manager or `taskkill`) leaves `alixai.db-shm` behind; `backup restore` and `upgrade` then refuse until you pass `-force`
([troubleshooting](troubleshooting.md)).

The first start with nothing provisioned logs two warnings that are expected here: `no issuer key is provisioned: the /v1 API refuses every token` until you add one,
and the parser-worker warning if `alixai-parser` is not beside the binary.

## 6. Add a model runtime (optional)

LocalAI runs as a private container published on `127.0.0.1:8080`, and AlixAI calls it with one shared key. The steps, including pulling the pinned backend and the
sha256-pinned model file on the host, are in [`deploy/container/localai/README.md`](../../../deploy/container/localai/README.md) ("Run" and "Backend and model pull from the
host"); they were run live in M0-13 and M0-18 on Docker Desktop (emulated). Then point AlixAI at it:

```yaml
localai:
  endpoint: http://127.0.0.1:8080
  api_key_ref: env:LOCALAI_API_KEY          # or file:/path/to/key; never the key itself
  qualified_versions: ["v4.10.0"]           # a LocalAI version AlixAI was not qualified against is reported unavailable (SEC-072)
inference:
  models_dir: /path/to/localai/data/models  # AlixAI re-hashes every listed model against its registry pin before serving (SEC-063)
  models: [<registry id>]
```

(`deploy/container/stack/config/alixai.yaml`; the four shipped configuration files all pass `alixai config check`, run in M5-13.) A plaintext LocalAI endpoint that is not
loopback is refused (SEC-065). A model file whose hash differs from its pin is refused.

## 7. Run it as a service of this machine (optional)

For a machine that should start AlixAI at boot, use the packaged service definitions of [`deploy/native/README.md`](../../../deploy/native/README.md): the
Windows service (`alixai service install -config C:\ProgramData\AlixAI\alixai.yaml`, elevated, then the `icacls` lines of that file), the systemd unit or the launchd
daemon. These are **Static**: checked by `go test ./deploy/native`, not run as a service. Read the [server guide](server.md), section 4, before using them.

## 8. Connect an application

An application is a signed package plus an issuer key; the order is in [operator commands](operator-commands.md), section 3. The reference applications under
`examples/reference-apps/` are the worked examples. A browser application should reach AlixAI through its own backend ([browser hosts](browser-hosts.md)).

## 9. Back up, upgrade, remove

- `alixai backup create -config alixai.yaml -dir ./backups` works while the service runs; copy `data/keys/backup.pub` somewhere that is not this machine
  ([backup and restore](backup-restore.md)).
- Upgrading is a staged, reversible procedure ([upgrades](upgrades.md)).
- To remove everything: stop the service and delete the data directory. `alixai service uninstall` removes the Windows service and **keeps** the data.

## 10. What this setup does and does not protect

Loopback means nothing off the machine can connect. Every other account on the same machine can still reach the port: `/healthz` and `/readyz` answer anyone, and
`/v1` needs a context token the host application minted, which nobody can forge without its private key. What protects the data itself is the operating system:
the data directory is owner-only, so on a shared machine keep it under your own account.
