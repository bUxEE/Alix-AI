// Package events is the event adapter (master plan 9.3 item 4, 11.3): the intake
// through which a host application tells AlixAI that something changed, so that
// state AlixAI holds about it can be invalidated and the sessions it concerns can
// be told. It is not a capability transport and is not an AppAdapter: it has no
// Read, no Prepare and no Commit, and nothing it receives can reach one.
//
// # What an event can do
//
// An event can only invalidate. A verified event becomes a sessions.ApplicationEvent
// and goes to sessions.Store.ApplyEvent, which voids preparations through the
// action coordinator's Invalidate, drops what was derived from stale facts, clears
// questions built on them and notifies the sessions it changed with TurnEvents. It
// never prepares, confirms or commits an action and never calls a tool. This
// package cannot do those things even by mistake: it imports neither the
// coordinator nor an adapter nor the dialogue engine (TestImportsStayWithinMap),
// and the only effectful object it is given is a sessions.Invalidator, which has
// one method. Events never directly trigger an AI commit.
//
// # The wire format
//
// A delivery is one JSON object, over HTTP POST (Intake.Handler) or as the params
// of an MCP notification (Intake.DeliverNotification, method
// MCPNotificationMethod):
//
//	{ "event":      { ... },
//	  "signatures": [ { "key_id", "algorithm", "signature", "signed_at", "expires_at" } ] }
//
// The signature is the application's: one detached Ed25519 signature over the
// canonical JSON (RFC 8785, internal/trust) of the "event" member exactly as it
// arrived, by a key registered for the application in the trust store. A delivery
// carries exactly one signature (none is unauthenticated, several are refused):
// events are short lived, so there is no key-rotation overlap to carry, and the
// id of an event is remembered until the expiry of the one signature that
// authenticated it, which is only a bound if no other signature could take over
// after it expires. The event
// carries everything that decides what it does, so the signature covers all of it:
//
//	format          always "alixai.application-event.v1": a signature made for
//	                something else (a package manifest, an audit report) cannot be
//	                replayed as an event
//	id              the event's own ID; deliveries are deduplicated by it
//	type            the host's name for it ("invoice.paid"), a short label
//	kind            record_revision | application_event | permission_change |
//	                principal_change; an unknown kind is refused
//	application_id, tenant_id, workspace_id
//	                the scope the event is about; there is no other source of scope
//	entities        records concerned: entity_type, entity_id, version
//	capabilities    capabilities concerned
//	everything      the host cannot say more: all pending state in the workspace
//	principal_id    restrict to one principal's sessions (required for
//	                principal_change)
//
// Sign builds a delivery for a host.
//
// # What is checked, in this order
//
//  1. The body is bounded (MaxBodyBytes), one JSON value, with no unknown
//     members, and the HTTP method, content type and size are checked before any
//     parsing happens.
//  2. The signature: it must verify under a key registered for the application
//     the intake is bound to, not revoked, not expired, not dated in the future. An event with no signature, a bad one, an unknown key
//     or an expired one is refused and touches nothing: not the replay cache, not
//     a session. The signature may not be valid for longer than MaxValidity,
//     which bounds how long ids must be remembered.
//  3. Only then is the verified event decoded, strictly (an unknown member of a
//     signed event is refused, never ignored), and its application must be the
//     one the intake is bound to: an event for another application is a
//     scope_violation even when its signature is genuine.
//  4. The scope is built from the signed payload alone (deployment from the
//     configuration, a fixed service principal and role, the capability
//     sessions.EventCapability) and validated: a missing, padded or wildcard
//     tenant or workspace is refused. Nothing in the URL, headers or MCP
//     connection can name or widen a scope. Config.AllowScope can narrow it
//     further.
//  5. Replay: an event id already seen for the scope is acknowledged as a
//     duplicate and does nothing, whatever signature it carries (an event
//     re-signed with a fresh signature is still the same event).
//  6. The event is applied. If that fails the event is not recorded as seen: the
//     sessions are left as they were (sessions.Store keeps state unchanged when a
//     preparation cannot be voided) and the sender redelivers.
//  7. The event id is recorded as seen until the signature that carried it
//     expires.
//
// # Replay semantics
//
// Ids are kept in the identity replay cache (a durable table, so a restart does
// not reopen the window) under the key "evt:" plus the id, which cannot collide
// with a context token's nonce. The cache only offers Consume, which records an id
// and fails if it is already live. Checking before applying (so that a replay
// cannot void a preparation the user made after the original event) and recording
// after (so that a failed delivery can be retried) are both needed, so the check
// is a Consume with an expiry that has already passed: by the cache's contract a
// record that is no longer live is not a presentation, so the id is neither
// recorded nor burned, and an id that is live reports replayed. TestReplayProbe
// pins that against the real SQLite cache. Two deliveries of the same new event
// that overlap in time may both apply it; applying is idempotent (it voids what is
// already void, and a session it already changed is not changed again), and the
// second to record gets "replayed" and is reported as a duplicate.
//
// # Notifying sessions
//
// A session an event changed is told through sessions.Notifier, which the
// composition root wires to the turn-event stream: a stale error event and a
// failed done, made of core data only (see sessions.ApplyEvent). A notice that
// cannot be delivered is counted in the Receipt and never undoes the
// invalidation.
//
// # Wiring that belongs to a composition root
//
// This package starts no listener and registers no route. The gateway mounts
// Intake.Handler, and the MCP adapter's owner passes notifications whose method is
// MCPNotificationMethod to Intake.DeliverNotification: concrete adapters never
// import one another. Both are the composition root's job.
package events
