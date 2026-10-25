-- transcript module, migration 0002: the index a history export pages through.
--
-- An export reads the caller's own rows of a workspace (principal_id) a page at a
-- time in seq order. Without this index every page sorted all the workspace's rows
-- in a temporary B-tree. Every index ends with the rowid (= seq), so this one
-- yields a principal's rows in seq order and seeks straight to "seq > the last
-- page". Nothing else changes; history's triggers on transcript_turns are untouched.
CREATE INDEX IF NOT EXISTS transcript_turns_principal
    ON transcript_turns (application_id, tenant_id, workspace_id, principal_id);
