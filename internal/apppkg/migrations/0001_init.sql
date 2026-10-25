-- Installed application packages (master plan 4.5, 9.4).
--
-- A package is installed content-addressed: the primary key is the application
-- and the SHA-256 of the signed integrity manifest, so the same bytes are one
-- row however often they are installed, and any changed byte is another row.
--
-- Like the trust store's publisher keys, this table is application-wide: a
-- package serves every tenant of its application, and it holds no tenant data,
-- so it has no tenant or workspace column and the module's tests exempt it from
-- the scope lint on purpose. Every query in internal/apppkg filters on
-- application_id.
--
-- The state machine is enforced here as well as in Go:
--
--   * a package is only ever INSERTED as 'quarantined' (the loader cannot
--     install a package straight into service);
--   * only a quarantined package can become 'active', and only with the ID of
--     the integration audit report that cleared it. The loader never writes
--     'active'; the integration audit (M1-17) is the only writer;
--   * 'rejected' and 'superseded' are final;
--   * rows are never deleted and a package never changes its identity or
--     content;
--   * at most one package of an application is 'active' at a time.
CREATE TABLE apppkg_packages (
    application_id    TEXT    NOT NULL,
    -- lower-case hex SHA-256 of the canonical signed integrity manifest
    manifest_hash     TEXT    NOT NULL CHECK (length(manifest_hash) = 64),
    package_version   TEXT    NOT NULL,
    status            TEXT    NOT NULL CHECK (status IN ('quarantined', 'active', 'rejected', 'superseded')),
    -- the optional security domain of the manifest (ADR-0016), or ''
    security_domain   TEXT    NOT NULL,
    -- the publisher key that authenticated the package
    signer_key_id     TEXT    NOT NULL,
    -- the install directory, relative to data_dir, with forward slashes
    install_path      TEXT    NOT NULL,
    file_count        INTEGER NOT NULL CHECK (file_count >= 1),
    byte_count        INTEGER NOT NULL CHECK (byte_count >= 0),
    -- findings of severity warning at install time
    warning_count     INTEGER NOT NULL CHECK (warning_count >= 0),
    -- unix milliseconds, from the injected clock
    installed_at      INTEGER NOT NULL,
    status_changed_at INTEGER NOT NULL,
    status_reason     TEXT    NOT NULL,
    -- set by the integration audit when it activates the package
    audit_report_id   TEXT,
    PRIMARY KEY (application_id, manifest_hash)
) STRICT;

CREATE INDEX apppkg_packages_by_application ON apppkg_packages (application_id, installed_at);

CREATE UNIQUE INDEX apppkg_packages_one_active ON apppkg_packages (application_id) WHERE status = 'active';

CREATE TRIGGER apppkg_packages_install_quarantined
BEFORE INSERT ON apppkg_packages
WHEN NEW.status <> 'quarantined' OR NEW.audit_report_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'a package is installed quarantined');
END;

CREATE TRIGGER apppkg_packages_no_delete
BEFORE DELETE ON apppkg_packages
BEGIN
    SELECT RAISE(ABORT, 'installed packages are never deleted');
END;

CREATE TRIGGER apppkg_packages_immutable_content
BEFORE UPDATE OF application_id, manifest_hash, package_version, security_domain, signer_key_id, install_path, file_count, byte_count, warning_count, installed_at ON apppkg_packages
BEGIN
    SELECT RAISE(ABORT, 'an installed package never changes its identity or content');
END;

CREATE TRIGGER apppkg_packages_final_states
BEFORE UPDATE ON apppkg_packages
WHEN OLD.status IN ('rejected', 'superseded') AND NEW.status <> OLD.status
BEGIN
    SELECT RAISE(ABORT, 'a rejected or superseded package keeps its state');
END;

CREATE TRIGGER apppkg_packages_activation_needs_audit
BEFORE UPDATE ON apppkg_packages
WHEN NEW.status = 'active' AND OLD.status <> 'active'
 AND (OLD.status <> 'quarantined' OR NEW.audit_report_id IS NULL OR NEW.audit_report_id = '')
BEGIN
    SELECT RAISE(ABORT, 'only a quarantined package with an integration audit report can become active');
END;
