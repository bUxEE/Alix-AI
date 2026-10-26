-- credentials module, migration 0002: allow kind oauth_issued_key (browser login that
-- stores a provider-issued API key, e.g. OpenRouter PKCE). SQLite cannot ALTER a CHECK,
-- so the table is rebuilt. Secrets stay in backends; only metadata moves.

CREATE TABLE credentials_refs_v2 (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    ref_id         TEXT    NOT NULL CHECK (ref_id GLOB 'cred_*'),
    deployment_id  TEXT    NOT NULL CHECK (deployment_id <> ''),
    provider_id    TEXT    NOT NULL CHECK (provider_id <> ''),
    kind           TEXT    NOT NULL CHECK (kind IN ('oauth', 'api_key', 'oauth_issued_key')),
    status         TEXT    NOT NULL CHECK (status IN ('pending', 'active', 'expired', 'revoked', 'invalid', 'quarantined', 'reauthorization_required')),
    backend        TEXT    NOT NULL CHECK (backend IN ('os_store', 'encrypted_file', 'env', 'file')),
    locator        TEXT    NOT NULL,
    has_refresh    INTEGER NOT NULL CHECK (has_refresh IN (0, 1)),
    document       TEXT    NOT NULL CHECK (json_valid(document) AND json_type(document) = 'object'),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    CHECK (application_id <> '' AND tenant_id <> '' AND workspace_id = ''),
    CHECK ((backend IN ('env', 'file')) = (locator <> ''))
) STRICT;

INSERT INTO credentials_refs_v2 (
    seq, application_id, tenant_id, workspace_id, ref_id, deployment_id, provider_id,
    kind, status, backend, locator, has_refresh, document, created_at, updated_at
)
SELECT
    seq, application_id, tenant_id, workspace_id, ref_id, deployment_id, provider_id,
    kind, status, backend, locator, has_refresh, document, created_at, updated_at
FROM credentials_refs;

DROP TABLE credentials_refs;
ALTER TABLE credentials_refs_v2 RENAME TO credentials_refs;

CREATE UNIQUE INDEX credentials_refs_key
    ON credentials_refs (application_id, tenant_id, workspace_id, ref_id);

CREATE INDEX credentials_refs_provider
    ON credentials_refs (application_id, tenant_id, workspace_id, provider_id, seq);
