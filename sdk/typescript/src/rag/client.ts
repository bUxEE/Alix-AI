import { createCaller, timeout } from "../call.js";
import type { Caller } from "../call.js";
import { sdkError } from "../errors.js";
import type { ContextTokenProvider, ContextTokenRequest } from "../transport.js";
import type { CallOptions } from "../transport.js";
import {
  isAnswerResult,
  isCitationResolution,
  isGenerationList,
  isIndexGeneration,
  isReauditReport,
  isSearchResponse,
  isSource,
  isSourceList,
  isSyncAccepted,
} from "./guards.js";
import type { RagOperationId } from "./operations.js";
import type {
  AnswerRequest,
  AnswerResult,
  CitationResolution,
  IndexGeneration,
  ReauditReport,
  SearchRequest,
  SearchResponse,
  Source,
  SyncAccepted,
} from "./types.js";

/** What the host's token provider is told about a RAG request: the shared request type, narrowed to the operations of this area. */
export type RagContextTokenRequest = ContextTokenRequest & { operation: RagOperationId };

/**
 * Supplies a fresh, single-use context token for one request. Called for every request and never cached by the SDK. Return the
 * bare token (no "Bearer " prefix). A provider shared with the main client can take the wider `ContextTokenRequest`.
 */
export type RagContextTokenProvider = (request: RagContextTokenRequest) => string | Promise<string>;

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface RagClientOptions {
  /** The AlixAI service, e.g. "https://alixai.example.com" or "http://127.0.0.1:8080". A path prefix is kept. */
  baseUrl: string;
  /**
   * Supplies a fresh single-use context token for each request. The SDK never mints, decodes, caches
   * or retries with a token; it holds one only for the call it is for.
   */
  getContextToken: RagContextTokenProvider;
  /** A fetch implementation. Defaults to the global `fetch`. */
  fetch?: FetchLike;
  /** Deadline for an ordinary request, in milliseconds. Default 30000; 0 disables it. */
  timeoutMs?: number;
  /**
   * Deadline for an answer, in milliseconds. An answer waits on the model, so the default is longer:
   * 120000 (the service bounds the model call itself by rag_answer.generate_timeout). 0 disables it.
   */
  answerTimeoutMs?: number;
  /** Allow plain http to a host that is not loopback. The context token is a bearer credential: leave this off. */
  allowInsecureHttp?: boolean;
}

/**
 * The RAG area of the public API (master plan 14.2): sources, indexing, scoped search, answers with
 * citations and the index generations. Every method takes the verified scope from the context token
 * the host supplies and nothing else: no request names a tenant, a workspace, a principal or a folder.
 */
export interface RagClient {
  // ---- sources ----
  /**
   * Registers the source the application's active package declares under `declaration`. A source is
   * never defined by a client: a name the package does not declare is `validation_failed`
   * (`details.reason` `not_declared`). Registering the same declaration again returns the same source.
   * Needs the deployment's operator action `manage_sources`; without it the call is `permission_denied`.
   */
  registerSource: (declaration: string, options?: CallOptions) => Promise<Source>;
  /** Every source registered for the verified tenant and workspace. */
  listSources: (options?: CallOptions) => Promise<Source[]>;
  /** One source with its indexing status. A source of another scope is `validation_failed` / not_found, like one that does not exist. */
  getSource: (sourceId: string, options?: CallOptions) => Promise<Source>;
  /** Removes a source: it stops being served at once and its declared deletion behaviour is applied. Needs `manage_sources`. */
  removeSource: (sourceId: string, options?: CallOptions) => Promise<void>;
  /** Re-binds the caller's sources to the active package after it was replaced or re-audited. Needs `manage_sources`. */
  reauditSources: (options?: CallOptions) => Promise<ReauditReport>;

  // ---- indexing ----
  /** Queues a sync of a source. Requests that arrive while one is queued or running are the same job. Needs `manage_sources`. */
  syncSource: (sourceId: string, options?: CallOptions) => Promise<SyncAccepted>;
  /** Builds a replacement index generation; the one that serves keeps serving until it is activated. Needs `manage_sources`. */
  rebuildSource: (sourceId: string, options?: CallOptions) => Promise<SyncAccepted>;
  /** The index generations of a source, newest first. */
  listGenerations: (sourceId: string, options?: CallOptions) => Promise<IndexGeneration[]>;
  /** Withdraws a generation: it is never retrievable again. Needs `manage_sources`. */
  invalidateGeneration: (sourceId: string, generationId: string, options?: CallOptions) => Promise<IndexGeneration>;

  // ---- questions ----
  /**
   * Passages of the authorised sources that match, best first. An empty `results` is a search that
   * matched nothing, not an error. `notices` label a degraded search.
   */
  search: (request: SearchRequest, options?: CallOptions) => Promise<SearchResponse>;
  /**
   * A grounded answer with a citation for every claim, or the typed `no_supported_evidence` result
   * (`outcome`). Never an uncited answer: a response that is neither is an error. A request that could
   * not run is a typed error, never `no_supported_evidence`.
   */
  answer: (request: AnswerRequest, options?: CallOptions) => Promise<AnswerResult>;
  /**
   * What the `file` open reference of a citation resolves to now: the source's state, the cited
   * passage and the way to open the source. A reference that does not resolve for the caller is
   * `permission_denied`, an expired one `expired`. Never use the files API for it.
   */
  resolveCitation: (fileId: string, options?: CallOptions) => Promise<CitationResolution>;
}

const DEFAULT_ANSWER_TIMEOUT_MS = 120_000;

const MAX_QUESTION_CHARS = 2000;
const MAX_SOURCE_IDS = 32;
const MAX_LIMIT = 50;
const MAX_DECLARATION_CHARS = 128;

const SOURCE_ID = /^src_[0-9A-HJKMNP-TV-Z]{26}$/;
const GENERATION_ID = /^gen_[0-9A-HJKMNP-TV-Z]{26}$/;
const FILE_ID = /^fil_[0-9A-HJKMNP-TV-Z]{26}$/;

const invalid = (argument: string, reason: string) =>
  sdkError("validation_failed", `${argument}: ${reason}`, { details: { argument } });

const id = (argument: string, pattern: RegExp, value: unknown): string => {
  if (typeof value !== "string" || !pattern.test(value)) throw invalid(argument, "it is not a well-formed identifier");
  return value;
};

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** Rejects a member the schema does not have: the request objects are closed, and a scope is never one of them. */
const closed = (argument: string, value: unknown, known: readonly string[]): Record<string, unknown> => {
  if (!isRecord(value)) throw invalid(argument, "it must be an object");
  for (const key of Object.keys(value)) if (!known.includes(key)) throw invalid(argument, `unknown member "${key}"`);
  return value;
};

/** A question or search text: a non-space character, no NUL, at most 2000 characters (code points). */
const text = (argument: string, value: unknown): string => {
  if (typeof value !== "string" || value.trim() === "") throw invalid(argument, "it must be a non-empty string");
  if (value.includes("\u0000")) throw invalid(argument, "it must not contain a NUL");
  // The schema counts characters, not UTF-16 units: spread by code point.
  if ([...value].length > MAX_QUESTION_CHARS) throw invalid(argument, `it must be at most ${MAX_QUESTION_CHARS} characters`);
  return value;
};

const sourceIds = (argument: string, value: unknown): string[] | undefined => {
  if (value === undefined) return undefined;
  if (!Array.isArray(value) || value.length === 0 || value.length > MAX_SOURCE_IDS) {
    throw invalid(argument, `it must list 1 to ${MAX_SOURCE_IDS} source ids`);
  }
  const list = value.map((item) => id(argument, SOURCE_ID, item));
  if (new Set(list).size !== list.length) throw invalid(argument, "it must not list a source twice");
  return list;
};

/** The body of a search, built from known members only. */
const searchBody = (request: unknown): string => {
  const r = closed("request", request, ["query", "source_ids", "limit"]);
  const ids = sourceIds("source_ids", r["source_ids"]);
  const limit = r["limit"];
  if (limit !== undefined && (typeof limit !== "number" || !Number.isInteger(limit) || limit < 1 || limit > MAX_LIMIT)) {
    throw invalid("limit", `it must be an integer from 1 to ${MAX_LIMIT}`);
  }
  return JSON.stringify({ query: text("query", r["query"]), ...(ids !== undefined && { source_ids: ids }), ...(limit !== undefined && { limit }) });
};

/** The body of an answer request, built from known members only. */
const answerBody = (request: unknown): string => {
  const r = closed("request", request, ["question", "source_ids"]);
  const ids = sourceIds("source_ids", r["source_ids"]);
  return JSON.stringify({ question: text("question", r["question"]), ...(ids !== undefined && { source_ids: ids }) });
};

const declarationBody = (declaration: unknown): string => {
  if (
    typeof declaration !== "string" ||
    declaration === "" ||
    declaration !== declaration.trim() ||
    declaration.length > MAX_DECLARATION_CHARS ||
    /[\u0000-\u001f\u007f]/.test(declaration)
  ) {
    throw invalid("declaration", `it must be the name the package declares: 1 to ${MAX_DECLARATION_CHARS} characters, no padding, no control character`);
  }
  return JSON.stringify({ declaration });
};

/**
 * Creates the client of the RAG area for one AlixAI service.
 *
 * Like the main client it uses `fetch` only, holds credentials in memory for the length of one call,
 * and checks every argument before a context token is requested, so a malformed request never costs
 * the single-use token it would have used.
 */
export const createRagClient = (options: RagClientOptions): RagClient => {
  if (typeof options !== "object" || options === null) throw sdkError("validation_failed", "createRagClient needs its options");
  const answerTimeout = timeout(options.answerTimeoutMs, DEFAULT_ANSWER_TIMEOUT_MS, "answerTimeoutMs");
  // The RAG area is one more set of operations on the shared transport (src/call.ts), so it keeps every rule of the main client.
  const caller: Caller = createCaller({
    baseUrl: options.baseUrl,
    // Only the operations of this area reach the provider, so the narrower request type is accurate.
    getContextToken: options.getContextToken as ContextTokenProvider,
    fetch: options.fetch,
    timeoutMs: options.timeoutMs,
    allowInsecureHttp: options.allowInsecureHttp,
  });

  /** One request: either the parsed, shape-checked JSON body or nothing. */
  const call = async <T>(
    operation: RagOperationId,
    params: Record<string, string>,
    what: string,
    callOptions: CallOptions | undefined,
    guard: ((value: unknown) => value is T) | undefined,
    extra: { body?: string; timeoutMs?: number } = {},
  ): Promise<T> => {
    const options2 = {
      ...(extra.body !== undefined && { body: { contentType: "application/json", payload: extra.body } }),
      ...(extra.timeoutMs !== undefined && { timeoutMs: extra.timeoutMs }),
    };
    if (guard === undefined) return (await caller.empty(operation, params, callOptions, options2)) as T;
    return caller.json(operation, params, guard, what, callOptions, options2);
  };

  const none: Record<string, string> = {};

  return {
    registerSource: async (declaration, callOptions) =>
      call("registerRagSource", none, "source", callOptions, isSource, { body: declarationBody(declaration) }),
    listSources: async (callOptions) => (await call("listRagSources", none, "source list", callOptions, isSourceList)).sources,
    getSource: async (sourceId, callOptions) =>
      call("getRagSource", { source_id: id("sourceId", SOURCE_ID, sourceId) }, "source", callOptions, isSource),
    removeSource: async (sourceId, callOptions) =>
      call<void>("removeRagSource", { source_id: id("sourceId", SOURCE_ID, sourceId) }, "removal", callOptions, undefined),
    reauditSources: async (callOptions) => call("reauditRagSources", none, "re-audit report", callOptions, isReauditReport, { body: "{}" }),

    syncSource: async (sourceId, callOptions) =>
      call("syncRagSource", { source_id: id("sourceId", SOURCE_ID, sourceId) }, "sync", callOptions, isSyncAccepted, { body: "{}" }),
    rebuildSource: async (sourceId, callOptions) =>
      call("rebuildRagSource", { source_id: id("sourceId", SOURCE_ID, sourceId) }, "rebuild", callOptions, isSyncAccepted, { body: "{}" }),
    listGenerations: async (sourceId, callOptions) =>
      (await call("listRagGenerations", { source_id: id("sourceId", SOURCE_ID, sourceId) }, "generation list", callOptions, isGenerationList)).generations,
    invalidateGeneration: async (sourceId, generationId, callOptions) =>
      call(
        "invalidateRagGeneration",
        { source_id: id("sourceId", SOURCE_ID, sourceId), generation_id: id("generationId", GENERATION_ID, generationId) },
        "generation",
        callOptions,
        isIndexGeneration,
        { body: "{}" },
      ),

    search: async (request, callOptions) => call("searchRag", none, "search", callOptions, isSearchResponse, { body: searchBody(request) }),
    answer: async (request, callOptions) =>
      call("answerRag", none, "answer", callOptions, isAnswerResult, { body: answerBody(request), timeoutMs: answerTimeout }),
    resolveCitation: async (fileId, callOptions) =>
      call("resolveRagCitation", { file_id: id("fileId", FILE_ID, fileId) }, "citation", callOptions, isCitationResolution),
  };
};
