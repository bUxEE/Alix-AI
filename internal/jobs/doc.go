// Package jobs is AlixAI's durable, bounded job queue (master plan sections 3
// rule 13, 6, 10.2 and 17.1; ADR 0007). Parsing, OCR, indexing, embeddings,
// bulk synchronisation and maintenance run as jobs held in SQLite through
// internal/store, so they survive a restart, are bounded, can be cancelled and
// can be inspected, with no Redis in the minimum profile.
//
// # Two layers
//
// Queue is the store-facing state machine and starts no goroutines: Enqueue,
// Get, Cancel, Lease, Heartbeat, Complete, Fail, Release, Reap, Prune, Status.
// Pool runs handlers over a Queue with one bounded set of workers per priority
// class. Callers that only enqueue (a request handler) hold a Queue; the process
// that runs work builds a Pool. Both take every time reading from the injected
// clock.Clock, so tests move a clock.Fake instead of sleeping.
//
// # Job life
//
//	pending --Lease--> running --Complete--> succeeded
//	   ^                  |  \--Fail (retryable, attempts left, backoff)--> pending
//	   |                  |   \-Fail (otherwise)--> failed (error code recorded)
//	   +--lease expired---+    \-Cancel while running--> cancelled
//	pending --Cancel--> cancelled        pending --deadline passed--> failed (timeout)
//
// A job is registered by type (Queue.Register): the type fixes its priority
// class and attempt limit, so a producer cannot promote its own work. Enqueue
// is idempotent per scope and key, bounded per class (resource_exhausted when
// the class is full) and passes through an optional EnqueueGuard first.
//
// # Priority classes and yielding
//
// The six classes of section 17.1 are Class values 1 to 6. Each has its own
// worker limit and queue depth (Config.Class1..Class6). Lease serves the most
// urgent class with ready work and orders inside the class FIFO, or through a
// DequeuePolicy. Lower classes yield: while any job of class 1 or 2 is waiting,
// classes 5 and 6 lease nothing (Config.InteractiveUpTo and BackgroundFrom).
//
// # Failure, retry and crash safety
//
// A handler error is classified by its contracts code: retryable codes retry
// with exponential backoff and jitter up to the type's attempt limit, anything
// else (a plain error included) fails the job at once with that code, and a job
// ends failed with the code stored. A job whose deadline passes fails with
// timeout instead of running, and a running job's context is cancelled at its
// deadline. Cancelling a running job cancels its context. A lease that is not
// heartbeated expires and Reap returns the job to pending; a job whose lease
// expires on its last attempt fails with uncertain_outcome, so a job that kills
// its worker cannot loop forever. Delivery is at least once: handlers must be
// idempotent.
//
// # Scope (section 10.2)
//
// Every row carries application, tenant and workspace, and the whole initiating
// Scope is stored with the job. Before a handler runs the Pool asks the
// ScopeChecker; a revoked or expired scope fails the job with permission_denied
// and the handler is never called, unless the job declares a service identity
// that the ServiceAuthorizer accepts. NewPool refuses to start without a
// checker. Tenant-facing methods (Enqueue, Get, Cancel, ScopedStatus) are
// confined to the caller's scope; the dispatcher side (Lease, Reap, Prune,
// Status) is deployment-wide by nature and must not be reachable from a tenant
// request.
//
// # Hooks for later modules
//
// EnqueueGuard (ADR 0018) runs inside the enqueue transaction with exact
// per-tenant counts and may refuse with rate_limited or resource_exhausted.
// DequeuePolicy lets the quota module order a class fairly across tenants.
// ScopeChecker and ServiceAuthorizer come from the identity module.
//
// # Operating notes
//
// Throughput is bounded by the single SQLite writer: each Enqueue, Lease and
// Complete is one short write transaction, so batch work into fewer, larger
// jobs rather than many tiny ones. An idle Pool costs one wake-up per
// Config.PollInterval and one read-only query per class that has handlers; it
// takes no write lock until there is work. Payloads are stored in clear text
// and capped (Config.MaxPayloadBytes): carry references, never credentials.
package jobs

import (
	"embed"

	"github.com/buxee/alix-ai/internal/store"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() { store.RegisterMigrations("jobs", migrations) }
