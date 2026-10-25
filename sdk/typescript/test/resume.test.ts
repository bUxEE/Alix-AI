import { describe, expect, it, vi } from "vitest";

import { AlixAIError, createClient, isAlixAIError, turnText } from "../src/index.js";
import type { TurnEvent } from "../src/index.js";
import { prepareAction, reply } from "../src/testing/index.js";
import { collect, createHost } from "./helpers/host.js";

// Reading a turn again from a cursor (GET .../turns/{turn_id}/events): the SDK's fetch-based reader with Last-Event-ID/cursor resume,
// the WebSocket transport beside it with automatic fallback, and the automatic reconnect of a stream that lost its connection.

const BOOK = { capability_id: "demo.book", summary: "Book the 10:00 slot", text: "Shall I book it?" };

const caught = async (promise: Promise<unknown>): Promise<AlixAIError> => {
  try {
    await promise;
  } catch (error) {
    expect(isAlixAIError(error), `not an AlixAIError: ${String(error)}`).toBe(true);
    return error as AlixAIError;
  }
  throw new Error("expected a failure");
};

/** A turn with a few events, run through the JSON form so that its id is known. */
const firstTurn = async (host: ReturnType<typeof createHost>, text = "hello") => {
  const turn = await host.client.sendTurnJson("ses-1", { text });
  return { turn, last: turn.events.length - 1 };
};

describe("streamTurnEvents over server-sent events", () => {
  it("reads the whole turn again, byte for byte the events the turn sent", async () => {
    const host = createHost();
    const { turn } = await firstTurn(host);
    const stream = host.client.streamTurnEvents("ses-1", turn.turn_id);
    expect(stream.turnId).toBe(turn.turn_id); // known before the first event
    expect(await collect(stream)).toEqual(turn.events);
    expect(stream.transport).toBe("sse");
    expect(stream.cursor).toBe(turn.events.length - 1);
  });

  it("continues after the cursor, each event exactly once and in order", async () => {
    const host = createHost();
    const { turn } = await firstTurn(host);
    const stream = host.client.streamTurnEvents("ses-1", turn.turn_id, { cursor: 1 });
    expect(stream.cursor).toBe(1);
    const events = await collect(stream);
    expect(events).toEqual(turn.events.slice(2));
    expect(events[0]?.seq).toBe(2);
  });

  it("a cursor at the end of a finished turn is an empty stream, not an error (the server's 204)", async () => {
    const host = createHost();
    const { turn, last } = await firstTurn(host);
    const stream = host.client.streamTurnEvents("ses-1", turn.turn_id, { cursor: last });
    expect(await stream.result()).toEqual({ turn_id: turn.turn_id, events: [] });
    expect(host.fake.requests.at(-1)?.status).toBe(204);
  });

  it("sends the cursor as the cursor parameter and asks for an event stream with a fresh token per open", async () => {
    const host = createHost();
    const { turn } = await firstTurn(host);
    host.tokenRequests.length = 0;
    await host.client.streamTurnEvents("ses-1", turn.turn_id, { cursor: 0 }).result();
    await host.client.streamTurnEvents("ses-1", turn.turn_id, { cursor: 0 }).result();
    expect(host.tokenRequests).toEqual([
      { operation: "streamTurnEvents", sessionId: "ses-1", turnId: turn.turn_id },
      { operation: "streamTurnEvents", sessionId: "ses-1", turnId: turn.turn_id },
    ]);
    const reads = host.fake.requests.filter((r) => r.operation === "streamTurnEvents");
    expect(reads.map((r) => [r.path.split("?")[0], r.accept])).toEqual(reads.map(() => [`/v1/sessions/ses-1/turns/${turn.turn_id}/events`, "text/event-stream"]));
  });

  it("the server also takes the cursor as Last-Event-ID, and the later of two wins", async () => {
    const host = createHost();
    const { turn } = await firstTurn(host);
    const get = async (query: string, header?: string): Promise<string> => {
      const response = await host.fake.fetch(`${host.fake.baseUrl}/v1/sessions/ses-1/turns/${turn.turn_id}/events${query}`, {
        headers: { Authorization: `Bearer ${host.fake.issueToken({ session_id: "ses-1" })}`, Accept: "text/event-stream", ...(header !== undefined && { "Last-Event-ID": header }) },
      });
      return (await response.text()).split("\n").filter((l) => l.startsWith("id: ")).join(",");
    };
    expect(await get("", "1")).toBe("id: 2,id: 3");
    expect(await get("?cursor=0", "2")).toBe("id: 3");
    expect(await get("?cursor=2", "0")).toBe("id: 3");
  });

  it("refuses with a typed error: a cursor ahead (400), a turn that is not visible (403, no oracle), a cursor behind the window (410)", async () => {
    const host = createHost({}, { replayEvents: 1 });
    const { turn, last } = await firstTurn(host);
    expect(await caught(host.client.streamTurnEvents("ses-1", turn.turn_id, { cursor: last + 3 }).result())).toMatchObject({
      code: "validation_failed",
      status: 400,
      details: { reason: "cursor_ahead" },
    });
    const unknown = await caught(host.client.streamTurnEvents("ses-1", "trn_unknown").result());
    expect(unknown).toMatchObject({ code: "scope_violation", status: 403, details: { reason: "turn_not_visible" } });
    const expired = await caught(host.client.streamTurnEvents("ses-1", turn.turn_id, { cursor: 0 }).result());
    expect(expired).toMatchObject({ code: "expired", status: 410, retryable: false, details: { reason: "cursor_expired" } });
  });

  it("a turn of another principal is the same 403 as one that does not exist", async () => {
    const host = createHost();
    const { turn } = await firstTurn(host);
    const intruder = createClient({
      baseUrl: host.fake.baseUrl,
      fetch: host.fake.fetch,
      getContextToken: () => host.fake.issueToken({ session_id: "ses-1", principal_id: "someone-else" }),
    });
    const error = await caught(intruder.streamTurnEvents("ses-1", turn.turn_id).result());
    expect(error).toMatchObject({ code: "scope_violation", details: { reason: "turn_not_visible" } });
  });

  it("a turn past its retention is not visible any more", async () => {
    const host = createHost({}, { turnRetentionMs: 60_000 });
    const { turn } = await firstTurn(host);
    host.fake.advanceTime(61_000);
    expect(await caught(host.client.streamTurnEvents("ses-1", turn.turn_id).result())).toMatchObject({ details: { reason: "turn_not_visible" } });
  });

  it("checks its arguments before a token is spent", async () => {
    const host = createHost();
    for (const bad of [-1, 1.5, "1", Number.NaN, 10 ** 11]) {
      const error = await caught(host.client.streamTurnEvents("ses-1", "trn_x", { cursor: bad as number }).result());
      expect(error.code, String(bad)).toBe("validation_failed");
    }
    expect((await caught(host.client.streamTurnEvents("ses-1", "../x").result())).code).toBe("validation_failed");
    expect((await caught(host.client.streamTurnEvents("ses-1", "trn_x", { transport: "carrier-pigeon" as never }).result())).code).toBe("validation_failed");
    expect(host.tokenRequests).toHaveLength(0);
  });

  it("never carries the confirmation secret: a resume is a read", async () => {
    const host = createHost();
    host.fake.onTurn(() => prepareAction(BOOK));
    const turn = await host.client.sendTurnJson("ses-1", { text: "book" });
    expect(turn.confirmation).toBeDefined();
    const again = await host.client.streamTurnEvents("ses-1", turn.turn_id).result();
    expect(again.confirmation).toBeUndefined();
    expect(JSON.stringify(again.events)).not.toContain(turn.confirmation!.token);
  });
});

describe("a stream that loses its connection", () => {
  it("is uncertain_outcome by default, with the events before it delivered (nothing is retried quietly)", async () => {
    const host = createHost();
    host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 2 });
    const stream = host.client.sendTurn("ses-1", { text: "hello" });
    const seen: TurnEvent[] = [];
    const failure = await caught(
      (async () => {
        for await (const event of stream) seen.push(event);
      })(),
    );
    expect(failure.code).toBe("uncertain_outcome");
    expect(seen).toHaveLength(2);
    expect(stream.cursor).toBe(1); // what a host needs to resume by hand
    expect(stream.turnId).toBeDefined();
  });

  it("resumes from the cursor with resume: true, over a fresh token, and the turn is not run again", async () => {
    const host = createHost();
    host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 2 });
    const stream = host.client.sendTurn("ses-1", { text: "hello" }, { resume: { backoffMs: 0 } });
    const events = await collect(stream);
    expect(events.map((e) => e.seq)).toEqual(events.map((_, i) => i));
    expect(events.at(-1)?.type).toBe("done");
    expect(turnText(events)).toBe("fake: hello");
    const ops = host.fake.requests.map((r) => r.operation);
    expect(ops).toEqual(["sendTurn", "streamTurnEvents"]); // a read, not a second turn
    expect(host.fake.session("ses-1")?.turn_count).toBe(1);
    expect(host.tokenRequests.map((t) => t.operation)).toEqual(["sendTurn", "streamTurnEvents"]);
    expect(host.fake.tokensRedeemed).toBe(2);
  });

  it("keeps the confirmation secret of the original response across the resume", async () => {
    const host = createHost();
    host.fake.onTurn(() => prepareAction(BOOK));
    host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 3 });
    const result = await host.client.sendTurn("ses-1", { text: "book" }, { resume: { backoffMs: 0 } }).result();
    expect(result.confirmation).toMatchObject({ preparation_id: expect.stringMatching(/^prep_/) });
    const confirmed = await host.client.confirmAction(result.confirmation!.preparation_id, result.confirmation!.token);
    expect(confirmed.receipt.status).toBe("committed");
  });

  it("can be switched on for the whole client", async () => {
    const host = createHost({}, {}, { resume: { backoffMs: 0 } });
    host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 1 });
    expect((await host.client.sendTurn("ses-1", { text: "x" }).result()).events.at(-1)?.type).toBe("done");
    host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 1 });
    // ... and off for one call
    expect((await caught(host.client.sendTurn("ses-1", { text: "x" }, { resume: false }).result())).code).toBe("uncertain_outcome");
  });

  it("gives up after maxAttempts connections lost in a row, as uncertain_outcome", async () => {
    const host = createHost();
    host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 2 });
    host.fake.failNext({ operation: "streamTurnEvents", truncateAfterEvents: 0, times: 5 });
    const failure = await caught(host.client.sendTurn("ses-1", { text: "x" }, { resume: { maxAttempts: 2, backoffMs: 0 } }).result());
    expect(failure.code).toBe("uncertain_outcome");
    expect(host.fake.requests.filter((r) => r.operation === "streamTurnEvents")).toHaveLength(2);
  });

  it("makes progress count: a long stream may lose its connection many times if every connection delivers something", async () => {
    const host = createHost();
    host.fake.onTurn(() => reply("x".repeat(4096 * 3))); // 3 token events plus the envelope
    host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 2 });
    host.fake.failNext({ operation: "streamTurnEvents", truncateAfterEvents: 1 });
    host.fake.failNext({ operation: "streamTurnEvents", truncateAfterEvents: 1 });
    const events = await collect(host.client.sendTurn("ses-1", { text: "x" }, { resume: { maxAttempts: 1, backoffMs: 0 } }));
    expect(events.at(-1)?.type).toBe("done");
    expect(events.map((e) => e.seq)).toEqual(events.map((_, i) => i));
  });

  it("waits as long as the server's Retry-After asks before connecting again, up to 60 seconds", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    vi.spyOn(Math, "random").mockReturnValue(0);
    try {
      for (const [retryAfter, wait] of [
        ["30", 30_000],
        ["3600", 60_000],
      ] as const) {
        const host = createHost();
        host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 2 });
        host.fake.failNext({ operation: "streamTurnEvents", status: 503, error: { code: "unavailable", message: "busy", retryable: true }, headers: { "Retry-After": retryAfter } });
        const result = host.client.sendTurn("ses-1", { text: "x" }, { resume: { backoffMs: 0 } }).result();
        const reads = (): number => host.fake.requests.filter((r) => r.operation === "streamTurnEvents").length;
        await vi.advanceTimersByTimeAsync(0);
        expect(reads(), `Retry-After ${retryAfter}`).toBe(1);
        await vi.advanceTimersByTimeAsync(wait - 1);
        expect(reads(), `Retry-After ${retryAfter}`).toBe(1); // a 5-second cap spent every attempt here
        await vi.advanceTimersByTimeAsync(1);
        expect((await result).events.at(-1)?.type).toBe("done");
        expect(reads()).toBe(2);
      }
    } finally {
      vi.useRealTimers();
      vi.restoreAllMocks();
    }
  });

  it("spreads the reconnects of many clients with a little jitter on the backoff", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
    vi.spyOn(Math, "random").mockReturnValue(0.99);
    try {
      const host = createHost();
      host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 2 });
      const result = host.client.sendTurn("ses-1", { text: "x" }, { resume: { backoffMs: 1000 } }).result();
      const reads = (): number => host.fake.requests.filter((r) => r.operation === "streamTurnEvents").length;
      await vi.advanceTimersByTimeAsync(1000);
      expect(reads()).toBe(0);
      await vi.advanceTimersByTimeAsync(200);
      expect((await result).events.at(-1)?.type).toBe("done");
      expect(reads()).toBe(1);
    } finally {
      vi.useRealTimers();
      vi.restoreAllMocks();
    }
  });

  it("does not resume a refusal that will not change: the cursor expired", async () => {
    const host = createHost({}, { replayEvents: 1 });
    host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 2 });
    const seen: TurnEvent[] = [];
    const failure = await caught(
      (async () => {
        for await (const event of host.client.sendTurn("ses-1", { text: "x" }, { resume: { backoffMs: 0 } })) seen.push(event);
      })(),
    );
    expect(failure).toMatchObject({ code: "expired", details: { reason: "cursor_expired" } });
    expect(seen).toHaveLength(2);
    expect(host.fake.requests.filter((r) => r.operation === "streamTurnEvents")).toHaveLength(1);
  });

  it("does not resume an abort", async () => {
    const host = createHost();
    const controller = new AbortController();
    const stream = host.client.streamTurnEvents("ses-1", "trn_x", { signal: controller.signal, resume: { backoffMs: 0 } });
    controller.abort(new DOMException("stop", "AbortError"));
    await expect(stream.result()).rejects.toMatchObject({ name: "AbortError" });
  });

  it("a stream that lost its connection before its first event cannot be resumed: the turn is unknown", async () => {
    const host = createHost();
    host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 0 });
    const failure = await caught(host.client.sendTurn("ses-1", { text: "x" }, { resume: { backoffMs: 0 } }).result());
    expect(failure.code).toBe("uncertain_outcome");
    expect(host.fake.requests.filter((r) => r.operation === "streamTurnEvents")).toHaveLength(0);
  });
});

describe("the WebSocket transport", () => {
  it("is not used unless the host supplies a factory (the gateway wants the Authorization header, which a browser WebSocket cannot send)", async () => {
    const host = createHost();
    const { turn } = await firstTurn(host);
    const stream = host.client.streamTurnEvents("ses-1", turn.turn_id);
    await stream.result();
    expect(stream.transport).toBe("sse");
    expect((await caught(host.client.streamTurnEvents("ses-1", turn.turn_id, { transport: "websocket" }).result())).code).toBe("validation_failed");
  });

  it("is preferred with a factory, and delivers the same events as SSE", async () => {
    const host = createHost({}, {}, { webSocket: true });
    const { turn } = await firstTurn(host);
    const sse = await host.client.streamTurnEvents("ses-1", turn.turn_id, { transport: "sse" }).result();
    const stream = host.client.streamTurnEvents("ses-1", turn.turn_id);
    const viaSocket = await collect(stream);
    expect(stream.transport).toBe("websocket");
    expect(viaSocket).toEqual(sse.events);
    expect(viaSocket).toEqual(turn.events);
  });

  it("resumes from a cursor, and a cursor at the end is an empty stream", async () => {
    const host = createHost({}, {}, { webSocket: true });
    const { turn, last } = await firstTurn(host);
    expect(await collect(host.client.streamTurnEvents("ses-1", turn.turn_id, { cursor: 1, transport: "websocket" }))).toEqual(turn.events.slice(2));
    expect(await collect(host.client.streamTurnEvents("ses-1", turn.turn_id, { cursor: last, transport: "websocket" }))).toEqual([]);
  });

  it("authenticates in a header and tells the token provider what the socket is for (the token is never in the URL)", async () => {
    const host = createHost({}, {}, { webSocket: true });
    const { turn } = await firstTurn(host);
    const urls: string[] = [];
    const headers: Array<Record<string, string>> = [];
    const client = createClient({
      baseUrl: host.fake.baseUrl,
      fetch: host.fake.fetch,
      webSocket: (url, init) => {
        urls.push(url);
        headers.push(init.headers);
        expect(init.protocols).toEqual(["alixai.turn-events.v1"]);
        return host.fake.webSocket(url, init);
      },
      getContextToken: (request) => {
        expect(request).toMatchObject({ operation: "streamTurnEvents", sessionId: "ses-1", turnId: turn.turn_id });
        return host.fake.issueToken({ session_id: "ses-1" });
      },
    });
    await client.streamTurnEvents("ses-1", turn.turn_id, { cursor: 0 }).result();
    expect(urls).toEqual([`${host.fake.baseUrl.replace(/^http/, "ws")}/v1/sessions/ses-1/turns/${turn.turn_id}/events?cursor=0`]);
    expect(headers[0]?.["Authorization"]).toMatch(/^Bearer [A-Za-z0-9._-]+$/);
    expect(urls[0]).not.toMatch(/token|Bearer/i);
  });

  it("falls back to SSE with a FRESH token when the upgrade cannot be opened, and the host sees the same stream", async () => {
    const host = createHost({}, {}, { webSocket: true });
    const { turn } = await firstTurn(host);
    host.fake.failNext({ operation: "streamTurnEvents", refuseWebSocket: true });
    host.tokenRequests.length = 0;
    const redeemed = host.fake.tokensRedeemed;
    const stream = host.client.streamTurnEvents("ses-1", turn.turn_id);
    expect(await collect(stream)).toEqual(turn.events);
    expect(stream.transport).toBe("sse");
    expect(host.tokenRequests).toHaveLength(2); // the refused upgrade used one up
    expect(host.fake.tokensRedeemed - redeemed).toBe(2);
  });

  it("falls back to report a typed refusal properly: the WebSocket API does not expose the status of a refused upgrade", async () => {
    const host = createHost({}, {}, { webSocket: true });
    const error = await caught(host.client.streamTurnEvents("ses-1", "trn_unknown").result());
    expect(error).toMatchObject({ code: "scope_violation", status: 403, details: { reason: "turn_not_visible" } });
    expect(host.tokenRequests).toHaveLength(2);
  });

  it("does not fall back when the host asked for a WebSocket: the failure to open it is the answer", async () => {
    const host = createHost({}, {}, { webSocket: true });
    const { turn } = await firstTurn(host);
    host.fake.failNext({ operation: "streamTurnEvents", refuseWebSocket: true });
    const error = await caught(host.client.streamTurnEvents("ses-1", turn.turn_id, { transport: "websocket" }).result());
    expect(error).toMatchObject({ code: "unavailable", details: { phase: "websocket_handshake" } });
  });

  it("falls back when the factory itself throws (a browser, where the headers cannot be set)", async () => {
    const host = createHost();
    const { turn } = await firstTurn(host);
    const client = createClient({
      baseUrl: host.fake.baseUrl,
      fetch: host.fake.fetch,
      webSocket: () => {
        throw new TypeError("headers are not supported");
      },
      getContextToken: () => host.fake.issueToken({ session_id: "ses-1" }),
    });
    const stream = client.streamTurnEvents("ses-1", turn.turn_id);
    expect(await collect(stream)).toEqual(turn.events);
    expect(stream.transport).toBe("sse");
  });

  it("a slow-consumer close (1013) is a lost connection: resume from the cursor, and finish the turn", async () => {
    const host = createHost({}, {}, { webSocket: true });
    const { turn } = await firstTurn(host);
    host.fake.failNext({ operation: "streamTurnEvents", truncateAfterEvents: 2, closeCode: 1013 });
    const stream = host.client.streamTurnEvents("ses-1", turn.turn_id, { transport: "websocket", resume: { backoffMs: 0 } });
    expect(await collect(stream)).toEqual(turn.events);
    expect(host.fake.requests.filter((r) => r.operation === "streamTurnEvents")).toHaveLength(2);
  });

  it("without resume, a lost WebSocket is unavailable and resumable by hand from the cursor", async () => {
    const host = createHost({}, {}, { webSocket: true });
    const { turn } = await firstTurn(host);
    host.fake.failNext({ operation: "streamTurnEvents", truncateAfterEvents: 2, closeCode: 1013 });
    const stream = host.client.streamTurnEvents("ses-1", turn.turn_id, { transport: "websocket" });
    const seen: TurnEvent[] = [];
    const failure = await caught(
      (async () => {
        for await (const event of stream) seen.push(event);
      })(),
    );
    expect(failure).toMatchObject({ code: "unavailable", details: { reason: "slow_consumer", code: 1013 } });
    const rest = await collect(host.client.streamTurnEvents("ses-1", turn.turn_id, { cursor: stream.cursor! }));
    expect([...seen, ...rest]).toEqual(turn.events);
  });

  it("an abandoned turn (1011) is a typed error that is not resumed", async () => {
    const host = createHost({}, {}, { webSocket: true });
    const { turn } = await firstTurn(host);
    host.fake.failNext({ operation: "streamTurnEvents", truncateAfterEvents: 1, closeCode: 1011 });
    const failure = await caught(host.client.streamTurnEvents("ses-1", turn.turn_id, { transport: "websocket", resume: { backoffMs: 0 } }).result());
    expect(failure).toMatchObject({ code: "unavailable", retryable: false, details: { reason: "aborted" } });
    expect(host.fake.requests.filter((r) => r.operation === "streamTurnEvents")).toHaveLength(1);
  });

  it("sendTurn resumes over a WebSocket too, and the confirmation secret survives the change of transport", async () => {
    const host = createHost({}, {}, { webSocket: true });
    host.fake.onTurn(() => prepareAction(BOOK));
    host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 3 });
    const stream = host.client.sendTurn("ses-1", { text: "book" }, { resume: { backoffMs: 0 } });
    const result = await stream.result();
    expect(stream.transport).toBe("websocket");
    expect(result.confirmation?.token).toMatch(/^cfm_/);
    expect(result.events.map((e) => e.seq)).toEqual(result.events.map((_, i) => i));
  });

  it("cancel closes the socket and ends the iteration with the reason", async () => {
    const host = createHost({}, {}, { webSocket: true });
    const { turn } = await firstTurn(host);
    const stream = host.client.streamTurnEvents("ses-1", turn.turn_id, { transport: "websocket" });
    const first = await stream[Symbol.asyncIterator]().next();
    expect(first.done).toBe(false);
    stream.cancel(new DOMException("enough", "AbortError"));
    await expect(stream[Symbol.asyncIterator]().next()).rejects.toMatchObject({ name: "AbortError" });
  });
});
