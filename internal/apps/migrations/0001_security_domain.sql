-- apps: the security-domain rule of ADR 0016 (item 5), enforced by the store.
--
-- Several applications may be active in one process (one data directory) only within one security
-- domain. The domain is the optional `security_domain` slug of the manifest, recorded in
-- apppkg_packages.security_domain ('' when the manifest declares none). The rule, which is the only
-- thing these triggers say:
--
--   a package cannot be INSERTED (installed) and a package cannot become 'active' while another
--   application has an active package whose security domain is not the same text.
--
-- The comparison is plain equality of the stored text: '' (no domain declared) equals only '', so an
-- application that declares none shares a process with other undeclared applications and with no
-- named domain. Only OTHER applications count: a new version of an application, and the supersede of
-- its own previous version in the same transaction, never conflict with themselves.
--
-- Why triggers and not only Go code: installing and activating are two writers in two modules (apppkg
-- inserts, integrationaudit activates inside its own transaction) and neither can be made to ask a
-- third module first. A trigger runs inside the very transaction that changes the row, so a check
-- and the change can never be separated by a concurrent activation of another application, and no
-- present or future caller can skip it. internal/apps adds what a trigger cannot: the typed error and
-- the audit event (apps.Guard, apps.AsRefusal).
--
-- This module owns these two triggers on a table of the "apppkg" module, as the repository's
-- migration rules allow for a rule that cuts across modules. The owner of apppkg_packages must
-- recreate them in any migration that rebuilds that table. The message of each is matched by
-- apps.AsRefusal: keep the two in step.

CREATE TRIGGER apps_security_domain_on_install
BEFORE INSERT ON apppkg_packages
WHEN EXISTS (
    SELECT 1 FROM apppkg_packages o
     WHERE o.status = 'active'
       AND o.application_id <> NEW.application_id
       AND o.security_domain <> NEW.security_domain
)
BEGIN
    SELECT RAISE(ABORT, 'security_domain_mismatch(install): the application declares a security domain that differs from the active applications of this process');
END;

CREATE TRIGGER apps_security_domain_on_activation
BEFORE UPDATE OF status ON apppkg_packages
WHEN NEW.status = 'active' AND OLD.status <> 'active'
 AND EXISTS (
    SELECT 1 FROM apppkg_packages o
     WHERE o.status = 'active'
       AND o.application_id <> NEW.application_id
       AND o.security_domain <> NEW.security_domain
)
BEGIN
    SELECT RAISE(ABORT, 'security_domain_mismatch(activation): the application declares a security domain that differs from the active applications of this process');
END;
