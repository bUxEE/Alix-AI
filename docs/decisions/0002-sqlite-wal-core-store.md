# 0002 — SQLite (WAL, FTS5) as the core store

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-03

## Context
Core state is sessions and goal state, action preparations and receipts, audit events, RAG
metadata, the lexical index and the job queue. It must be transactional and recoverable,
tenant-scoped on every row, and cheap enough that the baseline needs no PostgreSQL, Redis or
external vector database (§5.1). The same store should serve exact-term retrieval without a search
server.

## Options
- **SQLite in WAL mode with FTS5/BM25.** Embedded, one file, ACID, concurrent readers with one
  writer, built-in full-text search. No extra process and no resident memory beyond the page cache
  we configure. Public domain. Security: no network listener; access is file permissions plus the
  scoped query helpers.
- **PostgreSQL.** Strong concurrency and tooling, and a good scale-out path. A second resident
  server and a credential to manage; wrong for the minimum profile.
- **Embedded key-value store (bbolt, Badger).** Small and embedded, but no SQL, no full-text search
  and no per-table scoping; we would rebuild a query layer and an inverted index.
- **MongoDB.** The owner's default store. Not embeddable and a heavy resident service.

## Decision
SQLite in WAL mode is the store for all core metadata and state, with FTS5/BM25 for lexical
search. Settings: `busy_timeout`, `foreign_keys=ON`, `synchronous=NORMAL`, a bounded page cache,
mmap off by default, one writer connection plus a bounded reader pool. Every table carries
`application_id`, `tenant_id` and `workspace_id`, and every query filters on them. Migrations are
per module, embedded, and checksummed in `schema_migrations(module, version, checksum)`; a
checksum mismatch refuses to start. Online backup uses `VACUUM INTO`. The vector index is a
separate replaceable `VectorIndexProvider` and is not decided here (open decision §25.2).

**Measured (M0-05, 2026-10-02; Windows dev box, not emulated):** FTS5 is available on modernc v1.60.1 (SQLite 3.53.4);
default profile adds +39 to +40 MiB warm and +46 to +54 MiB peak over a 9 MiB baseline, small profile +17 to +19 MiB;
mmap stays off (256 MiB mmap raised the 100k-row peak from 63 to 212 MiB). Full figures in `docs/efficiency/M0-05.md`.

Measurement owed (now done, see above): RSS and throughput of open + migrate + 10k inserts, in
`docs/efficiency/M0-05.md`. FTS5 availability is proven by `TestFTS5Available` in the same task;
if it fails, work stops and this ADR is reopened.

## Consequences
- Easier: one file to back up, restore and inspect; the job queue ([0007](0007-sqlite-job-queue.md))
  and audit live in the same transactional store; no service to start.
- Harder: a single writer bounds write throughput and a long write transaction blocks other
  writers. Bulk work such as ingestion must batch and yield to the priority classes.
- Harder: corruption handling is ours: integrity check, metadata backup, rebuild of derived
  indexes (§18).
- Forbidden: SQL outside the owning module and `internal/store` helpers; a table without the three
  scope columns; a query without the scope predicates.

## Reversal plan
Callers use `internal/store` (`DB`, `Tx`, scoped helpers) and each module's own repository code.
Moving to PostgreSQL for larger deployments means a second implementation of those seams and a
per-module data export; per-module migrations make that incremental. The lexical index sits behind
the RAG index interface so FTS5 can be replaced by an engine-specific equivalent. §5.1 already
reserves provider contracts for PostgreSQL and Redis when measured scale requires them.
