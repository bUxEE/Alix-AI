-- providerreg module, migration 0002: per-principal LLM catalog and bindings (ADR 0041).
--
-- Catalog entries are application-level (same shape as profiles): operator-approved profiles a
-- principal may pick. Bindings and billing acknowledgements are keyed by application + principal
-- (no company tenant): the same person keeps their account across tenants of the application.
-- The reserved tenant id alixai-principal-credential never appears here.

CREATE TABLE providerreg_catalog (
    seq                     INTEGER PRIMARY KEY,
    application_id          TEXT    NOT NULL,
    tenant_id               TEXT    NOT NULL,
    workspace_id            TEXT    NOT NULL,
    profile_id              TEXT    NOT NULL CHECK (profile_id <> ''),
    display_name            TEXT    NOT NULL CHECK (display_name <> ''),
    terms_summary           TEXT    NOT NULL CHECK (terms_summary <> ''),
    billing_notice_version  TEXT    NOT NULL CHECK (billing_notice_version <> ''),
    roles_json              TEXT    NOT NULL CHECK (json_valid(roles_json) AND json_type(roles_json) = 'array'),
    model_ids_json          TEXT    NOT NULL CHECK (json_valid(model_ids_json) AND json_type(model_ids_json) = 'array'),
    price_hint_json         TEXT,
    oauth_available         INTEGER NOT NULL CHECK (oauth_available IN (0, 1)),
    created_at              INTEGER NOT NULL,
    updated_at              INTEGER NOT NULL,
    CHECK (application_id <> '' AND tenant_id = '' AND workspace_id = '')
) STRICT;

CREATE UNIQUE INDEX providerreg_catalog_key
    ON providerreg_catalog (application_id, tenant_id, workspace_id, profile_id);

CREATE TABLE providerreg_principal_bindings (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    principal_id   TEXT    NOT NULL CHECK (principal_id <> ''),
    role           TEXT    NOT NULL CHECK (role IN ('conversational_controller', 'language_realization', 'stt')),
    profile_id     TEXT    NOT NULL CHECK (profile_id <> ''),
    provider_id    TEXT    NOT NULL CHECK (provider_id <> ''),
    model_id       TEXT    NOT NULL CHECK (model_id <> ''),
    updated_at     INTEGER NOT NULL,
    CHECK (application_id <> '' AND tenant_id = '' AND workspace_id = '')
) STRICT;

CREATE UNIQUE INDEX providerreg_principal_bindings_key
    ON providerreg_principal_bindings (application_id, tenant_id, workspace_id, principal_id, role);

CREATE TABLE providerreg_principal_billing_acks (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    principal_id   TEXT    NOT NULL CHECK (principal_id <> ''),
    provider_id    TEXT    NOT NULL CHECK (provider_id <> ''),
    notice_version TEXT    NOT NULL CHECK (notice_version <> ''),
    acknowledged_at INTEGER NOT NULL,
    CHECK (application_id <> '' AND tenant_id = '' AND workspace_id = '')
) STRICT;

CREATE UNIQUE INDEX providerreg_principal_billing_acks_key
    ON providerreg_principal_billing_acks (application_id, tenant_id, workspace_id, principal_id, provider_id);
