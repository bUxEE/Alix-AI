import { readSse } from "./sse";
import type { AguiEvent, RunAgentInput } from "./types";

// A small AG-UI client for AlixAI's `POST /v1/agui` (docs/integration/agui.md). It speaks the protocol directly: no CopilotKit,
// no @ag-ui/client, no telemetry, no third-party request.
//
// The browser never holds the confirmation secret. The run that prepares an action returns it in the response header
// `X-AlixAI-Confirmation-Token`, and the resume that approves it sends it back; both belong to the host's relay (the host
// backend that fronts the gateway, `@alixai/copilotkit-adapter` does the same for CopilotKit). This client therefore never reads
// that response header and drops it from the request headers a host callback returns.
//
// Context tokens are single use, so `headers` is asked for every request (run, reconnect, cancel), never cached.

export const CONFIRMATION_TOKEN_HEADER = "x-alixai-confirmation-token";

export type AguiOperation = "agui.run" | "agui.events" | "agui.cancel" | "agui.capabilities";

/** What the host's header callback is told about the request it is about to authenticate. */
export interface AguiRequestInfo {
  readonly operation: AguiOperation;
  readonly threadId?: string;
  readonly runId?: string;
}

export interface AguiClientOptions {
  /** Where the host serves the AG-UI route: `/v1/agui` of the gateway, or the host relay's own path. */
  endpoint: string;
  /** The fetch to use; defaults to the global one. */
  fetch?: typeof fetch;
  /** Headers for one request, typically `{ Authorization: "Bearer <fresh context token>" }`. Called per request. */
  headers?: (info: AguiRequestInfo) => Record<string, string> | Promise<Record<string, string>>;
  /** How many times a cut or refused-for-now stream is reconnected before the run fails. Default 3. */
  maxReconnects?: number;
  /**
   * Milliseconds to wait before reconnect number `attempt` (1-based). Default 300, doubling, at most 4000, plus up to a fifth at
   * random. A refusal's Retry-After that asks for longer wins, up to 60 seconds.
   */
  reconnectDelayMs?: (attempt: number) => number;
  /** For tests: replaces the wait between reconnects. */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
}

export interface RunHandlers {
  /** Called once per event, in order, exactly once, also across reconnects. `cursor` is the last id the stream has delivered. */
  onEvent: (event: AguiEvent, cursor: string | undefined) => void;
  signal?: AbortSignal;
  /** Called when the stream was cut and the client is about to reconnect (`attempt` counts from 1). */
  onReconnecting?: (attempt: number) => void;
}

export interface RunResult {
  /** True when RUN_FINISHED or RUN_ERROR arrived. */
  readonly terminal: boolean;
  /** True when the caller aborted. */
  readonly aborted: boolean;
  readonly reconnects: number;
}

/** A refusal by the gateway or the relay: the status and the typed error body when there was one. */
export class AguiHttpError extends Error {
  readonly status: number;
  readonly code: string | undefined;

  constructor(status: number, code: string | undefined, message: string) {
    super(message);
    this.name = "AguiHttpError";
    this.status = status;
    this.code = code;
  }

  /** True for a status worth reconnecting on: the gateway or a proxy in front of it is briefly unavailable or rate limited. */
  get retryable(): boolean {
    return this.status === 429 || this.status === 502 || this.status === 503 || this.status === 504 || (this.status === 409 && this.code === "conflict");
  }
}

export interface AguiClient {
  run: (input: RunAgentInput, handlers: RunHandlers) => Promise<RunResult>;
  /** Best effort: stops a run that is still in flight. Never throws. */
  cancel: (threadId: string, runId: string) => Promise<boolean>;
  capabilities: () => Promise<unknown>;
}

const TERMINAL = new Set(["RUN_FINISHED", "RUN_ERROR"]);

const defaultSleep = (ms: number, signal?: AbortSignal): Promise<void> =>
  new Promise((resolve) => {
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

// Up to a fifth more at random, so that clients cut at the same moment do not all come back together.
const defaultDelay = (attempt: number): number => {
  const base = Math.min(4000, 300 * 2 ** (attempt - 1));
  return base + Math.floor(base * 0.2 * Math.random());
};

/** The longest wait a Retry-After can ask for before the next attempt. */
const MAX_RETRY_AFTER_MS = 60_000;

/** Retry-After in milliseconds (seconds or an HTTP date), at most MAX_RETRY_AFTER_MS; undefined when absent or malformed. */
const retryAfterMs = (value: string | null): number | undefined => {
  if (value === null || value.trim() === "") return undefined;
  const seconds = Number(value);
  const ms = Number.isFinite(seconds) ? seconds * 1000 : Date.parse(value) - Date.now();
  return Number.isNaN(ms) ? undefined : Math.min(Math.max(ms, 0), MAX_RETRY_AFTER_MS);
};

const isAbort = (error: unknown, signal: AbortSignal | undefined): boolean =>
  signal?.aborted === true || (error instanceof DOMException && error.name === "AbortError");

const readError = async (response: Response): Promise<AguiHttpError> => {
  let code: string | undefined;
  let message = `the AlixAI endpoint answered ${response.status}`;
  try {
    const body: unknown = await response.json();
    const inner = typeof body === "object" && body !== null && "error" in body ? (body as { error: unknown }).error : body;
    if (typeof inner === "object" && inner !== null) {
      const record = inner as Record<string, unknown>;
      if (typeof record["code"] === "string") code = record["code"];
      if (typeof record["message"] === "string") message = record["message"];
    }
  } catch {
    // not JSON: the status is all there is
  }
  return new AguiHttpError(response.status, code, message);
};

const parseEvent = (data: string): AguiEvent | undefined => {
  try {
    const value: unknown = JSON.parse(data);
    if (typeof value === "object" && value !== null && !Array.isArray(value) && typeof (value as { type?: unknown }).type === "string") {
      return value as AguiEvent;
    }
  } catch {
    // a frame that is not JSON is not an event
  }
  return undefined;
};

export const createAguiClient = (options: AguiClientOptions): AguiClient => {
  const base = options.endpoint.replace(/\/+$/, "");
  const doFetch = (input: string, init: RequestInit): Promise<Response> => (options.fetch ?? globalThis.fetch)(input, init);
  const maxReconnects = options.maxReconnects ?? 3;
  const delay = options.reconnectDelayMs ?? defaultDelay;
  const sleep = options.sleep ?? defaultSleep;

  const headersFor = async (info: AguiRequestInfo, extra: Record<string, string> = {}): Promise<Record<string, string>> => {
    const supplied = (await options.headers?.(info)) ?? {};
    const out: Record<string, string> = {};
    for (const [name, value] of Object.entries(supplied)) {
      if (name.toLowerCase() !== CONFIRMATION_TOKEN_HEADER) out[name] = value;
    }
    return { ...out, ...extra };
  };

  const run = async (input: RunAgentInput, handlers: RunHandlers): Promise<RunResult> => {
    const { signal } = handlers;
    let cursor: string | undefined;
    let group: AguiEvent[] = [];
    let terminal = false;
    let reconnects = 0;

    const flush = (): void => {
      const events = group;
      group = [];
      for (const event of events) {
        handlers.onEvent(event, cursor);
        if (TERMINAL.has(event.type)) terminal = true;
      }
    };

    const open = async (): Promise<Response> => {
      const info = { threadId: input.threadId, runId: input.runId };
      if (cursor === undefined) {
        const headers = await headersFor({ operation: "agui.run", ...info }, { "Content-Type": "application/json", Accept: "text/event-stream" });
        return doFetch(base, { method: "POST", headers, body: JSON.stringify(input), signal: signal ?? null, cache: "no-store" });
      }
      const headers = await headersFor({ operation: "agui.events", ...info }, { Accept: "text/event-stream", "Last-Event-ID": cursor });
      const url = `${base}/threads/${encodeURIComponent(input.threadId)}/runs/${encodeURIComponent(input.runId)}/events`;
      return doFetch(url, { method: "GET", headers, signal: signal ?? null, cache: "no-store" });
    };

    for (;;) {
      // What the last refusal's Retry-After asked for: the next attempt waits at least that long.
      let retryAfter: number | undefined;
      try {
        const response = await open();
        if (response.status === 204) {
          flush();
          break;
        }
        if (!response.ok) {
          retryAfter = retryAfterMs(response.headers.get("Retry-After"));
          throw await readError(response);
        }
        if (response.body === null) throw new TypeError("the response has no body");
        await readSse(
          response.body,
          (frame) => {
            const event = parseEvent(frame.data);
            if (event === undefined) return; // not an event: the cursor stays on the last one that arrived
            group.push(event);
            if (frame.id !== undefined) {
              cursor = frame.id;
              flush();
            }
          },
          signal,
        );
        // The stream ended. Whatever is left belongs to a group whose id never came. The server sends no ids on a run it ended
        // before it had a turn, and that run ends in its terminal event, so a group with one is delivered. Any other leftover
        // is a TurnEvent cut in the middle: it is dropped and the reconnect replays it from its start.
        if (group.some((event) => TERMINAL.has(event.type))) flush();
        else group = [];
        if (terminal || signal?.aborted === true) break;
        throw new TypeError("the stream ended before the run did");
      } catch (error) {
        if (isAbort(error, signal)) return { terminal, aborted: true, reconnects };
        group = [];
        const retryable = error instanceof AguiHttpError ? error.retryable : true;
        if (!retryable || reconnects >= maxReconnects) throw error;
        reconnects += 1;
        handlers.onReconnecting?.(reconnects);
        await sleep(Math.max(delay(reconnects), retryAfter ?? 0), signal);
        if (signal?.aborted === true) return { terminal, aborted: true, reconnects };
      }
    }
    return { terminal, aborted: signal?.aborted === true, reconnects };
  };

  const cancel = async (threadId: string, runId: string): Promise<boolean> => {
    try {
      const headers = await headersFor({ operation: "agui.cancel", threadId, runId });
      const url = `${base}/threads/${encodeURIComponent(threadId)}/runs/${encodeURIComponent(runId)}/cancel`;
      const response = await doFetch(url, { method: "POST", headers, cache: "no-store" });
      return response.ok;
    } catch {
      return false;
    }
  };

  const capabilities = async (): Promise<unknown> => {
    const headers = await headersFor({ operation: "agui.capabilities" }, { Accept: "application/json" });
    const response = await doFetch(`${base}/capabilities`, { method: "GET", headers, cache: "no-store" });
    if (!response.ok) throw await readError(response);
    return response.json();
  };

  return { run, cancel, capabilities };
};
