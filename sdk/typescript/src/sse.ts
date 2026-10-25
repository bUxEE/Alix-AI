import { sdkError } from "./errors.js";

// A small server-sent-events reader over a ReadableStream, written to the "Event stream
// interpretation" rules of the HTML standard: UTF-8 with an optional BOM, lines ended by CRLF, LF or
// CR, `:` comment lines, `field: value` with one optional space, data lines joined by LF, an event
// dispatched by a blank line. An event cut off before its blank line is never dispatched, so a
// connection that dies mid-frame cannot deliver half an event. It is pull-based, uses getReader()
// (not async iteration of the stream, which Safari lacks) and decodes with `stream: true` so a
// multi-byte character split across two chunks decodes correctly.

export interface SseMessage {
  /** The `event:` field; "message" when the frame has none. */
  event: string;
  /** The `data:` lines joined by "\n". */
  data: string;
  /** The `id:` field of this frame, undefined when the frame has none. */
  id: string | undefined;
}

export interface SseReader {
  /** The next complete event, or undefined at the clean end of the stream. */
  next: () => Promise<SseMessage | undefined>;
  /** Releases the connection. Safe to call more than once. */
  cancel: () => Promise<void>;
}

/** Upper bound for one event (or one unterminated line): a server that never ends a frame is refused. */
export const MAX_EVENT_CHARS = 8 * 1024 * 1024;

export const createSseReader = (body: ReadableStream<Uint8Array>, maxEventChars: number = MAX_EVENT_CHARS): SseReader => {
  const reader = body.getReader();
  // fatal: invalid UTF-8 is a protocol violation, never silently replaced.
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let buffer = "";
  let finished = false;
  let cancelled = false;

  let eventName = "";
  let dataLines: string[] = [];
  let dataChars = 0;
  let id: string | undefined;

  const resetEvent = (): void => {
    eventName = "";
    dataLines = [];
    dataChars = 0;
    id = undefined;
  };

  const tooLarge = () => sdkError("internal", "an event of the stream exceeds the size limit", { details: { reason: "event_too_large" } });

  /** Applies one line; returns the dispatched message when the line was the blank line ending an event. */
  const applyLine = (line: string): SseMessage | undefined => {
    if (line === "") {
      if (dataLines.length === 0) {
        resetEvent(); // a frame with no data (comments, or only `event:`) dispatches nothing
        return undefined;
      }
      const message: SseMessage = { event: eventName === "" ? "message" : eventName, data: dataLines.join("\n"), id };
      resetEvent();
      return message;
    }
    if (line.startsWith(":")) return undefined;
    const colon = line.indexOf(":");
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    switch (field) {
      case "event":
        eventName = value;
        break;
      case "data":
        dataChars += value.length + 1;
        if (dataChars > maxEventChars) throw tooLarge();
        dataLines.push(value);
        break;
      case "id":
        if (!value.includes("\u0000")) id = value;
        break;
      default:
        break; // retry and unknown fields are ignored
    }
    return undefined;
  };

  /** Consumes complete lines from the buffer until an event is dispatched or no complete line is left. */
  const drain = (): SseMessage | undefined => {
    for (;;) {
      let end = -1;
      let width = 1;
      for (let i = 0; i < buffer.length; i++) {
        const c = buffer.charCodeAt(i);
        if (c === 10) {
          end = i;
          break;
        }
        if (c === 13) {
          if (i + 1 < buffer.length) {
            end = i;
            width = buffer.charCodeAt(i + 1) === 10 ? 2 : 1;
            break;
          }
          if (finished) {
            end = i; // a CR at the very end of the stream ends its line
            break;
          }
          return undefined; // CR may be the first half of CRLF: wait for the next chunk
        }
      }
      if (end === -1) {
        if (buffer.length > maxEventChars) throw tooLarge();
        return undefined;
      }
      const line = buffer.slice(0, end);
      buffer = buffer.slice(end + width);
      const message = applyLine(line);
      if (message !== undefined) return message;
    }
  };

  const next = async (): Promise<SseMessage | undefined> => {
    for (;;) {
      const message = drain();
      if (message !== undefined) return message;
      if (finished || cancelled) return undefined;
      let chunk: ReadableStreamReadResult<Uint8Array>;
      try {
        chunk = await reader.read();
      } catch (error) {
        finished = true;
        throw error;
      }
      if (chunk.done) {
        finished = true;
        try {
          buffer += decoder.decode();
        } catch (cause) {
          throw sdkError("internal", "the event stream is not valid UTF-8", { cause });
        }
        continue; // drain once more: a trailing CR now counts as a line end; an unfinished line is dropped
      }
      try {
        buffer += decoder.decode(chunk.value, { stream: true });
      } catch (cause) {
        finished = true;
        throw sdkError("internal", "the event stream is not valid UTF-8", { cause });
      }
    }
  };

  const cancel = async (): Promise<void> => {
    if (cancelled) return;
    cancelled = true;
    finished = true;
    try {
      await reader.cancel();
    } catch {
      // the stream is already errored or closed: nothing is left to release
    }
  };

  return { next, cancel };
};
