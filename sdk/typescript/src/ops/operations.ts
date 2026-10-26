import type { HttpMethod } from "../operations.js";

/** Operations area: erasure over HTTP (1.3.0, ADR 0036). Operator only. */
export interface OpsOperationSpec {
  readonly method: HttpMethod;
  readonly path: string;
  readonly safe: boolean;
}

export const OPS_OPERATIONS = {
  startErasure: { method: "POST", path: "/v1/ops/erasure", safe: false },
  getErasureJob: { method: "GET", path: "/v1/ops/erasure/{job_id}", safe: true },
} as const satisfies Record<string, OpsOperationSpec>;

export type OpsOperationId = keyof typeof OPS_OPERATIONS;
