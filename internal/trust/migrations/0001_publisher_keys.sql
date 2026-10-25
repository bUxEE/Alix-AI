-- Publisher keys of the trust store (master plan 4.5 check 1, 9.1, 16.3).
--
-- A key is registered for ONE application; the same key id under two
-- applications are two unrelated rows. The table has no tenant or workspace
-- column on purpose: a publisher key authenticates an application's packages
-- and events for every tenant of that application, and it holds no tenant
-- data. Every query in internal/trust filters on application_id.
--
-- Nothing is trusted implicitly: the table starts empty. Rows are never
-- deleted and a revoked key never comes back, both enforced here by triggers
-- as well as in Go.
CREATE TABLE trust_publisher_keys (
    application_id TEXT    NOT NULL,
    key_id         TEXT    NOT NULL,
    -- raw 32-byte Ed25519 public key, standard base64 with padding
    public_key     TEXT    NOT NULL,
    -- lower-case hex SHA-256 of the raw public key
    fingerprint    TEXT    NOT NULL,
    status         TEXT    NOT NULL CHECK (status IN ('active', 'retired', 'revoked')),
    label          TEXT    NOT NULL,
    -- unix milliseconds, from the injected clock
    registered_at  INTEGER NOT NULL,
    retired_at     INTEGER,
    revoked_at     INTEGER,
    revoked_reason TEXT    NOT NULL,
    PRIMARY KEY (application_id, key_id),
    UNIQUE (application_id, fingerprint),
    CHECK (status <> 'active' OR (retired_at IS NULL AND revoked_at IS NULL)),
    CHECK (status <> 'retired' OR (retired_at IS NOT NULL AND revoked_at IS NULL)),
    CHECK (status <> 'revoked' OR revoked_at IS NOT NULL)
) STRICT;

CREATE INDEX trust_publisher_keys_fingerprint ON trust_publisher_keys (fingerprint);

CREATE TRIGGER trust_publisher_keys_no_delete
BEFORE DELETE ON trust_publisher_keys
BEGIN
    SELECT RAISE(ABORT, 'trust keys are never deleted; revoke them');
END;

CREATE TRIGGER trust_publisher_keys_immutable_identity
BEFORE UPDATE OF application_id, key_id, public_key, fingerprint, registered_at ON trust_publisher_keys
BEGIN
    SELECT RAISE(ABORT, 'a registered key cannot change its identity or material');
END;

CREATE TRIGGER trust_publisher_keys_revocation_is_permanent
BEFORE UPDATE ON trust_publisher_keys
WHEN OLD.status = 'revoked' AND (NEW.status <> 'revoked' OR NEW.revoked_at IS NOT OLD.revoked_at)
BEGIN
    SELECT RAISE(ABORT, 'a revoked key stays revoked');
END;

CREATE TRIGGER trust_publisher_keys_no_reactivation
BEFORE UPDATE ON trust_publisher_keys
WHEN OLD.status = 'retired' AND NEW.status = 'active'
BEGIN
    SELECT RAISE(ABORT, 'a retired key is not reactivated; register a new key');
END;
