-- audit module, migration 0002: signed payload tombstones (master plan 19, ADR-0036; M5-26).
--
-- Erasure must not rewrite history silently. An event keeps its row, its envelope
-- (id, chain position, type, time, workspace, principal, session) and, above all,
-- its hash and prev_hash, so the chain around it and every signed checkpoint stay
-- exactly as they were. Only the payload goes: data is replaced by the fixed marker
-- '{"erased":true}', and a signed tombstone row records, in the same transaction,
-- which event it was (its hash), why, by whom and when. VerifyChain cannot recompute
-- the hash of an erased event, so it believes the tombstone's signature instead: the
-- tombstone signs the whole envelope and the event hash, and `audit verify-chain`
-- therefore still proves that nothing else changed.
--
-- Whether an event is erased is decided by the existence of its tombstone row, never
-- by what data looks like (data keys are free-form).
--
-- A tombstone is itself append-only. Its application_id, tenant_id and workspace_id
-- are those of the erased event (the scope rule of internal/store); chain_seq is the
-- erased event's position, so one position has one tombstone.

CREATE TABLE audit_tombstones (
    seq            INTEGER PRIMARY KEY,
    id             TEXT    NOT NULL UNIQUE,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    event_id       TEXT    NOT NULL UNIQUE,
    chain_seq      INTEGER NOT NULL CHECK (chain_seq >= 1),
    -- the exact canonical JSON that was signed
    body           TEXT    NOT NULL,
    -- the trust.Signature record as JSON
    signature      TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    UNIQUE (application_id, tenant_id, chain_seq)
) STRICT;

CREATE TRIGGER audit_tombstones_no_update
BEFORE UPDATE ON audit_tombstones
BEGIN
    SELECT RAISE(ABORT, 'audit tombstones are append-only');
END;

CREATE TRIGGER audit_tombstones_no_delete
BEFORE DELETE ON audit_tombstones
BEGIN
    SELECT RAISE(ABORT, 'audit tombstones are append-only');
END;

-- The append-only guard on events, with the one exception erasure needs: the payload
-- of an event is replaced by the erased marker, once, when a tombstone for that event
-- already exists, and nothing else about the row changes. The trigger keeps its name
-- (a database writer who wants to tamper drops it by name; the tests do the same).
-- Plain comparisons and EXISTS only: no SQL function is used (trusted_schema is OFF).
DROP TRIGGER audit_events_no_update;

CREATE TRIGGER audit_events_no_update
BEFORE UPDATE ON audit_events
WHEN NOT (
        NEW.seq = OLD.seq
    AND NEW.id = OLD.id
    AND NEW.application_id = OLD.application_id
    AND NEW.tenant_id = OLD.tenant_id
    AND NEW.workspace_id = OLD.workspace_id
    AND NEW.chain_seq = OLD.chain_seq
    AND NEW.event_type = OLD.event_type
    AND NEW.occurred_at = OLD.occurred_at
    AND NEW.principal_id = OLD.principal_id
    AND NEW.session_id = OLD.session_id
    AND NEW.prev_hash = OLD.prev_hash
    AND NEW.hash = OLD.hash
    AND NEW.data = '{"erased":true}'
    AND OLD.data <> '{"erased":true}'
    AND EXISTS (SELECT 1 FROM audit_tombstones t WHERE t.event_id = OLD.id)
)
BEGIN
    SELECT RAISE(ABORT, 'audit events are append-only');
END;
