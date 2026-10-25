import { errorFromParsed, isAlixAIError, sdkError } from "./errors.js";
import type { OperationId } from "./operations.js";
import { createCallScope, createTransport } from "./transport.js";
import type { CallOptions, CallScope, ContextTokenProvider, SendSpec, Transport } from "./transport.js";
import { normalizeBaseUrl } from "./validate.js";

// The plumbing every area of the client shares: one scoped request, answered with a JSON document that has been shape-checked, or with
// the raw response for the few operations that answer with bytes or lines. It lives apart from client.ts so that the files, history,
// settings and RAG areas are built on the same transport, the same deadlines and the same error mapping, and so that none of them can
// forget a rule (the context token is requested after validation, once per request, and never kept).

/** What a client is built from. The public options of `createClient` and of `rag.createRagClient` both reduce to this. */
export interface CallerOptions {
  baseUrl: string;
  getContextToken: ContextTokenProvider;
  fetch?: ((input: string, init?: RequestInit) => Promise<Response>) | undefined;
  /** Deadline for an ordinary request, in milliseconds. Default 30000; 0 disables it. */
  timeoutMs?: number | undefined;
  allowInsecureHttp?: boolean | undefined;
}

export const DEFAULT_TIMEOUT_MS = 30_000;

/** A deadline option: undefined falls back, anything but a non-negative finite number is a validation error. */
export const timeout = (value: unknown, fallback: number, name: string): number => {
  if (value === undefined) return fallback;
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) {
    throw sdkError("validation_failed", `${name} must be a non-negative number of milliseconds`, { details: { argument: name } });
  }
  return value;
};

/** The error for a 2xx response whose body is not the shape the document promises. */
export const unexpected = (what: string, operation: OperationId) =>
  sdkError("internal", `the server's ${what} response has an unexpected shape`, { details: { operation, reason: "unexpected_shape" } });

/** What differs between the requests of one call. */
export interface CallExtra {
  /** Query parameters, already validated. */
  query?: Readonly<Record<string, string>>;
  headers?: Readonly<Record<string, string>>;
  /** The session to tell the token provider about when the path names none. */
  sessionId?: string | undefined;
  body?: SendSpec["body"];
  /** Statuses, besides 2xx, that are returned to the caller instead of thrown. */
  allowStatus?: (status: number) => boolean;
  /** The default deadline of this operation; the call's own `timeoutMs` wins. */
  timeoutMs?: number;
  /** Accept header. Default "application/json". */
  accept?: string;
}

/** A response head with the call's scope: the holder reads the body and then releases the scope. */
export interface Open {
  response: Response;
  scope: CallScope;
}

export interface Caller {
  readonly baseUrl: string;
  readonly transport: Transport;
  readonly requestTimeout: number;
  /** The session every request of a bound client (`forSession`) names to the token provider. */
  readonly sessionId: string | undefined;
  /** One JSON request/response: scope, send, parse, shape check, release. */
  json: <T>(
    operation: OperationId,
    params: Record<string, string>,
    guard: (value: unknown) => value is T,
    what: string,
    call: CallOptions | undefined,
    extra?: CallExtra,
  ) => Promise<T>;
  /** One request whose success carries no body (a 204): scope, send, release. */
  empty: (operation: OperationId, params: Record<string, string>, call: CallOptions | undefined, extra?: CallExtra) => Promise<void>;
  /** Sends one request and hands back the response with its still-open scope (bytes, lines, streams). The holder must release it. */
  open: (operation: OperationId, params: Record<string, string>, call: CallOptions | undefined, extra?: CallExtra) => Promise<Open>;
  /** A caller for a stream: the scope is the caller's own and lives as long as the stream. */
  scope: (operation: OperationId, call: CallOptions | undefined, defaultTimeoutMs: number) => CallScope;
  /** The same caller bound to a session: every token request names it. */
  bind: (sessionId: string) => Caller;
}

export const createCaller = (options: CallerOptions, sessionId?: string): Caller => {
  const baseUrl = normalizeBaseUrl(options.baseUrl, options.allowInsecureHttp === true);
  if (typeof options.getContextToken !== "function") {
    throw sdkError("validation_failed", "getContextToken is required: the host supplies a fresh single-use token per request", {
      details: { argument: "getContextToken" },
    });
  }
  const requestTimeout = timeout(options.timeoutMs, DEFAULT_TIMEOUT_MS, "timeoutMs");
  const userFetch = options.fetch;
  // Read the global lazily, at call time, so a late-installed or stubbed fetch is honoured.
  const fetchImpl = (input: string, init?: RequestInit): Promise<Response> =>
    userFetch !== undefined ? userFetch(input, init) : globalThis.fetch(input, init);
  const transport = createTransport(baseUrl, options.getContextToken, fetchImpl);

  const spec = (operation: OperationId, params: Record<string, string>, extra: CallExtra): SendSpec => {
    const session = extra.sessionId ?? sessionId;
    return {
      operation,
      params,
      accept: extra.accept ?? "application/json",
      ...(extra.query !== undefined && { query: extra.query }),
      ...(extra.headers !== undefined && { headers: extra.headers }),
      ...(session !== undefined && { sessionId: session }),
      ...(extra.body !== undefined && { body: extra.body }),
      ...(extra.allowStatus !== undefined && { allowStatus: extra.allowStatus }),
    };
  };

  const scopeFor = (operation: OperationId, call: CallOptions | undefined, extra: CallExtra): CallScope =>
    createCallScope(call?.signal, timeout(call?.timeoutMs, extra.timeoutMs ?? requestTimeout, "timeoutMs"), operation);

  const json: Caller["json"] = async (operation, params, guard, what, call, extra = {}) => {
    const scope = scopeFor(operation, call, extra);
    try {
      const response = await transport.send(spec(operation, params, extra), scope);
      let body: unknown;
      try {
        body = await transport.readJson(response, scope, operation);
      } catch (error) {
        // An allowed non-2xx status (a 503 report) whose body is not JSON is an ordinary HTTP failure.
        if (!response.ok && isAlixAIError(error) && error.details?.["reason"] === "not_json") throw errorFromParsed(response, undefined);
        throw error;
      }
      if (!guard(body)) throw response.ok ? unexpected(what, operation) : errorFromParsed(response, body);
      return body;
    } finally {
      scope.release();
    }
  };

  const empty: Caller["empty"] = async (operation, params, call, extra = {}) => {
    const scope = scopeFor(operation, call, extra);
    try {
      const response = await transport.send(spec(operation, params, extra), scope);
      // Nothing is read, so release the connection.
      await response.body?.cancel().catch(() => undefined);
    } finally {
      scope.release();
    }
  };

  const open: Caller["open"] = async (operation, params, call, extra = {}) => {
    const scope = scopeFor(operation, call, extra);
    try {
      return { response: await transport.send(spec(operation, params, extra), scope), scope };
    } catch (error) {
      scope.release();
      throw error;
    }
  };

  const caller: Caller = {
    baseUrl,
    transport,
    requestTimeout,
    sessionId,
    json,
    empty,
    open,
    scope: (operation, call, defaultTimeoutMs) =>
      createCallScope(call?.signal, timeout(call?.timeoutMs, defaultTimeoutMs, "timeoutMs"), operation),
    bind: (id) => createCaller(options, id),
  };
  return caller;
};
