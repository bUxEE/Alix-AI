import type {
  HistoryCleanupResult,
  HistoryExportRequest,
  HistoryExportRow,
  HistorySearchPage,
  HistorySearchRequest,
  HistorySegmentPage,
  HistorySessionCleared,
} from "./api-types.js";
import {
  isHistoryCleanupResult,
  isHistoryExportRow,
  isHistoryExportTrailer,
  isHistorySearchPage,
  isHistorySegmentPage,
  isHistorySessionCleared,
} from "./area-guards.js";
import type { Caller } from "./call.js";
import { isAlixAIError, sdkError } from "./errors.js";
import type { CallScope, CallOptions } from "./transport.js";
import { pathId } from "./validate.js";

// Conversation history and privacy (master plan 14.2 and 19). The host authorises each operation by putting its capability in the
// context token (`history.read`, `history.search`, `history.clear`, `history.export`, `history.cleanup`); a token without it is
// `permission_denied` before anything is read or deleted. Reading, searching, clearing and exporting concern the caller's OWN history.
// The text of an assistant row is Markdown that went through the same sanitiser as a live `token` event; a user row is plain text.

/** Options of a page of the transcript. */
export interface SegmentOptions extends CallOptions {
  /** The `next_cursor` of the previous page; absent means the start of the transcript. */
  cursor?: string;
  /** How many turns the page holds at most, 1 to 100 (the server's default is 25). */
  limit?: number;
}

/**
 * The rows of an export, read as they arrive. The export is whole only if it ends with the server's trailer and the trailer's count
 * matches the rows received: a stream that stops anywhere else throws (`unavailable`, `details.reason` `export_incomplete`, retryable:
 * an export only reads, so it is safe to ask again), never ends quietly.
 */
export interface HistoryExport extends AsyncIterable<HistoryExportRow> {
  /** Rows delivered so far. */
  readonly rows: number;
  /** Abandons the export and releases the connection. */
  cancel: (reason?: unknown) => void;
  /** Reads the export to its verified end and returns every row. */
  result: () => Promise<HistoryExportRow[]>;
}

/** The history area of the client: `client.history`. */
export interface HistoryApi {
  /** A page of the session's transcript: whole turns, oldest first. The session must be the token's. Needs `history.read`. */
  segments: (sessionId: string, options?: SegmentOptions) => Promise<HistorySegmentPage>;
  /** Deletes the transcript of the session and returns how many rows went; the session, receipts and audit are not touched. Needs `history.clear`. */
  clear: (sessionId: string, options?: CallOptions) => Promise<HistorySessionCleared>;
  /** Full-text search over the caller's own transcript rows in every session of the workspace. Needs `history.search`. */
  search: (request: HistorySearchRequest, options?: CallOptions) => Promise<HistorySearchPage>;
  /** Streams the caller's own history as it is exported (JSON lines, then a trailer). Needs `history.export`. Starts at once; never throws synchronously. */
  export: (request?: HistoryExportRequest, options?: CallOptions) => HistoryExport;
  /**
   * Deletes every transcript turn of the WORKSPACE (all its principals and sessions) created before `before`. It reaches beyond the
   * caller's own history: it needs `history.cleanup`, which a host grants only to whoever manages history.
   */
  cleanup: (before: string | Date, options?: CallOptions) => Promise<HistoryCleanupResult>;
}

const CURSOR = /^[0-9]{1,18}$/;
const invalid = (argument: string, reason: string) => sdkError("validation_failed", `${argument}: ${reason}`, { details: { argument } });
const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** An RFC 3339 instant as the wire wants it. A Date is converted; a string must parse. */
const instant = (argument: string, value: unknown): string => {
  const at = value instanceof Date ? value.getTime() : typeof value === "string" ? Date.parse(value) : Number.NaN;
  if (!Number.isFinite(at)) throw invalid(argument, "it must be a Date or an RFC 3339 time");
  return typeof value === "string" ? value : new Date(at).toISOString();
};

const closed = (argument: string, value: unknown, known: readonly string[]): Record<string, unknown> => {
  if (!isRecord(value)) throw invalid(argument, "it must be an object");
  for (const key of Object.keys(value)) if (!known.includes(key)) throw invalid(argument, `unknown member "${key}"`);
  return value;
};

const searchBody = (request: unknown): string => {
  const r = closed("request", request, ["text", "session_id", "limit", "offset"]);
  const text = r["text"];
  if (typeof text !== "string" || text.trim() === "" || text.length > 512) throw invalid("text", "it must be 1 to 512 characters");
  const limit = r["limit"];
  if (limit !== undefined && (typeof limit !== "number" || !Number.isInteger(limit) || limit < 1 || limit > 50)) {
    throw invalid("limit", "it must be an integer from 1 to 50");
  }
  const offset = r["offset"];
  if (offset !== undefined && (typeof offset !== "number" || !Number.isInteger(offset) || offset < 0 || offset > 1000)) {
    throw invalid("offset", "it must be an integer from 0 to 1000");
  }
  return JSON.stringify({
    text,
    ...(r["session_id"] !== undefined && { session_id: pathId("session_id", r["session_id"]) }),
    ...(limit !== undefined && { limit }),
    ...(offset !== undefined && { offset }),
  });
};

const exportBody = (request: unknown): string => {
  if (request === undefined) return "{}";
  const r = closed("request", request, ["session_id", "since", "until"]);
  return JSON.stringify({
    ...(r["session_id"] !== undefined && { session_id: pathId("session_id", r["session_id"]) }),
    ...(r["since"] !== undefined && { since: instant("since", r["since"]) }),
    ...(r["until"] !== undefined && { until: instant("until", r["until"]) }),
  });
};

/** Upper bound for one line of an export: a server that never ends a line is refused. */
const MAX_LINE_CHARS = 8 * 1024 * 1024;

const incomplete = (reason: string, extra: Record<string, unknown> = {}) =>
  sdkError("unavailable", `the history export is incomplete: ${reason}`, { details: { reason: "export_incomplete", ...extra } });

interface ExportParams {
  start: () => Promise<{ response: Response; scope: CallScope }>;
  mapFailure: (error: unknown, scope: CallScope) => unknown;
}

/** Reads an NDJSON export: rows, then the trailer, whose count must match. */
const createExport = (params: ExportParams): HistoryExport => {
  let scope: CallScope | undefined;
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let buffer = "";
  let ended = false;
  let finished = false;
  let failure: { error: unknown } | undefined;
  let failureDelivered = false;
  let rows = 0;

  const start: Promise<void> = params.start().then(({ response, scope: opened }) => {
    scope = opened;
    if (finished) {
      // Cancelled while the request was still being sent: release what it opened.
      opened.release();
      void response.body?.cancel().catch(() => undefined);
      return;
    }
    if (response.body === null) throw incomplete("the response has no body");
    // The deadline covers the request until its head arrives: a large export then streams as long as it takes (the caller's
    // signal and cancel still stop it).
    opened.endDeadline();
    reader = response.body.getReader();
  });

  const close = async (): Promise<void> => {
    if (finished) return;
    finished = true;
    scope?.release();
    await reader?.cancel().catch(() => undefined);
  };

  const fail = async (error: unknown): Promise<unknown> => {
    if (failure === undefined) failure = { error: isAlixAIError(error) || scope === undefined ? error : params.mapFailure(error, scope) };
    await close();
    return failure.error;
  };

  start.catch(async (error: unknown) => {
    await fail(error);
  });

  /** The next line of the buffer, reading more when there is none. Undefined at the clean end of the body. */
  const line = async (): Promise<string | undefined> => {
    for (;;) {
      const at = buffer.indexOf("\n");
      if (at !== -1) {
        const text = buffer.slice(0, at);
        buffer = buffer.slice(at + 1);
        return text.endsWith("\r") ? text.slice(0, -1) : text;
      }
      if (buffer.length > MAX_LINE_CHARS) throw sdkError("internal", "a line of the export exceeds the size limit", { details: { reason: "line_too_large" } });
      if (ended || reader === undefined) {
        if (buffer === "") return undefined;
        const rest = buffer;
        buffer = "";
        return rest; // a last line without its newline: the caller finds out whether it is the trailer
      }
      const chunk = await reader.read();
      if (chunk.done) {
        ended = true;
        try {
          buffer += decoder.decode();
        } catch (cause) {
          throw sdkError("internal", "the export is not valid UTF-8", { cause });
        }
      } else {
        try {
          buffer += decoder.decode(chunk.value, { stream: true });
        } catch (cause) {
          throw sdkError("internal", "the export is not valid UTF-8", { cause });
        }
      }
    }
  };

  const pull = async (): Promise<HistoryExportRow | undefined> => {
    await start;
    for (;;) {
      const text = await line();
      if (text === undefined) throw incomplete("it ended without its trailer", { rows });
      if (text === "") continue;
      let parsed: unknown;
      try {
        parsed = JSON.parse(text);
      } catch (cause) {
        throw sdkError("internal", "a line of the export is not valid JSON", { cause, details: { reason: "malformed_export" } });
      }
      if (isHistoryExportTrailer(parsed)) {
        if (parsed.rows !== rows) throw incomplete("the trailer counts a different number of rows", { rows, trailer_rows: parsed.rows });
        // Nothing may follow the trailer.
        let extra = await line();
        while (extra === "") extra = await line();
        if (extra !== undefined) throw sdkError("internal", "the export has lines after its trailer", { details: { reason: "malformed_export" } });
        return undefined;
      }
      if (!isHistoryExportRow(parsed)) throw sdkError("internal", "a line of the export is neither a row nor the trailer", { details: { reason: "malformed_export" } });
      rows += 1;
      return parsed;
    }
  };

  const iterator: AsyncIterator<HistoryExportRow, undefined> = {
    next: async () => {
      if (failure !== undefined) {
        if (failureDelivered) return { done: true, value: undefined };
        failureDelivered = true;
        throw failure.error;
      }
      if (finished) return { done: true, value: undefined };
      try {
        const row = await pull();
        if (row === undefined) {
          await close();
          return { done: true, value: undefined };
        }
        return { done: false, value: row };
      } catch (error) {
        failureDelivered = true;
        throw await fail(error);
      }
    },
    return: async () => {
      await close();
      return { done: true, value: undefined };
    },
  };

  const result = async (): Promise<HistoryExportRow[]> => {
    const all: HistoryExportRow[] = [];
    for (;;) {
      const step = await iterator.next();
      if (step.done === true) return all;
      all.push(step.value);
    }
  };

  return {
    get rows() {
      return rows;
    },
    cancel: (reason) => {
      if (finished) return;
      const why = reason ?? new DOMException("The export was cancelled", "AbortError");
      scope?.cancel(why); // aborts a body read in flight
      void fail(why);
    },
    result,
    [Symbol.asyncIterator]: () => iterator as AsyncIterator<HistoryExportRow>,
  };
};

/** Builds the history area over a caller. */
export const createHistoryApi = (caller: Caller): HistoryApi => ({
  segments: async (sessionId, options) => {
    const id = pathId("sessionId", sessionId);
    const query: Record<string, string> = {};
    if (options?.cursor !== undefined) {
      if (typeof options.cursor !== "string" || !CURSOR.test(options.cursor)) throw invalid("cursor", "it must be the next_cursor of a previous page");
      query["cursor"] = options.cursor;
    }
    if (options?.limit !== undefined) {
      if (typeof options.limit !== "number" || !Number.isInteger(options.limit) || options.limit < 1 || options.limit > 100) {
        throw invalid("limit", "it must be an integer from 1 to 100");
      }
      query["limit"] = String(options.limit);
    }
    return caller.json("listHistorySegments", { session_id: id }, isHistorySegmentPage, "history page", options, { query });
  },
  clear: async (sessionId, options) =>
    caller.json("clearHistorySession", { session_id: pathId("sessionId", sessionId) }, isHistorySessionCleared, "history clear", options),
  search: async (request, options) =>
    caller.json("searchHistory", {}, isHistorySearchPage, "history search", options, {
      body: { contentType: "application/json", payload: searchBody(request) },
    }),
  export: (request, options) => {
    let payload: string | undefined;
    let invalidRequest: unknown;
    try {
      payload = exportBody(request);
    } catch (error) {
      invalidRequest = error;
    }
    return createExport({
      start: async () => {
        if (payload === undefined) throw invalidRequest;
        return caller.open("exportHistory", {}, options, {
          accept: "application/x-ndjson",
          body: { contentType: "application/json", payload },
        });
      },
      mapFailure: (error, scope) => caller.transport.failure(error, scope, "exportHistory"),
    });
  },
  cleanup: async (before, options) =>
    caller.json("cleanupHistory", {}, isHistoryCleanupResult, "history cleanup", options, {
      body: { contentType: "application/json", payload: JSON.stringify({ before: instant("before", before) }) },
    }),
});
