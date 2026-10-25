-- readiness module, migration 0001: the stored integration readiness reports (master plan 9.5 and
-- 19, ADR 0037).
--
-- A report belongs to a scope KEY, not to a tenant: readiness runs on a project directory before
-- the application has a package, so the key is the application id the operator names, or a project
-- id derived from a hash of the directory path (prj_ and 32 hex digits). Rows are therefore
-- key-level: application_id carries the key, tenant_id and workspace_id are empty, and the CHECK
-- refuses any other shape (the same layout as the providerreg module). Every statement of the
-- module names all three scope columns, so one key never reads or changes another key's reports.
--
-- document is the canonical JSON of the typed report (schemas/readiness/report). It holds the
-- findings with their evidence entries (path, line span, file hash) and the bounded, already
-- redacted summary text of each finding, and nothing else of any scanned file: no file content,
-- no excerpt beyond that text, no secret value. The path of the scanned directory is kept only as
-- the display text of the report and never used as a file name. The indexed columns repeat what
-- the module selects on and are written with the document in the same statement.
-- Times are unix milliseconds from the injected clock.

CREATE TABLE readiness_reports (
    seq                INTEGER PRIMARY KEY,
    application_id     TEXT    NOT NULL,
    tenant_id          TEXT    NOT NULL,
    workspace_id       TEXT    NOT NULL,
    report_id          TEXT    NOT NULL CHECK (report_id <> ''),
    previous_report_id TEXT,
    inputs_sha256      TEXT    NOT NULL CHECK (length(inputs_sha256) = 64),
    catalog_sha256     TEXT    NOT NULL CHECK (length(catalog_sha256) = 64),
    mode               TEXT    NOT NULL CHECK (mode IN ('deterministic_only', 'deterministic_and_model')),
    found              INTEGER NOT NULL CHECK (found >= 0),
    partial            INTEGER NOT NULL CHECK (partial >= 0),
    missing            INTEGER NOT NULL CHECK (missing >= 0),
    unverified         INTEGER NOT NULL CHECK (unverified >= 0),
    contradicted       INTEGER NOT NULL CHECK (contradicted >= 0),
    document           TEXT    NOT NULL CHECK (json_valid(document) AND json_type(document) = 'object'),
    generated_at       INTEGER NOT NULL,
    stored_at          INTEGER NOT NULL,
    CHECK (application_id <> '' AND tenant_id = '' AND workspace_id = '')
) STRICT;

CREATE UNIQUE INDEX readiness_reports_key
    ON readiness_reports (application_id, tenant_id, workspace_id, report_id);

CREATE INDEX readiness_reports_history
    ON readiness_reports (application_id, tenant_id, workspace_id, seq);
