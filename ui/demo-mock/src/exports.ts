import type { IncomingMessage, ServerResponse } from "node:http";
import { BodyTooLargeError, isRecord, readJson, sendError, sendJson } from "./http";
import { isoAt, newId } from "./ids";

// Scripted POST /v1/sessions/{session_id}/exports (1.2.0): returns an OriginGenerated FileRef.
// The demo does not render real blocks; kits exercise the host download path against this shape.

const CREATE = /^\/v1\/sessions\/([A-Za-z0-9_.-]{1,128})\/exports$/;
const TURN_ID = /^trn_[0-9A-HJKMNP-TV-Z]{26}$/;
const FORMATS = new Set(["csv", "xlsx", "html", "md"]);
const MIME: Record<string, string> = {
  csv: "text/csv",
  xlsx: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
  html: "text/html",
  md: "text/markdown",
};

export interface ExportsHandler {
  handle: (req: IncomingMessage, res: ServerResponse, path: string) => Promise<boolean>;
}

export const createExportsHandler = (): ExportsHandler => {
  const handle = async (req: IncomingMessage, res: ServerResponse, path: string): Promise<boolean> => {
    const created = CREATE.exec(path);
    if (req.method !== "POST" || created === null) return false;

    let body: unknown;
    try {
      body = await readJson(req, 64 * 1024);
    } catch (error) {
      sendError(res, error instanceof BodyTooLargeError ? 413 : 400, "validation_failed", "the export body could not be read");
      return true;
    }
    if (!isRecord(body) || typeof body["turn_id"] !== "string" || !TURN_ID.test(body["turn_id"])) {
      sendError(res, 400, "validation_failed", "turn_id is required");
      return true;
    }
    if (typeof body["format"] !== "string" || !FORMATS.has(body["format"])) {
      sendError(res, 400, "validation_failed", "format must be csv, xlsx, html or md");
      return true;
    }
    if (String(body["turn_id"]).endsWith("FA0")) {
      sendError(res, 404, "validation_failed", "the turn is not available for export", { reason: "not_found" });
      return true;
    }
    const format = body["format"] as string;
    const fileId = newId("fil");
    const ext = format === "md" ? "md" : format;
    sendJson(
      res,
      201,
      {
        file_id: fileId,
        name: `export.${ext}`,
        mime_type: MIME[format],
        size_bytes: 32,
        owner: "usr_demo_alex",
        origin: "generated",
        permissions: ["read", "download"],
        previewable: format !== "xlsx",
        expires_at: isoAt(Date.now() + 3_600_000),
      },
      { Location: `/v1/files/${fileId}` },
    );
    return true;
  };

  return { handle };
};
