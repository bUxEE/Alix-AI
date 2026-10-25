import { afterEach, beforeEach, describe, expect, it } from "vitest";

import type { HostResult } from "../src/results.ts";
import { createWorld, idemKey, preparedActionFrom, type World } from "./helpers/world.ts";

let world: World;

beforeEach(() => {
  world = createWorld();
});

afterEach(async () => {
  await world.close();
});

const id = (ref: string): string => world.records[ref]?.entity_id ?? "";
const code = (result: HostResult): string | undefined => result.errors[0]?.code;

describe("appointments.cancel", () => {
  const prepare = (who = "receptionA1", appointment = "apptA1", extra: Record<string, unknown> = {}): HostResult =>
    world.app.service.cancel.prepare(world.identity(who), { appointment: id(appointment), ...extra });

  it("prepares exact effects and reports the appointment's own version", () => {
    const result = prepare("receptionA1", "apptA1", { reason: "patient_request" });
    expect(result.status).toBe("ok");
    expect(result.preparation?.declared_risk_level).toBe(2);
    expect(result.preparation?.effects.map((e) => `${e.operation}:${e.entity_type}:${e.entity_id ?? "-"}`)).toEqual([
      `update:appointment:${id("apptA1")}`,
      "send:notification:-",
    ]);
    expect(result.preparation?.data_versions).toEqual([{ entity_type: "appointment", entity_id: id("apptA1"), version: "1" }]);
    const validate = world.app.service.cancel.validate(world.identity("receptionA1"), { appointment: id("apptA1"), reason: "patient_request" });
    expect(validate.freshness?.version).toBe(result.freshness?.version);
  });

  it("cancels, frees the slot and is idempotent", () => {
    const slot = (world.app.db.prepare("SELECT slot_id FROM appointments WHERE id = ?").get(id("apptA1")) as { slot_id: string }).slot_id;
    const prepared = preparedActionFrom("appointments.cancel", prepare(), { now: world.clock.now() });
    const receipt = world.app.service.cancel.commit(world.identity("receptionA1"), prepared.idempotency_key, prepared);
    expect(receipt.status).toBe("ok");
    expect(receipt.values.find((f) => f.field === "appointment_status")?.value).toMatchObject({ value: "cancelled" });
    expect(world.app.db.prepare("SELECT status, appointment_id FROM slots WHERE id = ?").get(slot)).toEqual({
      status: "free",
      appointment_id: null,
    });
    const again = world.app.service.cancel.commit(world.identity("receptionA1"), prepared.idempotency_key, prepared);
    expect(again).toEqual(receipt);
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM notifications WHERE kind = 'cancellation_notice'").get()).toEqual({ n: 1 });
  });

  it("refuses to cancel an appointment twice", () => {
    const prepared = preparedActionFrom("appointments.cancel", prepare(), { now: world.clock.now() });
    world.app.service.cancel.commit(world.identity("receptionA1"), prepared.idempotency_key, prepared);
    const second = prepare();
    expect(second.status).toBe("rejected");
    expect(code(second)).toBe("conflict");
  });

  it("is stale when the appointment was cancelled by someone else in between", () => {
    const mine = preparedActionFrom("appointments.cancel", prepare("receptionA1"), { now: world.clock.now() });
    const theirs = preparedActionFrom("appointments.cancel", prepare("patientA1"), { now: world.clock.now() });
    expect(world.app.service.cancel.commit(world.identity("patientA1"), theirs.idempotency_key, theirs).status).toBe("ok");
    const result = world.app.service.cancel.commit(world.identity("receptionA1"), mine.idempotency_key, mine);
    expect(code(result)).toBe("stale");
  });

  it("refuses an appointment that has started", () => {
    const row = world.app.db.prepare("SELECT starts_at FROM appointments WHERE id = ?").get(id("apptA1")) as { starts_at: string };
    world.clock.set(new Date(new Date(row.starts_at).getTime() + 60_000).toISOString());
    const result = prepare();
    expect(result.errors[0]).toMatchObject({ code: "validation_failed", field: "appointment" });
  });

  it("refuses an unknown reason", () => {
    expect(prepare("receptionA1", "apptA1", { reason: "whim" }).errors[0]).toMatchObject({ code: "validation_failed", field: "reason" });
  });

  it("lets a doctor cancel their own appointments only", () => {
    // apptA1 belongs to the site's only doctor (usr_doctor_a1), apptA2 to another site.
    expect(prepare("doctorA1", "apptA1").status).toBe("ok");
    expect(code(prepare("doctorA1", "apptA2"))).toBe("scope_violation");
  });

  it("lets a patient cancel their own appointment but not another patient's", () => {
    expect(prepare("patientA1", "apptA1").status).toBe("ok");
    const other = prepare("patientA1b", "apptA1");
    expect(other.status).toBe("rejected");
    expect(code(other)).toBe("scope_violation");
  });
});

describe("patients.update_contact (level 1, immediate)", () => {
  const prepare = (who: string, fields: Record<string, unknown>): HostResult =>
    world.app.service.contact.prepare(world.identity(who), fields);

  it("prepares a reversible, non-external effect with the patient's version", () => {
    const result = prepare("patientA1", { patient: id("patientA1"), phone: "+1 555 0123" });
    expect(result.status).toBe("ok");
    expect(result.preparation?.declared_risk_level).toBe(1);
    expect(result.preparation?.valid_for_seconds).toBe(120);
    expect(result.preparation?.effects).toHaveLength(1);
    expect(result.preparation?.effects[0]).toMatchObject({ operation: "update", entity_type: "patient", external: false });
    const validate = world.app.service.contact.validate(world.identity("patientA1"), { patient: id("patientA1"), phone: "+1 555 0123" });
    expect(validate.freshness?.version).toBe(result.freshness?.version);
  });

  it("commits immediately and moves the record's version", () => {
    const result = prepare("patientA1", { patient: id("patientA1"), phone: "+1 555 0123", email: "Alma.New@Example.test" });
    const prepared = preparedActionFrom("patients.update_contact", result, { now: world.clock.now(), mode: "none" });
    const receipt = world.app.service.contact.commit(world.identity("patientA1"), prepared.idempotency_key, prepared);
    expect(receipt.status).toBe("ok");
    expect(receipt.identifiers).toEqual([{ entity_type: "patient", entity_id: id("patientA1") }]);
    expect(world.app.db.prepare("SELECT phone, email, version FROM patients WHERE id = ?").get(id("patientA1"))).toEqual({
      phone: "+1 555 0123",
      email: "alma.new@example.test",
      version: 2,
    });
    expect(world.app.service.contact.commit(world.identity("patientA1"), prepared.idempotency_key, prepared)).toEqual(receipt);
    expect(world.app.db.prepare("SELECT version FROM patients WHERE id = ?").get(id("patientA1"))).toEqual({ version: 2 });
  });

  it("updates only the fields given", () => {
    const before = world.app.db.prepare("SELECT email FROM patients WHERE id = ?").get(id("patientA1")) as { email: string };
    const prepared = preparedActionFrom("patients.update_contact", prepare("patientA1", { patient: id("patientA1"), phone: "+1 555 0199" }), {
      now: world.clock.now(),
      mode: "none",
    });
    world.app.service.contact.commit(world.identity("patientA1"), idemKey(), prepared);
    expect(world.app.db.prepare("SELECT email FROM patients WHERE id = ?").get(id("patientA1"))).toEqual(before);
  });

  it("validates the formats and needs at least one change", () => {
    expect(prepare("patientA1", { patient: id("patientA1") }).errors[0]?.code).toBe("validation_failed");
    expect(prepare("patientA1", { patient: id("patientA1"), phone: "call me" }).errors[0]).toMatchObject({ code: "validation_failed", field: "phone" });
    expect(prepare("patientA1", { patient: id("patientA1"), phone: "123" }).errors[0]).toMatchObject({ code: "validation_failed", field: "phone" });
    expect(prepare("patientA1", { patient: id("patientA1"), email: "nobody" }).errors[0]).toMatchObject({ code: "validation_failed", field: "email" });
  });

  it("is stale when the record changed after prepare", () => {
    const mine = preparedActionFrom("patients.update_contact", prepare("receptionA1", { patient: id("patientA1"), phone: "+1 555 0140" }), {
      now: world.clock.now(),
      mode: "none",
    });
    const theirs = preparedActionFrom("patients.update_contact", prepare("patientA1", { patient: id("patientA1"), phone: "+1 555 0141" }), {
      now: world.clock.now(),
      mode: "none",
    });
    expect(world.app.service.contact.commit(world.identity("patientA1"), theirs.idempotency_key, theirs).status).toBe("ok");
    const result = world.app.service.contact.commit(world.identity("receptionA1"), mine.idempotency_key, mine);
    expect(code(result)).toBe("stale");
    expect(world.app.db.prepare("SELECT phone FROM patients WHERE id = ?").get(id("patientA1"))).toEqual({ phone: "+1 555 0141" });
  });

  it("is not available to a doctor", () => {
    const result = prepare("doctorA1", { patient: id("patientA1"), phone: "+1 555 0123" });
    expect(code(result)).toBe("permission_denied");
  });

  it("lets a receptionist change any patient of the site but not of another site", () => {
    expect(prepare("receptionA1", { patient: id("patientA1b"), phone: "+1 555 0123" }).status).toBe("ok");
    expect(code(prepare("receptionA1", { patient: id("patientA2"), phone: "+1 555 0123" }))).toBe("scope_violation");
  });

  it("lets a patient change only their own record", () => {
    expect(code(prepare("patientA1", { patient: id("patientA1b"), phone: "+1 555 0123" }))).toBe("scope_violation");
  });
});
