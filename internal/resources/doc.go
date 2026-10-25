// Package resources is AlixAI's resource governor (master plan 3 rule 12, 17.1,
// 17.2 and 18). It keeps background work from starving interactive work and
// keeps the process inside a memory budget, through hooks the job queue and
// ingestion already accept; it starts nothing on its own except Run.
//
// # What it does
//
//   - Admission (Governor.Admit, AdmitModelLoad): work is checked against a
//     budget made of this process's RSS, the memory the provider runtime
//     reports, and the reservations of work already admitted. Live work that
//     does not fit is rejected with a typed resource_exhausted; background work
//     that does not fit is held (it waits in the durable queue). Background
//     work may use only Config.BackgroundPercent of the budget, so an
//     interactive reserve always remains. Admission never unloads a model to
//     make room. Live work that reserves nothing (Bytes 0: a commit, an ordinary
//     read) is only arbitrated, never refused for memory.
//   - Arbitration (BeginInteractive, Policy, Yield): a live turn registers
//     itself; while one is active the background classes (5 and 6) lease nothing
//     and running background handlers wait, for a bounded time, at Yield.
//     Policy covers what the queue cannot see (live turns are not jobs) and
//     the memory ceiling; the queue's own rule still covers waiting class 1 and
//     2 jobs.
//   - Idle unload (Touch, UseModel, Sweep): a model is unloaded, through the
//     provider's Unloader, only when it has been idle for Config.IdleUnloadAfter
//     AND loaded for Config.MinWarm AND is not pinned or in use. Stats counts
//     unloads and reloads so a soak run can fail on thrash.
//   - Disk pressure (DiskGuard): satisfies ingest.DiskPressure. Ingestion pauses
//     below a free-space floor and resumes at a higher one; a volume that cannot
//     be measured counts as pressure.
//   - Loop bounds (Governor.NewLoop): every agent or tool loop has a step and a
//     wall-clock limit.
//
// # No I/O in the queue transaction
//
// Policy runs inside the queue's lease transaction, so it only reads readings
// cached by Refresh (called by Run). A reading older than Config.StaleAfter is
// unknown, and unknown holds background work: Run must be running for ingestion
// to proceed. Admit re-reads the cheap process RSS itself when it is older than
// Config.ProbeInterval, but never calls the runtime: a hung runtime must not
// delay a live request.
//
// # Provider interface
//
// internal/providers has no memory or unload method. The governor consumes the
// optional MemoryReporter and Unloader interfaces, which a runtime adapter
// (adapters/localai) implements; RuntimeFromRegistry finds it behind the
// registered providers by type assertion. With no such adapter the governor
// still arbitrates and bounds loops, counts no runtime memory and unloads
// nothing.
//
// # Composition (done by the composition root, internal/cli)
//
//	gov, _ := resources.New(resources.Options{Config: resources.FromConfig(), Runtime: resources.RuntimeFromRegistry(reg)})
//	disk, _ := resources.NewDiskGuard(resources.DiskOptions{Config: resources.FromConfig(), Paths: []string{dataDir}})
//	pause := ingest.NewPause(disk, clk, 0, log)
//	queue := jobs.NewQueue(db, jobs.Options{Policy: gov.Policy(pause.Policy(nil)), ...})
//	go gov.Run(ctx)
//	// a live turn: release := gov.BeginInteractive(jobs.ClassTurn); defer release()
//
// Accounting is process-wide, not per tenant: per-tenant fairness is the quota
// module's job (EnqueueGuard, DequeuePolicy), which composes with this one.
package resources
