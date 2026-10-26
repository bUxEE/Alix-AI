-- memory module, migration 0001: long-term memory items (ADR 0042).
--
-- One row per item, scoped to application × tenant × workspace × principal.
-- Status is proposed | active | rejected. Times are unix milliseconds from the
-- injected clock. Structured facts are JSON object text or empty.

CREATE TABLE memory_items (
    id              TEXT    NOT NULL,
    application_id  TEXT    NOT NULL,
    tenant_id       TEXT    NOT NULL,
    workspace_id    TEXT    NOT NULL,
    principal_id    TEXT    NOT NULL,
    text            TEXT    NOT NULL CHECK (length(text) BETWEEN 1 AND 4000),
    structured_json TEXT    NOT NULL DEFAULT '',
    origin          TEXT    NOT NULL CHECK (origin IN ('user_explicit', 'assistant_proposed')),
    status          TEXT    NOT NULL CHECK (status IN ('proposed', 'active', 'rejected')),
    data_class      TEXT    NOT NULL CHECK (data_class IN ('public', 'internal', 'confidential', 'restricted')),
    expires_at      INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, id)
) STRICT;

CREATE INDEX memory_items_principal_status
    ON memory_items (application_id, tenant_id, workspace_id, principal_id, status, updated_at DESC);

CREATE INDEX memory_items_principal_created
    ON memory_items (application_id, tenant_id, workspace_id, principal_id, created_at DESC);
