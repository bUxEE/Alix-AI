// Package actionssec is the actions security suite of master plan 11.4, 16.4
// bullets 4 and 5, 18 and 22.5: what the prepare / confirm / commit coordinator
// (internal/actions) does when the process dies in the middle of an action.
//
// The suite runs the real coordinator over a real persistent store (SQLite in a
// temporary directory), the real audit log and policy gateway, and the
// in-memory host application (internal/adapters/fake) behind adapters.Wrap, which
// is how the composition root wires them. "The process dies" is simulated at each
// boundary of the lifecycle by stopping the goroutine that is inside the call
// (runtime.Goexit, which no recover sees, so nothing after that point runs: no
// Status check, no outcome write) and then closing the store and building a NEW
// coordinator over the same database file, against the SAME host, as a restarted
// service would. The host outlives the service, so it keeps what it applied.
//
// # Tests and the backlog (docs/security/test-backlog.md)
//
//	SEC-046  TestSecKillDuringPrepareAndCommit
//
// # Boundaries covered
//
//   - during Prepare, in the adapter: no preparation is stored, nothing is
//     committed;
//   - after Prepare was stored: the preparation is not confirmable and not
//     committable once it has expired, and an untouched one is expired by the sweep;
//   - after the confirmation was accepted, before Commit started: the commit
//     proceeds once and the confirmation cannot be used again;
//   - after the "committing" row was written, before the host answered (and the
//     host never received the commit): resolved by Status, not by a blind commit,
//     and retryable only under the same idempotency key;
//   - after the host applied the commit, before the receipt was stored: the
//     receipt the host recorded is adopted, the commit is not repeated;
//   - the same two, where Status cannot decide (denied, malformed, unreachable or
//     the adapter is gone): the outcome is uncertain and the user-facing result is
//     typed (uncertain_outcome, receipt status uncertain), never success-looking,
//     and a later reconciliation still does not commit again.
//
// # Controls
//
// Every "no second commit" assertion counts the commits per idempotency key twice:
// at the host (what it received) and at the adapter the new coordinator calls
// (what recovery dispatched). The counters are shown to see a violation by
// planting one: a blind recovery that re-sends the commit it found in the
// "committing" row to a host whose commit is not idempotent produces two effects
// and the detector reports two commits for the key. The Status-reports-committed
// case adopts the host's receipt, and the Status-reports-not-committed case
// leaves the preparation retryable, so recovery is not simply "do nothing".
package actionssec
