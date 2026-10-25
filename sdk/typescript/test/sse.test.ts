import { describe, expect, it } from "vitest";

import { createSseReader } from "../src/sse.js";
import type { SseMessage } from "../src/sse.js";
import { bytes, split, streamOf } from "./helpers/host.js";

const readAll = async (chunks: Uint8Array[], max?: number): Promise<SseMessage[]> => {
  const reader = createSseReader(streamOf(chunks), max);
  const out: SseMessage[] = [];
  for (;;) {
    const message = await reader.next();
    if (message === undefined) return out;
    out.push(message);
  }
};

const TEXT = "héllo 世界 😀";
const FRAMES = `id: 0\nevent: token\ndata: {"text":"${TEXT}"}\n\n` + `id: 1\nevent: done\ndata: {"type":"done"}\n\n`;

describe("the SSE reader", () => {
  it("reads a stream delivered one byte at a time, including a split multi-byte character", async () => {
    const data = bytes(FRAMES);
    // The emoji is 4 bytes and the CJK characters 3 bytes each: one-byte chunks split every one of them.
    expect(data.length).toBeGreaterThan(FRAMES.length);
    const messages = await readAll(split(data, 1));
    expect(messages).toEqual([
      { id: "0", event: "token", data: `{"text":"${TEXT}"}` },
      { id: "1", event: "done", data: '{"type":"done"}' },
    ]);
  });

  it("gives the same messages whatever the chunking", async () => {
    const data = bytes(FRAMES);
    const whole = await readAll([data]);
    expect(whole).toHaveLength(2);
    for (const size of [1, 2, 3, 5, 7, 16, 64]) expect(await readAll(split(data, size)), `chunk size ${size}`).toEqual(whole);
  });

  it("accepts CRLF, CR and LF line endings, also when CRLF is split across chunks", async () => {
    for (const eol of ["\r\n", "\r", "\n"]) {
      const text = `id: 0${eol}event: token${eol}data: x${eol}${eol}`;
      const expected = [{ id: "0", event: "token", data: "x" }];
      expect(await readAll([bytes(text)]), JSON.stringify(eol)).toEqual(expected);
      expect(await readAll(split(bytes(text), 1)), `${JSON.stringify(eol)} byte by byte`).toEqual(expected);
    }
  });

  it("joins multi-line data with LF, ignores comments, and strips one optional space after the colon", async () => {
    const text = ": a comment\nevent: m\ndata: one\ndata:two\ndata:  three\n\n";
    expect(await readAll([bytes(text)])).toEqual([{ id: undefined, event: "m", data: "one\ntwo\n three" }]);
  });

  it("names an event 'message' when it has no event field, and ignores retry and unknown fields", async () => {
    expect(await readAll([bytes("retry: 1000\nfoo: bar\ndata: x\n\n")])).toEqual([{ id: undefined, event: "message", data: "x" }]);
  });

  it("dispatches nothing for a frame without data", async () => {
    expect(await readAll([bytes(": keep-alive\n\nevent: only\n\ndata: x\n\n")])).toEqual([{ id: undefined, event: "message", data: "x" }]);
  });

  it("strips a leading byte order mark", async () => {
    expect(await readAll([bytes("﻿data: x\n\n")])).toEqual([{ id: undefined, event: "message", data: "x" }]);
  });

  it("never dispatches an event that was cut off before its blank line", async () => {
    expect(await readAll([bytes("event: token\ndata: complete\n\nevent: token\ndata: cut off")])).toEqual([
      { id: undefined, event: "token", data: "complete" },
    ]);
    expect(await readAll([bytes("data: no blank line yet\n")])).toEqual([]);
  });

  it("refuses invalid UTF-8 instead of replacing it", async () => {
    const invalid = new Uint8Array([0x64, 0x61, 0x74, 0x61, 0x3a, 0x20, 0xff, 0x0a, 0x0a]);
    await expect(readAll([invalid])).rejects.toMatchObject({ code: "internal" });
  });

  it("refuses an event larger than the limit", async () => {
    await expect(readAll([bytes(`data: ${"x".repeat(100)}\n\n`)], 50)).rejects.toMatchObject({ code: "internal" });
    await expect(readAll([bytes("x".repeat(100))], 50)).rejects.toMatchObject({ code: "internal" });
  });

  it("releases the connection on cancel, and cancelling twice is harmless", async () => {
    let cancelled = 0;
    const body = new ReadableStream<Uint8Array>({
      pull: (controller) => controller.enqueue(bytes("data: x\n\n")),
      cancel: () => {
        cancelled += 1;
      },
    });
    const reader = createSseReader(body);
    expect(await reader.next()).toMatchObject({ data: "x" });
    await reader.cancel();
    await reader.cancel();
    expect(cancelled).toBe(1);
    expect(await reader.next()).toBeUndefined();
  });

  it("propagates a read error", async () => {
    const body = new ReadableStream<Uint8Array>({
      pull: () => {
        throw new Error("connection reset");
      },
    });
    await expect(createSseReader(body).next()).rejects.toThrow("connection reset");
  });
});
