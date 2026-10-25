-- Module "ragsources": the durable registry of the sources a deployment serves
-- (master plan 12.1, 12.4, 10.2; M2-17). Every table carries application_id,
-- tenant_id and workspace_id; every query filters on them (store.ScopedWhere),
-- except the one the process makes on purpose and the package documents: the
-- start-up read of the whole deployment (Registry.LoadAll).
--
-- A row says WHICH declaration of WHICH package a scope registered, not what the
-- declaration says: the definition is rebuilt from the active package (source.
-- FromPackage), so a package that was replaced while the process was down is
-- read as what it is now, and the host path of a mount is never copied here.
-- manifest_hash is the content address of the package the registration was last
-- bound to; a registration is served only while it equals the active package's.
--
-- Times are unix nanoseconds from the injected clock.

CREATE TABLE ragsources_registration (
    application_id     TEXT    NOT NULL,
    tenant_id          TEXT    NOT NULL,
    workspace_id       TEXT    NOT NULL,
    source_id          TEXT    NOT NULL,
    -- The declaration reference: the slug the package declares the source under.
    package_source_id  TEXT    NOT NULL CHECK (package_source_id <> ''),
    package_version    TEXT    NOT NULL DEFAULT '',
    -- The content hash of the package the registration is bound to.
    manifest_hash      TEXT    NOT NULL CHECK (manifest_hash <> ''),
    -- The tenant and workspace the definition names, as FromPackage bound it:
    -- empty for a shared level, the declaration's own id for a fixed one, the
    -- registering scope's for a request-bound one. They are not always the scope
    -- columns, so a rebuild reads them from here and never derives them.
    bound_tenant_id    TEXT    NOT NULL DEFAULT '',
    bound_workspace_id TEXT    NOT NULL DEFAULT '',
    -- The scope the source was registered under, as JSON: the initiating scope
    -- of the work queued for it (re-audit, removal) under the service identity.
    scope_json         TEXT    NOT NULL,
    -- removing: the removal began and is not finished. Such a source is never
    -- served and a repeat of the removal completes it.
    state              TEXT    NOT NULL CHECK (state IN ('active', 'removing')),
    created_ns         INTEGER NOT NULL,
    updated_ns         INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, source_id)
) WITHOUT ROWID;

-- One registration of a declaration per scope: registering it again is the same
-- registration (a gateway call is idempotent), never a second copy of the source.
CREATE UNIQUE INDEX ragsources_registration_declaration
    ON ragsources_registration (application_id, tenant_id, workspace_id, package_source_id);
