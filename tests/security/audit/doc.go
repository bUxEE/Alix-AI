// Package auditsec is the audit-log security suite of master plan 16.4 b14 and 19: the log cannot be
// truncated or rewritten unseen, and nothing consequential happens without its record.
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-086  TestSecAuditTailTruncationDetected
//	SEC-087  TestSecEveryActionTransitionAudited
//
// # Tail truncation and rewrite (SEC-086, gap G6)
//
// A real audit log signs chain heads with a platform key registered in the trust store. A head is exported
// (audit.Log.ExportCheckpoint), serialised as JSON and read back strictly, as a report or a backup manifest
// would carry it, and held outside the database. The attacker then has write access to the database: the
// append-only triggers are dropped, the newest N events are removed, or the whole chain is rewritten with
// every hash recomputed by the production hash function, the stored checkpoints are deleted, and, in one case,
// the attacker registers their own platform key and signs a head that fits the forged chain. Verification
// against the held head (audit.VerifyOptions.Expected) reports the truncation or the rewrite in every case.
// Controls: the intact chain verifies against its head before and after it grows; the database refuses a
// deletion until its triggers are dropped; each forged chain verifies clean without the held head (the
// attack is real); a head from an unknown key, an altered head and another chain's head are not believed.
//
// # Every transition is audited (SEC-087, gap G10)
//
// The real action coordinator runs over the real audit log, policy gateway and the in-memory host. Eleven
// flows reach every state (prepared, confirmed, committing, committed, rejected, expired, invalidated,
// uncertain, and the reopening of a commit that never arrived); after each operation the stored state is read
// back, and from inside the adapter's Commit the stored state and the audit chain are read, so the events are
// compared with what the preparation really did. Every policy decision (allow or deny) is in the chain once, in
// order. A failed append of the committing event aborts the commit: the adapter is never called, the action
// stays where it was with a typed unavailable error, and once the log is back it commits. A failed append of
// any other transition leaves the state where it was. Controls: the same checks, over an appender that
// swallows or delays the committing event, report it.
package auditsec
