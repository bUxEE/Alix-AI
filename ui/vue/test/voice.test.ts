import { voice } from "@alixai/sdk";
import { flushPromises, mount } from "@vue/test-utils";
import { createSSRApp, h } from "vue";
import { renderToString } from "vue/server-renderer";
import { describe, expect, it } from "vitest";
import { AlixProvider, AlixTranscriptEditor, AlixVoiceButton, AlixVoiceMode } from "../src/index";
import { VOICE_MESSAGES } from "../src/voice";
import type { AlixMicrophone, AlixPlayer } from "../src/voice";

// Voice Mode against the SDK's own fake voice server: no network, no microphone, no loudspeaker.

const id = (prefix: string, n: number): string => `${prefix}_${String(n).padStart(26, "0")}`;
const UTT = id("utt", 1);
const PBK = id("pbk", 1);
const TURN = "turn_1";
const SECRET = `cfm_${"A".repeat(43)}`;
const FORMAT = { encoding: "pcm_s16le" as const, sample_rate: 22050, channels: 1 };

const tick = (ms = 5): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));
const until = async (condition: () => boolean, ms = 2000): Promise<void> => {
  const end = Date.now() + ms;
  while (!condition() && Date.now() < end) await tick();
  if (!condition()) throw new Error("the condition did not become true");
};

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

const mountMode = async (extra: Record<string, unknown> = {}, fake: Parameters<typeof voice.createFakeVoiceTransport>[0] = {}) => {
  const { session, transport } = setup(fake);
  const microphone = fakeMicrophone();
  const player = fakePlayer();
  const wrapper = mount(AlixVoiceMode, { props: { session, microphone, player, ...extra }, attachTo: document.body });
  await flushPromises();
  await until(() => session.getState().connection === "open");
  await flushPromises();
  return { wrapper, session, transport, microphone, player, talk: () => wrapper.find("button[aria-label]") };
};

describe("AlixVoiceButton", () => {
  it("reports a release that comes before the host has opened the microphone", async () => {
    const hold = mount(AlixVoiceButton, { props: { mode: "push-to-talk" } });
    await hold.trigger("pointerdown");
    await hold.trigger("pointerup"); // state is still "idle": the microphone is not open yet
    expect(hold.emitted("start")).toHaveLength(1);
    expect(hold.emitted("stop")).toHaveLength(1);
    await hold.trigger("pointerup");
    expect(hold.emitted("stop")).toHaveLength(1);
  });

  it("releases when it loses focus, cancels on Escape, and lets a screen reader toggle it", async () => {
    const hold = mount(AlixVoiceButton, { props: { mode: "push-to-talk" } });
    await hold.trigger("keydown", { key: "Enter" });
    await hold.trigger("blur");
    expect(hold.emitted("stop")).toHaveLength(1);

    await hold.trigger("keydown", { key: " " });
    await hold.trigger("keydown", { key: "Escape" });
    await hold.trigger("keyup", { key: " " });
    expect(hold.emitted("cancel")).toHaveLength(1);
    expect(hold.emitted("stop")).toHaveLength(1);

    const assistive = mount(AlixVoiceButton, { props: { mode: "push-to-talk" } });
    await assistive.trigger("click"); // detail 0: no pointer and no key
    expect(assistive.emitted("start")).toHaveLength(1);
    await assistive.setProps({ state: "listening" });
    await assistive.trigger("click");
    expect(assistive.emitted("stop")).toHaveLength(1);
    expect(assistive.attributes("aria-keyshortcuts")).toBe("Space Enter");
  });
});

describe("AlixTranscriptEditor", () => {
  it("sends with Ctrl+Enter, discards with Escape and goes back to what was heard", async () => {
    const wrapper = mount(AlixTranscriptEditor, { props: { modelValue: "book dr rosy", original: "book dr rosy", corrected: false } });
    const field = wrapper.find("textarea");
    expect(field.classes()).toContain("no-validation");
    expect(wrapper.text()).not.toContain("Back to what I heard");
    await field.setValue("book Dr. Rossi");
    await wrapper.setProps({ modelValue: "book Dr. Rossi" });
    expect(wrapper.text()).toContain("Back to what I heard");
    await field.trigger("keydown", { key: "Enter", ctrlKey: true });
    expect(wrapper.emitted("confirm")).toEqual([["book Dr. Rossi"]]);
    await field.trigger("keydown", { key: "Escape" });
    expect(wrapper.emitted("discard")).toHaveLength(1);
    await wrapper.findAll("button")[0]?.trigger("click");
    expect(wrapper.emitted("update:modelValue")?.pop()).toEqual(["book dr rosy"]);
  });

  it("does not send empty text or text while disabled", async () => {
    const wrapper = mount(AlixTranscriptEditor, { props: { modelValue: "   " } });
    await wrapper.find("form").trigger("submit");
    expect(wrapper.emitted("confirm")).toBeUndefined();
    const off = mount(AlixTranscriptEditor, { props: { modelValue: "hello", disabled: true } });
    await off.find("form").trigger("submit");
    expect(off.emitted("confirm")).toBeUndefined();
  });
});

describe("AlixVoiceMode with a voice session", () => {
  it("push to talk: holds the microphone only while the button is held and sends the utterance", async () => {
    const { wrapper, session, transport, microphone, talk } = await mountMode();
    await talk().trigger("pointerdown");
    await flushPromises();
    expect(microphone.starts).toBe(1);
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start"]);
    microphone.speak();
    expect(transport.last().audio().length).toBeGreaterThan(0);
    expect(session.getState().recording).toBe("recording");
    expect(wrapper.find("[role=status]").text()).toBe("Listening");
    expect(wrapper.emitted("recording-change")?.[0]).toEqual([true]);

    await talk().trigger("pointerup");
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start", "capture.end"]);
    expect(microphone.stops).toBeGreaterThan(0);
    wrapper.unmount();
    session.close();
  });

  it("a release before the microphone is ready never opens it afterwards", async () => {
    const { wrapper, session, transport } = await mountMode();
    let open!: () => void;
    const slow: AlixMicrophone & { stops: number } = {
      stops: 0,
      start: () => new Promise<void>((resolve) => (open = resolve)),
      stop: () => {
        slow.stops += 1;
      },
    };
    await wrapper.setProps({ microphone: slow });
    await flushPromises();
    await until(() => session.getState().connection === "open");
    const button = wrapper.find("button[aria-label]");
    await button.trigger("pointerdown");
    await button.trigger("pointerup");
    open();
    await flushPromises();
    expect(transport.last().messages().map((m) => m.type)).toEqual([]);
    wrapper.unmount();
    session.close();
  });

  it("shows the live transcript and the typed reply, and announces only the final transcript", async () => {
    const { wrapper, session, transport, player } = await mountMode();
    const c = transport.last();
    c.serverSend({ type: "transcript.interim", utterance_id: UTT, seq: 1, text: "book tue" });
    await flushPromises();
    expect(wrapper.text()).toContain("book tue");
    expect(wrapper.find(".alix-sr-only[aria-live]").text()).toBe("");
    expect(wrapper.text()).not.toContain("Correct what I heard");

    c.serverSend({ type: "transcript.final", utterance_id: UTT, text: "book Tuesday" });
    c.serverSend({ type: "response", utterance_id: UTT, turn_id: TURN, text: "Which doctor?", source: "template", speech: "skipped", speech_reason: "tts_unavailable" });
    await flushPromises();
    expect(wrapper.find(".alix-sr-only[aria-live]").text()).toBe("Heard: book Tuesday");
    expect(wrapper.text()).toContain("Which doctor?"); // no speech, still shown
    expect(player.enqueued).toBe(0);
    expect(wrapper.emitted("transcript")?.at(-1)).toEqual([{ text: "book Tuesday", final: true, corrected: false }]);
    expect(wrapper.emitted("response")?.[0]).toEqual([{ text: "Which doctor?", spoken: false }]);
    wrapper.unmount();
    session.close();
  });

  it("corrects the transcript once, and only when it changed", async () => {
    const { wrapper, session, transport } = await mountMode();
    const c = transport.last();
    c.serverSend({ type: "transcript.final", utterance_id: UTT, text: "book thursday" });
    await flushPromises();
    const edit = wrapper.findAll("button").find((button) => button.text() === "Correct what I heard");
    await edit?.trigger("click");
    await flushPromises();
    const field = wrapper.find("textarea");
    expect((field.element as HTMLTextAreaElement).value).toBe("book thursday");

    await wrapper.find("form").trigger("submit"); // unchanged: nothing is sent
    expect(c.messages().some((m) => m.type === "transcript.correct")).toBe(false);
    expect(wrapper.find("textarea").exists()).toBe(false);

    await edit?.trigger("click");
    await wrapper.findAll("button").find((button) => button.text() === "Correct what I heard")?.trigger("click");
    await flushPromises();
    await wrapper.find("textarea").setValue("  book tuesday ");
    await wrapper.find("form").trigger("submit");
    expect(c.messages().filter((m) => m.type === "transcript.correct")).toEqual([{ type: "transcript.correct", utterance_id: UTT, text: "book tuesday" }]);
    expect(wrapper.emitted("correct")).toEqual([["book tuesday"]]);

    c.serverSend({ type: "transcript.corrected", utterance_id: UTT, text: "book tuesday", previous_text: "book thursday" });
    await flushPromises();
    expect(wrapper.text()).toContain("Corrected");
    wrapper.unmount();
    session.close();
  });

  it("never shows or forwards the confirmation secret, and says confirmation happens on screen", async () => {
    const { wrapper, session, transport } = await mountMode();
    const c = transport.last();
    c.serverSend({ type: "transcript.final", utterance_id: UTT, text: "yes confirm it" });
    c.serverSend({ type: "turn.completed", utterance_id: UTT, turn_id: TURN, act: "confirm", failed: false, preparation_id: "prep_1", confirmation: { preparation_id: "prep_1", token: SECRET } });
    await flushPromises();
    expect(wrapper.html()).not.toContain(SECRET);
    expect(JSON.stringify(wrapper.emitted())).not.toContain(SECRET);
    expect(wrapper.emitted("turn")?.[0]).toEqual([{ turnId: TURN, act: "confirm", failed: false, confirmationRequired: true, preparationId: "prep_1" }]);
    expect(wrapper.text()).toContain("Saying yes does not confirm");
    // Nothing that was said became a message to the core other than audio and corrections.
    expect(c.messages()).toEqual([]);

    c.serverSend({ type: "transcript.corrected", utterance_id: UTT, text: "no, tomorrow", previous_text: "yes confirm it" });
    c.serverSend({ type: "turn.completed", utterance_id: UTT, turn_id: "turn_2", act: "confirm", failed: false, corrects_turn_id: TURN, superseded_preparation_id: "prep_1" });
    await flushPromises();
    expect(wrapper.emitted("turn")?.[1]).toEqual([{ turnId: "turn_2", act: "confirm", failed: false, confirmationRequired: false, supersededPreparationId: "prep_1" }]);
    expect(wrapper.text()).toContain("replaces the earlier request");
    expect(wrapper.text()).not.toContain("Saying yes does not confirm");
    wrapper.unmount();
    session.close();
  });

  it("plays verified speech, interrupts it at once, and lets a new press barge in", async () => {
    const { wrapper, session, transport, player, talk } = await mountMode();
    const c = transport.last();
    c.serverSend({ type: "playback.started", playback_id: PBK, turn_id: TURN, format: FORMAT });
    c.serverAudio(new Uint8Array([1, 0, 2, 0]));
    await flushPromises();
    expect(player.enqueued).toBe(1);
    expect(wrapper.find("[role=status]").text()).toBe("Speaking");

    const stop = wrapper.findAll("button").find((button) => button.text().includes("Stop speaking"));
    await stop?.trigger("click");
    expect(player.cancels).toBeGreaterThan(0);
    expect(c.messages().at(-1)).toEqual({ type: "playback.cancel" });
    expect(wrapper.emitted("interrupt")).toHaveLength(1);
    expect(wrapper.find("[role=status]").text()).not.toBe("Speaking");

    c.serverSend({ type: "playback.cancelled", playback_id: PBK, turn_id: TURN, reason: "client" });
    c.serverSend({ type: "playback.started", playback_id: id("pbk", 2), turn_id: "turn_2", format: FORMAT });
    c.serverAudio(new Uint8Array([1, 0]));
    await flushPromises();
    const before = player.cancels;
    await talk().trigger("pointerdown");
    await flushPromises();
    expect(player.cancels).toBeGreaterThan(before);
    expect(c.messages().some((m) => m.type === "capture.start")).toBe(true);
    wrapper.unmount();
    session.close();
  });

  it("keeps the speech on screen until the queued audio has drained, not only until the core says done", async () => {
    const { wrapper, session, transport, player } = await mountMode();
    const c = transport.last();
    c.serverSend({ type: "playback.started", playback_id: PBK, turn_id: TURN, format: FORMAT });
    c.serverAudio(new Uint8Array([1, 0]));
    c.serverSend({ type: "playback.done", playback_id: PBK, turn_id: TURN });
    await flushPromises();
    expect(wrapper.find("[role=status]").text()).toBe("Speaking");
    player.cancel(); // drained
    await flushPromises();
    expect(wrapper.find("[role=status]").text()).toBe("Ready");
    wrapper.unmount();
    session.close();
  });

  it("hands free keeps the microphone open between utterances and streams to the endpointer", async () => {
    const { wrapper, session, transport, microphone, talk } = await mountMode();
    await wrapper.find("input[type=checkbox]").setValue(true);
    expect(session.getState().mode).toBe("hands_free");
    await talk().trigger("click");
    await flushPromises();
    expect(session.getState().mode).toBe("hands_free");
    expect(talk().attributes("aria-pressed")).toBe("true");
    microphone.speak(1600, 0.01); // silence: nothing is sent, no capture starts
    expect(transport.last().messages()).toEqual([]);
    microphone.speak(1600, 0.4);
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start"]);
    expect(wrapper.find("[role=status]").text()).toBe("Listening");

    await talk().trigger("click"); // stop: the utterance in progress is finished and the microphone closes
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start", "capture.end"]);
    expect(microphone.stops).toBeGreaterThan(0);
    wrapper.unmount();
    session.close();
  });

  it("falls back to text when the core reports text_input_only, and keeps the typed slot", async () => {
    const { wrapper, session, transport, talk } = await mountMode({}, {});
    transport.last().serverSend({ type: "error", error: { code: "unavailable", message: "no speech recogniser", retryable: false }, effect: "text_input_only" });
    await flushPromises();
    expect(wrapper.text()).toContain("Voice is not available here. Type your message instead.");
    expect(talk().attributes("aria-disabled")).toBe("true");
    expect(wrapper.emitted("fallback")?.[0]).toEqual(["text_input_only"]);
    wrapper.unmount();
    session.close();
  });

  it("shows the fallback slot when the socket cannot be opened, and tries again on request", async () => {
    const { session } = setup({ refuse: (n) => n === 1 });
    const wrapper = mount(AlixVoiceMode, {
      props: { session, microphone: fakeMicrophone(), player: fakePlayer() },
      slots: { fallback: '<label>Type<input class="no-validation" id="typed"></label>' },
    });
    await flushPromises();
    await until(() => wrapper.text().includes("Type your message instead"));
    expect(wrapper.find("#typed").exists()).toBe(true);
    expect(wrapper.emitted("fallback")?.[0]).toEqual(["connect_failed"]);
    await wrapper.findAll("button").find((button) => button.text() === "Try again")?.trigger("click");
    await until(() => session.getState().connection === "open");
    await flushPromises();
    expect(wrapper.text()).not.toContain("Type your message instead");
    wrapper.unmount();
    session.close();
  });

  it("asks for a typed message when the microphone is blocked, and recovers when it is allowed", async () => {
    const { wrapper, session, talk } = await mountMode();
    const blocked: AlixMicrophone = {
      start: () => Promise.reject(new Error("denied")),
      stop: () => undefined,
    };
    await wrapper.setProps({ microphone: blocked });
    await flushPromises();
    await until(() => session.getState().connection === "open");
    await talk().trigger("pointerdown");
    await flushPromises();
    expect(wrapper.text()).toContain("The microphone is blocked or missing");
    expect(wrapper.emitted("fallback")?.[0]).toEqual(["microphone_unavailable"]);
    expect(talk().attributes("aria-disabled")).not.toBe("true");
    wrapper.unmount();
    session.close();
  });

  it("says when replies are text only (no spoken replies) and still shows them", async () => {
    const { wrapper, session, transport } = await mountMode({}, { session: { speech: false } });
    transport.last().serverSend({ type: "response", utterance_id: UTT, turn_id: TURN, text: "Booked for Tuesday.", source: "model", speech: "skipped", speech_reason: "speech_disabled" });
    await flushPromises();
    expect(wrapper.text()).toContain("Replies appear as text and are not read aloud.");
    expect(wrapper.text()).toContain("Booked for Tuesday.");
    wrapper.unmount();
    session.close();
  });

  it("renders reconnection and the end of the session, and hands back a restart", async () => {
    const { wrapper, session, transport } = await mountMode();
    transport.last().serverClose(1001, "credential_expired");
    await flushPromises();
    await until(() => session.getState().connection === "open" && transport.connections.length === 2);
    await flushPromises();
    expect(wrapper.find("[role=status]").text()).toBe("Ready");

    transport.last().serverClose(1000, "replaced");
    await flushPromises();
    await until(() => wrapper.text().includes("The voice session ended."));
    await wrapper.findAll("button").find((button) => button.text() === "Start voice again")?.trigger("click");
    expect(wrapper.emitted("restart")).toHaveLength(1);
    wrapper.unmount();
    session.close();
  });

  it("releases the microphone and drops a half-said utterance when it goes away", async () => {
    const { wrapper, session, transport, microphone, talk } = await mountMode();
    await talk().trigger("pointerdown");
    await flushPromises();
    const stopsBefore = microphone.stops;
    wrapper.unmount();
    expect(microphone.stops).toBeGreaterThan(stopsBefore);
    expect(transport.last().messages().map((m) => m.type)).toEqual(["capture.start", "capture.cancel"]);
    session.close();
  });

  it("works without a session: the host drives state, levels, transcript and reply", async () => {
    const wrapper = mount(AlixVoiceMode, { props: { state: "idle", levels: [0.2, 0.9], transcript: "book dr rosy", reply: "Which day?" } });
    await wrapper.setProps({ state: "processing" });
    expect(wrapper.find("[role=status]").text()).toBe("Processing");
    await wrapper.setProps({ state: "idle" });
    expect(wrapper.find(".alix-voice-orb").attributes("data-state")).toBe("idle");
    expect(wrapper.text()).toContain("Which day?");
    await wrapper.find("button[aria-label]").trigger("pointerdown");
    expect(wrapper.emitted("start")).toHaveLength(1);
    const edit = wrapper.findAll("button").find((button) => button.text() === "Correct what I heard");
    await edit?.trigger("click");
    await wrapper.find("textarea").setValue("book Dr. Rossi");
    await wrapper.find("form").trigger("submit");
    expect(wrapper.emitted("correct")).toEqual([["book Dr. Rossi"]]);
    await wrapper.setProps({ state: "unavailable" });
    expect(wrapper.text()).toContain("Type your message instead");
    await wrapper.find("button:last-of-type").trigger("click");
  });

  it("speaks Italian from the voice catalogue", async () => {
    const wrapper = mount(AlixProvider, { props: { locale: "it" }, slots: { default: () => h(AlixVoiceMode, { state: "unavailable" }) } });
    expect(wrapper.text()).toContain("La voce non è disponibile qui");
    expect(wrapper.text()).toContain("Vivavoce");
  });
});

describe("the voice catalogue", () => {
  it("has the same keys and placeholders in English and Italian, with no dashes", () => {
    const en = VOICE_MESSAGES["en"] as Record<string, string>;
    const it = VOICE_MESSAGES["it"] as Record<string, string>;
    expect(Object.keys(it).sort()).toEqual(Object.keys(en).sort());
    const placeholders = (text: string): string[] => [...text.matchAll(/\{(\w+)\}/g)].map((match) => match[1] as string).sort();
    for (const [key, text] of Object.entries(en)) {
      expect(placeholders(it[key] as string), key).toEqual(placeholders(text));
      expect(text + (it[key] as string)).not.toMatch(/—|–|--/);
    }
  });

  it("renders on a server without touching the browser", async () => {
    const html = await renderToString(createSSRApp({ render: () => h(AlixVoiceMode, { state: "idle" }) }));
    expect(html).toContain("Voice mode");
  });
});
