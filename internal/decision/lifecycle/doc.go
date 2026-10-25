// Package lifecycle supervises a decision scorer: it loads it on demand, probes its
// health, unloads it when idle, restarts it with backoff, and swaps model revisions in
// stages with automatic rollback (master plan 8.3, 8.5, 17.2, 18; docs/operations/
// decision-layer.md sections 2 to 5).
//
// The decision layer is optional, bounded, non-generative and non-authoritative, and so
// is this package: it never reads, ranks or acts on what a scorer says, it only keeps
// the scorer alive within its resource budget. A Supervisor IS a
// decision.DecisionModelProvider, so replacing the scorer (CLM-v0.1-8B for a smaller
// one, one runtime for another) changes the Loader and the Revision and nothing the
// Engine, the contract or any caller sees. With the scorer unloaded, backing off,
// rejected by the resource gate or rolled back, the Engine records a typed fallback
// and the deterministic route plus the controller answer as they always do.
//
//   - Supervisor (supervisor.go): demand-load, readiness, idle unload after a minimum
//     warm period, restart with doubling backoff, resource-gate rejection.
//   - ResourceGate: admit-or-reject only. It has no way to evict or pause the
//     conversational controller, retrieval or speech workers; the real governor
//     (internal/resources) is wired behind it by the composition root.
//   - Update (update.go): hash verification, staged load, health probe, promotion
//     gate, atomic switch, probation and rollback to the previous revision.
//   - Metrics (metrics.go): a small recording Registry (latency histogram, fallback
//     reasons, acceptance, controller calls avoided, cache hit rate, evictions,
//     memory). Labels are provider, model, revision and profile; nothing identifies a
//     tenant or a user, and nothing is pushed anywhere.
//
// No goroutines are started and the injected clock is the only source of time; the
// owner calls Supervisor.Tick periodically.
package lifecycle
