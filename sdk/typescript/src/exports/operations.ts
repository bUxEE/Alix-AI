import type { HttpMethod } from "../operations.js";

/** Downloadable exports from verified turn blocks (1.2.0). */
export interface ExportsOperationSpec {
  readonly method: HttpMethod;
  readonly path: string;
  readonly safe: boolean;
}

export const EXPORTS_OPERATIONS = {
  createExport: { method: "POST", path: "/v1/sessions/{session_id}/exports", safe: false },
} as const satisfies Record<string, ExportsOperationSpec>;

export type ExportsOperationId = keyof typeof EXPORTS_OPERATIONS;
