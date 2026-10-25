import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { APPLICATION_ID, CAPABILITY_PERMISSION, ROLE_PERMISSIONS, roleCapabilities, type Role } from "../src/access.ts";
import type { HostResult } from "../src/results.ts";
import { policy } from "./helpers/package.ts";
import { createWorld, type World } from "./helpers/world.ts";

let world: World;

beforeEach(() => {
  world = createWorld();
});

afterEach(async () => {
  await world.close();
});

const id = (ref: string): string => world.records[ref]?.entity_id ?? "";
const code = (result: HostResult): string | undefined => result.errors[0]?.code;
const slotIds = (result: HostResult): string[] => result.identifiers.filter((r) => r.entity_type === "slot").map((r) => r.entity_id);

describe("the identity is checked on every call", () => {
  const call = (identity: unknown): HostResult => world.app.service.availabilityList(identity, {});

  it("refuses a request with no identity or an incomplete one as scope_missing", () => {
    expect(code(call(undefined))).toBe("scope_missing");
    const { session_id: _omitted, ...withoutSession } = world.identity("receptionA1");
    expect(code(call(withoutSession))).toBe("scope_missing");
    expect(code(call({ ...world.identity("receptionA1"), roles: [] }))).toBe("scope_missing");
    expect(code(call({ ...world.identity("receptionA1"), tenant_id: "  " }))).toBe("scope_missing");
  });

  it("refuses wildcards, whitespace and non-strings as scope_violation", () => {
    expect(code(call({ ...world.identity("receptionA1"), tenant_id: "*" }))).toBe("scope_violation");
    expect(code(call({ ...world.identity("receptionA1"), workspace_id: "site a1" }))).toBe("scope_violation");
    expect(code(call({ ...world.identity("receptionA1"), principal_id: 7 }))).toBe("scope_missing");
  });

  it("refuses another application's identity", () => {
    expect(code(call({ ...world.identity("receptionA1"), application_id: "other-app" }))).toBe("scope_violation");
    expect(APPLICATION_ID).toBe("clinic-booking");
  });

  it("does not trust the roles an identity asserts", () => {
    // A patient claiming to be a receptionist is an escalation attempt.
    expect(code(call({ ...world.identity("patientA1"), roles: ["receptionist"] }))).toBe("permission_denied");
    expect(code(call({ ...world.identity("patientA1"), roles: ["patient", "receptionist"] }))).toBe("permission_denied");
    expect(call({ ...world.identity("patientA1"), roles: ["patient"] }).status).toBe("ok");
  });

  it("refuses a principal presented in the wrong workspace or tenant, without saying which is wrong", () => {
    const wrongSite = { ...world.identity("receptionA1"), workspace_id: "site_a2" };
    const wrongTenant = { ...world.identity("receptionA1"), tenant_id: "tnt_b" };
    const unknown = { ...world.identity("receptionA1"), tenant_id: "tnt_nobody" };
    for (const identity of [wrongSite, wrongTenant, unknown]) {
      const result = call(identity);
      expect(result.status).toBe("rejected");
      expect(result.errors[0]).toMatchObject({ code: "scope_violation", message: "the principal is not known in this tenant and workspace" });
    }
  });
});

describe("a receptionist of one site cannot reach another site's records", () => {
  it("availability never lists another site's slots, and another site's doctor is out of scope", () => {
    const result = world.app.service.availabilityList(world.identity("receptionA1"), { days: 14, max_results: 10 });
    expect(result.status).toBe("ok");
    expect(slotIds(result)).not.toContain(id("slotA2"));
    expect(slotIds(result)).not.toContain(id("slotB1"));
    const doctorOfA1 = world.app.db.prepare("SELECT doctor_id FROM slots WHERE id = ?").get(id("slotA1")) as { doctor_id: string };
    const foreignDoctor = world.app.db.prepare("SELECT doctor_id FROM slots WHERE id = ?").get(id("slotA2")) as { doctor_id: string };
    expect(world.app.service.availabilityList(world.identity("receptionA1"), { doctor: doctorOfA1.doctor_id }).status).toBe("ok");
    expect(code(world.app.service.availabilityList(world.identity("receptionA1"), { doctor: foreignDoctor.doctor_id }))).toBe("scope_violation");
  });

  it("booking another site's slot, or another tenant's, is a scope violation and applies nothing", () => {
    for (const slot of ["slotA2", "slotB1"]) {
      const result = world.app.service.book.prepare(world.identity("receptionA1"), { patient: id("patientA1"), slot: id(slot) });
      expect(code(result)).toBe("scope_violation");
    }
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM idempotency").get()).toEqual({ n: 0 });
  });

  it("an unknown record and an out-of-scope record are refused identically", () => {
    const outOfScope = world.app.service.book.prepare(world.identity("receptionA1"), { patient: id("patientA1"), slot: id("slotA2") });
    const unknown = world.app.service.book.prepare(world.identity("receptionA1"), { patient: id("patientA1"), slot: "slot_doesnotexist01" });
    expect(outOfScope).toEqual(unknown);
  });

  it("searching by name never surfaces another site's or tenant's patients", () => {
    const names = world.app.db.prepare("SELECT given_name, family_name FROM patients WHERE id IN (?, ?)").all(id("patientA2"), id("patientB1")) as {
      given_name: string;
      family_name: string;
    }[];
    for (const n of names) {
      const result = world.app.service.patientsFind(world.identity("receptionA1"), { query: n.family_name });
      expect(result.status).toBe("ok");
      expect(result.identifiers).toEqual([]);
    }
    // An identifier is an address: out of scope is refused, in scope is found.
    expect(code(world.app.service.patientsFind(world.identity("receptionA1"), { query: id("patientA2") }))).toBe("scope_violation");
    expect(world.app.service.patientsFind(world.identity("receptionA1"), { query: id("patientA1") }).identifiers).toHaveLength(1);
  });

  it("cancelling another site's appointment is a scope violation", () => {
    expect(code(world.app.service.cancel.prepare(world.identity("receptionA1"), { appointment: id("apptA2") }))).toBe("scope_violation");
    expect(code(world.app.service.cancel.prepare(world.identity("receptionB1"), { appointment: id("apptA1") }))).toBe("scope_violation");
  });
});

describe("record-level permissions per role", () => {
  it("a patient sees only themselves", () => {
    const own = world.app.service.patientsFind(world.identity("patientA1"), { query: "Testpatient" });
    expect(own.identifiers).toEqual([{ entity_type: "patient", entity_id: id("patientA1") }]);
    // Another patient of the same site is invisible, by name and by identifier.
    const other = world.app.db.prepare("SELECT family_name FROM patients WHERE id = ?").get(id("patientA1b")) as { family_name: string };
    expect(world.app.service.patientsFind(world.identity("patientA1"), { query: other.family_name }).identifiers).toEqual([]);
    expect(code(world.app.service.patientsFind(world.identity("patientA1"), { query: id("patientA1b") }))).toBe("scope_violation");
    expect(code(world.app.service.book.prepare(world.identity("patientA1"), { patient: id("patientA1b"), slot: id("slotA1") }))).toBe("scope_violation");
  });

  it("a doctor sees only patients they have an appointment with", () => {
    const visible = world.app.service.patientsFind(world.identity("doctorA1"), { query: "Testpatient", max_results: 10 });
    // Only patientA1 has an appointment (apptA1) with the site's doctor.
    expect(visible.identifiers).toEqual([{ entity_type: "patient", entity_id: id("patientA1") }]);
    expect(code(world.app.service.patientsFind(world.identity("doctorA1"), { query: id("patientA1b") }))).toBe("scope_violation");
  });

  it("a receptionist sees every patient of their site", () => {
    const found = world.app.service.patientsFind(world.identity("receptionA1"), { query: "Testpatient", max_results: 10 });
    expect(found.identifiers.map((r) => r.entity_id).sort()).toEqual([id("patientA1"), id("patientA1b")].sort());
  });

  it("resolvers apply the same visibility", () => {
    const self = world.app.service.resolvePatient(world.identity("patientA1"), { mention: "me" });
    expect(self.identifiers).toEqual([{ entity_type: "patient", entity_id: id("patientA1") }]);
    const stranger = world.app.service.resolvePatient(world.identity("receptionA1"), { mention: id("patientA2") });
    expect(code(stranger)).toBe("scope_violation");
    expect(code(world.app.service.resolveAppointment(world.identity("patientA1b"), { mention: id("apptA1") }))).toBe("scope_violation");
  });
});

describe("the permission matrix", () => {
  it("grants exactly the capabilities the package's policy file lists", () => {
    // the policy file is read from the package, not restated here
    for (const grant of policy.role_grants) {
      expect([...grant.capabilities].sort(), `role ${grant.role}`).toEqual(roleCapabilities(grant.role as Role).sort());
    }
    expect(policy.role_grants.map((g) => g.role)).toEqual(Object.keys(ROLE_PERMISSIONS));
    expect(roleCapabilities("receptionist").sort()).toEqual(Object.keys(CAPABILITY_PERMISSION).sort());
    expect(roleCapabilities("doctor").sort()).toEqual(["appointments.cancel", "availability.list", "patients.find"]);
  });

  it("denies a doctor booking and contact changes at every operation", () => {
    const identity = world.identity("doctorA1");
    const fields = { patient: id("patientA1"), slot: id("slotA1") };
    expect(code(world.app.service.book.validate(identity, fields))).toBe("permission_denied");
    expect(code(world.app.service.book.prepare(identity, fields))).toBe("permission_denied");
    expect(code(world.app.service.book.commit(identity, "idem_000000000000000000", { effects: [{ operation: "create", entity_type: "appointment" }], data_versions: [] }))).toBe(
      "permission_denied",
    );
    expect(code(world.app.service.book.status(identity, "idem_000000000000000000"))).toBe("permission_denied");
  });
});
