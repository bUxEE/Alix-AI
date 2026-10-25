# Operator commands

Status: written in task M5-13, 2026-10-03. Code: `internal/cli/cmd_*.go`; every command answers `alixai <command> -h`.

`alixai` is the only operator tool. Every command that touches a deployment takes `-config FILE` (default: the `ALIXAI_CONFIG` environment variable; empty
means built-in defaults plus environment) and works on the **data directory** that configuration names (`data_dir`). Most commands print one JSON document on
stdout and put notes and warnings on stderr. Exit status: `0` the command ran, `1` it ran and failed or refused, `2` the command line was wrong
(`internal/cli/cli.go`). The text of messages on stderr is not a contract; the JSON members and the exit status are (see
[compatibility](../../policies/compatibility.md)).

## 1. Who the operator is

There is no operator login. The boundary is the operating system:

- **OS access to the data directory is the trust boundary.** Whoever can read and write `<data_dir>` holds the database, the key files in `keys/` and the credential of
  the running server's admin interface (`admin-endpoint.json`). That person is the operator, and nothing else is needed to be one (SEC-081, SEC-093;
  `internal/gateway/routes_admin.go`). The data directory and the database are created owner-only (0700 and 0600, `internal/store/store.go`). On Windows file modes are advisory: restrict
  the directory to the service identity and Administrators yourself (`deploy/native/README.md`, the `icacls` lines).
- **`-operator ID` is self-asserted.** It is a free string of 1 to 128 letters, digits and `._@:-`, typed by whoever runs the command. Most commands only **record** it
  in the audit log (`package install`, `trust add-key`, `trust revoke-key`, `audit run`, `audit quarantine`); `identity` commands take no operator name.
- **One command checks it:** `alixai audit accept-warning` needs `-operator`, `-reason` and `-finding`, and refuses an operator who is not listed in
  `integration_audit.accept_operators`. **The list is empty by default, so nobody may accept a warning until you name someone** (deny by default). A critical finding
  can never be accepted. Accepting a warning never widens the tools or the scope the model receives
  (`internal/cli/cmd_audit.go`; `TestAuditCommandsEndToEnd` in `internal/cli/cmd_audit_test.go`, `tests/e2e/m1/activation_test.go`).

This is a record of who decided, not a proof of it. List only people who already have access to the data directory, give them separate OS accounts if you
need to tell them apart, and treat the audit log as the account of what they asserted.

Run the commands as the identity that owns the data directory, or as one that may write it, and on the same host: `audit run` and `audit quarantine` find the running
server through `<data_dir>/admin-endpoint.json` to refresh or evict an application at once, and a file the command creates must be readable by the service. For a
packaged install (container stack, systemd `DynamicUser`) **no earlier task ran an operator command against the service's data directory**; see the
[server guide](server.md), section 7.

## 2. The commands

| Command | What it does | Defined in | State |
| --- | --- | --- | --- |
| `alixai version` | Prints the build version, commit and Go version. | `cmd_version.go` | Run (this task, by hand) |
| `alixai config check -config F` | Loads and validates the configuration, then exits. Does not read TLS or key files. | `cmd_config_check.go` | Run (this task, by hand) |
| `alixai serve -config F` | Runs the service: store, model preflight, providers, gateway. | `cmd_serve.go` | Run (`cmd_serve_test.go`; this task, by hand) |
| `alixai service install \| uninstall \| run` | The Windows service (virtual account `NT SERVICE\AlixAI`; refuses LocalSystem and named users). Windows only. | `cmd_service.go` | Static (`cmd_service_test.go`; not run as a service) |
| `alixai keys init [-key-id ID]` | Creates the platform checkpoint signing key under `<data_dir>/keys/` and registers its public half. | `cmd_keys.go` | Run (`cmd_keys_test.go`; this task, by hand) |
| `alixai package keygen -key-id ID [-out DIR]` | Generates a **development** Ed25519 key pair (`ID.key` 0600, `ID.pub`). Production publishers use their own key management. | `cmd_sign.go` | Run (this task, by hand) |
| `alixai package sign -app ID -version V -key KEYID=KEYFILE DIR` | Hashes and signs a package directory; repeat `-key` (at most 4) for a rotation overlap, `-append` to add to an existing signature. Redo after every change. | `cmd_sign.go` | Run (`cmd_sign_test.go`) |
| `alixai package validate [-trust-key KEYID=PUBKEY] [-core-version V] PKG` | Runs every check on a package (directory or `.tar.gz`) and prints the findings as JSON; exit 1 on a critical finding. | `cmd_package.go` | Run (`docs/integration/application-package.md` section 16; this task, by hand) |
| `alixai package install [-operator ID] PKG` | Verifies a package against the trust store and installs it **into quarantine**. It serves nothing until an audit activates it. | `cmd_install.go` | Run (`cmd_install_test.go`, `tests/e2e/m1`) |
| `alixai trust add-key -app ID -key-id KID (-public-key B64 \| -public-key-file F)` | Registers a publisher's (or, for `-app alixai.platform`, a platform) public key. | `cmd_trust.go` | Run (`cmd_trust_test.go`, `tests/e2e/m1`; this task, by hand) |
| `alixai trust revoke-key -app ID -key-id KID -reason T` | Revokes a key for good: nothing it signed verifies again, for any application that registered the same key material. | `cmd_trust.go` | Run (`cmd_trust_test.go`) |
| `alixai trust list -app ID` | Lists the keys of one application as JSON (`alixai.platform` for the platform keys). | `cmd_trust.go` | Run (`cmd_trust_test.go`; this task, by hand) |
| `alixai identity add-issuer-key -package P -key-id KID (-public-key B64 \| -public-key-file F)` | Registers the public key that verifies a host application's context tokens, checked against the package's `context_exchange` block. | `cmd_identity.go` | Run (`cmd_identity_test.go`, `tests/e2e/m1`) |
| `alixai identity list [-app ID] [-json]` | Lists issuer keys. | `cmd_identity.go` | Run (`cmd_identity_test.go`; this task, by hand) |
| `alixai identity revoke-issuer-key -app ID -key-id KID [-reason T]` | Tokens signed by the key are refused from the next request. The key id and material can never be registered again. | `cmd_identity.go` | Run (`cmd_identity_test.go`) |
| `alixai audit run [-package HASH] [-no-activate] [-signing-key KEYID=KEYFILE] APP` | Audits an installed package, signs the report and **activates the package only if the audit is clear**. | `cmd_audit.go` | Run (`cmd_audit_test.go`, `tests/e2e/m1`) |
| `alixai audit accept-warning -finding K -operator ID -reason T APP` | Records that an authorised operator accepted one warning of the latest report. | `cmd_audit.go` | Run (`cmd_audit_test.go`, `tests/e2e/m1/activation_test.go`) |
| `alixai audit quarantine -reason T [-operator ID] APP` | Takes the active package out of service at once and tells the running server to stop serving it. | `cmd_audit.go` | Run (`cmd_audit_test.go`, `tests/e2e/m1`) |
| `alixai audit verify-chain [-db FILE \| -data-dir DIR \| -config FILE] [-application ID] [-tenant ID \| -application-chain] [-require-anchor] [-expected-head FILE] [-limit N]` | Verifies every audit hash chain of a database file (or the ones selected), against its checkpoints, **without a running service and without writing the file**. An erased event is checked through its signed tombstone. Exit 1 when a chain fails, cannot be checked or none matches. | `cmd_audit_inspect.go` | Run (`cmd_audit_inspect_test.go`, `tests/dr`; M5-26 by hand on an empty database, where exit 1 "nothing was verified" is the designed answer) |
| `alixai audit list -application ID [-db FILE \| -data-dir DIR \| -config FILE] [-tenant ID] [-workspace ID] [-type NAME] [-after SEQ] [-limit N] [-data]` | Lists the audit events of one application from a database file, paged (at most 1000), scope-filtered; the payload only with `-data`. | `cmd_audit_inspect.go` | Run (`cmd_audit_inspect_test.go`, `tests/dr`; M5-26 by hand on an empty database) |
| `alixai receipts list -application ID [-db FILE \| -data-dir DIR \| -config FILE] [-tenant ID] [-workspace ID] [-preparation ID] [-status STATUS] [-after SEQ] [-limit N]` | Lists the action receipts of one application from a database file, by identifiers and status; never the receipt body. | `cmd_audit_inspect.go` | Run (`cmd_audit_inspect_test.go`, `tests/dr`; M5-26 by hand on an empty database) |
| `alixai index repair -index NAME [-config FILE \| -data-dir DIR]` | Repairs one derived index (`history_fts`, the RAG full-text indexes, `vector_rows`) outside an upgrade: a healthy one is left alone, a damaged one is isolated, rebuilt and checked. A vector index stays `pending` until `serve` re-embeds it. | `cmd_index_repair.go` | Run (`cmd_index_repair_test.go`, `tests/dr`; M5-26 by hand on an empty database: `healthy`) |
| `alixai retention run [-config FILE] [-application ID] [-dry-run]` | One sweep of the receipt and audit retention the active packages declare (what `serve` does daily): expired receipts are removed, expired audit payloads become signed tombstones. An application with no active package is skipped. [Production guide](../production.md), section 8. | `cmd_retention.go` | Run (`cmd_retention_test.go`; M5-26 by hand on an empty database: nothing to remove) |
| `alixai retention erase -application ID -tenant ID -reason TEXT -operator ID [-principal ID] [-dry-run] [-config FILE]` | Erases the audit payloads (signed tombstones), the finished preparations and receipts, and the conversation history (transcript, so history search finds none of it; the sessions' recent windows; the sessions, active ones ended first) of a tenant, or of one principal in it. History goes in transactions of at most 1000 rows, each recorded as `deletion.history_cleared`; `-dry-run` reports every count and writes nothing. Refuses, whole, while a pending or uncertain action depends on them, and without the platform signing key. Show afterwards with `audit verify-chain` that the chains verify. | `cmd_retention.go` | Run (`TestErasureRefusesEventsPendingActionsDependOn`, `TestErasureOfAPrincipalTakesWhatNamesThem`, `TestRetentionEraseOfAPrincipalTakesTheirHistory`, `TestRetentionEraseOfATenantTakesAllItsHistory`; M5-26 by hand with `-dry-run` on an empty database); not run against a deployed service |
| `alixai supervisor exec` | Runs LocalAI under the supervisor. **It is the command of the separate `localai.service` unit, never of the AlixAI service, and not an operator command.** [`docs/operations/localai-supervisor.md`](../localai-supervisor.md). | `cmd_supervisor.go` | Static (`cmd_supervisor_test.go`, against a stub runtime; not run against a real LocalAI) |
| `alixai backup create \| verify \| restore` | Signed backup of the database and file trees. [Backup and restore](backup-restore.md). | `cmd_backup.go` | Run (`TestBackupCLIRoundTrip`; this task, by hand) |
| `alixai upgrade check \| apply \| status \| recover \| rollback` | Staged upgrade with automatic rollback. [Upgrades](upgrades.md). | `cmd_upgrade.go` | Partly run: the CLI refuses an unsigned manifest and runs `status` and `recover`; the apply and rollback paths are tested in `internal/upgrade` |

The usage lines of `audit verify-chain`, `audit list`, `receipts list`, `index repair`, `retention run` and `retention erase` above are copied from `-h` of a binary built from this task's tree (`go build ./cmd/alixai`).
They read a database file by **copying it to a private temporary directory** and opening the copy with no migration (`store.Open` has no read-only mode); the service should be stopped, or the file a copy, because a copy taken while the
service writes is not a consistent snapshot (use `backup create` for a running service). `retention run` and `retention erase` write to the live database and take the platform signing key from `keys.checkpoint_key_file` (or `<data_dir>/keys/`), as `serve` does.

The audit report is signed by a **platform key** you provision once: `alixai package keygen -key-id alixai-audit-signer-2026 -out DIR`, then
`alixai trust add-key -app alixai.platform -key-id alixai-audit-signer-2026 -public-key-file DIR/alixai-audit-signer-2026.pub`, then name it in the configuration:

```yaml
integration_audit:
  signing_key_id: alixai-audit-signer-2026
  signing_key_file: /etc/alixai/audit-signer.pem   # PKCS#8 PEM, readable by the service identity only
  accept_operators: [alice]                        # who may accept warnings; empty means nobody
  max_report_age_hours: 24                         # how old a report may be when a package is activated on it
```

(`internal/cli/cmd_audit.go`; the same sequence is `newOperator` in `tests/e2e/m1/operator_test.go`.) A published TEST-ONLY key is refused as the signer.

## 3. Putting an application on the air

The order, each step a command of section 2. The keys are in section 4.

1. **Publisher side.** The application's developer writes the package and signs it (`package sign`), with a key whose id the package lists in
   `integrity.publisher_key_ids`. `alixai package validate` before and after. ([Package format](../../integration/application-package.md))
2. **Operator, once per deployment.** Configure `identity.deployment_id` (without it the `/v1` API refuses every request), then `keys init`, and provision the audit
   signer above.
3. **Operator, per application.** `trust add-key -app <app>` with the publisher's public key; `identity add-issuer-key -package <pkg>` with the key the host application
   uses to sign context tokens; `package install` (quarantine); `audit run <app>`. If the audit leaves warnings, a listed operator reviews each and runs
   `audit accept-warning`, then `audit run` again. A critical finding blocks activation; fix the package and install the new version.
4. **When the application, its API, its MCP server, its documentation or its permissions change:** a new package version, a new install and a new audit. An audit report older than
   `integration_audit.max_report_age_hours` is not activated on.
5. **To stop serving an application at once:** `audit quarantine`.

Provisioning detail, rotation and refusals: [`docs/operations/key-provisioning.md`](../key-provisioning.md).

## 4. Which key is which

| Key | Made by | Private half lives | Public half registered with | Used for |
| --- | --- | --- | --- | --- |
| Platform **checkpoint** key | `alixai keys init` | `<data_dir>/keys/audit-checkpoint.key` (0600) | the trust store, `alixai.platform`, automatically | Signs the audit chain-head checkpoints (SEC-086). `serve` signs with it; it is **not** in a backup. |
| **Audit report signer** | `alixai package keygen` (or your own KMS) | the file named by `integration_audit.signing_key_file` | `alixai trust add-key -app alixai.platform` | Signs integration-audit reports. |
| **Backup** key | automatically, on the first `alixai backup create` | `<data_dir>/keys/backup.key` (0600) | nothing: `backup.pub` is the verifier and you copy it **off the machine** | Signs backups. Not in a backup (a key inside the thing it authenticates authenticates nothing). |
| **Release** key | the owner, offline (`alixai package keygen -key-id alixai-release-2026`) | offline, never on the host or in CI | `alixai trust add-key -app alixai.platform`, or `-pubkey ID=FILE` on `upgrade` | Signs release manifests (`docs/operations/release.md`, section 2). Real key and custody: task M5-H02. |
| **Publisher** key (per application) | the application's developer | the developer | `alixai trust add-key -app <app>` | Signs that application's packages. |
| **Issuer** key (per application) | the host application | the host application; AlixAI holds only the public key | `alixai identity add-issuer-key` | Verifies the context tokens the host mints for its users. |

The published TEST-ONLY keys (`tools/devkeys`) are refused by every command that signs or trusts, unless a developer flag (`-allow-test-keys`, `-allow-test-key`,
`-allow-dev-key`) says otherwise. Never pass one in production.
