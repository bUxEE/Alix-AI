import type { ActionReceipt, PreparedAction } from "./gen/actions/index.js";
import type { ErrorCode } from "./gen/common/index.js";

// Every failure the SDK reports is an AlixAIError carrying one of the closed ErrCode strings of
// schemas/common/error (pkg/contracts). A failure that never reached the server (a malformed
// argument, a host token provider that threw, an unreadable response) uses the same strings, so a
// host application has one vocabulary to switch on.

/** Every ErrorCode, in the order of schemas/common/error. A test keeps it in step with the schema. */
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
  "out_of_domain",
  "ability_disabled",
  "model_not_analyst_capable",
  "insufficient_data",
  "web_query_refused",
  "analyst_cap_exceeded",
  "unit_mismatch",
  "forecast_unavailable",
  "domain_classification_failed",
  "internal",
] as const satisfies readonly ErrorCode[];

// Compile-time proof that ERROR_CODES lists every generated ErrorCode.
type MissingErrorCodes = Exclude<ErrorCode, (typeof ERROR_CODES)[number]>;
export const errorCodesAreExhaustive: [MissingErrorCodes] extends [never] ? true : never = true;

const ERROR_CODE_SET: ReadonlySet<string> = new Set(ERROR_CODES);

/** The codes pkg/contracts classifies as retryable (testdata/errcodes.golden). */
const RETRYABLE_CODES: ReadonlySet<ErrorCode> = new Set<ErrorCode>([
  "unavailable",
  "degraded",
  "timeout",
  "rate_limited",
  "resource_exhausted",
]);

export const isErrorCode = (value: unknown): value is ErrorCode =>
  typeof value === "string" && ERROR_CODE_SET.has(value);

/** Whether a code is one for which re-sending the same request, after a delay, can succeed. */
export const isRetryableCode = (code: ErrorCode): boolean => RETRYABLE_CODES.has(code);

const BRAND = Symbol.for("alixai.sdk.error");

export interface AlixAIErrorInit {
  code: ErrorCode;
  message: string;
  /** Defaults to the classification of the code. */
  retryable?: boolean;
  /** The HTTP status, when the failure is an HTTP response. */
  status?: number;
  /** Structured, non-sensitive context from the server (an open map). */
  details?: Record<string, unknown>;
  /** The X-Request-ID the gateway minted, for support and log correlation. */
  requestId?: string;
  /** The server's Retry-After, in milliseconds. */
  retryAfterMs?: number;
  /** The receipt that came beside the error of a confirmation that did not commit. */
  receipt?: ActionReceipt;
  /** The preparation as it stood when a confirmation did not commit. */
  preparation?: PreparedAction;
  cause?: unknown;
}

/**
 * The one error type of the SDK. `code` is an ErrCode string; `retryable` says whether sending the
 * same request again can reasonably give a different answer. A request that may have reached the
 * server and whose outcome is unknown is `uncertain_outcome`, never retryable: read the state
 * (pending, receipt) before sending anything again. The message never carries a credential.
 */
export class AlixAIError extends Error {
  readonly code: ErrorCode;
  readonly retryable: boolean;
  readonly status: number | undefined;
  readonly details: Record<string, unknown> | undefined;
  readonly requestId: string | undefined;
  readonly retryAfterMs: number | undefined;
  readonly receipt: ActionReceipt | undefined;
  readonly preparation: PreparedAction | undefined;
  readonly alixaiError = BRAND;

  constructor(init: AlixAIErrorInit) {
    super(init.message, init.cause === undefined ? undefined : { cause: init.cause });
    this.name = "AlixAIError";
    this.code = init.code;
    this.retryable = init.retryable ?? isRetryableCode(init.code);
    this.status = init.status;
    this.details = init.details;
    this.requestId = init.requestId;
    this.retryAfterMs = init.retryAfterMs;
    this.receipt = init.receipt;
    this.preparation = init.preparation;
  }
}

/** True for an AlixAIError, also across two copies of the package (it checks a brand, not the class). */
export const isAlixAIError = (value: unknown): value is AlixAIError =>
  typeof value === "object" && value !== null && (value as { alixaiError?: unknown }).alixaiError === BRAND;

/** Builds an error for a failure that did not come from the server's error document. */
export const sdkError = (
  code: ErrorCode,
  message: string,
  extra: Omit<AlixAIErrorInit, "code" | "message"> = {},
): AlixAIError => new AlixAIError({ code, message, ...extra });

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

interface ErrorBody {
  code: ErrorCode;
  message: string;
  retryable: boolean;
  details?: Record<string, unknown>;
}

const asErrorBody = (value: unknown): ErrorBody | undefined => {
  if (!isRecord(value)) return undefined;
  const { code, message, retryable, details } = value;
  if (!isErrorCode(code) || typeof message !== "string" || typeof retryable !== "boolean") return undefined;
  if (details !== undefined && !isRecord(details)) return undefined;
  return details === undefined ? { code, message, retryable } : { code, message, retryable, details };
};

/** The code to report for an HTTP failure whose body is not an AlixAI error document (a proxy page). */
const codeForStatus = (status: number): ErrorCode => {
  switch (status) {
    case 400:
    case 413:
    case 415:
    case 422:
      return "validation_failed";
    case 401:
      return "unauthenticated";
    case 403:
      return "permission_denied";
    case 409:
      return "conflict";
    case 410:
      return "expired";
    case 429:
      return "rate_limited";
    case 502:
    case 503:
      return "unavailable";
    case 504:
      return "timeout";
    default:
      return "internal";
  }
};

/** Retry-After as milliseconds: delta-seconds or an HTTP date. */
export const parseRetryAfter = (value: string | null, now: number = Date.now()): number | undefined => {
  if (value === null) return undefined;
  const trimmed = value.trim();
  if (/^\d{1,9}$/.test(trimmed)) return Number(trimmed) * 1000;
  const at = Date.parse(trimmed);
  return Number.isNaN(at) ? undefined : Math.max(0, at - now);
};

const MAX_ERROR_BODY_CHARS = 64 * 1024;

/**
 * Turns a non-2xx response into an AlixAIError. The body is the bare Error document, or, for a
 * confirmation whose commit reached the host and did not succeed, the ActionOutcome with the
 * receipt beside an `error` member. Anything else (an HTML page from a proxy) becomes a typed
 * fallback chosen by the status; a JSON parse failure never escapes.
 */
export const errorFromResponse = async (response: Response): Promise<AlixAIError> => {
  let parsed: unknown;
  try {
    const text = await response.text();
    parsed = text.length > MAX_ERROR_BODY_CHARS ? undefined : JSON.parse(text);
  } catch {
    parsed = undefined;
  }
  return errorFromParsed(response, parsed);
};

/** The same, for a response whose body has already been read and parsed (undefined when it was not JSON). */
export const errorFromParsed = (response: Response, parsed: unknown): AlixAIError => {
  const requestId = response.headers.get("X-Request-ID") ?? undefined;
  const retryAfterMs = parseRetryAfter(response.headers.get("Retry-After"));
  const base = { status: response.status, requestId, retryAfterMs };

  if (isRecord(parsed) && isRecord(parsed["error"])) {
    // ActionOutcome: the receipt is the durable record, `error` says why it did not succeed.
    const body = asErrorBody(parsed["error"]);
    if (body !== undefined) {
      return new AlixAIError({
        ...base,
        code: body.code,
        message: body.message,
        retryable: body.retryable,
        details: body.details,
        receipt: isRecord(parsed["receipt"]) ? (parsed["receipt"] as unknown as ActionReceipt) : undefined,
        preparation: isRecord(parsed["preparation"]) ? (parsed["preparation"] as unknown as PreparedAction) : undefined,
      });
    }
  }
  const body = asErrorBody(parsed);
  if (body !== undefined) {
    return new AlixAIError({
      ...base,
      code: body.code,
      message: body.message,
      retryable: body.retryable,
      details: body.details,
    });
  }
  const code = codeForStatus(response.status);
  return new AlixAIError({
    ...base,
    code,
    message: `the server answered ${response.status} without an AlixAI error document`,
    details: { reason: "non_conforming_error_body" },
  });
};
