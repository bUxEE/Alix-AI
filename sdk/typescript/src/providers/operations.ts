import type { HttpMethod } from "../operations.js";

// The operations of the providers and OAuth areas, keyed by the operationId of schemas/api/paths/providers.yaml and oauth.yaml. The table
// is separate from src/operations.ts because that one is closed over the operations of the first SDK version; the providers client
// sends its requests itself (client.ts) for the same reason. A test keeps this table in step with the OpenAPI document.

export interface ProvidersOperationSpec {
  readonly method: HttpMethod;
  /** The OpenAPI path template. */
  readonly path: string;
  /** True for a read: sending it again is always safe, so a transport failure is `unavailable` or `timeout`; for anything else it is `uncertain_outcome`. */
  readonly safe: boolean;
}

export const PROVIDERS_OPERATIONS = {
  listProviders: { method: "GET", path: "/v1/providers", safe: true },
  putProviderCredential: { method: "PUT", path: "/v1/providers/{provider}/credential", safe: false },
  deleteProviderConnection: { method: "DELETE", path: "/v1/providers/{provider}/connection", safe: false },
  ackProviderBillingNotice: { method: "POST", path: "/v1/providers/{provider}/billing-notice/ack", safe: false },
  getProviderDisclosure: { method: "GET", path: "/v1/providers/{provider}/disclosure", safe: true },
  getProviderMode: { method: "GET", path: "/v1/providers/mode", safe: true },
  putProviderMode: { method: "PUT", path: "/v1/providers/mode", safe: false },
  getProviderEgressPolicy: { method: "GET", path: "/v1/providers/egress-policy", safe: true },
  putProviderEgressPolicy: { method: "PUT", path: "/v1/providers/egress-policy", safe: false },
  getProviderBudgets: { method: "GET", path: "/v1/providers/budgets", safe: true },
  putProviderBudget: { method: "PUT", path: "/v1/providers/budgets", safe: false },
  exportProviderUsage: { method: "GET", path: "/v1/providers/usage/export", safe: true },
  getProviderKillSwitch: { method: "GET", path: "/v1/providers/kill-switch", safe: true },
  putProviderKillSwitch: { method: "PUT", path: "/v1/providers/kill-switch", safe: false },
  startProviderOAuth: { method: "POST", path: "/v1/oauth/{provider}/start", safe: false },
  completeProviderOAuth: { method: "POST", path: "/v1/oauth/callback", safe: false },
  getProviderOAuthFlow: { method: "GET", path: "/v1/oauth/flows/{flow_id}", safe: true },
  cancelProviderOAuthFlow: { method: "DELETE", path: "/v1/oauth/flows/{flow_id}", safe: false },
} as const satisfies Record<string, ProvidersOperationSpec>;

export type ProvidersOperationId = keyof typeof PROVIDERS_OPERATIONS;
