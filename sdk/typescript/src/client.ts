import type {
  ActionOutcome,
  Application,
  CapabilityList,
  Health,
  Liveness,
  PendingState,
  ReadyReport,
  Registration,
  Session,
  SessionEnded,
  TurnInput,
  TurnResult,
} from "./api-types.js";
import { createExportsApi } from "./exports/client.js";
import type { ExportsApi } from "./exports/client.js";
import { createFilesApi } from "./files.js";
import type { FilesApi } from "./files.js";
import { createHistoryApi } from "./history.js";
import type { HistoryApi } from "./history.js";
import { createMeApi } from "./me/client.js";
import type { MeApi } from "./me/client.js";
import { createMemoryApi } from "./memory/client.js";
import type { MemoryApi } from "./memory/client.js";
import { createOpsApi } from "./ops/client.js";
import type { OpsApi } from "./ops/client.js";
import { createSettingsApi } from "./settings.js";
import type { SettingsApi } from "./settings.js";
import { createConversation } from "./conversation.js";
import type { Conversation, ConversationOptions } from "./conversation.js";
import { MAX_ATTACHMENTS, createMessageStream, withAttachments } from "./compose.js";
import type { MessageStream, OutgoingMessage } from "./compose.js";
import { isAlixAIError, sdkError } from "./errors.js";
import { createTurnSequence, parseTurnEvent } from "./events.js";
import type { ActionReceipt, PreparedAction } from "./gen/actions/index.js";
import {
  isActionOutcome,
  isActionReceipt,
  isApplication,
  isCapabilityList,
  isHealth,
  isLiveness,
  isPendingResponse,
  isPreparedAction,
  isReadyReport,
  isRegistration,
  isSession,
  isSessionEnded,
} from "./guards.js";
import { createCaller, timeout, unexpected } from "./call.js";
import type { CallExtra, Caller } from "./call.js";
import type { OperationId } from "./operations.js";
import type { CallOptions, CallScope, ContextTokenProvider, SendSpec } from "./transport.js";
import { createTurnStream } from "./turn.js";
import type { Opened, ReconnectPolicy, TurnStream } from "./turn.js";
import { createSocketReader } from "./ws.js";
import type { StreamTransport, WebSocketFactory } from "./ws.js";
import { FILE_ID, confirmationToken, isConfirmationToken, pathId } from "./validate.js";

export interface ClientOptions {
  /** The AlixAI service, e.g. "https://alixai.example.com" or "http://127.0.0.1:8080". A path prefix is kept. */
  baseUrl: string;
  /**
   * Supplies a fresh single-use context token for each request and each event-stream open. The SDK
   * never mints, decodes, caches or retries with a token; it holds one only for the call it is for.
   */
  getContextToken: ContextTokenProvider;
  /** A fetch implementation. Defaults to the global `fetch`. Pass the fake server's `fetch` in tests. */
  fetch?: (input: string, init?: RequestInit) => Promise<Response>;
  /** Deadline for an ordinary request, in milliseconds. Default 30000; 0 disables it. */
  timeoutMs?: number;
  /**
   * Deadline for a turn or a confirmation, in milliseconds. These wait on the model and on the host
   * application's commit, so the default is long: 600000 (the service's own limit). 0 disables it.
   */
  turnTimeoutMs?: number;
  /** Allow plain http to a host that is not loopback. The context token is a bearer credential: leave this off. */
  allowInsecureHttp?: boolean;
  /**
   * Opens a client WebSocket with headers, so that an event stream can be read over a WebSocket (see `ws.ts`). The gateway
   * authenticates with the Authorization header only, which a browser WebSocket cannot set, so the SDK never opens a socket by itself:
   * `(url, { protocols, headers }) => new WebSocket(url, { protocols, headers })` on Node 22+, Deno and Bun. Without it the stream is read over
   * server-sent events, and the host sees no difference.
   */
  webSocket?: WebSocketFactory;
  /**
   * What a stream does when its connection is lost before the turn's `done` event. Off by default (a lost stream is
   * `uncertain_outcome`); `true` or an object resumes from the last cursor with a fresh context token per connection, which only
   * reads: the turn is never run again. A per-call `resume` overrides it.
   */
  resume?: ResumeOptions | boolean;
}

/** How a stream reconnects after a lost connection. */
export interface ResumeOptions {
  /** How many connections may be lost in a row, without an event in between, before the stream gives up. Default 3. */
  maxAttempts?: number;
  /**
   * The wait before the first reconnect, in milliseconds; it doubles each attempt in a row, to at most 5000, plus up to a fifth at
   * random. A server's Retry-After that asks for longer is honoured, up to 60 seconds. Default 250.
   */
  backoffMs?: number;
  /** `auto` (default): a WebSocket when the client has a `webSocket` factory, falling back to server-sent events with a fresh token; or force one. */
  transport?: StreamTransport;
}

/** Options of a call that streams: the usual ones plus the reconnect policy of that stream. */
export interface StreamOptions extends CallOptions {
  /** Overrides the client's `resume` option for this stream; `false` turns reconnecting off. */
  resume?: ResumeOptions | boolean;
}

/** Options of `streamTurnEvents`. */
export interface StreamEventsOptions extends StreamOptions {
  /** The `seq` of the last event already received; the stream continues with the next one. Absent means the start of the turn. */
  cursor?: number;
  /** Which transport to read from. Default `auto`. */
  transport?: StreamTransport;
}

export interface AlixAIClient {
  // ---- sessions ----
  /** Opens the session the context token names (creating it on first sight). Opening an existing one answers the same way. */
  createSession: (options?: CallOptions) => Promise<Session>;
  /** Resumes a session, with its structured pending state. */
  getSession: (sessionId: string, options?: CallOptions) => Promise<Session>;
  /** Ends a session. A prepared action it waits on is invalidated; nothing is ever committed by ending. */
  endSession: (sessionId: string, options?: CallOptions) => Promise<SessionEnded>;
  /** What the active goal waits for, or null when there is none. The confirmation secret is not part of it. */
  getPendingState: (sessionId: string, options?: CallOptions) => Promise<PendingState | null>;

  // ---- turns ----
  /**
   * Sends a turn and streams its events (server-sent events over a POST). Returns at once; the
   * request is already on its way. Iterate the result with `for await`, or read it with `result()`.
   * Never throws synchronously: every failure surfaces through the stream.
   */
  sendTurn: (sessionId: string, input: TurnInput, options?: StreamOptions) => TurnStream;
  /**
   * Composes a message with attachments (images, audio, video, files, at most 4) and sends it: each attachment is uploaded first
   * (`client.files.upload`, with the session the message is for), then one text turn is sent that names the uploaded file ids in its
   * `attachments`; the core resolves them under the turn's scope before the model runs. Everything is checked before the first upload
   * is made. Returns at once, like `sendTurn`; `attachments` resolves with the references. If an upload fails nothing is sent and the
   * files already stored are deleted.
   */
  sendMessage: (sessionId: string, message: OutgoingMessage, options?: StreamOptions) => MessageStream;
  /**
   * Reads the events of a turn again, from a cursor, so that a client that lost its connection continues without the turn being run
   * again: the route only reads. Same `TurnStream` as `sendTurn`, over server-sent events or a WebSocket (see `webSocket`), with a
   * fresh context token per connection. A cursor behind the retained window is `expired` (read the outcome from the session and the
   * actions API), a turn that is not the caller's is `scope_violation`, and a cursor at the end of a finished turn yields an empty stream.
   * The confirmation secret is never part of it: it is shown once, to the turn that made it.
   */
  streamTurnEvents: (sessionId: string, turnId: string, options?: StreamEventsOptions) => TurnStream;
  /** Sends a turn and returns it as one JSON document (no streaming). Same checks as `sendTurn`. */
  sendTurnJson: (sessionId: string, input: TurnInput, options?: CallOptions) => Promise<TurnResult>;

  // ---- actions ----
  /** The prepared action with its current state. */
  getAction: (preparationId: string, options?: CallOptions) => Promise<PreparedAction>;
  /**
   * Presents the single-use confirmation secret and commits the action. A commit the host refused
   * (409) or whose outcome is partial or unknown (500) throws an AlixAIError that carries the
   * `receipt` and `preparation`; a reused secret is `replayed`.
   */
  confirmAction: (preparationId: string, token: string, options?: CallOptions) => Promise<ActionOutcome>;
  /** Declines a preparation. Nothing is sent to the application. */
  rejectAction: (preparationId: string, options?: CallOptions) => Promise<PreparedAction>;
  /** The durable receipt of an action. `conflict` while it has no outcome yet. */
  getActionReceipt: (preparationId: string, options?: CallOptions) => Promise<ActionReceipt>;

  // ---- applications ----
  /** The caller's application with its installed packages and states. */
  getApplication: (applicationId: string, options?: CallOptions) => Promise<Application>;
  /** The capabilities the caller may see, with the effective risk level of each. */
  listCapabilities: (options?: CallOptions) => Promise<CapabilityList>;
  /** Installs a signed `.tar.gz` application package into quarantine. Needs the `applications.register` scope. */
  registerApplication: (archive: Uint8Array | ArrayBuffer | Blob, options?: CallOptions) => Promise<Registration>;

  // ---- files, history, settings, me, memory, exports ----
  /** Scoped files: upload, inspect, download, preview, delete. */
  readonly files: FilesApi;
  /** Conversation history, search, export and clearing. */
  readonly history: HistoryApi;
  /** User, tenant and deployment settings, profiles and privacy preferences. */
  readonly settings: SettingsApi;
  /**
   * Per-principal LLM account and catalog (ADR 0041): `me.catalog`, `me.llm.*`, `me.providers.*`.
   * `me.providers.submitCredential` is host-server only — never call it from browser code.
   */
  readonly me: MeApi;
  /** Long-term memory (ADR 0042): list, create, patch, delete, confirm, reject. */
  readonly memory: MemoryApi;
  /** Downloadable files from a turn's verified blocks (1.2.0): `exports.create`. Needs `exports.create`. */
  readonly exports: ExportsApi;
  /** Operator erasure over HTTP (1.3.0, ADR 0036): `ops.erasure.start` / `status`. Server-side only. */
  readonly ops: OpsApi;

  // ---- conversations ----
  /**
   * A client bound to a session: every request tells the token provider which session it is for (`ContextTokenRequest.sessionId`),
   * including those whose path names none (an upload, a history search). A session is the one its context token names, so switching
   * conversations is asking the host for tokens of another session: bind one client per conversation.
   */
  forSession: (sessionId: string) => AlixAIClient;
  /**
   * A conversation over a session: sends turns, keeps what was said, and offers stop, regenerate, edit-and-resubmit and resume.
   * See `Conversation`; one conversation per session, so open another to switch.
   */
  conversation: (sessionId: string, options?: ConversationOptions) => Conversation;

  // ---- operations ----
  /** The readiness report plus the caller's application. A service that is not ready returns its report; it does not throw. */
  getHealth: (options?: CallOptions) => Promise<Health>;
  /** Liveness: unauthenticated, no context token is requested. */
  getLiveness: (options?: CallOptions) => Promise<Liveness>;
  /** Readiness: unauthenticated. A service that is not ready returns its report; it does not throw. */
  getReadiness: (options?: CallOptions) => Promise<ReadyReport>;
}

const DEFAULT_TURN_TIMEOUT_MS = 600_000;
/** An upload carries a body, so its deadline is longer than an ordinary request's. */
const DEFAULT_UPLOAD_TIMEOUT_MS = 120_000;

/**
 * Checks a turn input before anything is sent: exactly one of text, choice_id, confirmation and
 * reject, with nothing else. A malformed input must not cost a single-use context token. Returns the
 * request body, built from known members only.
 */
const turnBody = (input: unknown): string => {
  const bad = (reason: string) => sdkError("validation_failed", `invalid turn input: ${reason}`, { details: { argument: "input" } });
  if (typeof input !== "object" || input === null || Array.isArray(input)) throw bad("it must be an object");
  const record = input as Record<string, unknown>;
  const known = ["text", "choice_id", "confirmation", "reject", "language", "attachments"];
  for (const key of Object.keys(record)) if (!known.includes(key)) throw bad(`unknown member "${key}"`);
  const kinds = ["text", "choice_id", "confirmation", "reject"].filter((key) => record[key] !== undefined);
  if (kinds.length !== 1) throw bad("exactly one of text, choice_id, confirmation and reject is required");
  const language = record["language"];
  if (language !== undefined && (typeof language !== "string" || language === "")) throw bad("language must be a non-empty string");
  const attachments = record["attachments"];
  if (attachments !== undefined) {
    // Files belong to a text message; the list is counted as given (the core counts a repeated id once, but limits the list).
    if (kinds[0] !== "text") throw bad("attachments go with a text message");
    if (!Array.isArray(attachments) || attachments.length > MAX_ATTACHMENTS) throw bad(`attachments must be a list of at most ${MAX_ATTACHMENTS} file ids`);
    for (const id of attachments) if (typeof id !== "string" || !FILE_ID.test(id)) throw bad("an attachment is not a file id");
  }

  switch (kinds[0]) {
    case "text": {
      const text = record["text"];
      if (typeof text !== "string" || text === "") throw bad("text must be a non-empty string");
      return JSON.stringify({
        text,
        ...(language !== undefined && { language }),
        ...(Array.isArray(attachments) && attachments.length > 0 && { attachments }),
      });
    }
    case "choice_id": {
      const choice = record["choice_id"];
      if (typeof choice !== "string" || choice === "") throw bad("choice_id must be a non-empty string");
      return JSON.stringify(language === undefined ? { choice_id: choice } : { choice_id: choice, language });
    }
    case "confirmation": {
      if (language !== undefined) throw bad("language does not apply to a confirmation");
      const c = record["confirmation"] as { preparation_id?: unknown; token?: unknown } | null;
      if (typeof c !== "object" || c === null) throw bad("confirmation must be an object");
      const preparationId = pathId("confirmation.preparation_id", c.preparation_id);
      if (!isConfirmationToken(c.token)) throw bad("confirmation.token is malformed");
      return JSON.stringify({ confirmation: { preparation_id: preparationId, token: c.token } });
    }
    default: {
      if (language !== undefined) throw bad("language does not apply to a rejection");
      const r = record["reject"] as { preparation_id?: unknown } | null;
      if (typeof r !== "object" || r === null) throw bad("reject must be an object");
      return JSON.stringify({ reject: { preparation_id: pathId("reject.preparation_id", r.preparation_id) } });
    }
  }
};

const isJsonTurn = (value: unknown): value is { turn_id: string; events: unknown[]; confirmation?: unknown } =>
  typeof value === "object" &&
  value !== null &&
  typeof (value as { turn_id?: unknown }).turn_id === "string" &&
  Array.isArray((value as { events?: unknown }).events) &&
  (value as { events: unknown[] }).events.length > 0;

// /v1/health and /readyz answer 503 with the same report when the service is not ready: that is
// information, so the report is returned for both statuses.
const notReadyIsAReport = (status: number): boolean => status === 503;

/**
 * Creates a client for one AlixAI service.
 *
 * The SDK has no runtime dependencies: it uses `fetch` and a small server-sent-events reader over
 * `ReadableStream`, so it runs in Node 20 and later and in browsers. Credentials are held in memory
 * only, for the length of one call.
 */
export const createClient = (options: ClientOptions): AlixAIClient => {
  if (typeof options !== "object" || options === null) throw sdkError("validation_failed", "createClient needs its options");
  const turnTimeout = timeout(options.turnTimeoutMs, DEFAULT_TURN_TIMEOUT_MS, "turnTimeoutMs");
  const webSocket = options.webSocket;
  if (webSocket !== undefined && typeof webSocket !== "function") {
    throw sdkError("validation_failed", "webSocket must be a function that opens a client WebSocket with headers", { details: { argument: "webSocket" } });
  }
  resumeSettings(options.resume, "resume");
  return build(createCaller(options), options, turnTimeout);
};

/** The resume options checked and filled in; undefined when resuming is off. */
const resumeSettings = (
  value: ResumeOptions | boolean | undefined,
  name: string,
): { maxAttempts: number; backoffMs: number; transport: StreamTransport } | undefined => {
  if (value === undefined || value === false) return undefined;
  const config: ResumeOptions = value === true ? {} : value;
  if (typeof config !== "object" || config === null) throw sdkError("validation_failed", `${name} must be a boolean or an object`, { details: { argument: name } });
  const maxAttempts = config.maxAttempts ?? 3;
  const backoffMs = config.backoffMs ?? 250;
  const transport = config.transport ?? "auto";
  if (!Number.isInteger(maxAttempts) || maxAttempts < 1 || maxAttempts > 20) {
    throw sdkError("validation_failed", `${name}.maxAttempts must be an integer from 1 to 20`, { details: { argument: name } });
  }
  if (typeof backoffMs !== "number" || !Number.isFinite(backoffMs) || backoffMs < 0) {
    throw sdkError("validation_failed", `${name}.backoffMs must be a non-negative number of milliseconds`, { details: { argument: name } });
  }
  if (transport !== "auto" && transport !== "sse" && transport !== "websocket") {
    throw sdkError("validation_failed", `${name}.transport must be auto, sse or websocket`, { details: { argument: name } });
  }
  return { maxAttempts, backoffMs, transport };
};

const CURSOR_MAX = 9_999_999_999;

const build = (caller: Caller, options: ClientOptions, turnTimeout: number): AlixAIClient => {
  const transport = caller.transport;
  const webSocket = options.webSocket;
  const files = createFilesApi(caller, DEFAULT_UPLOAD_TIMEOUT_MS);

  /** One JSON request/response: scope, send, parse, shape check, release. */
  const json = <T>(
    operation: OperationId,
    params: Record<string, string>,
    guard: (value: unknown) => value is T,
    what: string,
    call: CallOptions | undefined,
    extra: CallExtra = {},
  ): Promise<T> => caller.json(operation, params, guard, what, call, extra);

  const noParams: Record<string, string> = {};

  /**
   * Opens the event stream of a turn from a cursor, over a WebSocket when the host supplied a factory and the preference allows it,
   * and over server-sent events otherwise. A WebSocket that cannot be opened (a refused upgrade, a network failure, a runtime that
   * cannot send the Authorization header) falls back to SSE with a fresh token, which also reports a typed refusal properly: the
   * WebSocket API does not expose the status of a refused upgrade.
   */
  const openEvents = async (
    sessionId: string,
    turnId: string,
    cursor: number | undefined,
    preference: StreamTransport,
    scope: CallScope,
  ): Promise<Opened> => {
    const spec: SendSpec = {
      operation: "streamTurnEvents",
      params: { session_id: sessionId, turn_id: turnId },
      accept: "text/event-stream",
      ...(cursor !== undefined && { query: { cursor: String(cursor) } }),
    };
    if (preference !== "sse") {
      if (webSocket === undefined) {
        if (preference === "websocket") {
          throw sdkError("validation_failed", "the websocket transport needs the webSocket option of createClient", { details: { argument: "transport" } });
        }
      } else {
        try {
          return { reader: createSocketReader(await transport.socket(spec, scope, webSocket)), via: "websocket" };
        } catch (error) {
          if (scope.signal.aborted) throw scope.signal.reason;
          const handshake = isAlixAIError(error) && error.details?.["phase"] === "websocket_handshake";
          if (preference === "websocket" || !handshake) throw error;
        }
      }
    }
    const response = await transport.send(spec, scope);
    if (response.status === 204) {
      await response.body?.cancel().catch(() => undefined);
      return "complete";
    }
    return response;
  };

  /** The reconnect policy of a stream, from the call's option or the client's. */
  const reconnectFor = (sessionId: string, call: StreamOptions | undefined, scope: CallScope, fallbackTransport?: StreamTransport): ReconnectPolicy | undefined => {
    const settings = resumeSettings(call?.resume === undefined ? options.resume : call.resume, "resume");
    if (settings === undefined) return undefined;
    return {
      maxAttempts: settings.maxAttempts,
      backoffMs: settings.backoffMs,
      reopen: ({ turnId, cursor }) => openEvents(sessionId, turnId, cursor, fallbackTransport ?? settings.transport, scope),
    };
  };

  const failedStream = (operation: OperationId, error: unknown): TurnStream =>
    createTurnStream({ start: () => Promise.reject(error), scope: caller.scope(operation, undefined, 0), mapFailure: (e) => e });

  const streamTurnEvents: AlixAIClient["streamTurnEvents"] = (sessionId, turnId, call) => {
    const operation = "streamTurnEvents";
    let id: string;
    let turn: string;
    let cursor: number | undefined;
    let preference: StreamTransport;
    let deadline: number;
    try {
      id = pathId("sessionId", sessionId);
      turn = pathId("turnId", turnId);
      cursor = call?.cursor;
      if (cursor !== undefined && (typeof cursor !== "number" || !Number.isInteger(cursor) || cursor < 0 || cursor > CURSOR_MAX)) {
        throw sdkError("validation_failed", "cursor must be the seq of the last event received", { details: { argument: "cursor" } });
      }
      preference = call?.transport ?? "auto";
      if (preference !== "auto" && preference !== "sse" && preference !== "websocket") {
        throw sdkError("validation_failed", "transport must be auto, sse or websocket", { details: { argument: "transport" } });
      }
      deadline = timeout(call?.timeoutMs, turnTimeout, "timeoutMs");
      resumeSettings(call?.resume, "resume");
    } catch (error) {
      return failedStream(operation, error);
    }
    const scope = caller.scope(operation, call, deadline);
    const reconnect = reconnectFor(id, call, scope, preference);
    return createTurnStream({
      start: () => openEvents(id, turn, cursor, preference, scope),
      scope,
      mapFailure: (error) => transport.failure(error, scope, operation),
      turnId: turn,
      ...(cursor !== undefined && { cursor }),
      ...(reconnect !== undefined && { reconnect }),
    });
  };

  const sendTurn: AlixAIClient["sendTurn"] = (sessionId, input, call) => {
    const operation = "sendTurn";
    // Validate everything first. A failure here is delivered through the stream (never thrown
    // synchronously) and no context token is requested.
    let id: string;
    let payload: string;
    let deadline: number;
    try {
      id = pathId("sessionId", sessionId);
      payload = turnBody(input);
      deadline = timeout(call?.timeoutMs, turnTimeout, "timeoutMs");
      resumeSettings(call?.resume, "resume");
    } catch (error) {
      return createTurnStream({
        start: () => Promise.reject(error),
        scope: caller.scope(operation, undefined, 0),
        mapFailure: (e) => e,
      });
    }
    const scope = caller.scope(operation, call, deadline);
    const reconnect = reconnectFor(id, call, scope);
    return createTurnStream({
      start: () =>
        transport.send(
          { operation, params: { session_id: id }, accept: "text/event-stream", body: { contentType: "application/json", payload } },
          scope,
        ),
      scope,
      mapFailure: (error) => transport.failure(error, scope, operation),
      ...(reconnect !== undefined && { reconnect }),
    });
  };

  const sendMessage: AlixAIClient["sendMessage"] = (sessionId, message, call) => {
    const operation = "sendTurn";
    let id: string;
    let deadline: number;
    try {
      id = pathId("sessionId", sessionId);
      deadline = timeout(call?.timeoutMs, turnTimeout, "timeoutMs");
      resumeSettings(call?.resume, "resume");
    } catch (error) {
      return withAttachments(failedStream(operation, error), Promise.reject(error));
    }
    const scope = caller.scope(operation, call, deadline);
    const reconnect = reconnectFor(id, call, scope);
    return createMessageStream({
      message,
      sessionId: id,
      files,
      scope,
      turnBody,
      send: (payload) =>
        transport.send({ operation, params: { session_id: id }, accept: "text/event-stream", body: { contentType: "application/json", payload } }, scope),
      mapFailure: (error) => transport.failure(error, scope, operation),
      ...(reconnect !== undefined && { reconnect }),
    });
  };

  const sendTurnJson: AlixAIClient["sendTurnJson"] = async (sessionId, input, call) => {
    const id = pathId("sessionId", sessionId);
    const payload = turnBody(input);
    const body = await json("sendTurn", { session_id: id }, isJsonTurn, "turn", call, {
      body: { contentType: "application/json", payload },
      timeoutMs: turnTimeout,
    });
    const sequence = createTurnSequence(body.turn_id);
    const events = body.events.map((raw) => sequence.accept(parseTurnEvent(raw)));
    sequence.finish();
    const raw = body.confirmation;
    if (raw === undefined) return { turn_id: body.turn_id, events };
    const grant = raw as { preparation_id?: unknown; token?: unknown } | null;
    if (typeof grant !== "object" || grant === null || typeof grant.preparation_id !== "string" || !isConfirmationToken(grant.token)) {
      throw sdkError("internal", "the server's turn response carries a malformed confirmation", { details: { reason: "malformed_turn" } });
    }
    const prepared = events.find((event) => event.type === "prepared_action");
    if (prepared?.type === "prepared_action" && prepared.prepared_action.preparation_id !== grant.preparation_id) {
      throw sdkError("internal", "the confirmation names another preparation than the prepared_action event", {
        details: { reason: "malformed_turn" },
      });
    }
    return { turn_id: body.turn_id, events, confirmation: { preparation_id: grant.preparation_id, token: grant.token } };
  };

  return {
    createSession: async (call) => json("createSession", noParams, isSession, "session", call),
    getSession: async (sessionId, call) =>
      json("getSession", { session_id: pathId("sessionId", sessionId) }, isSession, "session", call),
    endSession: async (sessionId, call) =>
      json("endSession", { session_id: pathId("sessionId", sessionId) }, isSessionEnded, "session", call),
    getPendingState: async (sessionId, call) => {
      const body = await json("getPendingState", { session_id: pathId("sessionId", sessionId) }, isPendingResponse, "pending state", call);
      return body.pending;
    },

    sendTurn,
    sendMessage,
    streamTurnEvents,
    sendTurnJson,

    getAction: async (preparationId, call) =>
      json("getAction", { preparation_id: pathId("preparationId", preparationId) }, isPreparedAction, "action", call),
    confirmAction: async (preparationId, token, call) => {
      const id = pathId("preparationId", preparationId);
      const secret = confirmationToken(token);
      return json("confirmAction", { preparation_id: id }, isActionOutcome, "confirmation", call, {
        body: { contentType: "application/json", payload: JSON.stringify({ token: secret }) },
        timeoutMs: turnTimeout,
      });
    },
    rejectAction: async (preparationId, call) =>
      json("rejectAction", { preparation_id: pathId("preparationId", preparationId) }, isPreparedAction, "action", call),
    getActionReceipt: async (preparationId, call) =>
      json("getActionReceipt", { preparation_id: pathId("preparationId", preparationId) }, isActionReceipt, "receipt", call),

    getApplication: async (applicationId, call) =>
      json("getApplication", { application_id: pathId("applicationId", applicationId) }, isApplication, "application", call),
    listCapabilities: async (call) => json("listCapabilities", noParams, isCapabilityList, "capabilities", call),
    registerApplication: async (archive, call) => {
      const payload = archive instanceof ArrayBuffer ? new Uint8Array(archive) : archive;
      // A Blob is not read here: its content is the server's to judge. Bytes are checked for the gzip magic.
      if (payload instanceof Uint8Array && (payload.length < 2 || payload[0] !== 0x1f || payload[1] !== 0x8b)) {
        throw sdkError("validation_failed", "an application package is a gzip-compressed tar archive", { details: { argument: "archive" } });
      }
      return json("registerApplication", noParams, isRegistration, "registration", call, {
        body: { contentType: "application/gzip", payload },
      });
    },

    files,
    history: createHistoryApi(caller),
    settings: createSettingsApi(caller),
    me: createMeApi(caller),
    memory: createMemoryApi(caller),
    exports: createExportsApi(caller),
    ops: createOpsApi(caller),
    forSession: (sessionId) => build(caller.bind(pathId("sessionId", sessionId)), options, turnTimeout),
    conversation: (sessionId, conversationOptions) => createConversation(build(caller.bind(pathId("sessionId", sessionId)), options, turnTimeout), pathId("sessionId", sessionId), conversationOptions),

    getHealth: async (call) => json("getHealth", noParams, isHealth, "health", call, { allowStatus: notReadyIsAReport }),
    getLiveness: async (call) => json("getLiveness", noParams, isLiveness, "liveness", call),
    getReadiness: async (call) => json("getReadiness", noParams, isReadyReport, "readiness", call, { allowStatus: notReadyIsAReport }),
  };
};
