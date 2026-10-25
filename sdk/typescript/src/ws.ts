import { sdkError } from "./errors.js";
import type { SseMessage, SseReader } from "./sse.js";

// The WebSocket transport of the event stream of a turn (GET .../turns/{turn_id}/events with `Upgrade: websocket`), server to client
// only: one text message per TurnEvent, the subprotocol `alixai.turn-events.v1`.
//
// The gateway authenticates with the `Authorization` header and nothing else (a credential is never put in a URL), and a browser's
// WebSocket cannot set a header. So the SDK never opens a socket by itself: the host passes a `webSocket` factory that does, which
// is one line on a runtime whose WebSocket takes headers (Node's global one, Deno, Bun, the `ws` package):
//
//   webSocket: (url, { protocols, headers }) => new WebSocket(url, { protocols, headers })     // Node 22+, Deno, Bun
//   webSocket: (url, { protocols, headers }) => new WsWebSocket(url, protocols, { headers })   // the `ws` package
//
// Where there is no such factory (a browser) the stream is read over server-sent events with `fetch`, which can set the header, and
// the host sees no difference: both are a `TurnStream`.

/** The subprotocol the gateway offers on the event stream. */
export const TURN_EVENTS_SUBPROTOCOL = "alixai.turn-events.v1";

/** The `readyState` of a socket while it is still connecting. */
const CONNECTING = 0;
const OPEN = 1;

/** The part of the standard WebSocket interface the SDK uses. Node's, the browser's, Deno's and Bun's all satisfy it. */
export interface WebSocketLike {
  readonly readyState: number;
  addEventListener: (type: string, listener: (event: never) => void) => void;
  removeEventListener: (type: string, listener: (event: never) => void) => void;
  close: (code?: number, reason?: string) => void;
}

/** What the SDK asks a factory to open: the URL, the subprotocol, and the headers (the context token is in them). */
export interface WebSocketInit {
  protocols: string[];
  headers: Record<string, string>;
}

/** Opens a client WebSocket with the given headers. See the note at the top of this file for the one-line implementations. */
export type WebSocketFactory = (url: string, init: WebSocketInit) => WebSocketLike;

/** Which transport to read an event stream over. `auto` tries a WebSocket when the host supplied a factory, then falls back to SSE. */
export type StreamTransport = "auto" | "sse" | "websocket";

/** The WebSocket URL of an http(s) URL. */
export const webSocketUrl = (httpUrl: string): string => httpUrl.replace(/^http/, "ws");

interface CloseLike {
  code?: number;
  reason?: string;
}

interface MessageLike {
  data?: unknown;
}

/** The longest queue of unread messages before the reader gives up: the server never gets this far ahead (its own lag limit is smaller). */
const MAX_QUEUED_MESSAGES = 4096;

/** The error for a socket that closed (or failed) before it opened. The caller decides whether to fall back. */
export const handshakeError = (cause: unknown) =>
  sdkError("unavailable", "the WebSocket could not be opened", { cause, details: { phase: "websocket_handshake", resumable: true } });

/**
 * Opens a socket through the factory and waits until it is open. A failure before that (a refused upgrade, a network error, a
 * factory that throws, as it does in a browser) is a `handshakeError`, and so is an abort of the call (reported by the caller).
 */
export const openSocket = (
  factory: WebSocketFactory,
  url: string,
  init: WebSocketInit,
  signal: AbortSignal,
): Promise<WebSocketLike> =>
  new Promise<WebSocketLike>((resolve, reject) => {
    let socket: WebSocketLike;
    try {
      socket = factory(url, init);
    } catch (cause) {
      reject(handshakeError(cause));
      return;
    }
    const cleanup = (): void => {
      socket.removeEventListener("open", onOpen);
      socket.removeEventListener("error", onFail);
      socket.removeEventListener("close", onFail);
      signal.removeEventListener("abort", onAbort);
    };
    const onOpen = (): void => {
      cleanup();
      resolve(socket);
    };
    const onFail = (): void => {
      cleanup();
      reject(handshakeError(undefined));
    };
    const onAbort = (): void => {
      cleanup();
      try {
        socket.close(1000, "aborted");
      } catch {
        // nothing is left to release
      }
      reject(signal.reason);
    };
    if (signal.aborted) {
      onAbort();
      return;
    }
    if (socket.readyState === OPEN) {
      onOpen();
      return;
    }
    if (socket.readyState !== CONNECTING) {
      onFail();
      return;
    }
    socket.addEventListener("open", onOpen);
    socket.addEventListener("error", onFail);
    socket.addEventListener("close", onFail);
    signal.addEventListener("abort", onAbort, { once: true });
  });

/**
 * Reads an open event-stream socket as a sequence of frames, the shape the SSE reader has, so that the rest of the SDK does not know
 * which transport it is reading. The frame is rebuilt from the message: the event name is the event's `type` and the id its `seq`
 * (what the server's SSE frames carry), so the same sequence checks apply.
 *
 * How a close is read: 1000 is the clean end (after `done`; a 1000 without it is resumed by the caller like any lost connection);
 * 1013 (slow consumer) and an abnormal closure (1006, 1001) are a lost connection to resume from the cursor; 1011 (the turn was
 * abandoned) and 1008 (a protocol violation, since the client never sends) end the stream with a typed error that is not resumed.
 */
export const createSocketReader = (socket: WebSocketLike): SseReader => {
  const queue: SseMessage[] = [];
  let waiter: { resolve: () => void } | undefined;
  let end: { error: unknown } | { clean: true } | undefined;
  let cancelled = false;

  const wake = (): void => {
    const w = waiter;
    waiter = undefined;
    w?.resolve();
  };

  const finish = (outcome: NonNullable<typeof end>): void => {
    if (end === undefined) end = outcome;
    wake();
  };

  const onMessage = (event: MessageLike): void => {
    if (typeof event.data !== "string") {
      finish({ error: sdkError("internal", "the event stream sent a message that is not text", { details: { reason: "malformed_event" } }) });
      socket.close(1008, "text only");
      return;
    }
    if (queue.length >= MAX_QUEUED_MESSAGES) {
      finish({ error: sdkError("internal", "the event stream is not being read: too many unread messages", { details: { reason: "slow_reader" } }) });
      socket.close(1008, "unread");
      return;
    }
    let type = "";
    let id: string | undefined;
    try {
      const parsed: unknown = JSON.parse(event.data);
      if (typeof parsed === "object" && parsed !== null) {
        const record = parsed as { type?: unknown; seq?: unknown };
        if (typeof record.type === "string") type = record.type;
        if (typeof record.seq === "number") id = String(record.seq);
      }
    } catch {
      // the consumer reports the JSON error with the same words as for an SSE frame
    }
    queue.push({ event: type, data: event.data, id });
    wake();
  };

  const onClose = (event: CloseLike): void => {
    const code = event.code ?? 1006;
    if (code === 1000) finish({ clean: true });
    else if (code === 1011) {
      finish({ error: sdkError("unavailable", "the turn was abandoned by the server", { details: { reason: "aborted", code }, retryable: false }) });
    } else if (code === 1008) {
      finish({ error: sdkError("internal", "the server closed the event stream for a protocol violation", { details: { reason: "protocol", code } }) });
    } else {
      finish({
        error: sdkError("unavailable", code === 1013 ? "the connection was too slow and was closed: resume from the cursor" : "the connection was lost", {
          details: { reason: code === 1013 ? "slow_consumer" : "socket_closed", code, resumable: true },
        }),
      });
    }
  };

  const onError = (): void => {
    // The close event follows with the code and says more; an error that no close follows is a lost connection all the same.
    setTimeout(() => {
      finish({ error: sdkError("unavailable", "the connection was lost", { details: { reason: "socket_error", resumable: true } }) });
    }, 0);
  };

  socket.addEventListener("message", onMessage as (event: never) => void);
  socket.addEventListener("close", onClose as (event: never) => void);
  socket.addEventListener("error", onError);

  const next: SseReader["next"] = async () => {
    for (;;) {
      const message = queue.shift();
      if (message !== undefined) return message;
      if (cancelled) return undefined;
      if (end !== undefined) {
        if ("clean" in end) return undefined;
        throw end.error;
      }
      await new Promise<void>((resolve) => {
        waiter = { resolve };
      });
    }
  };

  const cancel: SseReader["cancel"] = async () => {
    if (cancelled) return;
    cancelled = true;
    queue.length = 0;
    try {
      socket.close(1000, "done");
    } catch {
      // already closed
    }
    wake();
  };

  return { next, cancel };
};
