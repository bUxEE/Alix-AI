import { describe, expect, it } from "vitest";

import { createClient } from "../src/client.js";
import { ME_OPERATIONS } from "../src/me/index.js";
import { MEMORY_OPERATIONS } from "../src/memory/index.js";
import { OPERATIONS } from "../src/operations.js";
import { createFakeServer } from "../src/testing/index.js";
import { loadSpec } from "./helpers/openapi.js";

describe("ME_OPERATIONS matches OpenAPI", () => {
  const spec = loadSpec();
  it("every operation exists in the document", () => {
    for (const operationId of Object.keys(ME_OPERATIONS)) {
      const declared = spec.operations.find((o) => o.operationId === operationId);
      expect(declared, `${operationId} is not in the OpenAPI document`).toBeDefined();
      expect(declared?.method).toBe(ME_OPERATIONS[operationId as keyof typeof ME_OPERATIONS].method);
      expect(declared?.path).toBe(ME_OPERATIONS[operationId as keyof typeof ME_OPERATIONS].path);
    }
  });
  it("covers the me area", () => {
    const documented = spec.operations.filter((o) => o.area === "me");
    expect(Object.keys(ME_OPERATIONS).sort()).toEqual(documented.map((o) => o.operationId).sort());
  });
  it("is merged into OPERATIONS", () => {
    for (const id of Object.keys(ME_OPERATIONS) as Array<keyof typeof ME_OPERATIONS>) {
      expect(OPERATIONS[id].path).toBe(ME_OPERATIONS[id].path);
      expect(OPERATIONS[id].method).toBe(ME_OPERATIONS[id].method);
      expect(OPERATIONS[id].authenticated).toBe(true);
    }
  });
});

describe("MEMORY_OPERATIONS matches OpenAPI", () => {
  const spec = loadSpec();
  it("every operation exists in the document", () => {
    for (const operationId of Object.keys(MEMORY_OPERATIONS)) {
      const declared = spec.operations.find((o) => o.operationId === operationId);
      expect(declared, `${operationId} is not in the OpenAPI document`).toBeDefined();
      expect(declared?.method).toBe(MEMORY_OPERATIONS[operationId as keyof typeof MEMORY_OPERATIONS].method);
      expect(declared?.path).toBe(MEMORY_OPERATIONS[operationId as keyof typeof MEMORY_OPERATIONS].path);
    }
  });
  it("covers the memory area", () => {
    const documented = spec.operations.filter((o) => o.area === "memory");
    expect(Object.keys(MEMORY_OPERATIONS).sort()).toEqual(documented.map((o) => o.operationId).sort());
  });
});

describe("client.me and client.memory against the fake", () => {
  it("exercises the principal LLM and memory surfaces", async () => {
    const fake = createFakeServer();
    const client = createClient({
      baseUrl: fake.baseUrl,
      fetch: fake.fetch,
      getContextToken: () => fake.issueToken({ session_id: "ses-1" }),
    });
    expect((await client.me.catalog()).entries).toEqual([]);
    expect((await client.me.llm.get()).bindings).toEqual([]);
    const bound = await client.me.llm.putRole("conversational_controller", { profile_id: "pprf_01J0000000000000000000000A" });
    expect(bound.bindings[0]?.role).toBe("conversational_controller");
    await client.me.llm.deleteRole("conversational_controller");
    expect((await client.me.providers.submitCredential("openai", { key: "sk-test" })).status).toBe("active");
    expect((await client.me.providers.deleteCredential("openai")).status).toBe("revoked");
    expect(
      (
        await client.me.providers.acknowledgeBillingNotice("openai", {
          notice_version: "2026-01",
          consumer_subscription_is_not_api_access: true,
          separate_api_billing_understood: true,
        })
      ).notice_version,
    ).toBe("2026-01");
    expect((await client.me.providers.oauthStart("openai", { capabilities: ["chat"], redirect_uri: "http://127.0.0.1/cb" })).authorization_url).toContain(
      "oauth",
    );
    expect((await client.memory.list()).items).toEqual([]);
    const created = await client.memory.create({ text: "prefer dark mode" });
    expect(created.text).toBe("prefer dark mode");
    expect((await client.memory.patch(created.id, { text: "prefer light mode" })).text).toBe("prefer light mode");
    expect((await client.memory.confirm(created.id)).status).toBe("active");
    expect((await client.memory.reject(created.id)).status).toBe("rejected");
    await client.memory.delete(created.id);
  });

  it("refuses a malformed memory id before spending a token", async () => {
    const fake = createFakeServer();
    let tokens = 0;
    const client = createClient({
      baseUrl: fake.baseUrl,
      fetch: fake.fetch,
      getContextToken: () => {
        tokens += 1;
        return fake.issueToken({ session_id: "ses-1" });
      },
    });
    await expect(client.memory.confirm("not-an-id")).rejects.toMatchObject({ code: "validation_failed" });
    expect(tokens).toBe(0);
  });
});
