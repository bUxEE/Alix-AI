import { timeout } from "../call.js";
import { errorFromResponse, isAlixAIError, sdkError } from "../errors.js";
import type { CallOptions } from "../transport.js";
import { contextToken, normalizeBaseUrl } from "../validate.js";
import { isDecisionStatus } from "./guards.js";
import { DECISION_OPERATIONS } from "./operations.js";
import type { DecisionOperationId } from "./operations.js";
import type { DecisionStatus } from "./types.js";

/** What the host's token provider is told about a decision request: the operationId, which is all the status concerns. */
export interface DecisionContextTokenRequest {
  /** The OpenAPI operationId of the request. */
  operation: DecisionOperationId;
}

/**
 * Supplies a fresh, single-use context token for one request. Called for every request and never cached by the SDK. Return the
 * bare token (no "Bearer " prefix). The token must carry the capability `decision.status.read`.
 */
export type DecisionContextTokenProvider = (request: DecisionContextTokenRequest) => string | Promise<string>;

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface DecisionClientOptions {
  /** The AlixAI service, e.g. "https://alixai.example.com" or "http://127.0.0.1:8080". A path prefix is kept. */
  baseUrl: string;
  /**
   * Supplies a fresh single-use context token for each request. The SDK never mints, decodes, caches or retries with a token; it
   * holds one only for the call it is for.
   */
  getContextToken: DecisionContextTokenProvider;
  /** A fetch implementation. Defaults to the global `fetch`. */
  fetch?: FetchLike;
  /** Deadline for a request, in milliseconds. Default 30000; 0 disables it. */
  timeoutMs?: number;
  /** Allow plain http to a host that is not loopback. The context token is a bearer credential: leave this off. */
  allowInsecureHttp?: boolean;
}

/**
 * The decision-layer area of the public API (master plan 8.5 and 14.2), read-only.
 *
 * The decision layer is optional, bounded, non-generative and non-authoritative: it scores a short list of candidates that were
 * already authorised and never authorises, validates, prepares, confirms or commits anything. Nothing this client returns is an
 * authorisation: the status is ADVISORY, operational information about the layer, and no counter in it may be read as permission
 * to do anything. Authority stays with the deterministic controller and the application's own checks.
 */
export interface DecisionClient {
  /**
   * The decision-layer status of the caller's own application and tenant: the configured provider and model, the mode (`off`,
   * `shadow` or `advisory`), how many results were accepted and rejected since the counters started, and how often the layer fell
   * back, by reason.
   *
   * Advisory aggregate state: never an authorisation, a score or a commit result, and it carries no threshold, gate, candidate,
   * key or tenant datum. Needs the capability `decision.status.read` (`permission_denied` without it). A deployment without a
   * decision service answers `unavailable`: that is the absence of the layer, never an empty status, and every flow runs through
   * the controller either way. A body that is not a complete, self-consistent status (an unknown member, a counter that is not
   * the sum of its parts) is an `internal` error with `details.reason` `unexpected_shape`.
   */
  getStatus: (options?: CallOptions) => Promise<DecisionStatus>;
}

const DEFAULT_TIMEOUT_MS = 30_000;
const OPERATION: DecisionOperationId = "getDecisionStatus";

/** The cancellation and deadline of one call: a read that is safe to repeat, so a timeout is `timeout`. */
const callScope = (signal: AbortSignal | undefined, timeoutMs: number) => {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const onAbort = (): void => controller.abort(signal?.reason);
  if (signal !== undefined) {
    if (signal.aborted) controller.abort(signal.reason);
    else signal.addEventListener("abort", onAbort, { once: true });
  }
  if (timeoutMs > 0 && !controller.signal.aborted) {
    timer = setTimeout(() => {
      controller.abort(sdkError("timeout", `the request timed out after ${timeoutMs} ms`, { details: { operation: OPERATION, timeout_ms: timeoutMs } }));
    }, timeoutMs);
    (timer as { unref?: () => void }).unref?.();
  }
  return {
    signal: controller.signal,
    release: (): void => {
      if (timer !== undefined) clearTimeout(timer);
      timer = undefined;
      signal?.removeEventListener("abort", onAbort);
    },
  };
};

/**
 * Creates the client of the decision area for one AlixAI service.
 *
 * Like the main client it uses `fetch` only, holds the credential in memory for the length of one call, follows no redirect,
 * sends no cookie and retries nothing. The shared transport (src/call.ts) is closed over the operations of the first SDK version,
 * so this client sends its one request itself, with the same rules.
 */
export const createDecisionClient = (options: DecisionClientOptions): DecisionClient => {
  if (typeof options !== "object" || options === null) throw sdkError("validation_failed", "createDecisionClient needs its options");
  const baseUrl = normalizeBaseUrl(options.baseUrl, options.allowInsecureHttp === true);
  if (typeof options.getContextToken !== "function") {
    throw sdkError("validation_failed", "getContextToken is required: the host supplies a fresh single-use token per request", {
      details: { argument: "getContextToken" },
    });
  }
  const requestTimeout = timeout(options.timeoutMs, DEFAULT_TIMEOUT_MS, "timeoutMs");
  const spec = DECISION_OPERATIONS[OPERATION];

  return {
    getStatus: async (callOptions) => {
      const scope = callScope(callOptions?.signal, timeout(callOptions?.timeoutMs, requestTimeout, "timeoutMs"));
      /** The error the host should see for whatever a fetch or a body read threw. */
      const failure = (error: unknown): unknown => {
        if (scope.signal.aborted) return scope.signal.reason;
        if (isAlixAIError(error)) return error;
        return sdkError("unavailable", "the request could not be completed (network error)", { cause: error, details: { operation: OPERATION, phase: "transport" } });
      };
      try {
        if (scope.signal.aborted) throw scope.signal.reason;
        let raw: unknown;
        try {
          raw = await options.getContextToken({ operation: OPERATION });
        } catch (cause) {
          if (scope.signal.aborted) throw scope.signal.reason;
          throw sdkError("unauthenticated", "getContextToken failed, so the request was not sent", {
            cause,
            details: { operation: OPERATION, reason: "token_provider_failed" },
          });
        }
        const token = contextToken(raw);
        // The call may have been aborted while the host was minting the token: do not spend it.
        if (scope.signal.aborted) throw scope.signal.reason;

        const fetchImpl: FetchLike = (input, init) => (options.fetch !== undefined ? options.fetch(input, init) : globalThis.fetch(input, init));
        let response: Response;
        try {
          response = await fetchImpl(`${baseUrl}${spec.path}`, {
            method: spec.method,
            headers: { Accept: "application/json", Authorization: `Bearer ${token}` },
            signal: scope.signal,
            redirect: "error",
            credentials: "omit",
            cache: "no-store",
          });
        } catch (error) {
          throw failure(error);
        }
        if (!response.ok) {
          let refusal: unknown;
          try {
            refusal = await errorFromResponse(response);
          } catch (error) {
            refusal = failure(error);
          }
          throw refusal;
        }
        let text: string;
        try {
          text = await response.text();
        } catch (error) {
          throw failure(error);
        }
        const requestId = response.headers.get("X-Request-ID") ?? undefined;
        let body: unknown;
        try {
          body = JSON.parse(text);
        } catch (cause) {
          throw sdkError("internal", "the server answered with a body that is not JSON", {
            cause,
            status: response.status,
            requestId,
            details: { operation: OPERATION, reason: "not_json" },
          });
        }
        if (!isDecisionStatus(body)) {
          throw sdkError("internal", "the server's decision status response has an unexpected shape", {
            status: response.status,
            requestId,
            details: { operation: OPERATION, reason: "unexpected_shape" },
          });
        }
        return body;
      } finally {
        scope.release();
      }
    },
  };
};
