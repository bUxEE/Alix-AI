import type { ErrorBody, ErrorCode } from "./types.js";

// What the fake server hands to the modules that implement one area of it (files, history, settings): its clock, its way of building
// a JSON answer or an error document, and the scope of the request. Keeping them apart keeps fake-server.ts about the protocol and
// each area about its own rules.

/** The scope a verified context token carries: every request is bound to all of it. */
export interface FakeScope {
  application_id: string;
  tenant_id: string;
  workspace_id: string;
  principal_id: string;
  session_id: string;
  capability_scopes: string[];
}

export type Json = Record<string, unknown>;

export interface FakeContext {
  json: (status: number, body: unknown, headers?: Record<string, string>) => Response;
  problem: (code: ErrorCode, message: string, extra?: { status?: number; details?: Json }) => Response;
  /** A 403 `scope_violation` for a path id that is not the token's (or is not visible to it). */
  levelViolation: (level: string) => Response;
  nowMs: () => number;
  iso: (ms: number) => string;
  /** The refusal for an application whose package is not active, or undefined when it is. */
  quarantine: () => Response | undefined;
}

/** A 403 `permission_denied` unless the token lists the capability (exact match: there is no wildcard). */
export const requireCapability = (context: FakeContext, scope: FakeScope, capability: string): Response | undefined =>
  scope.capability_scopes.includes(capability)
    ? undefined
    : context.problem("permission_denied", `the context token does not carry the capability ${capability}`, { details: { capability } });

/** The whole scope of a record, for comparisons: application, tenant, workspace, principal. */
export const sameOwner = (a: FakeScope, b: FakeScope): boolean =>
  a.application_id === b.application_id && a.tenant_id === b.tenant_id && a.workspace_id === b.workspace_id && a.principal_id === b.principal_id;

export const sameWorkspace = (a: FakeScope, b: FakeScope): boolean =>
  a.application_id === b.application_id && a.tenant_id === b.tenant_id && a.workspace_id === b.workspace_id;

export type { ErrorBody, ErrorCode };

/** The capabilities a context token carries to use the history area (each operation needs its own; there is no wildcard). */
export const FAKE_HISTORY_SCOPES: readonly string[] = ["history.read", "history.search", "history.clear", "history.export", "history.cleanup"];

/** The capability a context token carries to create a downloadable export from verified turn blocks. */
export const FAKE_EXPORTS_SCOPES: readonly string[] = ["exports.create"];

/** The capabilities a context token carries to read and write the settings of every level. */
export const FAKE_SETTINGS_SCOPES: readonly string[] = [
  "settings.user.read",
  "settings.user.write",
  "settings.tenant.read",
  "settings.tenant.write",
  "settings.deployment.read",
  "settings.deployment.write",
];
