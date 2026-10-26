import { isErrorCode, sdkError } from "./errors.js";
import type { DoneEvent, TurnEvent } from "./gen/conversation/index.js";

// Turn events arrive from a server the SDK does not control (a proxy sits in between, a new server
// may be older or newer). The generated TurnEvent type is a promise about the data, so before an
// event is handed to the host it passes a shallow structural check: the discriminant, the envelope
// (turn_id, seq) and the member each variant must carry. The JSON Schema is the authority for
// everything deeper; this check exists so that a malformed or unknown event is a typed error
// instead of an `undefined` three calls later. Decoding fails closed: an unknown event type is an
// error, not an event to skip.

export const TURN_EVENT_TYPES = [
  "token",
  "directive",
  "clarification",
  "prepared_action",
  "receipt",
  "citation",
  "progress",
  "error",
  "done",
  "block",
  "memory_proposal",
] as const satisfies readonly TurnEvent["type"][];

type EventTypeName = (typeof TURN_EVENT_TYPES)[number];

const FINISH_REASONS: ReadonlySet<string> = new Set(["completed", "awaiting_input", "awaiting_confirmation", "cancelled", "failed"]);

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const malformed = (reason: string, extra: Record<string, unknown> = {}) =>
  sdkError("internal", `the server sent a malformed turn event: ${reason}`, { details: { reason: "malformed_event", ...extra } });

// The member each variant carries besides the envelope, and how to check it.
const VARIANT_CHECKS: Record<EventTypeName, (event: Record<string, unknown>) => string | undefined> = {
  token: (e) => (typeof e["text"] === "string" ? undefined : "token.text must be a string"),
  directive: (e) => (isRecord(e["directive"]) ? undefined : "directive must be an object"),
  clarification: (e) => (isRecord(e["clarification"]) ? undefined : "clarification must be an object"),
  prepared_action: (e) => {
    const action = e["prepared_action"];
    return isRecord(action) && typeof action["preparation_id"] === "string"
      ? undefined
      : "prepared_action must be an object with a preparation_id";
  },
  receipt: (e) => (isRecord(e["receipt"]) ? undefined : "receipt must be an object"),
  citation: (e) => (isRecord(e["citation"]) ? undefined : "citation must be an object"),
  progress: (e) =>
    typeof e["stage"] === "string" && typeof e["state"] === "string" ? undefined : "progress needs a stage and a state",
  error: (e) => {
    const error = e["error"];
    return isRecord(error) &&
      isErrorCode(error["code"]) &&
      typeof error["message"] === "string" &&
      typeof error["retryable"] === "boolean"
      ? undefined
      : "error must be an error document";
  },
  done: (e) => {
    if (typeof e["finish_reason"] !== "string" || !FINISH_REASONS.has(e["finish_reason"])) return "done.finish_reason is not a known reason";
    const hasPreparation = e["preparation_id"] !== undefined;
    if (hasPreparation && typeof e["preparation_id"] !== "string") return "done.preparation_id must be a string";
    if ((e["finish_reason"] === "awaiting_confirmation") !== hasPreparation) {
      return "done.preparation_id is present exactly when finish_reason is awaiting_confirmation";
    }
    return undefined;
  },
  block: (e) => (isRecord(e["block"]) && typeof e["block"]["type"] === "string" ? undefined : "block must be an object with a type"),
  memory_proposal: (e) => (isRecord(e["memory"]) && typeof e["memory"]["id"] === "string" ? undefined : "memory_proposal.memory must be an object with an id"),
};

const isEventType = (value: unknown): value is EventTypeName =>
  typeof value === "string" && (TURN_EVENT_TYPES as readonly string[]).includes(value);

/** Checks one decoded event and returns it typed. Throws an `internal` AlixAIError when it is malformed or of an unknown type. */
export const parseTurnEvent = (value: unknown): TurnEvent => {
  if (!isRecord(value)) throw malformed("an event must be a JSON object");
  const type = value["type"];
  if (!isEventType(type)) throw malformed("unknown event type", { event_type: typeof type === "string" ? type.slice(0, 64) : typeof type });
  if (typeof value["turn_id"] !== "string" || value["turn_id"] === "") throw malformed("turn_id must be a non-empty string");
  const seq = value["seq"];
  if (typeof seq !== "number" || !Number.isInteger(seq) || seq < 0) throw malformed("seq must be a non-negative integer");
  const problem = VARIANT_CHECKS[type](value);
  if (problem !== undefined) throw malformed(problem, { event_type: type });
  return value as unknown as TurnEvent;
};

/**
 * Enforces what the API promises about the sequence of a turn: one turn_id throughout, seq running
 * from 0 without gaps, `done` last. An SSE frame's `event:` and `id:` fields must agree with the
 * event they carry.
 */
export interface TurnSequence {
  /** Accepts the next event; throws when it breaks the sequence. Returns it unchanged. */
  accept: (event: TurnEvent, frame?: { event: string; id: string | undefined }) => TurnEvent;
  /** Throws unless the `done` event has been accepted: a stream that ends earlier is an unknown outcome. */
  finish: () => void;
  readonly done: () => boolean;
  readonly turnId: () => string | undefined;
  /** The `seq` of the last event accepted (or the one the sequence was started after); undefined when none was. */
  readonly cursor: () => number | undefined;
}

/**
 * `nextSeq` is the `seq` the first event must carry: 0 for a whole turn, the cursor plus one for a turn read again from a cursor
 * (the stream continues with the next event, each exactly once and in order).
 */
export const createTurnSequence = (expectedTurnId?: string, nextSeq = 0): TurnSequence => {
  let turnId: string | undefined = expectedTurnId;
  let done = false;

  const accept: TurnSequence["accept"] = (event, frame) => {
    if (done) throw malformed("an event follows the done event");
    if (turnId === undefined) turnId = event.turn_id;
    else if (event.turn_id !== turnId) throw malformed("the turn_id changed within one turn", { seq: event.seq });
    if (event.seq !== nextSeq) {
      throw sdkError("internal", `the event sequence is broken: expected seq ${nextSeq}, got ${event.seq}`, {
        details: { reason: "sequence_gap", expected: nextSeq, got: event.seq },
      });
    }
    if (frame !== undefined) {
      if (frame.event !== event.type) throw malformed("the frame's event name differs from the event type", { seq: event.seq });
      if (frame.id !== String(event.seq)) throw malformed("the frame's id differs from the event's seq", { seq: event.seq });
    }
    nextSeq += 1;
    if (event.type === "done") done = true;
    return event;
  };

  const finish = (): void => {
    if (!done) {
      throw sdkError(
        "uncertain_outcome",
        "the turn ended before its done event: what the turn did is unknown; read the session's pending state before sending anything again",
        { details: { reason: "stream_ended_early", received: nextSeq } },
      );
    }
  };

  return { accept, finish, done: () => done, turnId: () => turnId, cursor: () => (nextSeq === 0 ? undefined : nextSeq - 1) };
};

/** The text of a turn: its token events, joined in order. */
export const turnText = (events: readonly TurnEvent[]): string =>
  events.map((event) => (event.type === "token" ? event.text : "")).join("");

/** The `done` event of a finished turn. */
export const doneEvent = (events: readonly TurnEvent[]): DoneEvent | undefined => {
  const last = events[events.length - 1];
  return last !== undefined && last.type === "done" ? last : undefined;
};
