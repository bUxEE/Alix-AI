# 0018 — Resource accounting and quotas for shared multi-tenant deployments

- **Status:** accepted
- **Date:** 2026-10-02 (provisional), accepted 2026-10-04
- **Decided in task:** M0-16 (provisional default); finalised in M5-06

## Context
A deployment can host many tenants of one application and, within one security domain, several
applications ([0016](0016-multi-app-process-model.md)). They share CPU, the single SQLite writer,
the resident model's KV slots and disk. Without accounting, one tenant's bulk ingestion or request
flood degrades every other tenant (§17.1 says lower classes yield to higher ones, but says nothing
about tenants within a class).

[0007](0007-sqlite-job-queue.md) bounds queue depth *per priority class*, not per tenant, so a single
tenant can fill a class's depth and starve the rest. That gap is part of this decision.

§25 item 9 asks for resource accounting and quotas for shared deployments. What has to be true:

- Quota keys always include **application and tenant** (never a bare tenant id), derived from the
  verified `Scope`, never from request input.
- Exhaustion is a typed, visible error, never a silent drop: `rate_limited` for rates and
  `resource_exhausted` for volumes (both exist in the error taxonomy of M0-04).
- Fail closed, but never half-done: quotas are checked at admission, before a prepare or any other
  step that starts work. A confirmed commit is not rejected for quota after the user has confirmed.
- Operational metrics stay local (rule 3); no mandatory telemetry.
- Footprint stays negligible: no external service in the minimum profile (§5.1), and the 4 GB
  direction counts every resident byte.

Dimensions named by the plan: requests per minute, concurrent sessions, ingestion bytes per day and
storage.

## Options
A. **No quotas, single-tenant assumption.** Simplest. Wrong for any shared deployment, and it
   makes the §17.1 guarantees only hold between classes. Rejected for shared deployments; a
   single-tenant install gets generous finite defaults instead (see Decision).
B. **Reverse-proxy or API-gateway limits only.** Useful as an outer layer against floods. But it
   sees addresses and paths, not the verified tenant, ingestion bytes or storage, and a request that
   reaches the service directly bypasses it. Not sufficient alone; recommended in front of option C.
C. **In-process accounting in a dedicated `internal/quotas` module** (chosen, below). A token bucket
   and a leased semaphore per tenant in memory, counters in SQLite for the volumes.
D. **Distributed limiter (Redis and similar).** Needed only for multi-node deployments, and
   excluded from the minimum profile. Stays possible behind the same interface.
E. **Per-tenant OS limits (cgroup per tenant).** Impossible inside one shared process. The only way
   to get it is a deployment per tenant, which is option A of
   [0016](0016-multi-app-process-model.md).

## Decision
**Option C, accepted**, with option B as a recommended outer layer in the deployment documentation.
`internal/quotas` (M5-06) implements the simplest accounting that protects a shared deployment:

| Dimension | Mechanism | Where it lives | A restart |
| --- | --- | --- | --- |
| `requests_per_minute` | token bucket per tenant: capacity `burst`, refill `requests_per_minute`, integer arithmetic on the injected clock | memory | every bucket starts full (a bounded gift of one burst per tenant) |
| `concurrent_sessions` | counting semaphore per tenant; every slot is a lease that expires after `lease_ttl` unless renewed | memory | every lease is gone, which is exact: the sessions died with the process |
| `ingest_bytes_per_day` | counter per tenant and UTC day | SQLite `quota_counters` | kept; rolls over at 00:00 UTC |
| `storage_bytes` | gauge per tenant | SQLite `quota_counters` | kept |

- **Key.** `(application_id, tenant_id)` of the verified `Scope`, as a struct (no string concatenation, no
  collision). The same tenant id under two applications is two tenants; every workspace of a tenant shares
  the tenant's quota; an invalid scope is refused with its own code and a tenant with no scope has no
  quota. Rows carry `workspace_id = ''` (the settings module does the same for tenant-level rows).
- **API.** `Admit(ctx, scope, dimension, amount) (*Lease, error)` (the interface the reversal plan below
  names; the lease is empty except for sessions), `ChargeTx(ctx, tx, scope, dimension, amount)` for the
  two volumes inside the caller's own write transaction, `Refund`, `SetStorage` (the class 6 reconciliation
  of the gauge), `Usage` (the caller's own tenant only), `Prune` (dispatcher side) and `Stats`.
- **Exactness.** A volume is charged in the same transaction as the data it describes: an exhausted quota
  aborts the data write, and a failed data write takes its charge with it. Reads and writes happen inside one
  `BEGIN IMMEDIATE` transaction, so two writers cannot both pass the check. A refused `Admit` takes nothing: no
  token, slot or byte, and no lease comes back with the error. The table refuses a negative counter, a
  workspace-level row and a gauge with a window, whatever the code does.
- **Errors.** `rate_limited` (retryable) for the bucket, with `retry_after_ms` (the exact wait: after that long
  the request is admitted, one nanosecond less is not). `resource_exhausted` (retryable) for everything else,
  with `dimension`, `limit`, `used`, `requested` and, for the daily counter, `resets_at` (RFC 3339, UTC). A request
  larger than the burst can never be admitted and is `resource_exhausted`, not a retry that cannot work.
  Messages never name the tenant. Nothing is silently dropped, queued unboundedly or half-executed
  (`tests/degradation` asserts the section 18 rows that quota exhaustion must never turn into a success).
- **Defaults are finite, and unlimited is explicit.** A missing or zero configuration is never unlimited: every
  field inherits tenant override, application override, `defaults`, profile, in that order, and the chain ends in the
  profile's finite numbers (`lite` when no profile is named). The one way to remove a limit is the explicit
  value `-1`. Configuration is the `quotas` section: `profile`, `defaults`, `overrides` (per application, and per
  tenant of an application), `lease_ttl` (1 h) and `idle_evict_after` (10 min); a malformed value is a startup error.
- **Memory.** A tenant that has been seen costs about 160 bytes plus its id strings; idle tenants are forgotten (a
  sweep every `idle_evict_after/2`), but only when forgetting changes nothing: no live lease, and a bucket that is
  already full again, so nobody gains tokens by being forgotten.
- **Accounting is local.** `Usage` and `Stats` are for the local operations API; nothing is exported unless the
  operator configures it.

### Finite defaults per profile
Per tenant. Derived from `docs/operations/resource-budgets.md`; every number is a proposal (basis P) that the operator
overrides, and none is certified.

| Profile | requests / min | burst | concurrent sessions | ingest bytes / day | storage |
| --- | ---: | ---: | ---: | ---: | ---: |
| `experimental-4g` | 60 | 6 | 4 | 256 MiB | 2 GiB |
| `lite` (the default) | 120 | 12 | 8 | 1 GiB | 8 GiB |
| `balanced` | 300 | 30 | 32 | 2 GiB | 16 GiB |
| `performance` | 600 | 60 | 64 | 8 GiB | 64 GiB |

How they were derived:

- **Requests.** §7.2 puts a deterministic turn at p95 <= 100 ms of one core (section 5.1: 9 to 22 ms), so one
  worker serves several hundred such requests a minute. The default is a fifth of that on `lite`, so one tenant cannot
  take the machine while an ordinary user (a few requests per action) never meets it. Model-backed turns are bounded by
  the resident controller's single slot (rule R1, 35 s p95 per turn on `lite`) and by the resource governor's live
  admission, not by this number. `burst` is a tenth of a minute of traffic, at least 1; over any minute a tenant
  can spend `burst + requests_per_minute`.
- **Sessions.** An open session is a connection and a few kilobytes; the cost is its turns, which queue behind the one
  interactive slot. The default is how many a tenant may keep open, small where the slot is single.
- **Storage.** Section 5.2: provisioned free space is 8, 20 and 40 GB for the three profiles, an install is 1.7, 2.6 and
  4.1 GB, and the disk guard pauses ingestion under a 2 GiB free floor. What is left is 4.3, 15.4 and 33.9 GB; the
  default is about half of it (2, 8 and 16 GiB), so one tenant on its own cannot reach the guard's floor. `performance`
  has no row there and is a proposal at four times `balanced`.
- **Ingestion per day** is an eighth of the storage quota. Ingestion throughput on a profile is not measured (resource
  budgets, section 9), so the daily number is a disk guard, not a throughput claim.
- On a shared deployment the operator sizes `tenants x quota` against the disk. The per-tenant defaults do not add up
  to a machine; the disk guard of the resource governor is the second line.

### Measured (task M5-06; Windows 11, Core i7-10700K, 16 threads, Go 1.27, `CGO_ENABLED=0`; emulated, not certified)

1. **Admission.** `go test ./internal/quotas -bench Admit` over 10,000 distinct tenants (round robin, buckets
   refilled between rounds): 0.7 to 1.1 us per admission, **0 allocations**; 10,000 tenants concurrently active
   cost **159 bytes each** (heap 2.42 MB to 4.01 MB, `TestMemoryPerActiveTenant`, which fails above 1 KiB). One tenant
   hammered by 16 goroutines: about 0.45 us per admission, 0 allocations (the global mutex is not a bottleneck at
   this scale; shard it only if a profile shows contention). A refusal builds its typed error: about 0.8 to 1.3 us,
   4 allocations, 408 bytes. A `storage_bytes` or `ingest_bytes_per_day` charge is one short write transaction:
   about **0.15 ms** and 59 allocations.
2. **SQLite write rate and noisy neighbour.** There is no flush loop and no steady write rate: the persisted counters
   are charged once per ingested document or upload, never per request, and the request rate is in memory. A
   neighbour hammering 4 goroutines for 0.2 s (about 200,000 refusals, one goroutine of them write transactions
   against the one writer) moved the quiet tenant's ingest-charge p95 from 0.5 ms to 0.6 ms in three runs and its
   request admission stayed below the timer resolution (`TestNoisyNeighbourDoesNotStarveOthers`; no quiet-tenant
   request was refused). The full interactive p95 under a flooding tenant needs the whole service and belongs to the
   performance suite (M5-GATE), not to this module.
3. **Races and restart.** The exactness tests run 32 goroutines against each dimension and require the admitted count
   to equal the limit exactly, with every request decided (admitted or refused, none dropped); `go test -race` runs in
   Linux CI only (needs cgo). A restart loses nothing persisted: `TestCountersSurviveReopeningTheStore`. In-memory
   state is bounded by the table above (a refilled bucket, no lease). The persisted counter cannot go negative or
   wrap: the column has `CHECK (used >= 0)`, `Refund` stops at zero, an amount is `1..2^60`, and a charge past the
   ceiling is refused (mutation-checked, see below).
4. **Defaults** are the table above.
5. **Section 18.** `tests/degradation` holds one test per row; quota exhaustion is covered by
   `TestTenantQuotaEnforced` and `TestQuotaExhaustionIsTypedError`.

Mutation check (security-relevant module): fifteen hand-made faults were each caught by at least one test: the key
without the application (admission and charge), `>=` for `>` on a volume and on sessions, a zero limit meaning
unlimited, no profile defaults, the burst check removed, a blind `Release`, a negative amount accepted, eviction of a
bucket that is not full, expired leases not purged, a charge before the check, a charge on a refused request, a
select that ignores the tenant, the day taken in the clock's own zone, `resets_at` in the clock's zone.

### What changed from the provisional text, and why
- **No buffered request counters.** The provisional text buffered request-level counters in memory and flushed them in
  batches. With the request rate in memory and the volumes charged per document, nothing is written per request, so there
  is no flush interval, no under-count after a crash and no write amplification. Overturn criterion below.
- **Deferred: model tokens per day, and per-class job caps with fair dequeue.** Neither is in the four dimensions of
  the task, and tokens per day needs a measured per-turn cost that the profile table above cannot yet supply. The
  fairness check ADR 0018 asked for was made: `internal/jobs` already passes an `EnqueueGuard` the exact per-tenant
  queued and running counts inside the enqueue transaction, and a `DequeuePolicy` the oldest ready job of each tenant
  in the class (one candidate per tenant) but defaults to FIFO. So no change to `internal/jobs` is needed: a cap and a
  round-robin policy are small adapters over `jobs.GuardRequest` and `jobs.Candidate`. `internal/quotas` may not
  import `internal/jobs` (module map: base and `store` only), so the adapters belong to a composition root or need
  that map entry changed. Until then one tenant can still fill a class's bounded depth; it is rate limited, and its
  ingestion bytes are capped, but its queue share is not.
- **Accounting is not wired into any request path yet.** The module is complete and tested on its own; the admission
  calls (gateway, session open, ingestion, upload) are composition-root work.

**Overturn criteria.** Replace the in-memory bucket's mutex by shards if a profile shows contention. Add buffering for
request-level persisted counters (the original design) if a persisted counter is ever charged per request. Add a
distributed limiter only when a multi-node deployment exists. Raise per-tenant isolation to a deployment per tenant
when a tenant needs guarantees quotas cannot give (shared memory and CPU are not partitioned by quotas).

## Consequences
- Easier: one local mechanism, deterministic under the injected clock, visible per tenant, and no extra service or
  dependency. A tenant's exhaustion is a typed error a host can react to.
- Harder: every admission point (turn, session, ingestion, upload) must call the module, and defaults need care so a
  small deployment is never throttled by accident (they are generous on purpose).
- Security impact: a noisy or hostile tenant can no longer exhaust a shared deployment's request budget, sessions,
  daily ingestion or storage; quota state cannot be forged (the key comes from the verified scope) or read across
  tenants; and the denial is a typed error. Quotas bound *consumption*, not isolation; they do not make a shared process
  hard-multi-tenant ([0016](0016-multi-app-process-model.md)).
- Forbidden: a quota key without application and tenant, an implicit "unlimited", rejecting an already-confirmed commit
  for quota reasons, and dropping work without a typed error.

## Reversal plan
Callers depend on `quotas.Admit(ctx, scope, dimension, amount)` and its lease, not on the storage. Replacing the
mechanism (a distributed limiter, synchronous or buffered counters) is a second implementation of that interface selected
by configuration. Persisted counters are plain SQLite rows (`quota_counters`) and can be dropped and rebuilt (usage from
rows, storage from sizes with `SetStorage`) without affecting tenant data.

## Sources (checked 2026-10-04)
- Soft versus hard multi-tenancy and quotas as a control-plane tool: <https://kubernetes.io/docs/concepts/security/multi-tenancy/>
- Queue depth per priority class: [0007](0007-sqlite-job-queue.md); error taxonomy: M0-04 (`pkg/contracts`);
  resource budgets: `docs/operations/resource-budgets.md`.
