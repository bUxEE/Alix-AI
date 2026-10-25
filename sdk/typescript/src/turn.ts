import type { ConfirmationGrant, TurnResult } from "./api-types.js";
import { createTurnSequence, parseTurnEvent } from "./events.js";
import { isAlixAIError, sdkError } from "./errors.js";
import type { TurnEvent } from "./gen/conversation/index.js";
import { createSseReader } from "./sse.js";
import type { SseReader } from "./sse.js";
import type { CallScope } from "./transport.js";
import { isConfirmationToken } from "./validate.js";

/** The response header that carries the single-use confirmation secret of an event-stream turn. */
export const CONFIRMATION_TOKEN_HEADER = "X-AlixAI-Confirmation-Token";

/** How the events of a turn reached the client. A host never needs this: the stream is the same whichever it is. */
export type TurnTransport = "sse" | "websocket" | "json";

/**
 * The events of one turn, as an async iterable, plus what is not an event.
 *
 * Sending happens when `sendTurn` is called (the request is on its way before you iterate). Iterate
 * with `for await`, or call `result()` to read it all. Events are validated as they arrive and the
 * sequence is enforced (one turn_id, seq from 0 without gaps, `done` last): a stream that ends
 * before `done`, a gap and an event of an unknown type each throw a typed AlixAIError rather than
 * ending the turn quietly. Breaking out of the loop releases the connection.
 *
 * The same type is returned by `streamTurnEvents`, which reads a turn again from a cursor.
 */
export interface TurnStream extends AsyncIterable<TurnEvent> {
  /**
   * The single-use confirmation secret and the preparation it belongs to, when the turn prepared an
   * action that needs the user's explicit confirmation; undefined otherwise (also for a level 3
   * action, which returns a `request_step_up` directive and cannot be confirmed here). Settles once
   * the turn has been read to its `done` event, and rejects if the turn fails or is closed early.
   * The secret is not a TurnEvent: it is read from the response header (or the JSON body). A stream
   * opened with `streamTurnEvents` never carries one: the secret is shown once, to the turn that made it.
   */
  readonly confirmation: Promise<ConfirmationGrant | undefined>;
  /** The turn this stream reads: known once its first event arrived (at once for `streamTurnEvents`). */
  readonly turnId: string | undefined;
  /**
   * The reconnect cursor: the `seq` of the last event this stream delivered (or the cursor it was opened
   * with), undefined before the first event. Pass it to `streamTurnEvents` to continue after a lost connection.
   */
  readonly cursor: number | undefined;
  /** How the events are reaching the client, once the connection is open; it can change when the stream resumes. */
  readonly transport: TurnTransport | undefined;
  /** Abandons the turn: the request is aborted and the connection released. Iteration then throws the abort reason. */
  cancel: (reason?: unknown) => void;
  /** Reads the stream to its end and returns every event with the confirmation grant. */
  result: () => Promise<TurnResult>;
}

/** A connection that already is a reader of frames (a WebSocket): the response of an SSE request is a `Response` instead. */
export interface FrameSource {
  reader: SseReader;
  via: "sse" | "websocket";
}

/** What opening a connection produced: the response of a request, a frame reader, or the server's 204 (the turn was read to its end). */
export type Opened = Response | FrameSource | "complete";

/** What a stream does after its connection was lost, when it is allowed to resume. */
export interface ReconnectPolicy {
  /** How many connections may be lost in a row without an event in between before the stream gives up. */
  maxAttempts: number;
  /** The wait before the first reconnect, in milliseconds; it doubles with each attempt in a row, to at most 5000, plus jitter. */
  backoffMs: number;
  /** Opens a new connection that continues after `cursor`. A refusal that is not retryable (expired, scope) ends the stream. */
  reopen: (at: { turnId: string; cursor: number | undefined }) => Promise<Opened>;
}

/** What the stream is reading from right now. */
type Source =
  | { kind: "frames"; reader: SseReader; via: "sse" | "websocket" }
  | { kind: "json"; events: TurnEvent[]; index: number; grant: ConfirmationGrant | undefined }
  | { kind: "complete" };

interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
  reject: (reason: unknown) => void;
}

const defer = <T>(): Deferred<T> => {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
};

const malformed = (reason: string) => sdkError("internal", `the server's turn response is malformed: ${reason}`, { details: { reason: "malformed_turn" } });

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const MAX_BACKOFF_MS = 5000;
/** The longest wait a server's Retry-After can ask for before a reconnect; the stream's own deadline still applies. */
const MAX_RETRY_AFTER_MS = 60_000;
/** Up to this share is added to a backoff at random, so that clients cut at the same moment do not all come back together. */
const BACKOFF_JITTER = 0.2;

const jittered = (ms: number): number => ms + Math.floor(ms * BACKOFF_JITTER * Math.random());

/** Waits, unless the call is abandoned first (then it rejects with the abort reason). */
const pause = (ms: number, signal: AbortSignal): Promise<void> =>
  new Promise<void>((resolve, reject) => {
    if (signal.aborted) {
      reject(signal.reason);
      return;
    }
    const onAbort = (): void => {
      clearTimeout(timer);
      reject(signal.reason);
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal.addEventListener("abort", onAbort, { once: true });
  });

/** A lost connection, as opposed to a protocol failure: the reason is the read failure, or undefined for a clean end before `done`. */
interface Disconnect {
  disconnected: true;
  reason: unknown;
}

const isDisconnect = (value: unknown): value is Disconnect => isRecord(value) && value["disconnected"] === true;

/**
 * Whether a failure to read a frame is a lost connection (worth resuming) and not a violation of the protocol. A network failure
 * is (a plain error from the body or the socket); so is a typed error that says so with `details.resumable`. A typed error of the
 * reader itself (an event too large, invalid UTF-8) is a violation and ends the turn.
 */
const isConnectionLoss = (error: unknown): boolean =>
  isAlixAIError(error) ? error.details?.["resumable"] === true : true;

export interface TurnStreamParams {
  /** Sends the request and resolves with the connection; throws a typed error for a non-2xx status. */
  start: () => Promise<Opened>;
  scope: CallScope;
  /** Maps whatever a fetch or a read threw to the error the host should see. */
  mapFailure: (error: unknown) => unknown;
  /** The turn being read again, when it is known before the first event (`streamTurnEvents`). */
  turnId?: string;
  /** The `seq` of the last event the reader already has: the stream continues at the next one. */
  cursor?: number;
  /** Present when a lost connection may be resumed from the cursor; absent means it ends the stream as `uncertain_outcome`. */
  reconnect?: ReconnectPolicy;
}

export const createTurnStream = (params: TurnStreamParams): TurnStream => {
  const { scope } = params;
  const sequence = createTurnSequence(params.turnId, params.cursor === undefined ? 0 : params.cursor + 1);
  const events: TurnEvent[] = [];
  const confirmation = defer<ConfirmationGrant | undefined>();
  confirmation.promise.catch(() => undefined); // a rejection nobody awaits is not an unhandled one

  let source: Source | undefined;
  let preparationId: string | undefined;
  /** The confirmation secret of the first response (a turn request); a resumed connection never carries one. */
  let headerToken: string | undefined;
  let closed = false;
  let failure: { error: unknown } | undefined;
  let failureDelivered = false;
  let attempts = 0;
  /** True once a lost connection was replaced: the first connection is the one that answers for the cursor the host gave. */
  let reopened = false;

  const close = async (): Promise<void> => {
    if (closed) return;
    closed = true;
    scope.release();
    if (source?.kind === "frames") await source.reader.cancel();
  };

  /** Records the first failure (later ones are consequences of it), settles the grant and releases everything. */
  const fail = async (error: unknown): Promise<unknown> => {
    if (failure === undefined) failure = { error: isAlixAIError(error) ? error : params.mapFailure(error) };
    confirmation.reject(failure.error);
    await close();
    return failure.error;
  };

  const openSource = async (opened: Opened, first: boolean): Promise<Source> => {
    if (opened === "complete") return { kind: "complete" };
    if (!(opened instanceof Response)) return { kind: "frames", reader: opened.reader, via: opened.via };
    const response = opened;
    const contentType = (response.headers.get("Content-Type") ?? "").toLowerCase();
    if (contentType.includes("text/event-stream")) {
      if (response.body === null) throw malformed("an event stream without a body");
      const header = response.headers.get(CONFIRMATION_TOKEN_HEADER);
      if (header !== null && !isConfirmationToken(header)) throw malformed("the confirmation token header is malformed");
      if (first && header !== null) headerToken = header;
      return { kind: "frames", reader: createSseReader(response.body), via: "sse" };
    }
    if (contentType.includes("application/json")) {
      // The server answered with the JSON form of the turn instead of a stream: same events, same checks.
      let body: unknown;
      try {
        body = JSON.parse(await response.text());
      } catch (error) {
        if (scope.signal.aborted) throw scope.signal.reason;
        throw isAlixAIError(error) ? error : malformed("the body is not JSON");
      }
      return jsonSource(body);
    }
    throw malformed("the content type is neither text/event-stream nor application/json");
  };

  const jsonSource = (body: unknown): Source => {
    if (!isRecord(body) || typeof body["turn_id"] !== "string" || !Array.isArray(body["events"]) || body["events"].length === 0) {
      throw malformed("a JSON turn needs a turn_id and a non-empty events array");
    }
    const jsonSequence = createTurnSequence(body["turn_id"]);
    const list = body["events"].map((raw: unknown) => jsonSequence.accept(parseTurnEvent(raw)));
    jsonSequence.finish();
    let grant: ConfirmationGrant | undefined;
    const raw = body["confirmation"];
    if (raw !== undefined) {
      if (!isRecord(raw) || typeof raw["preparation_id"] !== "string" || !isConfirmationToken(raw["token"])) {
        throw malformed("the confirmation member is malformed");
      }
      grant = { preparation_id: raw["preparation_id"], token: raw["token"] };
    }
    return { kind: "json", events: list, index: 0, grant };
  };

  /** The grant: the header's secret paired with the preparation the turn prepared. */
  const complete = (done: Extract<TurnEvent, { type: "done" }>): void => {
    const id = preparationId ?? done.preparation_id;
    if (preparationId !== undefined && done.preparation_id !== undefined && preparationId !== done.preparation_id) {
      throw malformed("done names another preparation than the prepared_action event");
    }
    let grant: ConfirmationGrant | undefined;
    if (headerToken !== undefined) {
      if (id === undefined) throw malformed("a confirmation token arrived without a prepared action");
      grant = { preparation_id: id, token: headerToken };
    } else if (source?.kind === "json" && source.grant !== undefined) {
      if (id !== undefined && id !== source.grant.preparation_id) throw malformed("the confirmation names another preparation than the prepared_action event");
      grant = source.grant;
    }
    confirmation.resolve(grant);
  };

  const record = (event: TurnEvent): void => {
    events.push(event);
    attempts = 0; // progress: the next loss of the connection starts a fresh series
    if (event.type === "prepared_action") preparationId = event.prepared_action.preparation_id;
  };

  const opened: Promise<void> = params
    .start()
    .then((first) => openSource(first, true))
    .then((resolved) => {
      source = resolved;
    });
  // An error before the first read is parked and delivered to the first next(); it must not be unhandled.
  opened.catch(async (error: unknown) => {
    await fail(error);
  });

  /** Reads the next event from the current connection; undefined when the server reported the turn as read to its end. A lost connection is thrown as a Disconnect. */
  const pullOnce = async (): Promise<TurnEvent | undefined> => {
    // An abort ends iteration at once, even when events are already buffered (the SSE reader can hand out a parsed frame
    // without touching the network, and a JSON turn is all in memory).
    if (scope.signal.aborted) throw scope.signal.reason;
    const src = source;
    if (src === undefined) throw malformed("no response");
    if (src.kind === "complete") {
      // The server says the turn was read to its end. That is only credible when we have its `done` event.
      if (sequence.done()) throw malformed("the stream was read past its end");
      throw sdkError("uncertain_outcome", "the server reports the turn as read to its end, but its done event was not received", {
        details: { reason: "stream_ended_early" },
      });
    }
    if (src.kind === "json") {
      const next = src.events[src.index++];
      if (next === undefined) throw malformed("the events ran out before done");
      const event = sequence.accept(next); // already checked as a whole when the body was read; this keeps the stream's own account
      record(event);
      if (event.type === "done") complete(event);
      return event;
    }
    let frame: Awaited<ReturnType<SseReader["next"]>>;
    try {
      frame = await src.reader.next();
    } catch (error) {
      if (scope.signal.aborted || !isConnectionLoss(error)) throw error;
      throw { disconnected: true, reason: error } satisfies Disconnect;
    }
    if (scope.signal.aborted) throw scope.signal.reason;
    if (frame === undefined) {
      if (src.via === "websocket" && params.cursor !== undefined && events.length === 0 && !reopened && !scope.signal.aborted) {
        // A normal close with nothing sent, for a cursor the host gave: the cursor was at the end of a finished turn (over SSE the
        // server says so with a 204). Resumed after a lost connection it would be a contradiction and is handled as one.
        source = { kind: "complete" };
        return undefined;
      }
      if (!scope.signal.aborted && !sequence.done()) throw { disconnected: true, reason: undefined } satisfies Disconnect;
      sequence.finish(); // throws: the stream ended before done
      throw malformed("the stream ended");
    }
    let json: unknown;
    try {
      json = JSON.parse(frame.data);
    } catch {
      throw malformed("an event is not valid JSON");
    }
    const event = sequence.accept(parseTurnEvent(json), { event: frame.event, id: frame.id });
    record(event);
    if (event.type === "done") complete(event);
    return event;
  };

  /**
   * After a lost connection: opens a new one from the cursor, as often as the policy allows, and swaps it in. Returns normally when
   * the stream is ready to be read again; throws the error the host should see otherwise.
   */
  const resume = async (lost: Disconnect): Promise<void> => {
    const policy = params.reconnect;
    const turnId = sequence.turnId();
    const unresumable = (): unknown => {
      // Without a policy, or before the first event names the turn, a lost connection is what it always was.
      if (lost.reason === undefined) {
        try {
          sequence.finish();
        } catch (error) {
          return error;
        }
        return malformed("the stream ended");
      }
      return lost.reason;
    };
    if (policy === undefined || turnId === undefined) throw unresumable();
    let last: unknown = lost.reason;
    while (attempts < policy.maxAttempts) {
      const wait = jittered(Math.min(policy.backoffMs * 2 ** attempts, MAX_BACKOFF_MS));
      attempts += 1;
      if (source?.kind === "frames") await source.reader.cancel();
      // The server's Retry-After is honoured (a busy server asks for longer than the backoff), up to a bound.
      await pause(isAlixAIError(last) && last.retryAfterMs !== undefined ? Math.max(wait, Math.min(last.retryAfterMs, MAX_RETRY_AFTER_MS)) : wait, scope.signal);
      try {
        source = await openSource(await policy.reopen({ turnId, cursor: sequence.cursor() }), false);
        reopened = true;
        return;
      } catch (error) {
        if (scope.signal.aborted) throw scope.signal.reason;
        // A refusal that will not change (the cursor expired, the turn is not visible, the token is bad) ends the stream with it.
        if (isAlixAIError(error) && !error.retryable) throw error;
        last = error;
      }
    }
    throw isAlixAIError(last) ? last : unresumable();
  };

  const pull = async (): Promise<TurnEvent | undefined> => {
    await opened;
    for (;;) {
      try {
        return await pullOnce();
      } catch (error) {
        if (!isDisconnect(error)) throw error;
        await resume(error);
      }
    }
  };

  const iterator: AsyncIterator<TurnEvent, undefined> = {
    next: async () => {
      if (failure !== undefined) {
        if (failureDelivered) return { done: true, value: undefined };
        failureDelivered = true;
        throw failure.error;
      }
      if (closed) return { done: true, value: undefined };
      try {
        await opened;
        if (source?.kind === "complete") {
          await close();
          return { done: true, value: undefined };
        }
        const event = await pull();
        if (event === undefined) {
          await close();
          return { done: true, value: undefined };
        }
        if (event.type === "done") await close();
        return { done: false, value: event };
      } catch (error) {
        failureDelivered = true;
        throw await fail(error);
      }
    },
    return: async () => {
      if (!closed && failure === undefined) {
        confirmation.reject(
          sdkError("uncertain_outcome", "the turn stream was closed before its done event", { details: { reason: "closed_early" } }),
        );
      }
      await close();
      return { done: true, value: undefined };
    },
  };

  const cancel = (reason?: unknown): void => {
    if (closed) return;
    const why = reason ?? new DOMException("The turn was cancelled", "AbortError");
    scope.cancel(why); // aborts the request or the body read in flight
    void fail(why); // and ends iteration with the reason, whether or not a read was in flight
  };

  const result = async (): Promise<TurnResult> => {
    for (;;) {
      const step = await iterator.next();
      if (step.done === true) break;
    }
    const turnId = sequence.turnId();
    if (turnId === undefined) throw malformed("the turn has no events");
    // A stream the server reported as read to its end has nothing more to give: its events are the ones before the cursor.
    if (!sequence.done() && source?.kind !== "complete") throw malformed("the turn has no events");
    if (source?.kind === "complete") confirmation.resolve(undefined);
    const grant = await confirmation.promise;
    return grant === undefined ? { turn_id: turnId, events: [...events] } : { turn_id: turnId, events: [...events], confirmation: grant };
  };

  const stream: TurnStream = {
    confirmation: confirmation.promise,
    get turnId() {
      return sequence.turnId();
    },
    get cursor() {
      return sequence.cursor();
    },
    get transport() {
      if (source === undefined || source.kind === "complete") return undefined;
      return source.kind === "json" ? "json" : source.via;
    },
    cancel,
    result,
    [Symbol.asyncIterator]: () => iterator as AsyncIterator<TurnEvent>,
  };
  return stream;
};
