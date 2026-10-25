CREATE TABLE appointments (
  id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  workspace_id TEXT NOT NULL,
  patient_id TEXT NOT NULL,
  starts_at TEXT NOT NULL
);
