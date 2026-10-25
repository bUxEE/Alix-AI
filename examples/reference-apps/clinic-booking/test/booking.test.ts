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

const slotId = (ref: string): string => world.records[ref]?.entity_id ?? "";
const patientId = (ref: string): string => world.records[ref]?.entity_id ?? "";

const bookFields = (slot = "slotA1", patient = "patientA1", extra: Record<string, unknown> = {}) => ({
  patient: patientId(patient),
  slot: slotId(slot),
  ...extra,
});

const prepareBooking = (who = "receptionA1", fields: Record<string, unknown> = bookFields()): HostResult =>
  world.app.service.book.prepare(world.identity(who), fields);

const commit = (who: string, prepared: Record<string, unknown>): HostResult =>
  world.app.service.book.commit(world.identity(who), prepared.idempotency_key, prepared);

const failureCode = (result: HostResult): string | undefined => result.errors[0]?.code;

describe("appointments.book: prepare", () => {
  it("returns exact effects, the data versions used and a snapshot version", () => {
    const result = prepareBooking("receptionA1", bookFields("slotA1", "patientA1", { reason: "annual check" }));
    expect(result.status).toBe("ok");
    expect(result.errors).toEqual([]);
    const preparation = result.preparation;
    expect(preparation?.declared_risk_level).toBe(2);
    expect(preparation?.valid_for_seconds).toBe(600);
    expect(preparation?.effects.map((e) => `${e.operation}:${e.entity_type}:${e.external}`)).toEqual([
      "create:appointment:false",
      "send:notification:true",
    ]);
    expect(preparation?.data_versions.map((v) => v.entity_type).sort()).toEqual(["patient", "slot"]);
    expect(result.freshness?.version).toMatch(/^rv1-[0-9a-f]{32}$/u);
    // Nothing was applied by preparing.
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM idempotency").get()).toEqual({ n: 0 });
    expect(world.app.db.prepare("SELECT status FROM slots WHERE id = ?").get(slotId("slotA1"))).toEqual({ status: "free" });
  });

  it("states every fact a confirmation must show", () => {
    const result = prepareBooking();
    const fields = result.values.map((f) => f.field);
    expect(fields).toEqual(expect.arrayContaining(["patient_name", "doctor_name", "appointment_starts_at", "site_name"]));
    const starts = result.values.find((f) => f.field === "appointment_starts_at");
    expect(starts?.value).toMatchObject({ kind: "datetime", zone: "Europe/Rome" });
    // 2026-10-06 is tomorrow; the first synthetic slot is 09:00 Rome time.
    expect(starts?.display).toBe("2026-10-06 09:00");
  });

  it("asks for a missing required field by name", () => {
    const result = world.app.service.book.prepare(world.identity("receptionA1"), { patient: patientId("patientA1") });
    expect(result.status).toBe("rejected");
    expect(result.errors).toMatchObject([{ code: "missing_fields", field: "slot" }]);
  });

  it("refuses an unknown field and a wrongly typed one", () => {
    expect(world.app.service.book.prepare(world.identity("receptionA1"), { ...bookFields(), colour: "red" }).errors[0]).toMatchObject({
      code: "validation_failed",
      field: "colour",
    });
    expect(world.app.service.book.prepare(world.identity("receptionA1"), { ...bookFields(), reason: 12 }).errors[0]).toMatchObject({
      code: "validation_failed",
      field: "reason",
    });
  });
});

describe("appointments.book: validate reports the version prepare stored", () => {
  it("is identical on unchanged data", () => {
    const prepare = prepareBooking();
    const validate = world.app.service.book.validate(world.identity("receptionA1"), bookFields());
    expect(validate.status).toBe("ok");
    expect(validate.freshness?.version).toBe(prepare.freshness?.version);
    // And it is the same again a minute later: the version is not a timestamp or an ETag.
    world.clock.advanceSeconds(60);
    const later = world.app.service.book.validate(world.identity("receptionA1"), bookFields());
    expect(later.freshness?.version).toBe(prepare.freshness?.version);
    expect(later.freshness?.as_of).not.toBe(prepare.freshness?.as_of);
  });

  it("is not a site-wide counter: booking a different slot does not move it", () => {
    const prepare = prepareBooking("receptionA1", bookFields("slotA1", "patientA1"));
    const other = prepareBooking("receptionA1", bookFields("slotA1b", "patientA1b"));
    expect(commit("receptionA1", preparedActionFrom("appointments.book", other, { now: world.clock.now() })).status).toBe("ok");
    const validate = world.app.service.book.validate(world.identity("receptionA1"), bookFields("slotA1", "patientA1"));
    expect(validate.status).toBe("ok");
    expect(validate.freshness?.version).toBe(prepare.freshness?.version);
  });

  it("changes when the same slot is taken, and reports it", () => {
    const prepare = prepareBooking("receptionA1", bookFields("slotA1", "patientA1"));
    const taker = prepareBooking("receptionA1", bookFields("slotA1", "patientA1b"));
    expect(commit("receptionA1", preparedActionFrom("appointments.book", taker, { now: world.clock.now() })).status).toBe("ok");

    const validate = world.app.service.book.validate(world.identity("receptionA1"), bookFields("slotA1", "patientA1"));
    expect(validate.status).toBe("rejected");
    expect(failureCode(validate)).toBe("conflict");
    expect(validate.freshness?.version).toMatch(/^rv1-/u);
    expect(validate.freshness?.version).not.toBe(prepare.freshness?.version);
  });

  it("changes when the patient record changes", () => {
    const prepare = prepareBooking();
    const update = world.app.service.contact.prepare(world.identity("receptionA1"), {
      patient: patientId("patientA1"),
      phone: "+1 555 0177",
    });
    expect(
      world.app.service.contact.commit(
        world.identity("receptionA1"),
        "idem_contact_000000000000",
        preparedActionFrom("patients.update_contact", update, { now: world.clock.now(), mode: "none" }),
      ).status,
    ).toBe("ok");
    const validate = world.app.service.book.validate(world.identity("receptionA1"), bookFields());
    expect(validate.status).toBe("ok");
    expect(validate.freshness?.version).not.toBe(prepare.freshness?.version);
  });
});

describe("appointments.book: commit", () => {
  it("applies once, returns a receipt with stable identifiers and queues a notification", () => {
    const prepared = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now() });
    const receipt = commit("receptionA1", prepared);
    expect(receipt.status).toBe("ok");
    expect(receipt.errors).toEqual([]);
    expect(receipt.identifiers.map((r) => r.entity_type)).toEqual(["appointment", "notification"]);
    expect(receipt.values.find((f) => f.field === "appointment_reference")?.value).toMatchObject({
      kind: "text",
      value: expect.stringMatching(/^APT-[0-9A-Z]{6}$/u),
    });
    expect(receipt.follow_up).toMatchObject({ state: "queued" });
    expect(world.app.db.prepare("SELECT status FROM slots WHERE id = ?").get(slotId("slotA1"))).toEqual({ status: "booked" });
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM notifications WHERE kind = 'booking_confirmation'").get()).toEqual({ n: 1 });
  });

  it("answers a repeated commit with the original receipt and changes nothing", () => {
    const prepared = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now() });
    const first = commit("receptionA1", prepared);
    const appointments = world.app.db.prepare("SELECT COUNT(*) AS n FROM appointments").get();
    const second = commit("receptionA1", prepared);
    expect(second).toEqual(first);
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM appointments").get()).toEqual(appointments);
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM notifications WHERE kind = 'booking_confirmation'").get()).toEqual({ n: 1 });
  });

  it("refuses a key reused for a different action", () => {
    const key = idemKey();
    const first = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now(), key });
    expect(commit("receptionA1", first).status).toBe("ok");
    const different = preparedActionFrom("appointments.book", prepareBooking("receptionA1", bookFields("slotA1b", "patientA1b")), {
      now: world.clock.now(),
      key,
    });
    const result = commit("receptionA1", different);
    expect(result.status).toBe("rejected");
    expect(result.errors[0]).toMatchObject({ code: "conflict", field: "idempotency_key" });
    expect(world.app.db.prepare("SELECT status FROM slots WHERE id = ?").get(slotId("slotA1b"))).toEqual({ status: "free" });
  });

  it("refuses a key another principal already used", () => {
    const key = idemKey();
    const prepared = preparedActionFrom("appointments.book", prepareBooking("patientA1", bookFields("slotA1", "patientA1")), {
      now: world.clock.now(),
      key,
    });
    expect(commit("patientA1", prepared).status).toBe("ok");
    const result = commit("receptionA1", prepared);
    expect(failureCode(result)).toBe("conflict");
  });

  it("is stale when the slot was taken between prepare and commit, and applies nothing", () => {
    const mine = preparedActionFrom("appointments.book", prepareBooking("receptionA1", bookFields("slotA1", "patientA1")), {
      now: world.clock.now(),
    });
    const theirs = preparedActionFrom("appointments.book", prepareBooking("receptionA1", bookFields("slotA1", "patientA1b")), {
      now: world.clock.now(),
    });
    expect(commit("receptionA1", theirs).status).toBe("ok");

    const result = commit("receptionA1", mine);
    expect(result.status).toBe("rejected");
    expect(failureCode(result)).toBe("stale");
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM appointments WHERE slot_id = ?").get(slotId("slotA1"))).toEqual({ n: 1 });
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM idempotency").get()).toEqual({ n: 1 });
  });

  it("is stale when another record the booking relied on changed, even though the slot is still free", () => {
    const prepared = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now() });
    const update = world.app.service.contact.prepare(world.identity("receptionA1"), {
      patient: patientId("patientA1"),
      email: "new.address@example.test",
    });
    world.app.service.contact.commit(
      world.identity("receptionA1"),
      idemKey(),
      preparedActionFrom("patients.update_contact", update, { now: world.clock.now(), mode: "none" }),
    );
    expect(failureCode(commit("receptionA1", prepared))).toBe("stale");
  });

  it("refuses an expired preparation", () => {
    const prepared = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now(), ttlSeconds: 600 });
    const before = world.app.db.prepare("SELECT COUNT(*) AS n FROM appointments").get();
    world.clock.advanceSeconds(601);
    const result = commit("receptionA1", prepared);
    expect(result.status).toBe("rejected");
    expect(failureCode(result)).toBe("expired");
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM appointments").get()).toEqual(before);
  });

  it("refuses a malformed or incomplete prepared action without applying anything", () => {
    const identity = world.identity("receptionA1");
    expect(world.app.service.book.commit(identity, idemKey(), undefined).errors[0]?.code).toBe("validation_failed");
    expect(world.app.service.book.commit(identity, idemKey(), { effects: [], data_versions: [] }).errors[0]?.code).toBe("validation_failed");
    expect(world.app.service.book.commit(identity, "short", {}).errors[0]).toMatchObject({
      code: "validation_failed",
      field: "idempotency_key",
    });
    const prepared = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now() });
    const noVersions = { ...prepared, data_versions: [] };
    expect(commit("receptionA1", noVersions).errors[0]?.code).toBe("validation_failed");
    expect(world.app.db.prepare("SELECT status FROM slots WHERE id = ?").get(slotId("slotA1"))).toEqual({ status: "free" });
  });

  it("refuses a prepared action for another capability", () => {
    const prepared = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now() });
    const result = world.app.service.cancel.commit(world.identity("receptionA1"), prepared.idempotency_key, prepared);
    expect(failureCode(result)).toBe("validation_failed");
  });

  it("re-checks the permission at commit, not only at prepare", () => {
    const prepared = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now() });
    const result = commit("doctorA1", prepared);
    expect(failureCode(result)).toBe("permission_denied");
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM idempotency").get()).toEqual({ n: 0 });
  });
});

describe("appointments.book: double booking", () => {
  it("refuses a slot that is already booked", () => {
    const taken = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now() });
    expect(commit("receptionA1", taken).status).toBe("ok");
    const again = prepareBooking("receptionA1", bookFields("slotA1", "patientA1b"));
    expect(again.status).toBe("rejected");
    expect(failureCode(again)).toBe("conflict");
  });

  it("refuses a second appointment for the same patient at the same time", () => {
    // slotA1 and slotA1b are consecutive, so book the patient at slotA1 and then try the same instant on a second doctor.
    const first = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now() });
    expect(commit("receptionA1", first).status).toBe("ok");
    const startsAt = world.app.db.prepare("SELECT starts_at FROM slots WHERE id = ?").get(slotId("slotA1")) as { starts_at: string };
    const tenant = "tnt_a";
    const site = "site_a1";
    const doctor = "doc_otherdoctor01";
    world.app.db.prepare("INSERT INTO doctors (tenant_id, site_id, id, name, specialty) VALUES (?, ?, ?, 'Dr. Second Doctor', 'general practice')").run(tenant, site, doctor);
    world.app.db
      .prepare(
        "INSERT INTO slots (tenant_id, site_id, id, doctor_id, starts_at, duration_minutes, status, version, updated_at) VALUES (?, ?, 'slot_parallel0001', ?, ?, 30, 'free', 1, ?)",
      )
      .run(tenant, site, doctor, startsAt.starts_at, startsAt.starts_at);
    const result = world.app.service.book.prepare(world.identity("receptionA1"), {
      patient: patientId("patientA1"),
      slot: "slot_parallel0001",
    });
    expect(result.status).toBe("rejected");
    expect(result.errors[0]).toMatchObject({ code: "conflict", field: "patient" });
  });

  it("is backed by the database: two active appointments for one slot cannot exist", () => {
    const prepared = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now() });
    expect(commit("receptionA1", prepared).status).toBe("ok");
    const insert = (): unknown =>
      world.app.db
        .prepare(
          `INSERT INTO appointments (tenant_id, site_id, id, reference, slot_id, patient_id, doctor_id, starts_at, status, version, created_at, updated_at)
           VALUES ('tnt_a', 'site_a1', 'apt_second00001', 'APT-ZZZZZZ', ?, ?, 'doc_x', '2026-10-06T07:00:00Z', 'booked', 1, 'x', 'x')`,
        )
        .run(slotId("slotA1"), patientId("patientA1b"));
    expect(insert).toThrow(/UNIQUE/u);
  });
});

describe("appointments.book: time window", () => {
  it("refuses a slot that starts too soon or too far ahead", () => {
    const soon = slotId("slotA1");
    const row = world.app.db.prepare("SELECT starts_at FROM slots WHERE id = ?").get(soon) as { starts_at: string };
    // Move the clock to 30 minutes before the slot.
    world.clock.set(new Date(new Date(row.starts_at).getTime() - 30 * 60_000).toISOString());
    const tooSoon = prepareBooking();
    expect(tooSoon.errors[0]).toMatchObject({ code: "validation_failed", field: "slot" });
    world.clock.set("2026-01-01T00:00:00Z");
    const tooFar = prepareBooking();
    expect(tooFar.errors[0]).toMatchObject({ code: "validation_failed", field: "slot" });
  });
});

describe("appointments.book: status by idempotency key", () => {
  it("returns the original receipt after commit", () => {
    const prepared = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now() });
    const receipt = commit("receptionA1", prepared);
    const status = world.app.service.book.status(world.identity("receptionA1"), prepared.idempotency_key);
    expect(status).toEqual(receipt);
  });

  it("says nothing was recorded in exactly one shape", () => {
    const status = world.app.service.book.status(world.identity("receptionA1"), idemKey());
    expect(status.status).toBe("rejected");
    expect(status.identifiers).toEqual([]);
    expect(status.errors).toHaveLength(1);
    expect(status.errors[0]).toMatchObject({ code: "validation_failed", field: "idempotency_key" });
  });

  it("never claims nothing happened for another principal's key", () => {
    const prepared = preparedActionFrom("appointments.book", prepareBooking(), { now: world.clock.now() });
    commit("receptionA1", prepared);
    const status = world.app.service.book.status(world.identity("receptionA2"), prepared.idempotency_key);
    // receptionA2 is another site: the key does not exist in its scope at all.
    expect(status.status).toBe("rejected");
    expect(status.errors[0]).toMatchObject({ code: "validation_failed", field: "idempotency_key" });
    // Inside the same site another principal is refused outright, not told "no record".
    const sameSite = world.app.service.book.status(world.identity("patientA1"), prepared.idempotency_key);
    expect(failureCode(sameSite)).toBe("permission_denied");
  });
});
