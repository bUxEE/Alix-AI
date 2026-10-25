# 0007 — SQLite-backed bounded job queue

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-03

## Context
Parsing, OCR, indexing, embeddings, bulk synchronization and maintenance are expensive and must
not starve interactive turns (rules 12, 13). They must survive restarts, be bounded, cancellable
and observable, and keep the scope they were started under (§10.2). The minimum profile has no
Redis.

## Options
- **Queue tables in the core SQLite store.** Durable and transactional with the data it works on
  (a job and its source row commit together), no extra process. Polling or in-process wake-up
  rather than push, and it shares the single writer ([0002](0002-sqlite-wal-core-store.md)).
- **Redis with a job library (asynq, BullMQ-style).** Mature features and higher throughput, but a
  resident service and a second persistence and backup story. Wrong for the minimum profile.
- **In-memory goroutine pool.** Cheapest, but jobs are lost on restart and cannot be inspected or
  retried durably.
- **Embedded broker (NATS JetStream and similar).** Durable and fast, but another component to
  size, secure and back up.

## Decision
Background work uses a SQLite-backed bounded durable queue in `internal/jobs`, with its own
migrations (module `jobs`). It provides enqueue with an idempotency key, lease with heartbeat
(an expired lease returns the job to `pending`), complete, fail with exponential backoff and
jitter, cancel, deadlines (a job past its deadline fails with `timeout`) and a terminal `failed`
state with an error code. Priority classes follow §17.1: (1) authentication, authorization and
commits, (2) active turn, (3) current RAG query, (4) ordinary application reads, (5) ingestion and
embeddings, (6) OCR, reindexing and maintenance. Each class has a bounded depth and a worker
limit, and lower classes yield: while a class 1 or 2 job is waiting, class 5 and 6 workers do not
lease new work. Every job stores its initiating scope and a `ScopeChecker` re-validates it before
the run; a revoked or expired scope fails the job with `permission_denied` unless a declared
service identity is authorized.

## Consequences
- Easier: nothing extra to run or back up; restart safety and observability come from the same
  store.
- Harder: throughput is bounded by the single SQLite writer, so jobs must batch writes and keep
  transactions short. Idle polling costs wake-ups; the interval is a measured setting.
- Forbidden: unbounded queues, jobs without a retained scope, and background work that bypasses
  the priority classes.

## Reversal plan
Callers depend on the `internal/jobs` API (enqueue, lease, complete, fail, cancel, status), not on
its tables. A Redis or broker backend for larger deployments is a second implementation of that
API selected in configuration (§5.1). In-flight jobs are drained, or re-enqueued from their
idempotency keys, at cutover; derived work such as indexes can be rebuilt.
