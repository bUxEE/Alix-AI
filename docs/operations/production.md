# Production operations guide

Status: written in task M6-04, 2026-10-03; the inspection, index-repair and reconciliation commands of section 3 and runbooks B to D were added by M5-25. Evidence:
[`docs/evidence/m6-dr/report.md`](../evidence/m6-dr/report.md), written by the disaster-recovery drill (`tests/dr`).

This is the guide for the person who **runs** an AlixAI deployment after it is installed: how much it needs, what to watch, how to upgrade it, what to do when it breaks, how to rotate its keys and what happens to
its data over time. It does not repeat the installation and the command reference. Those are the [deployment guides](guides/README.md) (read its legend and its "known gaps" first); this guide links to them and adds
what only a running deployment needs. AlixAI is headless: everything below is a configuration file, a command line or an HTTP probe.

**How to read the states.** The guides label a command **Run**, **Static** or **Not run**. This guide adds **Drill**: run by `tests/dr` against the `alixai` binary built from the checkout, as a subprocess, against a throw-away
data directory, on the development machine (Windows 11), where it is **emulated**, never certified, and always with test keys. A step with no label is a procedure assembled from commands that are each labelled in the guide
they come from; nothing has run it as a whole.

**What is not true yet, and shapes this guide.**

- No release has been published ([guides index](guides/README.md), gap 1): there is no signed release to upgrade to, and the commands that take a manifest were exercised with a test-signed one.
- No controller model is activatable on a fresh install, so the service runs with the **inference path off** (`/readyz` answers 503 naming it). The drill ran in that state; no turn ran through the binary
  ([the report](../evidence/m6-dr/report.md), sections 1 and 6).
- Nothing was run on a real host, a packaged install or a Linux machine. The service was stopped with a hard kill (TerminateProcess), never a power cut.
- Several recoveries below need more commands than the product gives you today. They are called out as **gaps** where they matter, and listed together in section 10.

## 1. Before you go live

| Check | Why | Where |
| --- | --- | --- |
| The install shape is chosen and its hardening is in place. | The container stack is the preferred shape; a native service is supported. | [Server](guides/server.md), sections 1 to 4 |
| `identity.deployment_id` is set; issuer keys, the audit signer and the checkpoint key are provisioned, and **none is a TEST-ONLY key**. | Without them the `/v1` API refuses every request. | [Operator commands](guides/operator-commands.md), sections 3 and 4; [key provisioning](key-provisioning.md) |
| The checkpoint key and `backup.pub` are copied somewhere that is not the machine. | A restore after the data directory is lost needs both, and neither is in a backup. | [Backup and restore](guides/backup-restore.md), sections 1 and 5 |
| A backup is scheduled by you, verified from the **off-machine copy**, and a **restore has been rehearsed** on a scratch directory. | A backup you have not restored is a hope. | Section 4; Drill |
| Something reads the signals of section 3 and someone is told when they change. | AlixAI sends nothing anywhere. | Section 3 |
| The backups live on a different volume from the data directory. | The data directory's volume is the one the disk guard watches, and a full volume takes the backups with it. | Section 2.4 |
| Retention is decided and set: history, and the receipt and audit days your packages declare; you know what is erased on request and what never is. | Section 8. | Section 8 |
| You have rehearsed an upgrade on a copy and know how you go back. | An upgrade changes data the old program may not read. | Section 5 |
| Who is called, and where this guide and the evidence are kept. | Section 6. A suspected vulnerability is reported as [SECURITY.md](../../SECURITY.md) says. | Section 6 |

## 2. Capacity

### 2.1 What grows

| What | Where | Grows with | Shrinks how |
| --- | --- | --- | --- |
| The database, `alixai.db` and its `-wal` | `<data_dir>` | sessions, transcript turns and their full-text index, RAG metadata and indexes, preparations, receipts and audit events (removed or tombstoned only past the days a package declares, or on an erasure request; a tombstone is about 870 bytes and stays), the trust store | transcript, receipt and audit retention and erasure (section 8) free pages for reuse but **do not shrink the file**; only `backup restore` replaces it with a compact copy (a backup is written with `VACUUM INTO`); there is no compaction command |
| File trees | `<data_dir>/files`, `<data_dir>/packages` | imported files; installed package copies | file expiry and deletion, then a background compaction (section 8) |
| Backups | wherever you point `-dir`; each is a **full** copy of the database and both trees | every backup you keep | you delete the old directories; nothing prunes them |
| The pre-upgrade backup | `<data_dir>/upgrade/backups/<id>` | one full copy, kept for `upgrade rollback` | replaced by the next upgrade |
| `.pre-restore-*` directories | `<data_dir>` | one per `backup restore` that replaced something | you delete them, after deciding what to do with the records in them |
| Logs | stderr, journald, the container's log driver | traffic | the log system's rotation ([server guide](guides/server.md), section 8) |
| Models and the runtime | outside the data directory | the profile | [resource budgets](resource-budgets.md) |

So the data directory's volume must hold the data **plus one full copy for the pre-upgrade backup, plus whatever `.pre-restore-*` you keep, plus headroom**, and the backups need a volume of their own.

### 2.2 What was measured

All figures are from the drill, on the development machine, and **emulated**. They show the order of magnitude; do not extrapolate them to your volumes.

| Quantity | Measured | Source |
| --- | --- | --- |
| A fresh deployment's database | 640 KiB | report, section 2 |
| 20 000 transcript turns, with their history index | the database grows to 9.3 MiB, about **450 bytes per turn** | report, section 2 |
| A backup of that database and a 64 MiB file tree | 74 MiB, made in about 1 s while the service served | report, section 4 |
| `backup verify` of it | about 1 s | report, section 4 |
| `backup restore` of it, service stopped | about 1 s (0.85 to 1.4 s over the runs) | report, section 4 |
| The service from start to listening on a restored directory | under 150 ms; under 0.5 s when it also resolves an interrupted commit | report, section 4 |
| Rebuilding the history index of those 20 000 turns | about 0.1 s | report, section 4 |

A restore is fast; **your RTO is not the restore**. It is the time to notice, to decide, to fetch the backup and the keys from where you keep them, to restore, and to check. Rehearse it to learn yours (section 4).

### 2.3 Memory, CPU and latency

The profile files cap LocalAI and the AlixAI container; [resource budgets](resource-budgets.md) holds the arithmetic and the basis of every number. The ones a capacity plan needs:

- **One interactive turn at a time per deployment** on the Lite profile: a second request queues (resource budgets, R1 and section 7.4). A model-backed turn is budgeted at p95 35 s on Lite and 20 s on Balanced (section 7.2,
  proposals), so a Lite deployment completes on the order of one hundred model-backed turns an hour. That is arithmetic from the budgets, not a measurement of this release. A turn answered by the deterministic path
  is budgeted at p95 100 ms.
- The AlixAI container is capped at 512 MB and measured 96 to 160 MB. LocalAI's budget is 5.8 GiB on Lite and 12.1 GiB on Balanced, and the 4 GB profile is experimental.
- Background work (parsing, indexing, retention) yields to interactive work and runs one job at a time on the small profiles.
- Size for the busiest hour of the deployment, not the average, and add a second deployment rather than raising a profile's concurrency: the figures above are limits of the runtime, not of a setting.

### 2.4 Disk pressure

The service watches the volume of the data directory. By default ingestion **pauses** when less than 2 GiB is free, resumes at 3 GiB, and below 512 MiB the volume is critical and what is left belongs to the store:
commits, audit events and sessions ("pause ingestion before interactive or transactional storage is endangered"). The floors are `resources.disk_pause_free_bytes`, `disk_resume_free_bytes` and
`disk_reserve_bytes`. A volume whose free space cannot be read counts as under pressure. The service logs `disk pressure` at warn level when the level changes. **Unit-tested, not exercised by the drill.** Put backups and
logs on other volumes, and alert well above these floors (section 3).

### 2.5 Tenant quotas

One tenant's flood must not degrade the others (ADR 0018). `alixai serve` puts four per-tenant limits in front of the `/v1` API. A tenant is the pair **application and tenant** of the verified token, never anything the request
says: the same tenant id under two applications is two tenants, and every workspace of a tenant shares its quota.

| Dimension | Counted where | A refusal is |
| --- | --- | --- |
| `requests_per_minute` | every `/v1` request except the commit path below; one token each, refilled evenly, `burst` deep | `429 rate_limited`, retryable, with `retry_after_ms` and a `Retry-After` header in whole seconds |
| `concurrent_sessions` | one slot per session, taken when it is opened (`POST /v1/sessions`, its first turn, an AG-UI run), renewed by every later request of the session, freed by `DELETE /v1/sessions/{id}`, by a failed opening, or after `lease_ttl` (1 h) without a request | `503 resource_exhausted`, retryable, with `dimension`, `limit`, `used`, `requested` |
| `storage_bytes` | charged by the files service in the transaction that stores the file; `POST /v1/files` is only checked early against its declared length, and afterwards the gauge is set to what the files service measures, so a delete or an expiry lowers it again | `507 resource_exhausted` with the same details; nothing is stored and nothing is charged |
| `ingest_bytes_per_day` | per UTC day: a file stored as a `rag_source` (charged in the same transaction as its storage) and every source sync, for the bytes of the documents that become new versions (charged in the transaction that writes them) | `507 resource_exhausted` for an upload, with the same details and `resets_at` (the next 00:00 UTC), nothing stored; a sync ends as a failed job with the same code (it is not retried), and the next sync requested or scheduled after the window resets picks up the rest |

**The commit path is never refused for quota.** A confirmation, a rejection and the reads of an action's status and receipt (`/v1/actions/...`) and an AG-UI run that is a resume always reach their handlers, whatever the tenant has
used: preparing may be refused, committing what the user already confirmed is not ("commit response uncertain"). Those requests still spend a token when one is left, so the traffic is counted.

**Defaults are finite, per profile**, and the quota table follows the deployment `profile` unless `quotas.profile` names another one. Per tenant, each number a proposal the operator overrides, none certified (ADR 0018):

| Profile | requests / min | burst | concurrent sessions | ingest bytes / day | storage |
| --- | ---: | ---: | ---: | ---: | ---: |
| `experimental-4g` | 60 | 6 | 4 | 256 MiB | 2 GiB |
| `lite` (the default) | 120 | 12 | 8 | 1 GiB | 8 GiB |
| `balanced` | 300 | 30 | 32 | 2 GiB | 16 GiB |
| `performance` | 600 | 60 | 64 | 8 GiB | 64 GiB |

**How to raise them.** The `quotas` section of the configuration file. A field you leave out (or set to `0`) inherits, in this order: the tenant's override, the application's override, `defaults`, the profile. A value of
`-1` removes that one limit; there is no other way to get an unlimited one. A malformed value stops the start.

```yaml
quotas:
  profile: balanced              # omit to follow the deployment profile
  defaults:                      # every tenant that has no override
    requests_per_minute: 300
    storage_bytes: 34359738368   # 32 GiB
  overrides:
    - application: demo-library              # every tenant of this application
      limits: { concurrent_sessions: 64 }
    - application: demo-library
      tenant: ten_acme                       # one tenant; inherits from its application's override
      limits: { ingest_bytes_per_day: -1 }   # -1: explicitly unlimited
  lease_ttl: 1h
```

Size `tenants x quota` against the disk: the per-tenant defaults do not add up to a machine, and the disk guard of section 2.4 is the second line. Put a reverse proxy with its own limits in front for floods that never
reach a verified tenant (unauthenticated traffic is refused before any quota is spent).

**Where the charge is made (M9-01).** The two volumes are charged with `ChargeTx` inside the transaction that writes the data. `internal/files` and `internal/rag/ingest` each define a small charge hook (`files.Charger`,
`ingest.Charger`) and know nothing of `internal/quotas`; the composition root (`internal/cli/quotas_wire.go`) adapts the quotas to both. A refusal aborts the write, so a refused upload leaves no file, no blob and no charge,
and a failure after the charge takes it back with the row. A file that the RAG ingestion refuses after its transaction committed is withdrawn and its charges are given back. A sync charges only documents that are new or
changed in the source; an unchanged document and the rebuild of an index under a new parser or embedder version cost nothing. The batches of a sync that were written before a refusal stay (the next sync repeats the rest).
The HTTP boundary keeps the cheap early check and the reconciliation of the storage gauge; a files service built without the hook is still charged there by the reservation M5-28 used. The files service charges a
write of any origin (an attachment the core stores for a generated file is storage too), and no commit path stores a file, so a confirmed commit is still never refused. A write made outside the HTTP route does not reconcile
the gauge first, so a gauge that a delete or an expiry left high refuses it until the next upload through the route (or the reconciliation) sets it right. Model tokens per day, a per-class job cap and fair round-robin dequeue are not built. Buckets and session leases live in
memory: a restart refills every bucket and forgets every lease (the sessions died with the process); the daily and storage counters survive it. The counters are local; nothing is exported.

## 3. Monitoring

AlixAI pushes nothing. These are the signals, how to read them, and what to alert on. **Drill:** `TestDRMonitoringSignals` reads each of them from a running service.

```sh
curl -fsS http://127.0.0.1:7470/healthz
curl -sS http://127.0.0.1:7470/readyz
curl -sS http://127.0.0.1:9464/metrics
curl -sS -H "X-Alixai-Admin-Token: TOKEN" http://127.0.0.1:9464/diagnostics
```

The last two need `metrics.enabled: true`; `TOKEN` is the `token` member of `<data_dir>/admin-endpoint.json`, which changes at every start and is readable only by the identity that owns the data directory. Setup, the
container stack's missing scrape path and the host-visible `/v1/ops` routes are in the [server guide](guides/server.md), section 9.

| Signal | Alert when | Meaning and first action |
| --- | --- | --- |
| `/healthz` is not 200 | two consecutive probes fail | The process is not serving. Section 6, runbook A. |
| `/readyz` changes from its baseline | the set of failing checks changes | On a machine with no model the baseline is 503 naming `inference`. A new failing check (the store, ingestion, `runtime_supervisor`) is a degradation: [troubleshooting](guides/troubleshooting.md), section 2. |
| Log `interrupted commits resolved before serving` | always | The service was killed or crashed while committing. Read `examined`, `recorded`, `reopened` and `uncertain`; any `uncertain` is an incident (runbook B). |
| Log `disk pressure` | always | Section 2.4 and runbook G. |
| Log `audit checkpoints are not signed` | after the first day | The checkpoint key is missing or not usable: `alixai keys init` ([key provisioning](key-provisioning.md), section 2). |
| Any log record at level `ERROR` other than the baseline `the inference path is off` | always | Read it; [troubleshooting](guides/troubleshooting.md). |
| `alixai_queue_oldest_ready_age_seconds` | rising for ten minutes | Background work is held or stuck behind interactive work or disk pressure. |
| `alixai_resource_rss_bytes` against `alixai_resource_memory_budget_bytes` | above 90 percent | The governor will hold background work and unload models; add memory or lower the profile's load. |
| `alixai_resource_model_reloads_total` | any increase on ordinary turns | A resident model was evicted: a defect, not a degradation (resource budgets, R3). |
| `alixai_provider_status{status="unavailable"}` | 1 for a configured role | A model role is unavailable; the deterministic path still answers (runbook H). |
| `alixai_scrape_collector_up` | 0 | A collector failed; the bundle names it. |
| The age of the newest verified backup | older than your RPO | Section 4. This one is yours: AlixAI does not schedule or track backups. |
| Free space of the data directory and of the backup volume | below four times the floor of section 2.4 | Section 2. |

**Reading the audit log and the receipts without a running service.** The audit chains are hash-linked and checkpointed. Three commands read a database file (the data directory's `alixai.db`, a restored copy, the
`pre_restore_dir` a restore leaves, a backup) and never change it:

```sh
alixai audit verify-chain -config /etc/alixai/alixai.yaml
alixai audit verify-chain -db /srv/alixai/.pre-restore-DATE/alixai.db -application APP
alixai receipts list -config /etc/alixai/alixai.yaml -application APP -tenant TENANT
alixai audit list -config /etc/alixai/alixai.yaml -application APP -tenant TENANT -type action.committed
```

- `audit verify-chain` verifies every audit chain of the file, or the ones `-application` and `-tenant` select: every event's hash against its content, that positions are contiguous, that each event links to the one
  before it, and the newest signed checkpoint. It prints a report that carries no event data (events, head, whether the chain is anchored by a verified checkpoint, and each problem by kind and position, for example
  `hash_mismatch` at position 3), **exits non-zero when any chain fails** and when no chain matches, and with `-require-anchor` also fails a chain that no verified checkpoint covers. A checkpoint is verified against the
  trust store in the same file, so the file is believed only as far as its own keys are: keep the checkpoint key and a backup off the machine (section 4).
- `receipts list` and `audit list` print one page of an application's receipts or events, for `-application` (required: there is no listing of everything), narrowed by `-tenant`, `-workspace` and, for receipts,
  `-preparation` and `-status`, and for events `-type`. A page is at most 1000 rows (`-limit`, default 100) and says whether there are `more` and the `next_after` cursor. They print identifiers, status, capability and
  time; **never** the stored scope of a preparation, its confirmation binding, the prepared action or what the application returned in a receipt. `audit list -data` adds an event's payload, which the audit log redacted when it
  wrote it, redacted again and left out when it is large.
- They are read-only by construction: the file (and its `-wal`) is **copied** into a private temporary directory, opened there with no migration, and the copy is removed afterwards, so a read-only medium, a backup whose
  manifest names its hash and a file the operator must not touch are all safe to read. The cost is the disk space of one copy and the time to make it. Stop the service first, or point at a copy: a copy taken while the service
  writes is not a consistent snapshot (`backup create` is the command for a running service).

The operator's other evidence of integrity is unchanged: the signed checkpoints, a verified backup, and the `/v1/ops/audit/export` route a host application may call. **Drill:** the real commands, with the service stopped, verified
every chain of a deployment that had committed a booking (the same events and heads the drill reads in-process), listed its one receipt and its `action.committed` event without printing the confirmation secret, found a
copy whose last event was changed, and left the operator's file byte for byte as it was (`TestDROperatorCommandsInspectAndRepair`).

## 4. Routine operations

Backups, verification and restore are described once, in [backup and restore](guides/backup-restore.md). The routine that goes with them:

```sh
alixai backup create -config /etc/alixai/alixai.yaml -dir /srv/alixai-backups
alixai backup verify -config /etc/alixai/alixai.yaml -dir /srv/alixai-backups/BACKUP_ID -pubkey backup=/safe/place/backup.pub
```

| When | What | Notes |
| --- | --- | --- |
| Every backup interval (your RPO) | `backup create`, then `backup verify`, then copy the directory off the machine | **Drill.** Run it while the service serves: a consistent snapshot, about 1 s for 74 MiB. Schedule it with the machine's scheduler and alert when it fails. |
| Daily | Look at the signals of section 3; check free space | |
| Weekly | Verify the newest backup **from the off-machine copy**, with the off-machine `backup.pub` | `backup verify` fails with `no public key to verify with` when you rely on the copy that sits beside the backup. |
| Monthly, and after every upgrade | **Rehearse a restore** into a scratch directory and start a service on it | The procedure below. **Drill.** Record how long it took. |
| After a crash or a kill | Delete any `*.partial` directory in the backup directory | A killed backup leaves one; it has no manifest, does not verify and cannot be restored. **Drill.** |
| Per backup you delete | Keep at least one backup that predates each upgrade, and the program version that can read it | A restore returns the **data** to the state of the backup; the program is yours to keep. |
| Quarterly | Run the drill on the release you run (section 9) | |

**Restore rehearsal.** Restore the newest backup into a directory beside the live one, point a copy of the configuration at it, and start a service on another port:

```sh
alixai backup restore -config /etc/alixai/alixai.yaml -dir /srv/alixai-backups/BACKUP_ID -data-dir /srv/rehearsal/data -pubkey backup=/safe/place/backup.pub
alixai serve -config /srv/rehearsal/alixai.yaml
```

Then check what the drill checks: the service starts, its receipts are the backup's, `alixai trust list -app alixai.platform` shows the platform keys, and the checkpoint key you keep off the machine is the one the
trust store holds. Stop the rehearsal service with its own stop, delete the directory, and write down the time. **Drill:** `restore_into_an_empty_data_directory` does exactly this and compares a receipt byte for byte.

**RPO and RTO.** The recovery point of an in-place restore is the **last backup**: what came after is not in it. If the old database is still readable, `backup restore` keeps it whole in `<data_dir>/.pre-restore-*`
(and prints the path as `pre_restore_dir`), receipts and audit events included; if it is destroyed, those records are gone from AlixAI, and the application they were committed to still holds what they describe
(section 6, runbook D). Set your backup interval from the loss you can accept, and your RTO from a rehearsal.

## 5. Upgrades in production

The upgrade commands upgrade the **data**, not the program: you replace the binary or the image, and the command verifies the signed release, backs up, migrates, records the runtime and probes
([upgrades](guides/upgrades.md), sections 1 and 2, has the full account of each step and flag). What a production upgrade adds:

1. **Read what changes.** The release notes, the [compatibility policy](../policies/compatibility.md), and `alixai package validate -core-version` for every installed package.
2. **Take and verify a backup of your own**, and copy it off the machine. The command takes one too, but it lives in the directory it protects.
3. **Rehearse on a copy.** Restore that backup into a scratch directory (section 4), then run `alixai upgrade check` and `alixai upgrade apply` against the scratch configuration with the **new** binary. The migrations are the new
   binary's; a rehearsal is the only way to see them run on your data before they run on it.
4. **In the window:** stop the service, replace the program and keep the old one, apply, start, check `/readyz`, run a real request from a host application.

   ```sh
   alixai upgrade check -config /etc/alixai/alixai.yaml -manifest release-manifest.json -artifacts ./release
   alixai upgrade apply -config /etc/alixai/alixai.yaml -manifest release-manifest.json -artifacts ./release
   alixai upgrade status -config /etc/alixai/alixai.yaml
   ```

   **Drill:** `upgrade apply` of a test-signed manifest on a deployment with a receipt and 20 000 transcript turns reported `applied`, took about 1.5 s and left receipts, transcript and audit chains unchanged
   (`upgrade check`, `status` and the rollback path are labelled in the guide, not run by the drill).
5. **Decide from the outcome.** The command prints `outcome`:

   | Outcome | The state | You |
   | --- | --- | --- |
   | `applied` | New version in place. | Start the service and verify. Keep the old program for the rollback window. |
   | `refused` | Nothing changed. | Read `cause` (not newer, unqualified runtime, bad signature, the database in use) and fix it. |
   | `rolled_back` | The backup was restored; what was in place is in `.pre-restore-*`. | Put the **old** program back and start it. Keep the cause. |
   | `interrupted` | The process died mid-upgrade. | `alixai upgrade recover` with the service stopped ([upgrades](guides/upgrades.md), section 3). |
   | `rollback_failed` | The data is in an unknown state. | Stop. Restore your own verified backup (section 6, runbook D). |

6. **Going back later** is `alixai upgrade rollback`, with the service stopped. What was written since the upgrade stays in `.pre-restore-*` and is **not merged back**: plan a rollback window in which that loss is
   acceptable, and shorten it by upgrading when traffic is low.

A killed service leaves `alixai.db-shm`, and `upgrade apply`, `upgrade rollback` and `backup restore` refuse while it exists. Pass `-force` only after the process you started has exited. **Drill.**

## 6. Incident response

### 6.1 The first minutes

1. **Name the incident and its class** with the table below, and write the time down.
2. **Preserve before you repair.** Copy the log, take the diagnostics bundle (section 3), and do not delete `.pre-restore-*` directories, `*.partial` directories or a damaged database: they are evidence, and a
   restore keeps the old state precisely so that nothing written after a backup is lost.
3. **Stop the harm, not the service.** If one application is the problem, `alixai audit quarantine -reason TEXT APP` takes it out of service at once and the rest keep running ([operator commands](guides/operator-commands.md)).
4. **Follow the runbook.** Each ends with a verification; do not close the incident without it.
5. **Afterwards:** write what happened, what the runbook got wrong, and change this guide. A suspected vulnerability follows [SECURITY.md](../../SECURITY.md) and the
   [vulnerability response policy](../policies/vulnerability-response.md), not a public issue.

| Class | Meaning | Examples |
| --- | --- | --- |
| S1 | Loss or corruption of data that cannot be rebuilt, a security event, or a wrong commit | runbooks B, D, E, I, J |
| S2 | The service or a capability is down or degraded; nothing lost | runbooks A, C, G, H, K |
| S3 | A routine failure with a known fix | runbook F |

### 6.2 Graceful degradation, row by row

| Failure | What happens by itself | What you do | Drill |
| --- | --- | --- | --- |
| Commit response uncertain | The service asks the application's status by idempotency key and never commits again. A recorded outcome becomes the receipt; proof of "not applied" reopens the preparation; anything else is `uncertain`, and a maintenance job asks again every five minutes until the application can say. | Runbooks A and B | `TestDRKillDuringCommit` (the kill points); the reconciliation of `uncertain` is not drilled |
| SQLite or index corruption | A destroyed database is refused at start (fail closed). A damaged derived index is **not** repaired by itself. | Runbooks C and D: `index repair` (or `upgrade apply`) isolates and rebuilds the damaged index; `backup restore` replaces a destroyed database | `TestDRCorruptIndexRebuild`, `TestDROperatorCommandsInspectAndRepair`, `TestDRRestoreKeepsReceiptsAndAuditChain` |
| LocalAI crash | The supervisor restarts it with backoff; an action in flight stays uncommitted unless a receipt proves otherwise. | Runbook H | not drilled ([supervisor](localai-supervisor.md)) |
| Disk pressure | Ingestion pauses before the store is endangered. | Runbook G | not drilled |
| LLM, embedding, reranker, STT or TTS unavailable | The application and search keep working with the degradation the plan names. | Runbook H | not drilled |
| Source, MCP or API adapter unavailable | Affected sources are marked stale; no commit is reported; sessions and prepared state stay safe. | Check the application; `/readyz`; runbook A when it recovers | the kill points of `TestDRKillDuringCommit` |

### 6.3 Runbooks

**A. The service crashed, was killed, or the machine lost power.**
*Detect:* `/healthz` fails, or the log shows `interrupted commits resolved before serving` after a restart. *Recover:* start the service. Before it listens it resolves every preparation left in `committing`, by asking the
application's status, never by committing again. Read the counts: `recorded` (the application had it; a receipt was written), `reopened` (the application proved it never applied it; the preparation is back to
`confirmed`), `uncertain` (runbook B). *Verify:* the receipts of the interrupted actions exist where the application has the work and not where it does not; the application's own records show each booking at most
once. *Why you can trust it:* the `committing` state and its audit event are written **before** the application is called, in one transaction (SEC-087). **Drill:** killed with the call held before the application saw it
(preparation back to `confirmed`, no receipt, application asked 0 times) and after the application applied it (receipt written from the application's status, application asked once, a replay of the spent confirmation
answers 409), audit chains verified; a fresh booking after the reopened one committed once. A user whose preparation was reopened **starts again from a new request**: the confirmation secret was spent.

**B. An outcome is `uncertain`.**
*Detect:* a preparation in state `uncertain` and a receipt with status `uncertain` (`receipts list -status uncertain` lists them, section 3), or the `uncertain` count of runbook A. *What it means:* the service could not tell whether
the application applied the action, and it does **not** retry the commit. *What the service does by itself:* `alixai serve` runs a reconciliation job, a class 6 (maintenance) job on the one job queue and pool, so it yields to every
live turn. At start and every five minutes (not configurable in this build) it queues one job for each preparation that has been `uncertain` for at least five minutes, up to 100 per pass, and each job asks the application's status
again by the preparation's idempotency key, with the scope the preparation was made under, and never commits. A recorded outcome becomes a **new** receipt (`committed`, `queued` or `partial`) beside the `uncertain` one, which stays in
the ledger, with the `action.committed` event the coordinator defines; proof that the application never applied it reopens the preparation under the same idempotency key (the user asks again, from a new request); an application
that still cannot tell leaves it `uncertain` and is asked again next period, and the log says so (`an uncertain outcome is still undecided`). *Recover:* nothing, in the usual case: read the log record `an uncertain outcome was reconciled`.
If an outcome stays `uncertain` for longer than the application's outage, ask the application, with the idempotency key and its own tools, whether the action exists; if it does, tell the user, if it does not, the user asks again.
*Verify:* the application's records and AlixAI's receipts agree: `receipts list -application APP -preparation ID` shows the receipts of one preparation in order. **Run** (`TestServeReconcilesUncertainOutcomes`: a lost reply is recorded, a
dropped request is reopened, a refused status is asked again next period, and the application received no second commit); **not drilled** (the drill's kill points produce `recorded` and `reopened`, and the drill's application
is never made unable to answer).

**C. A derived index is damaged.**
*Detect:* searches miss what they should find; an integrity check of the index fails. The service keeps running. *Recover:* repair one index with `alixai index repair -index NAME` (the names are the derived indexes of the
database, `history_fts`, the full-text indexes of RAG and `vector_rows`; an unknown name is refused and the ones there are listed). It does what `upgrade apply` does to every index, for one index and outside an upgrade: it checks the
index and leaves a healthy one alone (`healthy`); it records a damaged one in the quarantine (`<data_dir>/upgrade/quarantine`) **before** its contents are dropped, so the running service labels what the index cannot answer
(retrieval says `reduced_semantic_recall`, history search is unavailable) from that moment; it rebuilds the index from its source, checks it and releases the record (`rebuilt`). It touches the record of the index it was given and no other, so
another index that is still isolated stays isolated. A repair that was interrupted is finished by running it again (`resumed`). A rebuild that cannot succeed (the damage is in the file) fails, leaves the index isolated and says to restore
the last backup. A **vector** index is the output of the embedding model, so it cannot be rebuilt by this command: a damaged one is isolated and reported `pending`, and `alixai serve` re-embeds it and releases the record once an embedder role
and `rag.embedding.model` are configured; until then retrieval is full-text only and says so. The other ways to repair are `alixai upgrade apply`, which does this for every index before it takes its backup, and `alixai backup restore` of a
backup (the index travels in it). **`upgrade recover` and `upgrade status` do not find fresh damage**: with nothing isolated, `recover` answers `nothing_to_do` ([the report](../evidence/m6-dr/report.md), section 5; [backup and restore](guides/backup-restore.md),
section 6, says otherwise). *Verify:* the command prints `outcome` `rebuilt`, `upgrade status` lists nothing under `quarantined`, and receipts, transcript and audit chains are unchanged. **Drill:** one copy of an indexed term was
flipped in the file; the check failed and a search found nothing; `upgrade apply` reported `history_fts` damaged and rebuilt, the term was found again and nothing else changed (the rebuild of 20 000 turns took about 0.1 s); and, in
`TestDROperatorCommandsInspectAndRepair`, `index repair` did the same with no release applied, left a healthy index alone, and left receipts, transcript and audit chains as they were.

**D. The database is damaged or destroyed.**
*Detect:* the service exits at start with `open the store: ... database disk image is malformed` (fail closed; **Drill**). *Contain:* stop the service; do not delete anything. *Recover:* restore the newest backup that verifies,
with the public key you kept off the machine, after the service has exited:

```sh
alixai backup verify -config /etc/alixai/alixai.yaml -dir /srv/alixai-backups/BACKUP_ID -pubkey backup=/safe/place/backup.pub
alixai backup restore -config /etc/alixai/alixai.yaml -dir /srv/alixai-backups/BACKUP_ID -pubkey backup=/safe/place/backup.pub -force -skip-newer-revocations
```

`-force` is for the `-shm` file a killed service leaves behind; the command refuses without it and says so. A restore keeps the key revocations of the database it replaces; a damaged or destroyed
database cannot give them, so the command refuses without `-skip-newer-revocations`, and afterwards you revoke again by hand any key revoked after the backup (`alixai trust revoke-key`,
`alixai identity revoke-issuer-key`; [backup and restore](guides/backup-restore.md), section 4). With the database intact, leave the flag out. The live database and trees move to `<data_dir>/.pre-restore-*` and the command prints the path. *Verify:* start the
service; its receipts are the backup's; read the database in `pre_restore_dir` with `alixai receipts list -db PRE_RESTORE_DIR/alixai.db -application APP -tenant TENANT` and `alixai audit list -db PRE_RESTORE_DIR/alixai.db -application APP -tenant TENANT` (section 3; they
copy the file and never change it, so keep the directory as it is) to find the receipts and audit events written after the backup; **reconcile them with the application** (the application
holds what AlixAI forgot). The audit chains of the restored database verify. **Drill:** a destroyed database refused at start; restored in about 1 s with receipts byte for byte as served before, chains verified, and the
booking made after the backup missing from AlixAI while the application still held it; and, with the database intact, the same restore kept both receipts and every audit event in `pre_restore_dir`.

**E. The data directory is lost.** Install and configure as in the [server guide](guides/server.md), do not start the service, and restore into the empty directory with the off-machine `backup.pub`:
`alixai backup restore` (the same command, no `-force`; `pre_restore_dir` is empty). A public key that did not sign the backup is refused and nothing is restored. Put the **checkpoint key** back at
`<data_dir>/keys/audit-checkpoint.key` (mode 0600, directory 0700); the restored trust store already holds its public half. Without it the audit log is not broken (the existing checkpoints verify from the public key in the restored trust store) and the service starts, logging that checkpoints are not signed; to sign again, create a new
key under a **new** id, because the old id is registered to the old key (`alixai keys init -key-id audit-checkpoint-2`), and set `keys.checkpoint_key_file` and `keys.checkpoint_key_id`. The first `backup create` afterwards makes a **new backup key** (the old one
was not in the backup): keep the old `backup.pub` too, because it verifies the older backups. **Drill:** restored with the off-machine key, a wrong key refused, the checkpoint key put back signed checkpoints, the next backup made a
new key and both backups verified.

**F. A backup was interrupted.** Delete the `<id>.partial` directory, run `backup create` again and verify it. **Drill:** killed while copying a file tree of 128 MiB; only the `.partial` directory remained, with no manifest;
it did not verify and could not be restored; the service and the database were untouched; the next backup completed and verified, and the leftover was still there.

**G. The disk is filling.** Free space first (old backups, `.pre-restore-*` once their records are handled, logs), then look at what grew (section 2.1). Ingestion resumes by itself at the resume floor. Never delete files from
inside `alixai.db`'s directory by hand.

**H. LocalAI or a model is down.** The application and search keep working with the graceful degradation of section 6.2; a model-backed answer is refused locally and nothing is executed from partial model output. Under the exec
supervisor, LocalAI is restarted with backoff and an action in flight settles by receipt ([supervisor](localai-supervisor.md)); if the supervisor gave up, restart `alixai` itself. Check `/readyz` and the journal of the
LocalAI unit.

**I. A key is compromised.** Contain first, rotate second (section 7): revoke the key, add its replacement, and review the audit log from before the suspected time. A compromised **issuer key** means someone could mint
context tokens for the application: `alixai identity revoke-issuer-key` refuses its tokens from the next request. A compromised **publisher key** means packages signed by it must not be trusted: `alixai trust revoke-key`, then
re-audit every package it signed. A compromised **checkpoint key** means checkpoints signed by it cannot be believed: revoking it makes them fail verification on purpose.

**J. Suspected data exposure or a cross-tenant leak.** Quarantine the application (`alixai audit quarantine`), preserve the log, the diagnostics bundle and the database, export the audit events of the tenant
through the host's `/v1/ops/audit/export` or list them from the preserved database with `alixai audit list -db FILE -application APP -tenant TENANT` after `alixai audit verify-chain -db FILE` (section 3), and report it as [SECURITY.md](../../SECURITY.md) says. Every scoped table carries application, tenant and workspace, so the scope
of an event names who could have seen what.

**K. An upgrade went wrong.** Section 5, step 5: `rolled_back` and `refused` leave the old state; `interrupted` is `upgrade recover`; later, `upgrade rollback`.

## 7. Key rotation

AlixAI never rotates a key by itself and never makes one up. Rotate on the cadence below (a **proposal** of this guide, not a product rule), at a change of people, and at once on suspicion. The detail of each
procedure is in the guide it points to; this table is the whole set.

| Key | Proposed cadence | Procedure | Overlap | Revoke the old one when | State |
| --- | --- | --- | --- | --- | --- |
| **Issuer** (a host application's context-token key) | yearly | Declare the new key id in the package, add it with `identity add-issuer-key` while the old one is active, let the host switch, then revoke the old. A `-not-after` ends an overlap by itself. [Key provisioning](key-provisioning.md), 1.3 | yes | the host has switched, or at once on compromise | Run (`cmd_identity_test.go`, `tests/e2e/m1`); not by the drill |
| **Publisher** (signs an application's packages) | with each key holder change | `trust add-key -app APP` for the new key; sign the next package version with both keys (`package sign` takes up to four `-key`); install and audit it; `trust revoke-key` the old | yes | the new version is active, or at once on compromise | Run (`cmd_trust_test.go`, `cmd_sign_test.go`); not by the drill |
| **Audit report signer** | yearly | `package keygen`, `trust add-key -app alixai.platform`, set `integration_audit.signing_key_id` and `signing_key_file`, restart, re-run `audit run` for each application | yes | the old reports no longer need verifying, or on compromise | **Not run** as a whole |
| **Checkpoint key** | yearly | `alixai keys init -key-id audit-checkpoint-2`, set `keys.checkpoint_key_file` and `keys.checkpoint_key_id`, restart. **Do not revoke the old key for a routine rotation**: a revoked key makes every checkpoint it signed fail verification, and the service refuses to start while it still names the old key. Revoke only on compromise. | yes | compromise only | **Drill:** new key made and used; old key revoked; the service refused to start on the revoked key and started on the new |
| **Backup key** | yearly | Make a key (`alixai package keygen -key-id backup-2 -out DIR`) and pass it to `backup create -key FILE -key-id backup-2`. **Keep every public key you ever used**: each backup verifies only with the key that signed it. | automatic | compromise only (then older backups cannot be believed either) | **Drill:** both backups verified with their own public key and not with the other; both public keys together verified both |
| **Release key** | owner's policy; held by the maintainer, never in CI or in the repository | `alixai trust add-key -app alixai.platform` for the new public key, or `-pubkey` on `upgrade`; [release](release.md), sections 2 and 3 | yes | per the owner's plan (task M5-H02) | **Not run**: no real key exists |
| **LocalAI API key** | yearly | Replace the file or environment that `localai.api_key_ref` names and recreate **both** AlixAI and LocalAI | no | on compromise | Static ([server guide](guides/server.md)) |

After any rotation: `alixai trust list -app alixai.platform` and `alixai identity list` show what you expect and nothing marked `TEST-ONLY`; take a backup (a new backup key signs it); and write the date down.

## 8. Data lifecycle

The data-lifecycle rules separate what a deployment keeps. This is where each class lives, how long it stays, and who removes it. **Of the retention numbers a package declares, `retention.action_receipt_days` and `audit_days` are enforced
(by the retention job, below); `session_state_days` and `log_days` are declarations: no job in this build deletes session rows or logs by them.**

| Class | Where | How long | Removed by | Rebuildable | In a backup |
| --- | --- | --- | --- | --- | --- |
| Application configuration | your configuration file; installed packages and the trust store in the database | until you change it | you | the file is yours | the database part; **not** the file, **not** private keys |
| Session and goal state | `sessions` (and its recent window `session_turns`) in the database | the session's goal timeouts; the row stays | a session ends; no deletion job; `alixai retention erase` removes the rows | no | yes |
| Transcript (what users read back) | `transcript_turns`, and its index `history_fts` | until a stored retention policy deletes it; with no stored policy, nothing is deleted | the daily retention job, the host's clear and cleanup calls, or `alixai retention erase` | the index only | yes |
| RAG metadata and indexes | the database | until the source is removed | deleting a source or a tenant writes tombstones; compaction removes the bytes later | yes, from the sources | yes |
| Source originals | the provider's own store | the provider's | the provider (they are not copied unless imported) | n/a | imported files only |
| Prepared actions and **receipts** | `action_preparations`, `action_receipts` | the package's `retention.action_receipt_days` after the action finished; never while it is pending or uncertain. Receipts are immutable (a trigger refuses update) | the retention job, or `alixai retention erase` | **no** | yes |
| **Audit events**, tombstones and checkpoints | `audit_events`, `audit_tombstones`, `audit_checkpoints` | the event stays for good; its **payload** is replaced by a signed tombstone after the package's `retention.audit_days`, or on an erasure request. Append-only (triggers refuse delete, and refuse update except that one replacement); the chain is never cut | the retention job, or `alixai retention erase` | **no** | yes |
| Logs and metrics | stderr, journald, your collector | yours | your log system | n/a | no |
| Backups | wherever you put them | yours | you | n/a | n/a |

**What this means in operation.**

- **History retention** is stated per level through the Settings API of the host application and capped by the operator's `settings.max_history_retention_days` (default 365). A retention job runs once a day as a
  low-priority background job, per workspace, and deletes the transcript turns older than the retention a **stored** policy states; with none stored it deletes nothing. The Settings API reports a default
  (`settings.default_history_retention_days`, 90) as the effective value when no level states one, so do not assume the default deletes anything: have the deployment level state a retention explicitly and
  read it back with `GET /v1/settings/effective`. Clearing history does **not** touch application records, receipts or audit events (SEC-087).
- **Deleting a source or a tenant** produces tombstones and invalidates caches first; bytes are removed by compaction afterwards. The audit chain records the deletion and is never cut.
- **Receipt and audit retention is enforced** (M5-26, [ADR-0036](../decisions/0036-audit-retention-and-erasure.md)). A maintenance job (class 6, one per application, queued at start and daily) removes the finished preparations and
  receipts older than the active package's `retention.action_receipt_days` and replaces the payload of audit events older than `retention.audit_days` by a **signed tombstone**: the event keeps its place, envelope and hash, so the chain
  and every checkpoint still verify, and the tombstone records who erased it, why and when. It needs the platform signing key (`alixai keys init`); without it receipts are still removed and audit payloads are not, and the log says so.
  **An application with no active package, or a package declaring less than the schema's minimum, has nothing removed**: unknown is never read as expired. The operator overrides the manifest mentions are not implemented; the package is the only source.
  `alixai retention run -dry-run` shows what a sweep would do; `alixai retention run` does one now.
- **Erasure on request.** `alixai retention erase -application APP -tenant TENANT -reason TEXT -operator ID` erases the payloads of every audit event of the tenant, removes its finished preparations and receipts, and
  removes its conversation history: the transcript (so history search finds none of it), the sessions' recent windows and the sessions themselves. An active session is ended first, as if the host had ended it, and a still-valid
  token for it opens a new, empty session. With `-principal ID` only what names that principal, and only that principal's sessions. The history goes in transactions of at most 1000 rows, each recorded in the tenant's chain as
  `deletion.history_cleared` (counts, kind, reason, operator; never whose); the report gives `transcript_turns_removed`, `session_turns_removed`, `sessions_removed` and `sessions_ended`. It is checked first in transactions that
  are rolled back and `-dry-run` stops there with the same counts, so a refused request changes nothing, history included. **It refuses, whole, while a pending or uncertain action still
  depends on what it would erase**: reconcile or finish the action (runbook B) and run it again; a retention sweep skips those rows instead. **After an erasure, show that the chain still verifies:**

  ```sh
  alixai retention erase -config /etc/alixai/alixai.yaml -application APP -tenant TENANT -reason "tenant erasure request" -operator OPERATOR
  alixai audit verify-chain -config /etc/alixai/alixai.yaml -application APP
  ```

  `chains_failed` must be `0`. **Run:** `TestErasureRefusesEventsPendingActionsDependOn` (the refusal, then the erasure and `audit verify-chain`) and `TestRetentionRunUsesTheDaysTheActivePackageDeclares` (`retention run` against an installed, active package, then
  `audit verify-chain`) in `internal/cli/cmd_retention_test.go`; the history part is `TestRetentionEraseOfAPrincipalTakesTheirHistory` and `TestRetentionEraseOfATenantTakesAllItsHistory` in
  `internal/cli/cmd_retention_history_test.go`; the job itself is `TestRetentionJobRemovesExpiredReceiptsAndAuditPayloads`. **Not drilled** on a deployed service.
- **What an erasure leaves.** The event's envelope (type, time, workspace, **principal id and session id**) and its hash stay, so erasing a principal removes what they did and said, not their identifier; if that identifier is personal data
  under your obligations, the host application must issue pseudonymous ones. A payload with little entropy can be confirmed against the kept hash by someone who can reconstruct it. Events on the platform's own chain that name a tenant are not covered.
  **Do not revoke the checkpoint key to rotate it**: revoking makes every tombstone it signed fail verification (keep the old key registered, section 7).
- **Receipts and audit events still grow**, by at most a tombstone (about 870 bytes) per erased event: plan the volume (section 2.1).
- **Backups outlive deletions.** A backup is a full copy of everything not yet deleted when it was made, so a deleted tenant, a cleared history or an erased receipt or audit payload is still in every older backup. Keep backups no longer than your
  deletion obligations allow, and delete expired ones yourself.
- **A restore resurrects what was deleted since the backup.** After a restore, re-apply the deletions you recorded outside the database (a list of tenants, sources, histories and erasure requests since the backup; `retention erase` again), and
  expect the retention jobs to remove expired transcript turns, receipts and audit payloads again on their next run.
- **Disposal.** To retire a deployment: stop it, delete the data directory (database, keys, file trees, `upgrade/` and `.pre-restore-*`), delete or securely erase every backup and the off-machine copies of
  `backup.pub` and the checkpoint key, and remove the service. `alixai service uninstall` on Windows keeps the data on purpose.

## 9. Evidence and the drill

The drill is `go test ./tests/dr/...`. It builds the binary, starts the clinic application and a proxy, puts a signed package into service with the real commands, and breaks the deployment on purpose
([the report](../evidence/m6-dr/report.md) lists each scenario, the timings and what it does not show). Run it on every release you intend to operate, on the machine you intend to operate it on:

```sh
go test -count=1 ./tests/dr/...
```

It needs Go, node 22.18 or newer and the clinic application's dependencies, and writes nothing into the repository; with `ALIXAI_DR_EVIDENCE_DIR` set it writes the report and the raw results where you point it. It also
checks every command line in a shell block of this guide and of the report, and every inline code span that starts with an `alixai` command name, against the built binary: the command must exist and each flag shown must be
in its `-h` (`TestDRDocumentedCommandsExist`). A command renamed in a later release fails the drill before it fails an operator.

## 10. Gaps this guide could not close

1. **Closed by M5-25:** `alixai audit verify-chain` verifies the audit chains of a database file (section 3).
2. **Closed by M5-25 and M5-26:** `upgrade recover` does not look for fresh index damage (runbook C); `alixai index repair` repairs one index without applying a release, and the [backup and restore guide](guides/backup-restore.md), section 6, now says so.
3. **Closed by M5-25:** an `uncertain` outcome is reconciled by a periodic job of the service (runbook B); the job is tested, not drilled.
4. **Closed by M5-26:** receipt and audit retention is enforced and both can be erased on request without breaking the audit chain (section 8). **Still open:** `session_state_days` and `log_days` are not enforced; the operator overrides per application and tenant are not implemented; an erasure does not reach backups, the platform chain or the host application's own records; the job and the commands were tested, not drilled on a deployed service.
5. **No release exists**, and nothing was run on a real host, a Linux machine, a packaged install or a container stack ([guides index](guides/README.md)). The drill is emulated.
6. **No model is activatable**, so no turn has run through the binary in the drill; preparations were made in-process.
7. **Backups are full copies with no schedule, no prune and no off-machine copy by AlixAI**: all three are yours (section 4).
8. **Closed by M5-25:** `alixai receipts list` and `alixai audit list` list receipts and audit events from a database file, so `pre_restore_dir` and the log after an incident are read with the product (section 3, runbooks D and J).
9. **The disk guard and the supervisor were not exercised by the drill.**
10. **Closed by M9-01:** the storage and ingestion charges are made with `ChargeTx` in the data's own transaction and source syncs are charged to ingestion (section 2.5). Still open: an upload to this build as a `rag_source` is refused (`attachRAGSources.RegisterFile`), so the ingestion charge of an uploaded source has only run with an accepting ingestion in tests, and `internal/cli/cmd_rag.go` does not pass the hook to `ingest.New` (the ingestion finds it through `ingest.BindCharger`, bound by the quota wiring).
