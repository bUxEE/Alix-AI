import { describe, expect, it } from "vitest";

import { createClient } from "../src/client.js";
import { EXPORTS_OPERATIONS } from "../src/exports/index.js";
import { OPERATIONS } from "../src/operations.js";
import { FAKE_EXPORTS_SCOPES, createFakeServer } from "../src/testing/index.js";
import { loadSpec } from "./helpers/openapi.js";

describe("EXPORTS_OPERATIONS matches OpenAPI", () => {
  const spec = loadSpec();
  it("every operation exists in the document", () => {
    for (const operationId of Object.keys(EXPORTS_OPERATIONS)) {
      const declared = spec.operations.find((o) => o.operationId === operationId);
      expect(declared, `${operationId} is not in the OpenAPI document`).toBeDefined();
      expect(declared?.method).toBe(EXPORTS_OPERATIONS[operationId as keyof typeof EXPORTS_OPERATIONS].method);
      expect(declared?.path).toBe(EXPORTS_OPERATIONS[operationId as keyof typeof EXPORTS_OPERATIONS].path);
    }
  });
  it("covers the exports area", () => {
    const documented = spec.operations.filter((o) => o.area === "exports");
    expect(Object.keys(EXPORTS_OPERATIONS).sort()).toEqual(documented.map((o) => o.operationId).sort());
  });
  it("is merged into OPERATIONS", () => {
    expect(OPERATIONS.createExport.path).toBe(EXPORTS_OPERATIONS.createExport.path);
  });
});

describe("client.exports against the fake", () => {
  it("creates a generated FileRef and refuses a missing turn", async () => {
    const fake = createFakeServer();
    const client = createClient({
      baseUrl: fake.baseUrl,
      fetch: fake.fetch,
      getContextToken: () => fake.issueToken({ session_id: "ses-1", capability_scopes: [...FAKE_EXPORTS_SCOPES] }),
    });
    const ref = await client.exports.create("ses-1", { turnId: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", format: "md" });
    expect(ref.origin).toBe("generated");
    expect(ref.mime_type).toBe("text/markdown");
    await expect(client.exports.create("ses-1", { turn_id: "trn_01ARZ3NDEKTSV4RRFFQ69G5FA0", format: "csv" })).rejects.toMatchObject({
      code: "validation_failed",
    });
    const denied = createClient({
      baseUrl: fake.baseUrl,
      fetch: fake.fetch,
      getContextToken: () => fake.issueToken({ session_id: "ses-1", capability_scopes: [] }),
    });
    await expect(denied.exports.create("ses-1", { turn_id: "trn_01ARZ3NDEKTSV4RRFFQ69G5FAV", format: "html" })).rejects.toMatchObject({
      code: "permission_denied",
    });
  });
});
