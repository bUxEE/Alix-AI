-- Integration audit runs and operator acceptances (master plan 4.5, 9.4).
--
-- Like the package table of internal/apppkg and the publisher keys of
-- internal/trust, these tables are application-wide: an audit belongs to one
-- package of one application and serves every tenant of it, and holds no tenant
-- data, so they have no tenant or workspace column and the module's tests
-- exempt them from the scope lint on purpose. Every query in
-- internal/integrationaudit filters on application_id.
--
-- Both tables are evidence and are append-only: a run is never changed or
-- removed, and neither is an acceptance. A re-audit adds a run; it does not
-- replace one, and activation always uses the newest.
CREATE TABLE integrationaudit_runs (
    -- insertion order; the newest run of a package is the one with the highest seq
    seq            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id         TEXT    NOT NULL UNIQUE,
    application_id TEXT    NOT NULL,
    -- the package audited: the manifest hash of the installed package
    manifest_hash  TEXT    NOT NULL CHECK (length(manifest_hash) = 64),
    -- SHA-256 of the canonical report; the attestation signs it
    report_sha256  TEXT    NOT NULL CHECK (length(report_sha256) = 64),
    -- what the report permits: blocked, needs_acceptance or clear
    disposition    TEXT    NOT NULL CHECK (disposition IN ('blocked', 'needs_acceptance', 'clear')),
    -- unix milliseconds, from the injected clock
    issued_at      INTEGER NOT NULL,
    -- the signed report as canonical JSON (report plus attestation)
    document       TEXT    NOT NULL
) STRICT;

CREATE INDEX integrationaudit_runs_by_package ON integrationaudit_runs (application_id, manifest_hash, seq);

CREATE TRIGGER integrationaudit_runs_no_update
BEFORE UPDATE ON integrationaudit_runs
BEGIN
    SELECT RAISE(ABORT, 'an integration audit run is never changed');
END;

CREATE TRIGGER integrationaudit_runs_no_delete
BEFORE DELETE ON integrationaudit_runs
BEGIN
    SELECT RAISE(ABORT, 'an integration audit run is never deleted');
END;

-- A warning an authorised operator accepted for one package. It is bound to the
-- package (application and manifest hash) and to the finding by its key, not to
-- a run, so a re-audit that finds the same warning needs no second acceptance
-- and a finding that changed needs a new one. An acceptance records who and
-- when; it carries nothing that could widen a capability or a scope, and
-- nothing reads it except the activation gate.
CREATE TABLE integrationaudit_acceptances (
    application_id TEXT    NOT NULL,
    manifest_hash  TEXT    NOT NULL CHECK (length(manifest_hash) = 64),
    finding_key    TEXT    NOT NULL CHECK (length(finding_key) = 16),
    check_id       TEXT    NOT NULL,
    capability     TEXT    NOT NULL,
    operator_id    TEXT    NOT NULL CHECK (length(operator_id) >= 1),
    reason         TEXT    NOT NULL CHECK (length(reason) >= 1),
    -- the run whose report contained the finding when it was accepted
    run_id         TEXT    NOT NULL,
    accepted_at    INTEGER NOT NULL,
    PRIMARY KEY (application_id, manifest_hash, finding_key)
) STRICT;

CREATE TRIGGER integrationaudit_acceptances_no_update
BEFORE UPDATE ON integrationaudit_acceptances
BEGIN
    SELECT RAISE(ABORT, 'an acceptance is never changed');
END;

CREATE TRIGGER integrationaudit_acceptances_no_delete
BEFORE DELETE ON integrationaudit_acceptances
BEGIN
    SELECT RAISE(ABORT, 'an acceptance is never deleted');
END;
