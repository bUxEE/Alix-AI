import { ROLES, type Role } from "./access.ts";
import { toTimestamp, type Clock } from "./clock.ts";
import { inTransaction, TENANT_TABLES, type Database } from "./db.ts";
import { refuse } from "./errors.ts";
import { isIdentifier, newId, newReference } from "./ids.ts";
import { addDays, localDate, zonedToUtc } from "./time.ts";

// Disposable tenants for the integration audit's runtime probes (master plan
// 4.5 check 10). The audit creates a tenant with its sites and principals,
// seeds synthetic records, runs its probes through the real tools and then
// removes everything. All of it is synthetic: no name, phone number or
// address here belongs to a real person (phone numbers are the fictional
// +1-555-01xx block and addresses use the reserved .test domain).
//
// Everything here sits behind test support mode. A tenant that was not created
// as disposable is never touched by these operations.

export type TestSupportEnv = { db: Database; clock: Clock };

const DEFAULT_TIMEZONE = "Europe/Rome";

export type CreateTenantInput = {
  tenant_id?: unknown;
  name?: unknown;
  workspaces?: unknown;
  principals?: unknown;
};

type WorkspaceInput = { workspace_id: string; name: string; timezone: string };
type PrincipalInput = { principal_id: string; workspace_id: string; role: Role; display_name: string };

const isObject = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const validTimeZone = (timeZone: string): boolean => {
  try {
    new Intl.DateTimeFormat("en-CA", { timeZone });
    return true;
  } catch {
    return false;
  }
};

const textOr = (value: unknown, fallback: string, max: number, field: string): string => {
  if (value === undefined || value === null) {
    return fallback;
  }
  if (typeof value !== "string" || value.trim() === "" || [...value].length > max) {
    throw refuse("validation_failed", `${field} must be text of at most ${max} characters`, { field });
  }
  return value.trim();
};

const parseWorkspaces = (raw: unknown): WorkspaceInput[] => {
  if (!Array.isArray(raw) || raw.length === 0 || raw.length > 16) {
    throw refuse("validation_failed", "workspaces must list between 1 and 16 workspaces", { field: "workspaces" });
  }
  const seen = new Set<string>();
  return raw.map((item, index) => {
    if (!isObject(item) || !isIdentifier(item.workspace_id) || seen.has(item.workspace_id)) {
      throw refuse("validation_failed", "a workspace needs a unique valid workspace_id", { field: "workspaces" });
    }
    seen.add(item.workspace_id);
    const timezone = textOr(item.timezone, DEFAULT_TIMEZONE, 64, "timezone");
    if (!validTimeZone(timezone)) {
      throw refuse("validation_failed", "a workspace timezone must be an IANA zone", { field: "workspaces" });
    }
    return {
      workspace_id: item.workspace_id,
      name: textOr(item.name, `Synthetic Clinic Site ${index + 1}`, 100, "name"),
      timezone,
    };
  });
};

const parsePrincipals = (raw: unknown, workspaces: WorkspaceInput[]): PrincipalInput[] => {
  if (raw === undefined) {
    return [];
  }
  if (!Array.isArray(raw) || raw.length > 64) {
    throw refuse("validation_failed", "principals must list at most 64 principals", { field: "principals" });
  }
  const seen = new Set<string>();
  return raw.map((item, index) => {
    if (!isObject(item) || !isIdentifier(item.principal_id) || seen.has(item.principal_id)) {
      throw refuse("validation_failed", "a principal needs a unique valid principal_id", { field: "principals" });
    }
    seen.add(item.principal_id);
    if (!isIdentifier(item.workspace_id) || !workspaces.some((w) => w.workspace_id === item.workspace_id)) {
      throw refuse("validation_failed", "a principal must belong to one of the created workspaces", {
        field: "principals",
      });
    }
    // One clinic role per principal: the host's directory holds exactly one.
    const roles = item.roles;
    const role = Array.isArray(roles) && roles.length === 1 ? roles[0] : item.role;
    if (typeof role !== "string" || !(ROLES as readonly string[]).includes(role)) {
      throw refuse("validation_failed", "a principal holds exactly one role: receptionist, doctor or patient", {
        field: "principals",
      });
    }
    return {
      principal_id: item.principal_id,
      workspace_id: item.workspace_id,
      role: role as Role,
      display_name: textOr(item.display_name, `Synthetic ${role} ${index + 1}`, 100, "display_name"),
    };
  });
};

let syntheticCounter = 0;

const syntheticPerson = (n: number): { given: string; family: string; dob: string; phone: string; email: string } => ({
  given: ["Alma", "Bruno", "Carla", "Dario", "Elena", "Fabio", "Giada", "Hugo"][n % 8] ?? "Alma",
  family: `Testpatient${n}`,
  dob: `19${String(60 + (n % 40)).padStart(2, "0")}-${String(1 + (n % 12)).padStart(2, "0")}-${String(1 + (n % 28)).padStart(2, "0")}`,
  phone: `+1-555-01${String(n % 100).padStart(2, "0")}`,
  email: `patient${n}@example.test`,
});

const insertPatient = (
  env: TestSupportEnv,
  tenantId: string,
  siteId: string,
  names?: { given: string; family: string },
): string => {
  syntheticCounter += 1;
  const person = syntheticPerson(syntheticCounter);
  const id = newId("pat");
  const now = toTimestamp(env.clock.now());
  env.db
    .prepare(
      `INSERT INTO patients (tenant_id, site_id, id, given_name, family_name, date_of_birth, phone, email, version, created_at, updated_at)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
    )
    .run(
      tenantId,
      siteId,
      id,
      names?.given ?? person.given,
      names?.family ?? person.family,
      person.dob,
      person.phone,
      person.email,
      now,
      now,
    );
  return id;
};

const renamePatient = (env: TestSupportEnv, tenantId: string, siteId: string, id: string, given: string, family: string): void => {
  env.db
    .prepare("UPDATE patients SET given_name = ?, family_name = ? WHERE tenant_id = ? AND site_id = ? AND id = ?")
    .run(given, family, tenantId, siteId, id);
};

const insertDoctor = (env: TestSupportEnv, tenantId: string, siteId: string, name?: string): string => {
  syntheticCounter += 1;
  const id = newId("doc");
  env.db
    .prepare("INSERT INTO doctors (tenant_id, site_id, id, name, specialty) VALUES (?, ?, ?, ?, ?)")
    .run(tenantId, siteId, id, name ?? `Dr. Synthetic Doctor${syntheticCounter}`, "general practice");
  return id;
};

export type CreatedPrincipal = {
  principal_id: string;
  workspace_id: string;
  role: Role;
  patient_id: string | null;
  doctor_id: string | null;
};

export const createDisposableTenant = (
  env: TestSupportEnv,
  input: CreateTenantInput,
): { tenant_id: string; workspaces: string[]; principals: CreatedPrincipal[] } => {
  const tenantId = input.tenant_id === undefined ? newId("tnt_test") : input.tenant_id;
  if (!isIdentifier(tenantId)) {
    throw refuse("validation_failed", "tenant_id must be a valid scope identifier", { field: "tenant_id" });
  }
  const name = textOr(input.name, "Synthetic Clinic Group", 100, "name");
  const workspaces = parseWorkspaces(input.workspaces);
  const principals = parsePrincipals(input.principals, workspaces);
  return inTransaction(env.db, () => {
    if (env.db.prepare("SELECT 1 AS found FROM tenants WHERE id = ?").get(tenantId) !== undefined) {
      throw refuse("conflict", "a tenant with this identifier already exists", { field: "tenant_id" });
    }
    env.db
      .prepare("INSERT INTO tenants (id, name, disposable, created_at) VALUES (?, ?, 1, ?)")
      .run(tenantId, name, toTimestamp(env.clock.now()));
    for (const workspace of workspaces) {
      env.db
        .prepare("INSERT INTO sites (tenant_id, id, name, timezone) VALUES (?, ?, ?, ?)")
        .run(tenantId, workspace.workspace_id, workspace.name, workspace.timezone);
    }
    const created: CreatedPrincipal[] = [];
    for (const principal of principals) {
      let patientId: string | null = null;
      let doctorId: string | null = null;
      if (principal.role === "patient") {
        patientId = insertPatient(env, tenantId, principal.workspace_id);
      } else if (principal.role === "doctor") {
        doctorId = insertDoctor(env, tenantId, principal.workspace_id, `Dr. ${principal.display_name}`);
      }
      env.db
        .prepare(
          `INSERT INTO principals (tenant_id, site_id, id, display_name, role, patient_id, doctor_id)
           VALUES (?, ?, ?, ?, ?, ?, ?)`,
        )
        .run(tenantId, principal.workspace_id, principal.principal_id, principal.display_name, principal.role, patientId, doctorId);
      created.push({
        principal_id: principal.principal_id,
        workspace_id: principal.workspace_id,
        role: principal.role,
        patient_id: patientId,
        doctor_id: doctorId,
      });
    }
    return { tenant_id: tenantId, workspaces: workspaces.map((w) => w.workspace_id), principals: created };
  });
};

// ---- seeding ----------------------------------------------------------------

// Optional data a seed record may carry, so the audit's conversations can name what
// exists: a patient's given_name and family_name, a doctor's name, an appointment's
// reference. Without them the application invents synthetic ones.
export type SeedRecordInput = {
  ref: string;
  entity_type: string;
  workspace_id: string;
  principal_id?: string;
  given_name?: string;
  family_name?: string;
  name?: string;
  reference?: string;
};

export type SeededRecord = { ref: string; entity_type: string; entity_id: string; workspace_id: string };

const SEED_ENTITY_TYPES = ["patient", "doctor", "slot", "appointment"] as const;

export const requireDisposable = (env: TestSupportEnv, tenantId: unknown): string => {
  if (!isIdentifier(tenantId)) {
    throw refuse("validation_failed", "tenant_id must be a valid scope identifier", { field: "tenant_id" });
  }
  const row = env.db.prepare("SELECT disposable FROM tenants WHERE id = ?").get(tenantId) as
    | { disposable: number }
    | undefined;
  if (row === undefined) {
    throw refuse("scope_violation", "no such tenant", { field: "tenant_id" });
  }
  if (row.disposable !== 1) {
    throw refuse("permission_denied", "test support only operates on disposable tenants", { field: "tenant_id" });
  }
  return tenantId;
};

type SiteRow = { id: string; timezone: string };

// A seed text member: only on the entity type that owns it, a plain bounded string.
const optionalSeedText = (
  item: Record<string, unknown>,
  member: string,
  owner: string,
  entityType: string,
): string | undefined => {
  const value = item[member];
  if (value === undefined) {
    return undefined;
  }
  if (entityType !== owner || typeof value !== "string" || value.trim() === "" || [...value].length > 100) {
    throw refuse("validation_failed", `${member} is a bounded text of a ${owner} record`, { field: "records" });
  }
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/u.test(value)) {
    throw refuse("validation_failed", `${member} contains control characters`, { field: "records" });
  }
  return value.trim();
};

const nextSlotStart = (env: TestSupportEnv, tenantId: string, site: SiteRow): Date => {
  const last = env.db
    .prepare("SELECT MAX(starts_at) AS last FROM slots WHERE tenant_id = ? AND site_id = ?")
    .get(tenantId, site.id) as { last: string | null };
  if (last.last !== null) {
    return new Date(new Date(last.last).getTime() + 30 * 60_000);
  }
  // The first synthetic slot is tomorrow at 09:00 on the site's wall clock, so
  // it is always inside the booking window.
  const tomorrow = addDays(localDate(env.clock.now(), site.timezone), 1);
  return zonedToUtc(tomorrow, "09:00", site.timezone);
};

const firstDoctor = (env: TestSupportEnv, tenantId: string, siteId: string): string => {
  const row = env.db
    .prepare("SELECT id FROM doctors WHERE tenant_id = ? AND site_id = ? ORDER BY id LIMIT 1")
    .get(tenantId, siteId) as { id: string } | undefined;
  return row?.id ?? insertDoctor(env, tenantId, siteId);
};

const principalLink = (
  env: TestSupportEnv,
  tenantId: string,
  siteId: string,
  principalId: string,
): { patient_id: string | null; doctor_id: string | null; role: Role } => {
  const row = env.db
    .prepare("SELECT patient_id, doctor_id, role FROM principals WHERE tenant_id = ? AND site_id = ? AND id = ?")
    .get(tenantId, siteId, principalId) as { patient_id: string | null; doctor_id: string | null; role: Role } | undefined;
  if (row === undefined) {
    throw refuse("validation_failed", "principal_id is not a principal of that workspace", { field: "principal_id" });
  }
  return row;
};

const insertSlot = (
  env: TestSupportEnv,
  tenantId: string,
  site: SiteRow,
  doctorId: string,
  status: "free" | "booked",
  appointmentId: string | null,
): { id: string; startsAt: string } => {
  const id = newId("slot");
  const startsAt = toTimestamp(nextSlotStart(env, tenantId, site));
  env.db
    .prepare(
      `INSERT INTO slots (tenant_id, site_id, id, doctor_id, starts_at, duration_minutes, status, appointment_id, version, updated_at)
       VALUES (?, ?, ?, ?, ?, 30, ?, ?, 1, ?)`,
    )
    .run(tenantId, site.id, id, doctorId, startsAt, status, appointmentId, toTimestamp(env.clock.now()));
  return { id, startsAt };
};

export const seedRecords = (
  env: TestSupportEnv,
  input: { tenant_id?: unknown; records?: unknown },
): { tenant_id: string; records: SeededRecord[] } => {
  const tenantId = requireDisposable(env, input.tenant_id);
  if (!Array.isArray(input.records) || input.records.length === 0 || input.records.length > 256) {
    throw refuse("validation_failed", "records must list between 1 and 256 records", { field: "records" });
  }
  const specs: SeedRecordInput[] = input.records.map((item) => {
    if (
      !isObject(item) ||
      !isIdentifier(item.ref) ||
      !isIdentifier(item.workspace_id) ||
      typeof item.entity_type !== "string" ||
      !(SEED_ENTITY_TYPES as readonly string[]).includes(item.entity_type) ||
      (item.principal_id !== undefined && !isIdentifier(item.principal_id))
    ) {
      throw refuse("validation_failed", "a record needs ref, workspace_id and an entity_type of patient, doctor, slot or appointment", {
        field: "records",
      });
    }
    const given = optionalSeedText(item, "given_name", "patient", item.entity_type);
    const family = optionalSeedText(item, "family_name", "patient", item.entity_type);
    const doctorName = optionalSeedText(item, "name", "doctor", item.entity_type);
    const reference = optionalSeedText(item, "reference", "appointment", item.entity_type);
    if (reference !== undefined && !/^APT-[0-9A-HJKMNP-TV-Z]{6}$/u.test(reference)) {
      throw refuse("validation_failed", "a seeded appointment reference looks like APT-7K3Q9X", { field: "records" });
    }
    if ((given === undefined) !== (family === undefined)) {
      throw refuse("validation_failed", "a seeded patient name needs both given_name and family_name", { field: "records" });
    }
    return {
      ref: item.ref,
      entity_type: item.entity_type,
      workspace_id: item.workspace_id,
      ...(typeof item.principal_id === "string" ? { principal_id: item.principal_id } : {}),
      ...(given === undefined || family === undefined ? {} : { given_name: given, family_name: family }),
      ...(doctorName === undefined ? {} : { name: doctorName }),
      ...(reference === undefined ? {} : { reference }),
    };
  });
  return inTransaction(env.db, () => {
    const seeded: SeededRecord[] = [];
    for (const spec of specs) {
      const site = env.db
        .prepare("SELECT id, timezone FROM sites WHERE tenant_id = ? AND id = ?")
        .get(tenantId, spec.workspace_id) as SiteRow | undefined;
      if (site === undefined) {
        throw refuse("validation_failed", "a record names a workspace the tenant does not have", { field: "records" });
      }
      const link = spec.principal_id === undefined ? undefined : principalLink(env, tenantId, site.id, spec.principal_id);
      let entityId: string;
      switch (spec.entity_type) {
        case "patient":
          entityId = link?.patient_id ?? insertPatient(env, tenantId, site.id, spec.given_name === undefined || spec.family_name === undefined ? undefined : { given: spec.given_name, family: spec.family_name });
          if (link?.patient_id != null && spec.given_name !== undefined && spec.family_name !== undefined) {
            renamePatient(env, tenantId, site.id, entityId, spec.given_name, spec.family_name);
          }
          break;
        case "doctor":
          entityId = link?.doctor_id ?? insertDoctor(env, tenantId, site.id, spec.name);
          if (link?.doctor_id != null && spec.name !== undefined) {
            env.db
              .prepare("UPDATE doctors SET name = ? WHERE tenant_id = ? AND site_id = ? AND id = ?")
              .run(spec.name, tenantId, site.id, entityId);
          }
          break;
        case "slot":
          entityId = insertSlot(env, tenantId, site, link?.doctor_id ?? firstDoctor(env, tenantId, site.id), "free", null).id;
          break;
        default: {
          const doctorId = link?.doctor_id ?? firstDoctor(env, tenantId, site.id);
          const patientId = link?.patient_id ?? insertPatient(env, tenantId, site.id);
          const reference = spec.reference ?? newReference();
          if (
            spec.reference !== undefined &&
            env.db.prepare("SELECT 1 AS found FROM appointments WHERE tenant_id = ? AND reference = ?").get(tenantId, reference) !== undefined
          ) {
            throw refuse("conflict", "a seeded appointment reference is already in use in this tenant", { field: "records" });
          }
          const appointmentId = newId("apt");
          const slot = insertSlot(env, tenantId, site, doctorId, "booked", appointmentId);
          const now = toTimestamp(env.clock.now());
          env.db
            .prepare(
              `INSERT INTO appointments (tenant_id, site_id, id, reference, slot_id, patient_id, doctor_id, starts_at, status,
                                         version, created_at, updated_at)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'booked', 1, ?, ?)`,
            )
            .run(tenantId, site.id, appointmentId, reference, slot.id, patientId, doctorId, slot.startsAt, now, now);
          entityId = appointmentId;
        }
      }
      seeded.push({ ref: spec.ref, entity_type: spec.entity_type, entity_id: entityId, workspace_id: site.id });
    }
    return { tenant_id: tenantId, records: seeded };
  });
};

// ---- changing a record --------------------------------------------------------

// The records a capability's data versions are taken over (patient, slot and appointment
// each carry a revision counter), by the table that holds them. The table is picked from this
// list and never built from the caller's text.
const CHANGEABLE_TABLES = { patient: "patients", slot: "slots", appointment: "appointments" } as const;

export type ChangedRecord = { tenant_id: string; entity_type: string; entity_id: string; changed: true };

// Moves the revision counter of one record of a disposable tenant, and nothing else: the
// record keeps every value it had (a free slot stays free, a booked appointment stays
// booked), so the only thing that differs is the snapshot version a preparation made
// before it was taken over. The integration audit uses it to show that validate notices a
// change and that a commit prepared before the change is refused as stale, which it cannot
// show by committing an action, because that changes the business state too.
export const changeRecord = (
  env: TestSupportEnv,
  input: { tenant_id?: unknown; entity_type?: unknown; entity_id?: unknown },
): ChangedRecord => {
  const tenantId = requireDisposable(env, input.tenant_id);
  const entityType = input.entity_type;
  if (typeof entityType !== "string" || !Object.hasOwn(CHANGEABLE_TABLES, entityType)) {
    throw refuse("validation_failed", "entity_type must be patient, slot or appointment", { field: "entity_type" });
  }
  if (!isIdentifier(input.entity_id)) {
    throw refuse("validation_failed", "entity_id must be a valid record identifier", { field: "entity_id" });
  }
  const table = CHANGEABLE_TABLES[entityType as keyof typeof CHANGEABLE_TABLES];
  const result = env.db
    .prepare(`UPDATE ${table} SET version = version + 1, updated_at = ? WHERE tenant_id = ? AND id = ?`)
    .run(toTimestamp(env.clock.now()), tenantId, input.entity_id);
  if (Number(result.changes) !== 1) {
    // The same answer for a record that does not exist and one of another tenant: the
    // tenant is the only scope this tool has, and it is a disposable one.
    throw refuse("scope_violation", "no such record in that tenant", { field: "entity_id" });
  }
  return { tenant_id: tenantId, entity_type: entityType, entity_id: input.entity_id, changed: true };
};

// ---- cleanup ----------------------------------------------------------------

export type CleanupReport = {
  tenant_id: string;
  existed: boolean;
  removed: Record<string, number>;
  remaining: Record<string, number>;
};

const countsFor = (env: TestSupportEnv, tenantId: string): Record<string, number> => {
  const counts: Record<string, number> = {};
  for (const table of [...TENANT_TABLES, "tenants"] as const) {
    const column = table === "tenants" ? "id" : "tenant_id";
    const row = env.db.prepare(`SELECT COUNT(*) AS n FROM ${table} WHERE ${column} = ?`).get(tenantId) as { n: number };
    counts[table] = Number(row.n);
  }
  return counts;
};

// Removes a disposable tenant and everything the run created, then proves it:
// every table is counted again and a non-zero count fails the call. A tenant
// that is not disposable is refused, and an unknown tenant is already clean.
export const cleanupDisposableTenant = (env: TestSupportEnv, tenantId: unknown): CleanupReport => {
  if (!isIdentifier(tenantId)) {
    throw refuse("validation_failed", "tenant_id must be a valid scope identifier", { field: "tenant_id" });
  }
  const row = env.db.prepare("SELECT disposable FROM tenants WHERE id = ?").get(tenantId) as
    | { disposable: number }
    | undefined;
  if (row === undefined) {
    const none = countsFor(env, tenantId);
    return { tenant_id: tenantId, existed: false, removed: Object.fromEntries(Object.keys(none).map((k) => [k, 0])), remaining: none };
  }
  if (row.disposable !== 1) {
    throw refuse("permission_denied", "test support only operates on disposable tenants", { field: "tenant_id" });
  }
  const before = countsFor(env, tenantId);
  inTransaction(env.db, () => {
    for (const table of TENANT_TABLES) {
      env.db.prepare(`DELETE FROM ${table} WHERE tenant_id = ?`).run(tenantId);
    }
    env.db.prepare("DELETE FROM tenants WHERE id = ? AND disposable = 1").run(tenantId);
  });
  const remaining = countsFor(env, tenantId);
  if (Object.values(remaining).some((n) => n !== 0)) {
    throw refuse("internal", "the disposable tenant was not fully removed");
  }
  return { tenant_id: tenantId, existed: true, removed: before, remaining };
};

// What the disposable-tenant operations did, for the gate: how many rows of
// each kind exist across all tenants.
export const databaseTotals = (db: Database): Record<string, number> => {
  const totals: Record<string, number> = {};
  for (const table of ["tenants", ...TENANT_TABLES] as const) {
    totals[table] = Number((db.prepare(`SELECT COUNT(*) AS n FROM ${table}`).get() as { n: number }).n);
  }
  const keyed = db
    .prepare("SELECT capability_id, COUNT(*) AS n FROM idempotency GROUP BY capability_id ORDER BY capability_id")
    .all() as { capability_id: string; n: number }[];
  for (const k of keyed) {
    totals[`idempotency:${k.capability_id}`] = Number(k.n);
  }
  return totals;
};
