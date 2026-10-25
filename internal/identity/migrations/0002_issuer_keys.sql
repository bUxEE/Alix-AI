-- Issuer keys of host applications (ADR 0013): the PUBLIC Ed25519 keys that
-- verify application context tokens. AlixAI never holds a private half.
--
-- A key speaks for ONE application. key_id is the primary key because the
-- verifier looks a key up by the "kid" of the token header alone, before it
-- knows anything else about the token, so a kid must be unique across the
-- deployment; the application binding is then checked by the verifier against
-- the application_id of the verified claims. The table follows the scope rule
-- of the store: the three scope columns exist, and since an issuer key belongs
-- to an application as a whole (every tenant and workspace of it) the tenant
-- and workspace columns are always empty, which the CHECK enforces.
--
-- Rows are never deleted: a revoked key stays as the record that it was
-- revoked, its key id is never reusable, and revocation is permanent (the same
-- guarantees as the publisher keys of the trust store, enforced by triggers as
-- well as in Go). Lookups read this table on every verification so a
-- revocation made by `alixai identity revoke-issuer-key` takes effect for the
-- very next token, in a running service too.
CREATE TABLE identity_issuer_keys (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL DEFAULT '',
    workspace_id   TEXT    NOT NULL DEFAULT '',
    key_id         TEXT    NOT NULL PRIMARY KEY,
    -- the exact "iss" value tokens signed by this key must carry
    issuer         TEXT    NOT NULL,
    -- raw 32-byte Ed25519 public key, standard base64 with padding
    public_key     TEXT    NOT NULL,
    -- lower-case hex SHA-256 of the raw public key
    fingerprint    TEXT    NOT NULL,
    status         TEXT    NOT NULL CHECK (status IN ('active', 'revoked')),
    -- 1 when the operator added the key with the explicit developer flag; the
    -- only rows allowed to hold a published TEST-ONLY key
    dev_key        INTEGER NOT NULL DEFAULT 0 CHECK (dev_key IN (0, 1)),
    label          TEXT    NOT NULL,
    -- unix milliseconds, from the injected clock
    registered_at  INTEGER NOT NULL,
    -- when set, the key verifies nothing from this instant
    not_after      INTEGER,
    revoked_at     INTEGER,
    revoked_reason TEXT    NOT NULL,
    UNIQUE (application_id, fingerprint),
    CHECK (tenant_id = '' AND workspace_id = ''),
    CHECK (status <> 'active' OR revoked_at IS NULL),
    CHECK (status <> 'revoked' OR revoked_at IS NOT NULL)
) STRICT;

CREATE INDEX identity_issuer_keys_application ON identity_issuer_keys (application_id);
CREATE INDEX identity_issuer_keys_fingerprint ON identity_issuer_keys (fingerprint);

CREATE TRIGGER identity_issuer_keys_no_delete
BEFORE DELETE ON identity_issuer_keys
BEGIN
    SELECT RAISE(ABORT, 'issuer keys are never deleted; revoke them');
END;

CREATE TRIGGER identity_issuer_keys_immutable_identity
BEFORE UPDATE OF application_id, key_id, issuer, public_key, fingerprint, dev_key, registered_at, not_after ON identity_issuer_keys
BEGIN
    SELECT RAISE(ABORT, 'a registered issuer key cannot change its identity or material');
END;

CREATE TRIGGER identity_issuer_keys_revocation_is_permanent
BEFORE UPDATE ON identity_issuer_keys
WHEN OLD.status = 'revoked' AND (NEW.status <> 'revoked' OR NEW.revoked_at IS NOT OLD.revoked_at)
BEGIN
    SELECT RAISE(ABORT, 'a revoked issuer key stays revoked');
END;
