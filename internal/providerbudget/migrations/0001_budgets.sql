-- providerbudget module, migration 0001: external provider budgets, quotas and the reserve / settle /
-- release ledger (additive plan section 11.2, ADR-0025).
--
-- Every table carries application_id, tenant_id and workspace_id and every statement of the module
-- names all three. The columns mean the same thing as in internal/quotas, so a later per-tenant quota
-- task can share the counter layout:
--
--   budgets, counters, holds, rate_events  the scope of the BUDGET: a provider budget is application
--                                          level (tenant and workspace are ''), a tenant budget names
--                                          the tenant (workspace ''), a workspace or principal budget
--                                          names tenant and workspace.
--   reservations, usage                    the scope of the CALL that made them.
--
-- Money is an integer number of micro-units, never a float, capped at 2^53-1. Times are unix
-- milliseconds from the injected clock. Windows are UTC calendar days and months; window_start is the
-- first millisecond of the window.

CREATE TABLE providerbudget_budgets (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    budget_id      TEXT    NOT NULL CHECK (budget_id <> ''),
    kind           TEXT    NOT NULL CHECK (kind IN ('provider', 'tenant', 'workspace', 'principal')),
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
    CHECK (kind <> 'principal' OR (tenant_id <> ''   AND workspace_id <> '' AND principal_id <> ''))
) STRICT;

CREATE UNIQUE INDEX providerbudget_budgets_id
    ON providerbudget_budgets (application_id, budget_id);

-- One budget per subject, optional provider narrowing and channel.
CREATE UNIQUE INDEX providerbudget_budgets_subject
    ON providerbudget_budgets (application_id, tenant_id, workspace_id, kind, principal_id, provider_id, channel);

-- Settled consumption: dimension is 'cost' (micro-units) or a meter unit, per window.
CREATE TABLE providerbudget_counters (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    budget_id      TEXT    NOT NULL,
    dimension      TEXT    NOT NULL CHECK (dimension IN ('cost', 'toks_in', 'toks_out', 'toks_in_cached', 'images_in',
                       'images_out', 'audio_seconds_in', 'audio_seconds_out', 'file_bytes_in', 'requests')),
    window_kind    TEXT    NOT NULL CHECK (window_kind IN ('daily', 'monthly')),
    window_start   INTEGER NOT NULL CHECK (window_start >= 0),
    used           INTEGER NOT NULL CHECK (used >= 0 AND used <= 9007199254740991),
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, budget_id, dimension, window_kind, window_start),
    CHECK (application_id <> '')
) STRICT, WITHOUT ROWID;

-- A reservation: the call, its estimate and its lifecycle. A reserved row whose expires_at has passed
-- no longer holds anything (holds are ignored once expired), so a crash cannot lock a budget.
CREATE TABLE providerbudget_reservations (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    principal_id   TEXT    NOT NULL,
    reservation_id TEXT    NOT NULL CHECK (reservation_id <> ''),
    provider_id    TEXT    NOT NULL,
    channel        TEXT    NOT NULL CHECK (channel IN ('interactive', 'background')),
    state          TEXT    NOT NULL CHECK (state IN ('reserved', 'settled', 'released', 'expired')),
    budget_ids     TEXT    NOT NULL CHECK (json_valid(budget_ids) AND json_type(budget_ids) = 'array'),
    call           TEXT    NOT NULL CHECK (json_valid(call) AND json_type(call) = 'object'),
    reserved_micro INTEGER NOT NULL CHECK (reserved_micro >= 0),
    daily_start    INTEGER NOT NULL,
    monthly_start  INTEGER NOT NULL,
    created_at     INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    ended_at       INTEGER,
    CHECK (application_id <> '' AND tenant_id <> '' AND workspace_id <> '' AND principal_id <> ''),
    CHECK ((state = 'reserved') = (ended_at IS NULL))
) STRICT;

CREATE UNIQUE INDEX providerbudget_reservations_id
    ON providerbudget_reservations (application_id, tenant_id, workspace_id, reservation_id);

CREATE INDEX providerbudget_reservations_open
    ON providerbudget_reservations (application_id, state, expires_at);

-- What an open reservation holds against one budget, per dimension. Rows disappear on settle, release
-- and expiry. Concurrency is the number of live 'cost' holds of a budget.
CREATE TABLE providerbudget_holds (
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    budget_id      TEXT    NOT NULL,
    reservation_id TEXT    NOT NULL,
    dimension      TEXT    NOT NULL,
    amount         INTEGER NOT NULL CHECK (amount >= 0),
    daily_start    INTEGER NOT NULL,
    monthly_start  INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,
    PRIMARY KEY (application_id, tenant_id, workspace_id, budget_id, reservation_id, dimension),
    CHECK (application_id <> '')
) STRICT, WITHOUT ROWID;

CREATE INDEX providerbudget_holds_reservation
    ON providerbudget_holds (application_id, reservation_id);

-- One row per admitted request of a budget with a per-minute rate limit (a sliding window).
CREATE TABLE providerbudget_rate_events (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    budget_id      TEXT    NOT NULL,
    at             INTEGER NOT NULL,
    CHECK (application_id <> '')
) STRICT;

CREATE INDEX providerbudget_rate_events_budget
    ON providerbudget_rate_events (application_id, tenant_id, workspace_id, budget_id, at);

-- One usage record per settled reservation (no prompt, no response). charged_day is the UTC day the
-- spend was charged to: the day the call was reserved. charged_micro is what the budgets were charged,
-- which is the reserved upper bound for an uncertain call.
CREATE TABLE providerbudget_usage (
    seq            INTEGER PRIMARY KEY,
    application_id TEXT    NOT NULL,
    tenant_id      TEXT    NOT NULL,
    workspace_id   TEXT    NOT NULL,
    principal_id   TEXT    NOT NULL,
    usage_id       TEXT    NOT NULL CHECK (usage_id <> ''),
    reservation_id TEXT    NOT NULL,
    provider_id    TEXT    NOT NULL,
    channel        TEXT    NOT NULL CHECK (channel IN ('interactive', 'background')),
    currency       TEXT    NOT NULL CHECK (length(currency) = 3),
    outcome        TEXT    NOT NULL,
    reserved_micro INTEGER NOT NULL CHECK (reserved_micro >= 0),
    actual_micro   INTEGER CHECK (actual_micro IS NULL OR actual_micro >= 0),
    charged_micro  INTEGER NOT NULL CHECK (charged_micro >= 0),
    charged_day    INTEGER NOT NULL,
    recorded_at    INTEGER NOT NULL,
    document       TEXT    NOT NULL CHECK (json_valid(document) AND json_type(document) = 'object'),
    CHECK (application_id <> '' AND tenant_id <> '' AND workspace_id <> '' AND principal_id <> '')
) STRICT;

CREATE UNIQUE INDEX providerbudget_usage_id
    ON providerbudget_usage (application_id, tenant_id, workspace_id, usage_id);

CREATE UNIQUE INDEX providerbudget_usage_reservation
    ON providerbudget_usage (application_id, tenant_id, workspace_id, reservation_id);

CREATE INDEX providerbudget_usage_day
    ON providerbudget_usage (application_id, charged_day, provider_id);
