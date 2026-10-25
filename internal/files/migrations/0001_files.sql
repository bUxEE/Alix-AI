-- files module, migration 0001: the metadata of attachments and generated files
-- (master plan 14.1, 16.2, 19; task M3-06).
--
-- One row per scoped file reference. The bytes are NOT here: they are a
-- content-addressed blob under <data_dir>/files, found through blob_key, and no
-- column holds a path. blob_key is a SHA-256 over the application, tenant and
-- workspace AND the content (SEC-023), so two scopes that store identical bytes
-- never share a key, a directory or a file, and a row of one scope can never
-- name the blob of another. content_hash is the plain SHA-256 of the content, kept
-- only to report it back to the owner of the reference.
--
-- id is the opaque FileRef id the model and the SDKs see (fil_ plus a ULID); a row
-- is found only through the application, tenant and workspace of the verified
-- scope. owner_id is the principal that uploaded it; session_id is non-empty only
-- for an ephemeral turn context, which resolves only inside the session that
-- uploaded it. permissions is a comma-separated subset of read, preview and
-- download.
--
-- expires_at is when the reference stops resolving. tombstoned_at is set the moment
-- a file is deleted, expires or its tenant is deleted: the row stops resolving
-- at once and compaction later removes the blob (when no live row of the scope
-- still uses it) and then the row (master plan 19).
--
-- Times are unix milliseconds from the injected clock.

CREATE TABLE files (
    seq            INTEGER PRIMARY KEY,
    id             TEXT    NOT NULL,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    owner_id       TEXT    NOT NULL,
    session_id     TEXT    NOT NULL,
    name           TEXT    NOT NULL,
    mime_type      TEXT    NOT NULL,
    size_bytes     INTEGER NOT NULL CHECK (size_bytes > 0),
    content_hash   TEXT    NOT NULL,
    blob_key       TEXT    NOT NULL,
    origin         TEXT    NOT NULL CHECK (origin IN ('upload', 'generated', 'application')),
    treatment      TEXT    NOT NULL CHECK (treatment IN ('ephemeral_context', 'rag_source', 'application_object')),
    visibility     TEXT    NOT NULL CHECK (visibility IN ('owner', 'workspace')),
    permissions    TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    tombstoned_at  INTEGER
) STRICT;

CREATE UNIQUE INDEX files_key
    ON files (application_id, tenant_id, workspace_id, id);

-- The per-tenant quota sums live rows of the whole tenant.
CREATE INDEX files_tenant_live
    ON files (application_id, tenant_id, tombstoned_at, expires_at);

-- Compaction: does any row still use this blob? (blob_key already carries the scope.)
CREATE INDEX files_blob
    ON files (blob_key);

-- Compaction: what has expired, and what is tombstoned and due for removal.
CREATE INDEX files_expiry
    ON files (expires_at)
    WHERE tombstoned_at IS NULL;

CREATE INDEX files_tombstones
    ON files (tombstoned_at)
    WHERE tombstoned_at IS NOT NULL;
