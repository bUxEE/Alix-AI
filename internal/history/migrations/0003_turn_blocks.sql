-- history module, migration 0003: durable verified rich blocks per turn for
-- exports after the stream hub has forgotten the turn (retention / restart).
--
-- blocks_json is a JSON array of conversation.RichBlock values already accepted
-- by blocks.Validator (the same set BlockEvents carry on the live stream).
-- principal_id scopes reads to the session owner; created_at lets retention and
-- cleanup drop rows with the transcript. Times are unix milliseconds.

CREATE TABLE history_turn_blocks (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    principal_id   TEXT    NOT NULL,
    session_id     TEXT    NOT NULL,
    turn_id        TEXT    NOT NULL,
    blocks_json    TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, principal_id, session_id, turn_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX history_turn_blocks_age
    ON history_turn_blocks (application_id, tenant_id, workspace_id, created_at);

CREATE INDEX history_turn_blocks_session
    ON history_turn_blocks (application_id, tenant_id, workspace_id, session_id, principal_id);
