-- sessions module, migration 0001: the minimal durable session state of the
-- dialogue engine (master plan 11.3; full goal state arrives in M3-01).
--
-- One row per conversation session. The session ID is the one the verified
-- context token carries: a session is never minted by the core, and a row is
-- found only through the application, tenant and workspace of the verified
-- scope. scope_json is the scope the session was bound to at its first turn;
-- every later presentation is checked against it (identity.CheckContinuity),
-- because a session cannot change principal, tenant or workspace without a new
-- authenticated context (master plan 10.2).
--
-- state_json holds the goal (capability, phase, collected slots with their
-- origins, pending clarification, prepared action reference) and a compact
-- summary of the last directive. It never holds a token, a confirmation
-- secret, an idempotency key or a transcript. version is compared on every
-- write, so two writers cannot silently overwrite each other.
--
-- Times are unix milliseconds from the injected clock.

CREATE TABLE sessions (
    seq            INTEGER PRIMARY KEY,
    id             TEXT    NOT NULL,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    principal_id   TEXT    NOT NULL,
    scope_json     TEXT    NOT NULL,
    status         TEXT    NOT NULL CHECK (status IN ('active', 'ended')),
    state_json     TEXT    NOT NULL,
    version        INTEGER NOT NULL CHECK (version >= 1),
    turn_count     INTEGER NOT NULL DEFAULT 0 CHECK (turn_count >= 0),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX sessions_key
    ON sessions (application_id, tenant_id, workspace_id, id);

-- The bounded recent turns of a session: what the prompt builder may replay and
-- what a later history API pages through. Text is clipped when written and old
-- turns are pruned, so the table cannot grow without bound inside one session.
CREATE TABLE session_turns (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    session_id     TEXT    NOT NULL,
    turn_id        TEXT    NOT NULL,
    n              INTEGER NOT NULL CHECK (n >= 1),
    role           TEXT    NOT NULL CHECK (role IN ('user', 'assistant')),
    act            TEXT    NOT NULL,
    body           TEXT    NOT NULL,
    created_at     INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX session_turns_order
    ON session_turns (application_id, tenant_id, workspace_id, session_id, n, role);
