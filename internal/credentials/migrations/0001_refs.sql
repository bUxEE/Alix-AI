-- credentials module, migration 0001: the credential references of the local credential service
-- (additive plan section 11.1).
--
-- A reference belongs to one tenant of one application: application_id and tenant_id are set and
-- workspace_id is empty (the CHECK refuses any other shape), so no workspace can hold a credential
-- that another workspace of the tenant cannot use, and no tenant can reach another tenant's. Every
-- statement of the module names all three scope columns.
--
-- THE SECRET IS NEVER HERE. document is the canonical JSON of the typed credential reference
-- (schemas/providers/credential-ref): an opaque ID, the kind, a label, the binding to provider,
-- deployment, application, tenant and permitted scopes, the status, a MASKED fingerprint, the safe
-- account identifiers, times and warnings. The value of the key or token lives in the backend named
-- by backend (the OS secret store, the encrypted file, or an environment variable or file for
-- headless installs; locator names the variable or file for the last two and is empty otherwise).
-- The indexed columns repeat what the module selects on and are rewritten with the document in the
-- same statement; a read compares them with the document and refuses a row that disagrees.
-- Times are unix milliseconds from the injected clock.

CREATE TABLE credentials_refs (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    ref_id         TEXT    NOT NULL CHECK (ref_id GLOB 'cred_*'),
    deployment_id  TEXT    NOT NULL CHECK (deployment_id <> ''),
    provider_id    TEXT    NOT NULL CHECK (provider_id <> ''),
    kind           TEXT    NOT NULL CHECK (kind IN ('oauth', 'api_key')),
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

CREATE UNIQUE INDEX credentials_refs_key
    ON credentials_refs (application_id, tenant_id, workspace_id, ref_id);

CREATE INDEX credentials_refs_provider
    ON credentials_refs (application_id, tenant_id, workspace_id, provider_id, seq);
