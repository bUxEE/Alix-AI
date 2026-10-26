import type { HttpMethod } from "../operations.js";

// Dictation HTTP (record-to-text, 1.1.0): GET /v1/dictations/capabilities and POST /v1/sessions/{session_id}/dictations.
// Separate from the voice WebSocket and from the core OPERATIONS table (that table is closed over the first SDK version).
// A test keeps this table in step with schemas/api/paths/voice.yaml.

export interface DictationOperationSpec {
  readonly method: HttpMethod;
  readonly path: string;
  /** True for a read: a transport failure is `unavailable` / `timeout`; otherwise `uncertain_outcome`. */
  readonly safe: boolean;
}

export const DICTATION_OPERATIONS = {
  getDictationCapabilities: { method: "GET", path: "/v1/dictations/capabilities", safe: true },
  createSessionDictation: { method: "POST", path: "/v1/sessions/{session_id}/dictations", safe: false },
} as const satisfies Record<string, DictationOperationSpec>;

export type DictationOperationId = keyof typeof DICTATION_OPERATIONS;
