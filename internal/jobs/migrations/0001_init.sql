-- jobs module, migration 0001: the durable bounded job queue (ADR 0007, master
-- plan sections 6, 10.2 and 17.1). One row per job; the row is the whole state
-- machine: pending -> running -> succeeded | failed | cancelled, with retries
-- going running -> pending.
--
-- Times are unix milliseconds from the injected clock. FIFO order inside a
-- class is the rowid (seq): it is monotonic, needs no timestamp, and stays
-- exact when the clock does not move (tests) or two jobs share a millisecond.

CREATE TABLE jobs (
    seq              INTEGER PRIMARY KEY,
    id               TEXT    NOT NULL UNIQUE,
    application_id   TEXT    NOT NULL,
    tenant_id        TEXT    NOT NULL,
    workspace_id     TEXT    NOT NULL,
    type             TEXT    NOT NULL,
    class            INTEGER NOT NULL CHECK (class BETWEEN 1 AND 6),
    state            TEXT    NOT NULL CHECK (state IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')),
    payload          BLOB    NOT NULL,
    idempotency_key  TEXT,
    -- The whole initiating scope (principal, roles, capability scopes, session)
    -- as JSON, for the ScopeChecker. application_id, tenant_id and workspace_id
    -- above are the indexed copy; the loader refuses a row where they differ.
    scope_json       TEXT    NOT NULL,
    service_identity TEXT,
    attempts         INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts     INTEGER NOT NULL CHECK (max_attempts >= 1),
    run_at           INTEGER NOT NULL,
    deadline_at      INTEGER,
    -- A fresh token per lease: heartbeat, complete and fail must present it, so
    -- a worker whose lease expired cannot finish a job that was leased again.
    lease_token      TEXT,
    lease_until      INTEGER,
    cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
    error_code       TEXT,
    error_message    TEXT,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    started_at       INTEGER,
    finished_at      INTEGER
);

-- An idempotency key is unique inside one application, tenant and workspace,
-- never across them: two tenants may use the same key.
CREATE UNIQUE INDEX jobs_idempotency
    ON jobs (application_id, tenant_id, workspace_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- Waiting work: priority class, then FIFO (seq is the rowid). run_at is kept in
-- the index so a probe for ready work, or the depth count, skips delayed retries
-- without reading their rows. Only pending rows are indexed, so it stays small.
CREATE INDEX jobs_ready ON jobs (class, seq, run_at) WHERE state = 'pending';

-- Pending jobs that carry a deadline, for expiring them without a table scan.
CREATE INDEX jobs_deadline ON jobs (deadline_at) WHERE state = 'pending' AND deadline_at IS NOT NULL;

-- Running jobs by lease expiry, for returning abandoned work to pending.
CREATE INDEX jobs_lease ON jobs (lease_until) WHERE state = 'running';

-- Queued-plus-running jobs per tenant and class, for the enqueue guard.
CREATE INDEX jobs_tenant_active ON jobs (application_id, tenant_id, class) WHERE state IN ('pending', 'running');

-- Finished jobs by finish time (retention) and, covering, by class and state
-- (status counts).
CREATE INDEX jobs_finished ON jobs (finished_at, state, class) WHERE state IN ('succeeded', 'failed', 'cancelled');
