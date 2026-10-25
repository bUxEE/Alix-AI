// Package upgrade runs staged upgrades of an AlixAI installation with automatic
// rollback, recovers from interrupted ones, and rebuilds damaged derived indexes
// (master plan 16.3 "staged upgrades, compatibility checks, rollback", 18, 22.5,
// 23 gate 9; SEC-072 to SEC-075).
//
// # The staged upgrade (Upgrader.Apply)
//
//  1. The signed release manifest is verified (ReleaseVerifier): signature by a
//     key registered for the platform application, kind and application inside the
//     signed part, and every pin the upgrade uses present. tools/release writes the
//     manifest and this package reads it with the same code: internal/release holds
//     the format, the signer and the verifier, and ReleaseVerifier only reduces the
//     verified manifest to what an upgrade needs. The published TEST-ONLY key
//     verifies only when the key lookup was opened with an explicit allow.
//  2. A release that is not newer than the installed one is refused: without TUF
//     style metadata (docs/operations/release.md section 4) a replayed signed
//     manifest is a downgrade attack. Going back is Rollback, never Apply.
//  3. The runtime set the upgrade would leave running (LocalAI and its backends)
//     must be one the release was qualified for (Release.Qualify), or the upgrade
//     is refused before anything changes (SEC-072).
//  4. Damaged derived indexes are rebuilt, then a signed backup is taken
//     (internal/backup) of the state the upgrade starts from.
//  5. The migrations are applied, the runtime is switched, the health probe runs
//     (store opens, indexes check out, integrity check, the caller's probe, the
//     runtime that runs is the one asked for and is qualified).
//  6. Any failure after the backup restores the database and the runtime from it.
//     What was in place is kept in <data dir>/.pre-restore-*.
//
// Progress is journaled in <data dir>/upgrade/journal.json, outside the database
// because the database is what a rollback replaces. After a crash, Recover rolls
// the upgrade forward (every step is idempotent: each migration is its own
// transaction recorded in schema_migrations) and falls back to the backup if it
// cannot complete.
//
// # What the upgrade does not do
//
// It does not install binaries or images and it imports no runtime code: Runtime
// is the seam a composition root fills (FileRuntime records the desired set for the
// deployment to read). The API is complete without a live LocalAI.
//
// # Derived data
//
// Full-text and vector indexes are derived and rebuildable; receipts and the audit
// log are not and are never part of a repair (derived.go). The FTS5 tables are
// found in the database and the vector index (vector.go) is wired in by New, so
// the probe checks both. An isolated index is recorded in the quarantine, and code
// that serves from one consults Quarantine.Has (internal/rag/query and
// internal/history do) and labels what it returns reduced recall instead of
// returning nothing in silence. A vector index is rebuilt by embedding its chunks
// again, which only the serving process can do: unless Config.VectorRebuild is
// set it stays quarantined (DerivedReport.Pending) until that job releases it.
package upgrade
