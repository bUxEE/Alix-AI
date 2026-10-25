import { appointmentVisibility, patientVisibility, type Caller } from "../access.ts";
import type { Database } from "../db.ts";
import { refuse } from "../errors.ts";

export type PatientRow = {
  tenant_id: string;
  site_id: string;
  id: string;
  given_name: string;
  family_name: string;
  date_of_birth: string;
  phone: string | null;
  email: string | null;
  version: number;
};

export type DoctorRow = { tenant_id: string; site_id: string; id: string; name: string; specialty: string };

export type SlotRow = {
  tenant_id: string;
  site_id: string;
  id: string;
  doctor_id: string;
  starts_at: string;
  duration_minutes: number;
  status: "free" | "booked" | "blocked";
  appointment_id: string | null;
  version: number;
};

export type AppointmentRow = {
  tenant_id: string;
  site_id: string;
  id: string;
  reference: string;
  slot_id: string;
  patient_id: string;
  doctor_id: string;
  starts_at: string;
  status: "booked" | "cancelled";
  reason: string | null;
  cancel_reason: string | null;
  version: number;
};

export const patientName = (patient: Pick<PatientRow, "given_name" | "family_name">): string =>
  `${patient.given_name} ${patient.family_name}`;

// A record outside the caller's tenant, site or role-level visibility, and a
// record that does not exist, are the same refusal: the response never says
// which, so an identifier cannot be used to probe other scopes.
const outOfScope = (field: string) =>
  refuse("scope_violation", "the record is outside the caller's access scope or does not exist", { field });

export const getPatient = (db: Database, caller: Caller, id: string, field = "patient"): PatientRow => {
  const visible = patientVisibility(caller);
  const row = db
    .prepare(`SELECT p.* FROM patients p WHERE p.tenant_id = ? AND p.site_id = ? AND p.id = ? AND (${visible.sql})`)
    .get(caller.tenantId, caller.siteId, id, ...visible.params) as PatientRow | undefined;
  if (row === undefined) {
    throw outOfScope(field);
  }
  return row;
};

export const getDoctor = (db: Database, caller: Caller, id: string, field = "doctor"): DoctorRow => {
  const row = db
    .prepare("SELECT * FROM doctors WHERE tenant_id = ? AND site_id = ? AND id = ?")
    .get(caller.tenantId, caller.siteId, id) as DoctorRow | undefined;
  if (row === undefined) {
    throw outOfScope(field);
  }
  return row;
};

export const getSlot = (db: Database, caller: Caller, id: string, field = "slot"): SlotRow => {
  const row = db
    .prepare("SELECT * FROM slots WHERE tenant_id = ? AND site_id = ? AND id = ?")
    .get(caller.tenantId, caller.siteId, id) as SlotRow | undefined;
  if (row === undefined) {
    throw outOfScope(field);
  }
  return row;
};

export const getAppointment = (db: Database, caller: Caller, id: string, field = "appointment"): AppointmentRow => {
  const visible = appointmentVisibility(caller);
  const row = db
    .prepare(`SELECT a.* FROM appointments a WHERE a.tenant_id = ? AND a.site_id = ? AND a.id = ? AND (${visible.sql})`)
    .get(caller.tenantId, caller.siteId, id, ...visible.params) as AppointmentRow | undefined;
  if (row === undefined) {
    throw outOfScope(field);
  }
  return row;
};

// Doctors of the site by id, for display. A slot or appointment always has one.
export const doctorOf = (db: Database, tenantId: string, siteId: string, doctorId: string): DoctorRow => {
  const row = db
    .prepare("SELECT * FROM doctors WHERE tenant_id = ? AND site_id = ? AND id = ?")
    .get(tenantId, siteId, doctorId) as DoctorRow | undefined;
  if (row === undefined) {
    throw refuse("internal", "the record refers to a doctor that does not exist");
  }
  return row;
};

export const patientOf = (db: Database, tenantId: string, siteId: string, patientId: string): PatientRow => {
  const row = db
    .prepare("SELECT * FROM patients WHERE tenant_id = ? AND site_id = ? AND id = ?")
    .get(tenantId, siteId, patientId) as PatientRow | undefined;
  if (row === undefined) {
    throw refuse("internal", "the record refers to a patient that does not exist");
  }
  return row;
};

export const doctorLabel = (doctor: DoctorRow): string => doctor.name;

// The same name matching for every lookup: case and accents are ignored and
// every word of the query must appear in the name.
export const normalise = (value: string): string =>
  value
    .normalize("NFD")
    .replace(/\p{Diacritic}/gu, "")
    .toLowerCase()
    .trim();

export const matchesName = (query: string, name: string): boolean => {
  const haystack = normalise(name);
  return normalise(query)
    .split(/\s+/u)
    .filter((word) => word !== "")
    .every((word) => haystack.includes(word));
};
