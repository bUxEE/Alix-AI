import { randomBytes } from "node:crypto";

import { createApp, type ClinicApp } from "../../src/app.ts";
import { APPLICATION_ID, type Identity, type Role } from "../../src/access.ts";
import { createMutableClock, toTimestamp, type MutableClock } from "../../src/clock.ts";
import type { HostResult } from "../../src/results.ts";
import { createDisposableTenant, seedRecords, type SeededRecord } from "../../src/testsupport.ts";

// 2026-10-05 is a Monday; 07:00Z is 09:00 in Rome (CEST, UTC+2).
export const START = "2026-10-05T07:00:00Z";

export type Who = {
  tenant: string;
  site: string;
  principal: string;
  role: Role;
};

export type World = {
  app: ClinicApp;
  clock: MutableClock;
  who: Record<string, Who>;
  records: Record<string, SeededRecord>;
  identity: (name: string) => Identity;
  close: () => Promise<void>;
};

// Two clinic groups, a few sites, every role, and synthetic records, all in a
// throw-away in-memory database.
export const createWorld = (options: { bearer?: string } = {}): World => {
  const clock = createMutableClock(START);
  const app = createApp({ clock, testSupport: true, ...(options.bearer === undefined ? {} : { mcpBearer: options.bearer }) });
  const env = app.service.env;

  const tenantA = createDisposableTenant(env, {
    tenant_id: "tnt_a",
    workspaces: [
      { workspace_id: "site_a1", name: "Synthetic Clinic A1" },
      { workspace_id: "site_a2", name: "Synthetic Clinic A2" },
    ],
    principals: [
      { principal_id: "usr_reception_a1", workspace_id: "site_a1", roles: ["receptionist"] },
      { principal_id: "usr_reception_a2", workspace_id: "site_a2", roles: ["receptionist"] },
      { principal_id: "usr_doctor_a1", workspace_id: "site_a1", roles: ["doctor"], display_name: "Anna Verdi" },
      { principal_id: "usr_patient_a1", workspace_id: "site_a1", roles: ["patient"] },
      { principal_id: "usr_patient_a1b", workspace_id: "site_a1", roles: ["patient"] },
      { principal_id: "usr_patient_a2", workspace_id: "site_a2", roles: ["patient"] },
    ],
  });
  const tenantB = createDisposableTenant(env, {
    tenant_id: "tnt_b",
    workspaces: [{ workspace_id: "site_b1", name: "Synthetic Clinic B1" }],
    principals: [{ principal_id: "usr_reception_b1", workspace_id: "site_b1", roles: ["receptionist"] }],
  });

  const who: Record<string, Who> = {
    receptionA1: { tenant: "tnt_a", site: "site_a1", principal: "usr_reception_a1", role: "receptionist" },
    receptionA2: { tenant: "tnt_a", site: "site_a2", principal: "usr_reception_a2", role: "receptionist" },
    doctorA1: { tenant: "tnt_a", site: "site_a1", principal: "usr_doctor_a1", role: "doctor" },
    patientA1: { tenant: "tnt_a", site: "site_a1", principal: "usr_patient_a1", role: "patient" },
    patientA1b: { tenant: "tnt_a", site: "site_a1", principal: "usr_patient_a1b", role: "patient" },
    patientA2: { tenant: "tnt_a", site: "site_a2", principal: "usr_patient_a2", role: "patient" },
    receptionB1: { tenant: "tnt_b", site: "site_b1", principal: "usr_reception_b1", role: "receptionist" },
  };
  void tenantA;
  void tenantB;

  const records: Record<string, SeededRecord> = {};
  const add = (tenant: string, list: { ref: string; entity_type: string; workspace_id: string; principal_id?: string }[]): void => {
    for (const r of seedRecords(env, { tenant_id: tenant, records: list }).records) {
      records[r.ref] = r;
    }
  };
  add("tnt_a", [
    { ref: "doctorA1", entity_type: "doctor", workspace_id: "site_a1", principal_id: "usr_doctor_a1" },
    { ref: "patientA1", entity_type: "patient", workspace_id: "site_a1", principal_id: "usr_patient_a1" },
    { ref: "patientA1b", entity_type: "patient", workspace_id: "site_a1", principal_id: "usr_patient_a1b" },
    { ref: "patientA2", entity_type: "patient", workspace_id: "site_a2", principal_id: "usr_patient_a2" },
    { ref: "slotA1", entity_type: "slot", workspace_id: "site_a1", principal_id: "usr_doctor_a1" },
    { ref: "slotA1b", entity_type: "slot", workspace_id: "site_a1", principal_id: "usr_doctor_a1" },
    { ref: "slotA1c", entity_type: "slot", workspace_id: "site_a1", principal_id: "usr_doctor_a1" },
    { ref: "slotA2", entity_type: "slot", workspace_id: "site_a2" },
    { ref: "apptA1", entity_type: "appointment", workspace_id: "site_a1", principal_id: "usr_patient_a1" },
    { ref: "apptA2", entity_type: "appointment", workspace_id: "site_a2" },
  ]);
  add("tnt_b", [
    { ref: "slotB1", entity_type: "slot", workspace_id: "site_b1" },
    { ref: "patientB1", entity_type: "patient", workspace_id: "site_b1" },
  ]);

  return {
    app,
    clock,
    who,
    records,
    identity: (name) => {
      const w = who[name];
      if (w === undefined) {
        throw new Error(`unknown test principal ${name}`);
      }
      return {
        application_id: APPLICATION_ID,
        tenant_id: w.tenant,
        workspace_id: w.site,
        principal_id: w.principal,
        roles: [w.role],
        session_id: "ses_01J0000000000000000000000A",
      };
    },
    close: () => app.close(),
  };
};

export const idemKey = (): string => `idem_${randomBytes(12).toString("hex")}`;

const ulid = (): string => {
  const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
  const bytes = randomBytes(26);
  return [...bytes].map((b, i) => (i === 0 ? alphabet[b % 8] : alphabet[b % 32])).join("");
};

// What the core holds after Prepare: the PreparedAction it hands back at Commit.
// Built from the host's own prepare result, the way the coordinator builds it.
export const preparedActionFrom = (
  capabilityId: string,
  prepare: HostResult,
  options: { now: Date; ttlSeconds?: number; mode?: "none" | "explicit"; key?: string } = { now: new Date(START) },
): Record<string, unknown> => {
  const preparation = prepare.preparation;
  if (preparation === undefined) {
    throw new Error("the result carries no preparation");
  }
  const mode = options.mode ?? "explicit";
  const ttl = options.ttlSeconds ?? preparation.valid_for_seconds ?? 600;
  return {
    preparation_id: `prep_${ulid()}`,
    capability_id: capabilityId,
    state: "committing",
    summary: preparation.summary,
    effects: preparation.effects,
    risk_level: Math.max(preparation.declared_risk_level, mode === "explicit" ? 2 : 1),
    data_versions: preparation.data_versions,
    prepared_at: toTimestamp(options.now),
    expires_at: toTimestamp(new Date(options.now.getTime() + ttl * 1000)),
    invalidation_conditions: ["data_version_changed"],
    confirmation: {
      mode,
      bindings: mode === "none" ? [] : ["preparation_id", "principal", "session", "data_versions"],
    },
    idempotency_key: options.key ?? idemKey(),
  };
};
