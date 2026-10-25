# Guide: backup and restore

Status: written in task M5-13, 2026-10-03. Code: `internal/backup/`, `internal/cli/cmd_backup.go`
(task M5-04). Read the [guides index](README.md) first for the legend and the known gaps.

A backup is **one signed directory** you can copy anywhere and verify with a public key. AlixAI makes it while the service runs, never uploads it and never schedules it: where backups
go, how often and for how long they are kept are the operator's decisions. Remote backup is outside the core.

## 1. What a backup holds

`<dir>/<backup id>/` (the id looks like `bkp_01M417...`), written as `<id>.partial` and renamed only when complete, so an interrupted backup never looks like a finished one:

| File | What it is |
| --- | --- |
| `alixai.db` | The database, copied with `VACUUM INTO`: a consistent snapshot taken while the service keeps serving, integrity-checked. |
| `tree/<name>/...` | The file trees that hang off the database, copied **after** it so every blob the snapshot's rows name is there. Default trees: `files` and `packages`. |
| `manifest.json` | The signed statement: every file with its SHA-256 and size, the tree names, the release version. Ed25519 signature through `internal/trust`, ten years' validity. |

The database carries receipts, the audit log, sessions, settings, the trust store (public keys), installed package records and the derived indexes (full text, vectors). **Receipts and the
audit log are not rebuildable and are in every backup.** Indexes travel too but can be rebuilt from their sources.

**Not in a backup**, and so yours to keep elsewhere:

| Not included | Why and what to do |
| --- | --- |
| **Signing keys**: the backup key and the checkpoint key (`<data_dir>/keys/`) | A key inside the thing it authenticates authenticates nothing. Keep the checkpoint key offline and separately from the database; copy `backup.pub` **off the machine** (below). |
| The configuration file, the LocalAI key | Keep them in your own configuration management. |
| Models and backends | They are pinned by hash and provisioned from the host; fetch them again. |
| Bytes of deleted rows | `VACUUM INTO` writes live pages only, so a restore also compacts the database. |

(`internal/backup/doc.go`.)

## 2. Make a backup

```bash
alixai backup create -config alixai.yaml -dir ./backups
```

**Run:** `TestBackupCLIRoundTrip` (`internal/cli/cmd_backup_test.go`), `TestBackupWhileServing` (`internal/backup/backup_test.go`), and by hand in M5-13 against a running service. It prints the
backup id, directory, file count, trees and key id, and `lost_blobs` (with a warning on stderr) when the data directory had already lost stored files that live records name: the backup holds it as it is. Flags: `-trees files,packages` (the default; an empty value is the database only), `-key FILE` and `-key-id ID` for a signing key of your
own.

**The first run creates the backup signing key** at `<data_dir>/keys/backup.key` (0600) and `backup.pub`, and says so on stderr. **Copy `backup.pub` to a place that is not this machine and keep
it with the backups**: restoring after the data directory is lost needs it, and a public key stored beside the backup it verifies proves nothing if both are replaced.

The backup lands in a new subdirectory of `-dir` each time. There is no prune command: after you have verified a newer one, delete old backup directories yourself. Then copy the backup
directory off the machine with your own tools; the manifest is signed, so the copy is verifiable wherever it is. Schedule the command with whatever scheduler the machine has.

Opening the database for a backup applies **no migration**, so a backup is of the state the files are in, whichever binary takes it.

## 3. Verify a backup

```bash
alixai backup verify -dir ./backups/bkp_01M417... -pubkey backup=/safe/place/backup.pub
```

**Run** (same tests; M5-13). It checks the signature, that no file is missing, modified or unlisted, that no listed path escapes the backup, and the integrity of the database, and changes nothing. It prints
`"verified": true`. Without `-pubkey` it uses `<data_dir>/keys/backup.pub`, and fails with `no public key to verify with` when that file is gone. `-allow-dev-key` accepts the published
TEST-ONLY key and exists for tests. **Verify backups on a schedule too**: an unverified backup is a hope.

## 4. Restore

Restore **replaces** the live database and the listed trees, so:

1. **Stop the service.** Restore refuses while `alixai.db-shm` exists beside the database (`conflict: restore: the database looks in use (alixai.db-shm exists); stop the service, or pass force if it crashed`). After a
   crash or a hard kill that file is left behind: start the service once and stop it cleanly so SQLite removes it, or pass `-force`, only when you know nothing has the database open.
2. Restore. Verification happens first, and nothing is replaced unless it passes:

   ```bash
   alixai backup restore -config alixai.yaml -dir ./backups/bkp_01M417... -pubkey backup=/safe/place/backup.pub
   ```

   **Run:** `TestBackupRestoreRoundTrip` and `TestRestoreRejectsTamperedBackup` (`internal/backup/backup_test.go`), and by hand in M5-13 (the `-shm` refusal, `-force`, and a restore into an empty data directory).
3. Start the service. The binary applies any migration the restored database is behind on, as on any start.

What was in place is **not deleted**: the live database (with its `-wal` and `-shm`, which must never meet a different main file) and the listed trees are moved, as a unit, into
`<data_dir>/.pre-restore-<id>-<time>`, and the command prints that path as `pre_restore_dir`. That directory is how audit records and receipts written **after** the backup survive a restore; a failure while
swapping puts the previous state back. Keep it until you have decided what to do with those records, then remove it yourself. `-data-dir DIR` restores into a directory other than the configured one.

**Key revocations are kept.** A backup holds the trust store and the issuer keys as they were when it was taken, so restoring an older one would make a publisher or issuer key that was revoked
since (a compromised key) valid again. Restore therefore reads the revocations of the database it replaces and applies them to the restored one before the swap: those keys stay revoked, and a key
registered and revoked after the backup is kept on record as revoked. The output says how many (`carried_revocations`). When the database in place cannot be read (it is the damaged file you are
restoring over) the restore refuses rather than lose them; pass `-skip-newer-revocations` then, and revoke again by hand (`alixai trust revoke-key`, `alixai identity revoke-issuer-key`) what you revoked
after the backup. **Run:** `TestRestoreKeepsNewerRevocations` (`internal/backup`).

The backup directory, the staging directory and a data directory the restore creates are owner only, on Windows too: a protected ACL that grants the account running the command (and
LocalSystem and Administrators) and nothing inherited from the parent directory.

## 5. Disaster recovery: the data directory is gone

You need, from outside the machine: the latest backup directory, **`backup.pub`**, the configuration, the LocalAI key, the models and the backend, and the checkpoint key if you kept it.

1. Install AlixAI and put the configuration back ([server guide](server.md)); do not start it yet.
2. Restore into the empty data directory, with the public key you kept:

   ```bash
   alixai backup restore -config alixai.yaml -dir ./backups/bkp_01M417... -pubkey backup=/safe/place/backup.pub
   ```

   **Run** in M5-13 against an empty directory (`pre_restore_dir` is empty because nothing was in place). The restored database brings the trust store, the issuer keys, the installed packages,
   the audit log and the receipts.
3. **The checkpoint key.** If you kept the key file, put it back at `<data_dir>/keys/audit-checkpoint.key` (mode 0600, directory 0700): `serve` finds it there, and the restored trust store already has its public
   half (`alixai keys init` would only be needed to register it in a database that lacks it, and never overwrites). If you did not, the audit log is **not broken**: the existing checkpoints stay verifiable from the public key in the restored trust store. Make a new key under a **new id**, because the old
   id is registered with the old key and `keys init` refuses to reuse it (`choose another -key-id`):

   ```bash
   alixai keys init -config alixai.yaml -key-id audit-checkpoint-2
   ```

   then set `keys.checkpoint_key_file` to `<data_dir>/keys/audit-checkpoint-2.key` ([key provisioning](../key-provisioning.md), section 2.3). **Run** in M5-13: the default id conflicted, the new id registered
   beside the old.
4. Start the service and check `/healthz`, `/readyz`, `alixai identity list` and `alixai trust list -app alixai.platform`.
5. The next `alixai backup create` makes a **new backup key** if `keys/backup.key` is gone. Backups made earlier still verify with the old `backup.pub`; keep both.

Practise this on a scratch machine before you need it. A restore you have never run is not a recovery plan; the reference disaster-recovery evidence is a milestone 6 deliverable and does not exist yet.

## 6. Corrupt derived indexes

The full-text and vector indexes are derived. A damaged one is repaired from its sources in either of two ways, and `upgrade recover` is neither:

- **`alixai index repair -index NAME`** repairs one index, with no release applied. A healthy index is left alone (`healthy`); a damaged one is isolated, rebuilt and checked (`rebuilt`). A **vector** index cannot be rebuilt by it (it is the
  output of the embedding model): it stays isolated and reported `pending` until `alixai serve` re-embeds it. **Run:** `TestDROperatorCommandsInspectAndRepair` (`tests/dr`) and `cmd_index_repair_test.go`; [production guide](../production.md), runbook C.
- **`alixai upgrade apply`** repairs every damaged index before it takes its backup and applies the release ([upgrades](upgrades.md)). **Run:** `TestDRCorruptIndexRebuild`.

**`alixai upgrade recover` finishes an interrupted upgrade and nothing else.** With nothing isolated it answers `nothing_to_do`, even when an index is damaged, and `alixai upgrade status` lists only what is already isolated: neither finds fresh damage.
Receipts and the audit log are never part of a repair, and a repair leaves them as they were ([upgrades](upgrades.md)).
