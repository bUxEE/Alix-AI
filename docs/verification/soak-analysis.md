# Soak behaviour: analysis, not a run

Status: **analysis only.** No 24-hour mixed-load run and no 7-day soak has been run on AlixAI 1.0, on any hardware. This page
says what the automated tests already cover, what the measured numbers predict for a long run, what a soak would catch that
the tests cannot, and how to run one. If you run one, please send the result (section 6).

Back to the [verification index](README.md).

## 1. What the existing tests cover

The tests below run on every change. They are short (seconds to minutes) and use fakes or small real components, so they
prove that a limit **exists and is enforced**, not that nothing drifts over days.

| Concern | What is tested | Where |
| --- | --- | --- |
| Request size | request bodies are capped (64 KiB by default) and rejected over the wire | `internal/gateway/limits_test.go` (`TestBodyLimit`, `TestBodyLimitOverTheWire`, `TestDefaultBodyLimitIs64KiB`) |
| Slow or stalled clients | slow-loris style clients are cut off | `tests/security/pentest` (`TestPentestSlowClientsAreCutOff`) |
| Streaming backpressure | a slow event consumer cannot make the server buffer without limit | `internal/stream` (`TestBackpressureBounded`) |
| Runaway loops | an agent or dialogue loop stops at its step and wall-clock limits | `internal/resources` (`TestNoUnboundedLoops`) |
| Background work vs interactive turns | background jobs yield to a live turn; leases expire and requeue; a panicking handler fails the job, not the process | `internal/jobs` (`TestPoolBackgroundYieldsToInteractiveJobs`, `TestLeaseExpiryRequeues`, `TestPoolHandlerPanicFailsTheJobNotTheProcess`), `internal/cli` (`TestLiveTurnYieldsBackgroundWorkThroughTheGovernor`) |
| Quotas and tenant state | admission is exact under contention; a noisy tenant does not starve others; idle tenants are evicted from memory; daily counters roll over at UTC midnight and survive a restart | `internal/quotas` (`TestAdmitIsExactUnderContention`, `TestNoisyNeighbourDoesNotStarveOthers`, `TestIdleTenantsAreEvictedWithoutAGift`, `TestMemoryPerActiveTenant`, `TestDailyCounterRollsOverAtUTCMidnight`, `TestCountersSurviveReopeningTheStore`) |
| Conversation memory | long histories (500 turns) stay inside the context budget; the oldest entries are pruned | `internal/dialogue/context` (`TestLongHistory500TurnsBounded`), `tests/gates/m3` (`TestM3LongHistoryBounded`) |
| Vector index and RAG | the resident vector cache stays current and bounded; tombstone compaction is bounded; ingestion pauses under disk pressure | `internal/rag/vector` (`TestResidentCacheStaysCurrentAndBounded`), `internal/rag/index` (`TestCompactionIsBounded`), `internal/rag/ingest` (`TestDiskPressurePausesIngestion`) |
| Optional models | an optional scorer is loaded on demand and unloaded after idle; a failed admission falls back with `resource_gate` instead of evicting the controller | `internal/decision/lifecycle` (`TestDemandLoadAndIdleUnloadAfterWarmPeriod`, `TestResourceGateRejectsWithoutEvictingController`), `internal/resources` (`TestIdleUnloadAfterWarmPeriod`) |
| Goroutine leaks | voice sessions and profiles, and the MCP client, return to the goroutine count they started with after close | `internal/voice/profile`, `internal/voice/session`, `adapters/mcp` |
| Retention | audit, history, actions and settings retention prune what they should | `internal/history`, `internal/actions`, `internal/settings`, `internal/cli` retention tests |
| Crash and restart | a hard kill during a commit or a backup leaves a recoverable state; restore keeps receipts and the audit chain | `tests/dr` (`TestDRKillDuringCommit`, `TestDRKillMidBackup`, `TestDRRestoreKeepsReceiptsAndAuditChain`); [drill report](../evidence/m6-dr/report.md), emulated |
| Footprint probes | the store, job queue and per-tenant quota state have measured per-unit costs | `internal/store` (`TestMemoryFootprint`), `internal/jobs` (`TestJobsFootprint`), `internal/quotas` (`TestMemoryPerActiveTenant`) |

**Hard caps that bound growth by construction** ([resource budgets](../operations/resource-budgets.md)): the SQLite WAL is
truncated to 64 MiB after a checkpoint; the vector cache is capped (64 MiB on Lite, 128 MiB on Balanced); a document is
parsed by one disposable worker process at a time with a hard memory budget (256 MiB on Lite, 512 MiB on Balanced); the
prompt, output, shortlist and RAG evidence caps are per profile; the job queue has one background job at a time on Lite.

**The harness itself is tested.** `tools/soak` has its own tests against fakes (`go test ./tools/soak/...`), and
`go run ./tools/soak --duration 1m --profile lite --fake` exercises it end to end. That run is labelled synthetic and soaks
nothing.

What is **not** covered: no test runs longer than a few minutes, none runs against a real LocalAI with real models over
hours, and none watches memory of the whole stack over time.

## 2. Expected steady-state memory, from measured numbers

All numbers below were measured on one x86-64 desktop under container memory caps (emulated, not certified hardware). See
[hardware.md](hardware.md) for the host and the method. "U" is the larger of the summed resident set of the LocalAI
processes and the container's working set, sampled every second.

**What runs by default on 1.0.** No controller model passes the Lite quality bar, so no profile has a default controller
([ADR 0010](../decisions/0010-lite-model-selection.md)). Lite and Balanced serve the deterministic path (typed forms, the
rules router, templates), RAG with the Granite Embedding 97M embedder, and the AlixAI core. The decision layer is optional,
bounded, non-generative and non-authoritative, and it is off on every profile.

| Component | Measured | Source |
| --- | --- | --- |
| AlixAI core process, idle | 23 MiB working set, 55 MiB private (Windows, one sample; no Linux figure) | core footprint probe |
| AlixAI core, budgeted with SQLite caches | 160 MiB (Lite), 256 MiB (Balanced) | resource budgets 4.2 |
| LocalAI service process, no model | 125 MiB idle, 146 to 160 MiB under load | resource budgets 4.1 |
| Embedder backend (Granite Embedding 97M, Q4_K_M) | 390 MiB alone; about +250 MiB beside a controller | resource budgets 4.1 and 4.3 |
| Vector cache | 0.38 KiB per hot vector, 37 MiB at 100,000 vectors | resource budgets 4.2 |
| Parser worker | 123 MiB for an 8 MiB result, no format above 152 MiB | resource budgets 4.2 |

**Predicted steady state, default Lite stack:** about 0.7 to 1.2 GiB in total (core 160, LocalAI 160, embedder 390 to 450,
vector cache up to 64, a transient parser up to 256), against 8 GiB with a required headroom of 1,229 MiB (15%). The margin is
large, so a Lite soak with the default configuration is expected to pass the headroom rule; what it tests is drift, not fit.

**If an operator adds a candidate controller** (none is qualified, so this is not a default), the measured peak U of the
whole LocalAI container was: Granite 4.0 1B 2,557 MiB, Qwen3.5-0.8B 2,062 to 2,082 MiB, MiniCPM5-1B 1,519 MiB on Lite, and
Qwen3.5-2B 3,121 to 3,136 MiB on Balanced ([model report](../evidence/m5-models/report.md)). These are still under the Lite
container budget of 5,811 MiB and the Balanced one of 12,134 MiB. The controller's memory follows the prompt: Qwen3.5-0.8B
went from 1,012 MiB loaded to 1,634 MiB under the bench prompts and 1,942 MiB under a 3,046-token prompt
([4 GB report](../evidence/m0-4gb/report.md)). The prompt caps of each profile are what keeps that growth bounded.

**Disk growth over a soak** (arithmetic, not measured over time). The default mix ([`tests/performance/soak/mix.json`](../../tests/performance/soak/mix.json))
is one session with a 20-second think time, about 180 turns an hour: about 4,300 turns in 24 hours and 30,000 in 7 days. At
the measured audit cost of about 8 KB per model-backed turn and 1.65 KB per policy decision, audit grows by tens of MB a day
and a few hundred MB over 7 days, before retention prunes it. The WAL stays at or under 64 MiB after checkpoints. Continuous
re-ingestion rebuilds the same source, so the index should stay flat; tombstones are compacted.

**CPU.** A resident model idles at 0.0 cores (resolution 0.1); the AlixAI core averages under 0.25 cores; CPU-quota
throttling was 1.5 to 2.3% of periods in the measured runs. A soak should see the same pattern.

## 3. What a soak would catch that the tests do not

1. **Slow leaks.** A few KiB per turn is invisible in a unit test and is 100+ MiB after 30,000 turns. Candidates: caches
   keyed by session or tenant that are never evicted, event subscribers that are not released, SQLite page cache growth,
   fragmentation in the LocalAI backends.
2. **Model eviction and reload.** LocalAI evicts the least recently used backend when a request needs one that is not
   resident. With the shipped `LOCALAI_MAX_ACTIVE_BACKENDS` (2 on Lite, 4 on Balanced) the controller and the embedder should
   both stay resident; a reload costs about 15 s per call when they do not. The harness fails a run on any reload.
3. **Swap.** The profiles forbid swap. A native install must turn swap and zram off itself; nothing enforces it outside the
   container cgroup.
4. **Time-based behaviour.** UTC day rollover of quota counters, retention sweeps, lease expiry, the provider terms
   revalidation job (External mode only), log rotation (the shipped compose sets no container log cap).
5. **Index and store drift.** Repeated re-ingestion of the same source, WAL checkpoints under constant writes, FTS5 and vector
   index size.
6. **Latency drift.** p95 turn latency creeping up over days, which points at one of the above.

## 4. How to run one

The harness is `tools/soak`. It acts as a host application: it mints context tokens with an issuer key you registered, drives
the [default mix](../../tests/performance/soak/mix.json) against a running stack, samples memory and swap of every container
(`docker stats` / `podman stats`) or of host processes (`/proc`, Linux only), reads `/readyz`, the queue and, if you give it,
the operator `/metrics`, and writes one `.soak` result file.

Start the stack under the profile's compose file (`deploy/container/stack/compose.<profile>.yaml`), install the clinic-booking
reference application, then:

```bash
# 24-hour mixed load, Lite
go run ./tools/soak --duration 24h --profile lite \
  --container alixai --container alixai-localai \
  --issuer-key app-issuer.pem --key-id <kid> --issuer <iss> --audience <deployment id> \
  --tenant <tenant id> --workspace <workspace id> --principal <principal id> \
  --metrics-url http://127.0.0.1:<metrics port>/metrics --out soak-results

# 7-day soak, Balanced (sample once a minute)
go run ./tools/soak --duration 168h --profile balanced --sample-interval 1m <same flags>

# re-derive the verdict from the stored result
go run ./tools/soak check -reject-synthetic -require-pass soak-results
```

Useful flags (`go run ./tools/soak run -h` lists all of them): `--docker-bin podman` for Podman, `--pid` instead of
`--container` for a native install on Linux, `--warmup` (default the smaller of 2 minutes and a tenth of the duration),
`--max-turn-p95-ms` and `--max-queue-depth` to fail on latency or queue depth (both report-only by default),
`--decision off|on` to state the decision-layer axis (`off` is the shipped state).

A run **fails** on: any swap in use (or swap that could not be read, unless `--allow-unmeasured-swap`); steady-state
headroom under max(15% of the profile's memory, 1 GiB), judged on the whole stack; a configured model role that is not ok
after warm-up; any model reload; more than 5% failed turns or ingestions; no turns or no ingestions; a restart; a run cut
short. The result is always labelled with the profile, the host and whether the caps were emulated.

## 5. What the result would mean

A pass on a container-capped desktop is an **emulated** result for that profile. A pass on a real 8 GB machine with no
container cap is a real-hardware result for that machine. Neither certifies a profile on its own: [hardware.md](hardware.md)
says what real-hardware certification needs.

## 6. Community results wanted

If you run a soak, open an issue titled "Soak result: <profile>, <duration>" with:

- the `.soak` result file, or at least its summary and failures sections;
- the AlixAI version and commit, and whether you changed any default (models, `LOCALAI_MAX_ACTIVE_BACKENDS`, caps);
- the host: CPU model, cores, RAM, disk type, OS and kernel, container runtime and version, and whether the profile was a
  container cap or the whole machine;
- the models in use, by registry id;
- anything you saw that the harness did not judge (latency spikes, log errors, disk growth with numbers).

Failures are as useful as passes. Please remove tokens, keys and private data before you post.
