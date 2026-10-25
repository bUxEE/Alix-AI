# Migration and support policy

Status: **written in task M6-02**, 2026-10-08. Release criterion 11 ([release-gates.md](../release-gates.md)): public API and package migrations are documented and tested. Extends the signed-off values of [compatibility.md](compatibility.md) section 6 for stable releases.

This policy says how long each major version is supported, how application packages declare and migrate compatibility ranges, how database migrations are versioned and rolled back, and the rule that replacing the model never requires an integration change.

## 1. Support windows for stable releases

Every support window and notice period in this document was **signed off by the owner on 2026-10-08**, like the numbers in [compatibility.md](compatibility.md) section 6. The dates in the examples are illustrative only; no release date is decided. The table below applies to `1.0.0` and all later releases.

| Release line | Latest release (`X.Y.Z`) | Previous minor (`X.Y-1.Z`) | Previous major (`X-1.Y.Z`) |
| --- | --- | --- | --- |
| **Supported fixes** | defect and security | security only, for 90 days after the next minor ships | security only, for 12 months after the next major's `.0` |
| **Support ends** | never — always supported | 90 days after the next minor | 12 months after the next major |

Examples (illustrative dates only; no release date is decided):
- `1.0.0` ships on 2026-11-01. `1.0.1` ships on 2026-11-15: `1.0.0` ends support 90 days later, on 2027-02-13.
- `1.1.0` ships on 2027-03-01: `1.0.z` security fixes stop; any `1.0.z` already released get a security patch on 2027-05-30.
- `2.0.0` ships on 2028-06-01: `1.y.z` security fixes stop; any `1.y.z` get a security patch on 2029-06-01.

A release outside its window gets **no fix**. To stay supported, operators upgrade within the window: to the latest of the current line to stay current, or to the next minor (or major) before the current one ends support.

Read [vulnerability response](vulnerability-response.md) for the process and advisories.

## 2. Application package compatibility declaration and migration

Every application package states which AlixAI core versions it works with, using the `compatibility.alixai_core` range in the manifest (`alixai.yaml`). This is how operators know whether a package can run on their core version.

### 2.1 Declaring compatibility

```yaml
compatibility:
  alixai_core:
    min: 1.0.0           # inclusive: works with this version and later
    max_exclusive: 2.0.0 # exclusive: does NOT work with this version or later
```

A package with `min: 1.0.0, max_exclusive: 2.0.0` runs on `1.0.0`, `1.0.1`, `1.1.0`, `1.5.3` but not on `2.0.0`.

**Both bounds are required.** A package never claims compatibility with every future core. If a package uses something introduced in `1.2.0`, the author sets `min: 1.2.0`. If it is not tested on the new `1.3.0`, the author does not change `max_exclusive`.

### 2.2 Core rejecting an incompatible package

When a package is installed or activated, the core checks the range:

- Core `X.Y.Z` < package `min`: **rejected.** The core is too old; the operator must upgrade the core.
- Core `X.Y.Z` >= package `max_exclusive`: **rejected.** The core is too new; the package must be updated by the publisher or the operator must downgrade the core (within the [support window](#1-support-windows-for-stable-releases)).
- Core `X.Y.Z` within the range: **accepted.**

### 2.3 When a package needs an update

**A package is re-audit triggered when:**

1. the package declares a new or changed capability, RAG source, endpoint, contract version, or risk level;
2. a new core minor (e.g. `1.1.0` → `1.2.0`) introduces a capability or contract the package now uses; or
3. the operator updates the core to a new major (e.g. `1.y.z` → `2.0.0`).

The package publisher:

1. **Tests the package on the new core.** Run the integration audit against the new core version.
2. **Updates the manifest** if the package uses new features: set `min` to at least the new core's `X.Y` or `X.Y.Z`.
3. **Increments `package.version`** (semver) and the `version` of each changed capability or contract.
4. **Generates a new signature** of the updated files.
5. **Re-submits for audit** with the core version specified: `alixai package validate -core-version <new-core-version> <package>`.

Once the audit passes, the updated package is ready to install on the new core.

## 3. Database migrations: versioning and rollback

Database migrations live in `internal/<module>/migrations/NNNN_name.sql` (e.g. `internal/store/migrations/0001_init.sql`, `internal/dialogue/migrations/0001_transcripts.sql`). They are forward-only: a new release applies new migrations, and an older release never touches them.

### 3.1 Migration numbering

- Each module has its own migration sequence, starting from `0001`.
- Migrations are contiguous: `0001`, `0002`, `0003`, never `0001`, `0003`, `0005`.
- Applied migrations are recorded in `schema_migrations(module, version, checksum)` with the SHA-256 hash of the migration file. The checksum detects file tampering or corruption.
- A migration file must never be edited: to change a migration, author a new one (`0042_rename_column_a_to_b.sql`) that fixes or undoes the mistake.

### 3.2 How migrations apply

1. **At startup:** the new binary applies all pending migrations in order, each in its own database transaction.
2. **Idempotency:** every migration is safe to run twice (all-or-nothing: it either fully applies or is rolled back). A migration that says `CREATE TABLE ... IF NOT EXISTS` is idempotent; one that says `ALTER TABLE ... ADD COLUMN` without `IF NOT EXISTS` is not, and it will fail the second time with a database error, correctly.
3. **No downtime:** migrations must run while the old binary is still in use, or data and indices must be consistent when the new binary takes over. The upgrade procedure allows for this: `alixai upgrade apply` applies migrations with the new binary, so they are always forward-compatible.

### 3.3 Rolling back

The only supported way to go back is `alixai upgrade rollback`:

```bash
alixai upgrade rollback -config alixai.yaml
```

This restores the **backup the upgrade took** before it applied migrations, rolling back all changes at once. The old binary is then put back in place.

**What is NOT supported:** running an older binary over a migrated database. Migrations are forward-only and cumulative; the old binary does not know about new schema changes and will fail or corrupt data. Always use `rollback` to restore the pre-upgrade state.

**Going back manually is not supported:** there is no `0042_undo_rename_column_a_back_to_a.sql`. If a migration is wrong, it is fixed by a subsequent migration in the same release or in the next release, depending on severity.

### 3.4 Migration testing

Every migration is tested to ensure it applies cleanly from a clean schema and from every state the previous release could leave. A migration test:

1. starts with a database in the schema state of the previous release (a snapshot taken during that release's qualification);
2. applies the new migration;
3. verifies the schema and data are correct and consistent.

## 4. Model replacement and integration

The design states: **"Model replacement requires no host-application integration change."**

This means:

- Replacing or upgrading a qualified model (the controller, router, embedding, reranker, STT or TTS model) is a **deployment decision**, not an integration change.
- The core may change its qualified model set from release to release (documented in the release manifest).
- An application package never needs to be re-audited because the runtime model changed.
- A host application never needs to change its code, API calls, or package configuration when AlixAI selects a different model.
- The core is responsible for ensuring the new model is compatible with the capability contracts and RAG sources that applications declare.

**Consequence:** if an application's results depend on a specific model, the operator may need to update the core to keep using that model, but the application's own integration with AlixAI does not change.

## 5. Operator-facing upgrade flow

When an operator runs `alixai upgrade check` and `apply`:

1. **Compatibility check:** the core checks every installed application package against the release's core version (section 2.2). Incompatible packages block the upgrade.
2. **Database migration:** the new binary applies any pending migrations and probes the schema.
3. **Runtime and model qualification:** the operator specifies the LocalAI version and model set; the core verifies they are qualified for this release (see [upgrades guide](../operations/guides/upgrades.md)).
4. **Backup and restore:** on failure, the database and recorded runtime are restored to the pre-upgrade state.

Read [upgrades guide](../operations/guides/upgrades.md) for the full procedure.

## 6. No surprises

- **No automatic schema upgrades across majors:** when a major version breaks a database contract, the operator must run the upgrade command; the old binary is not put back.
- **No silent model downgrades:** if the release does not qualify a model the deployment uses, the operator is warned and must choose a qualified replacement before the upgrade applies.
- **No package activation without audit:** when an application is installed or activated, its package is checked against the core version (section 2.2) and the audit is re-run; it is quarantined until both pass.
- **Audit reports state the core version:** every signed audit report names the core version it was run on, so the operator and the package author know whether the package is still valid on a newer core.

## 7. Summary table

| Task | Who | When | How | Reference |
| --- | --- | --- | --- | --- |
| **Operator upgrades core** | Operator | before end of support window | `alixai upgrade check` then `apply`; packages are re-checked; DB migrations apply | [upgrades](../operations/guides/upgrades.md) |
| **Package publisher updates package** | Package author | when core has new capability or contract; when core major version ships | `alixai package validate -core-version X.Y.Z`; audit re-run; `package.version` incremented; signature regenerated | [application package](../integration/application-package.md) 2.3 |
| **Audit is re-run** | Operator or auditor | when package is installed, activated, or updated; when core is upgraded | `alixai package audit <package>`; report is signed and persisted | [operator commands](../operations/guides/operator-commands.md) |
| **Model is replaced** | Operator (with release notes) | per release or per deployment policy | no application package changes required; core qualifies the new model set | [release manifest](./release.md) |
| **Database is rolled back** | Operator (after a failed upgrade) | only with `alixai upgrade rollback` | restores pre-upgrade backup; old binary is put back | [upgrades](../operations/guides/upgrades.md) section 4 |

## 8. What exists today, and what does not yet

| Mechanism | State |
| --- | --- |
| Stable support windows for `1.0.0` and later | This document (milestone 6). The values are signed off by the owner (2026-10-08). |
| Application packages with `compatibility.alixai_core` range | Built and tested ([application-package.md](../integration/application-package.md) section 2.1); `alixai package validate -core-version` checks the range. |
| Database migrations in `internal/<module>/migrations/NNNN_name.sql`, per-module, forward-only, with checksums | Built and tested (`internal/store`, `internal/upgrade`, M5-04). |
| `alixai upgrade apply` applies migrations, probes the schema, and rolls back on failure | Built and tested (`internal/upgrade`, M5-13). |
| `alixai upgrade rollback` restores the pre-upgrade backup | Built and tested. |
| Model replacement without integration change | Qualified model set per release; release manifest pins the models; no app-package change required. |
| **Re-audit trigger on core upgrade and package update** | The rule is stated here; `internal/cli/cmd_package.go` checks the core version; audit state is persisted. |
| **Audit report is signed and names the core version run on** | The capability exists; audit output states the core version and signatures are verified at registration. The report lifecycle and re-audit triggers are in [operator commands](../operations/guides/operator-commands.md). |

## 9. Changing this policy

A change to this page follows GOVERNANCE: a pull request, reviewed in public, with an ADR when it:
- extends a support window,
- shortens the 90-day or 12-month windows,
- tightens the meaning of "model replacement requires no integration change",
- or changes the boundaries in section 1.
