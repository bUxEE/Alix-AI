-- Used by TestRegisterAcceptsEmbedFS: proves a real embed.FS registers.
CREATE TABLE embedded_items (
    id             TEXT PRIMARY KEY,
    application_id TEXT NOT NULL,
    tenant_id      TEXT NOT NULL,
    workspace_id   TEXT NOT NULL
);
