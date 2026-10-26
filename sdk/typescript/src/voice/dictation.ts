import { timeout } from "../call.js";
import { errorFromResponse, isAlixAIError, sdkError } from "../errors.js";
import type { AlixAIError } from "../errors.js";
import type { DictationTranscript } from "../gen/voice/dictation-transcript.js";
import type { TranscriptionCapabilities } from "../gen/voice/transcription-capabilities.js";
import type { CallOptions } from "../transport.js";
import { contextToken, normalizeBaseUrl, pathId } from "../validate.js";
import { isDictationTranscript, isTranscriptionCapabilities } from "./dictation-guards.js";
import { DICTATION_OPERATIONS } from "./dictation-operations.js";
import type { DictationOperationId } from "./dictation-operations.js";

/** What the host's token provider is told about a dictation request. */
export interface DictationContextTokenRequest {
  operation: DictationOperationId;
  sessionId?: string;
}

export type DictationContextTokenProvider = (request: DictationContextTokenRequest) => string | Promise<string>;

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export interface DictationClientOptions {
  baseUrl: string;
  getContextToken: DictationContextTokenProvider;
  fetch?: FetchLike;
  timeoutMs?: number;
  allowInsecureHttp?: boolean;
}

export interface CreateDictationOptions extends CallOptions {
  /** Optional BCP 47 language hint for the recogniser. */
  language?: string;
}

/**
 * Record-to-text (1.1.0): capabilities and one-shot clip transcription. Not the voice WebSocket.
 * AlixAI never submits the transcript as a turn; the host places it in the chat input for editing.
 */
export interface DictationClient {
  /** Whether the host should show a record control, max clip length, and the fixed WAV shape. */
  capabilities: (options?: CallOptions) => Promise<TranscriptionCapabilities>;
  /**
   * Transcribes a WAV clip (PCM s16le mono 16 kHz). Pass a Blob, ArrayBuffer or Uint8Array.
   * Returns text for the host to insert into the composer; never auto-sends.
   */
  create: (sessionId: string, wav: Blob | ArrayBuffer | Uint8Array, options?: CreateDictationOptions) => Promise<DictationTranscript>;
}

const LANGUAGE = /^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$/;
const DEFAULT_TIMEOUT_MS = 30_000;

const invalid = (message: string, argument: string): AlixAIError =>
  sdkError("validation_failed", message, { details: { argument } });

const languageHint = (value: unknown): string | undefined => {
  if (value === undefined) return undefined;
  if (typeof value !== "string" || value.length > 35 || !LANGUAGE.test(value)) {
    throw invalid("the language is not a BCP 47 language tag", "language");
  }
  return value;
};

const toWavBytes = async (wav: Blob | ArrayBuffer | Uint8Array): Promise<Uint8Array> => {
  if (wav instanceof Uint8Array) return wav;
  if (wav instanceof ArrayBuffer) return new Uint8Array(wav);
  if (typeof Blob !== "undefined" && wav instanceof Blob) return new Uint8Array(await wav.arrayBuffer());
  throw invalid("audio must be a Blob, ArrayBuffer or Uint8Array of WAV bytes", "wav");
};

const callScope = (operation: DictationOperationId, signal: AbortSignal | undefined, timeoutMs: number, safe: boolean) => {
  const controller = new AbortController();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const onAbort = (): void => controller.abort(signal?.reason);
  if (signal !== undefined) {
    if (signal.aborted) controller.abort(signal.reason);
    else signal.addEventListener("abort", onAbort, { once: true });
  }
  if (timeoutMs > 0 && !controller.signal.aborted) {
    timer = setTimeout(() => {
      controller.abort(
        safe
          ? sdkError("timeout", `the request timed out after ${timeoutMs} ms`, { details: { operation, timeout_ms: timeoutMs } })
          : sdkError("uncertain_outcome", `the request timed out after ${timeoutMs} ms and may have been processed: read the state before sending it again`, {
              details: { operation, phase: "timeout", timeout_ms: timeoutMs },
            }),
      );
    }, timeoutMs);
    (timer as { unref?: () => void }).unref?.();
  }
  return {
    signal: controller.signal,
    release: (): void => {
      if (timer !== undefined) clearTimeout(timer);
      timer = undefined;
      signal?.removeEventListener("abort", onAbort);
    },
  };
};

/** Creates the dictation client for one AlixAI service. */
export const createDictationClient = (options: DictationClientOptions): DictationClient => {
  if (typeof options !== "object" || options === null) throw sdkError("validation_failed", "createDictationClient needs its options");
  const baseUrl = normalizeBaseUrl(options.baseUrl, options.allowInsecureHttp === true);
  if (typeof options.getContextToken !== "function") {
    throw sdkError("validation_failed", "getContextToken is required: the host supplies a fresh single-use token per request", {
      details: { argument: "getContextToken" },
    });
  }
  const requestTimeout = timeout(options.timeoutMs, DEFAULT_TIMEOUT_MS, "timeoutMs");
  const fetchImpl: FetchLike = (input, init) => (options.fetch !== undefined ? options.fetch(input, init) : globalThis.fetch(input, init));

  const send = async <T>(args: {
    operation: DictationOperationId;
    params?: Record<string, string>;
    query?: Record<string, string>;
    body?: { contentType: string; payload: Uint8Array | Blob };
    sessionId?: string;
    guard: (value: unknown) => value is T;
    what: string;
    call?: CallOptions;
  }): Promise<T> => {
    const spec = DICTATION_OPERATIONS[args.operation];
    const scope = callScope(args.operation, args.call?.signal, timeout(args.call?.timeoutMs, requestTimeout, "timeoutMs"), spec.safe);
    const failure = (error: unknown): unknown => {
      if (scope.signal.aborted) return scope.signal.reason;
      if (isAlixAIError(error)) return error;
      return spec.safe
        ? sdkError("unavailable", "the request could not be completed (network error)", { cause: error, details: { operation: args.operation, phase: "transport" } })
        : sdkError("uncertain_outcome", "the request may not have completed (network error): read the state before sending it again", {
            cause: error,
            details: { operation: args.operation, phase: "transport" },
          });
    };
    try {
      if (scope.signal.aborted) throw scope.signal.reason;
      let raw: unknown;
      try {
        raw = await options.getContextToken(
          args.sessionId === undefined ? { operation: args.operation } : { operation: args.operation, sessionId: args.sessionId },
        );
      } catch (cause) {
        if (scope.signal.aborted) throw scope.signal.reason;
        throw sdkError("unauthenticated", "getContextToken failed, so the request was not sent", {
          cause,
          details: { operation: args.operation, reason: "token_provider_failed" },
        });
      }
      const token = contextToken(raw);
      if (scope.signal.aborted) throw scope.signal.reason;

      let path: string = spec.path;
      for (const [key, value] of Object.entries(args.params ?? {})) {
        path = path.replace(`{${key}}`, encodeURIComponent(value));
      }
      const search = args.query !== undefined && Object.keys(args.query).length > 0 ? `?${new URLSearchParams(args.query).toString()}` : "";
      const headers: Record<string, string> = { Accept: "application/json", Authorization: `Bearer ${token}` };
      let body: BodyInit | undefined;
      if (args.body !== undefined) {
        headers["Content-Type"] = args.body.contentType;
        body = args.body.payload instanceof Uint8Array ? (args.body.payload as unknown as BodyInit) : args.body.payload;
      }
      let response: Response;
      try {
        response = await fetchImpl(`${baseUrl}${path}${search}`, {
          method: spec.method,
          headers,
          ...(body !== undefined && { body }),
          signal: scope.signal,
          redirect: "error",
          credentials: "omit",
          cache: "no-store",
        });
      } catch (error) {
        throw failure(error);
      }
      if (!response.ok) throw await errorFromResponse(response);
      let parsed: unknown;
      try {
        parsed = await response.json();
      } catch (error) {
        throw sdkError("internal", `the server's ${args.what} response is not JSON`, {
          cause: error,
          details: { operation: args.operation, reason: "unexpected_shape" },
        });
      }
      if (!args.guard(parsed)) {
        throw sdkError("internal", `the server's ${args.what} response has an unexpected shape`, {
          details: { operation: args.operation, reason: "unexpected_shape" },
        });
      }
      return parsed;
    } catch (error) {
      throw failure(error);
    } finally {
      scope.release();
    }
  };

  return {
    capabilities: (call) =>
      send({
        operation: "getDictationCapabilities",
        guard: isTranscriptionCapabilities,
        what: "dictation capabilities",
        call,
      }),
    create: async (sessionId, wav, call) => {
      const id = pathId("sessionId", sessionId);
      const language = languageHint(call?.language);
      const payload = await toWavBytes(wav);
      if (payload.byteLength < 44) throw invalid("the WAV body is too short to be a valid clip", "wav");
      return send({
        operation: "createSessionDictation",
        params: { session_id: id },
        sessionId: id,
        ...(language !== undefined && { query: { language } }),
        body: { contentType: "audio/wav", payload },
        guard: isDictationTranscript,
        what: "dictation transcript",
        call,
      });
    },
  };
};

/** Convenience namespace matching the routes: `dictations.capabilities()` / `dictations.create(...)`. */
export const dictations = {
  createClient: createDictationClient,
};
