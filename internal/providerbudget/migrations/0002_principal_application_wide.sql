-- Allow kind principal_application: application-wide principal budgets (empty tenant_id and
-- workspace_id) so one hard stop covers every tenant the principal acts in (ADR 0041 / 1.3.0).
-- Workspace-bound principal rows stay kind=principal. SQLite cannot ALTER a CHECK; rebuild.

CREATE TABLE providerbudget_budgets_new (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    budget_id      TEXT    NOT NULL CHECK (budget_id <> ''),
    kind           TEXT    NOT NULL CHECK (kind IN ('provider', 'tenant', 'workspace', 'principal', 'principal_application')),
    provider_id    TEXT    NOT NULL,
    principal_id   TEXT    NOT NULL,
    channel        TEXT    NOT NULL CHECK (channel IN ('interactive', 'background')),
    currency       TEXT    NOT NULL CHECK (length(currency) = 3),
    revision       INTEGER NOT NULL CHECK (revision >= 1),
    document       TEXT    NOT NULL CHECK (json_valid(document) AND json_type(document) = 'object'),
    updated_at     INTEGER NOT NULL,
    CHECK (application_id <> ''),
    CHECK (kind <> 'provider'  OR (provider_id <> '' AND tenant_id = ''  AND workspace_id = ''  AND principal_id = '')),
    CHECK (kind <> 'tenant'    OR (tenant_id <> ''   AND workspace_id = ''  AND principal_id = '')),
    CHECK (kind <> 'workspace' OR (tenant_id <> ''   AND workspace_id <> '' AND principal_id = '')),
    CHECK (kind <> 'principal' OR (tenant_id <> ''   AND workspace_id <> '' AND principal_id <> '')),
    CHECK (kind <> 'principal_application' OR (tenant_id = '' AND workspace_id = '' AND principal_id <> ''))
) STRICT;

INSERT INTO providerbudget_budgets_new
    SELECT seq, application_id, tenant_id, workspace_id, budget_id, kind, provider_id, principal_id,
           channel, currency, revision, document, updated_at
      FROM providerbudget_budgets;

DROP TABLE providerbudget_budgets;
ALTER TABLE providerbudget_budgets_new RENAME TO providerbudget_budgets;

CREATE UNIQUE INDEX providerbudget_budgets_id
    ON providerbudget_budgets (application_id, budget_id);

CREATE UNIQUE INDEX providerbudget_budgets_subject
    ON providerbudget_budgets (application_id, tenant_id, workspace_id, kind, principal_id, provider_id, channel);
