-- Single-use nonce (jti) cache for application context tokens (ADR 0013).
--
-- A row exists from the first accepted presentation of a token until its
-- expiry plus the clock-skew allowance, after which the token could not be
-- accepted anyway. The primary key is what makes a second presentation lose
-- the race. The scope columns come from the verified token, never from the
-- request, and are in the key so no query here reaches across scopes.
CREATE TABLE identity_replay (
    application_id TEXT NOT NULL,
    tenant_id      TEXT NOT NULL,
    workspace_id   TEXT NOT NULL,
    jti            TEXT NOT NULL,
    seen_at        INTEGER NOT NULL, -- unix milliseconds, UTC
    expires_at     INTEGER NOT NULL, -- unix milliseconds, UTC
    PRIMARY KEY (application_id, tenant_id, workspace_id, jti)
) WITHOUT ROWID;

CREATE INDEX identity_replay_expires ON identity_replay (expires_at);
