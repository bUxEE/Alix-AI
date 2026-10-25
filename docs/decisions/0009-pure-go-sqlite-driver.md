# 0009 — Pure-Go SQLite driver: `modernc.org/sqlite`

- **Status:** accepted
- **Date:** 2026-10-02
- **Decided in task:** M0-03

## Context
[0002](0002-sqlite-wal-core-store.md) chose SQLite with FTS5; this ADR chooses the Go driver. The
founding design does not name one. The build must be `CGO_ENABLED=0`: the dev box has no C compiler,
the project cross-compiles for Linux, Windows and macOS (x86-64 and ARM64) and ships static
binaries without a per-target C toolchain, and the supply-chain goal is reproducible builds
(§16.3). FTS5 is a hard requirement. The driver must also keep the 4 GB direction possible, so
resident memory counts.

## Options
- **`modernc.org/sqlite`** (BSD-3-Clause). SQLite machine-translated from C to Go, no cgo, FTS5
  included. Works with `database/sql`. Costs: the translation lags upstream SQLite by some
  releases; it is generally slower than the C library (the cost for our workload is measured in
  M0-05, see Decision); and SQLite security fixes reach us only through a module upgrade.
- **`github.com/mattn/go-sqlite3`** (MIT). The reference cgo binding to the C library: fastest and
  most widely deployed. Requires `gcc` for every build target, which breaks `CGO_ENABLED=0`,
  cross-compilation and the dev box, and produces non-static binaries unless extra flags are used.
- **`github.com/ncruces/go-sqlite3`** (MIT). SQLite compiled to WebAssembly and run inside the pure
  Go `wazero` runtime. No cgo. Adds a WASM runtime layer (memory and start-up cost to be measured)
  and its own VFS and extension model, so behaviour such as FTS5 must be confirmed in our build
  configuration.

## Decision
The driver is `modernc.org/sqlite`, used through `database/sql` and imported **only** by
`internal/store`. The driver name is `sqlite`, not `sqlite3`. Everything else sees `store.DB`,
`store.Tx` and the scoped query helpers.

**Measured (M0-05, 2026-10-02):** `TestFTS5Available` and `TestFTS5BM25Ranking` pass on modernc v1.60.1. ncruces/go-sqlite3
matched modernc on memory but inserted about 15 % slower and needs per-connection FTS5 registration; mattn/go-sqlite3
could not be built on the dev box (no gcc). Details in `docs/efficiency/M0-05.md`.

Measurements owed, in M0-05 (done, see above):

1. `TestFTS5Available` creates an FTS5 virtual table and runs a `MATCH`; `TestFTS5BM25Ranking`
   checks ranking. If FTS5 is not available with this driver, work stops and this ADR is reopened
   before anything builds on it.
2. `docs/efficiency/M0-05.md` records RSS of open + migrate + 10k inserts, the cost of the
   configured page cache and insert/FTS throughput, against the Lite and 4 GB budgets.

The driver is revisited if either measurement fails or if write-heavy ingestion (M2-08) shows the
translation overhead is the bottleneck rather than the single writer.

## Consequences
- Easier: `CGO_ENABLED=0` everywhere; one-command cross-compilation; static binaries; no gcc on
  dev boxes or CI images; reproducible builds.
- Harder: `-race` still needs cgo, so race tests run only in the Linux CI job. SQLite CVEs are
  fixed by bumping `modernc.org/sqlite`, so dependency scanning (`govulncheck`) and a release
  watch on that module are part of the vulnerability policy.
- Harder: pragmas and extensions available differ from the C build; they are set once in
  `store.Open` and tested there, not scattered.
- Forbidden: importing the driver outside `internal/store`; any cgo dependency in the core.

## Reversal plan
The database file is standard SQLite, so swapping drivers needs no data migration.

1. The driver import and `sql.Open` driver name are confined to `internal/store`
   (rule 5 in [module-map.md](../architecture/module-map.md)).
2. Replace the import and driver name there (`sqlite3` for mattn; the ncruces `driver` package for
   wasm) and move connection pragmas that use the driver's DSN syntax.
3. Re-run the `internal/store` tests (WAL, FTS5, migrations, backup, concurrency) and the module
   suites that depend on it; no schema change.
4. Moving to mattn also means re-enabling cgo in release builds and CI, which supersedes
   [0001](0001-go-core-service.md)'s pure-Go constraint and needs its own ADR.
