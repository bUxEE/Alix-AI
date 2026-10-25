-- actions module, migration 0003: the index the erasure of receipts uses.
--
-- EraseFinished (retention.go) deletes the receipts of a chunk of preparations of
-- one application and tenant, across its workspaces. action_receipts_preparation
-- of 0001 has workspace_id before preparation_id, so each chunk walked every
-- receipt of the tenant; this index finds them by preparation.
CREATE INDEX IF NOT EXISTS action_receipts_erase
    ON action_receipts (application_id, tenant_id, preparation_id);
