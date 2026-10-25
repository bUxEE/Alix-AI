import type { IncomingMessage, ServerResponse } from "node:http";
import type { ErrorCode } from "@alixai/sdk";

// Small helpers over Node's request and response. Errors are the typed error body of the public API (schemas/common/error.schema.json):
// `{code, message, retryable, details?}`, with `retryable` true exactly for the codes the contract says.

const RETRYABLE: ReadonlySet<ErrorCode> = new Set<ErrorCode>(["unavailable", "degraded", "timeout", "rate_limited", "resource_exhausted"]);

export interface ApiError {
  readonly code: ErrorCode;
  readonly message: string;
  readonly retryable: boolean;
  readonly details?: Record<string, unknown>;
}

export const apiError = (code: ErrorCode, message: string, details?: Record<string, unknown>): ApiError => ({
  code,
  message,
  retryable: RETRYABLE.has(code),
  ...(details !== undefined && { details }),
});

export const sendJson = (res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void => {
  const text = JSON.stringify(body);
  res.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Content-Length": String(Buffer.byteLength(text)),
    "Cache-Control": "no-store",
    ...headers,
  });
  res.end(text);
};

export const sendError = (res: ServerResponse, status: number, code: ErrorCode, message: string, details?: Record<string, unknown>, headers: Record<string, string> = {}): void =>
  sendJson(res, status, apiError(code, message, details), headers);

export const sendEmpty = (res: ServerResponse, status: number): void => {
  res.writeHead(status, { "Cache-Control": "no-store" });
  res.end();
};

export class BodyTooLargeError extends Error {
  constructor() {
    super("the request body is larger than this route accepts");
    this.name = "BodyTooLargeError";
  }
}

/** Reads the whole body, refusing one larger than `limit` bytes. */
export const readBody = (req: IncomingMessage, limit: number): Promise<Buffer> =>
  new Promise((resolve, reject) => {
    const declared = Number(req.headers["content-length"] ?? "NaN");
    if (Number.isFinite(declared) && declared > limit) {
      req.resume();
      reject(new BodyTooLargeError());
      return;
    }
    const parts: Buffer[] = [];
    let size = 0;
    let failed = false;
    req.on("data", (part: Buffer) => {
      if (failed) return;
      size += part.length;
      if (size > limit) {
        failed = true;
        reject(new BodyTooLargeError());
        return;
      }
      parts.push(part);
    });
    req.on("end", () => {
      if (!failed) resolve(Buffer.concat(parts));
    });
    req.on("error", (error) => {
      if (!failed) reject(error);
    });
  });

/** Reads a JSON body; undefined when it is not JSON. */
export const readJson = async (req: IncomingMessage, limit: number): Promise<unknown> => {
  const body = await readBody(req, limit);
  try {
    return JSON.parse(body.toString("utf8")) as unknown;
  } catch {
    return undefined;
  }
};

export const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

export const wait = (ms: number, signal?: AbortSignal): Promise<void> =>
  new Promise((resolve) => {
    if (ms <= 0 || signal?.aborted === true) {
      resolve();
      return;
    }
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        resolve();
      },
      { once: true },
    );
  });
