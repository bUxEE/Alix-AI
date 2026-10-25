import type { WebSocketFactory, WebSocketLike } from "../ws.js";

// The WebSocket side of the fake server, in process: a factory for the client's `webSocket` option (src/ws.ts) that goes through the
// fake's own request handling, so the context token is checked and redeemed exactly as for a fetch, and an upgrade that the fake
// refuses is an upgrade that fails to open. There is no network and no real socket: the fake answers an upgrade request with the events
// as a JSON document, and this module delivers them to the client as the messages and the close of a WebSocket, one per tick.
//
// The header `Upgrade: websocket` marks the request, and the response carries `X-Fake-WebSocket: 1`; any other answer (a typed error
// before the stream starts: 403, 400, 410, 429) is a refused upgrade, which a client sees as an error and an abnormal close (1006),
// as it would see it from a real server (the WebSocket API does not expose the status).

/** What a fake answer to an upgrade request contains. */
export interface FakeSocketScript {
  /** The messages to deliver, one text message each (the events as JSON). */
  messages: string[];
  /** How the server then closes the connection. */
  close: { code: number; reason: string };
}

export const FAKE_WEBSOCKET_HEADER = "X-Fake-WebSocket";

/** The request header the fake reads, besides `Upgrade`, to tell an upgrade from a plain read (browsers strip `Upgrade` from a Request). */
export const FAKE_UPGRADE_HEADER = "X-Fake-Upgrade";

type Listener = (event: never) => void;

export const createFakeWebSocketFactory = (request: (request: Request) => Promise<Response>): WebSocketFactory => {
  return (url, init) => {
    const listeners = new Map<string, Set<Listener>>();
    let readyState = 0; // CONNECTING
    const emit = (type: string, event: Record<string, unknown>): void => {
      for (const listener of [...(listeners.get(type) ?? [])]) listener(event as never);
    };
    const finish = (code: number, reason: string): void => {
      if (readyState === 3) return;
      readyState = 3;
      emit("close", { type: "close", code, reason, wasClean: code === 1000 });
    };
    const tick = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

    const socket: WebSocketLike = {
      get readyState() {
        return readyState;
      },
      addEventListener: (type, listener) => {
        (listeners.get(type) ?? listeners.set(type, new Set()).get(type)!).add(listener);
      },
      removeEventListener: (type, listener) => {
        listeners.get(type)?.delete(listener);
      },
      close: (code = 1000, reason = "") => {
        // A client close: the closing handshake completes on the next tick.
        if (readyState >= 2) return;
        readyState = 2;
        void tick().then(() => finish(code, reason));
      },
    };

    void (async () => {
      await tick();
      if (readyState !== 0) return;
      let response: Response;
      try {
        response = await request(
          new Request(url.replace(/^ws/, "http"), {
            // `Upgrade` is what a real client sends; a browser silently drops it from a Request, so the fake also reads its own header.
            headers: { ...init.headers, Upgrade: "websocket", [FAKE_UPGRADE_HEADER]: "websocket", "Sec-WebSocket-Protocol": init.protocols.join(", ") },
          }),
        );
      } catch {
        emit("error", { type: "error" });
        finish(1006, "");
        return;
      }
      if (response.status !== 200 || response.headers.get(FAKE_WEBSOCKET_HEADER) !== "1") {
        // A refused upgrade: the client learns only that it did not open.
        await response.body?.cancel().catch(() => undefined);
        emit("error", { type: "error" });
        finish(1006, "");
        return;
      }
      const script = (await response.json()) as FakeSocketScript;
      readyState = 1;
      emit("open", { type: "open" });
      for (const message of script.messages) {
        await tick();
        if (readyState !== 1) return;
        emit("message", { type: "message", data: message });
      }
      await tick();
      if (readyState === 1) finish(script.close.code, script.close.reason);
    })();

    return socket;
  };
};
