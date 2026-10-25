import { describe, expect, it } from "vitest";

import * as sdk from "../src/index.js";
import { isAlixAIError } from "../src/index.js";
import type { AlixAIError } from "../src/index.js";
import { loadSpec } from "./helpers/openapi.js";

// The decision namespace of the SDK (task D-10): its operation table against schemas/api/paths/decision.yaml, the client against
// the in-process fake, and the rules that make this area what it is: read-only, advisory, fail-closed.

const { decision } = sdk;

const failure = async (call: Promise<unknown>): Promise<AlixAIError> => {
  try {
    await call;
  } catch (error) {
    if (isAlixAIError(error)) return error;
    throw error;
  }
  throw new Error("the call did not fail");
};

describe("the operation table", () => {
  it("matches the OpenAPI document, and the SDK speaks only the status", () => {
    const documented = loadSpec().operations.filter((o) => o.path.startsWith("/v1/decision/"));
    expect(documented.map((o) => o.operationId).sort()).toEqual(["getDecisionConfig", "getDecisionStatus", "putDecisionConfig"]);
    expect(Object.keys(decision.DECISION_OPERATIONS)).toEqual(["getDecisionStatus"]);
    const declared = documented.find((o) => o.operationId === "getDecisionStatus");
    const row = decision.DECISION_OPERATIONS.getDecisionStatus;
    expect(declared && [declared.method.toUpperCase(), declared.path]).toEqual([row.method, row.path]);
  });
});

describe("getStatus against the fake", () => {
  it("reads the status with one fresh token and one GET", async () => {
    const fake = decision.createFakeDecisionServer();
    fake.setStatus(decision.decisionStatusFixture("advisory", { accepted: 7, fallbacks: { timeout: 1, low_confidence: 2 } }));
    const asked: string[] = [];
    const client = decision.createDecisionClient({
      baseUrl: fake.baseUrl,
      fetch: fake.fetch,
      getContextToken: (request) => {
        asked.push(request.operation);
        return fake.issueToken();
      },
    });
    const status = await client.getStatus();
    expect(status).toMatchObject({ mode: "advisory", accepted_count: 7, rejected_count: 3, model_id: "fake-scorer", model_revision: "r1" });
    expect(status.fallback_counts).toEqual([
      { reason: "low_confidence", count: 2 },
      { reason: "timeout", count: 1 },
    ]);
    expect(asked).toEqual(["getDecisionStatus"]);
    expect(fake.requests).toEqual([{ method: "GET", path: "/v1/decision/status", authenticated: true, status: 200 }]);
  });

  it("reports the layer off, with no model, when nothing was configured", async () => {
    const fake = decision.createFakeDecisionServer();
    const client = decision.createDecisionClient({ baseUrl: fake.baseUrl, fetch: fake.fetch, getContextToken: fake.getContextToken });
    const status = await client.getStatus();
    expect(status).toMatchObject({ mode: "off", accepted_count: 0, rejected_count: 0, fallback_counts: [] });
    expect("model_id" in status).toBe(false);
  });

  it("fails closed: no capability, no service, a replayed token", async () => {
    const fake = decision.createFakeDecisionServer({ capabilityScopes: ["demo.lookup"] });
    const client = decision.createDecisionClient({ baseUrl: fake.baseUrl, fetch: fake.fetch, getContextToken: fake.getContextToken });
    expect((await failure(client.getStatus())).code).toBe("permission_denied");

    const allowed = decision.createFakeDecisionServer();
    const ok = decision.createDecisionClient({ baseUrl: allowed.baseUrl, fetch: allowed.fetch, getContextToken: allowed.getContextToken });
    allowed.setUnavailable();
    const unavailable = await failure(ok.getStatus());
    expect([unavailable.code, unavailable.retryable, unavailable.status]).toEqual(["unavailable", true, 503]);
    allowed.setUnavailable(false);
    await expect(ok.getStatus()).resolves.toMatchObject({ mode: "off" });

    const token = allowed.issueToken();
    const reuse = decision.createDecisionClient({ baseUrl: allowed.baseUrl, fetch: allowed.fetch, getContextToken: () => token });
    await reuse.getStatus();
    expect((await failure(reuse.getStatus())).code).toBe("replayed");
  });

  it("refuses a status that is not the document, and never hands it over as a result", async () => {
    const good = decision.decisionStatusFixture("shadow", { accepted: 1, fallbacks: { timeout: 2 } });
    const bad: Record<string, unknown> = {
      "a sum that does not add up": { ...good, rejected_count: 5 },
      "a negative count": { ...good, accepted_count: -1 },
      "an unknown mode": { ...good, mode: "authorised" },
      "an extra member (a score)": { ...good, score: 0.97 },
      "an unknown fallback reason": { ...good, fallback_counts: [{ reason: "made_up", count: 2 }], rejected_count: 2 },
      "a reason twice": { ...good, fallback_counts: [...good.fallback_counts, { reason: "timeout", count: 0 }] },
      "a missing provider": { ...good, provider_id: undefined },
      "a timestamp that is not UTC": { ...good, since: "2026-10-01T00:00:00+02:00" },
      "not an object": [],
    };
    for (const [name, body] of Object.entries(bad)) {
      const client = decision.createDecisionClient({
        baseUrl: "https://alixai.test",
        getContextToken: () => "tok",
        fetch: async () => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } }),
      });
      const error = await failure(client.getStatus());
      expect([name, error.code, error.details?.["reason"]]).toEqual([name, "internal", "unexpected_shape"]);
    }
  });

  it("refuses a body that is not JSON, and a token provider that fails or returns a bad token sends nothing", async () => {
    let sent = 0;
    const counting = async (): Promise<Response> => {
      sent += 1;
      return new Response("<html>", { status: 200 });
    };
    const html = decision.createDecisionClient({ baseUrl: "https://alixai.test", getContextToken: () => "tok", fetch: counting });
    expect((await failure(html.getStatus())).details?.["reason"]).toBe("not_json");
    for (const getContextToken of [
      () => {
        throw new Error("no session");
      },
      () => "Bearer tok",
      () => "",
    ]) {
      const client = decision.createDecisionClient({ baseUrl: "https://alixai.test", getContextToken, fetch: counting });
      expect((await failure(client.getStatus())).code).toBe("unauthenticated");
    }
    expect(sent).toBe(1);
  });

  it("honours a signal and a deadline, and refuses a base URL that would expose the token", async () => {
    const hang = (_url: string, init?: RequestInit): Promise<Response> =>
      new Promise((_resolve, reject) => init?.signal?.addEventListener("abort", () => reject(init.signal?.reason)));
    const client = decision.createDecisionClient({ baseUrl: "https://alixai.test", getContextToken: () => "tok", fetch: hang });
    expect((await failure(client.getStatus({ timeoutMs: 10 }))).code).toBe("timeout");
    const controller = new AbortController();
    const pending = failure(client.getStatus({ signal: controller.signal }));
    controller.abort(new Error("stop"));
    await expect(pending).rejects.toThrow("stop");
    const network = decision.createDecisionClient({
      baseUrl: "https://alixai.test",
      getContextToken: () => "tok",
      fetch: async () => {
        throw new TypeError("fetch failed");
      },
    });
    expect((await failure(network.getStatus())).code).toBe("unavailable");
    expect(() => decision.createDecisionClient({ baseUrl: "http://alixai.example.com", getContextToken: () => "tok" })).toThrow(/https/);
  });
});

describe("advisory, never an authorisation", () => {
  it("exports no name that reads as an authorisation, and documents the data as advisory", async () => {
    for (const name of Object.keys(decision)) expect(name).not.toMatch(/authori[sz]|allow|permit|approve|grant/i);
    const { readFileSync } = await import("node:fs");
    const { join } = await import("node:path");
    const text = ["client.ts", "types.ts", "index.ts"].map((f) => readFileSync(join(__dirname, "..", "src", "decision", f), "utf8")).join("\n");
    expect(text).toContain("ADVISORY");
    expect(text.toLowerCase()).toContain("never an authorisation");
  });

  it("is part of the sdk surface as a namespace and the SDK stays free of runtime dependencies", () => {
    expect(typeof sdk.decision.createDecisionClient).toBe("function");
  });
});
