-- transcript module, migration 0001: the conversation transcript (master plan
-- 11.3 and 19).
--
-- The transcript is what the user sees as history. It is stored separately from
-- the model's context: sessions.state_json and session_turns hold what the
-- dialogue engine reads each turn (structured state and a short clipped window),
-- and nothing on that path reads this table. A turn is appended here in the same
-- transaction as the session state it produced, so the two never disagree about
-- how many turns happened, but clearing history (retention, a user request, a
-- tenant deletion) deletes rows here and leaves the session state, prepared
-- actions, receipts and audit evidence alone.
--
-- body is the turn's text, clipped at write time to a generous bound so one turn
-- cannot grow the table without limit; clipped says whether that happened.
-- principal_id is the session's principal, so a history API can authorise a
-- reader against it. Times are unix milliseconds from the injected clock.
CREATE TABLE transcript_turns (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    session_id     TEXT    NOT NULL,
    principal_id   TEXT    NOT NULL,
    turn_id        TEXT    NOT NULL,
    n              INTEGER NOT NULL CHECK (n >= 1),
    role           TEXT    NOT NULL CHECK (role IN ('user', 'assistant')),
    act            TEXT    NOT NULL,
    body           TEXT    NOT NULL,
    clipped        INTEGER NOT NULL DEFAULT 0 CHECK (clipped IN (0, 1)),
    created_at     INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX transcript_turns_order
    ON transcript_turns (application_id, tenant_id, workspace_id, session_id, n, role);

CREATE INDEX transcript_turns_age
    ON transcript_turns (application_id, tenant_id, workspace_id, created_at);
