# Guide: troubleshooting

Status: written in task M5-13, 2026-10-03. Every message below is quoted from the code or from a run (M5-13 ran `serve`, `config check`, `backup`, `keys init` and `trust` on the dev box); the source
is in the last column or in the text. Messages on stderr can change between releases (see [compatibility](../../policies/compatibility.md)); the cause stays.

First look: `alixai config check -config FILE`, the JSON log on stderr (the container's log, `journalctl -u alixai`, the Windows Application event log, source `AlixAI`), `GET /healthz` and `GET /readyz`.
For a bug report, the redacted diagnostics bundle ([server guide](server.md), section 9).

## 1. The service will not start

`serve` refuses what it can from the configuration alone **before** it creates a file or opens a port.

| Message | Cause and fix |
| --- | --- |
| `gateway: refusing to listen on 0.0.0.0:7443: a listener beyond loopback needs tls.cert_file and tls.key_file` | `server.listen` is not loopback and `gateway.tls` is unset. Set both files, or listen on `127.0.0.1` ([private VPS](private-vps.md)). Run in M5-13. |
| `gateway: invalid TLS configuration: gateway: tls certificate: open ...` | The certificate or key file cannot be read, or `client_ca_file` holds no PEM certificate. Run in M5-13. |
| `dev.spike_endpoint is unauthenticated and needs a loopback or unix socket listener` | `dev.spike_endpoint` is on with a non-loopback listener, TLS or not. Turn it off. |
| `server.unix_socket is not served by this build of the gateway yet; unset it` | A unix socket is not supported yet. |
| `config: section metrics: ... is not a loopback address: metrics are only ever served on a loopback address` | `metrics.listen` must be loopback; no setting changes that. Run in M5-13. |
| `serve: the checkpoint signing key ...` | A configured checkpoint key cannot be used: missing, not a regular file, not an Ed25519 PKCS#8 key, readable by other users, not registered in the trust store, revoked or retired, or not the registered public key. `docs/operations/key-provisioning.md`, section 2.2. |
| `route areas without their services` | A route area is registered in the binary and nothing provided its services: a wiring bug found at start instead of as a 404. Report it with the diagnostics bundle. |
| `alixai: config: FILE: unknown section "x" (known: ...)`, or `field x not found in type config.ServerConfig` | A section or key that does not exist (a typo, or a key from another release); the file is refused as a whole. `config check` names it. Run in M5-13. |
| `config: section profile: ... the deployment profile is not known (want one of balanced, experimental-4g, lite, performance)` | An unknown `profile`. There is no fallback to a guessed profile. Run in M5-13. |
| Compose: `set LOCALAI_API_KEY in .env` | No `.env`. `cp .env.example .env` and set the key (`deploy/container/stack/README.md`). |
| The LocalAI container exits with `alixai-localai: refusing to start: ...` | A guard in `entrypoint.sh`: a weak key, a writable config directory, an unwritable data directory, verbose logging. Fix what the message names (`deploy/container/localai/README.md`, "Troubleshooting"). |

## 2. Running, but not ready or not useful

| Symptom | Cause and fix |
| --- | --- |
| `/readyz` answers 503 with `inference path is off: no model is configured for verification (inference.models)` | The designed state while `inference.models` is empty. At the time of writing no controller model is activatable ([local machine](local-machine.md), section 1). |
| `/readyz` 503, role `controller` `not configured` | The same: no model is configured. |
| `/readyz` 503 with a LocalAI check failing | LocalAI is unreachable, unqualified (`localai.qualified_versions`) or still starting; a first start downloads or re-hashes models and can take minutes (`deploy/container/localai/README.md`). Under the systemd unit with LocalAI in a container, some setups rewrite loopback to the bridge address before the cgroup IP filter sees it: add the bridge subnet to `IPAddressAllow=` in a drop-in (comment in `deploy/native/systemd/alixai.service`). |
| `/readyz` 503 with `inference_warmth` `warming up` | The first model call is loading the model (8 to 50 s measured). It turns ready when the call returns. |
| Every `/v1` request answers 503 `the /v1 API is not enabled: no identity verification is configured` | `identity.deployment_id` is not set. Run in M5-13. |
| Log: `no issuer key is provisioned: the /v1 API refuses every token until ... identity add-issuer-key adds one` | No host application can authenticate yet. [Operator commands](operator-commands.md), section 3. Run in M5-13. |
| Log: `audit checkpoints are not signed: no platform checkpoint signing key is provisioned (run alixai keys init)` | Events are still hash-chained; only the signed anchor is missing. `alixai keys init`. Run in M5-13. |
| Log: `rag: ingestion starts without a ready parser worker; parse jobs fail closed and are retried` | `alixai-parser` is not in the directory of the `alixai` that is running. The release archives and the service image carry it there (`/usr/local/bin/alixai-parser`, `C:\Program Files\AlixAI\alixai-parser.exe`; [index](README.md), gap 2): copy it next to the binary you installed, or set `rag_ingest.parser.worker_path` to its absolute path. Run in M5-13. |
| Log: the parser worker took longer than half its startup limit (Windows) | An antivirus scans the freshly started worker (26.5 s measured under Avast). Exclude the executable from scanning or raise `rag_ingest.parser.startup_timeout`. |
| `/readyz` 503 with the check `runtime_supervisor` failing (native install with `supervisor.enabled: true`) | The supervisor reports LocalAI as starting, in backoff after a failure, or **gave up**. After it gave up, the model path stays unavailable until `alixai` itself is restarted; systemd restarts the LocalAI unit separately (`Restart=on-failure`). Look at the journal of `localai.service` and of `alixai` (`docs/operations/localai-supervisor.md`). |
| Compose: `/healthz` passes but the published port is dead after the anchor restarted | In Compose only, `alixai` keeps the namespace it had. `docker compose restart anchor alixai relay` (`deploy/container/stack/README.md`). |
| Compose or Podman: `cpus` exceeds the host's cores, or limits are ignored on rootless Podman | Lower `cpus`; rootless Podman enforces limits only on cgroup v2 with the `memory` and `cpu` controllers delegated (`deploy/container/localai/README.md`, "Profiles"). |
| 405 `method not allowed` to a browser's `OPTIONS` request, and no CORS headers | By design: AlixAI sends no CORS headers. [Browser hosts](browser-hosts.md). Run in M5-13. |

## 3. Callers are refused

| Symptom | Cause and fix |
| --- | --- |
| 401 `unknown_key` | No issuer key for that `kid`, or it was revoked: `alixai identity list`. |
| 401 `wrong_audience` | The token's `aud` is not `identity.audience` (or `identity.deployment_id`). The package's declared audience and the deployment's differ. |
| 401 `wrong_issuer` or `wrong_application` | The token's `iss` or `application_id` is not the one the key was registered for. |
| `replayed` | A context token was presented twice. Tokens are single use: mint one per request and per stream open ([browser hosts](browser-hosts.md)). |
| `quarantined` on every route of an application | Its package is not active: installed but not audited, rejected, superseded or quarantined. `alixai audit run <app>`. |
| Turn `unavailable` in light External | Tenant effective mode is not External, no controller route, or provider down. The turn event stream is fine in External mode; assistant text arrives whole after verification. [light integration](../../integration/guides/light-integration.md), [light-external](light-external.md). |
| `permission_denied` with egress details on the first External turn | Egress policy denies category `user_message` (or related) at class `internal`. Chat text always leaves as `internal` (`internal/dialogue/extmode`). Allow it at deployment/application/tenant/workspace. |
| `resource_exhausted` reason `no_budget` | No provider budget, or it is exhausted. Configure a budget before the first billed turn. |
| Dictation `validation_failed` | Body is not WAV PCM s16le mono 16 kHz, or length/energy outside limits ([ADR 0039](../../decisions/0039-dictation-without-vad.md)). |
| `/readyz` 503 with no local models in light External | No external controller cover: set `routing.controller` and External mode for tenants that chat ([light-external](light-external.md)). |

(Table from `docs/operations/key-provisioning.md`, section 3, and `schemas/api/openapi.yaml`.)

## 4. Packages and audits

| Message | Cause and fix |
| --- | --- |
| `package install` exits 1 with findings and nothing installed | The package is unsigned, tampered, signed by a key the trust store does not trust (`alixai trust add-key -app <app>`) or has a critical finding. Read the findings; `alixai package validate` shows the same. |
| `compat.core_out_of_range` | The installed core version is outside the package's `compatibility.alixai_core` range. The finding names the core version and `[min, max_exclusive)`. Shipped examples use `[1.0.0, 2.0.0)`; a source build reports `1.1.0-dev`. |
| `audit run`: `no signing key: pass -signing-key KEYID=KEYFILE or set integration_audit.signing_key_id and signing_key_file` | The audit signer is not provisioned. [Operator commands](operator-commands.md), section 2. |
| `audit run`: `the signing key is a published TEST-ONLY key that anyone can sign with` | Generate your own (`alixai package keygen`). |
| `audit accept-warning`: `the operator is not authorised to accept audit warnings` | The operator is not in `integration_audit.accept_operators`, or the list is empty (the default). A critical finding can never be accepted. |
| `identity add-issuer-key` refuses | The key id or issuer is not what the package declares in `context_exchange`, the key id is already used, the key was revoked, or the value is not a raw 32-byte Ed25519 public key (`docs/operations/key-provisioning.md`, section 1.4). |
| `trust add-key` or `identity add-issuer-key` refuses a key as `TEST-ONLY` | A published developer key. Use your own. `-allow-test-keys` / `-allow-test-key` exist for tests only. |
| `keys init`: `a different key is registered under this id, or this key was revoked; choose another -key-id` | The id is taken by another key (after a restore into an empty directory, for example). `alixai keys init -key-id audit-checkpoint-2`, then set `keys.checkpoint_key_file`. |

## 5. Backup, restore and upgrade

| Message | Cause and fix |
| --- | --- |
| `conflict: restore: the database looks in use (alixai.db-shm exists); stop the service, or pass force if it crashed` | The service is running, or crashed or was killed hard. Stop it; after a crash start it once and stop it cleanly, or pass `-force` when nothing has the database open. Run in M5-13. |
| `backup verify: no public key to verify with: pass -pubkey ID=FILE (the backup's .pub file)` | `<data_dir>/keys/backup.pub` is gone. Pass the copy you kept off the machine. Run in M5-13. |
| `backup verify` or `restore` fails on the signature, a file or the database | The backup is damaged or was altered. Use an earlier one. Nothing was replaced. |
| `there is no database at <path>` | Wrong `-config` or `data_dir`. |
| `upgrade apply`: `release X is not newer than the installed Y` | A downgrade or a replay. Use `rollback` to go back. |
| `upgrade apply` or `check`: a signature or key error | The manifest is unsigned or unknown, or the key is revoked or expired; or the release key is not registered (`alixai trust add-key -app alixai.platform`) or given with `-pubkey`. |
| `upgrade apply`: the runtime is not qualified | The release does not qualify that LocalAI version, backend or model (SEC-072). Nothing changed. |
| `an earlier upgrade did not finish (phase ...); run recover first` | `alixai upgrade recover`, then retry ([upgrades](upgrades.md), section 3). |
| `the pre-upgrade backup ... is gone; restore a backup of your own with 'alixai backup restore'` | `rollback` has nothing to restore. Restore your own backup. |

## 6. Windows

| Symptom | Cause and fix |
| --- | --- |
| `alixai service run` refuses to start from a console, or with a relative config | It is for the service control manager and needs an absolute `-config` path. Use `alixai service install` and `Start-Service AlixAI`, or `alixai serve` for a console run. |
| `service install` refuses an account | LocalSystem and named users are refused; the default is `NT SERVICE\AlixAI`, or `NT AUTHORITY\LocalService` with `-account`. |
| A relative `data_dir` lands in `C:\Windows\System32` | A service starts there. Use an absolute path (`deploy/native/windows/alixai.yaml`). |
| The key or data directory is readable by `Users` | File modes are advisory on Windows; apply the `icacls` lines of `deploy/native/README.md`. |
| A `-shm` file is left after stopping | The process was killed hard. See section 5. |

Still stuck: the diagnostics bundle, the log, `alixai version` and the configuration (without secrets) are what a bug report needs. A suspected vulnerability is not a bug report: see
[SECURITY.md](../../../SECURITY.md) and the [vulnerability response policy](../../policies/vulnerability-response.md).
