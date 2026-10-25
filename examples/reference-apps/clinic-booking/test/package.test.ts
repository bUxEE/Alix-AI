import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import type { ValidateFunction } from "ajv/dist/2020.js";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { CAPABILITY_PERMISSION, ROLES, ROLE_PERMISSIONS, roleCapabilities, type CapabilityId } from "../src/access.ts";
import { MAX_LIFETIME_SECONDS } from "../src/context-token.ts";
import { BOOKING_TTL_SECONDS, CANCEL_TTL_SECONDS, CONTACT_TTL_SECONDS, IDEMPOTENCY_WINDOW_SECONDS } from "../src/domain/rules.ts";
import type { HostResult } from "../src/results.ts";
import { observeEverything, type Observed } from "./helpers/observe.ts";
import {
  PACKAGE_DIR,
  adapterConfig,
  capabilities,
  contract,
  docIndex,
  manifest,
  placeholders,
  policy,
  readText,
  resultSchema,
  suite,
  type Capability,
} from "./helpers/package.ts";
import { createAjv } from "./helpers/schemas.ts";
import { createWorld, idemKey, preparedActionFrom, type World } from "./helpers/world.ts";
import { verifyContextToken, devPublicKeyBase64 } from "./helpers/verify-token.ts";

// The package is a promise about the application. These tests hold the two to
// each other: what the package declares is what the running application does.

const BEARER = "test-credential-not-a-secret-4f9c1e7a";
const PLATFORM_CODES = ["scope_missing", "internal"];
// What a refusal can be that the host never produces itself: the adapter reports an
// uncertain outcome when a reply is lost.
const ADAPTER_ONLY_CODES = ["uncertain_outcome"];

let world: World;
let url: string;
let client: Client;

beforeEach(async () => {
  world = createWorld({ bearer: BEARER });
  url = (await world.app.listen()).url;
  client = new Client({ name: "package-test-client", version: "0.0.0" });
  await client.connect(
    new StreamableHTTPClientTransport(new URL(`${url}/mcp`), {
      requestInit: { headers: { Authorization: `Bearer ${BEARER}` } },
    }),
  );
});

afterEach(async () => {
  await client.close();
  await world.close();
});

const id = (ref: string): string => world.records[ref]?.entity_id ?? "";
const capabilityIds = Object.keys(capabilities).sort();
const operationNames = ["read", "validate", "prepare", "commit", "status"] as const;

const mappedTools = (): string[] => {
  const tools = new Set<string>();
  for (const cap of Object.values(capabilities)) {
    for (const op of Object.values(cap.operations)) {
      tools.add(op.mcp_tool);
    }
    for (const entity of Object.values(cap.entity_types)) {
      if (entity.resolve !== undefined) {
        tools.add(entity.resolve.mcp_tool);
      }
    }
  }
  for (const call of [suite.fixtures.provisioning.create_tenant, suite.fixtures.provisioning.seed, suite.fixtures.provisioning.cleanup]) {
    tools.add(call.mcp_tool);
  }
  // The audit's change-record and fault tools: approved by the adapter configuration, named by no capability
  // and by no suite (the package format declares none for them), so no model can reach them.
  tools.add("test_change_record");
  tools.add("test_inject_fault");
  return [...tools].sort();
};

describe("the control surface: package, adapter configuration and live MCP server agree", () => {
  it("maps only tools the server really offers, and the allow-list is exactly the mapped tools", async () => {
    const live = (await client.listTools()).tools.map((t) => t.name);
    const mapped = mappedTools();
    for (const tool of mapped) {
      expect(live, `${tool} is mapped by the package but the server does not offer it`).toContain(tool);
    }
    expect([...adapterConfig.tools.allow].sort()).toEqual(mapped);
    // What the server offers beyond the mapping is only the management surface the adapter itself uses.
    expect(live.filter((name) => !mapped.includes(name)).sort()).toEqual(["describe", "health", "inspect"]);
    expect(manifest.endpoints["clinic-mcp"]?.transport).toBe("mcp");
    expect(adapterConfig.server).toBe("clinic-mcp");
  });

  it("describes the same capabilities and operations the package declares", async () => {
    const out = await client.callTool({ name: "describe", arguments: {} });
    const described = (out.structuredContent as { capabilities: { capability_id: string; operations: string[] }[] }).capabilities;
    expect(described.map((c) => c.capability_id).sort()).toEqual(capabilityIds);
    for (const entry of described) {
      const declared = Object.keys(capabilities[entry.capability_id]?.operations ?? {}).sort();
      expect([...entry.operations].sort(), entry.capability_id).toEqual(declared);
    }
  });

  it("takes exactly the declared fields (and the reserved arguments for a resolver)", async () => {
    const { tools } = await client.listTools();
    const fieldKeys = (tool: string): string[] => {
      const found = tools.find((t) => t.name === tool);
      const fields = (found?.inputSchema.properties as Record<string, { properties?: Record<string, unknown> }> | undefined)?.fields;
      return Object.keys(fields?.properties ?? {}).sort();
    };
    for (const cap of Object.values(capabilities)) {
      const declared = cap.fields.map((f) => f.name).sort();
      for (const op of ["read", "validate", "prepare"] as const) {
        const call = cap.operations[op];
        if (call !== undefined) {
          expect(fieldKeys(call.mcp_tool), `${cap.id} ${op}`).toEqual(declared);
        }
      }
      for (const call of [cap.operations.commit, cap.operations.status]) {
        if (call !== undefined) {
          const input = tools.find((t) => t.name === call.mcp_tool)?.inputSchema.required ?? [];
          expect(input).toEqual(expect.arrayContaining(["identity", "idempotency_key"]));
        }
      }
      for (const entity of Object.values(cap.entity_types)) {
        if (entity.resolve !== undefined) {
          expect(fieldKeys(entity.resolve.mcp_tool), `${cap.id} resolver`).toEqual(["candidate_id", "entity_type", "mention"]);
        }
      }
    }
  });

  it("states the effect class, risk level and confirmation the schema couples, and the host agrees", () => {
    const s = world.app.service;
    const prepared: Record<string, HostResult> = {
      "appointments.book": s.book.prepare(world.identity("receptionA1"), { patient: id("patientA1"), slot: id("slotA1") }),
      "appointments.cancel": s.cancel.prepare(world.identity("receptionA1"), { appointment: id("apptA1") }),
      "patients.update_contact": s.contact.prepare(world.identity("patientA1"), { patient: id("patientA1"), phone: "+1 555 0150" }),
    };
    for (const cap of Object.values(capabilities)) {
      const floor = { none: 0, reversible: 1, external: 2, privileged: 3 }[cap.effect_class] ?? 99;
      expect(cap.risk_level, cap.id).toBeGreaterThanOrEqual(floor);
      const result = prepared[cap.id];
      if (cap.risk_level === 0) {
        expect(cap.operations.prepare, cap.id).toBeUndefined();
        expect(result, cap.id).toBeUndefined();
        continue;
      }
      expect(result?.status, cap.id).toBe("ok");
      const preparation = result?.preparation;
      expect(preparation?.declared_risk_level, cap.id).toBe(cap.risk_level);
      expect(preparation?.valid_for_seconds, cap.id).toBe(cap.prepared_action?.ttl_seconds);
      // An external effect class means some effect leaves the application; a reversible one means none does.
      const external = preparation?.effects.some((e) => e.external) ?? false;
      expect(external, `${cap.id} effect class ${cap.effect_class}`).toBe(cap.effect_class === "external");
    }
    expect(capabilities["appointments.book"]?.prepared_action?.ttl_seconds).toBe(BOOKING_TTL_SECONDS);
    expect(capabilities["appointments.cancel"]?.prepared_action?.ttl_seconds).toBe(CANCEL_TTL_SECONDS);
    expect(capabilities["patients.update_contact"]?.prepared_action?.ttl_seconds).toBe(CONTACT_TTL_SECONDS);
    for (const cap of Object.values(capabilities)) {
      if (cap.idempotency !== undefined) {
        expect(cap.idempotency.window_seconds, cap.id).toBe(IDEMPOTENCY_WINDOW_SECONDS);
        expect(cap.idempotency.on_replay, cap.id).toBe("return_original_receipt");
      }
    }
  });
});

describe("fields: required, optional and constrained exactly as declared", () => {
  type Call = (who: string, fields: Record<string, unknown>) => HostResult;
  const caller = (cap: string): Call => {
    const s = world.app.service;
    switch (cap) {
      case "availability.list":
        return (who, fields) => s.availabilityList(world.identity(who), fields);
      case "patients.find":
        return (who, fields) => s.patientsFind(world.identity(who), fields);
      case "appointments.book":
        return (who, fields) => s.book.prepare(world.identity(who), fields);
      case "appointments.cancel":
        return (who, fields) => s.cancel.prepare(world.identity(who), fields);
      default:
        return (who, fields) => s.contact.prepare(world.identity(who), fields);
    }
  };
  const baseline = (cap: string): Record<string, unknown> => {
    switch (cap) {
      case "availability.list":
        return {};
      case "patients.find":
        return { query: "Testpatient" };
      case "appointments.book":
        return { patient: id("patientA1"), slot: id("slotA1") };
      case "appointments.cancel":
        return { appointment: id("apptA1") };
      default:
        return { patient: id("patientA1"), phone: "+1 555 0150" };
    }
  };
  const OPTIONAL_SAMPLES: Record<string, unknown> = {
    date: "2026-10-06",
    days: 2,
    doctor: undefined, // filled per test: a doctor of the caller's site
    max_results: 3,
    date_of_birth: "1990-01-01",
    reason: "annual check",
    "appointments.cancel.reason": "other",
    email: "x@example.test",
    phone: "+1 555 0150",
  };

  it("asks for each required field by name when it is missing, and never for an optional one", () => {
    for (const cap of Object.values(capabilities)) {
      const call = caller(cap.id);
      const base = baseline(cap.id);
      const required = cap.fields.filter((f) => f.required).map((f) => f.name);
      for (const name of required) {
        const { [name]: _removed, ...rest } = base;
        const result = call("receptionA1", rest);
        expect(result.status, `${cap.id} without ${name}`).toBe("rejected");
        expect(result.errors[0], `${cap.id} without ${name}`).toMatchObject({ code: "missing_fields", field: name });
      }
      // Every optional field is accepted with a valid value, alone on top of the baseline.
      for (const field of cap.fields.filter((f) => !f.required)) {
        const sample = field.name === "doctor" ? id("doctorA1") : (OPTIONAL_SAMPLES[`${cap.id}.${field.name}`] ?? OPTIONAL_SAMPLES[field.name]);
        expect(sample, `no sample for ${cap.id}.${field.name}`).toBeDefined();
        const result = call("receptionA1", { ...base, [field.name]: sample });
        expect(result.status, `${cap.id} with optional ${field.name}: ${JSON.stringify(result.errors)}`).toBe("ok");
      }
      // Every declared field name is real and an undeclared one is refused.
      const unknown = call("receptionA1", { ...base, not_a_field: "x" });
      expect(unknown.errors[0], cap.id).toMatchObject({ code: "validation_failed", field: "not_a_field" });
    }
  });

  it("enforces the declared type, bounds, length and enumeration of every field", () => {
    let checked = 0;
    for (const cap of Object.values(capabilities)) {
      const call = caller(cap.id);
      const base = baseline(cap.id);
      for (const field of cap.fields) {
        const refused = (value: unknown, why: string): void => {
          const result = call("receptionA1", { ...base, [field.name]: value });
          expect(result.errors[0], `${cap.id}.${field.name} ${why}`).toMatchObject({ code: "validation_failed", field: field.name });
          checked += 1;
        };
        if (field.type === "integer") {
          refused("three", "as text");
          if (field.minimum !== undefined) refused(field.minimum - 1, "below the minimum");
          if (field.maximum !== undefined) refused(field.maximum + 1, "above the maximum");
          if (field.minimum !== undefined) {
            expect(call("receptionA1", { ...base, [field.name]: field.minimum }).status, `${cap.id}.${field.name} at minimum`).toBe("ok");
          }
          if (field.maximum !== undefined) {
            expect(call("receptionA1", { ...base, [field.name]: field.maximum }).status, `${cap.id}.${field.name} at maximum`).toBe("ok");
          }
        }
        if (field.type === "date") {
          refused("14 October", "as a free-form date");
        }
        if (field.type === "enum") {
          refused("not-a-reason", "outside the enumeration");
          for (const value of field.values ?? []) {
            expect(call("receptionA1", { ...base, [field.name]: value }).status, `${cap.id}.${field.name}=${value}`).toBe("ok");
          }
        }
        if (field.type === "string" && field.max_length !== undefined && field.name !== "email" && field.name !== "query") {
          refused("x".repeat(field.max_length + 1), "over the maximum length");
        }
        if (field.type === "string" && field.min_length !== undefined && field.name === "query") {
          refused("a".repeat(field.min_length - 1), "under the minimum length");
        }
        if (field.type === "boolean") {
          refused("yes", "as text");
        }
      }
    }
    expect(checked).toBeGreaterThan(10);
  });

  it("applies the declared literal defaults", () => {
    const s = world.app.service;
    const listed = s.availabilityList(world.identity("receptionA1"), { days: 14 });
    const slotCount = listed.values.find((f) => f.field === "slot_count")?.value as { value: number };
    const defaultMax = capabilities["availability.list"]?.fields.find((f) => f.name === "max_results")?.default?.value;
    expect(defaultMax).toBe(5);
    // 3 free slots are seeded: asking for no maximum lists them all (<= default); asking for 1 lists one.
    expect(slotCount.value).toBeLessThanOrEqual(defaultMax as number);
    const one = s.availabilityList(world.identity("receptionA1"), { days: 14, max_results: 1 });
    expect((one.values.find((f) => f.field === "slot_count")?.value as { value: number }).value).toBe(1);
    const defaultDays = capabilities["availability.list"]?.fields.find((f) => f.name === "days")?.default?.value;
    expect(defaultDays).toBe(1);
    expect(capabilities["patients.find"]?.fields.find((f) => f.name === "max_results")?.default?.value).toBe(5);
  });
});

describe("validation rules and failure states are real", () => {
  const idOf = (w: World, ref: string): string => w.records[ref]?.entity_id ?? "";

  // Each declared validation rule, violated through the real operation, in a world of its own.
  const RULES: Record<string, Record<string, (w: World) => HostResult>> = {
    "availability.list": {
      date_in_window: (w) => w.app.service.availabilityList(w.identity("receptionA1"), { date: "2025-01-01" }),
      days_in_range: (w) => w.app.service.availabilityList(w.identity("receptionA1"), { days: 99 }),
    },
    "patients.find": {
      query_not_blank: (w) => w.app.service.patientsFind(w.identity("receptionA1"), { query: "a" }),
      birth_date_not_future: (w) =>
        w.app.service.patientsFind(w.identity("receptionA1"), { query: "Test", date_of_birth: "2999-01-01" }),
    },
    "appointments.book": {
      slot_in_window: (w) => {
        const row = w.app.db.prepare("SELECT starts_at FROM slots WHERE id = ?").get(idOf(w, "slotA1")) as { starts_at: string };
        w.clock.set(new Date(new Date(row.starts_at).getTime() - 30 * 60_000).toISOString());
        return w.app.service.book.prepare(w.identity("receptionA1"), { patient: idOf(w, "patientA1"), slot: idOf(w, "slotA1") });
      },
      slot_free: (w) => {
        const reception = w.identity("receptionA1");
        const fields = { patient: idOf(w, "patientA1"), slot: idOf(w, "slotA1") };
        const prepared = preparedActionFrom("appointments.book", w.app.service.book.prepare(reception, fields), { now: w.clock.now() });
        w.app.service.book.commit(reception, prepared.idempotency_key, prepared);
        return w.app.service.book.prepare(reception, { patient: idOf(w, "patientA1b"), slot: idOf(w, "slotA1") });
      },
      patient_free: (w) => {
        const reception = w.identity("receptionA1");
        const first = preparedActionFrom(
          "appointments.book",
          w.app.service.book.prepare(reception, { patient: idOf(w, "patientA1"), slot: idOf(w, "slotA1") }),
          { now: w.clock.now() },
        );
        w.app.service.book.commit(reception, first.idempotency_key, first);
        const startsAt = w.app.db.prepare("SELECT starts_at FROM slots WHERE id = ?").get(idOf(w, "slotA1")) as { starts_at: string };
        w.app.db
          .prepare("INSERT INTO doctors (tenant_id, site_id, id, name, specialty) VALUES ('tnt_a', 'site_a1', 'doc_otherdoctor01', 'Dr. Second Doctor', 'general practice')")
          .run();
        w.app.db
          .prepare(
            "INSERT INTO slots (tenant_id, site_id, id, doctor_id, starts_at, duration_minutes, status, version, updated_at) VALUES ('tnt_a', 'site_a1', 'slot_parallel0001', 'doc_otherdoctor01', ?, 30, 'free', 1, ?)",
          )
          .run(startsAt.starts_at, startsAt.starts_at);
        return w.app.service.book.prepare(reception, { patient: idOf(w, "patientA1"), slot: "slot_parallel0001" });
      },
    },
    "appointments.cancel": {
      appointment_active: (w) => {
        const reception = w.identity("receptionA1");
        const prepared = preparedActionFrom(
          "appointments.cancel",
          w.app.service.cancel.prepare(reception, { appointment: idOf(w, "apptA1") }),
          { now: w.clock.now() },
        );
        w.app.service.cancel.commit(reception, prepared.idempotency_key, prepared);
        return w.app.service.cancel.prepare(reception, { appointment: idOf(w, "apptA1") });
      },
      appointment_not_started: (w) => {
        const row = w.app.db.prepare("SELECT starts_at FROM appointments WHERE id = ?").get(idOf(w, "apptA1")) as { starts_at: string };
        w.clock.set(new Date(new Date(row.starts_at).getTime() + 60_000).toISOString());
        return w.app.service.cancel.prepare(w.identity("receptionA1"), { appointment: idOf(w, "apptA1") });
      },
    },
    "patients.update_contact": {
      contact_change_present: (w) => w.app.service.contact.prepare(w.identity("patientA1"), { patient: idOf(w, "patientA1") }),
      phone_format: (w) =>
        w.app.service.contact.prepare(w.identity("patientA1"), { patient: idOf(w, "patientA1"), phone: "call me" }),
      email_format: (w) =>
        w.app.service.contact.prepare(w.identity("patientA1"), { patient: idOf(w, "patientA1"), email: "not-an-email" }),
    },
  };

  it("has a probe for every declared validation rule", () => {
    for (const cap of Object.values(capabilities)) {
      expect(Object.keys(RULES[cap.id] ?? {}).sort(), `rules of ${cap.id}`).toEqual(cap.validation.map((r) => r.id).sort());
    }
  });

  it("reports each violated rule with its declared error code, on a field the rule names", async () => {
    let probed = 0;
    for (const cap of Object.values(capabilities)) {
      for (const rule of cap.validation) {
        const fresh = createWorld();
        try {
          const result = RULES[cap.id]?.[rule.id]?.(fresh);
          expect(result?.status, `${cap.id}/${rule.id}`).toBe("rejected");
          expect(result?.errors[0]?.code, `${cap.id}/${rule.id}`).toBe(rule.error_code);
          expect(cap.failure_states.map((f) => f.code), `${cap.id}/${rule.id} code is a declared failure`).toContain(rule.error_code);
          const field = result?.errors[0]?.field;
          if (field !== undefined && rule.fields.length > 0) {
            expect(rule.fields, `${cap.id}/${rule.id} reported on ${field}`).toContain(field);
          }
          probed += 1;
        } finally {
          await fresh.close();
        }
      }
    }
    expect(probed).toBe(Object.values(capabilities).reduce((n, cap) => n + cap.validation.length, 0));
  });

  it("only ever reports a failure the capability declares, and declares each failure once", () => {
    const byCapability = new Map<string, Set<string>>();
    for (const o of observeEverything(world)) {
      const set = byCapability.get(o.capability) ?? new Set<string>();
      for (const error of o.result.errors) set.add(error.code);
      byCapability.set(o.capability, set);
    }
    expect(byCapability.size).toBeGreaterThan(5);
    for (const cap of Object.values(capabilities)) {
      const declared = new Set(cap.failure_states.map((f) => f.code));
      for (const code of byCapability.get(cap.id) ?? []) {
        expect(declared.has(code) || PLATFORM_CODES.includes(code), `${cap.id} reported undeclared ${code}`).toBe(true);
      }
      expect(declared.size, cap.id).toBe(cap.failure_states.length);
    }
  });

  it("reports a down dependency as unavailable on every mapped tool, never as a success", async () => {
    const identity = world.identity("receptionA1");
    // arguments the server accepts, so the call reaches the application and the fault is the only thing wrong
    const sampleFields: Record<string, Record<string, unknown>> = {
      "availability.list": {},
      "patients.find": { query: "Test" },
      "appointments.book": { patient: "pat_x", slot: "slot_x" },
      "appointments.cancel": { appointment: "apt_x" },
      "patients.update_contact": { patient: "pat_x" },
    };
    const argsFor = (capability: string, op: string): Record<string, unknown> => {
      switch (op) {
        case "commit":
          return { identity, idempotency_key: idemKey(), prepared: { effects: [], data_versions: [] } };
        case "status":
          return { identity, idempotency_key: idemKey() };
        default:
          return { identity, fields: sampleFields[capability] ?? {} };
      }
    };
    let probed = 0;
    for (const cap of Object.values(capabilities)) {
      for (const op of operationNames) {
        const call = cap.operations[op];
        if (call === undefined) continue;
        world.app.instrumentation.injectFault(call.mcp_tool, "unavailable");
        const out = await client.callTool({ name: call.mcp_tool, arguments: argsFor(cap.id, op) });
        const result = out.structuredContent as unknown as HostResult;
        expect(out.isError === true, call.mcp_tool).toBe(false);
        expect(result.status, call.mcp_tool).toBe("unavailable");
        expect(result.errors[0]?.code, call.mcp_tool).toBe("unavailable");
        expect(cap.failure_states.map((f) => f.code), cap.id).toContain("unavailable");
        probed += 1;
      }
    }
    expect(probed).toBe(14);
  });
});

describe("the wording and the obligations are backed by real facts", () => {
  const factNames = (result: HostResult): string[] => result.values.map((v) => v.field);

  const results = (): Record<string, { prepared?: HostResult; success: HostResult }> => {
    const s = world.app.service;
    const reception = world.identity("receptionA1");
    const patient = world.identity("patientA1");
    const bookPrepared = s.book.prepare(reception, { patient: id("patientA1"), slot: id("slotA1"), reason: "annual check" });
    const bookAction = preparedActionFrom("appointments.book", bookPrepared, { now: world.clock.now() });
    const cancelPrepared = s.cancel.prepare(reception, { appointment: id("apptA1"), reason: "other" });
    const cancelAction = preparedActionFrom("appointments.cancel", cancelPrepared, { now: world.clock.now() });
    const contactPrepared = s.contact.prepare(patient, { patient: id("patientA1"), phone: "+1 555 0150", email: "x@example.test" });
    const contactAction = preparedActionFrom("patients.update_contact", contactPrepared, { now: world.clock.now(), mode: "none" });
    return {
      "availability.list": { success: s.availabilityList(reception, { days: 14 }) },
      "patients.find": { success: s.patientsFind(reception, { query: "Testpatient" }) },
      "appointments.book": { prepared: bookPrepared, success: s.book.commit(reception, bookAction.idempotency_key, bookAction) },
      "appointments.cancel": { prepared: cancelPrepared, success: s.cancel.commit(reception, cancelAction.idempotency_key, cancelAction) },
      "patients.update_contact": { prepared: contactPrepared, success: s.contact.commit(patient, contactAction.idempotency_key, contactAction) },
    };
  };

  it("finds every obligated fact in the result it is obligated for", () => {
    const real = results();
    for (const cap of Object.values(capabilities)) {
      const entry = real[cap.id];
      expect(entry?.success.status, cap.id).toBe("ok");
      for (const obligation of cap.response_obligations) {
        if (obligation.when === "success" || obligation.when === "always") {
          expect(factNames(entry?.success as HostResult), `${cap.id} ${obligation.fact} on success`).toContain(obligation.fact);
        }
        if (obligation.when === "prepared" || obligation.when === "always") {
          expect(entry?.prepared, `${cap.id} has no prepare`).toBeDefined();
          expect(factNames(entry?.prepared as HostResult), `${cap.id} ${obligation.fact} on prepare`).toContain(obligation.fact);
        }
        expect(obligation.when, `${cap.id} ${obligation.fact}: nothing is returned on failure to state`).not.toBe("failure");
      }
    }
  });

  it("uses only placeholders that name a declared field or a fact the host really returns", () => {
    const real = results();
    let templates = 0;
    for (const cap of Object.values(capabilities)) {
      const fieldNames = cap.fields.map((f) => f.name);
      const entry = real[cap.id];
      const successFacts = factNames(entry?.success as HostResult);
      const preparedFacts = entry?.prepared === undefined ? [] : factNames(entry.prepared);
      for (const language of manifest.languages.supported) {
        const c = contract(cap.id, language);
        const check = (template: string, available: string[], where: string): void => {
          for (const name of placeholders(template)) {
            templates += 1;
            if (name.startsWith("input.")) {
              expect(fieldNames, `${cap.id}/${language} ${where}: {${name}}`).toContain(name.slice("input.".length));
            } else {
              expect(available, `${cap.id}/${language} ${where}: {${name}}`).toContain(name);
            }
          }
        };
        for (const s of c.success) check(s.text, successFacts, `success ${s.outcome}`);
        if (c.confirmations !== undefined) check(c.confirmations.request, preparedFacts, "confirmation request");
        for (const f of c.failures) {
          // A failure carries no result facts: only the user's own words can be quoted.
          for (const name of placeholders(f.text)) expect(name.startsWith("input."), `${cap.id}/${language} failure ${f.code}: {${name}}`).toBe(true);
        }
        for (const p of c.field_prompts) check(p.prompt, [], `prompt ${p.field}`);
        for (const x of c.clarifications) check(x.prompt, [], `clarification ${x.rule_id}`);
      }
    }
    expect(templates).toBeGreaterThan(40);
  });

  it("states every obligation in the wording, in both languages", () => {
    for (const cap of Object.values(capabilities)) {
      for (const language of manifest.languages.supported) {
        const c = contract(cap.id, language);
        const stated = new Set(c.success.flatMap((s) => placeholders(s.text)));
        const confirmed = new Set(placeholders(c.confirmations?.request ?? ""));
        for (const o of cap.response_obligations) {
          if (o.when === "success" || o.when === "always") expect(stated.has(o.fact), `${cap.id}/${language} success states ${o.fact}`).toBe(true);
          if (o.when === "prepared" || o.when === "always") expect(confirmed.has(o.fact), `${cap.id}/${language} confirmation states ${o.fact}`).toBe(true);
        }
        // one response per declared failure state, none for an undeclared one
        expect(c.failures.map((f) => f.code).sort(), `${cap.id}/${language} failures`).toEqual(cap.failure_states.map((f) => f.code).sort());
        // confirmation wording exactly when the mode asks for a confirmation
        expect(c.confirmations !== undefined, `${cap.id}/${language} confirmations`).toBe(cap.confirmation.mode !== "none");
      }
    }
  });
});

describe("the result schemas shipped in the package are the contract of the real results", () => {
  const ajv = createAjv();
  const compiled = new Map<string, ValidateFunction>();
  const validator = (path: string): ValidateFunction => {
    const found = compiled.get(path) ?? ajv.compile(resultSchema(path));
    compiled.set(path, found);
    return found;
  };

  const schemaFor = (o: Observed): string => {
    const cap = capabilities[o.capability];
    if (cap !== undefined) {
      const call = cap.operations[o.operation as (typeof operationNames)[number]];
      if (call === undefined) throw new Error(`${o.capability} has no ${o.operation}`);
      return call.result_schema;
    }
    for (const c of Object.values(capabilities)) {
      for (const entity of Object.values(c.entity_types)) {
        if (entity.resolve?.mcp_tool === o.capability) return entity.resolve.result_schema;
      }
    }
    throw new Error(`no schema for ${o.capability}`);
  };

  it("validates every real result of every operation, success and refusal, against the declared schema", () => {
    const observed = observeEverything(world);
    const used = new Set<string>();
    for (const o of observed) {
      const path = schemaFor(o);
      used.add(path);
      const validate = validator(path);
      expect(validate(o.result), `${o.capability} ${o.operation} (${o.label}) vs ${path}: ${JSON.stringify(validate.errors?.slice(0, 3))}\n${JSON.stringify(o.result)}`).toBe(true);
    }
    // every schema the package names was exercised, and every schema file is named
    const declared = new Set<string>();
    for (const cap of Object.values(capabilities)) {
      for (const call of Object.values(cap.operations)) declared.add(call.result_schema);
      for (const entity of Object.values(cap.entity_types)) if (entity.resolve !== undefined) declared.add(entity.resolve.result_schema);
    }
    expect([...used].sort()).toEqual([...declared].sort());
  });

  it("rejects results that break the declared facts (the schemas are not vacuous)", () => {
    const s = world.app.service;
    const reception = world.identity("receptionA1");
    const book = capabilities["appointments.book"] as Capability;
    const prepare = validator(book.operations.prepare?.result_schema ?? "");
    const good = s.book.prepare(reception, { patient: id("patientA1"), slot: id("slotA1") });
    expect(prepare(good)).toBe(true);
    const clone = (): HostResult => JSON.parse(JSON.stringify(good)) as HostResult;

    const noPatient = clone();
    noPatient.values = noPatient.values.filter((v) => v.field !== "patient_name");
    expect(prepare(noPatient), "a missing obligated fact").toBe(false);

    const invented = clone();
    invented.values.push({ field: "diagnosis", value: { kind: "text", value: "x" } });
    expect(prepare(invented), "an invented fact").toBe(false);

    const wrongKind = clone();
    wrongKind.values = wrongKind.values.map((v) => (v.field === "doctor_name" ? { ...v, value: { kind: "integer", value: 1 } } : v));
    expect(prepare(wrongKind), "a fact of the wrong kind").toBe(false);

    const noPreparation = clone();
    delete noPreparation.preparation;
    expect(prepare(noPreparation), "an ok prepare with no preparation").toBe(false);

    const wrongRisk = clone();
    if (wrongRisk.preparation !== undefined) wrongRisk.preparation.declared_risk_level = 1;
    expect(prepare(wrongRisk), "a different risk level").toBe(false);

    const noFreshness = clone();
    delete noFreshness.freshness;
    expect(prepare(noFreshness), "an ok result with no freshness").toBe(false);

    const failure = s.book.prepare(world.identity("doctorA1"), { patient: id("patientA1"), slot: id("slotA1") });
    expect(prepare(failure), "a real refusal").toBe(true);
    expect(prepare({ ...failure, errors: [] }), "a refusal with no error").toBe(false);
    expect(prepare({ ...failure, errors: [{ code: "teapot", message: "x" }] }), "an error code of no taxonomy").toBe(false);
    expect(prepare({ ...failure, values: good.values }), "a refusal that carries values").toBe(false);

    const find = validator(capabilities["patients.find"]?.operations.read?.result_schema ?? "");
    const found = s.patientsFind(reception, { query: "Testpatient" });
    expect(find(found)).toBe(true);
    expect(find({ ...found, identifiers: [{ entity_type: "appointment", entity_id: "apt_x" }] }), "a record of another type").toBe(false);
  });

  it("keeps the documented resolution contract: identifiers, a label each at the same position, the snapshot version", () => {
    const resolve = validator("schemas/resolve.result.schema.json");
    const result = world.app.service.resolvePatient(world.identity("receptionA1"), { entity_type: "patient", mention: "Testpatient" });
    expect(resolve(result)).toBe(true);
    expect(result.identifiers.length).toBe(result.values.filter((v) => v.field === "label").length);
    expect(resolve({ ...result, values: result.values.filter((v) => v.field !== "candidate_count") }), "no candidate_count").toBe(false);
  });
});

describe("policy, roles and permissions", () => {
  it("grants each role exactly the capabilities the application's own permission table gives it", () => {
    expect(Object.keys(manifest.tenancy.roles).sort()).toEqual([...ROLES].sort());
    expect(policy.role_grants.map((g) => g.role).sort()).toEqual([...ROLES].sort());
    for (const grant of policy.role_grants) {
      expect([...grant.capabilities].sort(), grant.role).toEqual(roleCapabilities(grant.role as (typeof ROLES)[number]).sort());
    }
    for (const cap of Object.values(capabilities)) {
      expect(cap.permissions, cap.id).toEqual([CAPABILITY_PERMISSION[cap.id as CapabilityId]]);
    }
  });

  it("refuses with permission_denied, at every operation, exactly the role and capability pairs the policy does not grant", () => {
    const s = world.app.service;
    const asRole = (role: string): ReturnType<World["identity"]> => ({
      ...world.identity(role === "doctor" ? "doctorA1" : role === "patient" ? "patientA1" : "receptionA1"),
    });
    const calls: Record<string, ((identity: unknown) => HostResult)[]> = {
      "availability.list": [(i) => s.availabilityList(i, {})],
      "patients.find": [(i) => s.patientsFind(i, { query: "Testpatient" })],
      "appointments.book": [
        (i) => s.book.validate(i, { patient: id("patientA1"), slot: id("slotA1") }),
        (i) => s.book.prepare(i, { patient: id("patientA1"), slot: id("slotA1") }),
        (i) => s.book.status(i, idemKey()),
      ],
      "appointments.cancel": [
        (i) => s.cancel.validate(i, { appointment: id("apptA1") }),
        (i) => s.cancel.prepare(i, { appointment: id("apptA1") }),
        (i) => s.cancel.status(i, idemKey()),
      ],
      "patients.update_contact": [
        (i) => s.contact.validate(i, { patient: id("patientA1"), phone: "+1 555 0150" }),
        (i) => s.contact.prepare(i, { patient: id("patientA1"), phone: "+1 555 0150" }),
        (i) => s.contact.status(i, idemKey()),
      ],
    };
    let denied = 0;
    for (const role of ROLES) {
      const granted = policy.role_grants.find((g) => g.role === role)?.capabilities ?? [];
      for (const cap of capabilityIds) {
        for (const call of calls[cap] ?? []) {
          const result = call(asRole(role));
          if (granted.includes(cap)) {
            expect(result.errors[0]?.code, `${role} may use ${cap}`).not.toBe("permission_denied");
          } else {
            expect(result.errors[0]?.code, `${role} may not use ${cap}`).toBe("permission_denied");
            denied += 1;
          }
        }
      }
    }
    expect(denied).toBe(6); // the doctor: no booking and no contact change, three operations each
    // and the table the policy mirrors is complete
    expect(Object.keys(ROLE_PERMISSIONS)).toEqual([...ROLES]);
  });
});

describe("the context exchange the manifest declares is the one the application performs", () => {
  it("mints tokens with the declared issuer, audience, key and lifetime ceiling", async () => {
    const response = await fetch(`${url}/host/context-token`, {
      method: "POST",
      headers: { Authorization: `Bearer ${BEARER}`, "content-type": "application/json" },
      body: JSON.stringify({ tenant_id: "tnt_a", workspace_id: "site_a1", principal_id: "usr_reception_a1" }),
    });
    const body = (await response.json()) as { token: string; key_id: string; max_lifetime_seconds: number };
    expect(manifest.context_exchange.key_ids).toEqual([body.key_id]);
    expect(manifest.integrity.publisher_key_ids).toEqual([body.key_id]);
    expect(manifest.context_exchange.max_lifetime_seconds).toBe(body.max_lifetime_seconds);
    expect(manifest.context_exchange.max_lifetime_seconds).toBe(MAX_LIFETIME_SECONDS);
    const verified = verifyContextToken(body.token, {
      publicKeyBase64: devPublicKeyBase64(),
      issuer: manifest.context_exchange.issuer,
      audience: manifest.context_exchange.audience,
      applicationId: manifest.application.id,
      keyId: manifest.context_exchange.key_ids[0] ?? "",
      now: world.clock.now(),
    });
    expect(verified.claims.roles).toEqual(["receptionist"]);
    expect([...(verified.claims.capability_scopes as string[])].sort()).toEqual(
      [...(policy.role_grants.find((g) => g.role === "receptionist")?.capabilities ?? [])].sort(),
    );
  });
});

describe("the documentation the package ships resolves", () => {
  it("points every index entry at a document that exists, at a heading that exists", () => {
    const slug = (heading: string): string => heading.toLowerCase().replace(/[^a-z0-9_ -]/gu, "").trim().replace(/\s+/gu, "-");
    for (const entry of docIndex.entries) {
      expect(existsSync(join(PACKAGE_DIR, entry.location.path)), `${entry.id}: ${entry.location.path}`).toBe(true);
      const headings = readFileSync(join(PACKAGE_DIR, entry.location.path), "utf8")
        .split("\n")
        .filter((line) => /^#{1,6} /u.test(line))
        .map((line) => slug(line.replace(/^#+ /u, "")));
      expect(headings, `${entry.id} anchor ${entry.location.anchor}`).toContain(entry.location.anchor);
    }
    for (const cap of Object.values(capabilities)) {
      const refs = docIndex.entries.filter((e) => e.capability_id === cap.id).map((e) => e.id);
      expect([...cap.doc_refs].sort(), cap.id).toEqual(refs.sort());
    }
  });

  it("ships the RAG source documents in both languages, and nothing in them contradicts the rules the app enforces", () => {
    const rules = readText("documentation/policies/booking-rules.md");
    expect(rules).toContain("60 minutes to 90 days");
    expect(rules).toContain(`${BOOKING_TTL_SECONDS / 60} minutes`);
    expect(readText("documentation/policies/booking-rules.it.md")).toContain("60 minuti a 90 giorni");
    for (const name of ["booking-rules", "contact-and-privacy"]) {
      expect(existsSync(join(PACKAGE_DIR, "documentation", "policies", `${name}.md`))).toBe(true);
      expect(existsSync(join(PACKAGE_DIR, "documentation", "policies", `${name}.it.md`))).toBe(true);
    }
  });
});
