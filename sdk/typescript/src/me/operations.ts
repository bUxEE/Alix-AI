import type { HttpMethod } from "../operations.js";

/** Per-principal LLM account and catalog (ADR 0041, 1.2.0). */
export interface MeOperationSpec {
  readonly method: HttpMethod;
  readonly path: string;
  readonly safe: boolean;
}

export const ME_OPERATIONS = {
  getProviderCatalog: { method: "GET", path: "/v1/providers/catalog", safe: true },
  getPrincipalLlm: { method: "GET", path: "/v1/me/llm", safe: true },
  putPrincipalLlmRole: { method: "PUT", path: "/v1/me/llm/{role}", safe: false },
  deletePrincipalLlmRole: { method: "DELETE", path: "/v1/me/llm/{role}", safe: false },
  putPrincipalProviderCredential: { method: "PUT", path: "/v1/me/providers/{provider}/credential", safe: false },
  deletePrincipalProviderCredential: { method: "DELETE", path: "/v1/me/providers/{provider}/credential", safe: false },
  ackPrincipalProviderBillingNotice: { method: "POST", path: "/v1/me/providers/{provider}/billing-notice/ack", safe: false },
  startPrincipalOAuth: { method: "POST", path: "/v1/me/oauth/{provider}/start", safe: false },
} as const satisfies Record<string, MeOperationSpec>;

export type MeOperationId = keyof typeof ME_OPERATIONS;
