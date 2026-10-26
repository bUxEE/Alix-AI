import type { Caller } from "../call.js";
import { sdkError } from "../errors.js";
import type { MemoryCreate } from "../gen/memory/memory-create.js";
import type { MemoryItem } from "../gen/memory/memory-item.js";
import type { MemoryList } from "../gen/memory/memory-list.js";
import type { MemoryPatch } from "../gen/memory/memory-patch.js";
import type { CallOptions } from "../transport.js";
import type { MemoryOperationId } from "./operations.js";

// Long-term memory (ADR 0042, 1.2.0). Scope is the verified token (principal × tenant × workspace).
// Proposals confirm/reject only through these structured calls; typed chat text never confirms.

const MEMORY_ID = /^mem_[0-9A-HJKMNP-TV-Z]{26}$/;
const STATUSES = ["proposed", "active", "rejected"] as const;

export type MemoryStatusFilter = (typeof STATUSES)[number];

export interface MemoryListOptions extends CallOptions {
  q?: string;
  status?: MemoryStatusFilter;
  cursor?: string;
  limit?: number;
}

/** The memory area of the client: `client.memory`. */
export interface MemoryApi {
  list: (options?: MemoryListOptions) => Promise<MemoryList>;
  create: (body: MemoryCreate, options?: CallOptions) => Promise<MemoryItem>;
  patch: (id: string, body: MemoryPatch, options?: CallOptions) => Promise<MemoryItem>;
  delete: (id: string, options?: CallOptions) => Promise<void>;
  confirm: (id: string, options?: CallOptions) => Promise<MemoryItem>;
  reject: (id: string, options?: CallOptions) => Promise<MemoryItem>;
}

const invalid = (argument: string, reason: string) => sdkError("validation_failed", `${argument}: ${reason}`, { details: { argument } });

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const memoryId = (value: unknown): string => {
  if (typeof value !== "string" || !MEMORY_ID.test(value)) throw invalid("id", "it is not a well-formed memory id");
  return value;
};

const isMemoryList = (value: unknown): value is MemoryList => isRecord(value) && Array.isArray(value["items"]);

const isMemoryItem = (value: unknown): value is MemoryItem =>
  isRecord(value) && typeof value["id"] === "string" && typeof value["text"] === "string";

/** Builds the memory area over a caller. */
export const createMemoryApi = (caller: Caller): MemoryApi => {
  const json = <T>(
    operation: MemoryOperationId,
    params: Record<string, string>,
    guard: (value: unknown) => value is T,
    what: string,
    options: CallOptions | undefined,
    extra?: { body?: string; query?: Record<string, string> },
  ): Promise<T> =>
    caller.json(operation, params, guard, what, options, {
      ...(extra?.query !== undefined && { query: extra.query }),
      ...(extra?.body !== undefined && { body: { contentType: "application/json", payload: extra.body } }),
    });

  return {
    list: async (options) => {
      const query: Record<string, string> = {};
      if (options?.q !== undefined) {
        if (typeof options.q !== "string" || options.q.length > 256) throw invalid("q", "it must be a string of at most 256 characters");
        if (options.q.length > 0) query["q"] = options.q;
      }
      if (options?.status !== undefined) {
        if (!(STATUSES as readonly string[]).includes(options.status)) throw invalid("status", "it must be proposed, active or rejected");
        query["status"] = options.status;
      }
      if (options?.cursor !== undefined) {
        if (typeof options.cursor !== "string" || options.cursor.length > 256) throw invalid("cursor", "it must be a string of at most 256 characters");
        if (options.cursor.length > 0) query["cursor"] = options.cursor;
      }
      if (options?.limit !== undefined) {
        if (!Number.isInteger(options.limit) || options.limit < 1 || options.limit > 200) throw invalid("limit", "it must be an integer from 1 to 200");
        query["limit"] = String(options.limit);
      }
      return json("listMemory", {}, isMemoryList, "memory list", options, Object.keys(query).length > 0 ? { query } : undefined);
    },

    create: async (body, options) => {
      if (!isRecord(body) || typeof body.text !== "string" || body.text.length === 0) throw invalid("text", "it is required");
      return json("createMemory", {}, isMemoryItem, "memory item", options, { body: JSON.stringify(body) });
    },

    patch: async (id, body, options) => {
      const mid = memoryId(id);
      if (!isRecord(body)) throw invalid("body", "it must be an object");
      return json("patchMemory", { id: mid }, isMemoryItem, "memory item", options, { body: JSON.stringify(body) });
    },

    delete: async (id, options) => {
      await caller.empty("deleteMemory", { id: memoryId(id) }, options);
    },

    confirm: async (id, options) => json("confirmMemory", { id: memoryId(id) }, isMemoryItem, "memory item", options),

    reject: async (id, options) => json("rejectMemory", { id: memoryId(id) }, isMemoryItem, "memory item", options),
  };
};
