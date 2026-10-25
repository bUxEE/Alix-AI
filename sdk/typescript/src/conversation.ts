import type { Session, SessionEnded } from "./api-types.js";
import type { AlixAIClient, StreamOptions } from "./client.js";
import { withAttachments } from "./compose.js";
import type { MessageStream, OutgoingMessage } from "./compose.js";
import { sdkError } from "./errors.js";
import type { FileRef, TurnEvent } from "./gen/conversation/index.js";
import type { TurnStream } from "./turn.js";
import { viewTurn } from "./view.js";
import type { TurnView } from "./view.js";

// A conversation over one session (master plan 15.1): the turns sent and what came back, with stop, regenerate, edit-and-resubmit and
// resume. It is a small state holder over the client's own methods, and it adds no authority: a regenerate or an edit is a NEW text
// turn on the same session, because the API has no route that rewinds or re-runs a turn, and the session is append-only. So the
// guards here are the ones that keep a new turn from doing something twice.
//
//   * A turn that committed something (it has a `receipt`) is never regenerated or edited: sending its text again could run the
//     action again. The user starts a new request, in words, like any other.
//   * A turn whose prepared action still waits for the user is regenerated or edited only with `discardPending: true`, which first
//     REJECTS that preparation (nothing is ever sent to the application by a rejection).
//   * A turn that was not read to its end is not sent again either: what it did is unknown. Read it with `resume()` first.
//   * Only the latest turn can be edited: the session cannot be rewound, so an older turn would leave later turns answering text
//     that no longer exists.
//   * Stop is client-side. The service runs a turn to completion before it sends anything, so stopping does not halt server work:
//     it releases the connection and ends the local iteration. The turn stays in the replay buffer for a few minutes, and
//     `resume()` reads the rest of it.
//
// Switching conversations is opening another `Conversation` (`client.conversation(otherSessionId)`): a session is the one its context
// token names, so each conversation has its own bound client and its own record, and nothing leaks between them.

/** What was sent as the input of a turn. */
export type SentInput =
  | { kind: "text"; text: string; language?: string; attachments: FileRef[] }
  | { kind: "choice"; choiceId: string };

/**
 * Where a recorded turn stands: `streaming` (events are still arriving), `done` (its `done` event arrived), `stopped` (the host
 * stopped it or abandoned the stream; `resume()` can read the rest) or `failed` (the stream failed; `error` says how).
 */
export type TurnStatus = "streaming" | "done" | "stopped" | "failed";

/** One turn of the conversation as this process saw it. The conversation owns these objects: read them, do not change them. */
export interface TurnRecord {
  /** Its position in `turns`. */
  readonly index: number;
  readonly input: SentInput;
  /** The turn's id, once its first event arrived. */
  turnId: string | undefined;
  /** The events received so far, in order. */
  events: TurnEvent[];
  status: TurnStatus;
  /** Why a `failed` turn failed. */
  error: unknown;
  /** The index of the record that replaced this one (a regenerate or an edit), when one did: show the latest and fold this one away. */
  supersededBy: number | undefined;
  /** The confirmation secret of the action this turn prepared, when it needs the user. Show it to the user's client only; present it once. */
  confirmation: { preparation_id: string; token: string } | undefined;
}

export interface ConversationOptions {
  /** Defaults for every stream of this conversation, for example the reconnect policy. */
  stream?: StreamOptions;
}

export interface RegenerateOptions extends StreamOptions {
  /** Reject the prepared action that still waits for the user, then send the text again. Without it that turn is refused. */
  discardPending?: boolean;
}

export interface Conversation {
  readonly sessionId: string;
  /** The client bound to this session. */
  readonly client: AlixAIClient;
  /** Every turn sent through this conversation, oldest first. */
  readonly turns: readonly TurnRecord[];
  /** The turns not replaced by a regenerate or an edit: what a transcript shows. */
  readonly transcript: readonly TurnRecord[];
  /** The turn whose stream is open, if any. */
  readonly active: TurnRecord | undefined;
  /** Opens the session the token names (creating it on first sight) and returns it with its structured pending state. */
  open: () => Promise<Session>;
  /** Sends a text message, with attachments if it has any. The stream records into `turns` as it is read. */
  send: (message: string | OutgoingMessage, options?: StreamOptions) => MessageStream;
  /** Answers the pending clarification with one of its choices. */
  choose: (choiceId: string, options?: StreamOptions) => TurnStream;
  /** Abandons the open stream (client-side; see the note above). Returns false when none is open. */
  stop: () => boolean;
  /** Sends the text of the latest turn again as a new turn. Refused after a committed action; see the note above. */
  regenerate: (options?: RegenerateOptions) => Promise<MessageStream>;
  /** Replaces the text of the latest turn with a new one and sends it. Same guards as `regenerate`. */
  editAndResubmit: (text: string, options?: RegenerateOptions) => Promise<MessageStream>;
  /** Reads the rest of the latest turn from the cursor the conversation holds (after a stop or a lost connection). */
  resume: (options?: StreamOptions) => TurnStream;
  /** The view of a recorded turn, for rendering. */
  view: (record: TurnRecord) => TurnView;
  /** Ends the session. A prepared action it waits on is invalidated; nothing is ever committed by ending. */
  end: () => Promise<SessionEnded>;
}

const conflict = (message: string, reason: string) => sdkError("conflict", message, { details: { reason } });

export const createConversation = (client: AlixAIClient, sessionId: string, defaults: ConversationOptions | undefined): Conversation => {
  const turns: TurnRecord[] = [];
  let active: TurnRecord | undefined;
  let activeStream: TurnStream | undefined;

  const merged = (options: StreamOptions | undefined): StreamOptions => ({ ...defaults?.stream, ...options });

  /** A stream with the same shape that also records what it yields into `record`. */
  const track = (stream: TurnStream, record: TurnRecord): TurnStream => {
    const iterator = stream[Symbol.asyncIterator]();
    const settle = (status: TurnStatus, error?: unknown): void => {
      if (record.status === "streaming") record.status = status;
      if (status === "failed") record.error = error;
      if (active === record) {
        active = undefined;
        activeStream = undefined;
      }
    };
    const observed: AsyncIterator<TurnEvent> = {
      next: async () => {
        try {
          const step = await iterator.next();
          if (step.done === true) {
            // A stream that ends without `done` was stopped or closed early.
            settle(record.events.at(-1)?.type === "done" ? "done" : "stopped");
            return step;
          }
          record.events.push(step.value);
          record.turnId ??= step.value.turn_id;
          if (step.value.type === "done") {
            settle("done");
            stream.confirmation.then(
              (grant) => {
                if (grant !== undefined) record.confirmation = grant;
              },
              () => undefined,
            );
          }
          return step;
        } catch (error) {
          // An abort that came from stop() is a stop, not a failure.
          settle(record.status === "stopped" ? "stopped" : "failed", error);
          throw error;
        }
      },
      return: async () => {
        const step = await (iterator.return?.() ?? Promise.resolve({ done: true as const, value: undefined }));
        settle("stopped");
        return step;
      },
    };
    return {
      confirmation: stream.confirmation,
      get turnId() {
        return stream.turnId ?? record.turnId;
      },
      get cursor() {
        return stream.cursor;
      },
      get transport() {
        return stream.transport;
      },
      cancel: (reason) => {
        if (record.status === "streaming") record.status = "stopped";
        stream.cancel(reason);
        settle("stopped");
      },
      result: async () => {
        for (;;) {
          const step = await observed.next();
          if (step.done === true) break;
        }
        return stream.result();
      },
      [Symbol.asyncIterator]: () => observed,
    };
  };

  const begin = (input: SentInput): TurnRecord => {
    if (active !== undefined) throw conflict("a turn is still streaming: stop it, or read it to its end, before sending another", "turn_in_flight");
    const record: TurnRecord = {
      index: turns.length,
      input,
      turnId: undefined,
      events: [],
      status: "streaming",
      error: undefined,
      supersededBy: undefined,
      confirmation: undefined,
    };
    turns.push(record);
    active = record;
    return record;
  };

  const latest = (): TurnRecord | undefined => [...turns].reverse().find((record) => record.supersededBy === undefined);

  const send: Conversation["send"] = (message, options) => {
    const outgoing: OutgoingMessage = typeof message === "string" ? { text: message } : message;
    const record = begin({
      kind: "text",
      text: typeof outgoing?.text === "string" ? outgoing.text : "",
      ...(outgoing?.language !== undefined && { language: outgoing.language }),
      attachments: [],
    });
    const stream = client.sendMessage(sessionId, outgoing, merged(options));
    activeStream = stream;
    stream.attachments.then(
      (refs) => {
        (record.input as { attachments: FileRef[] }).attachments = refs;
      },
      () => undefined,
    );
    return withAttachments(track(stream, record), stream.attachments);
  };

  /** The guards of a regenerate or an edit on the latest turn. Returns it, or throws. May reject the preparation it waits on. */
  const reusable = async (options: RegenerateOptions | undefined): Promise<TurnRecord & { input: Extract<SentInput, { kind: "text" }> }> => {
    const last = latest();
    if (last === undefined) throw conflict("there is no turn to send again", "no_turn");
    if (active !== undefined) throw conflict("a turn is still streaming: stop it, or read it to its end, first", "turn_in_flight");
    if (last.input.kind !== "text") throw sdkError("validation_failed", "only a text turn can be sent again", { details: { argument: "turn" } });
    if (last.status !== "done") {
      throw conflict("that turn was not read to its end, so what it did is unknown: resume it first", "turn_incomplete");
    }
    if (last.events.some((event) => event.type === "receipt")) {
      throw conflict("that turn committed an action: sending its text again could run it again, so start a new request instead", "turn_committed");
    }
    const prepared = last.events.find((event) => event.type === "prepared_action");
    if (prepared?.type === "prepared_action") {
      const waiting = await client.getAction(prepared.prepared_action.preparation_id);
      if (waiting.state === "prepared" || waiting.state === "confirmed") {
        if (options?.discardPending !== true) {
          throw conflict("that turn prepared an action that still waits for the user: confirm or reject it, or pass discardPending", "pending_preparation");
        }
        await client.rejectAction(waiting.preparation_id);
      } else if (waiting.state === "committed") {
        throw conflict("that turn's action was committed: sending its text again could run it again", "turn_committed");
      }
    }
    return last as TurnRecord & { input: Extract<SentInput, { kind: "text" }> };
  };

  const resend = (previous: TurnRecord, text: string, language: string | undefined, options: StreamOptions | undefined): MessageStream => {
    const stream = send({ text, ...(language !== undefined && { language }) }, options);
    previous.supersededBy = turns.length - 1;
    return stream;
  };

  return {
    sessionId,
    client,
    get turns() {
      return turns;
    },
    get transcript() {
      return turns.filter((record) => record.supersededBy === undefined);
    },
    get active() {
      return active;
    },
    open: () => client.createSession(),
    send,
    choose: (choiceId, options) => {
      const record = begin({ kind: "choice", choiceId });
      const stream = client.sendTurn(sessionId, { choice_id: choiceId }, merged(options));
      activeStream = stream;
      return track(stream, record);
    },
    stop: () => {
      if (active === undefined || activeStream === undefined) return false;
      active.status = "stopped";
      const stream = activeStream;
      active = undefined;
      activeStream = undefined;
      stream.cancel(new DOMException("The turn was stopped", "AbortError"));
      return true;
    },
    regenerate: async (options) => {
      const previous = await reusable(options);
      return resend(previous, previous.input.text, previous.input.language, options);
    },
    editAndResubmit: async (text, options) => {
      const previous = await reusable(options);
      return resend(previous, text, previous.input.language, options);
    },
    resume: (options) => {
      const record = latest();
      if (record?.turnId === undefined) {
        throw conflict("there is no turn with events to resume: a turn that has not shown its first event cannot be read again", "nothing_to_resume");
      }
      if (record.status === "done") throw conflict("that turn is complete", "turn_complete");
      if (active !== undefined) throw conflict("a turn is still streaming", "turn_in_flight");
      const cursor = record.events.at(-1)?.seq;
      record.status = "streaming";
      active = record;
      const stream = client.streamTurnEvents(sessionId, record.turnId, { ...merged(options), ...(cursor !== undefined && { cursor }) });
      activeStream = stream;
      return track(stream, record);
    },
    view: (record) => viewTurn(record.events),
    end: () => client.endSession(sessionId),
  };
};
