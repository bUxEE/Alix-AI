import { DECISION_STATUS_CAPABILITY } from "./operations.js";
import type { DecisionContextTokenProvider } from "./client.js";
import type { DecisionStatus, DecisionStatusMode, FallbackReason } from "./types.js";

// Fixtures and an in-process fake of the decision status route, for the tests of a host application. It is a `fetch` replacement:
// no port, no network, no process. Pass `server.fetch` and `server.getContextToken` to `createDecisionClient`.
//
// What it enforces, so a host's tests fail the way they would against the real service: the route needs a bearer token, a token is
// SINGLE USE (a second presentation is 401 `replayed`), the token must carry `decision.status.read` (403 `permission_denied`),
// a query string is `validation_failed`, and a server without a decision service answers 503 `unavailable`, never an empty status.
//
// ADVISORY, NOT AN AUTHORISATION: everything here is operational information about the optional decision layer. The fake keeps no
// secret: its tokens are random strings of its own.

/** Builds a decision status. `rejected_count` is derived from `fallbacks`, so the fixture is always consistent with itself. */
export const decisionStatusFixture = (
  mode: DecisionStatusMode = "off",
  options: {
    /** Results whose disposition was accepted. Default 0. */
    accepted?: number;
    /** How many results fell back for each reason. */
    fallbacks?: Partial<Record<FallbackReason, number>>;
    providerId?: string;
    since?: string;
    generatedAt?: string;
  } = {},
): DecisionStatus => {
  const since = options.since ?? "2026-10-01T00:00:00.000Z";
  const fallbackCounts = Object.entries(options.fallbacks ?? {})
    .map(([reason, count]) => ({ reason: reason as FallbackReason, count: count ?? 0 }))
    .sort((a, b) => a.reason.localeCompare(b.reason));
  return {
    mode,
    provider_id: options.providerId ?? "fake-decision-provider",
    // A layer that is not off reports its model and the pinned revision.
    ...(mode !== "off" && { model_id: "fake-scorer", model_revision: "r1" }),
    accepted_count: options.accepted ?? 0,
    rejected_count: fallbackCounts.reduce((total, entry) => total + entry.count, 0),
    fallback_counts: fallbackCounts,
    since,
    generated_at: options.generatedAt ?? since,
  };
};

/** One request the fake received, for assertions. */
export interface RecordedDecisionRequest {
  method: string;
  /** The path and the query, as received. */
  path: string;
  /** True when the request carried a bearer token. */
  authenticated: boolean;
  /** The status the fake answered with. */
  status: number;
}

export interface FakeDecisionServerOptions {
  /** The base URL the fake answers for. Default "https://alixai.test". */
  baseUrl?: string;
  /** The capabilities its tokens carry. Default: `decision.status.read`. */
  capabilityScopes?: readonly string[];
  /** The status it reports. Default: the layer `off`. */
  status?: DecisionStatus;
  /** The clock, in milliseconds since the epoch, stamping `generated_at`. Default `Date.now`. */
  now?: () => number;
}

export interface FakeDecisionServer {
  readonly baseUrl: string;
  /** A `fetch` that routes to the fake. Pass it as the `fetch` option of the client. */
  readonly fetch: (input: string, init?: RequestInit) => Promise<Response>;
  /** A token provider that issues a fresh single-use token on every call. Pass it as `getContextToken`. */
  readonly getContextToken: DecisionContextTokenProvider;
  /** Every request received so far, oldest first. */
  readonly requests: readonly RecordedDecisionRequest[];
  /** Sets the status the fake reports; `undefined` restores the default (the layer `off`). */
  setStatus: (status: DecisionStatus | undefined) => void;
  /** Makes the status answer 503 `unavailable`, as a server without a decision service does, or restores it. */
  setUnavailable: (unavailable?: boolean) => void;
  /** Issues one single-use token. With `capabilityScopes` it carries those instead of the fake's default. */
  issueToken: (capabilityScopes?: readonly string[]) => string;
}

const STATUS_PATH = "/v1/decision/status";

const problem = (status: number, code: string, message: string, retryable = false): Response =>
  new Response(JSON.stringify({ code, message, retryable }), {
    status,
    headers: { "Content-Type": "application/json", "X-Request-ID": "req_fake", ...(retryable && { "Retry-After": "1" }) },
  });

/** Creates the in-process fake of the decision status route. */
export const createFakeDecisionServer = (options: FakeDecisionServerOptions = {}): FakeDecisionServer => {
  const baseUrl = (options.baseUrl ?? "https://alixai.test").replace(/\/+$/, "");
  const defaultScopes = options.capabilityScopes ?? [DECISION_STATUS_CAPABILITY];
  const now = options.now ?? (() => Date.now());
  const tokens = new Map<string, readonly string[]>();
  const used = new Set<string>();
  const requests: RecordedDecisionRequest[] = [];
  let status: DecisionStatus | undefined = options.status;
  let unavailable = false;
  let counter = 0;

  const issueToken = (scopes: readonly string[] = defaultScopes): string => {
    counter += 1;
    const token = `fake.${counter}.${Math.random().toString(36).slice(2, 12)}`;
    tokens.set(token, scopes);
    return token;
  };

  const answer = (path: string, method: string, authorization: string | null): Response => {
    if (method !== "GET" || !path.startsWith(STATUS_PATH)) return problem(404, "validation_failed", "no such route");
    const token = authorization?.startsWith("Bearer ") === true ? authorization.slice("Bearer ".length) : undefined;
    const scopes = token === undefined ? undefined : tokens.get(token);
    if (token === undefined || scopes === undefined) return problem(401, "unauthenticated", "the context token is missing or unknown");
    if (used.has(token)) return problem(401, "replayed", "the context token was already used: a token is single use");
    used.add(token);
    if (path !== STATUS_PATH) return problem(400, "validation_failed", "this route takes no query string");
    if (!scopes.includes(DECISION_STATUS_CAPABILITY)) return problem(403, "permission_denied", `the token does not carry ${DECISION_STATUS_CAPABILITY}`);
    if (unavailable) return problem(503, "unavailable", "the server has no decision service", true);
    const body = { ...(status ?? decisionStatusFixture("off")), generated_at: new Date(now()).toISOString() };
    return new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_fake" } });
  };

  const fetchFake = async (input: string, init?: RequestInit): Promise<Response> => {
    if (!input.startsWith(baseUrl)) throw new TypeError(`${input} is not the fake server (${baseUrl})`);
    const path = input.slice(baseUrl.length);
    const headers = new Headers(init?.headers);
    const response = answer(path, init?.method ?? "GET", headers.get("Authorization"));
    requests.push({ method: init?.method ?? "GET", path, authenticated: headers.has("Authorization"), status: response.status });
    return response;
  };

  return {
    baseUrl,
    fetch: fetchFake,
    getContextToken: () => issueToken(),
    requests,
    setStatus: (next) => {
      status = next;
    },
    setUnavailable: (value = true) => {
      unavailable = value;
    },
    issueToken,
  };
};
