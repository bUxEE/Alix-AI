import { errorFromResponse, isAlixAIError, sdkError } from "./errors.js";
import type { AlixAIError } from "./errors.js";
import { fillPath, isSafeToRepeat, operationSpec } from "./operations.js";
import type { OperationId } from "./operations.js";
import { contextToken } from "./validate.js";
import { TURN_EVENTS_SUBPROTOCOL, openSocket, webSocketUrl } from "./ws.js";
import type { WebSocketFactory, WebSocketLike } from "./ws.js";

// The HTTP layer under the client. Its rules, all of them about the single-use context token and the
// fail-closed stance of the API:
//   * getContextToken is called once per request or stream open, after the arguments have been
//     validated and before fetch, and the token lives only in a local variable for that call. It is
//     never cached, retried with, logged or put in an error.
//   * The token is sent only as `Authorization: Bearer`; redirects are an error (never followed with
//     the credential), cookies are never sent, nothing is cached.
//   * Nothing is retried here. A request that may have reached the server and whose outcome is
//     unknown is `uncertain_outcome`; the host decides what to do with `retryable`.

/** What the host's token provider is told about the request it is minting a token for. */
export interface ContextTokenRequest {
  /** The OpenAPI operationId of the request. */
  operation: OperationId;
  /**
   * The session the request concerns: the one the path names, or the one the client is bound to (`client.forSession`, a
   * `Conversation`, an upload's `sessionId` option). Absent for a request that has none, which the token itself then scopes.
   */
  sessionId?: string;
  /** The turn the request concerns, when the path names one. */
  turnId?: string;
  /** The preparation the request concerns, when the path names one. */
  preparationId?: string;
  /** The application the request concerns, when the path names one. */
  applicationId?: string;
  /** The file (or citation reference) the request concerns, when the path names one. */
  fileId?: string;
  /** The RAG source the request concerns, when the path names one. */
  sourceId?: string;
  /** The index generation the request concerns, when the path names one. */
  generationId?: string;
}

/**
 * Supplies a fresh, single-use context token for one request. Called for every authenticated request
 * and every event-stream open; never cached by the SDK. Return the bare token (no "Bearer " prefix).
 */
export type ContextTokenProvider = (request: ContextTokenRequest) => string | Promise<string>;

/** Per-call options, accepted by every client method. */
export interface CallOptions {
  /** Aborts the call: a pending request is abandoned and an open stream is released. */
  signal?: AbortSignal;
  /** Overrides the client's timeout for this call, in milliseconds; 0 disables it. */
  timeoutMs?: number;
}

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

/** The cancellation and deadline of one call. For a stream it lives as long as the stream. */
export interface CallScope {
  readonly signal: AbortSignal;
  /** Aborts the call with a reason. */
  cancel: (reason: unknown) => void;
  /**
   * Clears the deadline timer only: the response head has arrived and its body (an export, a download) may take longer than the
   * deadline of a request. The host's signal and `cancel` still abort the call. Idempotent.
   */
  endDeadline: () => void;
  /** Clears the deadline timer and the listener on the host's signal. Idempotent. */
  release: () => void;
}

/** The error the host should see for whatever a fetch or a body read threw. */
const failureFor = (error: unknown, scope: CallScope, operation: OperationId): unknown => {
  // An abort (the host's signal, a cancel(), or our deadline) is reported with its own reason.
  if (scope.signal.aborted) return scope.signal.reason;
  if (isAlixAIError(error)) return error;
  return transportError(operation, error);
};

/**
 * A failure before a complete response. A request that is safe to repeat (a read, a search) may be retried; for anything else
 * the request may have been processed, so the honest answer is that the outcome is unknown.
 */
const transportError = (operation: OperationId, cause: unknown): AlixAIError =>
  isSafeToRepeat(operation)
    ? sdkError("unavailable", "the request could not be completed (network error)", { cause, details: { operation, phase: "transport" } })
    : sdkError("uncertain_outcome", "the request may not have completed (network error): read the state before sending it again", {
        cause,
        details: { operation, phase: "transport" },
      });

export const createCallScope = (
  signal: AbortSignal | undefined,
  timeoutMs: number,
  operation: OperationId,
): CallScope => {
  const safe = isSafeToRepeat(operation);
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const onAbort = (): void => controller.abort(signal?.reason);
  if (signal !== undefined) {
    if (signal.aborted) controller.abort(signal.reason);
    else signal.addEventListener("abort", onAbort, { once: true });
  }
  if (timeoutMs > 0 && !controller.signal.aborted) {
    timer = setTimeout(() => {
      controller.abort(
        safe
          ? sdkError("timeout", `the request timed out after ${timeoutMs} ms`, { details: { operation, timeout_ms: timeoutMs } })
          : sdkError("uncertain_outcome", `the request timed out after ${timeoutMs} ms: read the state before sending it again`, {
              details: { operation, timeout_ms: timeoutMs, phase: "timeout" },
            }),
      );
    }, timeoutMs);
    (timer as { unref?: () => void }).unref?.();
  }
  const endDeadline = (): void => {
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
  };
  return {
    signal: controller.signal,
    cancel: (reason) => controller.abort(reason),
    endDeadline,
    release: () => {
      endDeadline();
      signal?.removeEventListener("abort", onAbort);
    },
  };
};

export interface SendSpec {
  operation: OperationId;
  /** Path placeholders, already validated. */
  params?: Readonly<Record<string, string>>;
  /** Query parameters, already validated. Never a credential: those travel in the Authorization header only. */
  query?: Readonly<Record<string, string>>;
  /** Extra request headers (for example `Last-Event-ID`). Authorization, Accept and Content-Type are the transport's. */
  headers?: Readonly<Record<string, string>>;
  /** The session to tell the token provider about when the path names none. */
  sessionId?: string;
  accept: string;
  /** `contentType` is left out for a FormData payload, so that fetch sets the multipart boundary itself. */
  body?: { contentType?: string; payload: string | Uint8Array | Blob | FormData };
  /** Statuses, besides 2xx, that are returned to the caller instead of thrown. */
  allowStatus?: (status: number) => boolean;
}

export interface Transport {
  /** Sends one request inside `scope` and returns the response head. Throws a typed error for any other status. */
  send: (spec: SendSpec, scope: CallScope) => Promise<Response>;
  /**
   * Opens a WebSocket for a request through the host's factory, with the context token in the `Authorization` header (never in the URL),
   * and resolves once it is open. A failure to open it is an `unavailable` error that says so (`details.phase`), which the caller reads
   * as the cue to fall back to server-sent events with a fresh token; an abort of the call is its own reason.
   */
  socket: (spec: SendSpec, scope: CallScope, factory: WebSocketFactory) => Promise<WebSocketLike>;
  /** Reads a whole JSON body, mapping a failure to a typed error. */
  readJson: (response: Response, scope: CallScope, operation: OperationId) => Promise<unknown>;
  /** Maps whatever a read or a fetch threw to the error the host should see. */
  failure: (error: unknown, scope: CallScope, operation: OperationId) => unknown;
}

/** `?a=1&b=2` for the parameters, or nothing. */
const queryString = (query: Readonly<Record<string, string>> | undefined): string => {
  const entries = Object.entries(query ?? {});
  return entries.length === 0 ? "" : `?${entries.map(([name, value]) => `${encodeURIComponent(name)}=${encodeURIComponent(value)}`).join("&")}`;
};

export const createTransport = (
  baseUrl: string,
  getContextToken: ContextTokenProvider,
  fetchImpl: FetchLike,
): Transport => {
  const failure: Transport["failure"] = (error, scope, operation) => failureFor(error, scope, operation);

  /** The `Authorization` value for a request: asks the host for a token once, after the arguments were validated, and keeps nothing. */
  const bearer = async (spec: SendSpec, scope: CallScope): Promise<string> => {
    const params = spec.params ?? {};
    let raw: unknown;
    try {
      raw = await getContextToken({
        operation: spec.operation,
        ...((params["session_id"] ?? spec.sessionId) !== undefined && { sessionId: params["session_id"] ?? spec.sessionId }),
        ...(params["turn_id"] !== undefined && { turnId: params["turn_id"] }),
        ...(params["preparation_id"] !== undefined && { preparationId: params["preparation_id"] }),
        ...(params["application_id"] !== undefined && { applicationId: params["application_id"] }),
        ...(params["file_id"] !== undefined && { fileId: params["file_id"] }),
        ...(params["source_id"] !== undefined && { sourceId: params["source_id"] }),
        ...(params["generation_id"] !== undefined && { generationId: params["generation_id"] }),
      });
    } catch (cause) {
      if (scope.signal.aborted) throw scope.signal.reason;
      throw sdkError("unauthenticated", "getContextToken failed, so the request was not sent", {
        cause,
        details: { operation: spec.operation, reason: "token_provider_failed" },
      });
    }
    const token = contextToken(raw);
    // The call may have been aborted while the host was minting the token: do not spend it.
    if (scope.signal.aborted) throw scope.signal.reason;
    return `Bearer ${token}`;
  };

  const send: Transport["send"] = async (spec, scope) => {
    const op = operationSpec(spec.operation);
    const params = spec.params ?? {};
    const headers: Record<string, string> = { ...spec.headers, Accept: spec.accept };
    if (spec.body?.contentType !== undefined) headers["Content-Type"] = spec.body.contentType;

    // A call that is already aborted sends nothing and spends no token.
    if (scope.signal.aborted) throw scope.signal.reason;

    if (op.authenticated) headers["Authorization"] = await bearer(spec, scope);

    let response: Response;
    try {
      response = await fetchImpl(`${baseUrl}${fillPath(op.path, params)}${queryString(spec.query)}`, {
        method: op.method,
        headers,
        body: spec.body?.payload as BodyInit | undefined,
        signal: scope.signal,
        redirect: "error",
        credentials: "omit",
        cache: "no-store",
      });
    } catch (error) {
      throw failure(error, scope, spec.operation);
    }
    if (response.ok || spec.allowStatus?.(response.status) === true) return response;
    let error: unknown;
    try {
      error = await errorFromResponse(response);
    } catch (readError) {
      error = failure(readError, scope, spec.operation);
    }
    throw error;
  };

  const socket: Transport["socket"] = async (spec, scope, factory) => {
    const op = operationSpec(spec.operation);
    const params = spec.params ?? {};
    // A call that is already aborted opens nothing and spends no token.
    if (scope.signal.aborted) throw scope.signal.reason;
    const headers: Record<string, string> = {};
    if (op.authenticated) headers["Authorization"] = await bearer(spec, scope);
    return openSocket(factory, webSocketUrl(`${baseUrl}${fillPath(op.path, params)}${queryString(spec.query)}`), { protocols: [TURN_EVENTS_SUBPROTOCOL], headers }, scope.signal);
  };

  const readJson: Transport["readJson"] = async (response, scope, operation) => {
    let text: string;
    try {
      text = await response.text();
    } catch (error) {
      throw failure(error, scope, operation);
    }
    try {
      return JSON.parse(text);
    } catch (cause) {
      throw sdkError("internal", "the server answered with a body that is not JSON", {
        cause,
        status: response.status,
        requestId: response.headers.get("X-Request-ID") ?? undefined,
        details: { operation, reason: "not_json" },
      });
    }
  };

  return { send, socket, readJson, failure };
};
