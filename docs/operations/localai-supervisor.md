# The LocalAI supervisor in `alixai serve`

Status: M5-21. Rule: "LocalAI crash: supervisor restarts it with backoff; in-flight action remains uncommitted unless
receipt proves otherwise"; SEC-046, SEC-080, HC-41. The loop itself is `internal/supervisor` (M5-07); this page is how
`serve` runs it. Written and tested against fakes and a stub runtime; **not run against a real LocalAI under systemd** (ADR 0012, "What was not
measured").

## Off, attach or exec

| `supervisor.enabled` | `supervisor.runtime` | Where it runs | What it does |
| --- | --- | --- | --- |
| `false` (default) | | | Nothing. The controller role is the bare LocalAI adapter, as before. Container installs stay here: compose or Quadlet restarts LocalAI. |
| `true` | `unit` | `alixai serve` | **Attaches** to a LocalAI that something else keeps alive (`localai.service`, or a container on the same address). Health-checks it, backs off while it is unhealthy, settles in-flight actions when it dies. Starts and executes nothing. |
| `true` | `exec` | `alixai supervisor exec`, the command of `localai.service` | Spawns the absolute LocalAI binary (no shell, explicit environment, never root, own process group, limits) and keeps it alive with the same loop. `serve` refuses this value. |

The hardened AlixAI unit has an empty capability set, `NoNewPrivileges` and `DynamicUser`, so it cannot start LocalAI under another identity
and must not try. That is why the exec runtime lives in its own unit (`deploy/native/systemd/localai.service`, configuration `localai.yaml`)
and `alixai.service` carries only `supervisor: {enabled: true, runtime: unit}`. A test pins both halves.

## What the wrapped runtime does

With the supervisor on, the controller role is `supervisorRuntime`: Chat, ChatStream and Embed are the adapter's, **Name and Health are the
supervisor's**. The supervisor probes through the adapter's own `Health`, so no LocalAI route is called by it and the adapter's five-path
allowlist is unchanged.

| Supervisor state | Health (and `/readyz`) | Model calls (`Admit`) |
| --- | --- | --- |
| stopped | unavailable, "runtime supervisor not running" | refused, `unavailable` |
| starting | unavailable, "runtime starting" | refused |
| running | the adapter's report (ok, degraded, version) | admitted |
| backoff | unavailable, "runtime failed; restart scheduled" | refused |
| gave up | unavailable, "supervisor gave up, operator action required" | refused |

A refused call never reaches the adapter. `/readyz` has a `runtime_supervisor` check that fails with the state while the runtime is not running.
After it gave up, the model path stays unavailable until `alixai` itself is restarted (the loop is not re-armed by anything else); the
LocalAI unit is a separate matter, systemd restarts it (`Restart=on-failure`) and `exec` exits non-zero when it gives up.

## In-flight actions (SEC-046)

Every `Commit` the action coordinator sends to an application adapter, from the public API or from a dialogue turn, is a `Begin` and `End` on the
supervisor's ledger. A commit does not need the model, so it is never refused while the runtime is down. When an instance ends (crash,
unhealthy, shutdown), **before any restart**, the ledger settles what was in flight: an action is *committed* only when the coordinator's own
receipt for that preparation exists and says committed, queued or partial. No receipt yet, an uncertain receipt, a receipt of another
preparation or a failed lookup leave it uncommitted and flagged for reconciliation; a rejected receipt proves nothing was applied. Nothing is
retried or resumed: an uncertain outcome is resolved by the coordinator's `Recover` and `Reconcile` through the host's `Status`, never by a second commit.

The ledger is in memory by design. A crash of AlixAI itself is `actions.RecoverAtStartup`'s job, which already runs before serve listens.

## Configuration

```yaml
supervisor:
  enabled: true
  runtime: unit          # exec only in the LocalAI unit's own configuration
  max_restarts: 5        # tuning of the loop, all optional (defaults: 5 restarts in 5m, backoff 1s doubling to 30s,
  restart_window: 5m     #   5m to become healthy, 3 unhealthy probes)
```

`supervisor.exec.*` (path, args, env, `api_key_env`, limits) is read by `alixai supervisor exec` only; see `deploy/native/systemd/localai.yaml`. The
LocalAI key stays a file reference (`localai.api_key_ref`): the exec command reads it and puts it in the child's environment under `api_key_env`.

## Known limits

- The ledger sees commits that go through the coordinator's adapter source. A model call is not an "action"; its refusal is the `Admit` above.
- Only the controller role is registered with the registry in `serve` today (`startInference`); whoever registers the embedder must register the
  same wrapped value (`supervisorRuntimeOf`), not the bare adapter, so that its calls are admitted too.
- The supervisor discards LocalAI's output. No `exec.log_file` exists yet.
- Memory limits: the unit's `MemoryMax` is the cap that counts; `exec.max_memory_bytes` is `RLIMIT_DATA` on Linux (private memory, not mapped model files).
