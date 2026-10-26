import { isFileRef } from "../area-guards.js";
import type { Caller } from "../call.js";
import { sdkError } from "../errors.js";
import type { ExportRequest, ExportRequestFormat, FileRef } from "../gen/conversation/index.js";
import type { CallOptions } from "../transport.js";
import { pathId } from "../validate.js";

// Downloadable files from a turn's verified rich blocks (1.2.0). The core re-reads blocks it already
// validated and stores an OriginGenerated FileRef; never model-written file bytes. Needs `exports.create`.

const TURN_ID = /^trn_[0-9A-HJKMNP-TV-Z]{26}$/;
const BLOCK_ID = /^[A-Za-z0-9._:-]{1,128}$/;
const FORMATS: readonly ExportRequestFormat[] = ["csv", "xlsx", "html", "md"];

/** Body of `exports.create` with camelCase aliases accepted at the call site. */
export type ExportCreateInput = ExportRequest | {
  turnId: string;
  blockIds?: string[];
  format: ExportRequestFormat;
};

/** The exports area of the client: `client.exports`. */
export interface ExportsApi {
  /**
   * Generates a downloadable file from a turn's verified blocks and returns its `FileRef`
   * (`origin: generated`). Download with `client.files.download(file_id)`. Needs `exports.create`.
   */
  create: (sessionId: string, body: ExportCreateInput, options?: CallOptions) => Promise<FileRef>;
}

const invalid = (argument: string, reason: string) => sdkError("validation_failed", `${argument}: ${reason}`, { details: { argument } });

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const exportBody = (body: unknown): string => {
  if (!isRecord(body)) throw invalid("body", "it must be an object with turn_id (or turnId) and format");
  const turn = body["turn_id"] ?? body["turnId"];
  if (typeof turn !== "string" || !TURN_ID.test(turn)) throw invalid("turnId", "it is not a well-formed turn id");
  const format = body["format"];
  if (typeof format !== "string" || !(FORMATS as readonly string[]).includes(format)) {
    throw invalid("format", "it must be csv, xlsx, html or md");
  }
  const rawIds = body["block_ids"] ?? body["blockIds"];
  let blockIds: string[] | undefined;
  if (rawIds !== undefined) {
    if (!Array.isArray(rawIds) || rawIds.length > 64) throw invalid("blockIds", "it must be an array of at most 64 ids");
    const seen = new Set<string>();
    blockIds = [];
    for (const id of rawIds) {
      if (typeof id !== "string" || !BLOCK_ID.test(id)) throw invalid("blockIds", "each id must be 1 to 128 safe characters");
      if (seen.has(id)) throw invalid("blockIds", "ids must be unique");
      seen.add(id);
      blockIds.push(id);
    }
  }
  const wire: ExportRequest = { turn_id: turn as ExportRequest["turn_id"], format: format as ExportRequestFormat };
  if (blockIds !== undefined && blockIds.length > 0) wire.block_ids = blockIds;
  return JSON.stringify(wire);
};

/** Builds the exports area over a caller. */
export const createExportsApi = (caller: Caller): ExportsApi => ({
  create: async (sessionId, body, options) =>
    caller.json("createExport", { session_id: pathId("sessionId", sessionId) }, isFileRef, "export", options, {
      body: { contentType: "application/json", payload: exportBody(body) },
    }),
});
