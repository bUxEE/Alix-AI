// Package sessions holds the durable session state of the dialogue engine (master
// plan 11.3): what the turn pipeline needs to continue a conversation across
// turns, without replaying the conversation.
//
// # What is stored
//
// State holds, per session:
//
//   - the active goal and the suspended goals (a topic switch sets one aside, a
//     nested goal returns to its parent), each with its typed slots and where
//     every value came from (user turn, default, entity resolution, choice,
//     correction), its resolved entities with the data version each relied on,
//     the pending clarification, and the prepared-action reference with its
//     expiry;
//   - a compact summary of the last directive;
//   - the last deterministic results (the records a result presented, in order, so
//     "the second one" resolves from state);
//   - the capabilities the package allows next;
//   - compact capsules of completed goals, which replace the goal and the turns
//     that led to it;
//   - the permissions of the last presented token.
//
// Everything is bounded (Config) and holds identifiers, field names and short
// clipped text. It never holds a context token, a confirmation secret, an
// idempotency key, a model prompt, a host record, or a slot value in a capsule the
// writer did not name. The preparation ID is a reference; the preparation itself
// lives in internal/actions.
//
// # Identity and binding
//
// A session is keyed by the session ID the verified context token carries. The
// core never mints one, and a row is found only through the application, tenant
// and workspace of the verified Scope, so another tenant or workspace presenting
// the same session ID gets a new, empty session. The Scope a session was first
// presented with is stored; every later presentation is checked against it with
// identity.CheckContinuity, so a session cannot change principal, tenant,
// workspace or session without a new authenticated context (master plan 10.2).
// The binding is for life: Save writes only the row the session was read from, and
// the database refuses any UPDATE of the identity columns. Roles and capability
// scopes may differ between presentations: Reconcile applies a narrower token.
//
// # The transcript is separate
//
// The full transcript lives in its own module and table ("transcript"), written in
// the same transaction as the state it belongs to, and read only through
// Transcript, TranscriptLen, ClearTranscript and PruneTranscript. The model's
// context is built from State and the short clipped window of RecentTurns; nothing
// on that path reads the transcript, so old turns are not replayed. Ending a
// session, or clearing the transcript, leaves the other alone (master plan 19),
// with one exception: once a session has ended its window is read by nothing, so
// clearing or pruning the transcript takes the ended session's window too.
//
// # Writes
//
// Every write compares the version it read (optimistic concurrency), so two
// writers cannot silently overwrite one another: the loser gets a conflict and
// re-reads. Save bounds the state, rewrites the lookup rows the invalidation
// sweeps use and the earliest deadline, and writes the turn, in one transaction.
//
// # Invalidation
//
// Application events, record revisions, permission changes, a change of user and
// timeouts can invalidate pending state (master plan 11.3). The Invalidation type
// describes one; State.Invalidate applies it in memory, and Store.Invalidate,
// Store.InvalidateWorkspace and Store.Reconcile apply it to stored sessions. In
// every case the preparations that lose their goal are invalidated through the
// action coordinator first, outside any transaction and under the identity each
// session is bound to, and the state is saved only if that worked, so a
// preparation is never left confirmable behind a session that forgot it and a
// failure leaves the state as it was, to be retried. Invalidation only voids,
// discards or clears: it never prepares, confirms or commits anything, and it
// calls no tool. End closes a session the same way.
//
// # Application events
//
// Store.ApplyEvent is the session side of the event adapter (adapters/events,
// master plan 9.3 item 4). The adapter authenticates a host's event (a signature
// by a key registered for the application) and derives the service scope from
// the signed payload alone; ApplyEvent then does what InvalidateWorkspace does
// for the sessions the event concerns, reports each session it changed, and
// tells each one through a Notifier under the scope the session is bound to: a
// notice that is a failed turn of two events made of core data (a stale error
// that says nothing was committed, then a failed done). The composition root
// (internal/cli/cmd_events.go) wires the Notifier to the stream hub and to the
// session-level announcement of GET /v1/sessions/{session_id}/events, because a
// client never learns the id of a turn it did not start. An event can only
// invalidate: the one effectful thing ApplyEvent is handed is an Invalidator, and
// it prepares, confirms and commits nothing.
//
// # For the dialogue engine
//
// The state methods (Suspend, Resume, Nest, Complete, Expire, Invalidate) return
// an Impact and never call the coordinator; the caller invalidates Impact.Voided
// and saves. Goal.SetSlot records a value with its origin, Goal.SetPrepared and
// Goal.ClearPrepared maintain the prepared-action reference and its expiry, and
// State.RecordResult and State.SetAllowedNext keep the last results and the allowed
// next actions. A caller that wants a narrower token applied calls Reconcile where
// it would call Open.
package sessions
