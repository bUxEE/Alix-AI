-- history module, migration 0002: the search index carries the scope as an
-- indexed token, so a search MATCHes only its own scope's rows.
--
-- Migration 0001 kept the scope columns of history_fts UNINDEXED and filtered by
-- the join to transcript_turns, after the MATCH: every query visited (and the
-- join resolved) every tenant's rows that contain its words, so one tenant's
-- search cost grew with all tenants' history. That sentence of 0001 is superseded
-- here. The results were already correct; this changes only what is visited.
--
-- The index gets one more indexed column, scope, holding a single token derived
-- from the scope columns:
--
--     's' || hex(application_id) || 'g' || hex(tenant_id) || 'g' || hex(workspace_id)
--
-- Every character is a letter or a digit, so the unicode61 tokenizer keeps it as
-- one token; 'g' is not a hex digit, so the encoding is unambiguous. Search puts
-- scope:"<token>" in the MATCH next to the caller's words, which are confined to
-- the body column, so no word typed by a user can ever match a scope token. The
-- SQL scope predicate on the join stays as well.
--
-- transcript_turns has no such column and belongs to the transcript module, so
-- the index reads its content through history_search_src, a view that adds the
-- token. The triggers stay on transcript_turns and compute the token with the same
-- expression, from new.* and old.*; the update trigger now also fires on a change
-- of the scope columns, which are indexed now. A migration that rebuilds
-- transcript_turns must recreate the view as well as the triggers.

DROP TRIGGER history_transcript_ai;
DROP TRIGGER history_transcript_ad;
DROP TRIGGER history_transcript_au;
DROP TABLE history_fts;

CREATE VIEW history_search_src AS
SELECT seq, body,
       's' || hex(application_id) || 'g' || hex(tenant_id) || 'g' || hex(workspace_id) AS scope,
       application_id, tenant_id, workspace_id
FROM transcript_turns;

CREATE VIRTUAL TABLE history_fts USING fts5 (
    body, scope,
    application_id UNINDEXED, tenant_id UNINDEXED, workspace_id UNINDEXED,
    content = 'history_search_src', content_rowid = 'seq',
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER history_transcript_ai AFTER INSERT ON transcript_turns BEGIN
    INSERT INTO history_fts (rowid, body, scope, application_id, tenant_id, workspace_id)
    VALUES (new.seq, new.body,
            's' || hex(new.application_id) || 'g' || hex(new.tenant_id) || 'g' || hex(new.workspace_id),
            new.application_id, new.tenant_id, new.workspace_id);
END;

CREATE TRIGGER history_transcript_ad AFTER DELETE ON transcript_turns BEGIN
    INSERT INTO history_fts (history_fts, rowid, body, scope, application_id, tenant_id, workspace_id)
    VALUES ('delete', old.seq, old.body,
            's' || hex(old.application_id) || 'g' || hex(old.tenant_id) || 'g' || hex(old.workspace_id),
            old.application_id, old.tenant_id, old.workspace_id);
END;

CREATE TRIGGER history_transcript_au AFTER UPDATE OF body, application_id, tenant_id, workspace_id ON transcript_turns BEGIN
    INSERT INTO history_fts (history_fts, rowid, body, scope, application_id, tenant_id, workspace_id)
    VALUES ('delete', old.seq, old.body,
            's' || hex(old.application_id) || 'g' || hex(old.tenant_id) || 'g' || hex(old.workspace_id),
            old.application_id, old.tenant_id, old.workspace_id);
    INSERT INTO history_fts (rowid, body, scope, application_id, tenant_id, workspace_id)
    VALUES (new.seq, new.body,
            's' || hex(new.application_id) || 'g' || hex(new.tenant_id) || 'g' || hex(new.workspace_id),
            new.application_id, new.tenant_id, new.workspace_id);
END;

-- Index every turn written before this migration, with its token.
INSERT INTO history_fts (history_fts) VALUES ('rebuild');
