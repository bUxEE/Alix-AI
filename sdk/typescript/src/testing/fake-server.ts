import type {
  ActionOutcome,
  Application,
  ApplicationStatus,
  Capability,
  CapabilityList,
  Health,
  PackageRecord,
  PendingState,
  ReadyReport,
  Registration,
  Session,
  TurnInput,
} from "../api-types.js";
import { isRetryableCode } from "../errors.js";
import { OPERATIONS, matchPath } from "../operations.js";
import type { CoreOperationId } from "../operations.js";
import { CONFIRMATION_TOKEN_HEADER } from "../turn.js";
import { isConfirmationToken } from "../validate.js";
import type { HistoryExportRow } from "../api-types.js";
import type { FileRef } from "../gen/conversation/index.js";
import type { WebSocketFactory } from "../ws.js";
import type { FakeContext } from "./context.js";
import { createFakeExports } from "./fake-exports.js";
import { createFakeFiles } from "./fake-files.js";
import type { FakeFilesOptions } from "./fake-files.js";
import { createFakeHistory } from "./fake-history.js";
import { createFakeMeMemory } from "./fake-me-memory.js";
import { createFakeSettings } from "./fake-settings.js";
import type { FakeSettingsOptions } from "./fake-settings.js";
import { FAKE_UPGRADE_HEADER, FAKE_WEBSOCKET_HEADER, createFakeWebSocketFactory } from "./fake-ws.js";
import type { FakeSocketScript } from "./fake-ws.js";
import { base64urlText, confirmationSecret, decodeBase64urlText, idempotencyKey, randomSegment, typedId } from "./ids.js";
import type { PrepareInput, TurnPlan } from "./plans.js";
import { reply } from "./plans.js";
import type { ActionReceipt, ErrorBody, ErrorCode, PreparedAction, RiskLevel, TurnEvent } from "./types.js";

// An in-process fake of the AlixAI public API (/v1) for the tests of a host application.
//
// It is a fetch handler, so it needs no port and no network: pass `fake.fetch` to createClient. It
// implements the protocol the SDK relies on, not the product, so a host's tests fail the way they
// would against the real service when the host gets the protocol wrong:
//   * every authenticated request needs `Authorization: Bearer <token>`, and a token is SINGLE USE:
//     a second presentation is 401 `replayed` (a host that caches its token fails here);
//   * the session in a path must be the token's (403 `scope_violation`), and a session cannot change
//     principal, tenant or workspace;
//   * a turn answers with TurnEvents (numbered from 0, `done` last) as server-sent events when asked
//     for `text/event-stream`, else as one JSON document; the confirmation secret is NOT an event: it
//     is the X-AlixAI-Confirmation-Token header (stream) or the `confirmation` member (JSON);
//   * a confirmation secret works once (second use: 409 `replayed`); level 3 is `step_up_required`;
//     a receipt is `conflict` until there is an outcome; a package that is not active serves nothing
//     (403 `quarantined`).
//   * the events of a turn are kept in a replay buffer, and `GET .../turns/{turn_id}/events` serves them again from a cursor (`cursor` or
//     `Last-Event-ID`) as server-sent events or, through `fake.webSocket`, as a WebSocket: 400 for a bad or too-far cursor, 410 behind
//     the retained window, 204 at the end, and a turn that is not the caller's is the same 403 as one that does not exist;
//   * files (multipart upload, scoped references, download, preview), history (segments, search, export, clear, cleanup) and settings
//     (versioned documents with optimistic concurrency) follow the OpenAPI document; see fake-files.ts, fake-history.ts, fake-settings.ts.
// What it does NOT do: verify a token's signature (any well-formed token is accepted; the fake is not
// a security component, `issueToken` makes one), run a model, or reach an application. A test says
// what each turn does with `onTurn` and the plan builders (reply, askChoice, prepareAction, ...).

export interface FakeTokenClaims {
  /** The session the token is bound to (the session id in the path must be this one). */
  session_id: string;
  application_id?: string;
  tenant_id?: string;
  workspace_id?: string;
  /** The principal (`sub`). */
  principal_id?: string;
  roles?: string[];
  capability_scopes?: string[];
  /** Seconds from issue to expiry. Default 60. */
  ttlSeconds?: number;
}

export interface FakeTurn {
  input: Extract<TurnInput, { text: string } | { choice_id: string }>;
  session: { session_id: string; application_id: string; tenant_id: string; workspace_id: string; principal_id: string };
  /** 1 for the first turn of the session. */
  turn_number: number;
  /** What the session waited for when this turn arrived. */
  pending: PendingState | null;
}

export type TurnHandler = (turn: FakeTurn) => TurnPlan | Promise<TurnPlan>;

export interface RecordedRequest {
  operation: CoreOperationId | undefined;
  method: string;
  path: string;
  /** The status the fake answered with. */
  status: number;
  accept: string | null;
  contentType: string | null;
  /** Whether an Authorization header was presented. */
  authenticated: boolean;
  /** The token's `jti`, when the token decoded. The token itself is never recorded. */
  tokenId: string | undefined;
  /** The parsed JSON body, with every `token` member replaced by "[redacted]". */
  body: unknown;
}

export interface InjectedFailure {
  /** Only requests of this operation (default: any authenticated request). */
  operation?: CoreOperationId;
  /** Answer with this error document and status, once (or `times`), in place of the real handler. */
  status?: number;
  error?: ErrorBody;
  headers?: Record<string, string>;
  /** Make the fetch itself fail, as a network error would. */
  networkError?: boolean;
  /**
   * For an event stream (a turn, or a read of its events from a cursor): send only the first N events, then end the stream without
   * `done` (a WebSocket is closed with `closeCode`, 1006 by default).
   */
  truncateAfterEvents?: number;
  /** For a WebSocket event stream: the close code after the events (with `truncateAfterEvents`), such as 1013 (slow consumer) or 1011. */
  closeCode?: number;
  /** For an event stream read over a WebSocket: refuse the upgrade, after the token was checked and redeemed. The client falls back to SSE. */
  refuseWebSocket?: boolean;
  times?: number;
}

export interface FakeServerOptions {
  /** The origin (and optional path prefix) the fake answers for. Default "https://alixai.test". */
  baseUrl?: string;
  /** The clock. Default: the real one. `advanceTime` moves it forward. */
  now?: () => Date;
  capabilities?: Capability[];
  /** The state of the caller's application. Default "active". */
  applicationStatus?: ApplicationStatus;
  /** Split the bytes of an event stream into chunks of this size (0: one chunk per frame). Exercises the reader. */
  streamChunkBytes?: number;
  /** How long a preparation stays valid. Default 10 minutes. */
  preparationTtlMs?: number;
  /** What a text or choice turn does when `onTurn` has not been called. Default: echo the text. */
  turnHandler?: TurnHandler;
  /** How many events of a turn the replay buffer keeps (the newest ones). Default: all of them. A smaller number makes an old cursor `expired`. */
  replayEvents?: number;
  /** How long a finished turn can be read again, in milliseconds. Default 5 minutes. */
  turnRetentionMs?: number;
  /** The host's attachment policy and limits for the files area. */
  files?: FakeFilesOptions;
  /** The approved profiles of the settings area. */
  settings?: FakeSettingsOptions;
}

export interface FakeAlixAIServer {
  /** Pass to createClient as `baseUrl`. */
  readonly baseUrl: string;
  /** Pass to createClient as `fetch`: it routes to the fake and never touches the network. */
  readonly fetch: (input: string, init?: RequestInit) => Promise<Response>;
  /** A fresh single-use token for the claims. Each call is a new token with a new `jti`. */
  issueToken: (claims: FakeTokenClaims) => string;
  /** Replaces what text and choice turns do. Pass undefined to restore the default (echo). */
  onTurn: (handler: TurnHandler | undefined) => void;
  /** Makes the next request(s) fail. See InjectedFailure. */
  failNext: (failure: InjectedFailure) => void;
  /** Moves the fake's clock forward, to expire preparations and tokens. */
  advanceTime: (ms: number) => void;
  setApplicationStatus: (status: ApplicationStatus) => void;
  /** Makes /readyz and /v1/health report not ready (503 with the report) or ready again. */
  setReady: (ready: boolean) => void;
  /** Every request seen, in order. */
  readonly requests: readonly RecordedRequest[];
  /** The wire document of a session, as the API would report it. */
  session: (sessionId: string) => Session | undefined;
  /** The prepared actions the fake holds, as the API reports them. */
  preparations: () => PreparedAction[];
  /** Pass to createClient as `webSocket`: opens an in-process WebSocket that is served by the fake (the token is checked and redeemed as for a fetch). */
  readonly webSocket: WebSocketFactory;
  /** The references of the live files, for assertions. */
  files: () => FileRef[];
  /** The transcript rows the fake holds, as the API would export them. */
  history: () => HistoryExportRow[];
  /** How many context tokens have been accepted (and used up). */
  readonly tokensRedeemed: number;
  /** Forgets all state: sessions, preparations, redeemed tokens, recorded requests, injected failures. */
  reset: () => void;
  /** Serves the fake over HTTP on 127.0.0.1 (Node only), for code that cannot inject `fetch`. */
  listen: () => Promise<{ baseUrl: string; close: () => Promise<void> }>;
}

// ---- internals ----

interface Scope {
  application_id: string;
  tenant_id: string;
  workspace_id: string;
  principal_id: string;
  session_id: string;
  capability_scopes: string[];
}

interface SessionRecord {
  scope: Scope;
  status: "active" | "ended";
  turnCount: number;
  createdAt: number;
  updatedAt: number;
  clarification?: { field: string; answer_kind: "choice"; choices: Array<{ choice_id: string; label: string; value: unknown }> };
  preparationId?: string;
}

interface PreparedRecord {
  action: PreparedAction;
  scope: Scope;
  secret: string | undefined;
  secretUsed: boolean;
  commit: NonNullable<PrepareInput["commit"]>;
  receipt?: ActionReceipt;
}

type Json = Record<string, unknown>;

const STATUS_FOR_CODE = (code: ErrorCode): number => {
  switch (code) {
    case "unauthenticated":
    case "expired":
    case "replayed":
    case "scope_missing":
      return 401;
    case "scope_violation":
    case "permission_denied":
    case "step_up_required":
    case "quarantined":
      return 403;
    case "validation_failed":
      return 400;
    case "schema_invalid":
    case "unknown_capability":
    case "missing_fields":
    case "no_supported_evidence":
      return 422;
    case "ambiguous":
    case "stale":
    case "conflict":
      return 409;
    case "confirmation_required":
      return 428;
    case "rate_limited":
      return 429;
    case "unavailable":
    case "degraded":
    case "resource_exhausted":
      return 503;
    case "timeout":
      return 504;
    default:
      return 500;
  }
};

const MAX_TOKEN_EVENT_CHARS = 4096;
const encoder = new TextEncoder();

const isRecord = (value: unknown): value is Json => typeof value === "object" && value !== null && !Array.isArray(value);

const chunkText = (text: string): string[] => {
  if (text.trim() === "") return [];
  const chars = Array.from(text);
  const out: string[] = [];
  for (let i = 0; i < chars.length; i += MAX_TOKEN_EVENT_CHARS) out.push(chars.slice(i, i + MAX_TOKEN_EVENT_CHARS).join(""));
  return out;
};

const redact = (value: unknown): unknown => {
  if (Array.isArray(value)) return value.map(redact);
  if (!isRecord(value)) return value;
  return Object.fromEntries(Object.entries(value).map(([k, v]) => [k, k === "token" ? "[redacted]" : redact(v)]));
};

const defaultCapabilities = (): Capability[] => [
  { id: "demo.lookup", title: "Look something up", risk_level: 0, requires_confirmation: false, requires_step_up: false, fields: ["query"] },
  { id: "demo.book", title: "Book something", risk_level: 2, requires_confirmation: true, requires_step_up: false, fields: ["item", "when"] },
  { id: "demo.close_account", title: "Close the account", risk_level: 3, requires_confirmation: false, requires_step_up: true, fields: [] },
];

const SCOPE_DEFAULTS = {
  application_id: "fake-app",
  tenant_id: "tenant-1",
  workspace_id: "workspace-1",
  principal_id: "user-1",
};

export const createFakeServer = (options: FakeServerOptions = {}): FakeAlixAIServer => {
  const base = new URL(options.baseUrl ?? "https://alixai.test");
  const prefix = base.pathname.replace(/\/+$/, "");
  const baseUrl = `${base.origin}${prefix}`;
  const preparationTtlMs = options.preparationTtlMs ?? 10 * 60 * 1000;
  const chunkBytes = options.streamChunkBytes ?? 0;

  let offset = 0;
  const nowMs = (): number => (options.now ? options.now().getTime() : Date.now()) + offset;
  const iso = (ms: number): string => new Date(ms).toISOString();

  let handler: TurnHandler = options.turnHandler ?? ((turn) => reply(`fake: ${"text" in turn.input ? turn.input.text : turn.input.choice_id}`));
  let capabilities: Capability[] = options.capabilities ?? defaultCapabilities();
  let appStatus: ApplicationStatus = options.applicationStatus ?? "active";
  let ready = true;
  const sessions = new Map<string, SessionRecord>();
  const preparations = new Map<string, PreparedRecord>();
  const redeemed = new Set<string>();
  const registered = new Map<string, PackageRecord>();
  let failures: InjectedFailure[] = [];
  let recorded: RecordedRequest[] = [];
  /** The replay buffer: the events of every turn, for the route that reads a turn again from a cursor. */
  const turnBuffers = new Map<string, { scope: Scope; events: TurnEvent[]; createdAt: number }>();
  const replayEvents = options.replayEvents ?? Number.POSITIVE_INFINITY;
  const turnRetentionMs = options.turnRetentionMs ?? 5 * 60 * 1000;

  // ---- responses ----

  const json = (status: number, body: unknown, headers: Record<string, string> = {}): Response =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "Content-Type": "application/json", "Cache-Control": "no-store", "X-Request-ID": `req_${randomSegment(9)}`, ...headers },
    });

  const problem = (code: ErrorCode, message: string, extra: { status?: number; details?: Json } = {}): Response => {
    const status = extra.status ?? STATUS_FOR_CODE(code);
    const requestId = `req_${randomSegment(9)}`;
    const body: ErrorBody = { code, message, retryable: isRetryableCode(code), details: { ...extra.details, request_id: requestId } };
    const headers: Record<string, string> = { "X-Request-ID": requestId };
    if (status === 401) headers["WWW-Authenticate"] = "Bearer";
    if (body.retryable) headers["Retry-After"] = "1";
    return json(status, body, headers);
  };

  const levelViolation = (level: string): Response =>
    problem("scope_violation", `the ${level} in the path is not the one the context token is bound to`, { details: { levels: [level] } });

  const context: FakeContext = {
    json: (status, body, headers) => json(status, body, headers),
    problem: (code, message, extra) => problem(code, message, extra),
    levelViolation: (level) => levelViolation(level),
    nowMs: () => nowMs(),
    iso: (ms) => iso(ms),
    quarantine: () => quarantineRefusal(),
  };
  const filesArea = createFakeFiles(context, options.files);
  const historyArea = createFakeHistory(context);
  const settingsArea = createFakeSettings(context, () => capabilities, () => appStatus, options.settings);
  const meMemoryArea = createFakeMeMemory(context);
  const exportsArea = createFakeExports(context);

  // ---- tokens ----

  const issueToken = (claims: FakeTokenClaims): string => {
    const issued = Math.floor(nowMs() / 1000);
    const payload = {
      jti: `jti_${randomSegment(12)}`,
      iat: issued,
      exp: issued + (claims.ttlSeconds ?? 60),
      application_id: claims.application_id ?? SCOPE_DEFAULTS.application_id,
      tenant_id: claims.tenant_id ?? SCOPE_DEFAULTS.tenant_id,
      workspace_id: claims.workspace_id ?? SCOPE_DEFAULTS.workspace_id,
      sub: claims.principal_id ?? SCOPE_DEFAULTS.principal_id,
      roles: claims.roles ?? ["user"],
      // Default deny: a token permits exactly the capabilities the fake serves (and not applications.register)
      // unless the test says otherwise. There is no wildcard.
      capability_scopes: claims.capability_scopes ?? capabilities.map((c) => c.id),
      session_id: claims.session_id,
    };
    // Unsigned on purpose: this is the fake's token, and the header says so. A real service would refuse it.
    const header = { alg: "none", typ: "alixai-fake+jwt" };
    return `${base64urlText(JSON.stringify(header))}.${base64urlText(JSON.stringify(payload))}.${randomSegment(12)}`;
  };

  /** Verifies and consumes the bearer token. Returns the scope, or the refusal. */
  const authenticate = (request: Request): { scope: Scope; tokenId: string } | { refusal: Response; tokenId?: string } => {
    const header = request.headers.get("Authorization");
    if (header === null) return { refusal: problem("unauthenticated", "no context token was presented") };
    const [scheme, token, ...rest] = header.split(" ");
    if (scheme?.toLowerCase() !== "bearer" || token === undefined || token === "" || rest.length > 0) {
      return { refusal: problem("unauthenticated", "the Authorization header is not a bearer token") };
    }
    const parts = token.split(".");
    let claims: Json;
    try {
      if (parts.length !== 3) throw new Error("not a compact token");
      const parsed: unknown = JSON.parse(decodeBase64urlText(parts[1] ?? ""));
      if (!isRecord(parsed)) throw new Error("claims are not an object");
      claims = parsed;
    } catch {
      return { refusal: problem("unauthenticated", "the context token is malformed") };
    }
    const str = (key: string): string | undefined => (typeof claims[key] === "string" && claims[key] !== "" ? (claims[key] as string) : undefined);
    const jti = str("jti");
    const sub = str("sub");
    const app = str("application_id");
    const tenant = str("tenant_id");
    const workspace = str("workspace_id");
    const session = str("session_id");
    if (jti === undefined || sub === undefined || app === undefined || tenant === undefined || workspace === undefined || session === undefined) {
      return { refusal: problem("unauthenticated", "the context token does not carry the required claims") };
    }
    if (typeof claims["exp"] === "number" && claims["exp"] * 1000 <= nowMs()) {
      return { refusal: problem("expired", "the context token has expired"), tokenId: jti };
    }
    if (redeemed.has(jti)) return { refusal: problem("replayed", "the context token was already used"), tokenId: jti };
    redeemed.add(jti);
    const scopes = Array.isArray(claims["capability_scopes"]) ? (claims["capability_scopes"] as unknown[]).filter((s): s is string => typeof s === "string") : [];
    return { scope: { application_id: app, tenant_id: tenant, workspace_id: workspace, principal_id: sub, session_id: session, capability_scopes: scopes }, tokenId: jti };
  };

  // ---- state ----

  const sessionKey = (scope: Scope): string => `${scope.application_id}\u0000${scope.tenant_id}\u0000${scope.workspace_id}\u0000${scope.session_id}`;

  const effectiveState = (record: PreparedRecord): PreparedAction => {
    const a = record.action;
    if (a.state === "prepared" && Date.parse(a.expires_at) <= nowMs()) a.state = "expired";
    return a;
  };

  const pendingOf = (session: SessionRecord): PendingState | null => {
    if (session.preparationId !== undefined) {
      const record = preparations.get(session.preparationId);
      if (record !== undefined) {
        const action = effectiveState(record);
        if (action.state === "prepared" || action.state === "confirmed") {
          return {
            kind: "confirmation",
            capability_id: action.capability_id,
            phase: "awaiting_confirmation",
            collected_fields: [],
            preparation: { ...action },
            expires_at: action.expires_at,
          };
        }
      }
      session.preparationId = undefined;
    }
    if (session.clarification !== undefined) {
      return {
        kind: "clarification",
        capability_id: "demo.clarify",
        phase: "awaiting_answer",
        collected_fields: [],
        question: { field: session.clarification.field, answer_kind: "choice", choices: session.clarification.choices as never },
        missing_fields: [session.clarification.field],
      };
    }
    return null;
  };

  const sessionDoc = (session: SessionRecord): Session => ({
    session_id: session.scope.session_id,
    status: session.status,
    turn_count: session.turnCount,
    created_at: iso(session.createdAt),
    updated_at: iso(session.updatedAt),
    pending: session.status === "ended" ? null : pendingOf(session),
  });

  const packageRecord = (state: PackageRecord["state"], hash: string): PackageRecord => ({
    manifest_hash: hash,
    package_version: "1.0.0",
    state,
    warnings: 0,
    installed_at: iso(nowMs()),
    status_changed_at: iso(nowMs()),
  });

  const packagesFor = (): PackageRecord[] => {
    const own: PackageRecord[] =
      appStatus === "not_installed" ? [] : [packageRecord(appStatus === "active" ? "active" : appStatus, "0".repeat(64))];
    return [...own, ...registered.values()];
  };

  /** A package that is not active serves no traffic. */
  const quarantineRefusal = (): Response | undefined =>
    appStatus === "active" ? undefined : problem("quarantined", "the application's package is not active");

  /** Opens (or resumes) the session of the scope, enforcing continuity and the ended state. */
  const openSession = (scope: Scope): SessionRecord | Response => {
    const key = sessionKey(scope);
    const existing = sessions.get(key);
    if (existing === undefined) {
      const created: SessionRecord = { scope, status: "active", turnCount: 0, createdAt: nowMs(), updatedAt: nowMs() };
      sessions.set(key, created);
      return created;
    }
    if (existing.scope.principal_id !== scope.principal_id) {
      return problem("scope_violation", "the session belongs to another principal", { details: { levels: ["principal"] } });
    }
    if (existing.status === "ended") {
      return problem("expired", "the session has ended; a new session needs a new authenticated context", { status: 410 });
    }
    return existing;
  };

  // ---- actions ----

  const ownedPreparation = (scope: Scope, id: string): PreparedRecord | Response => {
    const record = preparations.get(id);
    const own = record?.scope;
    if (record === undefined || own === undefined || sessionKey(own) !== sessionKey(scope) || own.principal_id !== scope.principal_id) {
      return levelViolation("preparation");
    }
    return record;
  };

  const receiptOf = (record: PreparedRecord): ActionReceipt => {
    const plan = record.commit;
    const status = plan.status ?? "committed";
    const receipt: ActionReceipt = {
      receipt_id: typedId("rcpt", nowMs()),
      preparation_id: record.action.preparation_id,
      capability_id: record.action.capability_id,
      status,
      records: plan.records ?? [],
      facts: plan.facts ?? [],
      errors: status === "committed" || status === "queued" ? [] : [{ code: plan.error?.code ?? "conflict", message: plan.error?.message ?? "the host refused the commit" }],
      audit_ref: typedId("aud", nowMs()),
      issued_at: iso(nowMs()),
    };
    if (status === "queued") receipt.follow_up = { reference: typedId("job", nowMs()), state: "queued", as_of: iso(nowMs()) };
    return receipt;
  };

  /** Commits a confirmed preparation: records the receipt and moves the state. */
  const commitPrepared = (record: PreparedRecord): { receipt: ActionReceipt; error?: ErrorBody } => {
    const receipt = receiptOf(record);
    record.receipt = receipt;
    const status = receipt.status;
    record.action.state = status === "committed" || status === "queued" ? "committed" : status === "rejected" ? "rejected" : "uncertain";
    if (status === "committed" || status === "queued") return { receipt };
    const code: ErrorCode = record.commit.error?.code ?? (status === "partial" ? "partial_failure" : status === "uncertain" ? "uncertain_outcome" : "conflict");
    const error: ErrorBody = { code, message: record.commit.error?.message ?? `the commit ended ${status}`, retryable: isRetryableCode(code) };
    return { receipt, error };
  };

  type ConfirmFailure = { code: ErrorCode; message: string };

  /** The coordinator's Confirm: checks, then uses the secret up. Returns a failure or undefined on success. */
  const confirmPrepared = (record: PreparedRecord, token: string): ConfirmFailure | undefined => {
    const action = effectiveState(record);
    if (record.secretUsed) return { code: "replayed", message: "the confirmation was already used" };
    switch (action.state) {
      case "prepared":
        break;
      case "expired":
        return { code: "expired", message: "the preparation has expired; prepare the action again" };
      case "invalidated":
        return { code: "stale", message: "the data the preparation relied on changed; prepare the action again" };
      default:
        return { code: "conflict", message: `the preparation is ${action.state}` };
    }
    if (action.confirmation.mode === "none") return { code: "validation_failed", message: "this action needs no confirmation" };
    if (action.confirmation.mode === "step_up") {
      return { code: "step_up_required", message: "this action must be completed in the application's own authenticated interface" };
    }
    if (record.secret === undefined || token !== record.secret) {
      return { code: "permission_denied", message: "the confirmation does not belong to this preparation" };
    }
    record.secretUsed = true;
    action.state = "confirmed";
    return undefined;
  };

  const confirmEndpoint = (scope: Scope, id: string, body: unknown): Response => {
    const quarantined = quarantineRefusal();
    if (quarantined) return quarantined;
    const record = ownedPreparation(scope, id);
    if (record instanceof Response) return record;
    if (!isRecord(body) || Object.keys(body).some((k) => k !== "token") || !isConfirmationToken(body["token"])) {
      return problem("validation_failed", "the request body must be {\"token\": \"cfm_...\"}");
    }
    const failure = confirmPrepared(record, body["token"]);
    if (failure !== undefined) return problem(failure.code, failure.message, failure.code === "replayed" ? { status: 409 } : {});
    const { receipt, error } = commitPrepared(record);
    const outcome: ActionOutcome & { error?: ErrorBody } = { preparation: { ...record.action }, receipt };
    if (error === undefined) return json(200, outcome);
    const requestId = `req_${randomSegment(9)}`;
    return json(receipt.status === "rejected" ? 409 : 500, { ...outcome, error: { ...error, details: { request_id: requestId } } }, { "X-Request-ID": requestId });
  };

  // ---- turns ----

  interface Built {
    events: TurnEvent[];
    grant?: { preparation_id: string; token: string };
  }

  const finalize = (turnId: string, bodies: Json[]): TurnEvent[] =>
    bodies.map((body, seq) => ({ type: body["type"], turn_id: turnId, seq, ...body }) as unknown as TurnEvent);

  const directive = (turnId: string, act: string, extra: Json = {}): Json => ({
    turn_id: turnId,
    capability_id: null,
    act,
    facts: [],
    must_state: [],
    missing_fields: [],
    invalid_fields: [],
    allowed_suggestions: [],
    confirmation: { mode: "none", bindings: [] },
    permitted_next_actions: [],
    sources: [],
    ...extra,
  });

  const progress = (stage: string, capabilityId?: string): Json => ({
    type: "progress",
    stage,
    state: "completed",
    ...(capabilityId !== undefined && { capability_id: capabilityId }),
  });

  const textEvents = (text: string | undefined): Json[] => chunkText(text ?? "").map((chunk) => ({ type: "token", text: chunk }));

  const confirmationPanel = (action: PreparedAction): Json => ({
    type: "block",
    block: {
      type: "confirmation_panel",
      preparation_id: action.preparation_id,
      summary: action.summary,
      effects: action.effects,
      risk_level: action.risk_level,
      expires_at: action.expires_at,
      confirmation: action.confirmation,
    },
  });

  const errorEvents = (error: ConfirmFailure, text?: string): Json[] => [
    { type: "error", error: { code: error.code, message: error.message, retryable: isRetryableCode(error.code) } },
    ...textEvents(text ?? error.message),
  ];

  const prepare = (session: SessionRecord, input: PrepareInput): PreparedRecord => {
    const risk: RiskLevel = input.risk_level ?? 2;
    const mode = risk >= 3 ? "step_up" : risk === 2 ? "explicit" : "none";
    const id = typedId("prep", nowMs());
    const preparedAt = nowMs();
    const action: PreparedAction = {
      preparation_id: id,
      capability_id: input.capability_id,
      state: "prepared",
      summary: input.summary,
      effects: input.effects ?? [{ operation: "other", entity_type: "record", description: input.summary, changes: [], external: false }],
      risk_level: risk,
      data_versions: [{ entity_type: "record", entity_id: "record-1", version: "v1" }],
      prepared_at: iso(preparedAt),
      expires_at: iso(preparedAt + (input.validForMs ?? preparationTtlMs)),
      invalidation_conditions: ["data_version_changed", "session_ended"],
      confirmation:
        mode === "none"
          ? { mode, bindings: [] }
          : { mode, bindings: ["preparation_id", "principal", "session", "data_versions"] },
      idempotency_key: idempotencyKey(),
    };
    const record: PreparedRecord = {
      action,
      scope: session.scope,
      secret: mode === "explicit" ? confirmationSecret() : undefined,
      secretUsed: false,
      commit: input.commit ?? { status: "committed" },
    };
    preparations.set(id, record);
    return record;
  };

  const planEvents = (turnId: string, session: SessionRecord, plan: TurnPlan, scope: Scope): Built => {
    session.clarification = undefined;
    if (plan.kind === "prepare" && !scope.capability_scopes.includes(plan.input.capability_id)) {
      // Default deny: the token's capability scopes are exact ids, and anything not listed is refused.
      const refusal = { code: "permission_denied" as ErrorCode, message: `the context token does not permit the capability ${plan.input.capability_id}` };
      return {
        events: finalize(turnId, [
          progress("interpreting"),
          { type: "directive", directive: directive(turnId, "refuse", { reason_code: refusal.code }) },
          ...errorEvents(refusal),
          { type: "done", finish_reason: "failed" },
        ]),
      };
    }
    switch (plan.kind) {
      case "reply":
        return {
          events: finalize(turnId, [
            progress("interpreting"),
            { type: "directive", directive: directive(turnId, "present_result") },
            ...textEvents(plan.text),
            { type: "done", finish_reason: "completed" },
          ]),
        };
      case "clarify": {
        const choices = plan.choices.map((c) => ({ choice_id: c.choice_id, label: c.label, value: c.value ?? { kind: "identifier", value: c.choice_id } }));
        session.clarification = { field: plan.field, answer_kind: "choice", choices };
        return {
          events: finalize(turnId, [
            progress("interpreting"),
            {
              type: "directive",
              directive: directive(turnId, "disambiguate", {
                next_question: { field: plan.field, answer_kind: "choice", choices },
                allowed_suggestions: choices,
              }),
            },
            { type: "clarification", clarification: { type: "clarification_choices", field: plan.field, answer_kind: "choice", allow_free_text: true, choices } },
            ...textEvents(plan.question),
            { type: "done", finish_reason: "awaiting_input" },
          ]),
        };
      }
      case "fail": {
        const e = { code: plan.error.code, message: plan.error.message };
        return {
          events: finalize(turnId, [
            progress("interpreting"),
            { type: "directive", directive: directive(turnId, "report_failure", { reason_code: e.code }) },
            ...errorEvents(e, plan.text),
            { type: "done", finish_reason: "failed" },
          ]),
        };
      }
      case "prepare": {
        const record = prepare(session, plan.input);
        const action = record.action;
        const mode = action.confirmation.mode;
        const common = { capability_id: action.capability_id, preparation_id: action.preparation_id, confirmation: action.confirmation };
        const text = textEvents(plan.input.text ?? action.summary);
        if (mode === "none") {
          const { receipt } = commitPrepared(record);
          return {
            events: finalize(turnId, [
              progress("interpreting"),
              progress("tool_call", action.capability_id),
              { type: "directive", directive: directive(turnId, "report_outcome", { ...common, receipt_id: receipt.receipt_id }) },
              { type: "prepared_action", prepared_action: { ...action } },
              { type: "receipt", receipt },
              ...text,
              { type: "done", finish_reason: "completed" },
            ]),
          };
        }
        session.preparationId = action.preparation_id;
        if (mode === "step_up") {
          return {
            events: finalize(turnId, [
              progress("interpreting"),
              progress("tool_call", action.capability_id),
              { type: "directive", directive: directive(turnId, "request_step_up", { ...common, reason_code: "step_up_required" }) },
              { type: "prepared_action", prepared_action: { ...action } },
              confirmationPanel(action),
              ...text,
              { type: "done", finish_reason: "completed" },
            ]),
          };
        }
        return {
          events: finalize(turnId, [
            progress("interpreting"),
            progress("tool_call", action.capability_id),
            {
              type: "directive",
              directive: directive(turnId, "request_confirmation", {
                ...common,
                permitted_next_actions: [{ action: "confirm" }, { action: "reject" }],
              }),
            },
            { type: "prepared_action", prepared_action: { ...action } },
            confirmationPanel(action),
            ...text,
            { type: "done", finish_reason: "awaiting_confirmation", preparation_id: action.preparation_id },
          ]),
          grant: { preparation_id: action.preparation_id, token: record.secret ?? "" },
        };
      }
      case "http_error":
        throw new Error("http_error plans are handled before events are built");
    }
  };

  /** A confirmation or rejection arriving as a turn input: structured, never read from text. */
  const structuredTurn = (turnId: string, session: SessionRecord, input: Extract<TurnInput, { confirmation: unknown } | { reject: unknown }>): Built => {
    const scope = session.scope;
    const id = "confirmation" in input ? input.confirmation.preparation_id : input.reject.preparation_id;
    const record = ownedPreparation(scope, id);
    if (record instanceof Response) {
      return { events: finalize(turnId, [...errorEvents({ code: "scope_violation", message: "the preparation is not one of this session" }), { type: "done", finish_reason: "failed" }]) };
    }
    if ("reject" in input) {
      const action = effectiveState(record);
      if (action.state !== "prepared" && action.state !== "confirmed" && action.state !== "rejected") {
        return { events: finalize(turnId, [...errorEvents({ code: "conflict", message: `the preparation is ${action.state}` }), { type: "done", finish_reason: "failed" }]) };
      }
      action.state = "rejected";
      session.preparationId = undefined;
      return {
        events: finalize(turnId, [
          progress("interpreting"),
          { type: "directive", directive: directive(turnId, "acknowledge_cancel", { capability_id: action.capability_id }) },
          ...textEvents("Cancelled. Nothing was changed."),
          { type: "done", finish_reason: "cancelled" },
        ]),
      };
    }
    const failure = confirmPrepared(record, input.confirmation.token);
    if (failure !== undefined) {
      return {
        events: finalize(turnId, [
          progress("committing"),
          { type: "directive", directive: directive(turnId, "report_failure", { capability_id: record.action.capability_id, reason_code: failure.code }) },
          ...errorEvents(failure),
          { type: "done", finish_reason: "failed" },
        ]),
      };
    }
    const { receipt, error } = commitPrepared(record);
    session.preparationId = undefined;
    const events: Json[] = [
      progress("committing"),
      { type: "directive", directive: directive(turnId, "report_outcome", { capability_id: record.action.capability_id, preparation_id: id, receipt_id: receipt.receipt_id }) },
      { type: "receipt", receipt },
    ];
    if (error !== undefined) events.push({ type: "error", error });
    events.push(...textEvents(error === undefined ? "Done." : error.message), { type: "done", finish_reason: error === undefined ? "completed" : "failed" });
    return { events: finalize(turnId, events) };
  };

  const validateTurnBody = (body: unknown): TurnInput | Response => {
    const bad = (message: string) => problem("validation_failed", message);
    if (!isRecord(body)) return bad("the request body must be a JSON object");
    const known = ["text", "language", "choice_id", "confirmation", "reject", "attachments"];
    if (Object.keys(body).some((k) => !known.includes(k))) return bad("the request body has an unknown member");
    const kinds = ["text", "choice_id", "confirmation", "reject"].filter((k) => body[k] !== undefined);
    if (kinds.length !== 1) return bad("exactly one of text, choice_id, confirmation and reject is required");
    if (body["language"] !== undefined && typeof body["language"] !== "string") return bad("language must be a string");
    const kind = kinds[0];
    const attachments = body["attachments"];
    if (attachments !== undefined) {
      if (kind !== "text") return bad("attachments go with a text message");
      if (!Array.isArray(attachments) || attachments.length > 4) return bad("attachments must be a list of at most 4 file ids");
      if (attachments.some((id) => typeof id !== "string" || !/^fil_[0-9A-HJKMNP-TV-Z]{26}$/.test(id))) return bad("an attachment is not a file id");
    }
    if (kind === "text") {
      if (typeof body["text"] !== "string" || body["text"] === "") return bad("text must be a non-empty string");
      return { text: body["text"], ...(Array.isArray(attachments) && attachments.length > 0 && { attachments: attachments as string[] }) };
    }
    if (kind === "choice_id") return typeof body["choice_id"] === "string" && body["choice_id"] !== "" ? ({ choice_id: body["choice_id"] } as TurnInput) : bad("choice_id must be a non-empty string");
    if (kind === "confirmation") {
      const c = body["confirmation"];
      if (!isRecord(c) || typeof c["preparation_id"] !== "string" || !isConfirmationToken(c["token"]) || Object.keys(c).length !== 2) {
        return bad("confirmation must be {preparation_id, token}");
      }
      return { confirmation: { preparation_id: c["preparation_id"], token: c["token"] } };
    }
    const r = body["reject"];
    if (!isRecord(r) || typeof r["preparation_id"] !== "string" || Object.keys(r).length !== 1) return bad("reject must be {preparation_id}");
    return { reject: { preparation_id: r["preparation_id"] } };
  };

  const frame = (event: TurnEvent): string => `id: ${event.seq}\nevent: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`;

  const streamResponse = (built: Built, signal: AbortSignal, truncateAfter: number | undefined, retryHint = false): Response => {
    const frames = (truncateAfter === undefined ? built.events : built.events.slice(0, truncateAfter)).map(frame);
    if (retryHint) frames.unshift("retry: 3000\n\n");
    let chunks: Uint8Array[];
    if (chunkBytes > 0) {
      const all = encoder.encode(frames.join(""));
      chunks = [];
      for (let i = 0; i < all.length; i += chunkBytes) chunks.push(all.slice(i, i + chunkBytes));
    } else {
      chunks = frames.map((f) => encoder.encode(f));
    }
    let next = 0;
    const body = new ReadableStream<Uint8Array>({
      start: (controller) => {
        signal.addEventListener("abort", () => controller.error(signal.reason), { once: true });
      },
      pull: (controller) => {
        const chunk = chunks[next++];
        if (chunk === undefined) controller.close();
        else controller.enqueue(chunk);
      },
    });
    const headers: Record<string, string> = {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-store",
      "X-Accel-Buffering": "no",
      "X-Request-ID": `req_${randomSegment(9)}`,
    };
    if (built.grant !== undefined) headers[CONFIRMATION_TOKEN_HEADER] = built.grant.token;
    return new Response(body, { status: 200, headers });
  };

  const wantsStream = (request: Request): boolean =>
    (request.headers.get("Accept") ?? "").split(",").some((part) => part.split(";")[0]?.trim().toLowerCase() === "text/event-stream");

  const turnEndpoint = async (scope: Scope, sessionId: string, request: Request, body: unknown, truncateAfter: number | undefined): Promise<Response> => {
    if (sessionId !== scope.session_id) return levelViolation("session");
    const input = validateTurnBody(body);
    if (input instanceof Response) return input;
    const quarantined = quarantineRefusal();
    if (quarantined) return quarantined;
    const session = openSession(scope);
    if (session instanceof Response) return session;
    // The files are resolved together, under the scope, before anything runs: a refused turn runs no handler and is recorded nowhere.
    if ("attachments" in input && input.attachments !== undefined && !filesArea.attachable(scope, input.attachments)) {
      return problem("scope_violation", "the file reference does not resolve for this caller");
    }

    session.turnCount += 1;
    session.updatedAt = nowMs();
    const turnId = typedId("trn", nowMs());
    let built: Built;
    if ("confirmation" in input || "reject" in input) {
      built = structuredTurn(turnId, session, input);
    } else {
      const plan = await handler({
        input,
        session: { session_id: scope.session_id, application_id: scope.application_id, tenant_id: scope.tenant_id, workspace_id: scope.workspace_id, principal_id: scope.principal_id },
        turn_number: session.turnCount,
        pending: pendingOf(session),
      });
      if (plan.kind === "http_error") {
        session.turnCount -= 1;
        return json(plan.status, plan.error, { "X-Request-ID": `req_${randomSegment(9)}` });
      }
      built = planEvents(turnId, session, plan, scope);
    }
    turnBuffers.set(`${sessionKey(scope)}\u0000${turnId}`, { scope, events: built.events, createdAt: nowMs() });
    const said = built.events.map((event) => (event.type === "token" ? event.text : "")).join("");
    const act = built.events.find((event) => event.type === "directive");
    historyArea.record(scope, turnId, session.turnCount, "text" in input ? input.text : undefined, said, act?.type === "directive" ? (act.directive as { act?: string }).act : undefined);
    if (wantsStream(request)) return streamResponse(built, request.signal, truncateAfter);
    const document: Json = { turn_id: turnId, events: built.events };
    if (built.grant !== undefined) document["confirmation"] = built.grant;
    return json(200, document);
  };

  // ---- reading a turn again ----

  const CURSOR = /^[0-9]{1,10}$/;

  const wantsWebSocket = (request: Request): boolean =>
    [request.headers.get("Upgrade"), request.headers.get(FAKE_UPGRADE_HEADER)].some((value) => (value ?? "").trim().toLowerCase() === "websocket");

  const eventsEndpoint = (scope: Scope, params: Record<string, string>, request: Request, url: URL, failure: InjectedFailure | undefined): Response => {
    if (params["session_id"] !== scope.session_id) return levelViolation("session");
    const quarantined = quarantineRefusal();
    if (quarantined) return quarantined;
    // One cursor, from the query or from Last-Event-ID; with both, the later one wins.
    let cursor: number | undefined;
    for (const raw of [url.searchParams.get("cursor"), request.headers.get("Last-Event-ID")]) {
      if (raw === null) continue;
      if (!CURSOR.test(raw)) return problem("validation_failed", "the cursor must be the seq of the last event received", { details: { reason: "bad_cursor" } });
      cursor = Math.max(cursor ?? 0, Number(raw));
    }
    // A turn that does not exist, one of another principal and one past its retention are the same refusal.
    const turn = turnBuffers.get(`${sessionKey(scope)}\u0000${params["turn_id"] ?? ""}`);
    if (turn === undefined || turn.scope.principal_id !== scope.principal_id || nowMs() - turn.createdAt > turnRetentionMs) {
      return problem("scope_violation", "the turn is not visible in this scope", { details: { levels: ["turn"], reason: "turn_not_visible" } });
    }
    const total = turn.events.length;
    if (cursor !== undefined && cursor > total - 1) {
      return problem("validation_failed", "the cursor is ahead of the stream", { details: { reason: "cursor_ahead" } });
    }
    const firstRetained = Number.isFinite(replayEvents) ? Math.max(0, total - replayEvents) : 0;
    if ((cursor === undefined && firstRetained > 0) || (cursor !== undefined && cursor + 1 < firstRetained)) {
      return problem("expired", "the events after the cursor are no longer kept", { status: 410, details: { reason: "cursor_expired" } });
    }
    const events = turn.events.slice(cursor === undefined ? 0 : cursor + 1);
    const websocket = wantsWebSocket(request);
    if (events.length === 0 && !websocket) return new Response(null, { status: 204, headers: { "Cache-Control": "no-store" } });
    if (websocket) {
      if (failure?.refuseWebSocket === true) return problem("unavailable", "the upgrade was refused");
      const sent = failure?.truncateAfterEvents === undefined ? events : events.slice(0, failure.truncateAfterEvents);
      const cut = failure?.truncateAfterEvents !== undefined && sent.length < events.length;
      const script: FakeSocketScript = {
        messages: sent.map((event) => JSON.stringify(event)),
        close: cut || failure?.closeCode !== undefined ? { code: failure?.closeCode ?? 1006, reason: "" } : { code: 1000, reason: "done" },
      };
      return json(200, script, { [FAKE_WEBSOCKET_HEADER]: "1" });
    }
    return streamResponse({ events }, request.signal, failure?.truncateAfterEvents, true);
  };

  // ---- routing ----

  const handleAuthenticated = async (operation: CoreOperationId, scope: Scope, params: Record<string, string>, request: Request, url: URL, body: unknown, raw: Uint8Array<ArrayBuffer>, failure: InjectedFailure | undefined): Promise<Response> => {
    switch (operation) {
      case "streamTurnEvents":
        return eventsEndpoint(scope, params, request, url, failure);
      case "uploadFile":
      case "getFile":
      case "deleteFile":
      case "downloadFile":
      case "previewFile":
        return filesArea.handle(operation, scope, params, request, raw);
      case "listHistorySegments":
      case "clearHistorySession":
      case "searchHistory":
      case "exportHistory":
      case "cleanupHistory":
        return historyArea.handle(operation, scope, params, url.searchParams, body);
      case "getUserSettings":
      case "putUserSettings":
      case "getTenantSettings":
      case "putTenantSettings":
      case "getDeploymentSettings":
      case "putDeploymentSettings":
      case "getEffectiveSettings":
      case "listSettingsProfiles":
      case "getSettingsCapabilities":
      case "getSettingsIntegrationAudit":
      case "getSettingsProviderHealth":
        return settingsArea.handle(operation, scope, body);
      case "getProviderCatalog":
      case "getPrincipalLlm":
      case "putPrincipalLlmRole":
      case "deletePrincipalLlmRole":
      case "putPrincipalProviderCredential":
      case "deletePrincipalProviderCredential":
      case "ackPrincipalProviderBillingNotice":
      case "startPrincipalOAuth":
      case "completePrincipalOAuth":
      case "getPrincipalOAuthFlow":
      case "cancelPrincipalOAuthFlow":
      case "startErasure":
      case "getErasureJob":
      case "getMeUsage":
      case "getMeBudget":
      case "listMemory":
      case "createMemory":
      case "patchMemory":
      case "deleteMemory":
      case "confirmMemory":
      case "rejectMemory": {
        const answered = meMemoryArea.handle(operation, scope, params, body);
        return answered ?? problem("internal", "the fake server does not implement this operation");
      }
      case "createExport": {
        const answered = exportsArea.handle(operation, scope, params, body);
        return answered ?? problem("internal", "the fake server does not implement this operation");
      }
      case "createSession": {
        if (body !== undefined && !(isRecord(body) && Object.keys(body).length === 0)) return problem("validation_failed", "the request body, when present, must be {}");
        const quarantined = quarantineRefusal();
        if (quarantined) return quarantined;
        const session = openSession(scope);
        return session instanceof Response ? session : json(200, sessionDoc(session));
      }
      case "getSession":
      case "getPendingState":
      case "endSession": {
        if (params["session_id"] !== scope.session_id) return levelViolation("session");
        const quarantined = quarantineRefusal();
        if (quarantined) return quarantined;
        if (operation === "endSession") {
          const existing = sessions.get(sessionKey(scope));
          if (existing !== undefined && existing.status === "active") {
            if (existing.preparationId !== undefined) {
              const record = preparations.get(existing.preparationId);
              if (record !== undefined && (record.action.state === "prepared" || record.action.state === "confirmed")) record.action.state = "invalidated";
            }
            existing.status = "ended";
            existing.preparationId = undefined;
            existing.clarification = undefined;
          } else if (existing === undefined) {
            sessions.set(sessionKey(scope), { scope, status: "ended", turnCount: 0, createdAt: nowMs(), updatedAt: nowMs() });
          }
          return json(200, { session_id: scope.session_id, status: "ended" });
        }
        const session = openSession(scope);
        if (session instanceof Response) return session;
        return operation === "getSession" ? json(200, sessionDoc(session)) : json(200, { pending: pendingOf(session) });
      }
      case "sendTurn":
        return turnEndpoint(scope, params["session_id"] ?? "", request, body, failure?.truncateAfterEvents);
      case "getAction":
      case "rejectAction":
      case "getActionReceipt": {
        const quarantined = quarantineRefusal();
        if (quarantined) return quarantined;
        const record = ownedPreparation(scope, params["preparation_id"] ?? "");
        if (record instanceof Response) return record;
        const action = effectiveState(record);
        if (operation === "getAction") return json(200, action);
        if (operation === "getActionReceipt") {
          return record.receipt === undefined ? problem("conflict", "the preparation has no receipt yet") : json(200, record.receipt);
        }
        if (action.state === "rejected") return json(200, action);
        if (action.state !== "prepared" && action.state !== "confirmed") return problem("conflict", `the preparation is ${action.state}`);
        action.state = "rejected";
        const session = sessions.get(sessionKey(scope));
        if (session?.preparationId === action.preparation_id) session.preparationId = undefined;
        return json(200, action);
      }
      case "confirmAction":
        return confirmEndpoint(scope, params["preparation_id"] ?? "", body);
      case "registerApplication":
        return registerEndpoint(scope, request, raw);
      case "getApplication": {
        if (params["application_id"] !== scope.application_id) return levelViolation("application");
        const doc: Application = { application_id: scope.application_id, status: appStatus, packages: packagesFor() };
        if (appStatus === "active") doc.health = { status: "ok", application_version: "1.0.0" };
        return json(200, doc);
      }
      case "listCapabilities": {
        const quarantined = quarantineRefusal();
        if (quarantined) return quarantined;
        // Exact match: a capability the token does not list is not visible (and there is no wildcard).
        const visible = capabilities.filter((c) => scope.capability_scopes.includes(c.id));
        const list: CapabilityList = { capabilities: visible.map((c) => ({ ...c })) };
        return json(200, list);
      }
      case "getHealth": {
        const report = readyReport();
        const health: Health = { ...report, application: { application_id: scope.application_id, status: appStatus } };
        return json(report.ready ? 200 : 503, health, report.ready ? {} : { "Retry-After": "1" });
      }
      default:
        return problem("internal", "the fake server does not implement this operation");
    }
  };

  const readyReport = (): ReadyReport =>
    ready
      ? { status: "ok", ready: true, roles: [{ role: "controller", configured: true, status: "ok" }], checks: [{ name: "store", ok: true }] }
      : {
          status: "unavailable",
          ready: false,
          roles: [{ role: "controller", configured: true, status: "unavailable", reason: "the model runtime is not answering" }],
          checks: [{ name: "store", ok: true }, { name: "controller", ok: false, reason: "unavailable" }],
        };

  const registerEndpoint = async (scope: Scope, request: Request, bytes: Uint8Array<ArrayBuffer>): Promise<Response> => {
    if (!scope.capability_scopes.includes("applications.register")) {
      return problem("permission_denied", "the context token does not carry the applications.register scope");
    }
    if ((request.headers.get("Content-Type") ?? "").split(";")[0]?.trim().toLowerCase() !== "application/gzip") {
      return problem("validation_failed", "the package must be sent as application/gzip", { status: 415 });
    }
    if (bytes.length < 2 || bytes[0] !== 0x1f || bytes[1] !== 0x8b) return problem("validation_failed", "the body is not a gzip archive", { status: 422 });
    const digest = new Uint8Array(await globalThis.crypto.subtle.digest("SHA-256", bytes));
    const hash = Array.from(digest, (b) => b.toString(16).padStart(2, "0")).join("");
    const existing = registered.get(hash);
    const record = existing ?? packageRecord("quarantined", hash);
    if (existing === undefined) registered.set(hash, record);
    const registration: Registration = { application_id: scope.application_id, package: record, created: existing === undefined, findings: [] };
    return json(existing === undefined ? 201 : 200, registration);
  };

  const record = (entry: RecordedRequest): void => {
    recorded.push(entry);
  };

  const handle = async (request: Request): Promise<Response> => {
    const url = new URL(request.url);
    if (url.origin !== base.origin) throw new TypeError(`fetch failed: ${url.origin} is not the fake server (${base.origin})`);
    if (request.signal.aborted) throw request.signal.reason;
    let path = url.pathname;
    if (prefix !== "" && (path === prefix || path.startsWith(`${prefix}/`))) path = path.slice(prefix.length) || "/";
    else if (prefix !== "") return problem("validation_failed", "not found", { status: 404 });

    const raw = request.method === "GET" || request.method === "DELETE" ? new Uint8Array() : new Uint8Array(await request.arrayBuffer());
    const text = new TextDecoder().decode(raw);
    let body: unknown;
    if (text !== "" && (request.headers.get("Content-Type") ?? "").includes("json")) {
      try {
        body = JSON.parse(text);
      } catch {
        body = undefined;
      }
    }
    const entry: RecordedRequest = {
      operation: undefined,
      method: request.method,
      path,
      status: 0,
      accept: request.headers.get("Accept"),
      contentType: request.headers.get("Content-Type"),
      authenticated: request.headers.has("Authorization"),
      tokenId: undefined,
      body: redact(body),
    };
    const finish = (response: Response): Response => {
      entry.status = response.status;
      record(entry);
      return response;
    };

    let operation: CoreOperationId | undefined;
    let params: Record<string, string> = {};
    let pathKnown = false;
    for (const [id, spec] of Object.entries(OPERATIONS) as Array<[CoreOperationId, (typeof OPERATIONS)[CoreOperationId]]>) {
      const matched = matchPath(spec.path, path);
      if (matched === undefined) continue;
      pathKnown = true;
      if (spec.method !== request.method) continue;
      operation = id;
      params = matched;
      break;
    }
    entry.operation = operation;
    if (operation === undefined) {
      return finish(pathKnown ? problem("validation_failed", "method not allowed", { status: 405 }) : problem("validation_failed", "not found", { status: 404 }));
    }
    if (!OPERATIONS[operation].authenticated) {
      if (operation === "getLiveness") return finish(json(200, { status: "ok" }));
      const report = readyReport();
      return finish(json(report.ready ? 200 : 503, report, report.ready ? {} : { "Retry-After": "1" }));
    }
    const auth = authenticate(request);
    entry.tokenId = auth.tokenId;
    if ("refusal" in auth) return finish(auth.refusal);

    const index = failures.findIndex((f) => f.operation === undefined || f.operation === operation);
    const injected = index === -1 ? undefined : failures[index];
    if (injected !== undefined) {
      injected.times = (injected.times ?? 1) - 1;
      if (injected.times <= 0) failures = failures.filter((f) => f !== injected);
      if (injected.networkError === true) {
        record(entry);
        throw new TypeError("fetch failed (injected network error)");
      }
      if (injected.status !== undefined && injected.error !== undefined) {
        return finish(json(injected.status, injected.error, injected.headers ?? {}));
      }
    }
    return finish(await handleAuthenticated(operation, auth.scope, params, request, url, body, raw, injected));
  };

  const fetchFake = async (input: string, init?: RequestInit): Promise<Response> => handle(new Request(input, init));

  const listen: FakeAlixAIServer["listen"] = async () => {
    const http = await import("node:http");
    const server = http.createServer((req, res) => {
      void (async () => {
        const abort = new AbortController();
        res.on("close", () => {
          if (!res.writableEnded) abort.abort();
        });
        const chunks: Uint8Array[] = [];
        for await (const chunk of req) chunks.push(chunk as Uint8Array);
        const headers = new Headers();
        for (const [name, value] of Object.entries(req.headers)) {
          if (Array.isArray(value)) value.forEach((v) => headers.append(name, v));
          else if (value !== undefined) headers.set(name, value);
        }
        const method = req.method ?? "GET";
        const hasBody = method !== "GET" && method !== "HEAD" && method !== "DELETE";
        const request = new Request(`${base.origin}${req.url ?? "/"}`, {
          method,
          headers,
          signal: abort.signal,
          ...(hasBody && chunks.length > 0 && { body: Buffer.concat(chunks) }),
        });
        let response: Response;
        try {
          response = await handle(request);
        } catch {
          res.statusCode = 502;
          res.end();
          return;
        }
        res.writeHead(response.status, Object.fromEntries(response.headers));
        if (response.body === null) {
          res.end();
          return;
        }
        const reader = response.body.getReader();
        try {
          for (;;) {
            const { done, value } = await reader.read();
            if (done) break;
            res.write(value);
          }
        } catch {
          // the client went away or the stream was cut: nothing more to send
        }
        res.end();
      })();
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    const port = typeof address === "object" && address !== null ? address.port : 0;
    return {
      baseUrl: `http://127.0.0.1:${port}${prefix}`,
      close: () =>
        new Promise<void>((resolve, reject) => {
          server.closeAllConnections();
          server.close((error) => (error ? reject(error) : resolve()));
        }),
    };
  };

  return {
    baseUrl,
    fetch: fetchFake,
    issueToken,
    onTurn: (next) => {
      handler = next ?? options.turnHandler ?? ((turn) => reply(`fake: ${"text" in turn.input ? turn.input.text : turn.input.choice_id}`));
    },
    failNext: (failure) => {
      failures.push({ ...failure });
    },
    advanceTime: (ms) => {
      offset += ms;
    },
    setApplicationStatus: (status) => {
      appStatus = status;
    },
    setReady: (value) => {
      ready = value;
    },
    get requests() {
      return recorded;
    },
    session: (sessionId) => {
      for (const s of sessions.values()) if (s.scope.session_id === sessionId) return sessionDoc(s);
      return undefined;
    },
    preparations: () => [...preparations.values()].map((r) => ({ ...effectiveState(r) })),
    webSocket: createFakeWebSocketFactory(handle),
    files: () => filesArea.list(),
    history: () => historyArea.rows(),
    get tokensRedeemed() {
      return redeemed.size;
    },
    listen,
    reset: () => {
      sessions.clear();
      preparations.clear();
      turnBuffers.clear();
      filesArea.reset();
      historyArea.reset();
      settingsArea.reset();
      redeemed.clear();
      registered.clear();
      failures = [];
      recorded = [];
      offset = 0;
      ready = true;
      appStatus = options.applicationStatus ?? "active";
      capabilities = options.capabilities ?? defaultCapabilities();
    },
  };
};
