-- history module, migration 0001: full-text search over the conversation
-- transcript and the retention policies (master plan 14.2, 19).
--
-- The transcript itself belongs to the "transcript" module (internal/sessions
-- writes it in the same transaction as the session state). This module adds two
-- things next to it and never writes to transcript_turns:
--
--  1. history_fts, an external-content FTS5 index over transcript_turns.body.
--     It stores no second copy of any text: the index holds terms only and a hit
--     is resolved against transcript_turns by rowid (= seq) under a scope
--     predicate. Because the index follows the rows through triggers, ANY
--     deletion of transcript rows (a user clearing a session, retention, a tenant
--     deletion, a future compaction) removes them from the index in the same
--     statement: a cleared turn can never be found by a search afterwards.
--     The scope columns are UNINDEXED: they exist so the table obeys the scope
--     rule (every table carries application_id, tenant_id, workspace_id) and are
--     never used to filter; the join to transcript_turns does that.
--
--  2. history_retention, the retention policy per application and tenant (master
--     plan 19). A policy is a number of days a transcript turn is kept. The scope
--     columns of this table are the policy's own level, with '*' standing for
--     "every": a real scope value can never be '*' (contracts.Scope.Validate
--     refuses a wildcard), so a sentinel cannot collide with a tenant.
--       application policy:  tenant_id = '*', workspace_id = '*'
--       tenant policy:       tenant_id = <tenant>, workspace_id = '*'
--     A policy covers every workspace of its level. The effective retention of a
--     tenant is the SHORTER of the two that exist; with none, nothing is deleted.
--
-- Times are unix milliseconds from the injected clock.

CREATE VIRTUAL TABLE history_fts USING fts5 (
    body,
    application_id UNINDEXED, tenant_id UNINDEXED, workspace_id UNINDEXED,
    content = 'transcript_turns', content_rowid = 'seq',
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER history_transcript_ai AFTER INSERT ON transcript_turns BEGIN
    INSERT INTO history_fts (rowid, body, application_id, tenant_id, workspace_id)
    VALUES (new.seq, new.body, new.application_id, new.tenant_id, new.workspace_id);
END;

CREATE TRIGGER history_transcript_ad AFTER DELETE ON transcript_turns BEGIN
    INSERT INTO history_fts (history_fts, rowid, body, application_id, tenant_id, workspace_id)
    VALUES ('delete', old.seq, old.body, old.application_id, old.tenant_id, old.workspace_id);
END;

CREATE TRIGGER history_transcript_au AFTER UPDATE OF body ON transcript_turns BEGIN
    INSERT INTO history_fts (history_fts, rowid, body, application_id, tenant_id, workspace_id)
    VALUES ('delete', old.seq, old.body, old.application_id, old.tenant_id, old.workspace_id);
    INSERT INTO history_fts (rowid, body, application_id, tenant_id, workspace_id)
    VALUES (new.seq, new.body, new.application_id, new.tenant_id, new.workspace_id);
END;

-- Index the turns that were written before this module existed.
INSERT INTO history_fts (history_fts) VALUES ('rebuild');

CREATE TABLE history_retention (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    retain_days    INTEGER NOT NULL CHECK (retain_days BETWEEN 1 AND 3650),
    updated_by     TEXT    NOT NULL,
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id),
    CHECK (workspace_id = '*')
) STRICT, WITHOUT ROWID;
