-- actions module, migration 0001: the prepare / confirm / commit coordinator
-- (master plan 3 rule 8, 11.2, 11.4, 18; ADR 0006).
--
-- action_preparations holds one row per PreparedAction. The row is the whole
-- state machine:
--
--   prepared -> confirmed -> committing -> committed | rejected | invalidated | uncertain
--       \-----------------------^           (committing may fall back to the state
--        (mode none: no confirmation)        it came from when the host proves it
--                                            never applied the commit)
--   prepared | confirmed -> rejected | expired | invalidated
--
-- 'committing' is written, together with its audit event, in ONE transaction
-- BEFORE the adapter's Commit is called (SEC-087). A row found in 'committing'
-- after a restart is resolved by asking the host's Status, never by committing
-- again (SEC-046).
--
-- Times are unix milliseconds from the injected clock. The confirmation secret
-- is never stored: confirmation_mac is HMAC-SHA256 keyed by the secret over the
-- preparation id, principal, session and data versions it is bound to, so a
-- stolen database row cannot confirm anything and the secret verifies only
-- against exactly those four values.

CREATE TABLE action_preparations (
    seq               INTEGER PRIMARY KEY,
    id                TEXT    NOT NULL UNIQUE,
    application_id    TEXT    NOT NULL,
    tenant_id         TEXT    NOT NULL,
    workspace_id      TEXT    NOT NULL,
    principal_id      TEXT    NOT NULL,
    session_id        TEXT    NOT NULL,
    -- The whole scope the action was prepared under, for session continuity
    -- (identity.CheckContinuity) and for resolving a commit after a restart.
    -- The columns above are the indexed copy; the loader refuses a row where
    -- they differ.
    scope_json        TEXT    NOT NULL,
    capability_id     TEXT    NOT NULL,
    state             TEXT    NOT NULL CHECK (state IN
                        ('prepared', 'confirmed', 'committing', 'committed', 'rejected', 'expired', 'invalidated', 'uncertain')),
    risk_level        INTEGER NOT NULL CHECK (risk_level BETWEEN 0 AND 3),
    confirmation_mode TEXT    NOT NULL CHECK (confirmation_mode IN ('none', 'explicit', 'step_up')),
    -- '' unless confirmation_mode is 'explicit'.
    confirmation_mac  TEXT    NOT NULL,
    idempotency_key   TEXT    NOT NULL,
    -- The validated, projected field values the host saw at Prepare; the
    -- coordinator re-sends them to Validate and to the policy gateway at commit.
    fields_json       TEXT    NOT NULL,
    -- The PreparedAction exactly as announced. Its state member is not
    -- authoritative; the state column is.
    prepared_json     TEXT    NOT NULL,
    -- The freshness version of the host's Prepare result, compared at commit.
    snapshot_version  TEXT    NOT NULL,
    -- sha256 over the canonical data versions; part of the confirmation binding.
    data_digest       TEXT    NOT NULL,
    prepared_at       INTEGER NOT NULL,
    expires_at        INTEGER NOT NULL,
    -- Set once, by the single successful confirmation: a confirmation is single use.
    confirmed_at      INTEGER,
    committing_at     INTEGER,
    -- The state a 'committing' row returns to when the host proves the commit
    -- was not applied. Kept after the outcome so an 'uncertain' row can be
    -- reconciled.
    committing_from   TEXT    CHECK (committing_from IN ('prepared', 'confirmed')),
    commit_attempts   INTEGER NOT NULL DEFAULT 0 CHECK (commit_attempts >= 0),
    -- The current receipt, once an outcome exists.
    receipt_id        TEXT,
    updated_at        INTEGER NOT NULL
) STRICT;

-- One idempotency key per application, tenant and workspace.
CREATE UNIQUE INDEX action_preparations_key
    ON action_preparations (application_id, tenant_id, workspace_id, idempotency_key);

-- Recovery and expiry sweeps walk by state.
CREATE INDEX action_preparations_committing
    ON action_preparations (committing_at) WHERE state = 'committing';
CREATE INDEX action_preparations_open
    ON action_preparations (expires_at) WHERE state IN ('prepared', 'confirmed');

-- Receipts form a ledger: the first outcome of a preparation is one receipt,
-- and a later reconciliation of an 'uncertain' outcome adds another. They are
-- never edited, and history clean-up keeps them (SEC-087).
CREATE TABLE action_receipts (
    seq            INTEGER PRIMARY KEY,
    id             TEXT    NOT NULL UNIQUE,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    preparation_id TEXT    NOT NULL,
    status         TEXT    NOT NULL CHECK (status IN ('committed', 'rejected', 'queued', 'partial', 'uncertain')),
    receipt_json   TEXT    NOT NULL,
    issued_at      INTEGER NOT NULL
) STRICT;

CREATE INDEX action_receipts_preparation
    ON action_receipts (application_id, tenant_id, workspace_id, preparation_id, seq);

CREATE TRIGGER action_receipts_no_update
BEFORE UPDATE ON action_receipts
BEGIN
    SELECT RAISE(ABORT, 'action receipts are immutable');
END;
