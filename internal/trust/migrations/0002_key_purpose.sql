-- What a platform key may sign (security finding H1).
--
-- Every AlixAI signing key is registered under the one application
-- alixai.platform, so without a purpose any of them verified any platform
-- artifact: the audit-checkpoint key that `serve` keeps online could sign a
-- release manifest. A purpose binds a key to one kind of artifact:
--
--   ''                 no purpose: what keys registered before this migration,
--                      and keys of host applications, have. Verifications that
--                      name a purpose (release manifests, audit checkpoints and
--                      tombstones) never accept such a key.
--   'release'          signs release manifests (tools/release).
--   'audit-checkpoint' signs audit chain checkpoints and tombstones.
--
-- A purpose is set once: '' may become a purpose (an operator classifying an
-- existing key), a purpose never changes again. The key `alixai keys init`
-- registered is classified here by the label it always writes.
ALTER TABLE trust_publisher_keys ADD COLUMN purpose TEXT NOT NULL DEFAULT ''
    CHECK (purpose IN ('', 'release', 'audit-checkpoint'));

UPDATE trust_publisher_keys SET purpose = 'audit-checkpoint'
 WHERE application_id = 'alixai.platform' AND label = 'audit checkpoint signing key';

CREATE TRIGGER trust_publisher_keys_purpose_is_permanent
BEFORE UPDATE OF purpose ON trust_publisher_keys
WHEN OLD.purpose <> '' AND NEW.purpose IS NOT OLD.purpose
BEGIN
    SELECT RAISE(ABORT, 'a key purpose is set once and never changed');
END;

CREATE TRIGGER trust_publisher_keys_purpose_is_platform_only
BEFORE UPDATE OF purpose ON trust_publisher_keys
WHEN NEW.purpose <> '' AND NEW.application_id <> 'alixai.platform'
BEGIN
    SELECT RAISE(ABORT, 'only platform keys have a purpose');
END;

CREATE TRIGGER trust_publisher_keys_purpose_insert_platform_only
BEFORE INSERT ON trust_publisher_keys
WHEN NEW.purpose <> '' AND NEW.application_id <> 'alixai.platform'
BEGIN
    SELECT RAISE(ABORT, 'only platform keys have a purpose');
END;
