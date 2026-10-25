import { createHash } from "node:crypto";

import type { ErrorCode } from "./errors.ts";

// The host members of a DeterministicResult (schemas/actions/deterministic-
// result). `operation` and `capability_id` are the core's: it knows which call
// it made, so a host never states them.

export type CanonicalValue =
  | { kind: "text"; value: string }
  | { kind: "integer"; value: number }
  | { kind: "boolean"; value: boolean }
  | { kind: "date"; value: string }
  | { kind: "datetime"; value: string; zone?: string }
  | { kind: "identifier"; value: string };

export type Fact = { field: string; value: CanonicalValue; display?: string };

export type RecordRef = { entity_type: string; entity_id: string };

export type Issue = { code: ErrorCode; field?: string; message: string };

export type Freshness = { as_of: string; version: string };

export type NextAction = {
  action: "confirm" | "reject" | "cancel" | "modify" | "refresh" | "retry" | "continue_in_app" | "invoke_capability";
  capability_id?: string;
  label?: string;
};

export type Effect = {
  operation: "create" | "update" | "delete" | "send" | "other";
  entity_type: string;
  entity_id?: string;
  description: string;
  changes: Fact[];
  external: boolean;
};

export type DataVersion = { entity_type: string; entity_id: string; version: string };

export type Preparation = {
  summary: string;
  effects: Effect[];
  data_versions: DataVersion[];
  declared_risk_level: 0 | 1 | 2 | 3;
  valid_for_seconds?: number;
};

export type FollowUp = {
  reference: string;
  state: "queued" | "running" | "succeeded" | "failed" | "unknown";
  as_of?: string;
};

export type HostResult = {
  status: "ok" | "rejected" | "queued" | "partial" | "uncertain" | "unavailable";
  identifiers: RecordRef[];
  values: Fact[];
  errors: Issue[];
  freshness?: Freshness;
  allowed_next_actions: NextAction[];
  follow_up?: FollowUp;
  preparation?: Preparation;
};

// ---- canonical values ------------------------------------------------------

export const text = (field: string, value: string, display?: string): Fact => ({
  field,
  value: { kind: "text", value },
  ...(display === undefined ? {} : { display }),
});

export const integer = (field: string, value: number): Fact => ({ field, value: { kind: "integer", value } });

export const bool = (field: string, value: boolean): Fact => ({ field, value: { kind: "boolean", value } });

export const date = (field: string, value: string): Fact => ({ field, value: { kind: "date", value } });

export const datetime = (field: string, value: string, zone: string, display: string): Fact => ({
  field,
  value: { kind: "datetime", value, zone },
  display,
});

export const identifier = (field: string, value: string): Fact => ({
  field,
  value: { kind: "identifier", value },
});

export const record = (entityType: string, entityId: string): RecordRef => ({
  entity_type: entityType,
  entity_id: entityId,
});

// ---- results ---------------------------------------------------------------

export const okResult = (parts: {
  identifiers?: RecordRef[];
  values?: Fact[];
  freshness: Freshness;
  allowed_next_actions?: NextAction[];
  follow_up?: FollowUp;
  preparation?: Preparation;
}): HostResult => ({
  status: "ok",
  identifiers: parts.identifiers ?? [],
  values: parts.values ?? [],
  errors: [],
  freshness: parts.freshness,
  allowed_next_actions: parts.allowed_next_actions ?? [],
  ...(parts.follow_up === undefined ? {} : { follow_up: parts.follow_up }),
  ...(parts.preparation === undefined ? {} : { preparation: parts.preparation }),
});

export const rejectedResult = (
  code: ErrorCode,
  message: string,
  extra: { field?: string; freshness?: Freshness; allowed_next_actions?: NextAction[] } = {},
): HostResult => ({
  status: "rejected",
  identifiers: [],
  values: [],
  errors: [{ code, ...(extra.field === undefined ? {} : { field: extra.field }), message }],
  ...(extra.freshness === undefined ? {} : { freshness: extra.freshness }),
  allowed_next_actions: extra.allowed_next_actions ?? [],
});

// A dependency is down or the host failed in a way it cannot attribute to the
// request. Nothing was applied: the host only reports this from a rolled back
// transaction or before it started one.
export const unavailableResult = (code: "unavailable" | "internal", message: string): HostResult => ({
  status: "unavailable",
  identifiers: [],
  values: [],
  errors: [{ code, message }],
  allowed_next_actions: [{ action: "retry" }],
});

// ---- data versions ---------------------------------------------------------

// The version of one record is the row's own revision counter, which changes
// exactly when that record changes. It is never a timestamp, a response ETag or
// a counter shared by a site or a tenant.
export const recordVersion = (entityType: string, entityId: string, revision: number): DataVersion => ({
  entity_type: entityType,
  entity_id: entityId,
  version: String(revision),
});

// The snapshot version a result reports in `freshness.version`: a digest over
// the version of every record the result relied on, in a fixed order. Prepare
// and Validate compute it with this one function over the same records, so on
// unchanged data they agree, and any change to any of those records (and to
// nothing else) changes it.
export const snapshotVersion = (versions: DataVersion[]): string => {
  const lines = versions
    .map((v) => `${v.entity_type}:${v.entity_id}:${v.version}`)
    .sort()
    .join("\n");
  return `rv1-${createHash("sha256").update(lines).digest("hex").slice(0, 32)}`;
};
