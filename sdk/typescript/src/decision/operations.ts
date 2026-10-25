import type { HttpMethod } from "../operations.js";

// The operations of the decision area, keyed by the operationId of schemas/api/paths/decision.yaml. Only the read-only status
// is spoken: the operator-only configuration (getDecisionConfig, putDecisionConfig) is not part of the SDK surface. The table
// is separate from src/operations.ts because that one is closed over the operations of the first SDK version; the decision
// client sends its one request itself (client.ts) instead of through the shared transport for the same reason.

export interface DecisionOperationSpec {
  readonly method: HttpMethod;
  /** The OpenAPI path template. */
  readonly path: string;
  /** The status needs a context token. */
  readonly authenticated: true;
  /** A read: sending it again is always safe, so a transport failure is `unavailable` or `timeout`. */
  readonly safe: true;
}

export const DECISION_OPERATIONS = {
  getDecisionStatus: { method: "GET", path: "/v1/decision/status", authenticated: true, safe: true },
} as const satisfies Record<string, DecisionOperationSpec>;

export type DecisionOperationId = keyof typeof DECISION_OPERATIONS;

/** The exact capability the context token must carry to read the status. Without it the call is `permission_denied`. */
export const DECISION_STATUS_CAPABILITY = "decision.status.read";
