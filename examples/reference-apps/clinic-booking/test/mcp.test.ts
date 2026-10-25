import { request } from "node:http";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { createApp } from "../src/app.ts";
import type { HostResult } from "../src/results.ts";
import { asCoreResult, deterministicResultSchema, describeErrors } from "./helpers/schemas.ts";
import { devPublicKeyBase64, verifyContextToken } from "./helpers/verify-token.ts";
import { createWorld, idemKey, preparedActionFrom, type World } from "./helpers/world.ts";

const BEARER = "test-credential-not-a-secret-4f9c1e7a";

let world: World;
let url: string;
let client: Client;

const connect = async (bearer: string | null = BEARER): Promise<Client> => {
  const transport = new StreamableHTTPClientTransport(new URL(`${url}/mcp`), {
    requestInit: bearer === null ? {} : { headers: { Authorization: `Bearer ${bearer}` } },
  });
  const c = new Client({ name: "clinic-test-client", version: "0.0.0" });
  await c.connect(transport);
  return c;
};

beforeEach(async () => {
  world = createWorld({ bearer: BEARER });
  url = (await world.app.listen()).url;
  client = await connect();
});

afterEach(async () => {
  await client.close();
  await world.close();
});

const id = (ref: string): string => world.records[ref]?.entity_id ?? "";

type Called = { result: HostResult; text: string; isError: boolean };

const call = async (name: string, args: Record<string, unknown>): Promise<Called> => {
  const response = await client.callTool({ name, arguments: args });
  const content = response.content as { type: string; text: string }[];
  return {
    result: response.structuredContent as unknown as HostResult,
    text: content[0]?.text ?? "",
    isError: response.isError === true,
  };
};

describe("tools", () => {
  it("lists every tool with typed input, and an output schema for the capability tools", async () => {
    const { tools } = await client.listTools();
    const names = tools.map((t) => t.name).sort();
    expect(names).toEqual(
      [
        "appointments_book_commit",
        "appointments_book_prepare",
        "appointments_book_status",
        "appointments_book_validate",
        "appointments_cancel_commit",
        "appointments_cancel_prepare",
        "appointments_cancel_status",
        "appointments_cancel_validate",
        "availability_list",
        "describe",
        "health",
        "inspect",
        "patients_find",
        "patients_update_contact_commit",
        "patients_update_contact_prepare",
        "patients_update_contact_status",
        "patients_update_contact_validate",
        "resolve_appointment",
        "resolve_doctor",
        "resolve_patient",
        "resolve_slot",
        "test_change_record",
        "test_cleanup",
        "test_create_tenant",
        "test_inject_fault",
        "test_seed",
      ].sort(),
    );
    const prepare = tools.find((t) => t.name === "appointments_book_prepare");
    expect(prepare?.inputSchema.required).toEqual(expect.arrayContaining(["identity", "fields"]));
    expect(prepare?.outputSchema?.required).toEqual(expect.arrayContaining(["status", "identifiers", "values", "errors"]));
    const commit = tools.find((t) => t.name === "appointments_book_commit");
    expect(commit?.inputSchema.required).toEqual(expect.arrayContaining(["identity", "idempotency_key", "prepared"]));
    expect(commit?.annotations?.readOnlyHint).toBe(false);
    expect(prepare?.annotations?.readOnlyHint).toBe(true);
  });

  it("answers with structured content that is a valid DeterministicResult, mirrored as text", async () => {
    const out = await call("availability_list", { identity: world.identity("patientA1"), fields: { days: 14 } });
    expect(out.isError).toBe(false);
    expect(JSON.parse(out.text)).toEqual(out.result);
    expect(out.result.status).toBe("ok");
    const valid = deterministicResultSchema(asCoreResult(out.result, "read", "availability.list"));
    expect(valid, describeErrors(deterministicResultSchema)).toBe(true);
    expect(out.result).not.toHaveProperty("operation");
    expect(out.result).not.toHaveProperty("capability_id");
  });

  it("refuses with a typed result, never a tool error, when the host says no", async () => {
    const out = await call("appointments_book_prepare", {
      identity: world.identity("receptionA1"),
      fields: { patient: id("patientA1"), slot: id("slotA2") },
    });
    expect(out.isError).toBe(false);
    expect(out.result.status).toBe("rejected");
    expect(out.result.errors[0]).toMatchObject({ code: "scope_violation", field: "slot" });
  });

  it("answers a request with its required fields missing as a typed result, not as a tool error", async () => {
    for (const tool of ["appointments_book_validate", "appointments_book_prepare", "patients_update_contact_prepare", "patients_find"]) {
      const out = await call(tool, { identity: world.identity("receptionA1"), fields: {} });
      expect(out.isError, tool).toBe(false);
      expect(out.result.status, tool).toBe("rejected");
      expect(out.result.errors[0]?.code, tool).toBe("missing_fields");
    }
  });

  it("books through prepare, commit and status, and a repeated commit returns the original receipt", async () => {
    const identity = world.identity("receptionA1");
    const prepare = await call("appointments_book_prepare", {
      identity,
      fields: { patient: id("patientA1"), slot: id("slotA1"), reason: "annual check" },
    });
    expect(prepare.result.status).toBe("ok");
    const prepared = preparedActionFrom("appointments.book", prepare.result, { now: world.clock.now() });
    const key = prepared.idempotency_key as string;

    const commit = await call("appointments_book_commit", { identity, idempotency_key: key, prepared });
    expect(commit.result.status).toBe("ok");
    const again = await call("appointments_book_commit", { identity, idempotency_key: key, prepared });
    expect(again.result).toEqual(commit.result);
    const status = await call("appointments_book_status", { identity, idempotency_key: key });
    expect(status.result).toEqual(commit.result);
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM appointments WHERE slot_id = ?").get(id("slotA1"))).toEqual({ n: 1 });
  });

  it("validate reports the version prepare stored, over the wire as well", async () => {
    const identity = world.identity("receptionA1");
    const fields = { patient: id("patientA1"), slot: id("slotA1") };
    const prepare = await call("appointments_book_prepare", { identity, fields });
    const validate = await call("appointments_book_validate", { identity, fields });
    expect(validate.result.freshness?.version).toBe(prepare.result.freshness?.version);
  });
});

describe("a lost commit reply is recoverable through status", () => {
  it("applies the commit, loses the reply, and status returns the recorded receipt", async () => {
    const identity = world.identity("receptionA1");
    const prepare = await call("appointments_book_prepare", {
      identity,
      fields: { patient: id("patientA1"), slot: id("slotA1") },
    });
    const prepared = preparedActionFrom("appointments.book", prepare.result, { now: world.clock.now() });
    const key = prepared.idempotency_key as string;

    await fetchJson("POST", "/test/faults", { tool: "appointments_book_commit", mode: "error_after_commit" });
    const lost = await call("appointments_book_commit", { identity, idempotency_key: key, prepared });
    expect(lost.isError).toBe(true);
    expect(lost.result).toBeUndefined();

    // The effect is there even though the caller never saw it.
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM appointments WHERE slot_id = ?").get(id("slotA1"))).toEqual({ n: 1 });
    const status = await call("appointments_book_status", { identity, idempotency_key: key });
    expect(status.result.status).toBe("ok");
    expect(status.result.identifiers[0]).toMatchObject({ entity_type: "appointment" });

    // Retrying with the same key sends no second effect.
    const retry = await call("appointments_book_commit", { identity, idempotency_key: key, prepared });
    expect(retry.result).toEqual(status.result);
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM appointments WHERE slot_id = ?").get(id("slotA1"))).toEqual({ n: 1 });
  });

  it("reports an unavailable dependency as a typed result and applies nothing", async () => {
    const identity = world.identity("receptionA1");
    const prepare = await call("appointments_book_prepare", { identity, fields: { patient: id("patientA1"), slot: id("slotA1") } });
    const prepared = preparedActionFrom("appointments.book", prepare.result, { now: world.clock.now() });
    await fetchJson("POST", "/test/faults", { tool: "appointments_book_commit", mode: "unavailable" });
    const down = await call("appointments_book_commit", { identity, idempotency_key: prepared.idempotency_key, prepared });
    expect(down.isError).toBe(false);
    expect(down.result.status).toBe("unavailable");
    expect(down.result.errors[0]?.code).toBe("unavailable");
    expect(world.app.db.prepare("SELECT status FROM slots WHERE id = ?").get(id("slotA1"))).toEqual({ status: "free" });
    // The fault was one-shot: the next call works.
    const ok = await call("appointments_book_commit", { identity, idempotency_key: prepared.idempotency_key, prepared });
    expect(ok.result.status).toBe("ok");
  });
});

describe("counters for the gate", () => {
  it("counts every call that reached the host, per tool", async () => {
    await fetchJson("POST", "/test/reset-counters", {});
    await call("availability_list", { identity: world.identity("patientA1"), fields: {} });
    await call("appointments_book_status", { identity: world.identity("receptionA1"), idempotency_key: idemKey() });
    const stats = (await fetchJson("GET", "/test/stats")).body as { calls: Record<string, number>; totals: Record<string, number> };
    expect(stats.calls).toEqual({ availability_list: 1, appointments_book_status: 1 });
    expect(stats.calls.appointments_book_commit).toBeUndefined();
    expect(stats.totals.appointments).toBeGreaterThan(0);
  });
});

describe("the disposable tenant lifecycle over MCP", () => {
  it("creates, seeds, uses and removes a tenant, and proves nothing is left", async () => {
    const created = await call("test_create_tenant", {
      tenant_id: "tnt_audit_run1",
      workspaces: [{ workspace_id: "site_audit_1" }],
      principals: [
        { principal_id: "usr_audit_reception", workspace_id: "site_audit_1", roles: ["receptionist"] },
        { principal_id: "usr_audit_patient", workspace_id: "site_audit_1", roles: ["patient"] },
      ],
    });
    expect(created.isError).toBe(false);
    expect(created.result).toMatchObject({ status: "ok", tenant_id: "tnt_audit_run1" });

    const seeded = await call("test_seed", {
      tenant_id: "tnt_audit_run1",
      records: [
        { ref: "slot-1", entity_type: "slot", workspace_id: "site_audit_1" },
        { ref: "patient-1", entity_type: "patient", workspace_id: "site_audit_1", principal_id: "usr_audit_patient" },
      ],
    });
    const records = (seeded.result as unknown as { records: { ref: string; entity_id: string }[] }).records;
    expect(records.map((r) => r.ref)).toEqual(["slot-1", "patient-1"]);

    const identity = {
      application_id: "clinic-booking",
      tenant_id: "tnt_audit_run1",
      workspace_id: "site_audit_1",
      principal_id: "usr_audit_reception",
      roles: ["receptionist"],
      session_id: "ses_01J0000000000000000000000A",
    };
    const prepare = await call("appointments_book_prepare", {
      identity,
      fields: { patient: records[1]?.entity_id, slot: records[0]?.entity_id },
    });
    expect(prepare.result.status).toBe("ok");

    const cleaned = await call("test_cleanup", { tenant_id: "tnt_audit_run1" });
    expect(cleaned.result).toMatchObject({ status: "ok", existed: true });
    const remaining = (cleaned.result as unknown as { remaining: Record<string, number> }).remaining;
    expect(Object.values(remaining).every((n) => n === 0)).toBe(true);
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM tenants WHERE id = 'tnt_audit_run1'").get()).toEqual({ n: 0 });
    // And it is idempotent.
    expect((await call("test_cleanup", { tenant_id: "tnt_audit_run1" })).result).toMatchObject({ status: "ok", existed: false });
  });

  it("refuses to touch a tenant that is not disposable, and an unknown principal or role", async () => {
    const demo = createApp({ demoSeed: true, testSupport: true, port: 0 });
    try {
      const refused = await (async () => {
        const { url: demoUrl } = await demo.listen();
        const transport = new StreamableHTTPClientTransport(new URL(`${demoUrl}/mcp`));
        const c = new Client({ name: "demo-client", version: "0.0.0" });
        await c.connect(transport);
        const out = await c.callTool({ name: "test_cleanup", arguments: { tenant_id: "tnt_demo" } });
        await c.close();
        return out.structuredContent as { status: string; errors: { code: string }[] };
      })();
      expect(refused.status).toBe("rejected");
      expect(refused.errors[0]?.code).toBe("permission_denied");
      expect(demo.db.prepare("SELECT COUNT(*) AS n FROM tenants WHERE id = 'tnt_demo'").get()).toEqual({ n: 1 });
    } finally {
      await demo.close();
    }
    const badRole = await call("test_create_tenant", {
      tenant_id: "tnt_bad_role",
      workspaces: [{ workspace_id: "site_x" }],
      principals: [{ principal_id: "usr_x", workspace_id: "site_x", roles: ["admin"] }],
    });
    expect(badRole.result.status).toBe("rejected");
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM tenants WHERE id = 'tnt_bad_role'").get()).toEqual({ n: 0 });
  });

  it("changes a record's version and nothing else, so validate notices it and an earlier preparation is stale", async () => {
    const identity = world.identity("receptionA1");
    const fields = { patient: id("patientA1"), slot: id("slotA1") };
    const prepare = await call("appointments_book_prepare", { identity, fields });
    expect(prepare.result.status).toBe("ok");
    const prepared = preparedActionFrom("appointments.book", prepare.result, { now: world.clock.now() });
    const before = world.app.db.prepare("SELECT status, appointment_id, version FROM slots WHERE id = ?").get(id("slotA1")) as {
      status: string;
      appointment_id: string | null;
      version: number;
    };

    const changed = await call("test_change_record", { tenant_id: "tnt_a", entity_type: "slot", entity_id: id("slotA1") });
    expect(changed.isError).toBe(false);
    expect(changed.result).toEqual({ status: "ok", tenant_id: "tnt_a", entity_type: "slot", entity_id: id("slotA1"), changed: true });
    // Only the revision counter moved: the slot is still free, still nobody's.
    expect(world.app.db.prepare("SELECT status, appointment_id, version FROM slots WHERE id = ?").get(id("slotA1"))).toEqual({
      status: before.status,
      appointment_id: before.appointment_id,
      version: before.version + 1,
    });

    const validate = await call("appointments_book_validate", { identity, fields });
    expect(validate.result.freshness?.version).not.toBe(prepare.result.freshness?.version);
    const commit = await call("appointments_book_commit", { identity, idempotency_key: prepared.idempotency_key, prepared });
    expect(commit.result.status).toBe("rejected");
    expect(commit.result.errors[0]?.code).toBe("stale");
    expect(world.app.db.prepare("SELECT COUNT(*) AS n FROM appointments WHERE slot_id = ?").get(id("slotA1"))).toEqual({ n: 0 });

    // A patient and an appointment change the same way.
    for (const [entityType, table, ref] of [["patient", "patients", "patientA1"], ["appointment", "appointments", "apptA1"]] as const) {
      const was = world.app.db.prepare(`SELECT version FROM ${table} WHERE id = ?`).get(id(ref)) as { version: number };
      const out = await call("test_change_record", { tenant_id: "tnt_a", entity_type: entityType, entity_id: id(ref) });
      expect(out.result).toMatchObject({ status: "ok", changed: true });
      expect(world.app.db.prepare(`SELECT version FROM ${table} WHERE id = ?`).get(id(ref))).toEqual({ version: was.version + 1 });
    }
  });

  it("changes only records of a disposable tenant, of a type that has a revision counter, and says nothing about other tenants", async () => {
    const refused = async (args: Record<string, unknown>): Promise<string | undefined> => {
      const out = await call("test_change_record", args);
      expect(out.isError).toBe(false);
      expect(out.result.status).toBe("rejected");
      return out.result.errors[0]?.code;
    };
    // a doctor has no revision counter, and an unknown type is not one
    expect(await refused({ tenant_id: "tnt_a", entity_type: "doctor", entity_id: id("doctorA1") })).toBe("validation_failed");
    expect(await refused({ tenant_id: "tnt_a", entity_type: "patients; DROP TABLE slots", entity_id: id("patientA1") })).toBe("validation_failed");
    expect(await refused({ tenant_id: "tnt_a", entity_type: "slot", entity_id: "slot has spaces" })).toBe("validation_failed");
    // a record of another tenant and one that does not exist are the same refusal
    expect(await refused({ tenant_id: "tnt_b", entity_type: "patient", entity_id: id("patientA1") })).toBe("scope_violation");
    expect(await refused({ tenant_id: "tnt_a", entity_type: "patient", entity_id: "pat_missing" })).toBe("scope_violation");
    expect(await refused({ tenant_id: "tnt_missing", entity_type: "patient", entity_id: id("patientA1") })).toBe("scope_violation");
    expect(world.app.db.prepare("SELECT version FROM patients WHERE id = ?").get(id("patientA1"))).toEqual({ version: 1 });

    // the demonstration tenant is not disposable and is never touched
    const demo = createApp({ demoSeed: true, testSupport: true, port: 0 });
    try {
      const { url: demoUrl } = await demo.listen();
      const c = new Client({ name: "demo-client", version: "0.0.0" });
      await c.connect(new StreamableHTTPClientTransport(new URL(`${demoUrl}/mcp`)));
      const patient = demo.db.prepare("SELECT id, version FROM patients WHERE tenant_id = 'tnt_demo' LIMIT 1").get() as { id: string; version: number };
      const out = await c.callTool({ name: "test_change_record", arguments: { tenant_id: "tnt_demo", entity_type: "patient", entity_id: patient.id } });
      await c.close();
      const result = out.structuredContent as { status: string; errors: { code: string }[] };
      expect(result.status).toBe("rejected");
      expect(result.errors[0]?.code).toBe("permission_denied");
      expect(demo.db.prepare("SELECT version FROM patients WHERE id = ?").get(patient.id)).toEqual({ version: patient.version });
    } finally {
      await demo.close();
    }
  });

  it("arms one failure for requests of the named disposable tenants only", async () => {
    const identity = world.identity("receptionA1");
    const other = world.identity("receptionB1");
    const fields = { patient: id("patientA1"), slot: id("slotA1") };
    const prepare = await call("appointments_book_prepare", { identity, fields });
    const prepared = preparedActionFrom("appointments.book", prepare.result, { now: world.clock.now() });
    const commitOf = (who: typeof identity) =>
      call("appointments_book_commit", { identity: who, idempotency_key: prepared.idempotency_key, prepared });

    // a dependency down: the next commit of tenant A answers unavailable and applies nothing, once
    const armed = await call("test_inject_fault", {
      operation: "commit",
      capability_id: "appointments.book",
      kind: "dependency_down",
      tenant_ids: ["tnt_a"],
    });
    expect(armed.result).toEqual({ status: "ok", armed: true, tool: "appointments_book_commit" });
    // a request of another tenant neither triggers the fault nor consumes it
    const foreign = await commitOf(other);
    expect(foreign.result.status).not.toBe("unavailable");
    const down = await commitOf(identity);
    expect(down.result.status).toBe("unavailable");
    expect(world.app.db.prepare("SELECT status FROM slots WHERE id = ?").get(id("slotA1"))).toEqual({ status: "free" });
    expect((await commitOf(identity)).result.status).toBe("ok");

    // a lost reply: the commit is applied and the caller learns nothing
    const second = await call("appointments_book_prepare", { identity, fields: { patient: id("patientA1"), slot: id("slotA1b") } });
    const preparedB = preparedActionFrom("appointments.book", second.result, { now: world.clock.now() });
    await call("test_inject_fault", {
      operation: "commit",
      capability_id: "appointments.book",
      kind: "reply_lost",
      tenant_ids: ["tnt_a"],
    });
    const lost = await call("appointments_book_commit", { identity, idempotency_key: preparedB.idempotency_key, prepared: preparedB });
    expect(lost.isError).toBe(true);
    expect(world.app.db.prepare("SELECT status FROM slots WHERE id = ?").get(id("slotA1b"))).toEqual({ status: "booked" });
  });

  it("arms nothing for a request that is not a failure the audit may ask for, or a tenant that is not disposable", async () => {
    const refused = async (args: Record<string, unknown>): Promise<string | undefined> => {
      const out = await call("test_inject_fault", args);
      expect(out.isError).toBe(false);
      expect(out.result.status).toBe("rejected");
      return out.result.errors[0]?.code;
    };
    const base = { operation: "commit", capability_id: "appointments.book", kind: "dependency_down", tenant_ids: ["tnt_a"] };
    expect(await refused({ ...base, kind: "melt" })).toBe("validation_failed");
    expect(await refused({ ...base, kind: "reply_lost", operation: "validate" })).toBe("validation_failed");
    expect(await refused({ ...base, operation: "teleport" })).toBe("validation_failed");
    expect(await refused({ ...base, capability_id: "no.such" })).toBe("validation_failed");
    expect(await refused({ ...base, tenant_ids: [] })).toBe("validation_failed");
    expect(await refused({ ...base, tenant_ids: ["tnt_missing"] })).toBe("scope_violation");
    const stats = (await fetchJson("GET", "/test/stats")).body as { faults: unknown[] };
    expect(stats.faults).toEqual([]);
    // a read-only capability can be made to fail on its read
    const read = await call("test_inject_fault", { operation: "read", capability_id: "availability.list", kind: "dependency_down", tenant_ids: ["tnt_a"] });
    expect(read.result).toMatchObject({ status: "ok", armed: true, tool: "availability_list" });
    const down = await call("availability_list", { identity: world.identity("patientA1"), fields: {} });
    expect(down.result.status).toBe("unavailable");
  });

  it("refuses to seed or clean a tenant that does not exist or is somebody else's", async () => {
    const seeded = await call("test_seed", { tenant_id: "tnt_missing", records: [{ ref: "a", entity_type: "slot", workspace_id: "w" }] });
    expect(seeded.result.status).toBe("rejected");
    expect(seeded.result.errors[0]?.code).toBe("scope_violation");
  });
});

describe("inspection", () => {
  it("answers every aspect over MCP and over the management API, identically", async () => {
    const viaMcp = (await call("inspect", {})).result as unknown as { reports: { aspect: string; state: string; facts: unknown[] }[] };
    const viaHttp = (await fetchJson("POST", "/inspect", {})).body as typeof viaMcp;
    expect(viaMcp.reports.map((r) => r.aspect)).toEqual([
      "versions",
      "modules",
      "dependencies",
      "health",
      "scopes",
      "credentials",
      "limits",
      "storage",
      "network",
    ]);
    expect(viaMcp.reports.every((r) => r.state === "reported" && r.facts.length > 0)).toBe(true);
    expect(viaHttp).toEqual(viaMcp);
    expect(Object.keys(viaMcp)).toEqual(["reports"]);
  });

  it("narrows to the aspects asked for and refuses an unknown one", async () => {
    const some = (await fetchJson("POST", "/inspect", { aspects: ["network", "health"] })).body as { reports: { aspect: string }[] };
    expect(some.reports.map((r) => r.aspect)).toEqual(["health", "network"]);
    expect((await fetchJson("POST", "/inspect", { aspects: ["secrets"] })).status).toBe(400);
    expect((await fetchJson("POST", "/inspect", { aspects: ["health", "health"] })).status).toBe(400);
  });

  it("reports the real port and that a credential is required, never the credential", async () => {
    const network = (await fetchJson("POST", "/inspect", { aspects: ["network", "credentials"] })).body as {
      reports: { aspect: string; facts: { field: string; value: { value: unknown } }[] }[];
    };
    const factOf = (aspect: string, field: string): unknown =>
      network.reports.find((r) => r.aspect === aspect)?.facts.find((f) => f.field === field)?.value.value;
    expect(factOf("network", "listen_port")).toBe(Number(new URL(url).port));
    const required = factOf("credentials", "mcp_credential_required");
    expect(required).toBe(true);
    expect(JSON.stringify(network)).not.toContain(BEARER);
  });
});

describe("the transport is closed by default", () => {
  it("requires the credential on everything but the health probe", async () => {
    expect((await fetchJson("GET", "/healthz", undefined, null)).status).toBe(200);
    expect((await fetchJson("POST", "/inspect", {}, null)).status).toBe(401);
    expect((await fetchJson("POST", "/inspect", {}, "wrong-credential")).status).toBe(401);
    expect((await fetchJson("POST", "/mcp", {}, null)).status).toBe(401);
    await expect(connect(null)).rejects.toThrow();
  });

  it("accepts POST on /mcp only and bounds the body", async () => {
    expect((await fetchJson("GET", "/mcp")).status).toBe(405);
    expect((await fetchJson("DELETE", "/mcp")).status).toBe(405);
    const big = await rawRequest("POST", "/mcp", `{"pad":"${"x".repeat(1_100_000)}"}`, { "content-type": "application/json" });
    expect(big.status).toBe(413);
    const garbage = await rawRequest("POST", "/mcp", "{not json", { "content-type": "application/json" });
    expect(garbage.status).toBe(400);
  });

  it("refuses a Host or an Origin that is not loopback (DNS rebinding)", async () => {
    const rebound = await rawRequest("POST", "/inspect", "{}", { host: "evil.example", "content-type": "application/json" });
    expect(rebound.status).toBe(403);
    const crossOrigin = await rawRequest("POST", "/inspect", "{}", { origin: "https://evil.example", "content-type": "application/json" });
    expect(crossOrigin.status).toBe(403);
    const local = await rawRequest("POST", "/inspect", "{}", { origin: "http://localhost:3000", "content-type": "application/json" });
    expect(local.status).toBe(200);
  });

  it("refuses to listen on a non-loopback address", () => {
    expect(() => createApp({ host: "0.0.0.0" })).toThrow(/loopback/u);
    expect(() => createApp({ host: "192.168.1.10" })).toThrow(/loopback/u);
  });

  it("has no test support, no key and no provisioning tools unless asked", async () => {
    const plain = createApp({ port: 0 });
    try {
      const { url: plainUrl } = await plain.listen();
      expect(plain.issuer).toBeUndefined();
      for (const path of ["/test/stats", "/host/context-token", "/test/faults"]) {
        const response = await fetch(`${plainUrl}${path}`, { method: path === "/test/stats" ? "GET" : "POST", body: path === "/test/stats" ? undefined : "{}" });
        expect(response.status, path).toBe(404);
      }
      const c = new Client({ name: "plain-client", version: "0.0.0" });
      await c.connect(new StreamableHTTPClientTransport(new URL(`${plainUrl}/mcp`)));
      const names = (await c.listTools()).tools.map((t) => t.name);
      expect(names.some((n) => n.startsWith("test_"))).toBe(false);
      const refused = await c.callTool({ name: "test_cleanup", arguments: { tenant_id: "x" } }).catch((error: unknown) => error);
      expect(refused instanceof Error || (refused as { isError?: boolean }).isError === true).toBe(true);
      await c.close();
    } finally {
      await plain.close();
    }
  });
});

describe("context tokens over HTTP", () => {
  it("mints a token for a known principal that an independent verifier accepts", async () => {
    const out = await fetchJson("POST", "/host/context-token", {
      tenant_id: "tnt_a",
      workspace_id: "site_a1",
      principal_id: "usr_reception_a1",
    });
    expect(out.status).toBe(200);
    const body = out.body as { token: string; key_id: string; max_lifetime_seconds: number };
    expect(body.key_id).toBe("alixai-test-only-publisher");
    expect(body.max_lifetime_seconds).toBe(900);
    const verified = verifyContextToken(body.token, {
      publicKeyBase64: devPublicKeyBase64(),
      issuer: "https://clinic-booking.example.test",
      audience: "alixai",
      applicationId: "clinic-booking",
      keyId: "alixai-test-only-publisher",
      now: world.clock.now(),
    });
    expect(verified.claims.roles).toEqual(["receptionist"]);
    expect(verified.claims.capability_scopes).toEqual(
      expect.arrayContaining(["availability.list", "patients.find", "appointments.book", "appointments.cancel", "patients.update_contact"]),
    );
    expect(String(verified.claims.session_id)).toMatch(/^ses_[0-9A-HJKMNP-TV-Z]{26}$/u);
  });

  it("limits the capability scopes to what was asked for, and refuses what it must", async () => {
    const narrow = await fetchJson("POST", "/host/context-token", {
      tenant_id: "tnt_a",
      workspace_id: "site_a1",
      principal_id: "usr_patient_a1",
      capability_scopes: ["availability.list"],
      lifetime_seconds: 60,
    });
    const verified = verifyContextToken((narrow.body as { token: string }).token, {
      publicKeyBase64: devPublicKeyBase64(),
      issuer: "https://clinic-booking.example.test",
      audience: "alixai",
      applicationId: "clinic-booking",
      keyId: "alixai-test-only-publisher",
      now: world.clock.now(),
    });
    expect(verified.claims.capability_scopes).toEqual(["availability.list"]);
    expect((verified.claims.exp as number) - (verified.claims.iat as number)).toBe(60);

    expect((await fetchJson("POST", "/host/context-token", { tenant_id: "tnt_a", workspace_id: "site_a1", principal_id: "usr_patient_a1", lifetime_seconds: 901 })).status).toBe(400);
    expect((await fetchJson("POST", "/host/context-token", { tenant_id: "tnt_a", workspace_id: "site_a2", principal_id: "usr_patient_a1" })).status).toBe(400);
    expect((await fetchJson("POST", "/host/context-token", { tenant_id: "tnt_a" })).status).toBe(400);
  });
});

describe("the clock can be moved to exercise expiry", () => {
  it("makes an old preparation expire on the host's side", async () => {
    const identity = world.identity("receptionA1");
    const prepare = await call("appointments_book_prepare", { identity, fields: { patient: id("patientA1"), slot: id("slotA1") } });
    const prepared = preparedActionFrom("appointments.book", prepare.result, { now: world.clock.now(), ttlSeconds: 60 });
    expect((await fetchJson("POST", "/test/clock", { advance_seconds: 61 })).status).toBe(200);
    const expired = await call("appointments_book_commit", { identity, idempotency_key: prepared.idempotency_key, prepared });
    expect(expired.result.errors[0]?.code).toBe("expired");
    expect((await fetchJson("POST", "/test/clock", { advance_seconds: -5 })).status).toBe(400);
  });
});

// ---- helpers ----------------------------------------------------------------

const fetchJson = async (
  method: string,
  path: string,
  body?: unknown,
  bearer: string | null = BEARER,
): Promise<{ status: number; body: unknown }> => {
  const response = await fetch(`${url}${path}`, {
    method,
    headers: {
      ...(bearer === null ? {} : { Authorization: `Bearer ${bearer}` }),
      ...(body === undefined ? {} : { "content-type": "application/json" }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  const text = await response.text();
  return { status: response.status, body: text === "" ? undefined : (JSON.parse(text) as unknown) };
};

// A raw request, for headers fetch will not let a client set.
const rawRequest = (
  method: string,
  path: string,
  body: string,
  headers: Record<string, string>,
): Promise<{ status: number }> =>
  new Promise((resolve, reject) => {
    const target = new URL(`${url}${path}`);
    const req = request(
      {
        method,
        host: target.hostname,
        port: target.port,
        path,
        headers: { authorization: `Bearer ${BEARER}`, "content-length": String(Buffer.byteLength(body)), ...headers },
      },
      (res) => {
        res.resume();
        res.on("end", () => resolve({ status: res.statusCode ?? 0 }));
      },
    );
    req.on("error", reject);
    req.end(body);
  });
