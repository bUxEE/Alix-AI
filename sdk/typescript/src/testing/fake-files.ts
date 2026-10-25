import type { FileRef } from "../gen/conversation/index.js";
import { sameOwner } from "./context.js";
import type { FakeContext, FakeScope } from "./context.js";
import { typedId } from "./ids.js";

// The files area of the fake server (schemas/api/paths/files.yaml): multipart upload with the field order the core requires, the
// declared type checked against the bytes, scoped references that resolve only for their owner (and, for an ephemeral context, only in
// their session), expiry, quota, download behind the `download` permission and previews of images and text. The bytes live in memory.

export interface FakeFilesOptions {
  /** The media types the host's attachment policy allows. Default: images, PDF, text, JSON, MP3, WAV and MP4. */
  allowedTypes?: readonly string[];
  /** The treatments the policy accepts. Default: all three. A file with another one is `permission_denied` (`treatment_not_allowed`). */
  treatments?: readonly string[];
  /** The largest file in bytes. Default 1 MiB (413). */
  maxFileBytes?: number;
  /** How long a reference resolves, in milliseconds. Default 15 minutes. */
  ttlMs?: number;
  /** How many live files a tenant may hold. Default 100 (507). */
  quota?: number;
  /** A text preview is cut to this many bytes. Default 64 KiB. */
  previewTextBytes?: number;
}

const DEFAULT_TYPES = [
  "image/png",
  "image/jpeg",
  "image/gif",
  "image/webp",
  "application/pdf",
  "text/plain",
  "text/markdown",
  "text/csv",
  "application/json",
  "audio/mpeg",
  "audio/wav",
  "video/mp4",
];

/** Types the core never stores whatever the policy says. */
const NEVER_STORED = new Set([
  "text/html",
  "application/xhtml+xml",
  "image/svg+xml",
  "text/xml",
  "application/xml",
  "text/javascript",
  "application/javascript",
]);

const IMAGE_PREVIEW = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);
const isText = (type: string): boolean => type.startsWith("text/") || type === "application/json";

interface StoredFile {
  ref: FileRef;
  data: Uint8Array;
  owner: FakeScope;
  expiresAt: number;
  deleted: boolean;
}

const startsWith = (data: Uint8Array, magic: readonly number[], at = 0): boolean => magic.every((byte, index) => data[at + index] === byte);

/** Whether the bytes are what the declared type says they are (the core sniffs; the fake knows the common signatures). */
const matchesType = (data: Uint8Array, type: string): boolean => {
  switch (type) {
    case "image/png":
      return startsWith(data, [0x89, 0x50, 0x4e, 0x47]);
    case "image/jpeg":
      return startsWith(data, [0xff, 0xd8, 0xff]);
    case "image/gif":
      return startsWith(data, [0x47, 0x49, 0x46, 0x38]);
    case "application/pdf":
      return startsWith(data, [0x25, 0x50, 0x44, 0x46]);
    case "image/webp":
      return startsWith(data, [0x52, 0x49, 0x46, 0x46]) && startsWith(data, [0x57, 0x45, 0x42, 0x50], 8);
    default:
      break;
  }
  if (isText(type)) {
    if (data.includes(0)) return false;
    try {
      const text = new TextDecoder("utf-8", { fatal: true }).decode(data);
      if (type === "application/json") JSON.parse(text);
      return true;
    } catch {
      return false;
    }
  }
  return true; // audio and video: the fake does not parse containers
};

/** A bare file name: no path separators, no control characters. */
const bareName = (name: string): string => {
  const base = name.split(/[\\/]/).pop() ?? "";
  // eslint-disable-next-line no-control-regex
  const clean = base.replace(/[\u0000-\u001f\u007f]/g, "").trim();
  return clean === "" ? "file" : clean.slice(0, 255);
};

const sha256 = async (data: Uint8Array<ArrayBuffer>): Promise<string> =>
  `sha256:${Array.from(new Uint8Array(await globalThis.crypto.subtle.digest("SHA-256", data)), (b) => b.toString(16).padStart(2, "0")).join("")}`;

export interface FakeFiles {
  /** Answers one of the file operations. */
  handle: (
    operation: "uploadFile" | "getFile" | "deleteFile" | "downloadFile" | "previewFile",
    scope: FakeScope,
    params: Record<string, string>,
    request: Request,
    raw: Uint8Array<ArrayBuffer>,
  ) => Promise<Response>;
  /**
   * Whether every file id resolves for the caller as an attachment of a turn: the caller's own, in the caller's session when it is an
   * ephemeral context, not deleted, not expired. One that does not (another tenant's, principal's or session's file, a deleted one, an
   * expired one, one that never existed) is not told apart from the others: the turn gets one `scope_violation`.
   */
  attachable: (scope: FakeScope, ids: readonly string[]) => boolean;
  /** The references of the live files, for assertions. */
  list: () => FileRef[];
  reset: () => void;
}

export const createFakeFiles = (context: FakeContext, options: FakeFilesOptions = {}): FakeFiles => {
  const allowed = new Set(options.allowedTypes ?? DEFAULT_TYPES);
  const treatments = options.treatments ?? ["ephemeral_context", "rag_source", "application_object"];
  const maxBytes = options.maxFileBytes ?? 1024 * 1024;
  const ttlMs = options.ttlMs ?? 15 * 60 * 1000;
  const quota = options.quota ?? 100;
  const previewBytes = options.previewTextBytes ?? 64 * 1024;
  const files = new Map<string, StoredFile>();

  const bad = (reason: string, message: string, status = 400): Response =>
    context.problem("validation_failed", message, { status, details: { reason } });

  const live = (): StoredFile[] => [...files.values()].filter((f) => !f.deleted);

  /** The file a request may use: its owner's, in the same session when it is an ephemeral context. Anything else is one `scope_violation`. */
  const resolve = (scope: FakeScope, id: string): StoredFile | Response => {
    const found = files.get(id);
    if (found === undefined || found.deleted || !sameOwner(found.owner, scope)) return context.levelViolation("file");
    if (found.ref.treatment === "ephemeral_context" && found.owner.session_id !== scope.session_id) return context.levelViolation("file");
    if (found.expiresAt <= context.nowMs()) return context.problem("expired", "the file reference has expired", { status: 410 });
    return found;
  };

  const upload = async (scope: FakeScope, request: Request, raw: Uint8Array<ArrayBuffer>): Promise<Response> => {
    const contentType = request.headers.get("Content-Type") ?? "";
    if (!contentType.toLowerCase().startsWith("multipart/form-data")) {
      return context.problem("validation_failed", "an upload is multipart/form-data", { status: 415 });
    }
    let form: FormData;
    try {
      form = await new Response(raw, { headers: { "Content-Type": contentType } }).formData();
    } catch {
      return bad("malformed_form", "the multipart body cannot be read");
    }
    const entries = [...form.entries()];
    let treatment = "ephemeral_context";
    let file: File | undefined;
    for (const [name, value] of entries) {
      if (name === "treatment" && typeof value === "string" && file === undefined) {
        if (!["ephemeral_context", "rag_source", "application_object"].includes(value)) return bad("treatment_unknown", "treatment is not one of the three");
        treatment = value;
      } else if (name === "file" && typeof value !== "string" && file === undefined) {
        file = value;
      } else {
        return bad("malformed_form", `the field ${name} is unexpected, repeated or out of order (treatment comes before file)`);
      }
    }
    if (file === undefined) return bad("malformed_form", "the file part is missing");
    if (!treatments.includes(treatment)) {
      return context.problem("permission_denied", "the attachment policy does not accept this treatment", { details: { reason: "treatment_not_allowed" } });
    }
    const type = file.type.split(";")[0]?.trim().toLowerCase() ?? "";
    if (type === "") return bad("mime_declared", "the file part declares no media type");
    if (NEVER_STORED.has(type) || !allowed.has(type)) return bad("mime_not_allowed", "the attachment policy does not allow this media type");
    if (file.size === 0) return bad("empty", "an empty file is never stored");
    if (file.size > maxBytes) return bad("too_large", `the file is larger than ${maxBytes} bytes`, 413);
    const data = new Uint8Array(await file.arrayBuffer());
    if (!matchesType(data, type)) return bad("mime_mismatch", "the bytes are not what the declared media type says");
    const tenantFiles = live().filter((f) => f.owner.application_id === scope.application_id && f.owner.tenant_id === scope.tenant_id);
    if (tenantFiles.length >= quota) {
      return context.problem("resource_exhausted", "the tenant is over its quota of live files", { status: 507, details: { reason: "quota_files" } });
    }
    const previewable = IMAGE_PREVIEW.has(type) || isText(type);
    // The host policy's defaults per treatment, as the core has them: an ephemeral context is readable and previewable by its uploader,
    // an application object also downloadable, a RAG source readable only. Whether a previewer exists is a separate fact.
    const permissions: FileRef["permissions"] =
      treatment === "rag_source" ? ["read"] : treatment === "application_object" ? ["read", "preview", "download"] : ["read", "preview"];
    const id = typedId("fil", context.nowMs());
    const ref: FileRef = {
      file_id: id,
      name: bareName(file.name),
      mime_type: type,
      size_bytes: data.byteLength,
      content_hash: await sha256(data as Uint8Array<ArrayBuffer>),
      owner: scope.principal_id,
      origin: "upload",
      treatment: treatment as FileRef["treatment"],
      permissions,
      previewable,
      expires_at: context.iso(context.nowMs() + ttlMs),
    } as FileRef;
    files.set(id, { ref, data, owner: scope, expiresAt: context.nowMs() + ttlMs, deleted: false });
    return context.json(201, ref, { Location: `/v1/files/${id}` });
  };

  const handle: FakeFiles["handle"] = async (operation, scope, params, request, raw) => {
    const refusal = context.quarantine();
    if (refusal !== undefined) return refusal;
    if (operation === "uploadFile") return upload(scope, request, raw);
    const id = params["file_id"] ?? "";
    if (!/^fil_[0-9A-HJKMNP-TV-Z]{26}$/.test(id)) return context.problem("validation_failed", "the file id is not well formed");
    const found = resolve(scope, id);
    if (found instanceof Response) return found;
    switch (operation) {
      case "getFile":
        return context.json(200, found.ref);
      case "deleteFile":
        found.deleted = true;
        return new Response(null, { status: 204, headers: { "Cache-Control": "no-store" } });
      case "downloadFile": {
        if (!found.ref.permissions.includes("download")) {
          return context.problem("permission_denied", "the policy does not grant the download permission for this file", { details: { reason: "permission_missing" } });
        }
        return new Response(found.data as Uint8Array<ArrayBuffer>, {
          status: 200,
          headers: {
            "Content-Type": found.ref.mime_type,
            "Content-Disposition": `attachment; filename="${found.ref.name.replace(/(["\\])/g, "\\$1")}"`,
            "Content-Length": String(found.data.byteLength),
            "X-Content-Type-Options": "nosniff",
            "Content-Security-Policy": "sandbox",
            "Cache-Control": "no-store",
          },
        });
      }
      default: {
        if (!found.ref.permissions.includes("preview")) {
          return context.problem("permission_denied", "the policy does not grant the preview permission for this file", { details: { reason: "permission_missing" } });
        }
        if (!found.ref.previewable) return bad("not_previewable", "there is no safe previewer for this file type", 415);
        const text = isText(found.ref.mime_type);
        const cut = text && found.data.byteLength > previewBytes;
        const body = cut ? found.data.slice(0, previewBytes) : found.data;
        return new Response(body as Uint8Array<ArrayBuffer>, {
          status: 200,
          headers: {
            "Content-Type": text ? "text/plain" : found.ref.mime_type,
            "Content-Length": String(body.byteLength),
            "X-Content-Type-Options": "nosniff",
            "Content-Security-Policy": "sandbox",
            "Cache-Control": "no-store",
            ...(cut && { "X-Alixai-Preview-Truncated": "true" }),
          },
        });
      }
    }
  };

  const attachable: FakeFiles["attachable"] = (scope, ids) => ids.every((id) => !(resolve(scope, id) instanceof Response));

  return {
    handle,
    attachable,
    list: () => live().map((f) => ({ ...f.ref })),
    reset: () => files.clear(),
  };
};
