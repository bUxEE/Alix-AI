// Package supervisor keeps a native LocalAI alive: it starts it (or attaches
// to it), probes its health, restarts it with backoff, gives up after a
// bounded number of failures and says "unavailable" while it is down (master
// plan 18: "LocalAI crash: supervisor restarts it with backoff; in-flight
// action remains uncommitted unless receipt proves otherwise"). Container
// installs do not use it: the orchestrator's restart policy (compose, Quadlet)
// does the same job there.
//
// The package imports only the base packages and internal/providers (module
// map). It never imports LocalAI code and calls no LocalAI route itself:
// health is whatever the injected providers.AIRuntimeProvider reports, so the
// LocalAI adapter's endpoint allowlist is neither widened nor bypassed.
//
// # Two ways to run it
//
// The hardened native unit (deploy/native, M5-02) has an empty capability set,
// NoNewPrivileges and DynamicUser, so a supervisor inside the AlixAI service
// cannot start LocalAI under another identity, and must not try (SEC-080,
// HC-41). The two Runtime implementations reflect that:
//
//   - Unit (the default for the AlixAI service): LocalAI is a separate unit or
//     service (its own account, its own cgroup limits) that the service manager
//     keeps alive. The supervisor attaches to it, health-checks it, backs off
//     while it is unhealthy and gives up after the limit. It starts nothing. An
//     optional Restart controller (for example a polkit-authorised helper) is
//     called on every restart after the first; the supervisor never shells out
//     itself, so without a controller "restart" means waiting, with backoff,
//     for the service manager to bring it back.
//   - Exec: the supervisor runs inside the LocalAI identity's own unit (never
//     inside the AlixAI service) and spawns the configured absolute binary with
//     fixed arguments and no shell. It refuses to run as root, never switches
//     identity, gives the child an explicit environment (nothing of AlixAI's
//     secrets is inherited), puts it in its own process group (unix) or job
//     object (Windows) so LocalAI's backend processes die with it, and applies
//     Limits. A limit the platform cannot enforce is a configuration error, not
//     a silent no-op: Linux enforces memory (RLIMIT_DATA, which counts private
//     memory and not memory-mapped model files) and open files, and disables
//     core dumps; Windows enforces memory through the job object; other
//     platforms enforce none, so use the service manager's limits there.
//
// # Backoff and give-up
//
// A failure is a process that exited, a runtime that never became healthy
// within StartTimeout, a runtime that stayed unhealthy for UnhealthyAfter
// consecutive probes, or a start that failed. Failures are counted in a sliding
// window: the n-th failure inside RestartWindow waits Backoff.Delay(n) (initial
// delay doubled each time, capped, jittered), and the failure number
// MaxRestarts+1 inside the window stops the supervisor: Run returns an
// unavailable error, no further process is started and Health reports
// unavailable until an operator calls Run again. Because failures age out of the
// window, a runtime that stays healthy for a whole window starts again from the
// shortest delay, so one crash a day never exhausts the budget while a crash
// loop does. A child that cannot be confirmed stopped is never replaced: the
// supervisor gives up rather than run two LocalAI processes.
//
// # In-flight actions
//
// A Ledger tracks the preparations that were in flight against the runtime. At
// every end of an instance (crash, unhealthy, shutdown), before any restart, the
// supervisor settles it: an action is reported committed only when a receipt the
// core wrote (looked up through ReceiptSource, never taken from model output)
// names that preparation and says committed, queued or partial. No receipt, a
// lookup error, a receipt for another preparation or an uncertain one leaves the
// action uncommitted and flagged for reconciliation; a rejected receipt proves
// nothing was applied. The ledger never commits or retries anything: a
// restarted runtime resumes nothing, and an uncertain outcome is resolved by the
// action coordinator's Recover and Reconcile through the host's Status, never
// by a blind second commit. The ledger is in memory by design: a crash of
// AlixAI itself is covered by that startup recovery.
//
// # Wiring
//
// The composition root builds a Supervisor with New, runs it with Run in its own
// goroutine, registers Supervisor (it implements providers.AIRuntimeProvider)
// with the provider registry in place of the bare adapter so that "starting",
// "restarting" and "gave up" reach every role as unavailable, and gates new
// model-driven work with Admit. Event reasons are short fixed strings; nothing
// the child prints is logged or placed in an error.
package supervisor
