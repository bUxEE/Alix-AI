# Guide: upgrades

Status: written in task M5-13, 2026-10-03. Code: `internal/upgrade/`,
`internal/cli/cmd_upgrade.go` (task M5-04); the release side is `docs/operations/release.md` (task M5-03). Read the [guides index](README.md) first for the legend and the known gaps.

Two honest limits first.

- **No release exists yet.** The release workflow has never run, so there is no signed release manifest to upgrade to; the sections below describe the procedure that code and tests implement. The
  commands that take a manifest were exercised with test manifests, and by `TestVerifyManifestWrittenByReleaseTool` against a manifest `tools/release` wrote.
- **`alixai upgrade` upgrades the data, not the program.** It does not install a binary or an image and it does not start or stop anything (`internal/upgrade/doc.go`). You replace the program, and the
  command verifies the release, takes a backup, migrates the database, records the runtime and checks the result. The migrations come from the **new** binary: run `apply` with it.

## 1. What `upgrade apply` does

In this order, and it stops at the first refusal, before changing anything unless it says otherwise:

1. Verifies the signed release manifest: the signature by a **release key** (the `alixai-release-2026` key built into the binary, a key registered with `-purpose release`, or a `-pubkey` key;
   never the audit-checkpoint key or another platform key: [release keys](../release.md), section 2), the kind and application inside the signed part, and every pin the upgrade uses.
   Then it requires the release to be **this binary's**: its version is the binary's (`alixai version`), or the binary's own file is one of the release's `binary` artifacts by SHA-256. A binary
   cannot apply another release's manifest (`release X is not the release of this binary ...`): run `apply` with the binary of the release you apply.
2. **Refuses a release that is not newer** than the recorded one (`release X is not newer than the installed Y; ... use rollback to go back`). Without update metadata, a signed older manifest
   replayed is a downgrade attack (`docs/operations/release.md`, section 4).
3. With `-artifacts DIR`, requires that the files in that directory match the manifest's artifacts **exactly**.
4. **Qualifies the runtime** the upgrade would leave running (LocalAI and its backends) and the models in use against the release's pins, or refuses (SEC-072).
5. Rebuilds damaged derived indexes, then takes a **signed backup** of the state the upgrade starts from (`<data_dir>/upgrade/backups/<id>/`, [backup and restore](backup-restore.md)).
6. Applies the migrations (each its own transaction), records the new runtime set, and probes: the store opens, the indexes check out, the integrity check passes and the recorded runtime is the
   one asked for and is qualified.
7. **Any failure after the backup restores the database and the recorded runtime from it** and reports `rolled_back`. What was in place is kept in `<data_dir>/.pre-restore-*`.

Progress is journaled in `<data_dir>/upgrade/journal.json`, outside the database, because the database is what a rollback replaces. Two further things to know:

- **The runtime step is a record.** `-runtime-version` and `-backend NAME=VERSION` are checked against the release and written to `<data_dir>/upgrade/runtime.json`: *declared* state, not a probe of a
  running process. It does not change the LocalAI image or backends that run, and nothing in the shipped compose files, Quadlet units or native packages reads the file. You change the running
  runtime yourself (the image digest in your compose file or unit, or the configuration of `localai.service`, [`docs/operations/localai-supervisor.md`](../localai-supervisor.md)); AlixAI's own guards still apply at start (`localai.qualified_versions`, the model hash pins).
- **The probe does not start the service.** The command line supplies no probe beyond the database and the recorded runtime. After a successful `apply`, start the service and check `/healthz` and `/readyz`.

## 2. The procedure

Have a verified backup of your own before you start (`alixai backup create`, `alixai backup verify`, copied off the machine). `apply` takes one too, but it lives in the data directory it protects.

**On Windows, before the first upgrade to this version:** signing-key files are refused when other accounts can read them, on Windows as on Linux, and `apply` signs its backup with
`<data_dir>/keys/backup.key`. A data directory made by hand, or by an older version, inherits whatever its parent grants (`Authenticated Users` under a drive root), so its key files do too and
`apply` refuses before changing anything. Restrict the data directory to the account the service runs as first: `icacls <data_dir> /inheritance:r /grant:r "<account>:(OI)(CI)F" /T`, then
`/remove` any other account `icacls <data_dir>/keys/backup.key` still lists. `serve` names the same problem at start (`not owner only`).

1. **Get the release**: `release-manifest.json` and the files it lists, in one directory. The manifest is signed with the release key, offline, by its holder (`docs/operations/release.md`, section 3).
2. **The release key.** The `alixai-release-2026` public key is built into the binary: nothing to do for a release it signs. A release signed with another key needs that key registered as a
   release key once, `alixai trust add-key -app alixai.platform -key-id ID -public-key-file FILE -purpose release`, or `-pubkey ID=FILE` on each command. A platform key registered without
   `-purpose release` (for example `alixai-release-2026` registered by an older guide) is accepted when it is the built-in key's material; any other key without the purpose is refused
   (`key_wrong_purpose`): run the same `trust add-key` with `-purpose release` to classify it. The published TEST-ONLY key verifies only with `-allow-dev-key`: never in production.
3. **Check, changing nothing**, with the new binary:

   ```bash
   alixai upgrade check -config alixai.yaml -manifest release-manifest.json -artifacts ./release \
     -runtime-version <runtime version> -backend <name>=<version> -models <model id>,<model id>
   ```

   It prints the release's version, commit, creation time, signing key and counts, and whether the runtime set is qualified; it exits non-zero when it is not. Without `-runtime-version` it checks the
   runtime already recorded, and `apply` says on stderr that it qualifies nothing when none is recorded. Take the runtime version and the backend name and version from the pins of the release
   (`deploy/container/localai/config/backend-gallery.yaml` lists the backend pins that `tools/release` puts in the manifest).

   **What "qualified" means** (`internal/upgrade/release.go`, `Qualify`; `TestQualify`). The runtime version must be listed in `backend.allowed_runtime_versions` of **every model it is checked against**: the
   models you name with `-models`, else `inference.models`, else, when both are empty, every model of the release for that runtime. Case and a leading `v` do not matter. Every `-backend` must be one the
   release pins, by version or by digest. At the time of writing, four of the five entries of `models/registry` (the rejected controller candidates) list no runtime version and only the embedding model lists
   one, so **a runtime check with no models named, on a deployment whose `inference.models` is empty, is refused**: name the models your deployment really uses.
   Release files can also be checked with `go run ./tools/release verify -manifest release-manifest.json -pubkey alixai-release-2026=keys/alixai-release-2026.pub -dir .` (`docs/operations/release.md`, **Run** on the dev box).
4. **Stop the service.** `apply` refuses while `alixai.db-shm` exists beside the database (`the database looks in use`); after a crash, start the service once and stop it cleanly, or pass `-force` when
   you know nothing has the database open.
5. **Replace the program**: install the new binary over the old and keep the old one, or point the compose file or Quadlet unit at the new image digest. This step is yours.
6. **Apply**, with the new binary:

   ```bash
   alixai upgrade apply -config alixai.yaml -manifest release-manifest.json -artifacts ./release \
     -runtime-version <runtime version> -backend <name>=<version> -models <model id>,<model id>
   ```

   It prints JSON: `outcome` (`applied`, `refused`, `rolled_back`, `interrupted`, `rollback_failed`), `from`, `to`, `backup_id`, `backup_dir`, `derived_damaged`, `derived_rebuilt` and, when something failed,
   `cause`. The exit status is non-zero for anything but `applied`. `-key FILE` and `-key-id ID` name the key that signs the pre-upgrade backup (default `<data_dir>/keys/backup.key`, created on first use).
7. **Start the service** and check `/healthz`, `/readyz`, `alixai upgrade status` and a real request from a host application.
8. **If `apply` did not report `applied`:** the database and the recorded runtime are back where they were (`refused` changed nothing; `rolled_back` restored the backup). Put the **old** program back and start it.

**State of this procedure.** The library paths are tested (`TestUpgradeApplies`, `TestRollbackAfterFailedUpgrade`, `TestInterruptedUpgradeIsRecovered`, `TestInterruptedMigrationRecovers`, `TestVerifyRelease`,
`TestQualify` in `internal/upgrade`). Through the command line, the tests run `upgrade apply` only to a refusal of an unsigned manifest and run `upgrade status` and `upgrade recover` (`internal/cli/cmd_backup_test.go`); M5-13 ran `upgrade status`
by hand. **Nobody has run a full `apply` of a real signed release, replaced a real binary, or done any of it on a real host.** Rehearse on a copy first.

## 3. After a crash: `recover`

If the process died, the machine lost power, or `apply` returned `interrupted`, the journal names the step to redo. **Start with `alixai upgrade status`**: it shows the recorded release and runtime, an
unfinished upgrade and any isolated derived indexes, and changes nothing. Then, with the service stopped:

```bash
alixai upgrade recover -config alixai.yaml
```

An upgrade that stopped before its rollback began is rolled **forward** (every step is idempotent) and, if it cannot complete or fails its probe, rolled back to the backup; an interrupted rollback is finished. With no
journal it rebuilds any isolated index and reports `nothing_to_do`. `apply` and `rollback` refuse while a journal exists (`an earlier upgrade did not finish ... run recover first`).

## 4. Going back: `rollback`

```bash
alixai upgrade rollback -config alixai.yaml
```

With the service **stopped**. It restores the pre-upgrade backup of the last successful upgrade and the previous runtime set, exactly that backup and never "the newest one". **What was written since the upgrade
stays in `<data_dir>/.pre-restore-*` and is not merged back**; the command says so before it starts. Then put the previous program back. It refuses when no earlier version is recorded or when the pre-upgrade backup
is gone (`restore a backup of your own with 'alixai backup restore'`). Going back is always `rollback`, never an `apply` of an older release.

Like `backup restore`, `rollback` and `recover` keep the key revocations of the database they replace, and refuse when that database cannot be read. Over a database that is damaged as well, restore
the pre-upgrade backup yourself, `alixai backup restore -dir <data_dir>/upgrade/backups/<id> -pubkey backup=<data_dir>/keys/backup.pub -force -skip-newer-revocations` ([backup and
restore](backup-restore.md), section 4), and put the previous program back.

## 5. Before you upgrade: compatibility checks

- **Application packages.** A package declares the core versions it works with (`compatibility.alixai_core`, minimum inclusive, maximum exclusive); a core outside the range rejects it. Check each installed
  package against the new version first: `alixai package validate -core-version <new version> <package>` (`docs/integration/application-package.md`, section 16).
- **SDKs and CopilotKit.** The version matrix of `sdk/copilotkit-adapter/compat/matrix.json` and the SDKs' supported core range are in the [compatibility policy](../../policies/compatibility.md).
- **Read the release notes** for deprecations and migration notes: a contract changes in a major release, announced a release ahead ([compatibility policy](../../policies/compatibility.md)).
- **Models and the runtime.** The release lists the models and backends it was qualified with; `upgrade check` refuses a runtime or a model it does not qualify.
