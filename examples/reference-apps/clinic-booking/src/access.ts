import type { Database } from "./db.ts";
import { refuse } from "./errors.ts";
import { isIdentifier } from "./ids.ts";

export const APPLICATION_ID = "clinic-booking";

export type Role = "receptionist" | "doctor" | "patient";

export const ROLES: readonly Role[] = ["receptionist", "doctor", "patient"];

export type Permission =
  | "availability.read"
  | "patients.read"
  | "patients.update_contact"
  | "appointments.book"
  | "appointments.cancel";

// The capabilities of the application and the one permission each needs. The
// package declares the same permissions; a test keeps both in step.
export const CAPABILITY_PERMISSION = {
  "availability.list": "availability.read",
  "patients.find": "patients.read",
  "appointments.book": "appointments.book",
  "appointments.cancel": "appointments.cancel",
  "patients.update_contact": "patients.update_contact",
} as const satisfies Record<string, Permission>;

export type CapabilityId = keyof typeof CAPABILITY_PERMISSION;

// What a role may do at all. Which records it may do it to is a second,
// record-level question answered by the visibility rules below.
export const ROLE_PERMISSIONS: Record<Role, readonly Permission[]> = {
  receptionist: [
    "availability.read",
    "patients.read",
    "patients.update_contact",
    "appointments.book",
    "appointments.cancel",
  ],
  doctor: ["availability.read", "patients.read", "appointments.cancel"],
  patient: [
    "availability.read",
    "patients.read",
    "patients.update_contact",
    "appointments.book",
    "appointments.cancel",
  ],
};

// The capabilities a role is granted (the policy file's role_grants).
export const roleCapabilities = (role: Role): CapabilityId[] =>
  (Object.keys(CAPABILITY_PERMISSION) as CapabilityId[]).filter((capability) =>
    ROLE_PERMISSIONS[role].includes(CAPABILITY_PERMISSION[capability]),
  );

// The identity AlixAI sends with every call (adapters.Identity): the minimum
// the host needs to reauthorise the request itself.
export type Identity = {
  application_id: string;
  tenant_id: string;
  workspace_id: string;
  principal_id: string;
  roles: string[];
  session_id: string;
};

export type Caller = {
  tenantId: string;
  siteId: string;
  siteName: string;
  timezone: string;
  principalId: string;
  role: Role;
  patientId: string | null;
  doctorId: string | null;
  sessionId: string;
};

const present = (value: unknown): boolean => typeof value === "string" && value.trim() !== "";

// Missing levels are `scope_missing`; a present but malformed level is
// `scope_violation`; both before anything is read. A missing or doubtful
// identity never falls back to a default tenant or role.
export const parseIdentity = (raw: unknown): Identity => {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    throw refuse("scope_missing", "the request carries no identity context");
  }
  const input = raw as Record<string, unknown>;
  const levels = ["application_id", "tenant_id", "workspace_id", "principal_id", "session_id"] as const;
  const missing: string[] = levels.filter((level) => !present(input[level]));
  const roles = input.roles;
  if (!Array.isArray(roles) || roles.length === 0) {
    missing.push("roles");
  }
  if (missing.length > 0) {
    throw refuse("scope_missing", `scope level missing: ${missing.join(", ")}`);
  }
  const malformed: string[] = levels.filter((level) => !isIdentifier(input[level]));
  if (!Array.isArray(roles) || roles.some((role) => !isIdentifier(role))) {
    malformed.push("roles");
  }
  if (malformed.length > 0) {
    throw refuse("scope_violation", `scope level malformed: ${malformed.join(", ")}`);
  }
  return {
    application_id: input.application_id as string,
    tenant_id: input.tenant_id as string,
    workspace_id: input.workspace_id as string,
    principal_id: input.principal_id as string,
    roles: [...new Set(roles as string[])],
    session_id: input.session_id as string,
  };
};

type PrincipalRow = {
  display_name: string;
  role: Role;
  patient_id: string | null;
  doctor_id: string | null;
};

// Reauthorise one request. The host does not trust the roles the identity
// asserts: it looks the principal up in its own directory, inside the tenant
// and the site the identity names, and the asserted roles must be roles that
// principal really holds. Every refusal here discloses nothing about other
// tenants or sites: an unknown tenant, an unknown site and a principal that
// belongs somewhere else are all the same `scope_violation`.
export const resolveCaller = (db: Database, raw: unknown, permission: Permission): Caller => {
  const identity = parseIdentity(raw);
  if (identity.application_id !== APPLICATION_ID) {
    throw refuse("scope_violation", "the identity belongs to another application");
  }
  const site = db
    .prepare("SELECT name, timezone FROM sites WHERE tenant_id = ? AND id = ?")
    .get(identity.tenant_id, identity.workspace_id) as { name: string; timezone: string } | undefined;
  const principal = db
    .prepare(
      "SELECT display_name, role, patient_id, doctor_id FROM principals WHERE tenant_id = ? AND site_id = ? AND id = ?",
    )
    .get(identity.tenant_id, identity.workspace_id, identity.principal_id) as PrincipalRow | undefined;
  if (site === undefined || principal === undefined) {
    throw refuse("scope_violation", "the principal is not known in this tenant and workspace");
  }
  // Asserting a role the principal does not hold is an escalation attempt, not
  // a scope problem.
  if (identity.roles.some((role) => role !== principal.role)) {
    throw refuse("permission_denied", "the identity asserts a role the principal does not hold");
  }
  if (!ROLE_PERMISSIONS[principal.role].includes(permission)) {
    throw refuse("permission_denied", "the role does not allow this operation");
  }
  return {
    tenantId: identity.tenant_id,
    siteId: identity.workspace_id,
    siteName: site.name,
    timezone: site.timezone,
    principalId: identity.principal_id,
    role: principal.role,
    patientId: principal.patient_id,
    doctorId: principal.doctor_id,
    sessionId: identity.session_id,
  };
};

// ---- record-level visibility ----------------------------------------------
//
// SQL predicates over the aliases `p` (patients) and `a` (appointments). A
// record the caller may not see is simply absent from every query: lookups of
// it fail exactly like lookups of a record that does not exist.

export type Predicate = { sql: string; params: string[] };

export const patientVisibility = (caller: Caller): Predicate => {
  switch (caller.role) {
    case "receptionist":
      return { sql: "1 = 1", params: [] };
    case "doctor":
      // A doctor sees the patients they have an appointment with, nobody else.
      return {
        sql: `EXISTS (SELECT 1 FROM appointments av WHERE av.tenant_id = p.tenant_id AND av.site_id = p.site_id
                AND av.patient_id = p.id AND av.doctor_id = ?)`,
        params: [caller.doctorId ?? ""],
      };
    case "patient":
      // A patient sees themselves.
      return { sql: "p.id = ?", params: [caller.patientId ?? ""] };
  }
};

export const appointmentVisibility = (caller: Caller): Predicate => {
  switch (caller.role) {
    case "receptionist":
      return { sql: "1 = 1", params: [] };
    case "doctor":
      return { sql: "a.doctor_id = ?", params: [caller.doctorId ?? ""] };
    case "patient":
      return { sql: "a.patient_id = ?", params: [caller.patientId ?? ""] };
  }
};
