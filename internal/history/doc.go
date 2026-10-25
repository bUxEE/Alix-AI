// Package history is the conversation-history API of master plan 14.2 and its
// data lifecycle of section 19: paginated transcript segments, full-text search,
// segment cleanup and export under authorisation, and the retention policy per
// application and tenant with its scheduled cleanup job.
//
// # What it reads and what it never touches
//
// The transcript is what the user reads back. It lives in module "transcript"
// (internal/sessions writes it in the same transaction as the session state) and
// is separate from the structured state the dialogue engine reads each turn:
// nothing on the model's context path reads it, so old turns are never replayed.
// This package is the API over the transcript for a host's users: it reads it
// for them and deletes it ONLY through the session store's ClearTranscript and
// PruneTranscript, after authorising the caller. It never writes a row of any other
// module: clearing history leaves the session state, prepared actions,
// committed receipts, audit events, application records and sources alone (master
// plan 19, SEC-087). The only thing that follows a deletion is the full-text
// index, which is this module's own and is kept in step by triggers on the
// transcript table, so a cleared turn cannot be found by a search afterwards. (The
// session store also drops the recent window of an ENDED session with its
// transcript; an active session's window is session state and stays.)
//
// # Authorisation
//
// Every operation is the host's to allow. The host puts the capability in the
// context token (contracts.Scope.CapabilityScopes) and the check is an exact
// match, like audit.export: no wildcard, no role stands in for it, and a missing
// capability is permission_denied before anything is read or deleted.
//
//	history.read       list the transcript segments of the caller's own sessions
//	history.search     full-text search over the caller's own transcript
//	history.clear      clear the transcript of one of the caller's own sessions
//	history.export     export the caller's own transcript
//	history.cleanup    delete every transcript turn of the WORKSPACE older than a time
//	history.retention  read and change the tenant's retention policy (Go API)
//
// Reading, searching, clearing and exporting are about the caller's own history:
// a row is visible only if its application, tenant and workspace are the
// token's and its principal is the caller. A session that belongs to another
// principal is scope_violation (the session store says the same for the same
// session), and search simply never returns another principal's rows.
// Cleanup is different in kind: PruneTranscript deletes across the whole
// workspace, so it has a capability of its own and the caller is authorised
// BEFORE it is called. An optional Authorizer (typically a policy decision) can
// refuse on top of the capability; it can never grant. The retention policy of
// a whole application is an operator action: it needs the Operator hook, and
// with none it is denied.
//
// # What the text is
//
// A user row is the user's own text, verbatim. An assistant row is model Markdown
// that the engine stored as it verified it, while the stream hub sanitises a
// response as a whole before a client sees it. So Segments, the snippet of a
// search hit and Export run assistant text through the same sanitiser
// (blocks.Validator.SanitizeResponse) on its way out, and what a host renders from
// history is never more than it would have rendered live. The stored row is not
// changed. A text the sanitiser refuses is not shown: the row is Withheld, empty
// and flagged, and history never falls back to the raw text.
//
// # Segments and pagination
//
// A segment page is a run of whole turns of one session, oldest first. A turn is
// the user's row and the assistant's row that share a turn number; a page never
// splits one, so the cursor (the number of the last turn returned) cannot skip or
// repeat a row.
//
// # Search
//
// The index is an FTS5 table over the transcript text, external-content: it
// holds terms only, and a hit is read back from the transcript under the scope
// predicate and the caller's principal. Every row also carries one indexed token
// for its application, tenant and workspace (migration 0002), and the MATCH
// requires the caller's, so a search visits and ranks only its own scope's rows,
// whatever the other tenants hold. Query text is never passed to FTS5 as
// syntax: it is reduced to quoted words, so the operators, column filters and
// wildcards are plain words, and a query can neither fail on syntax nor reach
// another scope. Results are bounded (terms, bytes, page, offset).
//
// The index is derived data: when it is damaged the upgrade (internal/upgrade)
// isolates it, which empties it, and records it in its quarantine. Search consults
// Deps.Quarantine and, while the index is isolated, fails with unavailable
// instead of returning an empty page: no hit means "nothing matched" only when the
// index could be searched (master plan 18). The error crosses the gateway as it is;
// a label on the page would not, since the API page has no field for it.
//
// # Retention
//
// A policy is a number of days. There is an application policy and a tenant
// policy; a tenant may keep history for no longer than its application allows,
// and the effective retention is the shorter of the two. With neither, nothing is
// ever deleted. Setting a policy and recording that it was set are one
// transaction (settings.changed).
//
// Applying a policy is a durable maintenance job (internal/jobs, class 6) per
// workspace, run under a declared service identity so that a revoked user's
// token cannot stop retention from running. ScheduleRetention enqueues the jobs
// for the workspaces that hold turns past their policy; the job resolves the
// policy again when it runs, so a policy changed after the job was queued is the
// one that applies. The composition root provides the service identity to the
// pool's ServiceAuthorizer, attaches Handle to the pool and calls
// ScheduleRetention once per Config.RetentionInterval.
//
// # Evidence
//
// A deletion appends deletion.history_cleared to the audit log BEFORE it deletes
// and is refused if the append fails, so history is never removed without a record
// of it; an export appends history.exported before any data is written. The
// events carry ids, counts and times, never transcript text.
package history
