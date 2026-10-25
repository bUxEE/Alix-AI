import { describe, expect, it } from "vitest";

import { createVoiceSession, float32ToPcm16, parseServerEvent, resampleFloat32 } from "../src/voice/index.js";
import type { VoiceEvent, VoiceOptions } from "../src/voice/index.js";
import { createFakeVoiceTransport, fakeVoiceId } from "../src/voice/testing.js";
import type { FakeVoiceOptions } from "../src/voice/testing.js";

const tick = (ms = 5): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));
const until = async (condition: () => boolean, ms = 2000): Promise<void> => {
  const end = Date.now() + ms;
  while (!condition() && Date.now() < end) await tick(5);
  if (!condition()) throw new Error("the condition did not become true");
};
const UTT = fakeVoiceId("utt", 1);
const UTT2 = fakeVoiceId("utt", 2);
const PBK = fakeVoiceId("pbk", 1);
const TURN = "turn_1";
const SECRET = `cfm_${"A".repeat(43)}`;
const FORMAT_22K = { encoding: "pcm_s16le", sample_rate: 22050, channels: 1 };

const setup = (opts: Partial<VoiceOptions> = {}, fake: FakeVoiceOptions = {}) => {
  const transport = createFakeVoiceTransport(fake);
  let n = 0;
  const tokens: string[] = [];
  const session = createVoiceSession({
    baseUrl: "http://127.0.0.1:8080",
    sessionId: "ses_1",
    getContextToken: () => {
      n += 1;
      tokens.push(`tok${n}`);
      return `tok${n}`;
    },
    webSocket: transport.factory,
    reconnect: { baseDelayMs: 1, maxDelayMs: 2 },
    ...opts,
  });
  const events: VoiceEvent[] = [];
  session.on((e) => events.push(e));
  return { session, transport, events, tokens };
};
const pcm = (samples: number, value = 0): Int16Array => new Int16Array(samples).fill(value);
const notices = (events: VoiceEvent[], code: string): number => events.filter((e) => e.type === "notice" && e.code === code).length;

describe("opening", () => {
  it("opens the voice socket with the token in the header only, no subprotocol, and resolves session.started", async () => {
    const { session, transport } = setup({ language: "it", speak: false });
    const info = await session.connect();
    expect(info.session_id).toBe("ses_fake");
    const c = transport.last();
    expect(c.url).toBe("ws://127.0.0.1:8080/v1/sessions/ses_1/voice?language=it&speak=false");
    expect(c.headers).toEqual({ Authorization: "Bearer tok1" });
    expect(c.protocols).toEqual([]);
    expect(c.url).not.toContain("tok1");
    expect(session.getState().connection).toBe("open");
    session.close();
  });

  it("refuses bad arguments before a token is spent and fails closed without a socket factory", async () => {
    let minted = 0;
    const base = {
      baseUrl: "http://127.0.0.1:8080",
      sessionId: "ses_1",
      getContextToken: () => {
        minted += 1;
        return "t";
      },
    };
    expect(() => createVoiceSession({ ...base, language: "not a tag" })).toThrow();
    expect(() => createVoiceSession({ ...base, sessionId: ".." })).toThrow();
    expect(() => createVoiceSession({ ...base, baseUrl: "http://example.com" })).toThrow();
    expect(() => createVoiceSession({ ...base, mode: "duplex" as never })).toThrow();
    const noFactory = createVoiceSession(base);
    await expect(noFactory.connect()).rejects.toMatchObject({ code: "unavailable", retryable: false });
    expect(minted).toBe(0);
  });

  it("reports a refused upgrade as a typed error and may try again", async () => {
    const { session } = setup({}, { refuse: (n) => n === 1 });
    await expect(session.connect()).rejects.toMatchObject({ code: "unavailable" });
    expect(session.getState().connection).toBe("idle");
    await expect(session.connect()).resolves.toBeDefined();
    session.close();
  });

  it("offers no way to confirm, reject or choose", () => {
    const { session } = setup();
    expect(Object.keys(session).sort()).toEqual(
      ["cancelCapture", "cancelPlayback", "close", "connect", "correctTranscript", "endCapture", "getState", "on", "pushAudio", "setMode", "startCapture", "submitAudio"].sort(),
    );
  });
});

describe("push to talk", () => {
  it("streams a capture in chunks no larger than max_chunk_bytes and ends it", async () => {
    const { session, transport } = setup({}, { session: { max_chunk_bytes: 1000 } });
    await session.connect();
    session.startCapture();
    expect(session.getState().recording).toBe("recording");
    expect(session.pushAudio(pcm(1200))).toBe(true); // 2400 bytes
    session.endCapture();
    const c = transport.last();
    expect(c.messages().map((m) => m.type)).toEqual(["capture.start", "capture.end"]);
    expect(c.messages()[0]).toEqual({ type: "capture.start", format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1 } });
    expect(c.audio().map((a) => a.length)).toEqual([1000, 1000, 400]);
    expect(session.getState().recording).toBe("processing");
    session.close();
  });

  it("drops audio when no capture is active, and refuses half samples", async () => {
    const { session, transport } = setup();
    await session.connect();
    expect(session.pushAudio(pcm(10))).toBe(false);
    expect(transport.last().audio()).toEqual([]);
    session.startCapture();
    expect(() => session.pushAudio(new Uint8Array(3))).toThrow();
    expect(() => session.startCapture()).toThrow(/already/);
    session.close();
  });

  it("ends a capture that would pass max_audio_bytes and says so", async () => {
    const { session, transport, events } = setup({}, { session: { max_audio_bytes: 1000 } });
    await session.connect();
    session.startCapture();
    expect(session.pushAudio(pcm(400))).toBe(true);
    expect(session.pushAudio(pcm(400))).toBe(false);
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start", "capture.end"]);
    expect(notices(events, "capture_limit")).toBe(1);
    session.close();
  });

  it("walks recording state, transcripts, turn and response from the events of the core", async () => {
    const { session, transport, events } = setup();
    await session.connect();
    session.startCapture({ language: "en" });
    const c = transport.last();
    c.serverSend({ type: "capture.started", utterance_id: UTT });
    expect(session.getState().utteranceId).toBe(UTT);
    session.endCapture();
    c.serverSend({ type: "transcript.interim", utterance_id: UTT, seq: 2, text: "book tue" });
    c.serverSend({ type: "transcript.interim", utterance_id: UTT, seq: 1, text: "book" }); // backwards: dropped
    expect(session.getState().transcript).toMatchObject({ text: "book tue", interim: true });
    c.serverSend({ type: "transcript.final", utterance_id: UTT, text: "book Tuesday", language: "en" });
    expect(session.getState()).toMatchObject({ recording: "idle", turn: "running", transcript: { text: "book Tuesday", interim: false } });
    c.serverSend({ type: "transcript.interim", utterance_id: UTT, seq: 9, text: "late" }); // after final: dropped
    c.serverSend({
      type: "turn.completed",
      utterance_id: UTT,
      turn_id: TURN,
      act: "confirm",
      failed: false,
      preparation_id: "prep_1",
      confirmation: { preparation_id: "prep_1", token: SECRET },
    });
    c.serverSend({ type: "response", utterance_id: UTT, turn_id: TURN, text: "Shall I book it?", source: "model", speech: "skipped", speech_reason: "speech_disabled" });
    expect(session.getState()).toMatchObject({ turn: "idle", response: { text: "Shall I book it?", speechReason: "speech_disabled" } });
    // the secret reaches the host's listener, and is nowhere in the state
    expect(events.find((e) => e.type === "turn.completed")).toMatchObject({ confirmation: { token: SECRET } });
    expect(JSON.stringify(session.getState())).not.toContain(SECRET);
    expect(events.filter((e) => e.type === "transcript.interim")).toHaveLength(1);
    session.close();
  });

  it("ignores messages that do not match the schema, with a notice", async () => {
    const { session, transport, events } = setup();
    await session.connect();
    const c = transport.last();
    c.serverSend("{ not json");
    c.serverSend({ type: "transcript.final", utterance_id: "x", text: "hi" });
    c.serverSend({ type: "mystery" });
    c.serverSend({
      type: "turn.completed",
      utterance_id: UTT,
      turn_id: TURN,
      act: "a",
      failed: false,
      preparation_id: "p1",
      confirmation: { preparation_id: "p2", token: SECRET },
    });
    expect(notices(events, "malformed_event")).toBe(4);
    expect(session.getState().turn).toBe("idle");
    session.close();
  });
});

describe("correction", () => {
  it("sends the correction for the latest final utterance, and refuses nonsense locally", async () => {
    const { session, transport } = setup();
    await session.connect();
    expect(() => session.correctTranscript("x")).toThrow(/no utterance/);
    transport.last().serverSend({ type: "transcript.final", utterance_id: UTT, text: "book thursday" });
    expect(() => session.correctTranscript("  ")).toThrow();
    expect(() => session.correctTranscript("x".repeat(2001))).toThrow();
    session.correctTranscript("book tuesday");
    expect(transport.last().messages().pop()).toEqual({ type: "transcript.correct", utterance_id: UTT, text: "book tuesday" });
    transport.last().serverSend({ type: "transcript.corrected", utterance_id: UTT, text: "book tuesday", previous_text: "book thursday" });
    expect(session.getState().transcript).toMatchObject({ corrected: true, text: "book tuesday" });
    session.close();
  });
});

describe("playback", () => {
  it("delivers verified speech while playing and nothing after a local cancel", async () => {
    const { session, transport, events } = setup();
    await session.connect();
    const c = transport.last();
    c.serverSend({ type: "playback.started", playback_id: PBK, turn_id: TURN, format: FORMAT_22K });
    expect(session.getState()).toMatchObject({ playback: "playing", playbackTurnId: TURN });
    c.serverAudio(new Uint8Array([1, 2, 3, 4]));
    expect(events.filter((e) => e.type === "audio.out")).toHaveLength(1);
    session.cancelPlayback();
    expect(session.getState().playback).toBe("idle");
    expect(c.messages().pop()).toEqual({ type: "playback.cancel" });
    c.serverAudio(new Uint8Array([5, 6])); // still on the wire
    expect(events.filter((e) => e.type === "audio.out")).toHaveLength(1);
    c.serverSend({ type: "playback.cancelled", playback_id: PBK, turn_id: TURN, reason: "client" });
    c.serverAudio(new Uint8Array([7, 8])); // outside a playback: dropped
    expect(events.filter((e) => e.type === "audio.out")).toHaveLength(1);
    expect(notices(events, "audio_dropped")).toBe(1);
    session.close();
  });

  it("a capture that starts while speech plays stops delivering it (barge-in)", async () => {
    const { session, transport, events } = setup();
    await session.connect();
    const c = transport.last();
    c.serverSend({ type: "playback.started", playback_id: PBK, turn_id: TURN, format: FORMAT_22K });
    session.startCapture();
    expect(session.getState().playback).toBe("idle");
    c.serverAudio(new Uint8Array([1, 2]));
    expect(events.filter((e) => e.type === "audio.out")).toHaveLength(0);
    session.close();
  });
});

describe("hands free", () => {
  const loud = (): Int16Array => pcm(1600, 8000); // 100 ms
  const quiet = (): Int16Array => pcm(1600, 0);

  it("starts an utterance on speech with its pre-roll, ends it on silence, and sends nothing in between", async () => {
    const { session, transport } = setup({ mode: "hands_free", endpointer: { silenceMs: 300, preRollMs: 200, minSpeechMs: 100 } });
    await session.connect();
    expect(session.pushAudio(quiet())).toBe(false);
    expect(session.pushAudio(quiet())).toBe(false);
    expect(transport.last().messages()).toEqual([]);
    expect(session.pushAudio(loud())).toBe(true);
    expect(session.getState().recording).toBe("recording");
    expect(transport.last().audio().length).toBeGreaterThan(1); // the pre-roll went with it
    session.pushAudio(loud());
    session.pushAudio(quiet());
    session.pushAudio(quiet());
    expect(session.getState().recording).toBe("recording");
    session.pushAudio(quiet());
    expect(session.getState().recording).toBe("processing");
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start", "capture.end"]);
    session.close();
  });

  it("does not take the loudspeaker for the user unless barge-in is on", async () => {
    const { session, transport } = setup({ mode: "hands_free" });
    await session.connect();
    transport.last().serverSend({ type: "playback.started", playback_id: PBK, turn_id: TURN, format: FORMAT_22K });
    expect(session.pushAudio(loud())).toBe(false);
    expect(session.getState().recording).toBe("idle");
    session.close();
  });
});

describe("submit", () => {
  it("sends a whole utterance as base64 and refuses one that is too long", async () => {
    const { session, transport } = setup({}, { session: { max_audio_bytes: 100 } });
    await session.connect();
    session.submitAudio(new Uint8Array([1, 0, 2, 0]), { language: "it" });
    expect(transport.last().messages()[0]).toEqual({
      type: "audio.submit",
      format: { encoding: "pcm_s16le", sample_rate: 16000, channels: 1 },
      language: "it",
      audio: "AQACAA==",
    });
    expect(session.getState().recording).toBe("processing");
    expect(() => session.submitAudio(new Uint8Array(102))).toThrow(/longer/);
    session.close();
  });
});

describe("reconnecting", () => {
  it("opens a new connection with a fresh token when the credential expires, and ends what was live", async () => {
    const { session, transport, events, tokens } = setup();
    await session.connect();
    session.startCapture();
    transport.last().serverClose(1001, "credential_expired");
    await until(() => session.getState().connection === "open" && transport.connections.length === 2);
    expect(tokens).toEqual(["tok1", "tok2"]);
    expect(transport.connections).toHaveLength(2);
    expect(transport.connections[1]?.headers["Authorization"]).toBe("Bearer tok2");
    expect(session.getState()).toMatchObject({ connection: "open", recording: "idle", reconnectAttempt: 0 });
    expect(notices(events, "connection_lost")).toBe(1);
    expect(() => session.endCapture()).toThrow(); // the old capture is gone
    session.close();
  });

  it("does not reopen a connection that another one replaced", async () => {
    const { session, transport, events } = setup();
    await session.connect();
    transport.last().serverClose(1001, "replaced");
    await tick(20);
    expect(transport.connections).toHaveLength(1);
    expect(session.getState().connection).toBe("closed");
    expect(notices(events, "replaced")).toBe(1);
  });

  it("does not reopen after a protocol violation, a normal close or an oversize message", async () => {
    for (const [code, reason] of [[1008, "protocol_violation"], [1000, "client_closed"], [1009, ""]] as const) {
      const { session, transport } = setup();
      await session.connect();
      transport.last().serverClose(code, reason);
      await tick(10);
      expect(transport.connections).toHaveLength(1);
      expect(session.getState().connection).toBe("closed");
    }
  });

  it("gives up after a bounded number of attempts, each with its own token", async () => {
    const { session, transport, events, tokens } = setup({ reconnect: { maxAttempts: 2, baseDelayMs: 1 } }, { refuse: (n) => n > 1 });
    await session.connect();
    transport.last().serverClose(1006, "");
    await until(() => session.getState().connection === "closed");
    expect(tokens).toHaveLength(3);
    expect(session.getState()).toMatchObject({ connection: "closed", lastError: { code: "unavailable" } });
    expect(notices(events, "reconnect_failed")).toBe(1);
  });

  it("says it cannot send while reconnecting", async () => {
    const { session, transport } = setup({ reconnect: { baseDelayMs: 30, maxDelayMs: 30 } });
    await session.connect();
    transport.last().serverClose(1006, "");
    expect(session.getState().connection).toBe("reconnecting");
    expect(() => session.startCapture()).toThrow(/re-established/);
    session.close();
    expect(session.getState().connection).toBe("closed");
  });
});

describe("helpers", () => {
  it("converts and resamples audio", () => {
    expect(Array.from(float32ToPcm16(new Float32Array([0, 1, -1, 2])))).toEqual([0, 32767, -32768, 32767]);
    expect(resampleFloat32(new Float32Array(480).fill(0.5), 48000)).toHaveLength(160);
  });

  it("parses the events of the schema and nothing else", () => {
    expect(parseServerEvent(JSON.stringify({ type: "capture.started", utterance_id: UTT2 }))).toBeDefined();
    expect(parseServerEvent(JSON.stringify({ type: "capture.started", utterance_id: "utt_short" }))).toBeUndefined();
    expect(parseServerEvent("[]")).toBeUndefined();
  });
});
