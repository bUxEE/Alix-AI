import { readFileSync } from "node:fs";
import { join } from "node:path";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { createApp, type ClinicApp } from "../src/app.ts";
import { APPLICATION_ID } from "../src/access.ts";
import { createMutableClock, type MutableClock } from "../src/clock.ts";
import type { HostResult } from "../src/results.ts";
import { databaseTotals } from "../src/testsupport.ts";
import { PACKAGE_DIR, capabilities, manifest, suite, type Capability } from "./helpers/package.ts";
import { idemKey, preparedActionFrom, START } from "./helpers/world.ts";

// The package's own test suite (tests/clinic-safety.yaml) is what the integration
// audit runs. Here it is run against the real application, over the real MCP
// transport, through the real provisioning tools: the disposable tenants are
// created and seeded from the suite's fixtures, every scope and safe-execution
// probe the package declares is performed, and the tenants are removed and
// proven gone. A probe the package declares but the application fails is a bug
// in one of the two.

const BEARER = "test-credential-not-a-secret-4f9c1e7a";

type SeedRecord = {
  ref: string;
  entity_type: string;
  workspace: string;
  principal?: string;
  given_name?: string;
  family_name?: string;
  name?: string;
  reference?: string;
};
type Seed = { records: SeedRecord[] };
const seedData = JSON.parse(readFileSync(join(PACKAGE_DIR, suite.fixtures.provisioning.seed_data), "utf8")) as Seed;

type Run = {
  app: ClinicApp;
  clock: MutableClock;
  client: Client;
  suffix: string;
  // logical references to real identifiers
  tenant: (ref: string) => string;
  workspace: (ref: string) => string;
  principal: (ref: string) => string;
  entity: (ref: string) => string;
  identity: (principalRef: string) => Record<string, unknown>;
  call: (tool: string, args: Record<string, unknown>) => Promise<{ result: HostResult; isError: boolean }>;
  tenants: string[];
};

const slug = (value: string): string => value.replaceAll("-", "_");

const provision = async (app: ClinicApp, clock: MutableClock, client: Client, suffix: string): Promise<Run> => {
  const call = async (tool: string, args: Record<string, unknown>) => {
    const out = await client.callTool({ name: tool, arguments: args });
    return { result: out.structuredContent as unknown as HostResult, isError: out.isError === true };
  };
  const tenant = (ref: string): string => `tnt_${slug(ref)}_${suffix}`;
  const workspace = (ref: string): string => `site_${slug(ref)}_${suffix}`;
  const principal = (ref: string): string => `usr_${slug(ref)}_${suffix}`;
  const { provisioning } = suite.fixtures;
  const entities = new Map<string, string>();

  for (const t of suite.fixtures.tenants) {
    const created = await call(provisioning.create_tenant.mcp_tool, {
      tenant_id: tenant(t.ref),
      workspaces: t.workspaces.map((w) => ({ workspace_id: workspace(w) })),
      principals: suite.fixtures.principals
        .filter((p) => p.tenant === t.ref)
        .map((p) => ({ principal_id: principal(p.ref), workspace_id: workspace(p.workspace), roles: p.roles })),
    });
    expect(created.result, JSON.stringify(created.result)).toMatchObject({ status: "ok", tenant_id: tenant(t.ref) });
    // The records of this tenant, from the package's own seed data.
    const records = seedData.records
      .filter((r) => t.workspaces.includes(r.workspace))
      .map((r) => ({
        ref: r.ref,
        entity_type: r.entity_type,
        workspace_id: workspace(r.workspace),
        ...(r.principal === undefined ? {} : { principal_id: principal(r.principal) }),
        ...(r.given_name === undefined ? {} : { given_name: r.given_name }),
        ...(r.family_name === undefined ? {} : { family_name: r.family_name }),
        ...(r.name === undefined ? {} : { name: r.name }),
        ...(r.reference === undefined ? {} : { reference: r.reference }),
      }));
    const seeded = await call(provisioning.seed.mcp_tool, { tenant_id: tenant(t.ref), records });
    expect(seeded.result, JSON.stringify(seeded.result)).toMatchObject({ status: "ok" });
    for (const r of (seeded.result as unknown as { records: { ref: string; entity_id: string }[] }).records) {
      entities.set(r.ref, r.entity_id);
    }
  }

  return {
    app,
    clock,
    client,
    suffix,
    tenant,
    workspace,
    principal,
    entity: (ref) => {
      const found = entities.get(ref);
      if (found === undefined) throw new Error(`no seeded record ${ref}`);
      return found;
    },
    identity: (ref) => {
      const p = suite.fixtures.principals.find((x) => x.ref === ref);
      if (p === undefined) throw new Error(`no fixture principal ${ref}`);
      return {
        application_id: APPLICATION_ID,
        tenant_id: tenant(p.tenant),
        workspace_id: workspace(p.workspace),
        principal_id: principal(ref),
        roles: p.roles,
        session_id: "ses_01J0000000000000000000000A",
      };
    },
    call,
    tenants: suite.fixtures.tenants.map((t) => tenant(t.ref)),
  };
};

let app: ClinicApp;
let clock: MutableClock;
let client: Client;
let counter = 0;

beforeEach(async () => {
  clock = createMutableClock(START);
  app = createApp({ clock, testSupport: true, mcpBearer: BEARER });
  const { url } = await app.listen();
  client = new Client({ name: "package-probe-runner", version: "0.0.0" });
  await client.connect(
    new StreamableHTTPClientTransport(new URL(`${url}/mcp`), {
      requestInit: { headers: { Authorization: `Bearer ${BEARER}` } },
    }),
  );
});

afterEach(async () => {
  await client.close();
  await app.close();
});

const fresh = (): Promise<Run> => provision(app, clock, client, `r${(counter += 1)}`);

describe("the package's fixtures and seed data are one description", () => {
  it("lists in the seed file exactly the records the suite declares, in the workspaces it declares", () => {
    const declared = suite.fixtures.records.map((r) => `${r.ref}:${r.entity_type}:${r.workspace}`).sort();
    const seeded = seedData.records.map((r) => `${r.ref}:${r.entity_type}:${r.workspace}`).sort();
    expect(seeded).toEqual(declared);
    for (const r of suite.fixtures.records) {
      const tenant = suite.fixtures.tenants.find((t) => t.workspaces.includes(r.workspace));
      expect(tenant?.ref, `record ${r.ref}`).toBe(r.tenant);
    }
    for (const r of seedData.records) {
      if (r.principal !== undefined) {
        const p = suite.fixtures.principals.find((x) => x.ref === r.principal);
        expect(p?.workspace, `record ${r.ref} is linked to a principal of its own workspace`).toBe(r.workspace);
      }
    }
  });
});

// What the package's conversations and smoke paths say, resolved against what the seed
// creates. Each entry names the words an utterance uses, the entity they stand for and the
// seed record that must answer to them; the utterance is checked to contain the words, so
// the table and the package cannot drift apart.
const MENTIONS: {
  source: { conversation: string; turn: number } | { smoke: string };
  capability: string;
  entityType: string;
  mention: string;
  expect: string[];
}[] = [
  { source: { conversation: "book-asks-for-the-slot", turn: 0 }, capability: "appointments.book", entityType: "patient", mention: "Maria Rossi", expect: ["patient-rec-a1"] },
  { source: { conversation: "book-asks-for-the-slot", turn: 1 }, capability: "appointments.book", entityType: "slot", mention: "next available", expect: ["slot-a1"] },
  { source: { conversation: "prenota-chiede-il-paziente", turn: 0 }, capability: "appointments.book", entityType: "slot", mention: "primo orario libero", expect: ["slot-a1"] },
  { source: { conversation: "cancel-by-reference", turn: 0 }, capability: "appointments.cancel", entityType: "appointment", mention: "APT-7K3Q9X", expect: ["appt-a1"] },
  { source: { smoke: "smoke-action" }, capability: "appointments.book", entityType: "patient", mention: "Maria Rossi", expect: ["patient-rec-a1"] },
  { source: { smoke: "smoke-action" }, capability: "appointments.book", entityType: "slot", mention: "next available", expect: ["slot-a1"] },
];

describe("the conversations and smoke paths of the package only name what the seed creates", () => {
  it("resolves every name, reference and phrase they use to exactly the seeded record, through the declared resolver", async () => {
    const run = await fresh();
    for (const m of MENTIONS) {
      const utterance =
        "conversation" in m.source
          ? suite.conversation_cases.find((c) => c.id === (m.source as { conversation: string }).conversation)?.turns[(m.source as { turn: number }).turn]?.user
          : suite.smoke.paths.find((p) => p.id === (m.source as { smoke: string }).smoke)?.utterance;
      const label = JSON.stringify(m.source);
      expect(utterance?.toLowerCase(), `${label} says "${m.mention}"`).toContain(m.mention.toLowerCase());
      const principalRef =
        "conversation" in m.source
          ? suite.conversation_cases.find((c) => c.id === (m.source as { conversation: string }).conversation)?.principal
          : suite.smoke.paths.find((p) => p.id === (m.source as { smoke: string }).smoke)?.principal;
      const cap = capabilities[m.capability] as Capability;
      const resolver = cap.entity_types[m.entityType]?.resolve;
      expect(resolver, `${m.capability} declares a resolver for ${m.entityType}`).toBeDefined();
      const out = await run.call(resolver?.mcp_tool ?? "", {
        identity: run.identity(principalRef ?? ""),
        fields: { entity_type: m.entityType, mention: m.mention },
      });
      expect(out.result.status, `${label}: ${JSON.stringify(out.result.errors)}`).toBe("ok");
      expect(
        out.result.identifiers.map((r) => r.entity_id),
        label,
      ).toEqual(m.expect.map((ref) => run.entity(ref)));
    }
  });

  it("has a resolved mention for every conversation and smoke path that has to get as far as a confirmation or a commit", () => {
    const covered = new Set(MENTIONS.map((m) => ("conversation" in m.source ? m.source.conversation : m.source.smoke)));
    for (const c of suite.conversation_cases) {
      if (c.turns.some((t) => t.expect.act === "request_confirmation")) {
        expect(covered.has(c.id), `conversation ${c.id} reaches a confirmation, so what it names must be resolved above`).toBe(true);
      }
    }
    for (const p of suite.smoke.paths) {
      if (p.path === "action") {
        expect(covered.has(p.id), `smoke path ${p.id} commits, so what it names must be resolved above`).toBe(true);
      }
    }
  });

  it("only uses dates the seed cannot make wrong: none of them is a fixed calendar date", () => {
    const texts = [
      ...suite.conversation_cases.flatMap((c) => c.turns.map((t) => t.user)),
      ...suite.smoke.paths.map((p) => p.utterance),
    ];
    for (const text of texts) {
      expect(text, "a fixed date in a conversation would be wrong on any other day").not.toMatch(/\b\d{4}-\d{2}-\d{2}\b/u);
    }
  });
});

describe("the disposable tenant of the suite", () => {
  it("is created, seeded and removed through the declared provisioning tools, and nothing is left behind", async () => {
    const before = databaseTotals(app.db);
    const run = await fresh();
    const during = databaseTotals(app.db);
    expect(during.tenants).toBe((before.tenants ?? 0) + suite.fixtures.tenants.length);
    expect(during.patients).toBeGreaterThan(before.patients ?? 0);
    for (const tenant of run.tenants) {
      const cleaned = await run.call(suite.fixtures.provisioning.cleanup.mcp_tool, { tenant_id: tenant });
      expect(cleaned.result).toMatchObject({ status: "ok", existed: true });
      expect(Object.values((cleaned.result as unknown as { remaining: Record<string, number> }).remaining).every((n) => n === 0)).toBe(true);
    }
    expect(databaseTotals(app.db)).toEqual(before);
  });

  it("can be created twice side by side with different identifiers, and each is isolated from the other", async () => {
    const a = await fresh();
    const b = await fresh();
    expect(a.tenant("group-a")).not.toBe(b.tenant("group-a"));
    const out = await a.call("patients_find", { identity: a.identity("reception-a1"), fields: { query: b.entity("patient-rec-a1") } });
    expect(out.result.status).toBe("rejected");
    expect(out.result.errors[0]?.code).toBe("scope_violation");
  });
});

// ---- scope probes ----------------------------------------------------------

// Which fields a probe sends: the target record fills the field of its own
// entity type; the other required fields default to a record of the acting
// principal's own workspace, so the target is the only thing that can be wrong.
const fieldsFor = (run: Run, cap: Capability, principalRef: string, targetRef: string | undefined): Record<string, unknown> => {
  const acting = suite.fixtures.principals.find((p) => p.ref === principalRef);
  const own = (entityType: string): string | undefined =>
    suite.fixtures.records.find((r) => r.entity_type === entityType && r.workspace === acting?.workspace)?.ref;
  const target = targetRef === undefined ? undefined : suite.fixtures.records.find((r) => r.ref === targetRef);
  const pick = (entityType: string): string | undefined => (target?.entity_type === entityType ? target.ref : own(entityType));
  switch (cap.id) {
    case "availability.list":
      return target?.entity_type === "doctor" ? { doctor: run.entity(target.ref) } : {};
    case "patients.find":
      return { query: run.entity(target?.ref ?? "patient-rec-a1") };
    case "appointments.book": {
      const patient = pick("patient");
      const slot = pick("slot");
      return { patient: run.entity(patient ?? ""), slot: run.entity(slot ?? "") };
    }
    case "appointments.cancel":
      return { appointment: run.entity(pick("appointment") ?? "") };
    default:
      return { patient: run.entity(pick("patient") ?? ""), phone: "+1 555 0150" };
  }
};

describe("the scope probes the package declares, performed against the application", () => {
  it("allows every probe that expects allow and refuses every probe that expects deny, with the declared code", async () => {
    const run = await fresh();
    let allowed = 0;
    let denied = 0;
    for (const probe of suite.scope_probes) {
      if (probe.operation === "retrieve") continue;
      const cap = capabilities[probe.capability_id ?? ""];
      expect(cap, `probe ${probe.id}: capability`).toBeDefined();
      const call = cap?.operations[probe.operation as "read" | "validate" | "prepare"];
      expect(call, `probe ${probe.id}: ${probe.capability_id} has no ${probe.operation}`).toBeDefined();
      const identity = run.identity(probe.principal);
      const fields = fieldsFor(run, cap as Capability, probe.principal, probe.target_record);
      const out = await run.call(call?.mcp_tool ?? "", { identity, fields });
      expect(out.isError, probe.id).toBe(false);
      if (probe.expect === "allow") {
        expect(out.result.status, `${probe.id}: ${JSON.stringify(out.result.errors)}`).toBe("ok");
        allowed += 1;
      } else {
        expect(out.result.status, probe.id).toBe("rejected");
        expect(out.result.errors[0]?.code, probe.id).toBe(probe.error_code);
        // nothing about the target is disclosed
        expect(out.result.identifiers, probe.id).toEqual([]);
        expect(out.result.values, probe.id).toEqual([]);
        denied += 1;
      }
    }
    expect(allowed).toBeGreaterThan(8);
    expect(denied).toBeGreaterThan(14);
    expect(allowed + denied).toBe(suite.scope_probes.filter((p) => p.operation !== "retrieve").length);
  });

  it("answers an out-of-scope record exactly like one that does not exist (nothing to enumerate)", async () => {
    const run = await fresh();
    for (const probe of suite.scope_probes) {
      if (probe.expect !== "deny" || probe.error_code !== "scope_violation" || probe.operation === "retrieve") continue;
      const cap = capabilities[probe.capability_id ?? ""] as Capability;
      const call = cap.operations[probe.operation as "read" | "validate" | "prepare"];
      const identity = run.identity(probe.principal);
      const real = fieldsFor(run, cap, probe.principal, probe.target_record);
      const target = suite.fixtures.records.find((r) => r.ref === probe.target_record);
      const realId = run.entity(probe.target_record ?? "");
      const unknownId = `${realId.split("_")[0]}_0000000000000000dead`;
      expect(unknownId).not.toBe(realId);
      const swapped = JSON.parse(JSON.stringify(real).replaceAll(realId, unknownId)) as Record<string, unknown>;
      const a = await run.call(call?.mcp_tool ?? "", { identity, fields: real });
      const b = await run.call(call?.mcp_tool ?? "", { identity, fields: swapped });
      expect(b.result.errors[0]?.code, `${probe.id} (${target?.entity_type}) unknown id`).toBe("scope_violation");
      expect(a.result.errors, `${probe.id}: out of scope vs unknown`).toEqual(b.result.errors);
    }
  });

  it("covers every capability and every role with at least one probe, in both directions where the role differs", () => {
    for (const id of Object.keys(capabilities)) {
      const probes = suite.scope_probes.filter((p) => p.capability_id === id);
      expect(probes.some((p) => p.expect === "allow"), `${id} has an allow probe`).toBe(true);
      expect(probes.some((p) => p.expect === "deny"), `${id} has a deny probe`).toBe(true);
    }
    for (const dimension of ["tenant", "workspace", "principal", "role", "record", "source"]) {
      expect(suite.scope_probes.some((p) => p.dimension === dimension), `a ${dimension} probe`).toBe(true);
    }
    const sources = Object.keys(manifest.rag_sources);
    for (const source of sources) {
      expect(suite.scope_probes.some((p) => p.operation === "retrieve" && p.target_source === source), `a retrieve probe for ${source}`).toBe(true);
    }
  });
});

// ---- safe-execution probes -------------------------------------------------

type Outcome = { outcome: string; error_code?: string };

// What the host sees for each kind, performed for real. Two kinds are decided by
// the AlixAI core before the host is ever called (a commit that was not
// confirmed, a commit after the preparation was withdrawn); the host's side of
// them is checked separately below.
const HOST_KINDS = ["confirmed_commit", "replayed_commit", "expired_preparation", "stale_record", "uncertain_commit_recovery"];

const primary = (capabilityId: string, run: Run, principalRef: string): { fields: Record<string, unknown>; competing: () => Record<string, unknown> } => {
  switch (capabilityId) {
    case "appointments.book":
      return {
        fields: { patient: run.entity("patient-rec-a1"), slot: run.entity("slot-a1") },
        competing: () => ({ patient: run.entity("patient-rec-a1b"), slot: run.entity("slot-a1") }),
      };
    case "appointments.cancel":
      return { fields: { appointment: run.entity("appt-a1") }, competing: () => ({ appointment: run.entity("appt-a1"), reason: "other" }) };
    default:
      void principalRef;
      return {
        fields: { patient: run.entity("patient-rec-a1"), phone: "+1 555 0150" },
        competing: () => ({ patient: run.entity("patient-rec-a1"), phone: "+1 555 0177" }),
      };
  }
};

// How many times the effect of a capability is in the database.
const effectCount = (run: Run, capabilityId: string): number => {
  const db = run.app.db;
  switch (capabilityId) {
    case "appointments.book":
      return Number(
        (db.prepare("SELECT COUNT(*) AS n FROM appointments WHERE slot_id = ? AND status = 'booked'").get(run.entity("slot-a1")) as { n: number }).n,
      );
    case "appointments.cancel":
      return Number(
        (db.prepare("SELECT COUNT(*) AS n FROM appointments WHERE id = ? AND status = 'cancelled'").get(run.entity("appt-a1")) as { n: number }).n,
      );
    default:
      return Number(
        (db.prepare("SELECT COUNT(*) AS n FROM patients WHERE id = ? AND phone = '+1 555 0150'").get(run.entity("patient-rec-a1")) as { n: number }).n,
      );
  }
};

const toolOf = (cap: Capability, op: "validate" | "prepare" | "commit" | "status"): string => cap.operations[op]?.mcp_tool ?? "";

const performKind = async (kind: string, cap: Capability, principalRef: string): Promise<{ actual: Outcome; run: Run }> => {
  const run = await fresh();
  const identity = run.identity(principalRef);
  const { fields, competing } = primary(cap.id, run, principalRef);
  const mode = cap.confirmation.mode === "none" ? "none" : "explicit";
  const prepare = await run.call(toolOf(cap, "prepare"), { identity, fields });
  expect(prepare.result.status, `${cap.id} prepare: ${JSON.stringify(prepare.result.errors)}`).toBe("ok");
  const prepared = preparedActionFrom(cap.id, prepare.result, { now: clock.now(), mode });
  const key = prepared.idempotency_key as string;
  const commit = (): ReturnType<Run["call"]> => run.call(toolOf(cap, "commit"), { identity, idempotency_key: key, prepared });
  const refusal = (r: HostResult): Outcome => ({ outcome: "rejected_no_effect", error_code: r.errors[0]?.code ?? "none" });

  switch (kind) {
    case "confirmed_commit": {
      const out = await commit();
      expect(out.result.status).toBe("ok");
      return { actual: { outcome: effectCount(run, cap.id) === 1 ? "committed_once" : "not_once" }, run };
    }
    case "replayed_commit": {
      const first = await commit();
      const second = await commit();
      expect(second.result).toEqual(first.result);
      return { actual: { outcome: effectCount(run, cap.id) === 1 ? "original_receipt_returned" : "applied_twice" }, run };
    }
    case "expired_preparation": {
      clock.advanceSeconds((cap.prepared_action?.ttl_seconds ?? 0) + 1);
      const out = await commit();
      expect(effectCount(run, cap.id)).toBe(0);
      return { actual: refusal(out.result), run };
    }
    case "stale_record": {
      // Something else changes the records the action relied on, after prepare and before commit.
      const other = await run.call(toolOf(cap, "prepare"), { identity, fields: competing() });
      expect(other.result.status, `${cap.id} competing prepare: ${JSON.stringify(other.result.errors)}`).toBe("ok");
      const otherAction = preparedActionFrom(cap.id, other.result, { now: clock.now(), mode });
      const applied = await run.call(toolOf(cap, "commit"), { identity, idempotency_key: otherAction.idempotency_key, prepared: otherAction });
      expect(applied.result.status, `${cap.id} competing commit: ${JSON.stringify(applied.result.errors)}`).toBe("ok");
      const before = effectCount(run, cap.id);
      const out = await commit();
      // the original action applied nothing of its own
      expect(effectCount(run, cap.id)).toBe(before);
      return { actual: refusal(out.result), run };
    }
    case "uncertain_commit_recovery": {
      // the commit is applied and its reply is lost
      app.instrumentation.injectFault(toolOf(cap, "commit"), "error_after_commit");
      const lost = await commit();
      expect(lost.isError).toBe(true);
      const status = await run.call(toolOf(cap, "status"), { identity, idempotency_key: key });
      expect(status.result.status).toBe("ok");
      // status established what happened, so the retry sends no second effect
      const retry = await commit();
      expect(retry.result).toEqual(status.result);
      return { actual: { outcome: effectCount(run, cap.id) === 1 ? "recovered_via_status" : "lost_or_duplicated" }, run };
    }
    default:
      throw new Error(`kind ${kind} is not performed by the host`);
  }
};

describe("the safe-execution probes the package declares, performed against the application", () => {
  for (const probe of suite.safety_probes) {
    const cap = capabilities[probe.capability_id] as Capability;
    if (!HOST_KINDS.includes(probe.kind)) continue;
    it(`${probe.id} (${probe.capability_id}, ${probe.kind})`, async () => {
      const { actual } = await performKind(probe.kind, cap, probe.principal);
      expect(actual).toEqual(probe.expect);
    });
  }

  it("covers all seven kinds for every consequential capability, with the principals and codes the capability allows", () => {
    const kinds = ["confirmed_commit", "unconfirmed_commit", "replayed_commit", "expired_preparation", "stale_record", "cancelled_preparation", "uncertain_commit_recovery"];
    for (const cap of Object.values(capabilities)) {
      const probes = suite.safety_probes.filter((p) => p.capability_id === cap.id);
      if (cap.risk_level === 0) {
        expect(probes, `${cap.id} is read-only and has nothing to commit`).toEqual([]);
        continue;
      }
      expect(probes.map((p) => p.kind).sort(), cap.id).toEqual([...kinds].sort());
      for (const probe of probes) {
        // The two kinds the core decides (a commit without its confirmation, a commit after the user declined)
        // are refused by the platform's coordinator before the host is called: their code is the coordinator's
        // (confirmation_required, conflict), not a failure state the capability declares.
        const coreDecided = ["unconfirmed_commit", "cancelled_preparation"].includes(probe.kind);
        if (probe.expect.error_code !== undefined && !coreDecided) {
          expect(cap.failure_states.map((f) => f.code), `${probe.id}: ${probe.expect.error_code} is a failure the capability declares`).toContain(
            probe.expect.error_code,
          );
        }
      }
    }
  });

  it("states for the two core-decided kinds what follows from the confirmation mode, and the host holds up its side", async () => {
    for (const cap of Object.values(capabilities)) {
      const unconfirmed = suite.safety_probes.find((p) => p.capability_id === cap.id && p.kind === "unconfirmed_commit");
      if (unconfirmed === undefined) continue;
      if (cap.confirmation.mode === "none") {
        // no confirmation is required, so committing right after prepare is the designed path
        expect(unconfirmed.expect, cap.id).toEqual({ outcome: "committed_once" });
        const { actual } = await performKind("confirmed_commit", cap, unconfirmed.principal);
        expect(actual).toEqual({ outcome: "committed_once" });
      } else {
        // the confirmation is the core's: without it the commit never reaches the host
        expect(unconfirmed.expect, cap.id).toEqual({ outcome: "rejected_no_effect", error_code: "confirmation_required" });
        expect(["explicit", "step_up"]).toContain(cap.confirmation.mode);
      }
    }
  });

  it("applies nothing when a preparation is never committed (a withdrawn preparation leaves no trace on the host)", async () => {
    for (const cap of Object.values(capabilities)) {
      const withdrawn = suite.safety_probes.find((p) => p.capability_id === cap.id && p.kind === "cancelled_preparation");
      if (withdrawn === undefined) continue;
      // The user declined the preparation, so the platform's coordinator refuses a later commit as a conflict
      // (the preparation was rejected: internal/actions errForState). It is not "expired": nothing timed out.
      expect(withdrawn.expect, cap.id).toEqual({ outcome: "rejected_no_effect", error_code: "conflict" });
      const run = await fresh();
      const before = databaseTotals(run.app.db);
      const { fields } = primary(cap.id, run, withdrawn.principal);
      const prepare = await run.call(toolOf(cap, "prepare"), { identity: run.identity(withdrawn.principal), fields });
      expect(prepare.result.status).toBe("ok");
      expect(effectCount(run, cap.id)).toBe(0);
      expect(databaseTotals(run.app.db)).toEqual(before);
    }
  });

  it("proves nothing was applied only through the one no-record answer, so a lost reply is never retried blindly", async () => {
    const cap = capabilities["appointments.book"] as Capability;
    const run = await fresh();
    const identity = run.identity("reception-a1");
    const { fields } = primary(cap.id, run, "reception-a1");
    const prepare = await run.call(toolOf(cap, "prepare"), { identity, fields });
    const prepared = preparedActionFrom(cap.id, prepare.result, { now: clock.now() });
    // the dependency was down: nothing happened, and status says so in exactly one shape
    app.instrumentation.injectFault(toolOf(cap, "commit"), "unavailable");
    const down = await run.call(toolOf(cap, "commit"), { identity, idempotency_key: prepared.idempotency_key, prepared });
    expect(down.result.status).toBe("unavailable");
    const status = await run.call(toolOf(cap, "status"), { identity, idempotency_key: prepared.idempotency_key });
    expect(status.result.status).toBe("rejected");
    expect(status.result.errors).toHaveLength(1);
    expect(status.result.errors[0]).toMatchObject({ code: "validation_failed", field: "idempotency_key" });
    expect(status.result.identifiers).toEqual([]);
    expect(effectCount(run, cap.id)).toBe(0);
    // and now the retry is safe, and it applies once
    const retry = await run.call(toolOf(cap, "commit"), { identity, idempotency_key: prepared.idempotency_key, prepared });
    expect(retry.result.status).toBe("ok");
    expect(effectCount(run, cap.id)).toBe(1);
    void idemKey;
  });
});
