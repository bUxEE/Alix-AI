import type { ClientMessage, ServerEvent, SessionStarted, VoiceSocketFactory, VoiceSocketLike } from "./types.js";

// A fake voice server for a host's own tests (master plan 15.1: test fixtures and a fake AlixAI server): a socket factory for the
// `webSocket` option of `createVoiceSession` that never touches the network. The test plays the core: it sends events and audio with
// the connection's `serverSend` / `serverAudio` and reads what the client sent with `messages()` and `audio()`. Nothing here checks
// that the events make sense in sequence; `serverSend` does not validate, so a test can also send a malformed one.

type Listener = (event: never) => void;

/** One connection the client opened, seen from the server's side. */
export interface FakeVoiceConnection {
  readonly url: string;
  /** The headers the client opened it with: the token is in `Authorization`. */
  readonly headers: Record<string, string>;
  readonly protocols: string[];
  /** The JSON messages the client sent, parsed, in order. */
  messages: () => ClientMessage[];
  /** The binary messages the client sent (the audio), in order. */
  audio: () => Uint8Array[];
  /** Sends one event as a text message. A string is sent as it is. */
  serverSend: (event: ServerEvent | Record<string, unknown> | string) => void;
  /** Sends one binary message (a chunk of speech). */
  serverAudio: (bytes: Uint8Array) => void;
  /** Ends the connection from the server's side. */
  serverClose: (code: number, reason?: string) => void;
  /** Whether the client has closed it. */
  readonly closedByClient: boolean;
}

export interface FakeVoiceTransport {
  factory: VoiceSocketFactory;
  /** Every connection that was opened, oldest first. */
  connections: FakeVoiceConnection[];
  /** The newest connection. */
  last: () => FakeVoiceConnection;
}

export interface FakeVoiceOptions {
  /** Send `session.started` as soon as a connection opens. Default true. */
  autoStart?: boolean;
  /** Overrides the members of that `session.started`. */
  session?: Partial<Omit<SessionStarted, "type">>;
  /** Refuse the Nth (1-based) connection attempt, as a refused upgrade does: the socket closes without ever opening. */
  refuse?: (attempt: number) => boolean;
}

/** An id of the shape the schema asks for, from a number: `fakeVoiceId("utt", 1)`. */
export const fakeVoiceId = (prefix: "utt" | "pbk" | "vss", n: number): string => `${prefix}_${String(n).padStart(26, "0")}`;

export const createFakeVoiceTransport = (options: FakeVoiceOptions = {}): FakeVoiceTransport => {
  const connections: FakeVoiceConnection[] = [];
  let attempts = 0;

  const factory: VoiceSocketFactory = (url, init) => {
    attempts += 1;
    const attempt = attempts;
    const listeners = new Map<string, Set<Listener>>();
    const sent: (string | Uint8Array)[] = [];
    let readyState = 0;
    let closedByClient = false;
    const emit = (type: string, event: Record<string, unknown>): void => {
      for (const listener of [...(listeners.get(type) ?? [])]) listener(event as never);
    };
    const finish = (code: number, reason: string): void => {
      if (readyState === 3) return;
      readyState = 3;
      emit("close", { type: "close", code, reason, wasClean: code === 1000 });
    };

    const socket: VoiceSocketLike = {
      get readyState() {
        return readyState;
      },
      binaryType: "blob",
      addEventListener: (type, listener) => {
        (listeners.get(type) ?? listeners.set(type, new Set()).get(type)!).add(listener);
      },
      removeEventListener: (type, listener) => {
        listeners.get(type)?.delete(listener);
      },
      send: (data) => {
        if (readyState !== 1) throw new Error("the fake socket is not open");
        if (typeof data === "string") sent.push(data);
        else if (ArrayBuffer.isView(data)) sent.push(new Uint8Array(data.buffer.slice(data.byteOffset, data.byteOffset + data.byteLength)));
        else sent.push(new Uint8Array(data.slice(0)));
      },
      close: (code = 1000, reason = "") => {
        if (readyState >= 2) return;
        closedByClient = true;
        readyState = 2;
        setTimeout(() => finish(code, reason), 0);
      },
    };

    const connection: FakeVoiceConnection = {
      url,
      headers: init.headers,
      protocols: init.protocols,
      messages: () => sent.filter((m): m is string => typeof m === "string").map((m) => JSON.parse(m) as ClientMessage),
      audio: () => sent.filter((m): m is Uint8Array => typeof m !== "string"),
      serverSend: (event) => {
        if (readyState !== 1) return;
        emit("message", { type: "message", data: typeof event === "string" ? event : JSON.stringify(event) });
      },
      serverAudio: (bytes) => {
        if (readyState !== 1) return;
        emit("message", { type: "message", data: bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) });
      },
      serverClose: (code, reason = "") => finish(code, reason),
      get closedByClient() {
        return closedByClient;
      },
    };
    connections.push(connection);

    setTimeout(() => {
      if (readyState !== 0) return;
      if (options.refuse?.(attempt) === true) {
        emit("error", { type: "error" });
        finish(1006, "");
        return;
      }
      readyState = 1;
      emit("open", { type: "open" });
      // A real socket delivers its first message in a task of its own, after the client has had its turn on `open`.
      if (options.autoStart !== false) setTimeout(() => {
        connection.serverSend({
          type: "session.started",
          voice_session_id: fakeVoiceId("vss", attempt),
          session_id: "ses_fake",
          speech: true,
          retain_raw_audio: false,
          max_chunk_bytes: 32768,
          max_audio_bytes: 8 * 1024 * 1024,
          ...options.session,
        });
      }, 0);
    }, 0);

    return socket;
  };

  return {
    factory,
    connections,
    last: () => {
      const newest = connections[connections.length - 1];
      if (newest === undefined) throw new Error("no connection was opened");
      return newest;
    },
  };
};
