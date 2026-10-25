import { expect } from "vitest";

import type { HostResult } from "../../src/results.ts";
import { describeErrors, preparedActionSchema } from "./schemas.ts";
import { idemKey, preparedActionFrom, type World } from "./world.ts";

export type Observed = { operation: string; capability: string; label: string; result: HostResult };

// Runs every operation of every capability through success and failure paths
// and records what the host answered.
export const observeEverything = (world: World): Observed[] => {
  const id = (ref: string): string => world.records[ref]?.entity_id ?? "";
  const s = world.app.service;
  const out: Observed[] = [];
  const see = (operation: string, capability: string, label: string, result: HostResult): HostResult => {
    out.push({ operation, capability, label, result });
    return result;
  };
  const reception = world.identity("receptionA1");
  const patient = world.identity("patientA1");

  // reads
  see("read", "availability.list", "list", s.availabilityList(reception, { days: 14 }));
  see("read", "availability.list", "list empty", s.availabilityList(reception, { date: "2026-12-01", max_results: 1 }));
  see("read", "availability.list", "list bad date", s.availabilityList(reception, { date: "2025-01-01" }));
  see("read", "availability.list", "list foreign doctor", s.availabilityList(reception, { doctor: "doc_nothere0001" }));
  see("read", "patients.find", "find", s.patientsFind(reception, { query: "Testpatient" }));
  see("read", "patients.find", "find none", s.patientsFind(reception, { query: "Nobodyhere" }));
  see("read", "patients.find", "find short", s.patientsFind(reception, { query: "a" }));
  see("read", "availability.list", "doctor denied", s.availabilityList({ ...reception, roles: ["doctor"] }, {}));
  for (const [name, call] of [
    ["resolve_patient", s.resolvePatient],
    ["resolve_doctor", s.resolveDoctor],
    ["resolve_slot", s.resolveSlot],
    ["resolve_appointment", s.resolveAppointment],
  ] as const) {
    see("read", name, `${name} nothing`, call(reception, { mention: "2099-01-01" }));
  }
  see("read", "resolve_patient", "resolve patient", s.resolvePatient(reception, { mention: "Testpatient" }));
  see("read", "resolve_doctor", "resolve doctor", s.resolveDoctor(reception, { mention: "Dr. Synthetic" }));
  see("read", "resolve_slot", "resolve slot", s.resolveSlot(reception, { mention: "2026-10-06" }));
  see("read", "resolve_appointment", "resolve appointment", s.resolveAppointment(reception, { mention: "2026-10-06" }));
  // the reserved arguments of the core's resolution read
  see("read", "resolve_patient", "resolve reserved", s.resolvePatient(reception, { entity_type: "patient", mention: "Testpatient", candidate_id: id("patientA1") }));
  see("read", "resolve_patient", "resolve hint only", s.resolvePatient(reception, { entity_type: "patient", candidate_id: id("patientA1") }));
  see("read", "resolve_patient", "resolve foreign hint", s.resolvePatient(reception, { entity_type: "patient", candidate_id: id("patientA2") }));
  see("read", "resolve_patient", "resolve wrong type", s.resolvePatient(reception, { entity_type: "slot", mention: "x" }));
  see("read", "resolve_slot", "resolve bad mention", s.resolveSlot(reception, { mention: "tomorrow" }));
  see("read", "resolve_doctor", "resolve doctor by id", s.resolveDoctor(reception, { entity_type: "doctor", candidate_id: id("doctorA1") }));

  // booking: every operation, success and refusals
  const fields = { patient: id("patientA1"), slot: id("slotA1"), reason: "annual check" };
  see("validate", "appointments.book", "validate", s.book.validate(reception, fields));
  const prepared = see("prepare", "appointments.book", "prepare", s.book.prepare(reception, fields));
  see("prepare", "appointments.book", "prepare missing", s.book.prepare(reception, { patient: id("patientA1") }));
  see("prepare", "appointments.book", "prepare denied", s.book.prepare(world.identity("doctorA1"), fields));
  const action = preparedActionFrom("appointments.book", prepared, { now: world.clock.now() });
  expect(preparedActionSchema(action), describeErrors(preparedActionSchema)).toBe(true);
  see("commit", "appointments.book", "commit", s.book.commit(reception, action.idempotency_key, action));
  see("commit", "appointments.book", "replay", s.book.commit(reception, action.idempotency_key, action));
  see("status", "appointments.book", "status", s.book.status(reception, action.idempotency_key));
  see("status", "appointments.book", "status none", s.book.status(reception, idemKey()));
  see("validate", "appointments.book", "validate taken", s.book.validate(reception, fields));
  const stale = preparedActionFrom("appointments.book", s.book.prepare(reception, { patient: id("patientA1b"), slot: id("slotA1b") }), {
    now: world.clock.now(),
  });
  s.book.commit(reception, idemKey(), preparedActionFrom("appointments.book", s.book.prepare(reception, { patient: id("patientA1"), slot: id("slotA1b") }), { now: world.clock.now() }));
  see("commit", "appointments.book", "commit stale", s.book.commit(reception, stale.idempotency_key, stale));

  // cancellation
  const cancelPrepared = see("prepare", "appointments.cancel", "prepare", s.cancel.prepare(reception, { appointment: id("apptA1"), reason: "other" }));
  see("validate", "appointments.cancel", "validate", s.cancel.validate(reception, { appointment: id("apptA1"), reason: "other" }));
  const cancelAction = preparedActionFrom("appointments.cancel", cancelPrepared, { now: world.clock.now() });
  see("commit", "appointments.cancel", "commit", s.cancel.commit(reception, cancelAction.idempotency_key, cancelAction));
  see("status", "appointments.cancel", "status", s.cancel.status(reception, cancelAction.idempotency_key));
  see("prepare", "appointments.cancel", "prepare twice", s.cancel.prepare(reception, { appointment: id("apptA1") }));
  see("prepare", "appointments.cancel", "prepare foreign", s.cancel.prepare(reception, { appointment: id("apptA2") }));

  // contact details
  const contactPrepared = see("prepare", "patients.update_contact", "prepare", s.contact.prepare(patient, { patient: id("patientA1"), phone: "+1 555 0150", email: "x@example.test" }));
  see("validate", "patients.update_contact", "validate", s.contact.validate(patient, { patient: id("patientA1"), phone: "+1 555 0150", email: "x@example.test" }));
  const contactAction = preparedActionFrom("patients.update_contact", contactPrepared, { now: world.clock.now(), mode: "none" });
  see("commit", "patients.update_contact", "commit", s.contact.commit(patient, contactAction.idempotency_key, contactAction));
  see("status", "patients.update_contact", "status", s.contact.status(patient, contactAction.idempotency_key));
  see("prepare", "patients.update_contact", "prepare bad", s.contact.prepare(patient, { patient: id("patientA1"), phone: "x" }));
  return out;
};

