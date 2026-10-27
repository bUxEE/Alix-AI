import type { HttpMethod } from "../operations.js";

/** Operations area: erasure (1.3.0), private LLM (1.4.0), tenant analyst (1.5.0, ADR 0047). Operator only. */
export interface OpsOperationSpec {
  readonly method: HttpMethod;
  readonly path: string;
  readonly safe: boolean;
}

export const OPS_OPERATIONS = {
  startErasure: { method: "POST", path: "/v1/ops/erasure", safe: false },
  getErasureJob: { method: "GET", path: "/v1/ops/erasure/{job_id}", safe: true },
  putPrivateLlmEndpoint: { method: "PUT", path: "/v1/ops/private-llm/endpoints/{endpoint_id}", safe: false },
  getPrivateLlmEndpoint: { method: "GET", path: "/v1/ops/private-llm/endpoints/{endpoint_id}", safe: true },
  deletePrivateLlmEndpoint: { method: "DELETE", path: "/v1/ops/private-llm/endpoints/{endpoint_id}", safe: false },
  putTenantPrivateLlm: { method: "PUT", path: "/v1/ops/tenants/{tenant_id}/private-llm", safe: false },
  getTenantPrivateLlm: { method: "GET", path: "/v1/ops/tenants/{tenant_id}/private-llm", safe: true },
  deleteTenantPrivateLlm: { method: "DELETE", path: "/v1/ops/tenants/{tenant_id}/private-llm", safe: false },
  putPrincipalPrivateLlm: { method: "PUT", path: "/v1/ops/principals/{principal_id}/private-llm", safe: false },
  getPrincipalPrivateLlm: { method: "GET", path: "/v1/ops/principals/{principal_id}/private-llm", safe: true },
  deletePrincipalPrivateLlm: { method: "DELETE", path: "/v1/ops/principals/{principal_id}/private-llm", safe: false },
  getTenantAnalyst: { method: "GET", path: "/v1/ops/tenants/{tenant_id}/analyst", safe: true },
  putTenantAnalyst: { method: "PUT", path: "/v1/ops/tenants/{tenant_id}/analyst", safe: false },
} as const satisfies Record<string, OpsOperationSpec>;

export type OpsOperationId = keyof typeof OPS_OPERATIONS;
