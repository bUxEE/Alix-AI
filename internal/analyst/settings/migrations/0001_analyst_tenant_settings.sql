-- analyst settings module, migration 0001: tenant analyst ability ceilings (ADR 0047).
--
-- One row per application × tenant. workspace_id is always empty (tenant-level
-- document). document is the canonical JSON of schemas/analyst/tenant-settings.
-- Times are unix milliseconds.

CREATE TABLE analyst_tenant_settings (
    application_id  TEXT    NOT NULL,
    tenant_id       TEXT    NOT NULL,
    workspace_id    TEXT    NOT NULL,
    document        TEXT    NOT NULL CHECK (json_valid(document) AND json_type(document) = 'object'),
    updated_by      TEXT    NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id),
    CHECK (application_id <> ''),
    CHECK (tenant_id <> ''),
    CHECK (workspace_id = '')
) STRICT;
