import type { Caller } from "../access.ts";
import { toTimestamp } from "../clock.ts";
import { refuse } from "../errors.ts";
import { newId, newReference } from "../ids.ts";
import {
  datetime,
  identifier,
  record,
  recordVersion,
  text,
  type DataVersion,
  type Effect,
  type Fact,
} from "../results.ts";
import { localDisplay } from "../time.ts";
import type { ActionDefinition, Applied, Description, Env, Loaded, PreparedEffect } from "./actions.ts";
import {
  optionalEnum,
  optionalString,
  requiredReference,
  type RawFields,
} from "./fields.ts";
import {
  doctorOf,
  getAppointment,
  getPatient,
  getSlot,
  patientName,
  patientOf,
  type AppointmentRow,
  type DoctorRow,
  type PatientRow,
  type SlotRow,
} from "./records.ts";
import {
  BOOKING_TTL_SECONDS,
  CANCEL_REASONS,
  CANCEL_TTL_SECONDS,
  CONTACT_TTL_SECONDS,
  MAX_AHEAD_DAYS,
  MIN_LEAD_MINUTES,
  type CancelReason,
} from "./rules.ts";

// ---- shared pieces -----------------------------------------------------------

const insertNotification = (
  env: Env,
  caller: Caller,
  kind: "booking_confirmation" | "cancellation_notice",
  appointmentId: string,
  patientId: string,
): string => {
  const id = newId("ntf");
  env.db
    .prepare(
      `INSERT INTO notifications (tenant_id, site_id, id, kind, appointment_id, patient_id, state, created_at)
       VALUES (?, ?, ?, ?, ?, ?, 'queued', ?)`,
    )
    .run(caller.tenantId, caller.siteId, id, kind, appointmentId, patientId, toTimestamp(env.clock.now()));
  return id;
};

const effectOf = (effects: PreparedEffect[], operation: string, entityType: string): PreparedEffect => {
  const found = effects.find((e) => e.operation === operation && e.entity_type === entityType);
  if (found === undefined) {
    throw refuse("validation_failed", "the prepared action does not hold the effect this capability prepares", {
      field: "prepared",
    });
  }
  return found;
};

const asText = (value: string | number | boolean | undefined): string | undefined =>
  typeof value === "string" ? value : undefined;

const isUniqueViolation = (error: unknown): boolean =>
  error instanceof Error && /UNIQUE constraint failed/u.test(error.message);

const startsAtFact = (slotStartsAt: string, caller: Caller): Fact =>
  datetime(
    "appointment_starts_at",
    slotStartsAt,
    caller.timezone,
    localDisplay(new Date(slotStartsAt), caller.timezone),
  );

// ---- appointments.book --------------------------------------------------------

type BookingLoaded = Loaded & {
  patient: PatientRow;
  slot: SlotRow;
  doctor: DoctorRow;
  reason: string | undefined;
};

const overlaps = (aStart: number, aMinutes: number, bStart: number, bMinutes: number): boolean =>
  aStart < bStart + bMinutes * 60_000 && bStart < aStart + aMinutes * 60_000;

export const bookAppointment: ActionDefinition<BookingLoaded> = {
  capabilityId: "appointments.book",
  permission: "appointments.book",
  fieldNames: ["patient", "slot", "reason"],
  declaredRiskLevel: 2,
  validForSeconds: BOOKING_TTL_SECONDS,

  load: (env, caller, fields): BookingLoaded => {
    const patientRef = requiredReference(fields, "patient");
    const slotRef = requiredReference(fields, "slot");
    const reason = optionalString(fields, "reason", { max: 200 });
    const patient = getPatient(env.db, caller, patientRef, "patient");
    const slot = getSlot(env.db, caller, slotRef, "slot");
    const doctor = doctorOf(env.db, caller.tenantId, caller.siteId, slot.doctor_id);
    return {
      patient,
      slot,
      doctor,
      reason,
      // Booking relies on exactly two records: the slot (is it still free?) and
      // the patient (the confirmation goes to the contact details on file).
      versions: [recordVersion("slot", slot.id, slot.version), recordVersion("patient", patient.id, patient.version)],
    };
  },

  check: (env, caller, { patient, slot }) => {
    const startsAt = new Date(slot.starts_at).getTime();
    const lead = startsAt - env.clock.now().getTime();
    if (lead < MIN_LEAD_MINUTES * 60_000 || lead > MAX_AHEAD_DAYS * 86_400_000) {
      throw refuse(
        "validation_failed",
        `an appointment can be booked from ${MIN_LEAD_MINUTES} minutes to ${MAX_AHEAD_DAYS} days ahead`,
        { field: "slot" },
      );
    }
    if (slot.status !== "free") {
      throw refuse("conflict", "the appointment slot is no longer free", { field: "slot" });
    }
    const others = env.db
      .prepare(
        `SELECT a.starts_at, s.duration_minutes FROM appointments a
           JOIN slots s ON s.tenant_id = a.tenant_id AND s.site_id = a.site_id AND s.id = a.slot_id
          WHERE a.tenant_id = ? AND a.site_id = ? AND a.patient_id = ? AND a.status = 'booked'`,
      )
      .all(caller.tenantId, caller.siteId, patient.id) as { starts_at: string; duration_minutes: number }[];
    if (others.some((o) => overlaps(new Date(o.starts_at).getTime(), o.duration_minutes, startsAt, slot.duration_minutes))) {
      throw refuse("conflict", "the patient already has an appointment at that time", { field: "patient" });
    }
  },

  describe: (_env, caller, { patient, slot, doctor, reason }): Description => {
    const name = patientName(patient);
    const when = localDisplay(new Date(slot.starts_at), caller.timezone);
    const effects: Effect[] = [
      {
        operation: "create",
        entity_type: "appointment",
        description: `Create an appointment for ${name} with ${doctor.name} on ${when} at ${caller.siteName}.`,
        changes: [
          identifier("patient_id", patient.id),
          identifier("slot_id", slot.id),
          startsAtFact(slot.starts_at, caller),
          ...(reason === undefined ? [] : [text("reason", reason)]),
        ],
        external: false,
      },
      {
        operation: "send",
        entity_type: "notification",
        description: "Send a booking confirmation message to the patient's contact details on file.",
        changes: [text("message_kind", "booking_confirmation")],
        external: true,
      },
    ];
    return {
      values: [
        text("patient_name", name),
        text("doctor_name", doctor.name),
        startsAtFact(slot.starts_at, caller),
        text("site_name", caller.siteName),
      ],
      summary: `Book ${name} with ${doctor.name} on ${when} at ${caller.siteName}.`,
      effects,
      nextActionsWhenPrepared: [{ action: "confirm" }, { action: "cancel" }],
    };
  },

  fieldsFromEffects: (effects): RawFields => {
    const create = effectOf(effects, "create", "appointment");
    return {
      patient: asText(create.changes.patient_id),
      slot: asText(create.changes.slot_id),
      ...(create.changes.reason === undefined ? {} : { reason: asText(create.changes.reason) }),
    };
  },

  apply: (env, caller, { patient, slot, doctor, reason }): Applied => {
    const now = toTimestamp(env.clock.now());
    const appointmentId = newId("apt");
    let reference = newReference();
    while (env.db.prepare("SELECT 1 AS found FROM appointments WHERE tenant_id = ? AND reference = ?").get(caller.tenantId, reference)) {
      reference = newReference();
    }
    try {
      env.db
        .prepare(
          `INSERT INTO appointments (tenant_id, site_id, id, reference, slot_id, patient_id, doctor_id, starts_at, status,
                                     reason, version, created_at, updated_at)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'booked', ?, 1, ?, ?)`,
        )
        .run(
          caller.tenantId,
          caller.siteId,
          appointmentId,
          reference,
          slot.id,
          patient.id,
          doctor.id,
          slot.starts_at,
          reason ?? null,
          now,
          now,
        );
    } catch (error) {
      // The unique index on (slot, active appointment) is the last line of
      // defence against a double booking.
      if (isUniqueViolation(error)) {
        throw refuse("conflict", "the appointment slot is no longer free", { field: "slot" });
      }
      throw error;
    }
    const taken = env.db
      .prepare(
        `UPDATE slots SET status = 'booked', appointment_id = ?, version = version + 1, updated_at = ?
          WHERE tenant_id = ? AND site_id = ? AND id = ? AND status = 'free'`,
      )
      .run(appointmentId, now, caller.tenantId, caller.siteId, slot.id);
    if (Number(taken.changes) !== 1) {
      throw refuse("conflict", "the appointment slot is no longer free", { field: "slot" });
    }
    const notificationId = insertNotification(env, caller, "booking_confirmation", appointmentId, patient.id);
    return {
      identifiers: [record("appointment", appointmentId), record("notification", notificationId)],
      values: [
        text("appointment_reference", reference),
        text("appointment_status", "booked"),
        text("patient_name", patientName(patient)),
        text("doctor_name", doctor.name),
        startsAtFact(slot.starts_at, caller),
        text("site_name", caller.siteName),
      ],
      versionsAfter: [
        recordVersion("slot", slot.id, slot.version + 1),
        recordVersion("patient", patient.id, patient.version),
      ],
      followUp: { reference: notificationId, state: "queued", as_of: now },
      nextActions: [{ action: "invoke_capability", capability_id: "appointments.cancel", label: "Cancel this appointment" }],
    };
  },
};

// ---- appointments.cancel ------------------------------------------------------

type CancelLoaded = Loaded & {
  appointment: AppointmentRow;
  patient: PatientRow;
  doctor: DoctorRow;
  reason: CancelReason | undefined;
};

export const cancelAppointment: ActionDefinition<CancelLoaded> = {
  capabilityId: "appointments.cancel",
  permission: "appointments.cancel",
  fieldNames: ["appointment", "reason"],
  declaredRiskLevel: 2,
  validForSeconds: CANCEL_TTL_SECONDS,

  load: (env, caller, fields): CancelLoaded => {
    const appointmentRef = requiredReference(fields, "appointment");
    const reason = optionalEnum(fields, "reason", CANCEL_REASONS);
    const appointment = getAppointment(env.db, caller, appointmentRef, "appointment");
    return {
      appointment,
      patient: patientOf(env.db, caller.tenantId, caller.siteId, appointment.patient_id),
      doctor: doctorOf(env.db, caller.tenantId, caller.siteId, appointment.doctor_id),
      reason,
      versions: [recordVersion("appointment", appointment.id, appointment.version)],
    };
  },

  check: (env, _caller, { appointment }) => {
    if (appointment.status !== "booked") {
      throw refuse("conflict", "the appointment is already cancelled", { field: "appointment" });
    }
    if (new Date(appointment.starts_at).getTime() <= env.clock.now().getTime()) {
      throw refuse("validation_failed", "an appointment that has started or passed cannot be cancelled", {
        field: "appointment",
      });
    }
  },

  describe: (_env, caller, { appointment, patient, doctor, reason }): Description => {
    const name = patientName(patient);
    const when = localDisplay(new Date(appointment.starts_at), caller.timezone);
    return {
      values: [
        text("appointment_reference", appointment.reference),
        startsAtFact(appointment.starts_at, caller),
        text("patient_name", name),
        text("doctor_name", doctor.name),
        text("site_name", caller.siteName),
      ],
      summary: `Cancel appointment ${appointment.reference} for ${name} with ${doctor.name} on ${when} and release the time slot.`,
      effects: [
        {
          operation: "update",
          entity_type: "appointment",
          entity_id: appointment.id,
          description: `Cancel appointment ${appointment.reference} for ${name} on ${when} and release its time slot.`,
          changes: [text("appointment_status", "cancelled"), ...(reason === undefined ? [] : [text("cancel_reason", reason)])],
          external: false,
        },
        {
          operation: "send",
          entity_type: "notification",
          description: "Send a cancellation message to the patient's contact details on file.",
          changes: [text("message_kind", "cancellation_notice")],
          external: true,
        },
      ],
      nextActionsWhenPrepared: [{ action: "confirm" }, { action: "cancel" }],
    };
  },

  fieldsFromEffects: (effects): RawFields => {
    const update = effectOf(effects, "update", "appointment");
    return {
      appointment: update.entity_id,
      ...(update.changes.cancel_reason === undefined ? {} : { reason: asText(update.changes.cancel_reason) }),
    };
  },

  apply: (env, caller, { appointment, patient, doctor, reason }): Applied => {
    const now = toTimestamp(env.clock.now());
    const cancelled = env.db
      .prepare(
        `UPDATE appointments SET status = 'cancelled', cancel_reason = ?, version = version + 1, updated_at = ?
          WHERE tenant_id = ? AND site_id = ? AND id = ? AND status = 'booked'`,
      )
      .run(reason ?? null, now, caller.tenantId, caller.siteId, appointment.id);
    if (Number(cancelled.changes) !== 1) {
      throw refuse("conflict", "the appointment is already cancelled", { field: "appointment" });
    }
    env.db
      .prepare(
        `UPDATE slots SET status = 'free', appointment_id = NULL, version = version + 1, updated_at = ?
          WHERE tenant_id = ? AND site_id = ? AND id = ? AND appointment_id = ?`,
      )
      .run(now, caller.tenantId, caller.siteId, appointment.slot_id, appointment.id);
    const notificationId = insertNotification(env, caller, "cancellation_notice", appointment.id, patient.id);
    return {
      identifiers: [record("appointment", appointment.id), record("notification", notificationId)],
      values: [
        text("appointment_reference", appointment.reference),
        text("appointment_status", "cancelled"),
        startsAtFact(appointment.starts_at, caller),
        text("patient_name", patientName(patient)),
        text("doctor_name", doctor.name),
      ],
      versionsAfter: [recordVersion("appointment", appointment.id, appointment.version + 1)],
      followUp: { reference: notificationId, state: "queued", as_of: now },
      nextActions: [{ action: "invoke_capability", capability_id: "appointments.book", label: "Book a new appointment" }],
    };
  },
};

// ---- patients.update_contact --------------------------------------------------

type ContactLoaded = Loaded & {
  patient: PatientRow;
  phone: string | undefined;
  email: string | undefined;
};

const PHONE_PATTERN = /^[0-9+()\-. ]{5,32}$/u;
const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/u;

const normalisedPhone = (fields: RawFields): string | undefined => {
  const phone = optionalString(fields, "phone", { max: 32 });
  if (phone === undefined) {
    return undefined;
  }
  if (!PHONE_PATTERN.test(phone) || (phone.match(/\d/gu) ?? []).length < 5) {
    throw refuse("validation_failed", "phone must hold at least 5 digits and only digits, spaces and + - ( ) .", {
      field: "phone",
    });
  }
  return phone.replace(/\s+/gu, " ");
};

const normalisedEmail = (fields: RawFields): string | undefined => {
  const email = optionalString(fields, "email", { max: 254 });
  if (email === undefined) {
    return undefined;
  }
  if (!EMAIL_PATTERN.test(email)) {
    throw refuse("validation_failed", "email must be an address such as name@example.test", { field: "email" });
  }
  return email.toLowerCase();
};

const contactFieldsOf = (phone: string | undefined, email: string | undefined): string =>
  [phone === undefined ? "" : "phone", email === undefined ? "" : "email"].filter((s) => s !== "").join(", ");

export const updateContact: ActionDefinition<ContactLoaded> = {
  capabilityId: "patients.update_contact",
  permission: "patients.update_contact",
  fieldNames: ["patient", "phone", "email"],
  declaredRiskLevel: 1,
  validForSeconds: CONTACT_TTL_SECONDS,

  load: (env, caller, fields): ContactLoaded => {
    const patientRef = requiredReference(fields, "patient");
    const phone = normalisedPhone(fields);
    const email = normalisedEmail(fields);
    const patient = getPatient(env.db, caller, patientRef, "patient");
    return {
      patient,
      phone,
      email,
      versions: [recordVersion("patient", patient.id, patient.version)],
    };
  },

  check: (_env, _caller, { phone, email }) => {
    if (phone === undefined && email === undefined) {
      throw refuse("validation_failed", "give a new phone number, a new email address or both");
    }
  },

  describe: (_env, _caller, { patient, phone, email }): Description => {
    const name = patientName(patient);
    const changes: Fact[] = [
      ...(phone === undefined ? [] : [text("phone", phone)]),
      ...(email === undefined ? [] : [text("email", email)]),
    ];
    return {
      values: [text("patient_name", name), text("contact_fields_updated", contactFieldsOf(phone, email))],
      summary: `Update the ${contactFieldsOf(phone, email).replace(", ", " and ")} on file for ${name}.`,
      effects: [
        {
          operation: "update",
          entity_type: "patient",
          entity_id: patient.id,
          description: `Update the contact details on file for ${name}.`,
          changes,
          external: false,
        },
      ],
      nextActionsWhenPrepared: [{ action: "cancel" }],
    };
  },

  fieldsFromEffects: (effects): RawFields => {
    const update = effectOf(effects, "update", "patient");
    return {
      patient: update.entity_id,
      ...(update.changes.phone === undefined ? {} : { phone: asText(update.changes.phone) }),
      ...(update.changes.email === undefined ? {} : { email: asText(update.changes.email) }),
    };
  },

  apply: (env, caller, { patient, phone, email }): Applied => {
    const now = toTimestamp(env.clock.now());
    const updated = env.db
      .prepare(
        `UPDATE patients SET phone = COALESCE(?, phone), email = COALESCE(?, email), version = version + 1, updated_at = ?
          WHERE tenant_id = ? AND site_id = ? AND id = ?`,
      )
      .run(phone ?? null, email ?? null, now, caller.tenantId, caller.siteId, patient.id);
    if (Number(updated.changes) !== 1) {
      throw refuse("internal", "the patient record could not be updated");
    }
    const versionsAfter: DataVersion[] = [recordVersion("patient", patient.id, patient.version + 1)];
    return {
      identifiers: [record("patient", patient.id)],
      values: [text("patient_name", patientName(patient)), text("contact_fields_updated", contactFieldsOf(phone, email))],
      versionsAfter,
    };
  },
};
