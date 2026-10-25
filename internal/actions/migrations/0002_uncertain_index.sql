-- actions module, migration 0002: the index the reconciliation of uncertain
-- outcomes walks.
--
-- The reconcile pass of `alixai serve` (internal/cli/cmd_reconcile.go) lists the
-- uncertain preparations of every tenant in seq order after a cursor. Without this
-- index it walked every preparation of the deployment after the cursor to find
-- the few uncertain ones. Like the sweeps' indexes of 0001 it is partial on the
-- state, so the query must repeat state = 'uncertain' literally.
CREATE INDEX IF NOT EXISTS action_preparations_uncertain
    ON action_preparations (seq) WHERE state = 'uncertain';
