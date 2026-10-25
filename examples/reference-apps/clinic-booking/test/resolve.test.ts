import { afterEach, beforeEach, describe, expect, it } from "vitest";

import type { HostResult } from "../src/results.ts";
import { createWorld, type World } from "./helpers/world.ts";

// Entity resolution is an adapter READ with the reserved arguments entity_type,
// mention and candidate_id (the core's AdapterResolver). The core reads the
// candidates from the identifiers, the display text of the i-th candidate from
// the i-th text value named "label", and the data version from the freshness.

let world: World;

beforeEach(() => {
  world = createWorld();
});

afterEach(async () => {
  await world.close();
});

const id = (ref: string): string => world.records[ref]?.entity_id ?? "";
const code = (result: HostResult): string | undefined => result.errors[0]?.code;
const ids = (result: HostResult): string[] => result.identifiers.map((r) => r.entity_id);
const labels = (result: HostResult): string[] =>
  result.values.flatMap((f) => (f.field === "label" && f.value.kind === "text" ? [f.value.value] : []));

describe("a resolution is a read with the reserved arguments", () => {
  it("returns several matches as several candidates, each with a label at the same position", () => {
    const s = world.app.service;
    const result = s.resolvePatient(world.identity("receptionA1"), {
      entity_type: "patient",
      mention: "Testpatient",
    });
    expect(result.status).toBe("ok");
    expect(new Set(ids(result))).toEqual(new Set([id("patientA1"), id("patientA1b")]));
    expect(ids(result)).toHaveLength(2);
    expect(result.identifiers.every((r) => r.entity_type === "patient")).toBe(true);
    expect(labels(result)).toHaveLength(2);
    expect(new Set(labels(result)).size).toBe(2);
    expect(result.freshness?.version).toMatch(/^rv1-/u);
    const count = result.values.find((f) => f.field === "candidate_count");
    expect(count?.value).toEqual({ kind: "integer", value: 2 });
  });

  it("answers a mention that matches nothing with an empty, successful list", () => {
    const result = world.app.service.resolvePatient(world.identity("receptionA1"), {
      entity_type: "patient",
      mention: "Nobodyhere",
    });
    expect(result.status).toBe("ok");
    expect(result.identifiers).toEqual([]);
    expect(labels(result)).toEqual([]);
  });

  it("refuses to answer for another entity type", () => {
    const result = world.app.service.resolvePatient(world.identity("receptionA1"), {
      entity_type: "doctor",
      mention: "Testpatient",
    });
    expect(result.status).toBe("rejected");
    expect(code(result)).toBe("validation_failed");
    expect(result.errors[0]?.field).toBe("entity_type");
  });

  it("needs a mention or a candidate, and refuses any other argument", () => {
    const s = world.app.service;
    const who = world.identity("receptionA1");
    expect(code(s.resolvePatient(who, { entity_type: "patient" }))).toBe("missing_fields");
    expect(code(s.resolvePatient(who, { mention: "Test", sneaky: "x" }))).toBe("validation_failed");
    expect(code(s.resolvePatient(who, { mention: "Test", candidate_id: "has space" }))).toBe("validation_failed");
  });

  it("still takes the plain mention without the reserved type", () => {
    const result = world.app.service.resolveDoctor(world.identity("receptionA1"), { mention: "Dr. Synthetic" });
    expect(result.status).toBe("ok");
    expect(result.identifiers.every((r) => r.entity_type === "doctor")).toBe(true);
  });
});

describe("candidate_id is the model's proposal, never an instruction", () => {
  it("re-resolves exactly the named record when there is no mention", () => {
    const result = world.app.service.resolvePatient(world.identity("receptionA1"), {
      entity_type: "patient",
      candidate_id: id("patientA1"),
    });
    expect(result.status).toBe("ok");
    expect(ids(result)).toEqual([id("patientA1")]);
    expect(labels(result)).toHaveLength(1);
  });

  it("refuses a record the caller may not see exactly like one that does not exist", () => {
    const s = world.app.service;
    const who = world.identity("receptionA1");
    const elsewhere = s.resolvePatient(who, { entity_type: "patient", candidate_id: id("patientA2") });
    const otherTenant = s.resolvePatient(who, { entity_type: "patient", candidate_id: id("patientB1") });
    const unknown = s.resolvePatient(who, { entity_type: "patient", candidate_id: "pat_doesnotexist1" });
    for (const result of [elsewhere, otherTenant, unknown]) {
      expect(result.status).toBe("rejected");
      expect(result.identifiers).toEqual([]);
      expect(result.errors).toEqual([expect.objectContaining({ code: "scope_violation" })]);
    }
    expect(elsewhere.errors).toEqual(unknown.errors);
    expect(otherTenant.errors).toEqual(unknown.errors);
  });

  it("applies the visibility of the caller's role to the hint", () => {
    const s = world.app.service;
    // a patient sees only themselves: another patient of the same site is out of scope
    const own = s.resolvePatient(world.identity("patientA1"), { entity_type: "patient", candidate_id: id("patientA1") });
    expect(ids(own)).toEqual([id("patientA1")]);
    const other = s.resolvePatient(world.identity("patientA1"), { entity_type: "patient", candidate_id: id("patientA1b") });
    expect(code(other)).toBe("scope_violation");
  });

  it("never narrows an ambiguous mention to the proposed record: the user, not the model, chooses", () => {
    const s = world.app.service;
    const who = world.identity("receptionA1");
    const both = s.resolvePatient(who, { entity_type: "patient", mention: "Testpatient" });
    expect(ids(both)).toHaveLength(2);
    for (const proposed of [id("patientA1"), id("patientA1b")]) {
      const hinted = s.resolvePatient(who, { entity_type: "patient", mention: "Testpatient", candidate_id: proposed });
      expect(ids(hinted), `hint ${proposed}`).toEqual(ids(both));
      expect(labels(hinted)).toEqual(labels(both));
      expect(hinted.freshness?.version).toBe(both.freshness?.version);
    }
  });

  it("answers a mention exactly the same with any hint, so a hint cannot add a candidate or reveal a record", () => {
    const s = world.app.service;
    const who = world.identity("receptionA1");
    const plain = s.resolvePatient(who, { entity_type: "patient", mention: "Testpatient" });
    for (const proposed of [id("patientA2"), id("patientB1"), "pat_doesnotexist1"]) {
      const hinted = s.resolvePatient(who, { entity_type: "patient", mention: "Testpatient", candidate_id: proposed });
      expect(hinted).toEqual(plain);
    }
    // and a mention that matches nothing stays empty however the model hints
    const none = s.resolvePatient(who, { entity_type: "patient", mention: "Nobodyhere", candidate_id: id("patientA1") });
    expect(none.status).toBe("ok");
    expect(none.identifiers).toEqual([]);
  });

  it("works for slots, doctors and appointments too, with the same visibility", () => {
    const s = world.app.service;
    const who = world.identity("receptionA1");
    expect(ids(s.resolveSlot(who, { entity_type: "slot", candidate_id: id("slotA1") }))).toEqual([id("slotA1")]);
    expect(code(s.resolveSlot(who, { entity_type: "slot", candidate_id: id("slotA2") }))).toBe("scope_violation");
    expect(ids(s.resolveDoctor(who, { entity_type: "doctor", candidate_id: id("doctorA1") }))).toEqual([id("doctorA1")]);
    expect(ids(s.resolveAppointment(who, { entity_type: "appointment", candidate_id: id("apptA1") }))).toEqual([id("apptA1")]);
    expect(code(s.resolveAppointment(who, { entity_type: "appointment", candidate_id: id("apptA2") }))).toBe("scope_violation");
  });
});

describe("\"the next available one\" is decided by the host", () => {
  const slotIds = (): string[] =>
    (world.app.db.prepare("SELECT id FROM slots WHERE tenant_id = 'tnt_a' AND site_id = 'site_a1' AND status = 'free' ORDER BY starts_at, id").all() as {
      id: string;
    }[]).map((r) => r.id);

  it("resolves English and Italian phrases to the earliest bookable free slot of the caller's site, and only that one", () => {
    const s = world.app.service;
    const who = world.identity("receptionA1");
    const earliest = slotIds()[0];
    for (const mention of ["next available", "the next available appointment", "earliest free slot", "first available", "il primo orario libero", "prossimo appuntamento disponibile"]) {
      const result = s.resolveSlot(who, { entity_type: "slot", mention });
      expect(ids(result), mention).toEqual([earliest]);
      expect(labels(result), mention).toHaveLength(1);
    }
  });

  it("skips a slot that starts too soon to be booked, and never looks at another site", () => {
    const s = world.app.service;
    const who = world.identity("receptionA1");
    const [first = "", second = ""] = slotIds();
    const startsAt = (world.app.db.prepare("SELECT starts_at FROM slots WHERE id = ?").get(first) as { starts_at: string }).starts_at;
    world.clock.set(new Date(new Date(startsAt).getTime() - 30 * 60_000).toISOString());
    expect(ids(s.resolveSlot(who, { mention: "next available" }))).toEqual([second]);
    expect(ids(s.resolveSlot(world.identity("receptionA2"), { mention: "next available" }))).not.toContain(first);
  });

  it("answers nothing, not an error, when no slot is free, and leaves other words alone", () => {
    const s = world.app.service;
    const who = world.identity("receptionA1");
    world.clock.set("2027-12-31T00:00:00Z");
    expect(s.resolveSlot(who, { mention: "next available" }).identifiers).toEqual([]);
    world.clock.set("2026-10-05T07:00:00Z");
    expect(code(s.resolveSlot(who, { mention: "tomorrow" }))).toBe("validation_failed");
    expect(code(s.resolveSlot(who, { mention: "available" }))).toBe("validation_failed");
  });

  it("answers every role that may list availability, with the same slot", () => {
    const s = world.app.service;
    const earliest = slotIds()[0];
    for (const who of ["receptionA1", "patientA1", "doctorA1"]) {
      expect(ids(s.resolveSlot(world.identity(who), { mention: "first free slot" })), who).toEqual([earliest]);
    }
  });
});
