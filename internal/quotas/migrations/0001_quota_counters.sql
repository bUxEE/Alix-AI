-- quotas module, migration 0001: persisted per-tenant counters (ADR 0018, master plan 25 item 9).
--
-- One row per tenant, dimension and window. Quotas are per application AND tenant, never per workspace,
-- so workspace_id is the empty string on every row (the settings module does the same for its
-- tenant-level documents): the scope columns exist because the store rule asks for them, the key is
-- built only from a verified Scope, and every statement of the module names all three.
--
--   ingest_bytes_per_day  one row per UTC day; window_start is the day's first millisecond
--   storage_bytes         a gauge: window_start is 0
--
-- used is what the tenant has consumed in the window. The CHECK keeps it from ever going below zero,
-- whatever a caller or a refund does. Rows are charged in the same transaction as the data they
-- describe, so an exhausted quota rolls the data write back with it.
--
-- Times are unix milliseconds from the injected clock.

CREATE TABLE quota_counters (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    dimension      TEXT    NOT NULL CHECK (dimension IN ('ingest_bytes_per_day', 'storage_bytes')),
    window_start   INTEGER NOT NULL CHECK (window_start >= 0),
    used           INTEGER NOT NULL CHECK (used >= 0),
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, dimension, window_start),
    CHECK (application_id <> '' AND tenant_id <> '' AND workspace_id = ''),
    CHECK (dimension <> 'storage_bytes' OR window_start = 0)
) STRICT, WITHOUT ROWID;
