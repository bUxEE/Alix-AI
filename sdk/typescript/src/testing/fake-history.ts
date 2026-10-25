import type { HistoryExportRow, HistoryTurn } from "../api-types.js";
import { requireCapability, sameOwner, sameWorkspace } from "./context.js";
import type { FakeContext, FakeScope, Json } from "./context.js";

// The history area of the fake server (schemas/api/paths/history.yaml): the transcript of every turn the fake runs, paginated by whole
// turns, searched by words, exported as JSON lines with a trailer, cleared per session and cleaned per workspace. Each operation needs
// its capability in the context token (`history.read`, `history.search`, `history.clear`, `history.export`, `history.cleanup`), and a
// caller sees only its own rows. The fake does not sanitise assistant text (it has none that needs it).

interface Row {
  scope: FakeScope;
  session_id: string;
  turn_id: string;
  n: number;
  role: "user" | "assistant";
  act: string | undefined;
  body: string;
  createdMs: number;
}

export interface FakeHistory {
  /** Records the rows of one turn: the user's text (when the turn has one) and the assistant's text. */
  record: (scope: FakeScope, turnId: string, n: number, userText: string | undefined, assistantText: string, act: string | undefined) => void;
  handle: (
    operation: "listHistorySegments" | "clearHistorySession" | "searchHistory" | "exportHistory" | "cleanupHistory",
    scope: FakeScope,
    params: Record<string, string>,
    query: URLSearchParams,
    body: unknown,
  ) => Response;
  /** The rows as the API would export them, for assertions. */
  rows: () => HistoryExportRow[];
  reset: () => void;
}

const SESSION_ID = /^[A-Za-z0-9_.-]{1,128}$/;
const isRecord = (value: unknown): value is Json => typeof value === "object" && value !== null && !Array.isArray(value);
const fold = (text: string): string => text.normalize("NFD").replace(/[̀-ͯ]/g, "").toLowerCase();

export const createFakeHistory = (context: FakeContext): FakeHistory => {
  let rows: Row[] = [];
  const bad = (message: string): Response => context.problem("validation_failed", message);

  const exportRow = (row: Row): HistoryExportRow => ({
    session_id: row.session_id,
    turn_id: row.turn_id,
    n: row.n,
    role: row.role,
    ...(row.act !== undefined && { act: row.act }),
    body: row.body,
    clipped: false,
    created_at: context.iso(row.createdMs),
  });

  const own = (scope: FakeScope): Row[] => rows.filter((r) => sameOwner(r.scope, scope));

  const instant = (value: unknown): number | undefined => {
    if (typeof value !== "string") return undefined;
    const at = Date.parse(value);
    return Number.isFinite(at) && /^\d{4}-\d\d-\d\dT/.test(value) ? at : undefined;
  };

  const handle: FakeHistory["handle"] = (operation, scope, params, query, body) => {
    const refusal = context.quarantine();
    if (refusal !== undefined) return refusal;
    const capability = {
      listHistorySegments: "history.read",
      clearHistorySession: "history.clear",
      searchHistory: "history.search",
      exportHistory: "history.export",
      cleanupHistory: "history.cleanup",
    }[operation];
    const denied = requireCapability(context, scope, capability);
    if (denied !== undefined) return denied;

    switch (operation) {
      case "listHistorySegments": {
        const sessionId = params["session_id"] ?? "";
        if (!SESSION_ID.test(sessionId)) return bad("the session id is not well formed");
        if (sessionId !== scope.session_id) return context.levelViolation("session");
        const cursor = query.get("cursor");
        if (cursor !== null && !/^[0-9]{1,18}$/.test(cursor)) return bad("cursor must be the next_cursor of a previous page");
        const limitText = query.get("limit");
        if (limitText !== null && !/^[0-9]{1,3}$/.test(limitText)) return bad("limit must be an integer from 1 to 100");
        const limit = limitText === null ? 25 : Number(limitText);
        if (limit < 1 || limit > 100) return bad("limit must be an integer from 1 to 100");
        const session = own(scope).filter((r) => r.session_id === sessionId);
        const numbers = [...new Set(session.map((r) => r.n))].sort((a, b) => a - b).filter((n) => cursor === null || n > Number(cursor));
        const page = numbers.slice(0, limit);
        const turns: HistoryTurn[] = session
          .filter((r) => page.includes(r.n))
          .sort((a, b) => a.n - b.n || (a.role === b.role ? 0 : a.role === "user" ? -1 : 1))
          .map((r): HistoryTurn => {
            const { session_id: _session, ...turn } = exportRow(r);
            return turn;
          });
        const last = page.at(-1);
        return context.json(200, {
          session_id: sessionId,
          turns,
          next_cursor: numbers.length > page.length && last !== undefined ? String(last) : null,
          total: session.length,
        });
      }
      case "clearHistorySession": {
        const sessionId = params["session_id"] ?? "";
        if (!SESSION_ID.test(sessionId)) return bad("the session id is not well formed");
        if (sessionId !== scope.session_id) return context.levelViolation("session");
        const before = rows.length;
        rows = rows.filter((r) => !(sameOwner(r.scope, scope) && r.session_id === sessionId));
        return context.json(200, { session_id: sessionId, rows_deleted: before - rows.length });
      }
      case "searchHistory": {
        if (!isRecord(body)) return bad("the request body must be a JSON object");
        if (Object.keys(body).some((k) => !["text", "session_id", "limit", "offset"].includes(k))) return bad("the request body has an unknown member");
        const text = body["text"];
        if (typeof text !== "string" || text.trim() === "" || text.length > 512) return bad("text must be 1 to 512 characters");
        const sessionId = body["session_id"];
        if (sessionId !== undefined && (typeof sessionId !== "string" || !SESSION_ID.test(sessionId))) return bad("session_id is not well formed");
        const limit = body["limit"] ?? 10;
        const offset = body["offset"] ?? 0;
        if (typeof limit !== "number" || !Number.isInteger(limit) || limit < 1 || limit > 50) return bad("limit must be an integer from 1 to 50");
        if (typeof offset !== "number" || !Number.isInteger(offset) || offset < 0 || offset > 1000) return bad("offset must be an integer from 0 to 1000");
        const words = [...new Set(fold(text).split(/[^\p{L}\p{N}]+/u).filter((w) => w !== ""))].slice(0, 12);
        const hits = own(scope)
          .filter((r) => sessionId === undefined || r.session_id === sessionId)
          .filter((r) => words.every((w) => fold(r.body).includes(w)))
          .sort((a, b) => b.createdMs - a.createdMs || b.n - a.n);
        const page = hits.slice(offset, offset + limit);
        return context.json(200, {
          hits: page.map((r) => ({
            session_id: r.session_id,
            turn_id: r.turn_id,
            n: r.n,
            role: r.role,
            created_at: context.iso(r.createdMs),
            snippet: r.body.slice(0, 160),
          })),
          next_offset: offset + limit < hits.length ? offset + limit : null,
        });
      }
      case "exportHistory": {
        if (body !== undefined && !isRecord(body)) return bad("the request body must be a JSON object");
        const request = body ?? {};
        if (Object.keys(request).some((k) => !["session_id", "since", "until"].includes(k))) return bad("the request body has an unknown member");
        const sessionId = request["session_id"];
        if (sessionId !== undefined && (typeof sessionId !== "string" || !SESSION_ID.test(sessionId))) return bad("session_id is not well formed");
        const since = request["since"] === undefined ? undefined : instant(request["since"]);
        const until = request["until"] === undefined ? undefined : instant(request["until"]);
        if (request["since"] !== undefined && since === undefined) return bad("since must be an RFC 3339 time");
        if (request["until"] !== undefined && until === undefined) return bad("until must be an RFC 3339 time");
        const chosen = own(scope)
          .filter((r) => sessionId === undefined || r.session_id === sessionId)
          .filter((r) => (since === undefined || r.createdMs >= since) && (until === undefined || r.createdMs < until))
          .sort((a, b) => a.createdMs - b.createdMs || a.n - b.n || (a.role === b.role ? 0 : a.role === "user" ? -1 : 1));
        const lines = [...chosen.map((r) => JSON.stringify(exportRow(r))), JSON.stringify({ complete: true, rows: chosen.length })];
        return new Response(`${lines.join("\n")}\n`, {
          status: 200,
          headers: { "Content-Type": "application/x-ndjson", "Cache-Control": "no-store" },
        });
      }
      default: {
        if (!isRecord(body) || Object.keys(body).some((k) => k !== "before")) return bad("the request body must be {before}");
        const before = instant(body["before"]);
        if (before === undefined) return bad("before must be an RFC 3339 time");
        if (before > context.nowMs()) return bad("before cannot be in the future");
        const count = rows.length;
        rows = rows.filter((r) => !(sameWorkspace(r.scope, scope) && r.createdMs < before));
        return context.json(200, { rows_deleted: count - rows.length });
      }
    }
  };

  return {
    record: (scope, turnId, n, userText, assistantText, act) => {
      const createdMs = context.nowMs();
      if (userText !== undefined) rows.push({ scope, session_id: scope.session_id, turn_id: turnId, n, role: "user", act: undefined, body: userText, createdMs });
      rows.push({ scope, session_id: scope.session_id, turn_id: turnId, n, role: "assistant", act, body: assistantText, createdMs });
    },
    handle,
    rows: () => rows.map(exportRow),
    reset: () => {
      rows = [];
    },
  };
};
