// Package backup makes and restores signed, consistent copies of an AlixAI data
// directory (master plan 18 "SQLite/index corruption", 19 "Local backups remain
// operator-controlled", 22.5, 23 gate 9; SEC-075).
//
// # What a backup is
//
// One directory, <dir>/<backup id>/, written as <id>.partial and renamed only when
// complete, so an interrupted backup never looks like a finished one:
//
//	alixai.db           the database, copied with VACUUM INTO (store.DB.Backup): a consistent
//	                    snapshot taken while the service keeps serving, integrity-checked
//	tree/<name>/...     the file trees that hang off the database (default: files, packages),
//	                    copied AFTER the database; a blob a live row of the snapshot names that is
//	                    missing from the copy (compaction removed it meanwhile) refuses the backup
//	manifest.json       format, the signed statement and its signatures
//
// The signed statement lists every file with its SHA-256 and size and the tree names
// that were in scope. It is signed with an Ed25519 key through internal/trust, under the
// platform application, with kind "backup-manifest" inside the signed part so no other
// signed payload (a release manifest, an audit report) can be replayed as a backup.
//
// # What is and is not in it
//
// The database holds receipts, the audit log, sessions, settings and the derived indexes
// (full-text, vectors). Receipts and audit are not rebuildable and are therefore in
// every backup and in every restore. Derived data travels along but is rebuildable
// (internal/upgrade rebuilds a damaged index from its sources). The signing keys are
// NOT in a backup (a key inside the thing it authenticates authenticates nothing): a
// restore after the data directory is lost needs the public key supplied out of band.
//
// # Secure delete
//
// VACUUM INTO writes only live pages, so bytes of rows that were deleted (logical vector
// deletes, tombstoned sources and tenants) that still sit in free pages of the live file
// are NOT in a backup, and a restore replaces the live file with such a compacted copy. The
// live file is compacted by restoring, or by the store owner's vacuum; a live-file
// secure_delete policy belongs to internal/store and is not decided here.
//
// # Restore
//
// Restore verifies before it replaces anything: manifest syntax, the signature, that no
// file is missing, modified or unlisted, that no listed path escapes the backup, and an
// integrity check of the staged database. Only then are the live database (with its -wal
// and -shm, which must never meet a different main file), and the listed trees moved, as
// a unit, into <data dir>/.pre-restore-<id>-<time>. That directory is kept: it is how
// audit records and receipts written after the backup survive a restore. A failure while
// swapping puts the previous state back.
//
// Signatures expire (internal/trust accepts none without an expiry): backups are signed
// for ten years by default (Options.Validity).
package backup
