-- providerreg module, migration 0004: private self-hosted LLM endpoints (ADR 0045).

CREATE TABLE providerreg_private_endpoints (
    seq                 INTEGER PRIMARY KEY,
    application_id      TEXT    NOT NULL,
    tenant_id           TEXT    NOT NULL,
    workspace_id        TEXT    NOT NULL,
    endpoint_id         TEXT    NOT NULL CHECK (endpoint_id <> ''),
    base_url            TEXT    NOT NULL CHECK (base_url <> ''),
    api_dialect         TEXT    NOT NULL CHECK (api_dialect = 'openai-compatible'),
    model_ids_json      TEXT    NOT NULL CHECK (json_valid(model_ids_json) AND json_type(model_ids_json) = 'array'),
    roles_json          TEXT    NOT NULL CHECK (json_valid(roles_json) AND json_type(roles_json) = 'array'),
    credential_ref      TEXT,
    owner_kind          TEXT    NOT NULL CHECK (owner_kind IN ('principal', 'tenant', 'shared')),
    owner_principal_id  TEXT,
    owner_tenant_id     TEXT,
    health_url          TEXT,
    lifecycle           TEXT    NOT NULL CHECK (lifecycle IN ('provisioning', 'warming', 'ready', 'draining', 'stopped', 'failed')),
    profile_id          TEXT    NOT NULL CHECK (profile_id <> ''),
    price_in            INTEGER NOT NULL DEFAULT 0,
    price_out           INTEGER NOT NULL DEFAULT 0,
    updated_at          INTEGER NOT NULL,
    CHECK (application_id <> '' AND tenant_id = '' AND workspace_id = '')
) STRICT;

CREATE UNIQUE INDEX providerreg_private_endpoints_key
    ON providerreg_private_endpoints (application_id, tenant_id, workspace_id, endpoint_id);

CREATE TABLE providerreg_tenant_private_llm (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL CHECK (tenant_id <> ''),
    workspace_id   TEXT    NOT NULL,
    endpoint_id    TEXT    NOT NULL CHECK (endpoint_id <> ''),
    updated_at     INTEGER NOT NULL,
    CHECK (application_id <> '' AND workspace_id = '')
) STRICT;

CREATE UNIQUE INDEX providerreg_tenant_private_llm_key
    ON providerreg_tenant_private_llm (application_id, tenant_id);

CREATE TABLE providerreg_principal_private_bindings (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    principal_id   TEXT    NOT NULL CHECK (principal_id <> ''),
    role           TEXT    NOT NULL CHECK (role IN ('conversational_controller', 'language_realization', 'stt', 'tts')),
    endpoint_id    TEXT    NOT NULL CHECK (endpoint_id <> ''),
    model_id       TEXT    NOT NULL CHECK (model_id <> ''),
    updated_at     INTEGER NOT NULL,
    CHECK (application_id <> '' AND tenant_id = '' AND workspace_id = '')
) STRICT;

CREATE UNIQUE INDEX providerreg_principal_private_bindings_key
    ON providerreg_principal_private_bindings (application_id, tenant_id, workspace_id, principal_id, role);
