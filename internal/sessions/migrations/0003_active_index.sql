-- sessions module, migration 0003: the index the workspace sweeps page through.
--
-- InvalidateWorkspace and ApplyEvent read the active sessions of a workspace a
-- page at a time in seq order. When the sweep concerns every session, each page
-- sorted all the workspace's sessions in a temporary B-tree. Every index ends
-- with the rowid (= seq), so this one yields the active sessions in seq order and
-- seeks straight to "seq > the last page". It is partial on the status, so it
-- holds only live sessions and is used only by a query that repeats
-- status = 'active' literally: the sweep page writes it that way only when the
-- sweep can select every active session (invalidate.go, sweepPage), so a sweep
-- that names a principal, records or a deadline keeps its narrower index.
CREATE INDEX IF NOT EXISTS sessions_active
    ON sessions (application_id, tenant_id, workspace_id) WHERE status = 'active';
