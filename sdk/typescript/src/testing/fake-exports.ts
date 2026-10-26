import type { FileRef } from "../gen/conversation/index.js";
import type { CoreOperationId } from "../operations.js";
import type { FakeContext, FakeScope, Json } from "./context.js";
import { requireCapability } from "./context.js";
import { typedId } from "./ids.js";

const FORMATS = new Set(["csv", "xlsx", "html", "md"]);
const TURN_ID = /^trn_[0-9A-HJKMNP-TV-Z]{26}$/;
const BLOCK_ID = /^[A-Za-z0-9._:-]{1,128}$/;

const MIME: Record<string, string> = {
  csv: "text/csv",
  xlsx: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
  html: "text/html",
  md: "text/markdown",
};

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** Scripted POST /v1/sessions/{session_id}/exports (release 1.2.0). */
export const createFakeExports = (ctx: FakeContext) => {
  const handle = (operation: CoreOperationId, scope: FakeScope, params: Record<string, string>, body: unknown): Response | undefined => {
    if (operation !== "createExport") return undefined;
    if (params["session_id"] !== scope.session_id) {
      return ctx.problem("scope_violation", "the session is not the token's", { status: 403 });
    }
    const denied = requireCapability(ctx, scope, "exports.create");
    if (denied !== undefined) return denied;
    if (!isRecord(body) || typeof body["turn_id"] !== "string" || !TURN_ID.test(body["turn_id"])) {
      return ctx.problem("validation_failed", "turn_id is required");
    }
    if (typeof body["format"] !== "string" || !FORMATS.has(body["format"])) {
      return ctx.problem("validation_failed", "format must be csv, xlsx, html or md");
    }
    if (body["block_ids"] !== undefined) {
      if (!Array.isArray(body["block_ids"]) || body["block_ids"].length > 64) {
        return ctx.problem("validation_failed", "block_ids must be an array of at most 64 ids");
      }
      for (const id of body["block_ids"]) {
        if (typeof id !== "string" || !BLOCK_ID.test(id)) {
          return ctx.problem("validation_failed", "a block id is not well formed");
        }
      }
    }
    // A turn id ending in FA0 is the scripted not_found (scope isolation / missing turn).
    if (String(body["turn_id"]).endsWith("FA0")) {
      return ctx.problem("validation_failed", "the turn is not available for export", {
        status: 404,
        details: { reason: "not_found" },
      });
    }
    const format = body["format"] as string;
    const id = typedId("fil", ctx.nowMs());
    const ref: FileRef = {
      file_id: id,
      name: `export.${format === "md" ? "md" : format}`,
      mime_type: MIME[format] ?? "application/octet-stream",
      size_bytes: 32,
      owner: scope.principal_id,
      origin: "generated",
      permissions: ["read", "download"],
      previewable: format === "html" || format === "md" || format === "csv",
      expires_at: ctx.iso(ctx.nowMs() + 3_600_000),
    } as FileRef;
    return ctx.json(201, ref as unknown as Json, { Location: `/v1/files/${id}` });
  };

  return { handle };
};
