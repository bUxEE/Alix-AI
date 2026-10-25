import { voice } from "@alixai/sdk";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import WebSocket from "ws";
import { createAlixVoiceController } from "../../vue/src/voice/controller";
import type { AlixMicrophone, AlixPlayer } from "../../vue/src/voice/types";
import { voicePath } from "../src/index";
import { expectSchema, SCHEMA, startMock, uid, type RunningMock } from "./helpers";

// The voice socket, driven by the SDK's own voice client (which drops any event that does not match the schema with a
// `malformed_event` notice) and by the kits' voice controller, over a real WebSocket on the Vite dev server.

let mock: RunningMock;
beforeAll(async () => {
  mock = await startMock({ pace: 0 });
});
afterAll(async () => {
  await mock.close();
});

const factory: voice.VoiceSocketFactory = (url, init) => new WebSocket(url, init.protocols, { headers: init.headers }) as unknown as voice.VoiceSocketLike;

const open = (options: Partial<voice.VoiceOptions> = {}) => {
  const session = voice.createVoiceSession({ baseUrl: mock.base, sessionId: uid("ses_voice"), getContextToken: () => "demo-context-token", webSocket: factory, ...options });
  const events: voice.VoiceEvent[] = [];
  session.on((event) => events.push(event));
  return { session, events };
};

const wire = (events: readonly voice.VoiceEvent[]): voice.VoiceEvent[] =>
  events.filter((event) => !["state", "audio.out", "input.level", "notice"].includes(event.type));

const speakOneUtterance = async (session: voice.VoiceSession, events: voice.VoiceEvent[], untilType: string): Promise<void> => {
  const before = events.length;
  session.startCapture();
  for (let i = 0; i < 10; i += 1) session.pushAudio(new Int16Array(1600));
  session.endCapture();
  await vi.waitFor(() => expect(events.slice(before).some((event) => event.type === untilType)).toBe(true), { timeout: 5000 });
};

describe("the voice socket", () => {
  it("answers an utterance with transcripts, the turn, the reply and its speech, in the core's order", async () => {
    const { session, events } = open({ language: "en" });
    const started = await session.connect();
    expect(started.speech).toBe(true);
    await speakOneUtterance(session, events, "playback.done");
    const types = wire(events).map((event) => event.type);
    expect(types).toEqual([
      "session.started",
      "capture.started",
      "transcript.interim",
      "transcript.interim",
      "transcript.final",
      "turn.completed",
      "response",
      "playback.started",
      "playback.done",
    ]);
    for (const event of wire(events)) expectSchema(SCHEMA.serverEvent, event);
    expect(events.some((event) => event.type === "notice")).toBe(false);
    const audio = events.filter((event): event is Extract<voice.VoiceEvent, { type: "audio.out" }> => event.type === "audio.out");
    expect(audio.length).toBeGreaterThan(10);
    expect(audio[0]?.format).toEqual({ encoding: "pcm_s16le", sample_rate: 16000, channels: 1 });
    expect(session.getState().response?.text).toMatch(/WO-2291/);
    expect(session.getState().transcript).toMatchObject({ text: "What's the status of work order WO-2291?", interim: false });
    session.close();
  });

  it("prepares an action on the second utterance and supersedes it on a correction", async () => {
    const { session, events } = open({ language: "it", speak: false });
    const started = await session.connect();
    expect(started.speech).toBe(false);
    await speakOneUtterance(session, events, "response");
    await speakOneUtterance(session, events, "response");
    const turns = events.filter((event): event is voice.TurnCompleted => event.type === "turn.completed");
    expect(turns[1]?.act).toBe("request_confirmation");
    expect(turns[1]?.confirmation?.token).toMatch(/^cfm_[A-Za-z0-9_-]{43}$/);
    const reply = events.filter((event): event is voice.VoiceResponseEvent => event.type === "response")[1];
    expect(reply).toMatchObject({ speech: "skipped", speech_reason: "speech_disabled" });
    expect(reply?.text).toContain("Riverside");
    expect(events.some((event) => event.type === "playback.started")).toBe(false);
    session.correctTranscript("Prenota un'ispezione antincendio a Riverside per mercoledì");
    await vi.waitFor(() => expect(events.filter((event) => event.type === "turn.completed")).toHaveLength(3), { timeout: 5000 });
    const corrected = events.filter((event): event is voice.TurnCompleted => event.type === "turn.completed")[2];
    expect(corrected?.corrects_turn_id).toBe(turns[1]?.turn_id);
    expect(corrected?.superseded_preparation_id).toBe(turns[1]?.preparation_id);
    for (const event of wire(events)) expectSchema(SCHEMA.serverEvent, event);
    session.close();
  });

  it("drives the kits' voice controller from microphone to reply", async () => {
    const { session } = open();
    let onChunk: ((samples: Float32Array, rate: number) => void) | undefined;
    const microphone: AlixMicrophone = {
      start: async (chunk) => {
        onChunk = chunk;
      },
      stop: () => {
        onChunk = undefined;
      },
    };
    const played: Uint8Array[] = [];
    const idle = new Set<() => void>();
    const player: AlixPlayer = {
      enqueue: (_format, data) => void played.push(data),
      cancel: () => undefined,
      close: () => undefined,
      isPlaying: () => false,
      onIdle: (listener) => {
        idle.add(listener);
        return () => idle.delete(listener);
      },
    };
    const controller = createAlixVoiceController({ session, microphone, player, language: "en" });
    await vi.waitFor(() => expect(controller.getView().phase).toBe("idle"), { timeout: 5000 });
    await controller.start();
    expect(controller.getView().phase).toBe("listening");
    for (let i = 0; i < 8; i += 1) onChunk?.(new Float32Array(4800).fill(0.05), 48000);
    controller.stop();
    await vi.waitFor(() => expect(controller.getView().reply?.text).toMatch(/WO-2291/), { timeout: 5000 });
    expect(controller.getView().transcript).toMatchObject({ interim: false, corrected: false });
    await vi.waitFor(() => expect(played.length).toBeGreaterThan(5), { timeout: 5000 });
    controller.destroy();
    session.close();
  });

  it("leaves every other upgrade alone", async () => {
    const socket = new WebSocket(`${mock.base.replace("http", "ws")}/v1/sessions/bad id/voice`);
    const outcome = await new Promise<string>((resolve) => {
      socket.on("open", () => resolve("open"));
      socket.on("error", () => resolve("error"));
      setTimeout(() => resolve("timeout"), 1500);
    });
    expect(outcome).not.toBe("open");
    socket.terminate();
    expect(voicePath("ses_demo_welcome")).toBe("/v1/sessions/ses_demo_welcome/voice");
  });
});
