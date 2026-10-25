import { createHash } from "node:crypto";
import type { IncomingMessage, ServerResponse } from "node:http";
import type { FileRef } from "@alixai/sdk";
import { BodyTooLargeError, readBody, sendEmpty, sendError, sendJson } from "../http";
import { fixedId, isoAt, newId } from "../ids";
import { energyChartPng, fireSchedulePdf, floorPlanPng, hvacContractPdf, maintenancePolicyPdf, ventilationPdf, workOrdersCsv } from "./fixtures";

// The files API (schemas/api/paths/files.yaml) over an in-memory store. A file is known only by its FileRef; an upload is checked
// the way the core checks one: the declared type must be allowed, the bytes must be what the type says (sniffed), the size is
// bounded, and SVG, HTML, XML and scripts are never stored. A reference that does not resolve is the same `scope_violation` whether
// it never existed or was deleted. Previews exist only for raster images and text.

export const DEMO_OWNER = "usr_demo_alex";
const APPLICATION_OWNER = "app_northwind_facilities";

const ALLOWED = ["image/png", "image/jpeg", "image/gif", "image/webp", "application/pdf", "text/plain", "text/markdown", "text/csv", "application/json"] as const;
const RASTER = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);
const TEXT = new Set(["text/plain", "text/markdown", "text/csv", "application/json"]);
const NEVER = /(svg|html|xml|javascript|ecmascript|x-sh|x-php)/;
const FILE_ID = /^fil_[0-9A-HJKMNP-TV-Z]{26}$/;
const PREVIEW_TEXT_BYTES = 4096;
const MULTIPART_ALLOWANCE = 64 * 1024;
const EPHEMERAL_TTL_MS = 2 * 60 * 60 * 1000;

interface StoredFile {
  ref: FileRef;
  bytes: Buffer;
}

export interface FileStoreOptions {
  /** The largest file accepted, in megabytes (the effective `attachments.max_file_mb`). Default 8. */
  maxFileMb: number;
}

/** Fixed ids of the files the mock starts with, so citations and scenarios can point at them. */
export const SEEDED_FILES = Object.freeze({
  maintenancePolicy: fixedId("fil", 1),
  hvacContract: fixedId("fil", 2),
  fireSchedule: fixedId("fil", 3),
  ventilation: fixedId("fil", 4),
  floorPlan: fixedId("fil", 5),
  energyChart: fixedId("fil", 6),
  workOrdersCsv: fixedId("fil", 7),
});

const sniff = (bytes: Buffer): string | undefined => {
  if (bytes.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]))) return "image/png";
  if (bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) return "image/jpeg";
  const head = bytes.subarray(0, 12).toString("latin1");
  if (head.startsWith("GIF87a") || head.startsWith("GIF89a")) return "image/gif";
  if (head.startsWith("RIFF") && head.slice(8, 12) === "WEBP") return "image/webp";
  if (head.startsWith("%PDF-")) return "application/pdf";
  return undefined;
};

const isPlainText = (bytes: Buffer): boolean => {
  if (bytes.includes(0)) return false;
  try {
    const text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return !/^\s*<(\?xml|svg|!doctype|html|script)/i.test(text);
  } catch {
    return false;
  }
};

/** A bare file name: no directories, no control characters, at most 255 characters. */
const cleanName = (name: string): string => {
  const base = name.split(/[\\/]/).pop() ?? "";
  const clean = base.replace(/[\u0000-\u001f\u007f]/g, "").trim().slice(0, 255);
  return clean === "" ? "upload" : clean;
};

const hashOf = (bytes: Buffer): string => `sha256:${createHash("sha256").update(bytes).digest("hex")}`;

export interface FileStore {
  get: (fileId: string) => FileRef | undefined;
  list: () => FileRef[];
  /** Handles the files routes; false when the path is not one of them. */
  handle: (req: IncomingMessage, res: ServerResponse, path: string) => Promise<boolean>;
}

export const createFileStore = (options: FileStoreOptions): FileStore => {
  const files = new Map<string, StoredFile>();
  const maxBytes = Math.round(options.maxFileMb * 1024 * 1024);

  const seed = (fileId: string, name: string, mime: string, bytes: Buffer): void => {
    files.set(fileId, {
      bytes,
      ref: {
        file_id: fileId,
        name,
        mime_type: mime,
        size_bytes: bytes.length,
        content_hash: hashOf(bytes),
        owner: APPLICATION_OWNER,
        origin: "application",
        treatment: "application_object",
        permissions: ["read", "preview", "download"],
        previewable: RASTER.has(mime) || TEXT.has(mime),
        expires_at: isoAt(Date.now() + 30 * 24 * 60 * 60 * 1000),
      },
    });
  };
  seed(SEEDED_FILES.maintenancePolicy, "preventive-maintenance-policy-v12.pdf", "application/pdf", maintenancePolicyPdf());
  seed(SEEDED_FILES.hvacContract, "coolwave-hvac-service-contract.pdf", "application/pdf", hvacContractPdf());
  seed(SEEDED_FILES.fireSchedule, "fire-safety-inspection-schedule-2026.pdf", "application/pdf", fireSchedulePdf());
  seed(SEEDED_FILES.ventilation, "elm-street-ventilation-requirements.pdf", "application/pdf", ventilationPdf());
  seed(SEEDED_FILES.floorPlan, "harbor-point-level-3-floor-plan.png", "image/png", floorPlanPng());
  seed(SEEDED_FILES.energyChart, "energy-use-q3-2026.png", "image/png", energyChartPng());
  seed(SEEDED_FILES.workOrdersCsv, "work-orders-2026-q3.csv", "text/csv", workOrdersCsv());

  const live = (fileId: string): StoredFile | undefined => {
    const found = files.get(fileId);
    if (found === undefined) return undefined;
    if (Date.parse(found.ref.expires_at) <= Date.now()) {
      files.delete(fileId);
      return undefined;
    }
    return found;
  };

  const notVisible = (res: ServerResponse): void => sendError(res, 403, "scope_violation", "the file reference does not resolve in the caller's scope");

  const upload = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const contentType = req.headers["content-type"] ?? "";
    if (!/^multipart\/form-data/i.test(contentType)) {
      sendError(res, 415, "validation_failed", "an upload is multipart/form-data", { reason: "content_type" });
      return;
    }
    let body: Buffer;
    try {
      body = await readBody(req, maxBytes + MULTIPART_ALLOWANCE);
    } catch (error) {
      if (error instanceof BodyTooLargeError) sendError(res, 413, "validation_failed", `the file is larger than ${options.maxFileMb} MB`, { reason: "too_large" });
      else sendError(res, 400, "validation_failed", "the upload could not be read");
      return;
    }
    let form: FormData;
    try {
      form = await new Request("http://demo-mock.invalid/v1/files", { method: "POST", headers: { "content-type": contentType }, body: new Uint8Array(body) }).formData();
    } catch {
      sendError(res, 400, "validation_failed", "the multipart body is malformed");
      return;
    }
    const parts = form.getAll("file");
    const part = parts[0];
    if (parts.length !== 1 || part === undefined || typeof part === "string") {
      sendError(res, 400, "validation_failed", "an upload has exactly one file part", { reason: "file_part" });
      return;
    }
    const treatment = form.get("treatment") ?? "ephemeral_context";
    if (treatment !== "ephemeral_context" && treatment !== "rag_source" && treatment !== "application_object") {
      sendError(res, 400, "validation_failed", "treatment is ephemeral_context, rag_source or application_object", { reason: "treatment" });
      return;
    }
    if (treatment === "application_object") {
      sendError(res, 403, "permission_denied", "the attachment policy of this application does not accept application objects from a chat", { reason: "treatment_not_allowed" });
      return;
    }
    const bytes = Buffer.from(await part.arrayBuffer());
    const declared = part.type.split(";")[0]?.trim().toLowerCase() ?? "";
    if (bytes.length === 0) {
      sendError(res, 400, "validation_failed", "the file is empty", { reason: "empty" });
      return;
    }
    if (bytes.length > maxBytes) {
      sendError(res, 400, "validation_failed", `the file is larger than ${options.maxFileMb} MB`, { reason: "too_large", max_file_mb: options.maxFileMb });
      return;
    }
    if (declared === "" || declared === "application/octet-stream") {
      sendError(res, 400, "validation_failed", "the file part must declare its media type", { reason: "mime_declared" });
      return;
    }
    if (NEVER.test(declared) || !(ALLOWED as readonly string[]).includes(declared)) {
      sendError(res, 400, "validation_failed", `files of type ${declared} are not accepted here`, { reason: "mime_not_allowed", allowed: [...ALLOWED] });
      return;
    }
    const sniffed = sniff(bytes);
    const matches = TEXT.has(declared) ? sniffed === undefined && isPlainText(bytes) : sniffed === declared;
    if (!matches) {
      sendError(res, 400, "validation_failed", `the content is not ${declared}`, { reason: "mime_mismatch" });
      return;
    }
    const fileId = newId("fil");
    const ref: FileRef = {
      file_id: fileId,
      name: cleanName(part.name),
      mime_type: declared,
      size_bytes: bytes.length,
      content_hash: hashOf(bytes),
      owner: DEMO_OWNER,
      origin: "upload",
      treatment,
      permissions: ["read", "preview"],
      previewable: RASTER.has(declared) || TEXT.has(declared),
      expires_at: isoAt(Date.now() + EPHEMERAL_TTL_MS),
    };
    files.set(fileId, { ref, bytes });
    sendJson(res, 201, ref, { Location: `/v1/files/${fileId}` });
  };

  const sandboxHeaders = (mime: string, length: number): Record<string, string> => ({
    "Content-Type": mime,
    "Content-Length": String(length),
    "X-Content-Type-Options": "nosniff",
    "Content-Security-Policy": "sandbox; default-src 'none'",
    "Cache-Control": "no-store",
  });

  const handle = async (req: IncomingMessage, res: ServerResponse, path: string): Promise<boolean> => {
    const method = req.method ?? "GET";
    if (path === "/v1/files") {
      if (method !== "POST") return false;
      await upload(req, res);
      return true;
    }
    const match = /^\/v1\/files\/([^/]+)(\/content|\/preview)?$/.exec(path);
    if (match === null) return false;
    const fileId = decodeURIComponent(match[1] as string);
    const sub = match[2];
    if (!FILE_ID.test(fileId)) {
      sendError(res, 400, "validation_failed", "file_id is not a file id", { argument: "file_id" });
      return true;
    }
    const found = live(fileId);
    if (found === undefined) {
      notVisible(res);
      return true;
    }
    if (sub === undefined && method === "GET") {
      sendJson(res, 200, found.ref);
    } else if (sub === undefined && method === "DELETE") {
      if (found.ref.owner !== DEMO_OWNER) sendError(res, 403, "permission_denied", "only the owner can delete a file");
      else {
        files.delete(fileId);
        sendEmpty(res, 204);
      }
    } else if (sub === "/content" && method === "GET") {
      if (!found.ref.permissions.includes("download")) {
        sendError(res, 403, "permission_denied", "this file may be previewed but not downloaded", { reason: "download_not_permitted" });
        return true;
      }
      res.writeHead(200, { ...sandboxHeaders(found.ref.mime_type, found.bytes.length), "Content-Disposition": `attachment; filename="${found.ref.name.replace(/"/g, "")}"` });
      res.end(found.bytes);
    } else if (sub === "/preview" && method === "GET") {
      if (!found.ref.previewable) {
        sendError(res, 415, "validation_failed", `there is no safe preview for ${found.ref.mime_type}`, { reason: "not_previewable" });
      } else if (RASTER.has(found.ref.mime_type)) {
        res.writeHead(200, sandboxHeaders(found.ref.mime_type, found.bytes.length));
        res.end(found.bytes);
      } else {
        const cut = found.bytes.length > PREVIEW_TEXT_BYTES;
        let text = found.bytes.subarray(0, PREVIEW_TEXT_BYTES).toString("utf8");
        if (cut) text = text.replace(/�$/, "");
        const out = Buffer.from(text, "utf8");
        res.writeHead(200, { ...sandboxHeaders("text/plain; charset=utf-8", out.length), ...(cut && { "X-Alixai-Preview-Truncated": "true" }) });
        res.end(out);
      }
    } else {
      return false;
    }
    return true;
  };

  return {
    get: (fileId) => live(fileId)?.ref,
    list: () => [...files.keys()].map((id) => live(id)?.ref).filter((ref): ref is FileRef => ref !== undefined),
    handle,
  };
};
