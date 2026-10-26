-- providerreg module, migration 0003: TTS principal bindings and speech options (ADR 0043).
--
-- SQLite cannot alter a CHECK constraint in place: rebuild the bindings table so role may be
-- tts, and add optional voice / language columns for speech roles.

CREATE TABLE providerreg_principal_bindings_v3 (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    principal_id   TEXT    NOT NULL CHECK (principal_id <> ''),
    role           TEXT    NOT NULL CHECK (role IN ('conversational_controller', 'language_realization', 'stt', 'tts')),
    profile_id     TEXT    NOT NULL CHECK (profile_id <> ''),
    provider_id    TEXT    NOT NULL CHECK (provider_id <> ''),
    model_id       TEXT    NOT NULL CHECK (model_id <> ''),
    voice          TEXT,
    language       TEXT,
    updated_at     INTEGER NOT NULL,
    CHECK (application_id <> '' AND tenant_id = '' AND workspace_id = '')
) STRICT;

INSERT INTO providerreg_principal_bindings_v3
    (seq, application_id, tenant_id, workspace_id, principal_id, role, profile_id, provider_id, model_id, updated_at)
SELECT seq, application_id, tenant_id, workspace_id, principal_id, role, profile_id, provider_id, model_id, updated_at
  FROM providerreg_principal_bindings;

DROP TABLE providerreg_principal_bindings;

ALTER TABLE providerreg_principal_bindings_v3 RENAME TO providerreg_principal_bindings;

CREATE UNIQUE INDEX providerreg_principal_bindings_key
    ON providerreg_principal_bindings (application_id, tenant_id, workspace_id, principal_id, role);
