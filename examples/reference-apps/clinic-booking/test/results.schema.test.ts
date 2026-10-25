import { afterEach, beforeEach, describe, expect, it } from "vitest";

import type { HostResult } from "../src/results.ts";
import { ASPECTS, inspect } from "../src/inspect.ts";
import { asCoreResult, deterministicResultSchema, describeErrors, factSchema, preparedActionSchema } from "./helpers/schemas.ts";
import { observeEverything } from "./helpers/observe.ts";
import { createWorld, type World } from "./helpers/world.ts";

let world: World;

beforeEach(() => {
  world = createWorld();
});

afterEach(async () => {
  await world.close();
});

describe("every result is a valid DeterministicResult", () => {
  it("validates against schemas/actions/deterministic-result for every operation, success and failure", () => {
    const observed = observeEverything(world);
    expect(observed.length).toBeGreaterThan(35);
    for (const o of observed) {
      const asCore = asCoreResult(o.result, o.operation, o.capability);
      const valid = deterministicResultSchema(asCore);
      expect(valid, `${o.capability} ${o.operation} (${o.label}): ${describeErrors(deterministicResultSchema)}\n${JSON.stringify(o.result)}`).toBe(true);
    }
  });

  it("covers every status the application can answer and every failure code it declares", () => {
    const observed = observeEverything(world);
    const statuses = new Set(observed.map((o) => o.result.status));
    expect(statuses).toEqual(new Set(["ok", "rejected"]));
    const codes = new Set(observed.flatMap((o) => o.result.errors.map((e) => e.code)));
    for (const expected of ["permission_denied", "scope_violation", "validation_failed", "missing_fields", "conflict", "stale"]) {
      expect(codes, `no observed ${expected}`).toContain(expected);
    }
  });

  it("never states success when something failed, and states freshness on every ok", () => {
    for (const o of observeEverything(world)) {
      if (o.result.status === "ok") {
        expect(o.result.errors).toEqual([]);
        expect(o.result.freshness, `${o.label} has no freshness`).toBeDefined();
      } else {
        expect(o.result.errors.length, `${o.label} fails without an error`).toBeGreaterThan(0);
      }
    }
  });

  it("only ever uses names the verifier can compare: every fact is a typed canonical value", () => {
    for (const o of observeEverything(world)) {
      for (const fact of o.result.values) {
        expect(factSchema(fact), `${o.label}: ${fact.field}: ${describeErrors(factSchema)}`).toBe(true);
      }
    }
  });
});

describe("the validator is really enforcing the schema (negative controls)", () => {
  const base = {
    operation: "read",
    capability_id: "availability.list",
    status: "ok",
    identifiers: [],
    values: [],
    errors: [],
    allowed_next_actions: [],
    freshness: { as_of: "2026-10-05T07:00:00Z", version: "rv1-abc" },
  };

  it("accepts a minimal valid result and rejects broken ones", () => {
    expect(deterministicResultSchema(base)).toBe(true);
    expect(deterministicResultSchema({ ...base, errors: [{ code: "conflict", message: "x" }] })).toBe(false);
    expect(deterministicResultSchema({ ...base, freshness: undefined })).toBe(false);
    expect(deterministicResultSchema({ ...base, operation: "prepare" })).toBe(false);
    expect(deterministicResultSchema({ ...base, message: "Done!" })).toBe(false);
    expect(deterministicResultSchema({ ...base, values: [{ field: "a.b", value: { kind: "text", value: "x" } }] })).toBe(false);
  });
});

describe("inspection output", () => {
  it("reports every aspect with at least one fact and no secret", () => {
    const report = inspect(world.app.db, world.app.runtime(), [...ASPECTS]);
    expect(report.reports.map((r) => r.aspect)).toEqual([...ASPECTS]);
    for (const aspect of report.reports) {
      expect(aspect.state).toBe("reported");
      expect(aspect.facts.length).toBeGreaterThan(0);
      for (const fact of aspect.facts) {
        expect(factSchema(fact), `${aspect.aspect}.${fact.field}: ${describeErrors(factSchema)}`).toBe(true);
      }
    }
    const serialised = JSON.stringify(report);
    expect(serialised).not.toMatch(/BEGIN|PRIVATE|secret|password/iu);
  });
});
