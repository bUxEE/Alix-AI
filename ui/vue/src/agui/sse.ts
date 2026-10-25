// A small server-sent-events reader that keeps the `id:` field: the resume cursor of the AlixAI gateway lives there
// (`id: <seq>` sits on the last AG-UI event of each TurnEvent, docs/integration/agui.md section 9). Comments, `event:` and
// `retry:` fields are ignored; a frame without data is skipped; a frame longer than the limit is an error (the stream is
// hostile or broken). The reader is an arrow function with a callback, not a generator, so the kit has no `function` keyword.

export interface SseFrame {
  /** The `id:` field of this frame, when it had one. */
  readonly id: string | undefined;
  /** The `data:` lines joined with a newline. */
  readonly data: string;
}

/** The longest frame accepted. The gateway caps one event at 256 KiB, so this is generous and still bounded. */
export const MAX_FRAME_CHARS = 1024 * 1024;

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

/** Reads the body until it ends or the signal aborts, calling `onFrame` for each frame in order. */
export const readSse = async (body: ReadableStream<Uint8Array>, onFrame: (frame: SseFrame) => void, signal?: AbortSignal): Promise<void> => {
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
        if (frame !== undefined) onFrame(frame);
      }
      if (buffer.length > MAX_FRAME_CHARS) throw new Error("an event frame is too large");
    }
    // Whatever is left never got its blank line: the connection died inside it. It is dropped, as the HTML standard says, so
    // half an event (and the id written before its data) is never delivered; the reconnect replays it whole.
  } finally {
    signal?.removeEventListener("abort", onAbort);
    reader.cancel().catch(() => undefined);
  }
};
