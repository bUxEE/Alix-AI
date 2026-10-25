// Package quotas is the per-tenant resource accounting of ADR 0018 (master plan 25
// item 9): it keeps one tenant's request flood, session hoarding, bulk ingestion
// or storage from degrading every other tenant of a shared deployment. Section 17.1
// orders the priority classes; this package orders the tenants inside them.
//
// # What it limits
//
//	requests_per_minute   in-memory token bucket per tenant (refill + burst), exact integer arithmetic
//	concurrent_sessions   in-memory counting semaphore; leases expire, so a holder cannot leak a slot
//	ingest_bytes_per_day  SQLite counter per UTC day, charged in the transaction that writes the data
//	storage_bytes         SQLite gauge; Refund and SetStorage keep it honest
//
// Every quota is keyed by the application AND the tenant of a verified
// contracts.Scope, never by a bare tenant id and never by anything the request
// supplies: the same tenant id under two applications is two tenants, an invalid
// scope is refused with its own code, and Usage can only describe the caller's own
// tenant. There is no default tenant and no wildcard.
//
// # Typed refusals
//
// Exhaustion is a typed error, never a silent drop and never a success-looking
// result. The request rate returns rate_limited, retryable, with retry_after_ms.
// Everything else returns resource_exhausted naming the dimension, the limit, what
// is used and, for the daily counter, resets_at. A refused Admit takes nothing: no
// token, no slot, no byte, and no lease comes back with the error.
//
// # Defaults
//
// Limits are finite for every profile (experimental-4g, lite, balanced,
// performance). A missing or zero configuration inherits down to the profile's
// numbers; "unlimited" exists only as the explicit value -1. Per-application and
// per-tenant overrides inherit field by field.
//
// # Where it sits
//
// Admit belongs at the start of work: before a turn, a session, an ingestion or an
// upload begins. It must not refuse a commit the user has already confirmed
// (ADR 0018): a confirmed commit finishes or fails for its own reasons. Callers
// that write data in their own transaction use ChargeTx, so the charge and the data
// commit or roll back together, and exhaustion aborts the write.
//
// # What a restart does
//
// Buckets and leases live in memory and start full and empty: a restart refills
// every bucket (a bounded gift of one burst per tenant) and drops every lease (the
// sessions died with the process, so that is exact). The persisted counters survive
// a restart and roll over at UTC midnight.
//
// # Not here
//
// Per-class job caps and round-robin dequeue need internal/jobs types, which this
// package may not import (docs/architecture/module-map.md); the queue already
// offers the hooks (jobs.EnqueueGuard, jobs.DequeuePolicy with one candidate per
// tenant) and a composition root can adapt this package to them. Model tokens per
// day are deferred. See ADR 0018 for both.
package quotas
