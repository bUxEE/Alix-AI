-- providerreg module, migration 0001: the registry of external provider and model profiles
-- (additive plan section 5).
--
-- A profile belongs to one application: the application is the unit that owns the operator's
-- provider configuration, and the profile's permitted_scopes then narrow its use to tenants and
-- workspaces INSIDE that application. So every row is application-level: application_id is set,
-- tenant_id and workspace_id are empty, and the CHECK refuses any other shape (the same layout as
-- the settings module's deployment level). Every statement of the module names all three scope
-- columns, so one application can never read or change another application's profiles.
--
-- document is the canonical JSON of the typed profile (schemas/providers/provider-profile). It
-- holds the credential REFERENCE only: the profile has no member that can hold a secret. The
-- indexed columns repeat what the module selects on and are rewritten with the document in the
-- same statement; lifecycle and quarantine_reason are kept consistent by the CHECK (a profile is
-- quarantined exactly when it says why).
--
-- providerreg_events is the append-only record of lifecycle changes, billing acknowledgements,
-- conformance recordings and fallbacks, with who and when. reason is a fixed code, never free
-- text. Times are unix milliseconds from the injected clock.

CREATE TABLE providerreg_profiles (
    seq               INTEGER PRIMARY KEY,
    application_id    TEXT    NOT NULL,
    tenant_id         TEXT    NOT NULL,
    workspace_id      TEXT    NOT NULL,
    profile_id        TEXT    NOT NULL CHECK (profile_id <> ''),
    provider_id       TEXT    NOT NULL CHECK (provider_id <> ''),
    model_id          TEXT    NOT NULL CHECK (model_id <> ''),
    model_version     TEXT    NOT NULL CHECK (model_version <> ''),
    lifecycle         TEXT    NOT NULL CHECK (lifecycle IN ('candidate', 'qualified', 'quarantined', 'retired')),
    quarantine_reason TEXT,
    document          TEXT    NOT NULL CHECK (json_valid(document) AND json_type(document) = 'object'),
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL,
    CHECK (application_id <> '' AND tenant_id = '' AND workspace_id = ''),
    CHECK ((lifecycle = 'quarantined') = (quarantine_reason IS NOT NULL))
) STRICT;

CREATE UNIQUE INDEX providerreg_profiles_key
    ON providerreg_profiles (application_id, tenant_id, workspace_id, profile_id);

CREATE TABLE providerreg_events (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    profile_id     TEXT    NOT NULL CHECK (profile_id <> ''),
    kind           TEXT    NOT NULL CHECK (kind <> ''),
    from_lifecycle TEXT    NOT NULL,
    to_lifecycle   TEXT    NOT NULL,
    reason         TEXT    NOT NULL,
    related_id     TEXT    NOT NULL,
    actor          TEXT    NOT NULL CHECK (actor <> ''),
    at             INTEGER NOT NULL,
    CHECK (application_id <> '' AND tenant_id = '' AND workspace_id = '')
) STRICT;

CREATE INDEX providerreg_events_profile
    ON providerreg_events (application_id, tenant_id, workspace_id, profile_id, seq);
