-- audit module, migration 0001: the tamper-evident audit log (master plan 4.1,
-- 16.4 "audit tampering", 19; SEC-085, SEC-086, SEC-087).
--
-- One table of events and one of signed chain-head checkpoints. Events are
-- append-only and hash-chained per chain, a chain being one (application_id,
-- tenant_id) pair: tenant_id = '' is the application-level chain of an
-- application (package lifecycle, integration-audit runs), and the application
-- 'alixai.platform' holds the platform's own events. A real tenant id is never
-- empty (contracts.Scope refuses it), so the two cannot collide.
--
-- workspace_id is part of every event and of its hash, but NOT of the chain
-- key: a chain spans all workspaces of its tenant. Callers' reads (export) are
-- confined to application, tenant AND workspace through store.ScopedWhere; only
-- the operator paths (append, verify, checkpoint) address a whole chain.
--
-- Times are unix milliseconds from the injected clock. Hashes are lower-case
-- hex SHA-256: hash = sha256(canonical(event without hash and prev_hash) ||
-- prev_hash). Every column except the global rowid (seq) is covered by it.

CREATE TABLE audit_events (
    -- Physical insertion order across all chains; not hashed. Verification
    -- requires it to rise with chain_seq inside a chain, so a re-inserted row
    -- is seen.
    seq            INTEGER PRIMARY KEY,
    id             TEXT    NOT NULL UNIQUE,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    -- 1-based position inside the chain.
    chain_seq      INTEGER NOT NULL CHECK (chain_seq >= 1),
    event_type     TEXT    NOT NULL,
    occurred_at    INTEGER NOT NULL,
    principal_id   TEXT    NOT NULL,
    session_id     TEXT    NOT NULL,
    -- canonical JSON object, already redacted
    data           TEXT    NOT NULL,
    prev_hash      TEXT    NOT NULL CHECK (length(prev_hash) = 64),
    hash           TEXT    NOT NULL CHECK (length(hash) = 64),
    -- Fork guards: one event per position and one successor per event.
    UNIQUE (application_id, tenant_id, chain_seq),
    UNIQUE (application_id, tenant_id, prev_hash)
) STRICT;

-- Caller-scoped reads (export) walk one workspace in insertion order.
CREATE INDEX audit_events_scope ON audit_events (application_id, tenant_id, workspace_id, seq);

CREATE TRIGGER audit_events_no_update
BEFORE UPDATE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit events are append-only');
END;

CREATE TRIGGER audit_events_no_delete
BEFORE DELETE ON audit_events
BEGIN
    SELECT RAISE(ABORT, 'audit events are append-only');
END;

-- Signed chain-head checkpoints, kept apart from the events. A checkpoint
-- states: "the chain of (application_id, tenant_id) had head_seq events and
-- its head hash was head_hash", signed with a platform key from internal/trust
-- (SEC-086). The workspace_id column exists only to satisfy the scope rule of
-- internal/store and is always '': a checkpoint covers the whole chain.
--
-- A database writer can delete these rows too; the defence against that is a
-- copy held elsewhere (ExportCheckpoint, VerifyOptions.Expected). More than
-- one checkpoint may exist for the same head (a refresh after expiry).
CREATE TABLE audit_checkpoints (
    seq            INTEGER PRIMARY KEY,
    id             TEXT    NOT NULL UNIQUE,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL CHECK (workspace_id = ''),
    head_seq       INTEGER NOT NULL CHECK (head_seq >= 1),
    -- the exact canonical JSON that was signed
    body           TEXT    NOT NULL,
    -- the trust.Signature record as JSON
    signature      TEXT    NOT NULL,
    created_at     INTEGER NOT NULL
) STRICT;

CREATE INDEX audit_checkpoints_chain ON audit_checkpoints (application_id, tenant_id, head_seq, seq);

CREATE TRIGGER audit_checkpoints_no_update
BEFORE UPDATE ON audit_checkpoints
BEGIN
    SELECT RAISE(ABORT, 'audit checkpoints are append-only');
END;

CREATE TRIGGER audit_checkpoints_no_delete
BEFORE DELETE ON audit_checkpoints
BEGIN
    SELECT RAISE(ABORT, 'audit checkpoints are append-only');
END;
