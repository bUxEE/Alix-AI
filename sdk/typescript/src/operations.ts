// The operations of the public API the SDK speaks, keyed by the operationId of schemas/api/openapi.yaml. This table is the single place
// that knows a method and a path: the client builds its requests from it and the fake server (src/testing) routes by it. The RAG area
// keeps its own table (src/rag/operations.ts) and the transport reads both through `operationSpec`. test/openapi-contract reads the
// OpenAPI document and fails when a row here and the document disagree.

import { EXPORTS_OPERATIONS } from "./exports/operations.js";
import type { ExportsOperationId } from "./exports/operations.js";
import { ME_OPERATIONS } from "./me/operations.js";
import type { MeOperationId } from "./me/operations.js";
import { MEMORY_OPERATIONS } from "./memory/operations.js";
import type { MemoryOperationId } from "./memory/operations.js";
import { RAG_OPERATIONS } from "./rag/operations.js";
import type { RagOperationId } from "./rag/operations.js";

export type HttpMethod = "GET" | "POST" | "PUT" | "PATCH" | "DELETE";

export interface OperationSpec {
  readonly method: HttpMethod;
  /** The OpenAPI path template, with `{name}` placeholders. */
  readonly path: string;
  /** False for the unauthenticated probes: no context token is requested and none is sent. */
  readonly authenticated: boolean;
  /**
   * True when sending the request again is always safe, so that a transport failure is `unavailable` or `timeout` and not
   * `uncertain_outcome`. Absent means "a read is safe, anything else may have been carried out"; set it for a POST that only reads.
   */
  readonly safe?: boolean;
}

/** Area tables (me, memory) carry `safe` but not `authenticated`; every route there needs a context token. */
const areaOps = <T extends Record<string, { readonly method: HttpMethod; readonly path: string; readonly safe: boolean }>>(
  table: T,
): { [K in keyof T]: OperationSpec } =>
  Object.fromEntries(
    Object.entries(table).map(([id, op]) => [id, { method: op.method, path: op.path, authenticated: true, ...(op.safe ? { safe: true } : {}) }]),
  ) as { [K in keyof T]: OperationSpec };

export const OPERATIONS = {
  getLiveness: { method: "GET", path: "/healthz", authenticated: false },
  getReadiness: { method: "GET", path: "/readyz", authenticated: false },
  getHealth: { method: "GET", path: "/v1/health", authenticated: true },
  createSession: { method: "POST", path: "/v1/sessions", authenticated: true },
  getSession: { method: "GET", path: "/v1/sessions/{session_id}", authenticated: true },
  endSession: { method: "DELETE", path: "/v1/sessions/{session_id}", authenticated: true },
  getPendingState: { method: "GET", path: "/v1/sessions/{session_id}/pending", authenticated: true },
  sendTurn: { method: "POST", path: "/v1/sessions/{session_id}/turns", authenticated: true },
  streamTurnEvents: { method: "GET", path: "/v1/sessions/{session_id}/turns/{turn_id}/events", authenticated: true },
  getAction: { method: "GET", path: "/v1/actions/{preparation_id}", authenticated: true },
  confirmAction: { method: "POST", path: "/v1/actions/{preparation_id}/confirm", authenticated: true },
  rejectAction: { method: "POST", path: "/v1/actions/{preparation_id}/reject", authenticated: true },
  getActionReceipt: { method: "GET", path: "/v1/actions/{preparation_id}/receipt", authenticated: true },
  registerApplication: { method: "POST", path: "/v1/applications", authenticated: true },
  getApplication: { method: "GET", path: "/v1/applications/{application_id}", authenticated: true },
  listCapabilities: { method: "GET", path: "/v1/capabilities", authenticated: true },
  uploadFile: { method: "POST", path: "/v1/files", authenticated: true },
  getFile: { method: "GET", path: "/v1/files/{file_id}", authenticated: true },
  deleteFile: { method: "DELETE", path: "/v1/files/{file_id}", authenticated: true },
  downloadFile: { method: "GET", path: "/v1/files/{file_id}/content", authenticated: true },
  previewFile: { method: "GET", path: "/v1/files/{file_id}/preview", authenticated: true },
  listHistorySegments: { method: "GET", path: "/v1/history/sessions/{session_id}/segments", authenticated: true },
  clearHistorySession: { method: "DELETE", path: "/v1/history/sessions/{session_id}", authenticated: true },
  searchHistory: { method: "POST", path: "/v1/history/search", authenticated: true, safe: true },
  exportHistory: { method: "POST", path: "/v1/history/export", authenticated: true, safe: true },
  cleanupHistory: { method: "POST", path: "/v1/history/cleanup", authenticated: true },
  getUserSettings: { method: "GET", path: "/v1/settings/user", authenticated: true },
  putUserSettings: { method: "PUT", path: "/v1/settings/user", authenticated: true },
  getTenantSettings: { method: "GET", path: "/v1/settings/tenant", authenticated: true },
  putTenantSettings: { method: "PUT", path: "/v1/settings/tenant", authenticated: true },
  getDeploymentSettings: { method: "GET", path: "/v1/settings/deployment", authenticated: true },
  putDeploymentSettings: { method: "PUT", path: "/v1/settings/deployment", authenticated: true },
  getEffectiveSettings: { method: "GET", path: "/v1/settings/effective", authenticated: true },
  listSettingsProfiles: { method: "GET", path: "/v1/settings/profiles", authenticated: true },
  getSettingsCapabilities: { method: "GET", path: "/v1/settings/capabilities", authenticated: true },
  getSettingsIntegrationAudit: { method: "GET", path: "/v1/settings/integration-audit", authenticated: true },
  getSettingsProviderHealth: { method: "GET", path: "/v1/settings/providers", authenticated: true },
  ...areaOps(ME_OPERATIONS),
  ...areaOps(MEMORY_OPERATIONS),
  ...areaOps(EXPORTS_OPERATIONS),
} as const satisfies Record<string, OperationSpec>;

/** The operations the fake server implements: everything but the RAG area, which has its own table. */
export type CoreOperationId = keyof typeof OPERATIONS;

/** Every operation the SDK can send, the RAG area (src/rag) included. */
export type OperationId = CoreOperationId | RagOperationId;

/** The method, path and authentication of any operation the SDK sends, whichever table declares it. */
export const operationSpec = (operation: OperationId): OperationSpec =>
  operation in OPERATIONS ? OPERATIONS[operation as CoreOperationId] : RAG_OPERATIONS[operation as RagOperationId];

/** Re-export area ids so callers can narrow without importing the area modules. */
export type { ExportsOperationId, MeOperationId, MemoryOperationId };

/** Whether sending the operation again is always safe: a read, or a POST that only reads. */
export const isSafeToRepeat = (operation: OperationId): boolean => {
  const spec = operationSpec(operation);
  return spec.safe ?? spec.method === "GET";
};

/** Fills the placeholders of a path template. The values must already be validated identifiers. */
export const fillPath = (template: string, params: Readonly<Record<string, string>>): string =>
  template.replace(/\{([a-z_]+)\}/g, (_, name: string) => encodeURIComponent(params[name] ?? ""));

/** Matches a request path against an operation's template; returns the placeholder values or undefined. */
export const matchPath = (template: string, path: string): Record<string, string> | undefined => {
  // split() with a capture group alternates literal text (even indexes) and placeholder names (odd).
  const parts = template.split(/\{([a-z_]+)\}/);
  const names: string[] = [];
  const source = parts
    .map((part, index) => {
      if (index % 2 === 0) return part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
      names.push(part);
      return "([^/]+)";
    })
    .join("");
  const found = new RegExp(`^${source}$`).exec(path);
  if (found === null) return undefined;
  const params: Record<string, string> = {};
  names.forEach((name, index) => {
    params[name] = decodeURIComponent(found[index + 1] ?? "");
  });
  return params;
};
