# AlixAI native service packages (M5-02)

The `alixai` binary as a service of the host, without a container runtime ([ADR 0012](../../docs/decisions/0012-lite-install-native-vs-container.md)
says which install shape is preferred and why; this directory is the other one, and it is always shippable). The LocalAI binary is not part of
these packages: run it as a container published on `127.0.0.1:8080` ([`../container/localai`](../container/localai)) or, on Linux, as the
separate unit `systemd/localai.service` (M5-21), which runs it under the exec supervisor with an identity of its own. AlixAI never shares one
with it; `alixai serve` only attaches to it ([the supervisor](../../docs/operations/localai-supervisor.md)).

| Path | What it is |
| --- | --- |
| [`systemd/alixai.service`](systemd/alixai.service) | Hardened unit: `DynamicUser`, `ProtectSystem=strict`, `PrivateTmp`, `NoNewPrivileges`, no capability, loopback-only IP filter, cgroup caps, key through `LoadCredential`. systemd 247 or later. |
| [`systemd/localai.service`](systemd/localai.service) | The separate LocalAI unit (M5-21): a `DynamicUser` of its own, no capability, loopback-only IP filter, `MemoryMax`, `MemorySwapMax=0`, `Restart=on-failure`; runs `alixai supervisor exec`. Linux only. |
| [`launchd/ai.alixai.service.plist`](launchd/ai.alixai.service.plist) | System LaunchDaemon under the hidden account `_alixai`, umask 0077, restart on failure. |
| `alixai service install \| uninstall \| run` | Windows service (`internal/cli/cmd_service.go`): per-service virtual account `NT SERVICE\AlixAI`, delayed automatic start, restart on failure, logs to the Application event log. |
| [`systemd/localai.yaml`](systemd/localai.yaml) | The configuration of that unit: the LocalAI binary to spawn, its explicit environment, the key by reference. |
| `*/alixai.yaml` | The configuration of each platform: loopback listener, absolute data directory, key by file reference, inference path off until a model is provisioned. |
| [`../../.goreleaser.yaml`](../../.goreleaser.yaml) | The four release targets (linux amd64 and arm64, windows amd64, darwin arm64), static, `-trimpath`. Every archive carries `alixai` and `alixai-parser` side by side at its root, and this directory. |

## The document parser worker

RAG file ingestion needs a second executable, `alixai-parser` (`cmd/alixai-parser`, [ADR 0014](../../docs/decisions/0014-parser-sandbox.md)). `alixai serve` starts it once per
document, with an empty environment and no filesystem path (the document arrives on stdin), and the worker confines itself: a Windows Job Object; on Linux `no_new_privs`, rlimits,
Landlock and seccomp; fail closed. Every release archive carries it **next to `alixai`**, and the service looks for it in **the directory of its own executable**, so install the two
files into one directory:

| Platform | `alixai` | `alixai-parser` |
| --- | --- | --- |
| Linux | `/usr/local/bin/alixai` | `/usr/local/bin/alixai-parser` |
| macOS | `/usr/local/bin/alixai` | `/usr/local/bin/alixai-parser` |
| Windows | `C:\Program Files\AlixAI\alixai.exe` | `C:\Program Files\AlixAI\alixai-parser.exe` |

Nothing in the units, the plist or the shipped configurations changes for it. The worker is started by the service, not by the service manager, so the hardened unit of the parent
(no capability, `NoNewPrivileges`, the syscall deny list, the cgroup caps) is the one the worker runs in, and the worker asks for nothing more. Keep both files root-owned and not
writable by the service identity: the service runs whatever it finds there against untrusted documents. A different location is `rag_ingest.parser.worker_path` (an absolute path; a bare
name is refused). Without the worker the service still starts, logs `rag: ingestion starts without a ready parser worker` and every parse job fails closed.

The worker shares the unit's caps with the service: `MemoryMax=512M` and `TasksMax=256` cover both. One parse may use up to the worker's `memory_bytes` budget (512 MiB by default,
`internal/rag/parse`), so on these caps the cgroup limit, not the worker's own, is the first one a very large document could reach (not measured: the unit has not run on a host). Raise the unit's
`MemoryMax` in a drop-in (`systemctl edit alixai`) if you ingest large files; do not loosen the worker's limits.

Every platform keeps the core rules: the listener is loopback (the gateway refuses anything else without TLS, SEC-093), no key in a
config file or the environment, the data directory owner-only (SEC-081), the model directory read-only to the service.
**Written and checked statically on a Windows dev box; not run under systemd or launchd** (ADR 0012, "What was not measured").

## Linux (systemd)

```bash
sudo install -m 0755 alixai alixai-parser /usr/local/bin/         # the worker must sit beside the service binary
sudo install -d -m 0755 /etc/alixai /var/lib/alixai-models      # models: root-owned, read-only to the service
sudo install -m 0644 systemd/alixai.yaml /etc/alixai/alixai.yaml
openssl rand -hex 32 | sudo install -m 0600 /dev/stdin /etc/alixai/localai-api-key   # give LocalAI the same key
sudo install -m 0644 systemd/alixai.service /etc/systemd/system/alixai.service
sudo systemctl daemon-reload && sudo systemctl enable --now alixai
curl -fsS http://127.0.0.1:7470/healthz
systemd-analyze security alixai.service                          # the exposure score of the sandbox
```

LocalAI as the separate unit (skip it when LocalAI is a container on `127.0.0.1:8080`: `alixai.yaml` attaches to that one just the same):

```bash
sudo install -d -m 0755 /opt/localai                              # then put the pinned LocalAI binary there as local-ai, root-owned, mode 0755
sudo install -m 0644 systemd/localai.yaml /etc/alixai/localai.yaml
sudo install -m 0644 systemd/localai.service /etc/systemd/system/localai.service
sudo systemctl daemon-reload && sudo systemctl enable --now localai
systemd-analyze security localai.service
```

The backends are provisioned into `/var/lib/alixai-localai/backends` by the operator before the first start: the unit may not reach the network, so it
downloads nothing. The supervisor discards LocalAI's own output (it can print prompts at debug level); the unit's journal carries the supervisor's events.
How the supervisor behaves and what it settles when LocalAI dies: [`docs/operations/localai-supervisor.md`](../../docs/operations/localai-supervisor.md).

Memory is capped at 512 MB with no swap and one CPU, the caps of the container stack's `alixai` service. External provider mode needs
outbound traffic: widen `IPAddressDeny=` and `IPAddressAllow=` in a drop-in (`systemctl edit alixai`).

## macOS (launchd)

```bash
sudo dscl . -create /Groups/_alixai PrimaryGroupID 399
sudo dscl . -create /Users/_alixai UniqueID 399
sudo dscl . -create /Users/_alixai PrimaryGroupID 399
sudo dscl . -create /Users/_alixai UserShell /usr/bin/false
sudo dscl . -create /Users/_alixai NFSHomeDirectory /var/empty
sudo dscl . -create /Users/_alixai IsHidden 1
sudo install -m 0755 alixai alixai-parser /usr/local/bin/         # the worker must sit beside the service binary
sudo install -d -m 0700 -o _alixai -g _alixai /usr/local/var/lib/alixai /usr/local/var/log/alixai
sudo install -d -m 0755 /usr/local/etc/alixai /usr/local/var/lib/alixai-models
sudo install -m 0644 launchd/alixai.yaml /usr/local/etc/alixai/alixai.yaml
openssl rand -hex 32 | sudo install -m 0600 -o _alixai -g _alixai /dev/stdin /usr/local/etc/alixai/localai-api-key
sudo install -m 0644 -o root -g wheel launchd/ai.alixai.service.plist /Library/LaunchDaemons/ai.alixai.service.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/ai.alixai.service.plist
```

launchd has no memory or CPU cap and no sandbox profile for a daemon: the isolation is the account, the umask and the loopback listener.

## Windows

From an elevated PowerShell, in the extracted archive:

```powershell
New-Item -ItemType Directory -Force 'C:\Program Files\AlixAI', 'C:\ProgramData\AlixAI\data', 'C:\ProgramData\AlixAI\models' | Out-Null
Copy-Item alixai.exe, alixai-parser.exe 'C:\Program Files\AlixAI\'    # the worker must sit beside the service binary
Copy-Item deploy\native\windows\alixai.yaml 'C:\ProgramData\AlixAI\'
# the LocalAI key, one line; the ACL below makes it readable by the service and Administrators only
-join ((1..32) | ForEach-Object { '{0:x2}' -f (Get-Random -Max 256) }) | Set-Content -NoNewline 'C:\ProgramData\AlixAI\localai-api-key'
& 'C:\Program Files\AlixAI\alixai.exe' service install -config 'C:\ProgramData\AlixAI\alixai.yaml'

# SEC-081: the service identity exists once the service is registered. Cut the inherited access (ProgramData grants Users read),
# give the service read on the configuration, key and models, and write on the data directory only.
icacls 'C:\ProgramData\AlixAI' /inheritance:r /grant:r 'NT AUTHORITY\SYSTEM:(OI)(CI)F' 'BUILTIN\Administrators:(OI)(CI)F' 'NT SERVICE\AlixAI:(OI)(CI)R'
icacls 'C:\ProgramData\AlixAI\data' /grant:r 'NT SERVICE\AlixAI:(OI)(CI)M'
Start-Service AlixAI
Invoke-RestMethod http://127.0.0.1:7470/healthz
```

The service runs as the virtual account `NT SERVICE\AlixAI` (or `NT AUTHORITY\LocalService` with `-account`); `service install` refuses LocalSystem
and named users. Output goes to the Application event log, source `AlixAI`. `alixai service uninstall` removes the service and keeps the data.
A Windows service has no memory cap of its own; the working-set figure in ADR 0012 is what to expect, not a limit.
