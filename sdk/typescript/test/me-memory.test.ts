import { describe, expect, it } from "vitest";

import { ME_OPERATIONS } from "../src/me/index.js";
import { MEMORY_OPERATIONS } from "../src/memory/index.js";
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
