// @vitest-environment jsdom
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { renderToString } from "react-dom/server";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { compile } from "sass";
import { afterEach, describe, expect, it, vi } from "vitest";
import { voice } from "@alixai/sdk";
import { AlixProvider } from "../src/index.js";
import { AlixTranscriptEditor, AlixVoiceButton, AlixVoiceMode, VOICE_MESSAGES } from "../src/voice/index.js";
import type { AlixMicrophone, AlixPlayer } from "../src/voice/index.js";

// Voice Mode against the SDK's own fake voice server: no network, no microphone, no loudspeaker.

afterEach(cleanup);

const id = (prefix: string, n: number): string => `${prefix}_${String(n).padStart(26, "0")}`;
const UTT = id("utt", 1);
const PBK = id("pbk", 1);
const TURN = "turn_1";
const SECRET = `cfm_${"A".repeat(43)}`;
const FORMAT = { encoding: "pcm_s16le" as const, sample_rate: 22050, channels: 1 };
const tick = (ms = 5): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

const fakeMicrophone = () => {
  let push: ((samples: Float32Array, rate: number) => void) | undefined;
  const mic = {
    starts: 0,
    stops: 0,
    start: async (onChunk: (samples: Float32Array, rate: number) => void): Promise<void> => {
      mic.starts += 1;
      push = onChunk;
    },
    stop: (): void => {
      mic.stops += 1;
    },
    speak: (frames = 1600, level = 0.3): void => push?.(new Float32Array(frames).fill(level), 16000),
  };
  return mic satisfies AlixMicrophone & Record<string, unknown>;
};

const fakePlayer = () => {
  const idle = new Set<() => void>();
  const player = {
    enqueued: 0,
    cancels: 0,
    playing: false,
    enqueue: (): void => {
      player.enqueued += 1;
      player.playing = true;
    },
    cancel: (): void => {
      player.cancels += 1;
      const was = player.playing;
      player.playing = false;
      if (was) for (const listener of idle) listener();
    },
    close: (): void => undefined,
    isPlaying: (): boolean => player.playing,
    onIdle: (listener: () => void) => {
      idle.add(listener);
      return () => idle.delete(listener);
    },
  };
  return player satisfies AlixPlayer & Record<string, unknown>;
};

const setup = (fake: Parameters<typeof voice.createFakeVoiceTransport>[0] = {}) => {
  const transport = voice.createFakeVoiceTransport(fake);
  const session = voice.createVoiceSession({
    baseUrl: "http://127.0.0.1:8080",
    sessionId: "ses_1",
    getContextToken: () => "tok",
    webSocket: transport.factory,
    reconnect: { baseDelayMs: 1, maxDelayMs: 2 },
  });
  return { session, transport };
};

const open = async (session: voice.VoiceSession): Promise<void> => {
  await waitFor(() => expect(session.getState().connection).toBe("open"));
};
const talk = (): HTMLElement => document.querySelector('[data-alixai="voice-button"]') as HTMLElement;
const statusWord = (): string => (document.querySelector(".alix-voice__status") as HTMLElement).textContent ?? "";
const send = (transport: ReturnType<typeof setup>["transport"], event: Record<string, unknown>): void => {
  act(() => transport.last().serverSend(event));
};

const mountMode = async (extra: Partial<React.ComponentProps<typeof AlixVoiceMode>> = {}, fake: Parameters<typeof voice.createFakeVoiceTransport>[0] = {}) => {
  const { session, transport } = setup(fake);
  const microphone = fakeMicrophone();
  const player = fakePlayer();
  const view = render(<AlixVoiceMode session={session} microphone={microphone} player={player} {...extra} />);
  await open(session);
  await act(async () => {
    await tick();
  });
  return { view, session, transport, microphone, player };
};

describe("AlixVoiceButton", () => {
  it("reports a release that comes before the host has opened the microphone", () => {
    const onStart = vi.fn();
    const onStop = vi.fn();
    render(<AlixVoiceButton mode="push-to-talk" onStart={onStart} onStop={onStop} />);
    fireEvent.pointerDown(talk());
    fireEvent.pointerUp(talk()); // state is still "idle": the microphone is not open yet
    expect(onStart).toHaveBeenCalledTimes(1);
    expect(onStop).toHaveBeenCalledTimes(1);
    fireEvent.pointerUp(talk());
    expect(onStop).toHaveBeenCalledTimes(1);
  });

  it("releases when it loses focus, cancels on Escape, and lets a screen reader toggle it", () => {
    const onStart = vi.fn();
    const onStop = vi.fn();
    const onCancel = vi.fn();
    const { rerender } = render(<AlixVoiceButton mode="push-to-talk" onStart={onStart} onStop={onStop} onCancel={onCancel} />);
    fireEvent.keyDown(talk(), { key: "Enter" });
    fireEvent.blur(talk());
    expect(onStop).toHaveBeenCalledTimes(1);

    fireEvent.keyDown(talk(), { key: " " });
    fireEvent.keyDown(talk(), { key: "Escape" });
    fireEvent.keyUp(talk(), { key: " " });
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(onStop).toHaveBeenCalledTimes(1);

    fireEvent.click(talk()); // detail 0: no pointer and no key
    expect(onStart).toHaveBeenCalledTimes(3);
    rerender(<AlixVoiceButton mode="push-to-talk" state="listening" onStart={onStart} onStop={onStop} onCancel={onCancel} />);
    expect(talk().getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(talk());
    expect(onStop).toHaveBeenCalledTimes(2);
    expect(talk().getAttribute("aria-keyshortcuts")).toBe("Space Enter");
  });

  it("toggles in toggle mode and ignores a press while unavailable", () => {
    const onStart = vi.fn();
    const onStop = vi.fn();
    const { rerender } = render(<AlixVoiceButton onStart={onStart} onStop={onStop} />);
    fireEvent.click(talk());
    expect(onStart).toHaveBeenCalledTimes(1);
    rerender(<AlixVoiceButton state="listening" onStart={onStart} onStop={onStop} />);
    fireEvent.click(talk());
    expect(onStop).toHaveBeenCalledTimes(1);
    rerender(<AlixVoiceButton state="unavailable" onStart={onStart} onStop={onStop} />);
    fireEvent.click(talk());
    expect(onStart).toHaveBeenCalledTimes(1);
    expect(talk().getAttribute("aria-label")).toBe("Voice is not available");
  });
});

describe("AlixTranscriptEditor", () => {
  it("sends with Ctrl+Enter, discards with Escape and goes back to what was heard", () => {
    const onConfirm = vi.fn();
    const onDiscard = vi.fn();
    render(<AlixTranscriptEditor defaultValue="book dr rosy" original="book dr rosy" onConfirm={onConfirm} onDiscard={onDiscard} />);
    const field = screen.getByRole("textbox") as HTMLTextAreaElement;
    expect(field.className).toContain("no-validation");
    expect(screen.queryByText("Back to what I heard")).toBeNull();
    fireEvent.change(field, { target: { value: "book Dr. Rossi" } });
    expect(screen.getByText("Back to what I heard")).toBeTruthy();
    fireEvent.keyDown(field, { key: "Enter", ctrlKey: true });
    expect(onConfirm).toHaveBeenCalledWith("book Dr. Rossi");
    fireEvent.keyDown(field, { key: "Escape" });
    expect(onDiscard).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByText("Back to what I heard"));
    expect(field.value).toBe("book dr rosy");
  });

  it("does not send empty text or text while disabled, and the first button is Discard", () => {
    const onConfirm = vi.fn();
    const { rerender } = render(<AlixTranscriptEditor defaultValue="   " onConfirm={onConfirm} />);
    fireEvent.submit(screen.getByRole("textbox").closest("form") as HTMLFormElement);
    expect(onConfirm).not.toHaveBeenCalled();
    expect(screen.getAllByRole("button")[0]?.textContent).toBe("Discard");
    rerender(<AlixTranscriptEditor value="hello" disabled onConfirm={onConfirm} />);
    fireEvent.submit(screen.getByRole("textbox").closest("form") as HTMLFormElement);
    expect(onConfirm).not.toHaveBeenCalled();
  });
});

describe("AlixVoiceMode with a voice session", () => {
  it("push to talk: holds the microphone only while the button is held and sends the utterance", async () => {
    const onRecordingChange = vi.fn();
    const { session, transport, microphone } = await mountMode({ onRecordingChange });
    await act(async () => {
      fireEvent.pointerDown(talk());
      await tick();
    });
    expect(microphone.starts).toBe(1);
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start"]);
    act(() => microphone.speak());
    expect(transport.last().audio().length).toBeGreaterThan(0);
    expect(session.getState().recording).toBe("recording");
    expect(statusWord()).toBe("Listening");
    expect(onRecordingChange).toHaveBeenCalledWith(true);

    fireEvent.pointerUp(talk());
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start", "capture.end"]);
    expect(microphone.stops).toBeGreaterThan(0);
    session.close();
  });

  it("a release before the microphone is ready never opens it afterwards", async () => {
    const { session, transport } = setup();
    let openMic: (() => void) | undefined;
    const slow: AlixMicrophone = {
      start: () => new Promise<void>((resolve) => (openMic = resolve)),
      stop: () => undefined,
    };
    render(<AlixVoiceMode session={session} microphone={slow} player={fakePlayer()} />);
    await open(session);
    await act(async () => {
      await tick();
    });
    // Released at once: the microphone is never asked for.
    fireEvent.pointerDown(talk());
    fireEvent.pointerUp(talk());
    await act(async () => {
      await tick();
    });
    expect(openMic).toBeUndefined();
    // Released while the permission prompt is still open: the microphone is closed the moment it opens, and nothing is sent.
    await act(async () => {
      fireEvent.pointerDown(talk());
      await tick();
    });
    expect(openMic).toBeDefined();
    fireEvent.pointerUp(talk());
    await act(async () => {
      openMic?.();
      await tick();
    });
    expect(transport.last().messages().map((m) => m.type)).toEqual([]);
    session.close();
  });

  it("shows the live transcript and the typed reply, and announces only the final transcript", async () => {
    const onTranscript = vi.fn();
    const onResponse = vi.fn();
    const { session, transport, player } = await mountMode({ onTranscript, onResponse });
    send(transport, { type: "transcript.interim", utterance_id: UTT, seq: 1, text: "book tue" });
    expect(screen.getByText("book tue")).toBeTruthy();
    expect(document.querySelector(".alix-voice__sr")?.textContent).toBe("");
    expect(screen.queryByText("Correct what I heard")).toBeNull();

    send(transport, { type: "transcript.final", utterance_id: UTT, text: "book Tuesday" });
    send(transport, { type: "response", utterance_id: UTT, turn_id: TURN, text: "Which doctor?", source: "template", speech: "skipped", speech_reason: "tts_unavailable" });
    expect(document.querySelector(".alix-voice__sr")?.textContent).toBe("Heard: book Tuesday");
    expect(screen.getByText("Which doctor?")).toBeTruthy(); // no speech, still shown
    expect(player.enqueued).toBe(0);
    expect(onTranscript).toHaveBeenLastCalledWith({ text: "book Tuesday", final: true, corrected: false });
    expect(onResponse).toHaveBeenCalledWith({ text: "Which doctor?", spoken: false });
    session.close();
  });

  it("corrects the transcript once, and only when it changed", async () => {
    const onCorrect = vi.fn();
    const { session, transport } = await mountMode({ onCorrect });
    send(transport, { type: "transcript.final", utterance_id: UTT, text: "book thursday" });
    fireEvent.click(screen.getByText("Correct what I heard"));
    const field = screen.getByRole("textbox") as HTMLTextAreaElement;
    expect(field.value).toBe("book thursday");

    fireEvent.submit(field.closest("form") as HTMLFormElement); // unchanged: nothing is sent
    expect(transport.last().messages().some((m) => m.type === "transcript.correct")).toBe(false);
    expect(screen.queryByRole("textbox")).toBeNull();

    fireEvent.click(screen.getByText("Correct what I heard"));
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "  book tuesday " } });
    fireEvent.submit((screen.getByRole("textbox") as HTMLTextAreaElement).closest("form") as HTMLFormElement);
    expect(transport.last().messages().filter((m) => m.type === "transcript.correct")).toEqual([{ type: "transcript.correct", utterance_id: UTT, text: "book tuesday" }]);
    expect(onCorrect).toHaveBeenCalledWith("book tuesday");

    send(transport, { type: "transcript.corrected", utterance_id: UTT, text: "book tuesday", previous_text: "book thursday" });
    expect(screen.getByText("Corrected")).toBeTruthy();
    session.close();
  });

  it("never shows or forwards the confirmation secret, and says confirmation happens on screen", async () => {
    const onTurn = vi.fn();
    const onError = vi.fn();
    const { view, session, transport } = await mountMode({ onTurn, onError });
    send(transport, { type: "transcript.final", utterance_id: UTT, text: "yes confirm it" });
    send(transport, { type: "turn.completed", utterance_id: UTT, turn_id: TURN, act: "confirm", failed: false, preparation_id: "prep_1", confirmation: { preparation_id: "prep_1", token: SECRET } });
    expect(view.container.innerHTML).not.toContain(SECRET);
    expect(JSON.stringify(onTurn.mock.calls)).not.toContain(SECRET);
    expect(onTurn).toHaveBeenLastCalledWith({ turnId: TURN, act: "confirm", failed: false, confirmationRequired: true, preparationId: "prep_1" });
    expect(screen.getByText(/Saying yes does not confirm/)).toBeTruthy();
    // Nothing that was said became a message to the core other than audio and corrections.
    expect(transport.last().messages()).toEqual([]);

    send(transport, { type: "transcript.corrected", utterance_id: UTT, text: "no, tomorrow", previous_text: "yes confirm it" });
    send(transport, { type: "turn.completed", utterance_id: UTT, turn_id: "turn_2", act: "confirm", failed: false, corrects_turn_id: TURN, superseded_preparation_id: "prep_1" });
    expect(onTurn).toHaveBeenLastCalledWith({ turnId: "turn_2", act: "confirm", failed: false, confirmationRequired: false, supersededPreparationId: "prep_1" });
    expect(screen.getByText(/replaces the earlier request/)).toBeTruthy();
    expect(screen.queryByText(/Saying yes does not confirm/)).toBeNull();
    session.close();
  });

  it("plays verified speech, interrupts it at once, and lets a new press barge in", async () => {
    const onInterrupt = vi.fn();
    const { session, transport, player } = await mountMode({ onInterrupt });
    send(transport, { type: "playback.started", playback_id: PBK, turn_id: TURN, format: FORMAT });
    act(() => transport.last().serverAudio(new Uint8Array([1, 0, 2, 0])));
    expect(player.enqueued).toBe(1);
    expect(statusWord()).toBe("Speaking");

    fireEvent.click(screen.getByText("Stop speaking"));
    expect(player.cancels).toBeGreaterThan(0);
    expect(transport.last().messages().at(-1)).toEqual({ type: "playback.cancel" });
    expect(onInterrupt).toHaveBeenCalledTimes(1);
    expect(statusWord()).not.toBe("Speaking");

    send(transport, { type: "playback.cancelled", playback_id: PBK, turn_id: TURN, reason: "client" });
    send(transport, { type: "playback.started", playback_id: id("pbk", 2), turn_id: "turn_2", format: FORMAT });
    act(() => transport.last().serverAudio(new Uint8Array([1, 0])));
    const before = player.cancels;
    await act(async () => {
      fireEvent.pointerDown(talk());
      await tick();
    });
    expect(player.cancels).toBeGreaterThan(before);
    expect(transport.last().messages().some((m) => m.type === "capture.start")).toBe(true);
    session.close();
  });

  it("keeps the speech on screen until the queued audio has drained, not only until the core says done", async () => {
    const { session, transport, player } = await mountMode();
    send(transport, { type: "playback.started", playback_id: PBK, turn_id: TURN, format: FORMAT });
    act(() => transport.last().serverAudio(new Uint8Array([1, 0])));
    send(transport, { type: "playback.done", playback_id: PBK, turn_id: TURN });
    expect(statusWord()).toBe("Speaking");
    act(() => player.cancel()); // drained
    expect(statusWord()).toBe("Ready");
    session.close();
  });

  it("hands free keeps the microphone open between utterances and streams to the endpointer", async () => {
    const { session, transport, microphone } = await mountMode();
    fireEvent.click(screen.getByRole("checkbox"));
    expect(session.getState().mode).toBe("hands_free");
    await act(async () => {
      fireEvent.click(talk());
      await tick();
    });
    expect(talk().getAttribute("aria-pressed")).toBe("true");
    act(() => microphone.speak(1600, 0.01)); // silence: nothing is sent, no capture starts
    expect(transport.last().messages()).toEqual([]);
    act(() => microphone.speak(1600, 0.4));
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start"]);
    expect(statusWord()).toBe("Listening");

    act(() => fireEvent.click(talk())); // stop: the utterance in progress is finished and the microphone closes
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start", "capture.end"]);
    expect(microphone.stops).toBeGreaterThan(0);
    session.close();
  });

  it("falls back to text when the core reports text_input_only, and keeps the typed slot", async () => {
    const onFallback = vi.fn();
    const { session, transport } = await mountMode({ onFallback, renderFallback: (reason) => (reason === undefined ? null : <label>Type here</label>) });
    send(transport, { type: "error", error: { code: "unavailable", message: "no speech recogniser", retryable: false }, effect: "text_input_only" });
    expect(screen.getByText("Voice is not available here. Type your message instead.")).toBeTruthy();
    expect(screen.getByText("Type here")).toBeTruthy();
    expect(talk().getAttribute("aria-disabled")).toBe("true");
    expect(onFallback).toHaveBeenCalledWith("text_input_only");
    session.close();
  });

  it("shows the fallback when the socket cannot be opened, and tries again on request", async () => {
    const { session } = setup({ refuse: (n) => n === 1 });
    render(<AlixVoiceMode session={session} microphone={fakeMicrophone()} player={fakePlayer()} renderFallback={() => <label>Type here</label>} />);
    await waitFor(() => expect(screen.getByText(/Type your message instead/)).toBeTruthy());
    expect(screen.getByText("Type here")).toBeTruthy();
    await act(async () => {
      fireEvent.click(screen.getByText("Try again"));
      await tick(20);
    });
    await waitFor(() => expect(session.getState().connection).toBe("open"));
    await waitFor(() => expect(screen.queryByText(/Type your message instead/)).toBeNull());
    session.close();
  });

  it("asks for a typed message when the microphone is blocked", async () => {
    const onFallback = vi.fn();
    const { session } = setup();
    const blocked: AlixMicrophone = { start: () => Promise.reject(new Error("denied")), stop: () => undefined };
    render(<AlixVoiceMode session={session} microphone={blocked} player={fakePlayer()} onFallback={onFallback} />);
    await open(session);
    await act(async () => {
      await tick();
      fireEvent.pointerDown(talk());
      await tick();
    });
    expect(screen.getByText(/The microphone is blocked or missing/)).toBeTruthy();
    expect(onFallback).toHaveBeenCalledWith("microphone_unavailable");
    expect(talk().getAttribute("aria-disabled")).not.toBe("true");
    session.close();
  });

  it("says when replies are text only (no spoken replies) and still shows them", async () => {
    const { session, transport } = await mountMode({}, { session: { speech: false } });
    send(transport, { type: "response", utterance_id: UTT, turn_id: TURN, text: "Booked for Tuesday.", source: "model", speech: "skipped", speech_reason: "speech_disabled" });
    expect(screen.getByText("Replies appear as text and are not read aloud.")).toBeTruthy();
    expect(screen.getByText("Booked for Tuesday.")).toBeTruthy();
    session.close();
  });

  it("renders the end of the session and hands back a restart", async () => {
    const onRestart = vi.fn();
    const { session, transport } = await mountMode({ onRestart });
    act(() => transport.last().serverClose(1001, "credential_expired"));
    await waitFor(() => expect(transport.connections.length).toBe(2));
    await waitFor(() => expect(session.getState().connection).toBe("open"));
    await waitFor(() => expect(statusWord()).toBe("Ready"));

    act(() => transport.last().serverClose(1000, "replaced"));
    await waitFor(() => expect(screen.getByText("The voice session ended.")).toBeTruthy());
    fireEvent.click(screen.getByText("Start voice again"));
    expect(onRestart).toHaveBeenCalledTimes(1);
    session.close();
  });

  it("releases the microphone and drops a half-said utterance when it goes away", async () => {
    const { view, session, transport, microphone } = await mountMode();
    await act(async () => {
      fireEvent.pointerDown(talk());
      await tick();
    });
    const stopsBefore = microphone.stops;
    view.unmount();
    expect(microphone.stops).toBeGreaterThan(stopsBefore);
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start", "capture.cancel"]);
    session.close();
  });

  it("works without a session: the host drives state, levels, transcript and reply", () => {
    const onStart = vi.fn();
    const onCorrect = vi.fn();
    const { rerender } = render(<AlixVoiceMode state="processing" levels={[0.2, 0.9]} transcript="book dr rosy" reply="Which day?" onStart={onStart} onCorrect={onCorrect} />);
    expect(statusWord()).toBe("Processing");
    expect(document.querySelector(".alix-voice-orb")?.getAttribute("data-state")).toBe("thinking");
    expect(screen.getByText("Which day?")).toBeTruthy();
    rerender(<AlixVoiceMode state="idle" levels={[0.2, 0.9]} transcript="book dr rosy" reply="Which day?" onStart={onStart} onCorrect={onCorrect} />);
    fireEvent.pointerDown(talk());
    expect(onStart).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByText("Correct what I heard"));
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "book Dr. Rossi" } });
    fireEvent.submit((screen.getByRole("textbox") as HTMLTextAreaElement).closest("form") as HTMLFormElement);
    expect(onCorrect).toHaveBeenCalledWith("book Dr. Rossi");
    rerender(<AlixVoiceMode state="unavailable" />);
    expect(screen.getByText(/Type your message instead/)).toBeTruthy();
  });

  it("speaks Italian", () => {
    render(
      <AlixProvider locale="it">
        <AlixVoiceMode state="unavailable" />
      </AlixProvider>,
    );
    expect(screen.getByText(/La voce non è disponibile qui/)).toBeTruthy();
    expect(screen.getByText("Vivavoce")).toBeTruthy();
  });

  it("lets a host reword a word", () => {
    render(<AlixVoiceMode state="idle" messages={{ "voice.end": "Close" }} />);
    expect(screen.getByText("Close")).toBeTruthy();
  });
});

describe("the voice catalogue, the stylesheet and server rendering", () => {
  it("has the same keys and placeholders in English and Italian, with no dashes", () => {
    const en = VOICE_MESSAGES["en"] as Record<string, string>;
    const it = VOICE_MESSAGES["it"] as Record<string, string>;
    expect(Object.keys(it).sort()).toEqual(Object.keys(en).sort());
    const placeholders = (text: string): string[] => [...text.matchAll(/\{(\w+)\}/g)].map((match) => match[1] as string).sort();
    for (const [key, text] of Object.entries(en)) {
      expect(placeholders(it[key] as string), key).toEqual(placeholders(text));
      expect(text + (it[key] as string)).not.toMatch(/—|–|--/);
      expect(text + (it[key] as string)).not.toMatch(/\p{Extended_Pictographic}/u);
    }
  });

  it("compiles with Sass, reads only tokens, honours reduced motion and uses @use only", () => {
    const path = join(process.cwd(), "src", "voice", "_voice.scss"); // vitest runs inside the package
    expect(readFileSync(path, "utf8")).not.toMatch(/@import/);
    const css = compile(path).css;
    expect(css).toContain(".alix-voice__talk");
    expect(css).not.toMatch(/oklch\(|#[0-9a-fA-F]{3,8}\b|\brgba?\(/);
    expect(css).not.toMatch(/transition:\s*all/);
    expect(css).toContain("prefers-reduced-motion: reduce");
    expect(css).toContain("(hover: hover) and (pointer: fine)");
    expect(css).not.toMatch(/border-(left|right):\s*[2-9]px/);
  });

  it("renders on a server without touching the browser", () => {
    const html = renderToString(<AlixVoiceMode state="idle" />);
    expect(html).toContain("Voice mode");
  });
});
