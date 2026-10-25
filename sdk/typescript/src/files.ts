import type { FileContent, FilePreview, FileUpload } from "./api-types.js";
import { isFileRef } from "./area-guards.js";
import type { Caller } from "./call.js";
import { sdkError } from "./errors.js";
import type { FileRef } from "./gen/conversation/index.js";
import type { CallOptions } from "./transport.js";
import { FILE_ID } from "./validate.js";

// Scoped files (master plan 14.1): upload, inspect, download, preview and delete. A file is known to a client only by its FileRef:
// an opaque id and metadata, never a path or a URL. Every call re-checks scope, owner, session, expiry and permission on the server, so
// a reference that does not resolve for the caller is always the same `scope_violation`.

/** Options of an upload. */
export interface UploadOptions extends CallOptions {
  /**
   * The session the file belongs to, for a `FileRef` of `ephemeral_context`, which is readable inside the session that uploaded it.
   * Only needed when the client is not already bound to a session (`client.forSession`, a `Conversation`): the upload path names none,
   * so this is what the token provider is told, to mint a token for that session.
   */
  sessionId?: string;
}

/** Options of a download or preview. */
export interface ReadFileOptions extends CallOptions {
  /** Refuse a file larger than this many bytes (checked against `Content-Length` before the body is read, and again after). */
  maxBytes?: number;
}

/** The files area of the client: `client.files`. */
export interface FilesApi {
  /**
   * Stores one file for the caller and returns its reference. The request is checked locally first (a name, a media type, bytes,
   * and never an HTML, SVG, XML or script type, which the core never stores), so a malformed upload costs no context token. The core
   * sniffs the bytes: a declared type that is not what they are stores nothing (`validation_failed`, `details.reason`
   * `mime_mismatch`). To give the file to the model, name its `file_id` in the `attachments` of a text turn (`sendTurn`), or let
   * `sendMessage` upload and name it. Needs a host attachment policy that accepts it, and a context token for the session.
   */
  upload: (file: FileUpload, options?: UploadOptions) => Promise<FileRef>;
  /** The reference as the caller holds it: size, media type, owner, origin, treatment, permissions, preview, expiry. */
  get: (fileId: string, options?: CallOptions) => Promise<FileRef>;
  /** Deletes a file (only its owner can). The reference stops resolving at once. */
  delete: (fileId: string, options?: CallOptions) => Promise<void>;
  /** The file's bytes. Needs the `download` permission, which the host's policy grants per treatment. */
  download: (fileId: string, options?: ReadFileOptions) => Promise<FileContent>;
  /** A safe rendering (raster images, text). A type with no safe previewer is `validation_failed` (`not_previewable`). Needs `preview`. */
  preview: (fileId: string, options?: ReadFileOptions) => Promise<FilePreview>;
}

const MEDIA_TYPE = /^[a-z0-9][a-z0-9!#$&^_.+-]{0,126}\/[a-z0-9][a-z0-9!#$&^_.+-]{0,126}$/;
const MAX_NAME_CHARS = 255;

/** Types the core never stores; refused here so that the upload costs no token. The server stays the authority. */
const NEVER_STORED: ReadonlySet<string> = new Set([
  "text/html",
  "application/xhtml+xml",
  "image/svg+xml",
  "text/xml",
  "application/xml",
  "text/javascript",
  "application/javascript",
  "application/x-javascript",
  "application/ecmascript",
  "text/ecmascript",
]);

/** The treatments of the document. */
const TREATMENTS: readonly string[] = ["ephemeral_context", "rag_source", "application_object"];

const invalid = (argument: string, reason: string) => sdkError("validation_failed", `${argument}: ${reason}`, { details: { argument } });

/** A file id as it appears in a path: the pattern of the OpenAPI document. */
export const fileIdArgument = (value: unknown): string => {
  if (typeof value !== "string" || !FILE_ID.test(value)) throw invalid("fileId", "it is not a well-formed file id");
  return value;
};

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

/** The bytes of an upload as a Blob of the declared type, or the error for an upload that is not worth sending. */
const uploadForm = (file: unknown): FormData => {
  if (!isRecord(file)) throw invalid("file", "it must be an object with data, name and mimeType");
  for (const key of Object.keys(file)) if (!["data", "name", "mimeType", "treatment"].includes(key)) throw invalid("file", `unknown member "${key}"`);
  const { data, name, mimeType, treatment } = file;
  if (typeof name !== "string" || name === "" || name.length > MAX_NAME_CHARS || /[\u0000-\u001f\u007f]/.test(name)) {
    throw invalid("name", `it must be a file name of 1 to ${MAX_NAME_CHARS} characters without control characters`);
  }
  if (typeof mimeType !== "string" || !MEDIA_TYPE.test(mimeType)) throw invalid("mimeType", "it must be a media type such as image/png (lower case, no parameters)");
  if (NEVER_STORED.has(mimeType)) throw invalid("mimeType", "HTML, SVG, XML and script types are never stored");
  if (treatment !== undefined && (typeof treatment !== "string" || !TREATMENTS.includes(treatment))) {
    throw invalid("treatment", `it must be one of ${TREATMENTS.join(", ")}`);
  }
  let blob: Blob;
  if (data instanceof Blob) blob = data.type === mimeType ? data : data.slice(0, data.size, mimeType);
  else if (data instanceof Uint8Array) blob = new Blob([data as Uint8Array<ArrayBuffer>], { type: mimeType });
  else if (data instanceof ArrayBuffer) blob = new Blob([data], { type: mimeType });
  else throw invalid("data", "it must be a Blob, a Uint8Array or an ArrayBuffer");
  if (blob.size === 0) throw invalid("data", "an empty file is never stored");
  const form = new FormData();
  // The core requires `treatment` before the file part.
  if (treatment !== undefined) form.append("treatment", treatment);
  form.append("file", blob, name);
  return form;
};

/** The file name of a `Content-Disposition: attachment; filename="x"` header (or `filename*=UTF-8''x`), when it has one. */
const dispositionName = (header: string | null): string | undefined => {
  if (header === null) return undefined;
  const star = /filename\*\s*=\s*(?:UTF-8|utf-8)''([^;]+)/.exec(header);
  if (star?.[1] !== undefined) {
    try {
      return decodeURIComponent(star[1].trim());
    } catch {
      return undefined;
    }
  }
  const plain = /filename\s*=\s*(?:"((?:[^"\\]|\\.)*)"|([^;]+))/.exec(header);
  const value = plain?.[1] ?? plain?.[2];
  return value === undefined ? undefined : value.replace(/\\(.)/g, "$1").trim();
};

const maxBytesOption = (value: unknown): number | undefined => {
  if (value === undefined) return undefined;
  if (typeof value !== "number" || !Number.isInteger(value) || value < 1) throw invalid("maxBytes", "it must be a positive integer");
  return value;
};

/** Builds the files area over a caller. `uploadTimeoutMs` is the deadline of an upload (it carries a body). */
export const createFilesApi = (caller: Caller, uploadTimeoutMs: number): FilesApi => {
  /** Reads a binary body in full, refusing one over `maxBytes`, and maps a read failure to a typed error. */
  const readBytes = async (
    operation: "downloadFile" | "previewFile",
    fileId: string,
    options: ReadFileOptions | undefined,
  ): Promise<{ data: Uint8Array; response: Response }> => {
    const limit = maxBytesOption(options?.maxBytes);
    const { response, scope } = await caller.open(operation, { file_id: fileId }, options);
    // The deadline covers the request until its head arrives: a large file then takes as long as it takes (the caller's signal
    // still stops it).
    scope.endDeadline();
    try {
      const declared = Number(response.headers.get("Content-Length"));
      if (limit !== undefined && Number.isFinite(declared) && declared > limit) {
        await response.body?.cancel().catch(() => undefined);
        throw sdkError("resource_exhausted", `the file is larger than the ${limit} bytes allowed`, { details: { reason: "too_large", limit } });
      }
      let data: Uint8Array;
      try {
        data = new Uint8Array(await response.arrayBuffer());
      } catch (error) {
        throw caller.transport.failure(error, scope, operation);
      }
      if (limit !== undefined && data.byteLength > limit) {
        throw sdkError("resource_exhausted", `the file is larger than the ${limit} bytes allowed`, { details: { reason: "too_large", limit } });
      }
      return { data, response };
    } finally {
      scope.release();
    }
  };

  return {
    upload: async (file, options) => {
      const form = uploadForm(file);
      const sessionId = options?.sessionId;
      if (sessionId !== undefined && (typeof sessionId !== "string" || !/^[A-Za-z0-9_.-]{1,128}$/.test(sessionId))) {
        throw invalid("sessionId", "it is not a valid identifier");
      }
      return caller.json("uploadFile", {}, isFileRef, "file", options, {
        // No Content-Type: fetch sets the multipart boundary itself.
        body: { payload: form },
        sessionId,
        timeoutMs: caller.requestTimeout === 0 ? 0 : Math.max(caller.requestTimeout, uploadTimeoutMs),
      });
    },
    get: async (fileId, options) => caller.json("getFile", { file_id: fileIdArgument(fileId) }, isFileRef, "file", options),
    delete: async (fileId, options) => caller.empty("deleteFile", { file_id: fileIdArgument(fileId) }, options),
    download: async (fileId, options) => {
      const { data, response } = await readBytes("downloadFile", fileIdArgument(fileId), options);
      return {
        data,
        contentType: response.headers.get("Content-Type") ?? "application/octet-stream",
        fileName: dispositionName(response.headers.get("Content-Disposition")),
        size: data.byteLength,
      };
    },
    preview: async (fileId, options) => {
      const { data, response } = await readBytes("previewFile", fileIdArgument(fileId), options);
      return {
        data,
        contentType: response.headers.get("Content-Type") ?? "application/octet-stream",
        truncated: response.headers.get("X-Alixai-Preview-Truncated") === "true",
      };
    },
  };
};

/** Checks an upload without sending it: throws the `validation_failed` error a malformed one would get. */
export const validateUpload = (file: unknown): void => {
  uploadForm(file);
};
