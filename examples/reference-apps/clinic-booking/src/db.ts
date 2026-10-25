import { chmodSync, existsSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";
import { DatabaseSync } from "node:sqlite";

// Every table that belongs to a tenant carries tenant_id and site_id (the
// workspace), and every query filters on both. Revision counters (`version`)
// are per record: they change exactly when that record changes.
const SCHEMA = `
CREATE TABLE IF NOT EXISTS tenants (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  disposable  INTEGER NOT NULL DEFAULT 0 CHECK (disposable IN (0, 1)),
  created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sites (
  tenant_id  TEXT NOT NULL REFERENCES tenants (id),
  id         TEXT NOT NULL,
  name       TEXT NOT NULL,
  timezone   TEXT NOT NULL,
  PRIMARY KEY (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS principals (
  tenant_id     TEXT NOT NULL,
  site_id       TEXT NOT NULL,
  id            TEXT NOT NULL,
  display_name  TEXT NOT NULL,
  role          TEXT NOT NULL CHECK (role IN ('receptionist', 'doctor', 'patient')),
  patient_id    TEXT,
  doctor_id     TEXT,
  PRIMARY KEY (tenant_id, site_id, id),
  FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS doctors (
  tenant_id  TEXT NOT NULL,
  site_id    TEXT NOT NULL,
  id         TEXT NOT NULL,
  name       TEXT NOT NULL,
  specialty  TEXT NOT NULL,
  PRIMARY KEY (tenant_id, site_id, id),
  FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS patients (
  tenant_id      TEXT NOT NULL,
  site_id        TEXT NOT NULL,
  id             TEXT NOT NULL,
  given_name     TEXT NOT NULL,
  family_name    TEXT NOT NULL,
  date_of_birth  TEXT NOT NULL,
  phone          TEXT,
  email          TEXT,
  version        INTEGER NOT NULL DEFAULT 1,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL,
  PRIMARY KEY (tenant_id, site_id, id),
  FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id)
);

CREATE TABLE IF NOT EXISTS slots (
  tenant_id         TEXT NOT NULL,
  site_id           TEXT NOT NULL,
  id                TEXT NOT NULL,
  doctor_id         TEXT NOT NULL,
  starts_at         TEXT NOT NULL,
  duration_minutes  INTEGER NOT NULL,
  status            TEXT NOT NULL CHECK (status IN ('free', 'booked', 'blocked')),
  appointment_id    TEXT,
  version           INTEGER NOT NULL DEFAULT 1,
  updated_at        TEXT NOT NULL,
  PRIMARY KEY (tenant_id, site_id, id),
  FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id)
);
CREATE INDEX IF NOT EXISTS slots_by_time ON slots (tenant_id, site_id, status, starts_at);

CREATE TABLE IF NOT EXISTS appointments (
  tenant_id     TEXT NOT NULL,
  site_id       TEXT NOT NULL,
  id            TEXT NOT NULL,
  reference     TEXT NOT NULL,
  slot_id       TEXT NOT NULL,
  patient_id    TEXT NOT NULL,
  doctor_id     TEXT NOT NULL,
  starts_at     TEXT NOT NULL,
  status        TEXT NOT NULL CHECK (status IN ('booked', 'cancelled')),
  reason        TEXT,
  cancel_reason TEXT,
  version       INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL,
  PRIMARY KEY (tenant_id, site_id, id),
  FOREIGN KEY (tenant_id, site_id) REFERENCES sites (tenant_id, id)
);
-- The database itself refuses a double booking: one active appointment per slot.
CREATE UNIQUE INDEX IF NOT EXISTS appointments_one_active_per_slot
  ON appointments (tenant_id, site_id, slot_id) WHERE status = 'booked';
CREATE UNIQUE INDEX IF NOT EXISTS appointments_reference
  ON appointments (tenant_id, reference);

CREATE TABLE IF NOT EXISTS idempotency (
  tenant_id      TEXT NOT NULL,
  site_id        TEXT NOT NULL,
  key            TEXT NOT NULL,
  capability_id  TEXT NOT NULL,
  principal_id   TEXT NOT NULL,
  fingerprint    TEXT NOT NULL,
  response_json  TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  PRIMARY KEY (tenant_id, site_id, key)
);

-- The synthetic outbox: a notification the application would send. Nothing
-- leaves the process; the row is the follow-up the receipt points at.
CREATE TABLE IF NOT EXISTS notifications (
  tenant_id       TEXT NOT NULL,
  site_id         TEXT NOT NULL,
  id              TEXT NOT NULL,
  kind            TEXT NOT NULL,
  appointment_id  TEXT NOT NULL,
  patient_id      TEXT NOT NULL,
  state           TEXT NOT NULL,
  created_at      TEXT NOT NULL,
  PRIMARY KEY (tenant_id, site_id, id)
);
`;

// Every table, in the order rows may be deleted (children first).
export const TENANT_TABLES = [
  "notifications",
  "idempotency",
  "appointments",
  "slots",
  "patients",
  "doctors",
  "principals",
  "sites",
] as const;

export type Database = DatabaseSync;

export const openDatabase = (path: string): Database => {
  if (path !== ":memory:") {
    // A file database holds personal data even when it is synthetic: the
    // directory and the file are owner-only.
    const directory = dirname(path);
    if (!existsSync(directory)) {
      mkdirSync(directory, { recursive: true, mode: 0o700 });
    }
  }
  const db = new DatabaseSync(path);
  db.exec("PRAGMA foreign_keys = ON");
  db.exec("PRAGMA busy_timeout = 5000");
  if (path !== ":memory:") {
    db.exec("PRAGMA journal_mode = WAL");
  }
  db.exec(SCHEMA);
  if (path !== ":memory:") {
    try {
      chmodSync(path, 0o600);
    } catch {
      // Windows has no POSIX modes; the directory ACL is the protection there.
    }
  }
  return db;
};

// Runs fn inside one write transaction; any throw rolls it back.
export const inTransaction = <T>(db: Database, fn: () => T): T => {
  db.exec("BEGIN IMMEDIATE");
  try {
    const out = fn();
    db.exec("COMMIT");
    return out;
  } catch (error) {
    db.exec("ROLLBACK");
    throw error;
  }
};
