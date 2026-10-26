import type { IncomingMessage, ServerResponse } from "node:http";
import { BodyTooLargeError, isRecord, readJson, sendEmpty, sendError, sendJson } from "./http";
import { isoAt, newId } from "./ids";

// Scripted per-principal LLM catalog/me routes (ADR 0041) and long-term memory (ADR 0042) for the kit demos.

const CATALOG = "/v1/providers/catalog";
const LLM = "/v1/me/llm";
const LLM_ROLE = /^\/v1\/me\/llm\/(conversational_controller|language_realization|stt)$/;
const CREDENTIAL = /^\/v1\/me\/providers\/([a-z][a-z0-9_-]{0,31})\/credential$/;
const BILLING_ACK = /^\/v1\/me\/providers\/([a-z][a-z0-9_-]{0,31})\/billing-notice\/ack$/;
const OAUTH_START = /^\/v1\/me\/oauth\/([a-z][a-z0-9_-]{0,31})\/start$/;
const MEMORY = "/v1/memory";
const MEMORY_ID = /^\/v1\/memory\/(mem_[0-9A-HJKMNP-TV-Z]{26})$/;
const MEMORY_CONFIRM = /^\/v1\/memory\/(mem_[0-9A-HJKMNP-TV-Z]{26})\/confirm$/;
const MEMORY_REJECT = /^\/v1\/memory\/(mem_[0-9A-HJKMNP-TV-Z]{26})\/reject$/;

const SCOPE = {
  application_id: "app_demo",
  tenant_id: "ten_demo",
  workspace_id: "ws_demo",
  principal_id: "usr_demo_alex",
} as const;

interface MemoryRow {
  id: string;
  text: string;
  origin: "user_explicit" | "assistant_proposed";
  status: "proposed" | "active" | "rejected";
  data_class: "internal";
  created_at: string;
  updated_at: string;
  expires_at?: string;
}

const item = (row: MemoryRow) => ({ ...SCOPE, ...row });

export interface DemoMemoryItem {
  readonly id: string;
  readonly text: string;
  readonly origin: "user_explicit" | "assistant_proposed";
  readonly status: "proposed" | "active" | "rejected";
  readonly data_class: "internal";
  readonly created_at: string;
  readonly updated_at: string;
  readonly expires_at?: string;
}

export interface MeMemoryHandler {
  handle: (req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams) => Promise<boolean>;
  /** Adds a proposed item (for the AG-UI memory_proposal scenario) and returns the public shape. */
  propose: (text: string, expiresAtMs?: number) => DemoMemoryItem;
  list: () => readonly DemoMemoryItem[];
}

export const createMeMemoryHandler = (): MeMemoryHandler => {
  const memories = new Map<string, MemoryRow>();
  // Seed one active memory so AlixMemoryList demos are not empty on first paint.
  const seedId = newId("mem");
  const seededAt = isoAt(Date.now());
  memories.set(seedId, {
    id: seedId,
    text: "Prefer concise answers",
    origin: "user_explicit",
    status: "active",
    data_class: "internal",
    created_at: seededAt,
    updated_at: seededAt,
  });

  const principalLlm = (bindings: unknown[] = []) => ({
    principal_id: SCOPE.principal_id,
    application_id: SCOPE.application_id,
    bindings,
    updated_at: isoAt(Date.now()),
  });

  const handle = async (req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams): Promise<boolean> => {
    const method = req.method ?? "GET";

    if (method === "GET" && path === CATALOG) {
      sendJson(res, 200, {
        entries: [
          {
            profile_id: "pprf_01JDEMO000000000000000000A",
            provider_id: "openai",
            model_id: "gpt-test",
            roles: ["conversational_controller", "language_realization"],
            account_mode: "per_principal",
            display_name: "OpenAI (demo)",
            terms_summary: "Demo catalog entry. No real provider call is made.",
            billing_notice_version: "2026-01",
            qualified: true,
          },
        ],
      });
      return true;
    }

    if (method === "GET" && path === LLM) {
      sendJson(res, 200, principalLlm());
      return true;
    }

    const roleMatch = LLM_ROLE.exec(path);
    if (roleMatch !== null) {
      const role = roleMatch[1]!;
      if (method === "PUT") {
        let body: unknown;
        try {
          body = await readJson(req, 16 * 1024);
        } catch (error) {
          sendError(res, error instanceof BodyTooLargeError ? 413 : 400, "validation_failed", "the body could not be read");
          return true;
        }
        if (!isRecord(body) || typeof body["profile_id"] !== "string" || body["profile_id"] === "") {
          sendError(res, 400, "validation_failed", "profile_id is required");
          return true;
        }
        const model = typeof body["model_id"] === "string" && body["model_id"] !== "" ? body["model_id"] : "gpt-test";
        sendJson(
          res,
          200,
          principalLlm([
            {
              role,
              profile_id: body["profile_id"],
              provider_id: "openai",
              model_id: model,
              account_status: "missing",
              billing_notice_acked: false,
              updated_at: isoAt(Date.now()),
            },
          ]),
        );
        return true;
      }
      if (method === "DELETE") {
        sendJson(res, 200, principalLlm());
        return true;
      }
    }

    const credMatch = CREDENTIAL.exec(path);
    if (credMatch !== null) {
      const provider = credMatch[1]!;
      if (method === "PUT") {
        let body: unknown;
        try {
          body = await readJson(req, 16 * 1024);
        } catch (error) {
          sendError(res, error instanceof BodyTooLargeError ? 413 : 400, "validation_failed", "the body could not be read");
          return true;
        }
        if (!isRecord(body) || typeof body["key"] !== "string" || body["key"] === "") {
          sendError(res, 400, "validation_failed", "key is required");
          return true;
        }
        sendJson(res, 200, { provider_id: provider, status: "active" });
        return true;
      }
      if (method === "DELETE") {
        sendJson(res, 200, { provider_id: provider, status: "revoked" });
        return true;
      }
    }

    const ackMatch = BILLING_ACK.exec(path);
    if (ackMatch !== null && method === "POST") {
      let body: unknown;
      try {
        body = await readJson(req, 16 * 1024);
      } catch (error) {
        sendError(res, error instanceof BodyTooLargeError ? 413 : 400, "validation_failed", "the body could not be read");
        return true;
      }
      if (
        !isRecord(body) ||
        typeof body["notice_version"] !== "string" ||
        body["consumer_subscription_is_not_api_access"] !== true ||
        body["separate_api_billing_understood"] !== true
      ) {
        sendError(res, 400, "validation_failed", "billing notice acknowledgement is incomplete");
        return true;
      }
      sendJson(res, 200, {
        provider_id: ackMatch[1],
        notice_version: body["notice_version"],
        acknowledged_at: isoAt(Date.now()),
      });
      return true;
    }

    const oauthMatch = OAUTH_START.exec(path);
    if (oauthMatch !== null && method === "POST") {
      let body: unknown;
      try {
        body = await readJson(req, 16 * 1024);
      } catch (error) {
        sendError(res, error instanceof BodyTooLargeError ? 413 : 400, "validation_failed", "the body could not be read");
        return true;
      }
      if (!isRecord(body) || !Array.isArray(body["capabilities"]) || typeof body["redirect_uri"] !== "string" || body["redirect_uri"] === "") {
        sendError(res, 400, "validation_failed", "capabilities and redirect_uri are required");
        return true;
      }
      sendJson(res, 200, {
        authorization_url: "https://example.test/oauth",
        redirect_uri: body["redirect_uri"],
        expires_at: isoAt(Date.now() + 60_000),
      });
      return true;
    }

    if (path === MEMORY) {
      if (method === "GET") {
        const status = query.get("status");
        const q = (query.get("q") ?? "").toLowerCase();
        let items = [...memories.values()].map(item);
        if (status === "proposed" || status === "active" || status === "rejected") {
          items = items.filter((row) => row.status === status);
        }
        if (q !== "") items = items.filter((row) => row.text.toLowerCase().includes(q));
        sendJson(res, 200, { items });
        return true;
      }
      if (method === "POST") {
        let body: unknown;
        try {
          body = await readJson(req, 16 * 1024);
        } catch (error) {
          sendError(res, error instanceof BodyTooLargeError ? 413 : 400, "validation_failed", "the body could not be read");
          return true;
        }
        if (!isRecord(body) || typeof body["text"] !== "string" || body["text"] === "") {
          sendError(res, 400, "validation_failed", "text is required");
          return true;
        }
        const now = isoAt(Date.now());
        const row: MemoryRow = {
          id: newId("mem"),
          text: body["text"],
          origin: "user_explicit",
          status: "active",
          data_class: "internal",
          created_at: now,
          updated_at: now,
        };
        memories.set(row.id, row);
        sendJson(res, 201, item(row));
        return true;
      }
    }

    const confirmMatch = MEMORY_CONFIRM.exec(path);
    if (confirmMatch !== null && method === "POST") {
      const id = confirmMatch[1]!;
      const row = memories.get(id);
      if (row === undefined) {
        sendError(res, 404, "validation_failed", "no memory item with this id", { reason: "not_found" });
        return true;
      }
      if (row.expires_at !== undefined && Date.parse(row.expires_at) <= Date.now()) {
        sendError(res, 400, "validation_failed", "the memory proposal has expired", { reason: "memory_proposal_expired" });
        return true;
      }
      const next: MemoryRow = { ...row, status: "active", updated_at: isoAt(Date.now()) };
      memories.set(id, next);
      sendJson(res, 200, item(next));
      return true;
    }

    const rejectMatch = MEMORY_REJECT.exec(path);
    if (rejectMatch !== null && method === "POST") {
      const id = rejectMatch[1]!;
      const row = memories.get(id);
      if (row === undefined) {
        sendError(res, 404, "validation_failed", "no memory item with this id", { reason: "not_found" });
        return true;
      }
      const next: MemoryRow = { ...row, status: "rejected", updated_at: isoAt(Date.now()) };
      memories.set(id, next);
      sendJson(res, 200, item(next));
      return true;
    }

    const idMatch = MEMORY_ID.exec(path);
    if (idMatch !== null) {
      const id = idMatch[1]!;
      if (method === "PATCH") {
        let body: unknown;
        try {
          body = await readJson(req, 16 * 1024);
        } catch (error) {
          sendError(res, error instanceof BodyTooLargeError ? 413 : 400, "validation_failed", "the body could not be read");
          return true;
        }
        const row = memories.get(id);
        if (row === undefined) {
          sendError(res, 404, "validation_failed", "no memory item with this id", { reason: "not_found" });
          return true;
        }
        const text = isRecord(body) && typeof body["text"] === "string" ? body["text"] : row.text;
        const next: MemoryRow = { ...row, text, updated_at: isoAt(Date.now()) };
        memories.set(id, next);
        sendJson(res, 200, item(next));
        return true;
      }
      if (method === "DELETE") {
        memories.delete(id);
        sendEmpty(res, 204);
        return true;
      }
    }

    return false;
  };

  const propose = (text: string, expiresAtMs?: number): DemoMemoryItem => {
    const now = Date.now();
    const row: MemoryRow = {
      id: newId("mem", now),
      text,
      origin: "assistant_proposed",
      status: "proposed",
      data_class: "internal",
      created_at: isoAt(now),
      updated_at: isoAt(now),
      ...(expiresAtMs !== undefined && { expires_at: isoAt(expiresAtMs) }),
    };
    memories.set(row.id, row);
    return row;
  };

  return {
    handle,
    propose,
    list: () => [...memories.values()],
  };
};
