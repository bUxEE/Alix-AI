-- settings module, migration 0001: typed, versioned settings documents (master plan 14.2).
--
-- One row per document. A document belongs to one of three levels, and the level
-- decides which parts of the scope key it:
--
--   deployment  the application as a whole (every tenant and user of it):
--               application_id set; tenant_id, workspace_id and subject_id empty
--   tenant      one tenant of the application:
--               application_id and tenant_id set; workspace_id and subject_id empty
--   user        one principal in one workspace:
--               application_id, tenant_id, workspace_id and subject_id (the principal) set
--
-- The scope columns exist on every row as the store rule requires (TEXT NOT NULL);
-- the coarser levels leave the finer columns empty, as the issuer key table does
-- for an application-wide row, and the CHECK refuses any other shape, so a row can
-- never be half scoped. The key is built only from a verified token scope, never
-- from a request, and every statement of the module names the level and all four
-- key columns.
--
-- document is the canonical JSON of the typed document (schemas/settings); schema_id
-- is the urn of the schema it was validated against, so a later schema version can
-- migrate stored documents instead of guessing. version is the revision: 1 for the
-- first write and one more for every change, compared on every update so two
-- writers cannot silently overwrite each other (optimistic concurrency).
--
-- Times are unix milliseconds from the injected clock.

CREATE TABLE settings_documents (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    level          TEXT    NOT NULL CHECK (level IN ('deployment', 'tenant', 'user')),
    subject_id     TEXT    NOT NULL,
    schema_id      TEXT    NOT NULL CHECK (schema_id <> ''),
    version        INTEGER NOT NULL CHECK (version >= 1),
    document       TEXT    NOT NULL CHECK (json_valid(document) AND json_type(document) = 'object'),
    updated_by     TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    CHECK (application_id <> ''),
    CHECK (
        (level = 'deployment' AND tenant_id = ''  AND workspace_id = ''  AND subject_id = '')
     OR (level = 'tenant'     AND tenant_id <> '' AND workspace_id = ''  AND subject_id = '')
     OR (level = 'user'       AND tenant_id <> '' AND workspace_id <> '' AND subject_id <> '')
    )
) STRICT;

CREATE UNIQUE INDEX settings_documents_key
    ON settings_documents (application_id, tenant_id, workspace_id, level, subject_id);
