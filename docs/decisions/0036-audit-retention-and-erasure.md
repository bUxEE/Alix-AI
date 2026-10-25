# 0036 - Audit retention and erasure: the payload becomes a signed tombstone, the chain keeps its hashes

- **Status:** accepted
- **Date:** 2026-10-03
- **Decided in task:** M5-26 (internal/audit, internal/actions, internal/cli/cmd_retention.go)

## Context
The data-lifecycle rules say retention is configurable per application and tenant, that a deleted tenant leaves tombstones, and that required audit evidence is not deleted by clearing history. The disaster-recovery drill (M6-04) found the
gap: the days a package declares (`retention.action_receipt_days`, `retention.audit_days`) were not enforced, and a receipt or an audit payload could not be erased at all, because the tables refuse update and delete.

The constraints that make this hard are the ones that make the audit log worth having:

- An event's hash is `sha256(canonical(envelope and data) || prev_hash)`, and the next event carries it as its `prev_hash`. The payload is inside the hash.
- Signed checkpoints (SEC-086) name a chain head and its hash. Rewriting any hash breaks every checkpoint after it, and an erasure that rewrote history silently would be indistinguishable from an attack.
- Receipts are an immutable ledger (SEC-087) and a preparation's receipt is read back by id; a dangling receipt id is treated as corruption.
- Erasure must never take away what a pending or uncertain action still needs: the audit events that name it and its receipts (SEC-046, SEC-047).

## Options
- **A. Delete old events (cut the chain).** Simple, and wrong: positions have a gap, every link after the cut breaks, checkpoints stop matching, and "nothing was removed except what the operator
  asked" can no longer be shown. Rejected.
- **B. Hash a digest of the payload instead of the payload (a v2 event format).** After an erasure the envelope would still verify by hash alone. It needs a new `format`, and it can only apply to events written after it:
  the existing chains would have to be rehashed, which is the silent rewrite this decision exists to avoid. It also keeps a payload digest next to the envelope. Not taken now; it is the better design for a v2 format if the log is
  ever redesigned, and nothing here stops it.
- **C. Encrypt every payload under a per-subject key and delete the key (crypto-shredding).** Needs a key per tenant and principal from the first event, a key store with its own backup and loss story, and does nothing for events
  already written. Rejected for now.
- **D. Keep the row, the envelope, the hash and the links; replace the payload by a constant marker; sign a tombstone that says who erased what, why and when. Taken.**

## Decision
Option D.

**What an erased event is.** The row stays at its chain position with the same `id`, `chain_seq`, `event_type`, `occurred_at`, `workspace_id`, `principal_id`, `session_id`, `prev_hash` and `hash`. Its `data` becomes the fixed
string `{"erased":true}`: nothing about the payload survives, not its size and not a digest. Whether an event is erased is decided by the existence of its tombstone row (`audit_tombstones`, migration 0002), never by the
shape of `data`; an append whose data is exactly the marker is refused, so the two cannot be confused.

**The tombstone** is a canonical-JSON document signed with the platform signing key (the key that signs checkpoints, accepted only under `alixai.platform`):
`format` (`alixai.audit-tombstone/v1`), `id` (`atmb_...`), `application_id`, `tenant_id`, `workspace_id`, `event_id`, `chain_seq`, `event_type`, `occurred_at_ms`, `principal_id`, `session_id`, `prev_hash`, `event_hash`,
`reason`, `operator`, `recorded_by`, `erased_at`, `expires_at`. It signs the whole envelope and the hash the event had. `erased_at` and `expires_at` repeat the signature record's times inside the signed bytes, as a checkpoint does, so the
"when" is authenticated. `operator` is the operator id the person typed (the same self-asserted id as every other operator command, see the operator guide) or the retention job's service principal; `reason` is a short text with no
personal data. `recorded_by` is the id of the `audit.erased` event of the same chain that records the erasure.

**Verification.** `VerifyChain` walks the chain as before. For an erased event it cannot recompute the hash, so it requires instead that: the row's data is the marker; the row's envelope and hash equal what the tombstone signed; the tombstone's
signature verifies under a registered, active platform key and has not expired; and the `audit.erased` event it names is on the chain. The links (`prev_hash`, contiguous positions, insertion order) and the checkpoint checks run exactly as
before, on the stored hashes, which did not move. `audit verify-chain` therefore still proves that nothing else changed. New problem kinds: `tombstone_mismatch`, `tombstone_invalid`, `tombstone_expired`,
`tombstone_unverifiable` (a Log with no verifier cannot believe a tombstone: fail closed) and `tombstone_unrecorded`. A database copy that predates the migration has no tombstone table and verifies as it always did.

**Erasure is one transaction** (`Log.EraseTx`): refuse without the signing key ("never skip the tombstone"); refuse an event that does not exist on the chain, is itself an `audit.erased` record, or **no longer hashes to what
is stored** (tombstoning a tampered event would sign its tampered hash and launder it); ask the guard which events must keep their payload; append one `audit.erased` event (kind, reason, operator, counts and position range, **no subject**);
then write each tombstone and replace each payload. The database enforces the same rule: the `audit_events` update trigger (same name as before, so a tamper test that drops it by name still drops it) allows exactly one change, `data`
to the marker, and only when a tombstone for that event exists and no other column differs; tombstones themselves refuse update and delete.

**What stays: the dependency refusal.** An audit event whose data names a preparation that is `prepared`, `confirmed`, `committing` or `uncertain` keeps its payload (`actions.PendingGuard`, read inside the erasing transaction), and a
pending preparation and its receipts are never removed (`actions.EraseFinished`). An erasure **request** (a tenant or principal) that touches any of them is refused whole, writes nothing, and says how many; a retention **sweep** skips them,
counts them and goes on. A preparation whose action ended (`committed`, `rejected`, `expired`, `invalidated`) is no longer depended on.

**Receipts** are removed, not redacted, and with their preparation: the preparation row holds the principal in `scope_json` and the field values in `fields_json`, and a receipt without its preparation would be a dangling id. A removed
preparation answers "no such preparation in this scope", so nothing it was can be confirmed, committed or reconciled again; its idempotency key is free and a new prepare mints a new one. The removal and its `deletion.receipts` record
(counts, kind, reason, operator; no subject) are one transaction.

**Retention in `serve`.** A class 6 maintenance job per application on the RAG stack's one queue and pool (`audit.retention`, its own service identity, a payload that names exactly one application), queued at start and daily. It reads
`retention.action_receipt_days` and `retention.audit_days` from the application's **active** installed package, verified as everywhere else; no active package, an unreadable one or a value below the schema's minimum of 1 means nothing is
removed (unknown is never read as expired). It removes finished preparations and receipts past the first, tombstones audit payloads past the second (tenant chains and the application chain; not the platform's), at most 20 batches of 200
events per target per run, and records itself in the chains. Without the platform signing key it still removes receipts and says that audit payloads are not erased. `alixai retention run` does one sweep now.

**Erasure on request.** `alixai retention erase -application A -tenant T -reason R -operator O [-principal P]` erases the payloads of every event of the tenant's chain (or, with `-principal`, of the events whose envelope names the
principal or whose payload holds the principal id as a JSON string value) and the finished preparations and receipts of the tenant (or principal). It first runs the whole request in transactions that are rolled back, so every refusal is found
before anything is written, then runs it. `-dry-run` stops after the first part.

**Amended (owner decision, 2026-10-10): erasure also takes the conversation history.** After the receipts, `retention erase` removes the tenant's (or principal's) transcript turns, deleted through `transcript_turns` so the
`history_fts` triggers drop them from search, the sessions' recent windows (`session_turns`), their lookup rows and the session rows (`sessions.EraseHistory`). Nothing references a session row, so it is removed rather than reduced. An
active session is ended first, as `End` leaves one, without voiding a preparation: every batch re-runs `actions.EraseFinished` in its transaction, which refuses while a pending or uncertain action matches, so none can be left confirmable. It goes in
transactions of at most 1000 rows, each recorded as `deletion.history_cleared` (kind, reason, operator, counts; no subject); `-dry-run` counts it, and a refused request removes none of it.

**What an auditor can and cannot learn afterwards.**
- *Can:* that an event existed at chain position N with a given type, time, workspace, principal id and session id; that its payload was erased, by which operator id, on what date, for which stated reason, and recorded by which `audit.erased` event;
  how many events and receipts an erasure covered; that the chain and every checkpoint taken before the erasure still match, so nothing else changed; that the platform key holder signed it.
- *Cannot:* the payload, its size, or a digest of it; the receipt body and the field values; who the person was beyond the identifier the envelope keeps.
- **The envelope keeps `principal_id` and `session_id`.** Erasing a principal removes what the principal did and said, not the principal's identifier from the envelope of those events (changing it would change the hash). The identifiers are
  the opaque ones the host application's context tokens carry; if one is personal data under your obligations, the host must issue pseudonymous principal ids. The tenant and workspace ids are the chain's key and also stay.
- **The kept hash lets someone confirm a guess.** `event_hash` is the hash of the full event; anyone who can reconstruct the complete envelope and payload exactly can check it against the hash. A payload with little entropy (a status flag, a known
  identifier) is therefore not anonymised by erasure; a free-text payload nobody can guess is.
- **It does not reach:** events on the platform chain that name a tenant; backups (a backup made before an erasure still holds everything, see the production guide, section 8); the host application's own records; RAG
  sources (their own deletion path); logs; and personal data a person typed into the `-reason` text.

## Consequences
- Easier: retention and erasure exist, are audited, and leave a chain that `audit verify-chain` still passes; an operator can show an auditor exactly what was removed and that nothing else moved.
- Harder: the one place that updates `audit_events` is `EraseTx`; a payload of an event is no longer always present, so a consumer that reads `Event.Data` must handle the marker (`Event.Erased`, and an export line carries its `tombstone`).
  `Event.HashValid` on an erased event is a conflict, not a pass: verify its tombstone.
- **Tombstones depend on the signing key like checkpoints do.** Revoking the platform key makes every tombstone it signed fail verification, and a tombstone cannot be re-signed (it lives in an immutable row). Rotate by retiring the old key, not
  revoking it; revoke only on compromise, after which erased events must be treated as unverifiable and restored from a backup that predates it. The signature expires after a hundred years. The key holder can forge a tombstone, which is the
  trust placed in the same key for checkpoints.
- **Erasure does not verify the whole chain**; run `audit verify-chain` before a request. It does refuse any single event that fails its own hash.
- Forbidden: erasing a pending or uncertain action's events, preparations or receipts; erasing without the signing key; erasing an `audit.erased` record; downgrading the binary below this migration once anything was erased (an older binary
  reports every erased event as a hash mismatch, which fails closed).
- A retention sweep never guesses: no declared retention, no removal. Operator overrides per application and tenant (which the manifest allows for) are not implemented; the package's declaration is the only source.
- Measured on the dev box (Windows 11, emulated, one chain of 10 000 events, batches of 1 000): erasing all of them took 3.4 s (about 340 microseconds each, dominated by one Ed25519 signature and two inserts); `VerifyChain` took 0.20 s
  before and 1.41 s after (one signature check and one trust-store read per erased event); a stored tombstone and its signature are about 870 bytes. A sweep is a class 6 job in batches of 200 events and adds no always-on service, so the
  sweep does not affect the 4 GB profile; nothing was measured on a target host. **`VerifyChain` is not bounded the same way:** it loads every tombstone of the chain it verifies into memory before the walk (roughly 1 KB per erased event in Go
  structures), so its memory and its time grow with the number of erased events, and after `audit_days` most of an old chain is tombstones. Verify one chain at a time (`-application` and `-tenant`); reading the tombstones in pages is the
  fix if a deployment's chains grow past what its host can hold, and nothing in the format prevents it.

## Reversal plan
The code is behind `audit.Log.EraseTx`, `actions.EraseFinished` and the `retention` commands; not calling them restores today's behaviour, and the serve provider (`rag_retention`) is the only scheduler. Migration 0002 only adds a table and relaxes the
update trigger to one exact exception; it can stay. What was erased cannot be put back from the database (that is its purpose): restore from a backup that predates it. If a v2 event format (option B) is ever introduced, existing tombstones remain valid
for the v1 chains they sit in.
