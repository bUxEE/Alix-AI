// The platform error taxonomy of schemas/common/error.schema.json. The host
// answers with these codes only, so a failure is always typed.
export const ERROR_CODES = [
  "scope_missing",
  "unauthenticated",
  "permission_denied",
  "scope_violation",
  "schema_invalid",
  "unknown_capability",
  "ambiguous",
  "missing_fields",
  "validation_failed",
  "stale",
  "expired",
  "replayed",
  "confirmation_required",
  "step_up_required",
  "conflict",
  "unavailable",
  "degraded",
  "timeout",
  "rate_limited",
  "resource_exhausted",
  "no_supported_evidence",
  "uncertain_outcome",
  "partial_failure",
  "quarantined",
  "internal",
] as const;

export type ErrorCode = (typeof ERROR_CODES)[number];

// A refusal is how domain code stops early. It is thrown inside the domain and
// turned into a typed `rejected` result at the edge; it never leaves the app as
// an exception.
export type Refusal = Error & {
  refusal: { code: ErrorCode; message: string; field?: string; freshness?: { as_of: string; version: string } };
};

export const refuse = (
  code: ErrorCode,
  message: string,
  extra: { field?: string; freshness?: { as_of: string; version: string } } = {},
): Refusal => {
  const error = new Error(message) as Refusal;
  error.refusal = { code, message, ...extra };
  return error;
};

export const isRefusal = (value: unknown): value is Refusal =>
  value instanceof Error && typeof (value as Partial<Refusal>).refusal === "object";
