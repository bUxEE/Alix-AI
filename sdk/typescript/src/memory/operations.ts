import type { HttpMethod } from "../operations.js";

/** Long-term memory (ADR 0042, 1.2.0). */
export interface MemoryOperationSpec {
  readonly method: HttpMethod;
  readonly path: string;
  readonly safe: boolean;
}

export const MEMORY_OPERATIONS = {
  listMemory: { method: "GET", path: "/v1/memory", safe: true },
  createMemory: { method: "POST", path: "/v1/memory", safe: false },
  patchMemory: { method: "PATCH", path: "/v1/memory/{id}", safe: false },
  deleteMemory: { method: "DELETE", path: "/v1/memory/{id}", safe: false },
  confirmMemory: { method: "POST", path: "/v1/memory/{id}/confirm", safe: false },
  rejectMemory: { method: "POST", path: "/v1/memory/{id}/reject", safe: false },
} as const satisfies Record<string, MemoryOperationSpec>;

export type MemoryOperationId = keyof typeof MEMORY_OPERATIONS;
