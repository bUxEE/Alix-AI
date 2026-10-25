# Operating the decision scorer

Status: provisional plan, 2026-10-07, task D-01. Implemented by D-05 (adapters), D-06 (sidecar package) and D-13 (lifecycle and metrics); model and revision records are D-11.
The decision scorer is optional, bounded, non-generative and non-authoritative. With it disabled the system behaves identically through the deterministic route and the controller, so every step below can be skipped or reversed without a data or contract change.
No number in this document is a measurement: thresholds are hypotheses to be replaced by values from D-07 and D-08 runs on the target host, and every figure from a limited profile is labelled emulated.

## 1. Installation

1. Decide the deployment shape per [ADR 0030](../decisions/0030-decision-scorer-deployment.md): supervised sidecar (provisional preference) or LocalAI Decisions, whichever testing supports. A native in-process scorer is not planned.
2. Keep `decision.enabled=false`, the shipped state. Nothing is loaded and no scorer endpoint is contacted while it is false.
3. Sidecar: bring up `deploy/container/clm` (D-06) bound to loopback or a Unix socket only. The encoder download happens in the main service's entrypoint, not in an init container; start-up is gated by healthchecks.
4. Register the scorer in the model registry (D-11) with exact model and encoder revisions and sha256. An entry without both stays `candidate` and cannot be enabled.
5. Run `go run ./tools/decbench run` (D-07) on the target host and keep the result file. Enabling CLM-8B for a profile requires a recorded benchmark that passes the default-enable gate ([ADR 0031](../decisions/0031-decision-scorer-default-enable.md)); a smaller scorer needs the same evidence for its role.
6. Only then set `decision.enabled=true` for that profile, as a staged rollout (section 3).

## 2. Model revisions and hash verification

- A revision is the triple model commit, encoder commit and runtime image digest; the registry holds a sha256 for every artefact.
- The sidecar entrypoint verifies the hashes against the registry before serving and refuses to start on a mismatch; the lifecycle verifies them again before a staged switch (D-13).
- A revision change resets the status to `candidate` and invalidates the decision cache, which is keyed by scope, contract version and model revision.
- Nothing is pulled at run time except by an explicit operator action; downloads are outbound calls and are listed in `deploy/container/clm/hardening.md`.

## 3. Update and staged rollout

1. Stage the new revision beside the active one and verify its hashes.
2. Shadow stage: score a sample of requests with the new revision, record agreement and fallback reasons, and use none of the results.
3. Canary: enable for one application or a small share of turns; the deterministic route stays the fallback.
4. Promote only if the gate metrics of ADR 0031 do not regress; otherwise roll back.

## 4. Rollback

- Automatic: a failed health probe or a gate breach returns to the previous revision, which is kept until the new one is promoted (D-13).
- Manual: `decision.enabled=false`, or a build with the `nodecision` tag, returns to deterministic routing plus the controller. Stop and remove the sidecar with the commands in its README.
- A rollback never touches application data, contracts or the public API.

## 5. Observability

Metrics stay local; there is no outbound push. Labels are provider, model, revision and profile; none identifies a tenant or a user.

| Metric | Why |
| --- | --- |
| decision latency histogram (p50, p95, p99) | compare with the controller-only route |
| acceptance rate and fallback count by typed reason (`disabled`, `unavailable`, `timeout`, `malformed_output`, `low_confidence`, `calibration_failed`, `ood_unsupported_shape`, `model_mismatch`, `tenant_mismatch`, `policy_conflict`, `candidate_set_rejected`, `resource_gate`, `unsupported_runtime`) | why the layer was not used |
| controller calls avoided | the reason the layer exists |
| cache hit rate | warm-cache reuse |
| load and unload events, evictions | demand-load behaviour |
| resident memory and reserve | the memory gate |
| calibration error per revision | calibration drift |

## 6. Alerting thresholds (hypotheses)

Starting points only; an alert reports, and the automatic response is always the deterministic fallback.

| Signal | Initial hypothesis | Response |
| --- | --- | --- |
| fallback rate other than `disabled` | above 20% over 15 minutes | investigate; consider rollback |
| `timeout` rate | above 5% over 15 minutes | check load, memory pressure and eviction |
| `model_mismatch` or `tenant_mismatch` | any occurrence | treat as an incident; disable the scorer for that scope |
| p95 decision latency | above the controller-only p95 for the same turns | no benefit on this host; roll back |
| memory reserve | below the profile's declared reserve | the resource gate refuses further loads |
| calibration error | rising for two consecutive windows | re-calibrate or roll back |

## 7. Running it in `alixai serve` (D-15)

The decision scorer stays optional, bounded, non-generative and non-authoritative: serve wires it, it does not change what it is. `decision.enabled` is `false` as shipped, and with it off serve builds no provider, no supervisor, no tick goroutine and no router registration, the dialogue engine keeps its no-op decision stage and rule detectors, and every flow runs exactly as before. The only visible difference is that `GET /v1/decision/status` answers `mode: off`, `provider_id: none` and zero counts, and `PUT /v1/decision/config` is refused as unavailable (a layer the deployment did not build cannot be enabled from the API).

### Config fields (`decision:`)

| Field | Meaning |
| --- | --- |
| `enabled` | Builds the layer. Default `false`. Nothing below is acted on while it is false. |
| `provider.kind`, `provider.model`, `provider.model_revision`, `provider.endpoint`, `provider.mode` | The scorer (`localai` or `clm`). A model with no pinned revision reports `unverified` and the calibration gate refuses it. A `localai` scorer reaches the runtime through the `localai` section. |
| `provider.api_key_ref` | The CLM service key as `env:NAME` or `file:PATH`. The key itself is refused and never logged. |
| `timeout` | One scorer call (default 250 ms). A first call that must load a large scorer needs more, or a sidecar that is already running. |
| `platform_floor` | The lowest `min_confidence` the deployment accepts, between 0.5 and 1 (default 0.5). No API call can set a profile below it. |
| `profiles.<name>.mode`, `profiles.<name>.min_confidence` | The approved deployment profiles. The entry of the running profile governs; with none, the running profile starts in `shadow` at 0.85. `off` disables the stage, `shadow` runs the scorer and discards its answer, `advisory` lets a gated answer narrow the controller's shortlist. |
| `operators[]` | `application_id`, `tenant_id` and `principal_id` of each operator who may read and change the configuration. All three must match the verified token; a role or capability the token asserts counts for nothing; none configured means nobody. |
| `calibration[]` | Measured values only: `model_id`, `model_revision`, `contract_version` (`dialogue-routing-v1` or `dialogue-act-v1`), `ece`, `max_ece`. With no matching entry every result is `calibration_failed`. |
| `peak_bytes` | The scorer's measured memory, which the supervisor asks the resource governor to reserve (class: active turn). Zero reserves nothing, so the governor only arbitrates, and serve logs a warning. The CLM encoder dominates this figure; the registry's artifact size is only the head. |
| `tick_interval` | How often serve calls the supervisor's `Tick` (default 5 s). |
| `lifecycle.*` | `min_warm`, `idle_unload`, `backoff_initial`, `backoff_max`, `load_timeout`, `probe_timeout`, `probation`; a zero takes the lifecycle default. |

Changes made through the API are audited (`decision.config_changed`, per-mille integers, on the operator's tenant chain) before they are applied, and are not written back to the file: a restart returns to the file. A new threshold rebuilds the layer, because the engine fixes its confidence floor when it is built.

### Tick events

serve owns one goroutine that calls the supervisor's `Tick` and logs each event it returns under `decision scorer`. The supervisor never loads a scorer nobody asked for, so an unloaded scorer stays unloaded until a request needs it; it is stopped, and its reservation released, when serve stops.

| Event | Meaning |
| --- | --- |
| `probe_failed` | The health probe of the loaded scorer failed; it is marked down and backs off. |
| `restarted` / `restart_failed` | The backoff elapsed and the scorer was loaded again, or the load failed again. |
| `backoff_reset` | A restarted scorer stayed up for `min_warm`; the failure count is cleared. |
| `idle_unload` | No request for `idle_unload` and loaded for at least `min_warm`: the scorer was unloaded. |
| `promoted` | A staged revision's probation ended and the previous revision was released. |
| `rolled_back` | A staged revision in probation failed its probe (or a gate breach was reported) and the previous revision serves again. |

### Probation

A staged update (section 3) switches revisions only after the new one loads and passes its health probe. With `lifecycle.probation` above zero the previous revision stays resident until the probation ends: a failed probe at a `Tick`, or a reported gate breach, returns to it at once (`rolled_back`), and a clean probation ends in `promoted`. With zero the switch is final. serve runs the `Tick` that ends a probation; the operator command that stages an update is not part of this task.

### Limits to know

- The sessions, AG-UI and voice routes run a turn through `Layer.Wrap(engine).Turn` (provided to the gateway as `ddec.Turner` while the layer is built), which binds the scope the scorer-backed control detectors need. With the layer off no wrapper is provided and the routes call the engine as before.
- Counters and the latency, acceptance and fallback metrics are recorded from the `decision.accepted` and `decision.fallback` audit events, in memory and per application and tenant; the metrics carry no tenant label. The layer also has a metrics hook (`decision.Deps.Observer`, satisfied by `lifecycle.Metrics`) that is told every advice and every controller call the scorer avoided; a root that sets it should not also count the same events from the audit tee.
- The `nodecision` build refuses `decision.enabled` with a clear error.
