import type { DecisionStatus, DecisionStatusMode, FallbackReason } from "./types.js";

// The response guard of the decision status, in the style of src/guards.ts. The status is a closed document
// (additionalProperties false at both levels), so a member the schema does not have is refused: a server that adds a score, a
// threshold or any authority datum to it is not the server this SDK speaks to, and the host never sees such a member as if it
// were part of the status. The members that tie the numbers together are checked too: `rejected_count` is always the sum of
// `fallback_counts`, one entry per reason, so a status that contradicts itself is an error and never a result.

const MODES = ["off", "shadow", "advisory"] as const satisfies readonly DecisionStatusMode[];

/** Every fallback reason of schemas/decision/fallback-reason.schema.json. An unknown reason is refused. */
export const FALLBACK_REASONS = [
  "disabled",
  "unavailable",
  "timeout",
  "malformed_output",
  "low_confidence",
  "calibration_failed",
  "ood_unsupported_shape",
  "model_mismatch",
  "tenant_mismatch",
  "policy_conflict",
  "candidate_set_rejected",
  "resource_gate",
  "unsupported_runtime",
] as const satisfies readonly FallbackReason[];

// Compile-time check that the list above is every FallbackReason: a reason the schema gains makes this stop compiling.
type MissingReasons = Exclude<FallbackReason, (typeof FALLBACK_REASONS)[number]>;
export const fallbackReasonsAreExhaustive: [MissingReasons] extends [never] ? true : never = true;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const closed = (value: Record<string, unknown>, known: readonly string[]): boolean => Object.keys(value).every((key) => known.includes(key));

const count = (value: unknown): value is number => typeof value === "number" && Number.isInteger(value) && value >= 0;

// Identifier (schemas/common/ids): 1 to 128 characters, no whitespace, no control character, no '*'.
const IDENTIFIER = /^[^\u0000-\u0020\u007f-\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000*]{1,128}$/;
// OpaqueVersion (schemas/actions/shared): 1 to 256 characters, no whitespace or control character.
const OPAQUE_VERSION = /^[^\u0000-\u0020\u007f]{1,256}$/;
// Timestamp (schemas/common/timestamps): RFC 3339 in UTC with a literal Z.
const TIMESTAMP = /^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{1,9})?Z$/;

const matches = (pattern: RegExp, value: unknown): value is string => typeof value === "string" && pattern.test(value);

const isFallbackCount = (value: unknown): value is { reason: FallbackReason; count: number } =>
  isRecord(value) &&
  closed(value, ["reason", "count"]) &&
  typeof value["reason"] === "string" &&
  (FALLBACK_REASONS as readonly string[]).includes(value["reason"]) &&
  count(value["count"]);

/** True when `value` is a DecisionStatus: every required member, no member the schema does not have, and the counters consistent. */
export const isDecisionStatus = (value: unknown): value is DecisionStatus => {
  if (!isRecord(value)) return false;
  if (!closed(value, ["mode", "provider_id", "model_id", "model_revision", "accepted_count", "rejected_count", "fallback_counts", "since", "generated_at"])) {
    return false;
  }
  if (typeof value["mode"] !== "string" || !(MODES as readonly string[]).includes(value["mode"])) return false;
  if (!matches(IDENTIFIER, value["provider_id"])) return false;
  if ("model_id" in value && !matches(IDENTIFIER, value["model_id"])) return false;
  if ("model_revision" in value && !matches(OPAQUE_VERSION, value["model_revision"])) return false;
  if (!count(value["accepted_count"]) || !count(value["rejected_count"])) return false;
  if (!matches(TIMESTAMP, value["since"]) || !matches(TIMESTAMP, value["generated_at"])) return false;
  const entries = value["fallback_counts"];
  if (!Array.isArray(entries) || entries.length > FALLBACK_REASONS.length || !entries.every(isFallbackCount)) return false;
  if (new Set(entries.map((entry) => entry.reason)).size !== entries.length) return false;
  return entries.reduce((total, entry) => total + entry.count, 0) === value["rejected_count"];
};
