-- credentials module, migration 0003: optional owner_principal_id for SIWC (and similar)
-- connections that must resolve only for the principal whose plan they are.
-- Empty string means tenant-scoped as before (manual keys, OpenRouter issued keys, Gemini OAuth).

ALTER TABLE credentials_refs ADD COLUMN owner_principal_id TEXT NOT NULL DEFAULT '';
