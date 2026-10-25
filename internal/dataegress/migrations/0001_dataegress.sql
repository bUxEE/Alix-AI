-- dataegress module, migration 0001: the data-egress policy store and the global
-- external-provider kill switch (external provider mode additive plan, section 6).
--
-- dataegress_policies holds one row per egress policy, one policy per level and
-- target. A policy belongs to one of five levels and the level decides which parts
-- of the key are set; every coarser level leaves the finer columns empty (the
-- same convention as the settings module), and the CHECK refuses any other shape,
-- so a row can never be half scoped:
--
--   deployment  deployment_id only (the whole installation)
--   application deployment_id, application_id
--   tenant      ... and tenant_id
--   workspace   ... and workspace_id
--   source      ... and source_id (a RAG source of that workspace)
--
-- application_id, tenant_id and workspace_id exist on every row as the store rule
-- requires (TEXT NOT NULL); the deployment_id and source_id columns extend the
-- key. Every statement of the module names every key column: a request can never
-- read or write a row of another deployment, application, tenant or workspace.
-- The key is built from a verified scope, never from a request body.
--
-- document is the canonical JSON of the typed policy (providers.EgressPolicy,
-- schemas/providers/egress-policy), the revision is compared on every update
-- (optimistic concurrency), and times are unix milliseconds from the injected
-- clock. A level with no row is a level with no policy, which means disabled.

CREATE TABLE dataegress_policies (
    seq            INTEGER PRIMARY KEY,
    policy_id      TEXT    NOT NULL CHECK (policy_id <> ''),
    deployment_id  TEXT    NOT NULL,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    source_id      TEXT    NOT NULL,
    level          TEXT    NOT NULL CHECK (level IN ('deployment', 'application', 'tenant', 'workspace', 'source')),
    revision       INTEGER NOT NULL CHECK (revision >= 1),
    document       TEXT    NOT NULL CHECK (json_valid(document) AND json_type(document) = 'object'),
    updated_by     TEXT    NOT NULL CHECK (updated_by <> ''),
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    CHECK (deployment_id <> ''),
    CHECK (
        (level = 'deployment'  AND application_id = ''  AND tenant_id = ''  AND workspace_id = ''  AND source_id = '')
     OR (level = 'application' AND application_id <> '' AND tenant_id = ''  AND workspace_id = ''  AND source_id = '')
     OR (level = 'tenant'      AND application_id <> '' AND tenant_id <> '' AND workspace_id = ''  AND source_id = '')
     OR (level = 'workspace'   AND application_id <> '' AND tenant_id <> '' AND workspace_id <> '' AND source_id = '')
     OR (level = 'source'      AND application_id <> '' AND tenant_id <> '' AND workspace_id <> '' AND source_id <> '')
    )
) STRICT;

CREATE UNIQUE INDEX dataegress_policies_key
    ON dataegress_policies (deployment_id, application_id, tenant_id, workspace_id, level, source_id);

CREATE UNIQUE INDEX dataegress_policies_id ON dataegress_policies (policy_id);

-- dataegress_kill_switch holds the persistent emergency switch that disables every
-- external request of one deployment. A deployment with no row, or with engaged = 0,
-- is not under the switch (external mode is still off unless policy enables it).
-- The switch is deployment wide, so the three scope columns are always empty.
-- reason is a fixed code, never free text; changed_by is the operator who last
-- engaged or released it.

CREATE TABLE dataegress_kill_switch (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    deployment_id  TEXT    NOT NULL CHECK (deployment_id <> ''),
    engaged        INTEGER NOT NULL CHECK (engaged IN (0, 1)),
    reason         TEXT    NOT NULL CHECK (reason <> ''),
    changed_by     TEXT    NOT NULL CHECK (changed_by <> ''),
    changed_at     INTEGER NOT NULL,
    CHECK (application_id = '' AND tenant_id = '' AND workspace_id = '')
) STRICT;

CREATE UNIQUE INDEX dataegress_kill_switch_key ON dataegress_kill_switch (deployment_id);
