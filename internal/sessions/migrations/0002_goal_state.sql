-- sessions module, migration 0002: the full structured goal state of master plan
-- 11.3 and the indexes the invalidation sweeps need.
--
-- The state itself (active and suspended goals, slots with origins, resolved
-- entities, pending clarification, prepared-action reference and expiry, last
-- deterministic results, allowed next actions, completed-goal capsules) stays in
-- sessions.state_json, so a turn's state and its version change in one row.
-- Nothing here changes 0001; a row written by the minimal M1-14 state still
-- decodes, the new members are optional.
--
-- next_deadline is the earliest moment any part of the state lapses (goal idle
-- expiry, a suspended goal's expiry, a clarification's expiry, a prepared action's
-- expiry, a result's age). A timeout sweep reads it instead of every session.
-- It is maintained by Save, NULL when nothing can lapse.
ALTER TABLE sessions ADD COLUMN next_deadline INTEGER;

CREATE INDEX sessions_deadline
    ON sessions (application_id, tenant_id, workspace_id, next_deadline)
    WHERE next_deadline IS NOT NULL;

CREATE INDEX sessions_principal
    ON sessions (application_id, tenant_id, workspace_id, principal_id);

-- What a session's goals depend on, so an application event or a record revision
-- finds the sessions it concerns by index instead of scanning every state. One
-- row per distinct reference, rewritten by Save with the state it describes:
--   kind 'entity'     ref_type is the entity type, ref_id the record ID (resolved
--                     entities, offered choices, entities of the last results);
--   kind 'capability' ref_type is '', ref_id the capability ID of a goal.
-- The rows are identifiers only: no value, no label, no text.
CREATE TABLE session_refs (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    session_id     TEXT    NOT NULL,
    kind           TEXT    NOT NULL CHECK (kind IN ('entity', 'capability')),
    ref_type       TEXT    NOT NULL,
    ref_id         TEXT    NOT NULL
) STRICT;

CREATE UNIQUE INDEX session_refs_key
    ON session_refs (application_id, tenant_id, workspace_id, kind, ref_type, ref_id, session_id);

CREATE INDEX session_refs_session
    ON session_refs (application_id, tenant_id, workspace_id, session_id);

-- A session is bound to one application, tenant, workspace, principal and scope
-- for its whole life (master plan 10.2): the identity columns and the scope the
-- session was first presented with can never be rewritten, whatever the caller.
-- Another tenant presenting the same session ID is another session (the key
-- includes the scope), never this one with a new owner.
CREATE TRIGGER sessions_bound_for_life
    BEFORE UPDATE OF id, application_id, tenant_id, workspace_id, principal_id, scope_json ON sessions
BEGIN
    SELECT RAISE(ABORT, 'a session is bound to one scope for life');
END;
