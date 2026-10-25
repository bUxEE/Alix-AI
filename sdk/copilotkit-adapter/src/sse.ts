// A small server-sent-events reader that keeps the `id:` field. The AG-UI client's own reader does not expose it, and the
// resume cursor of the AlixAI gateway lives there: `id: <seq>` sits on the LAST event of each TurnEvent, so a frame
// with an id closes a group (docs/integration/agui.md section 9).

export interface SseFrame {
  /** The `id:` field of this frame, when it had one. */
  readonly id: string | undefined;
  /** The `data:` lines joined with a newline. */
  readonly data: string;
}

/** The longest frame accepted: the gateway caps an event at 256 KiB, so this is generous and still bounded. */
export const MAX_FRAME_CHARS = 1024 * 1024;

/**
 * Reads frames from a byte stream until it ends. Comments, `event:` and `retry:` fields are ignored. A frame without
 * data is skipped, and so is a last frame the stream ended inside. A frame longer than MAX_FRAME_CHARS is an error (the
 * stream is hostile or broken).
 */
// (A generator cannot be an arrow function.)
export async function* readSseFrames(body: ReadableStream<Uint8Array>, signal?: AbortSignal): AsyncGenerator<SseFrame> {
  const reader = body.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: false });
  let buffer = "";
  const onAbort = (): void => {
    reader.cancel(signal?.reason).catch(() => undefined);
  };
  signal?.addEventListener("abort", onAbort, { once: true });
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      if (buffer.length > MAX_FRAME_CHARS * 2) throw new Error("an event frame is too large");
      for (;;) {
        const split = /\r\n\r\n|\n\n|\r\r/.exec(buffer);
        if (split === null) break;
        const raw = buffer.slice(0, split.index);
        buffer = buffer.slice(split.index + split[0].length);
        const frame = parseFrame(raw);
        if (frame !== undefined) yield frame;
      }
      if (buffer.length > MAX_FRAME_CHARS) throw new Error("an event frame is too large");
    }
    // Whatever is left never got its blank line: the connection died inside it. It is dropped, as the HTML standard says, so
    // half an event (and the id written before its data) is never delivered and the cursor never passes it; the reconnect
    // replays it whole.
  } finally {
    signal?.removeEventListener("abort", onAbort);
    reader.cancel().catch(() => undefined);
  }
}

const parseFrame = (raw: string): SseFrame | undefined => {
  let id: string | undefined;
  const data: string[] = [];
  for (const line of raw.split(/\r\n|\n|\r/)) {
    if (line.length === 0 || line.startsWith(":")) continue;
    const colon = line.indexOf(":");
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "id") id = value;
    else if (field === "data") data.push(value);
  }
  return data.length === 0 ? undefined : { id, data: data.join("\n") };
};
