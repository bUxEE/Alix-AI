import type { HttpMethod } from "../operations.js";

// The operations of the RAG area, keyed by the operationId of schemas/api/paths/rag.yaml. The table is
// the single place of this namespace that knows a method and a path, as src/operations.ts is for the
// rest of the API. It is separate from that table because that one is closed over the operations of
// the first SDK version. A contract test that reads the OpenAPI document and fails when a row here and
// the document disagree is proposed in the M2-11 report (it belongs in sdk/typescript/test, outside
// the write scope of that task).

export interface RagOperationSpec {
  readonly method: HttpMethod;
  /** The OpenAPI path template, with `{name}` placeholders. */
  readonly path: string;
  /** Every operation of this area needs a context token. */
  readonly authenticated: true;
  /**
   * True when sending the request again is always safe: a read, and the two operations that only read
   * (search and answer). A transport failure of such a request is `unavailable` or `timeout`; of any
   * other it is `uncertain_outcome`, because the request may have been carried out.
   */
  readonly safe: boolean;
}

export const RAG_OPERATIONS = {
  registerRagSource: { method: "POST", path: "/v1/rag/sources", authenticated: true, safe: false },
  listRagSources: { method: "GET", path: "/v1/rag/sources", authenticated: true, safe: true },
  reauditRagSources: { method: "POST", path: "/v1/rag/sources/reaudit", authenticated: true, safe: false },
  getRagSource: { method: "GET", path: "/v1/rag/sources/{source_id}", authenticated: true, safe: true },
  removeRagSource: { method: "DELETE", path: "/v1/rag/sources/{source_id}", authenticated: true, safe: false },
  syncRagSource: { method: "POST", path: "/v1/rag/sources/{source_id}/sync", authenticated: true, safe: false },
  rebuildRagSource: { method: "POST", path: "/v1/rag/sources/{source_id}/rebuild", authenticated: true, safe: false },
  listRagGenerations: { method: "GET", path: "/v1/rag/sources/{source_id}/generations", authenticated: true, safe: true },
  invalidateRagGeneration: {
    method: "POST",
    path: "/v1/rag/sources/{source_id}/generations/{generation_id}/invalidate",
    authenticated: true,
    safe: false,
  },
  searchRag: { method: "POST", path: "/v1/rag/search", authenticated: true, safe: true },
  answerRag: { method: "POST", path: "/v1/rag/answer", authenticated: true, safe: true },
  resolveRagCitation: { method: "GET", path: "/v1/rag/citations/{file_id}", authenticated: true, safe: true },
} as const satisfies Record<string, RagOperationSpec>;

export type RagOperationId = keyof typeof RAG_OPERATIONS;

