import { createClient } from "../../src/index.js";
import type { AlixAIClient } from "../../src/index.js";
import { bytes } from "./host.js";

// A client over a hand-made fetch, for the responses a real service (or the fake) would never send:
// a truncated stream, a gap in the sequence, a proxy's HTML page.

export interface Seen {
  url: string;
  init: RequestInit;
}

export interface Stubbed {
  client: AlixAIClient;
  /** Every request the SDK made. */
  seen: Seen[];
  /** How many times the token provider was called. */
  tokenCalls: () => number;
}

export const stubbed = (respond: (url: string, init: RequestInit) => Response | Promise<Response>, baseUrl = "https://alixai.test"): Stubbed => {
  const seen: Seen[] = [];
  let tokens = 0;
  const client = createClient({
    baseUrl,
    getContextToken: () => `stub.token.${(tokens += 1)}`,
    fetch: async (url, init = {}) => {
      seen.push({ url, init });
      return respond(url, init);
    },
  });
  return { client, seen, tokenCalls: () => tokens };
};

export const json = (status: number, body: unknown, headers: Record<string, string> = {}): Response =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });

/**
 * A fetch that answers at once with a response head whose body arrives only when `finish` is called. Like a real fetch, an abort of
 * the request's signal errors the body.
 */
export const slowBody = (headers: Record<string, string>): { fetch: (url: string, init?: RequestInit) => Promise<Response>; finish: (text: string) => void } => {
  let controller: ReadableStreamDefaultController<Uint8Array> | undefined;
  const fetch = async (_url: string, init: RequestInit = {}): Promise<Response> => {
    const body = new ReadableStream<Uint8Array>({
      start: (c) => {
        controller = c;
      },
    });
    init.signal?.addEventListener("abort", () => controller?.error(init.signal?.reason), { once: true });
    return new Response(body, { status: 200, headers });
  };
  const finish = (text: string): void => {
    try {
      controller?.enqueue(bytes(text));
      controller?.close();
    } catch {
      // the body was already errored by an abort
    }
  };
  return { fetch, finish };
};

/** What a promise settled with, captured at once so that a rejection is never unhandled while a test advances timers. */
export const settled = <T>(promise: Promise<T>): Promise<{ value: T } | { error: unknown }> =>
  promise.then(
    (value) => ({ value }),
    (error: unknown) => ({ error }),
  );

/** An event stream response made of the given frames (already formatted) and extra headers. */
export const sse = (frames: string | string[], headers: Record<string, string> = {}): Response =>
  new Response(Array.isArray(frames) ? frames.join("") : frames, {
    status: 200,
    headers: { "Content-Type": "text/event-stream", ...headers },
  });

/** An SSE response whose body is a stream we can watch for cancellation. */
export const watchedSse = (frames: string[]): { response: Response; cancelled: () => boolean } => {
  let i = 0;
  let cancelled = false;
  const body = new ReadableStream<Uint8Array>({
    pull: (controller) => {
      const frame = frames[i++];
      if (frame === undefined) controller.close();
      else controller.enqueue(bytes(frame));
    },
    cancel: () => {
      cancelled = true;
    },
  });
  return { response: new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } }), cancelled: () => cancelled };
};

const TURN = "trn_01J0000000000000000000000A";

/** One TurnEvent as an SSE frame. */
export const frame = (seq: number, type: string, rest: Record<string, unknown> = {}, over: { id?: string; event?: string } = {}): string => {
  const event = { type, turn_id: TURN, seq, ...rest };
  return `id: ${over.id ?? seq}\nevent: ${over.event ?? type}\ndata: ${JSON.stringify(event)}\n\n`;
};

export const token = (seq: number, text = "hi"): string => frame(seq, "token", { text });
export const done = (seq: number, finish_reason = "completed", rest: Record<string, unknown> = {}): string =>
  frame(seq, "done", { finish_reason, ...rest });
