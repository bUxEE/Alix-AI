import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { isRefusal } from "../src/errors.ts";
import { createDisposableTenant, seedRecords } from "../src/testsupport.ts";
import { createWorld, type World } from "./helpers/world.ts";

// The audit's conversations name what exists ("Maria Rossi", "APT-7K3Q9X"), so a seed
// record may carry the names and the reference it should have. Without them the
// application invents synthetic ones.

let world: World;

beforeEach(() => {
  world = createWorld();
});

afterEach(async () => {
  await world.close();
});

const env = () => world.app.service.env;

const tenant = (id: string): void => {
  createDisposableTenant(env(), {
    tenant_id: id,
    workspaces: [{ workspace_id: `${id}_site` }],
    principals: [
      { principal_id: `${id}_reception`, workspace_id: `${id}_site`, roles: ["receptionist"] },
      { principal_id: `${id}_patient`, workspace_id: `${id}_site`, roles: ["patient"] },
      { principal_id: `${id}_doctor`, workspace_id: `${id}_site`, roles: ["doctor"] },
    ],
  });
};

const identity = (id: string, who: "reception" | "patient" | "doctor") => ({
  application_id: "clinic-booking",
  tenant_id: id,
  workspace_id: `${id}_site`,
  principal_id: `${id}_${who}`,
  roles: [who === "reception" ? "receptionist" : who],
  session_id: "ses_01J0000000000000000000000A",
});

const refusalOf = (action: () => unknown): string | undefined => {
  try {
    action();
  } catch (error) {
    return isRefusal(error) ? error.refusal.code : "not a refusal";
  }
  return undefined;
};

describe("seed records can carry the names and the reference they should have", () => {
  it("names a patient, a doctor and an appointment, so a conversation can find them by what it says", () => {
    tenant("tnt_seed1");
    const seeded = seedRecords(env(), {
      tenant_id: "tnt_seed1",
      records: [
        { ref: "maria", entity_type: "patient", workspace_id: "tnt_seed1_site", principal_id: "tnt_seed1_patient", given_name: "Maria", family_name: "Rossi" },
        { ref: "luca", entity_type: "patient", workspace_id: "tnt_seed1_site", given_name: "Luca", family_name: "Ferri" },
        { ref: "verdi", entity_type: "doctor", workspace_id: "tnt_seed1_site", principal_id: "tnt_seed1_doctor", name: "Dr. Anna Verdi" },
        { ref: "visit", entity_type: "appointment", workspace_id: "tnt_seed1_site", principal_id: "tnt_seed1_patient", reference: "APT-7K3Q9X" },
      ],
    });
    const byRef = Object.fromEntries(seeded.records.map((r) => [r.ref, r.entity_id]));
    const s = world.app.service;
    const reception = identity("tnt_seed1", "reception");

    // a linked principal's own record is renamed, an unlinked one is created with the name
    const maria = s.resolvePatient(reception, { entity_type: "patient", mention: "Maria Rossi" });
    expect(maria.identifiers.map((r) => r.entity_id)).toEqual([byRef.maria]);
    expect(s.resolvePatient(reception, { entity_type: "patient", mention: "Luca Ferri" }).identifiers.map((r) => r.entity_id)).toEqual([byRef.luca]);
    expect(s.resolveDoctor(reception, { entity_type: "doctor", mention: "Dr. Verdi" }).identifiers.map((r) => r.entity_id)).toEqual([byRef.verdi]);
    // the patient principal sees itself under the new name
    expect(s.resolvePatient(identity("tnt_seed1", "patient"), { mention: "me" }).identifiers.map((r) => r.entity_id)).toEqual([byRef.maria]);
    // the appointment answers to its fixed reference, for the receptionist and the patient it belongs to
    for (const who of ["reception", "patient"] as const) {
      const found = s.resolveAppointment(identity("tnt_seed1", who), { entity_type: "appointment", mention: "APT-7K3Q9X" });
      expect(found.identifiers.map((r) => r.entity_id), who).toEqual([byRef.visit]);
    }
    // and it is bookable through the real operations
    expect(s.cancel.prepare(reception, { appointment: byRef.visit }).status).toBe("ok");
  });

  it("still invents synthetic data when nothing is given", () => {
    tenant("tnt_seed2");
    const seeded = seedRecords(env(), {
      tenant_id: "tnt_seed2",
      records: [
        { ref: "p", entity_type: "patient", workspace_id: "tnt_seed2_site" },
        { ref: "a", entity_type: "appointment", workspace_id: "tnt_seed2_site" },
      ],
    });
    const patient = world.app.db.prepare("SELECT family_name FROM patients WHERE id = ?").get(seeded.records[0]?.entity_id ?? "") as { family_name: string };
    expect(patient.family_name).toMatch(/^Testpatient\d+$/u);
    const reference = world.app.db.prepare("SELECT reference FROM appointments WHERE id = ?").get(seeded.records[1]?.entity_id ?? "") as { reference: string };
    expect(reference.reference).toMatch(/^APT-[0-9A-Z]{6}$/u);
  });

  it("refuses data that does not belong on the record, half a name, a malformed reference and a reused one", () => {
    tenant("tnt_seed3");
    const seed = (record: Record<string, unknown>): string | undefined =>
      refusalOf(() => seedRecords(env(), { tenant_id: "tnt_seed3", records: [{ ref: "r", workspace_id: "tnt_seed3_site", ...record }] }));
    expect(seed({ entity_type: "patient", given_name: "Maria" })).toBe("validation_failed");
    expect(seed({ entity_type: "slot", given_name: "Maria", family_name: "Rossi" })).toBe("validation_failed");
    expect(seed({ entity_type: "patient", name: "Dr. Nobody" })).toBe("validation_failed");
    expect(seed({ entity_type: "slot", reference: "APT-7K3Q9X" })).toBe("validation_failed");
    expect(seed({ entity_type: "appointment", reference: "not a reference" })).toBe("validation_failed");
    expect(seed({ entity_type: "appointment", reference: "APT-ILOUUU" })).toBe("validation_failed");
    expect(seed({ entity_type: "patient", given_name: "x\u0007", family_name: "y" })).toBe("validation_failed");
    expect(seed({ entity_type: "patient", given_name: "", family_name: "y" })).toBe("validation_failed");
    expect(seed({ entity_type: "appointment", reference: "APT-7K3Q9X" })).toBeUndefined();
    expect(seed({ entity_type: "appointment", reference: "APT-7K3Q9X" })).toBe("conflict");
    // nothing from the refused call was left behind
    const count = world.app.db.prepare("SELECT COUNT(*) AS n FROM appointments WHERE tenant_id = 'tnt_seed3'").get() as { n: number };
    expect(count.n).toBe(1);
  });
});
