import { voice } from "@alixai/sdk";
import { createAlixMicrophone, createAlixPlayer } from "./audio-io.js";
import type {
  AlixMicrophone,
  AlixPlayer,
  AlixVoiceController,
  AlixVoiceControllerOptions,
  AlixVoiceEvent,
  AlixVoiceFallback,
  AlixVoiceListener,
  AlixVoiceNotice,
  AlixVoicePhase,
  AlixVoiceView,
} from "./types.js";

// The controller turns a voice session (the SDK's `voice.createVoiceSession`) into what a screen needs: one view to draw and a few
// events to report. It owns the microphone and the loudspeaker and nothing else.
//
// Rules it keeps, all from the voice protocol:
//   * It renders from the session's own state. It keeps only what the state cannot say (is the microphone open, did the last turn
//     prepare an action, which text fallback applies).
//   * Nothing a person says confirms anything: there is no confirm message on the voice socket and this file maps no word to an
//     action. A prepared action is confirmed on screen, with the confirmation card. The secret that card needs reaches the host
//     through the session's own events; it is not read, kept, shown or forwarded here.
//   * Turn taking, not full duplex: pressing talk while the assistant speaks stops the speech first (barge-in). Hands free listens
//     again after the reply; with the session's `bargeIn` off the endpointer ignores the microphone while speech plays.
//   * Voice is optional. A connection that cannot be opened, a `text_input_only` effect or a blocked microphone become a text
//     fallback; the typed path never depends on this controller. Replies are always shown as text, spoken or not.
//   * A connection lives at most 15 minutes; the session replaces it with a fresh token by itself. A capture in progress is lost
//     with it (push to talk asks to press again); the hands-free microphone stays open and resumes by itself.

const BARS = 24;
const SILENT: readonly number[] = Object.freeze(Array.from({ length: BARS }, () => 0));

/** Speech levels are small (0.02 to 0.3): a square root and a gain make the bars use their height. */
const barHeight = (level: number): number => Math.min(1, Math.sqrt(Math.max(0, level)) * 1.6);

const failureOf = (error: unknown, fallbackMessage: string, retryable: boolean): { code: string; message: string; retryable: boolean } => {
  const failure = (typeof error === "object" && error !== null ? error : {}) as { code?: unknown; message?: unknown; retryable?: unknown };
  return {
    code: typeof failure.code === "string" ? failure.code : "unavailable",
    message: typeof failure.message === "string" ? failure.message : fallbackMessage,
    retryable: typeof failure.retryable === "boolean" ? failure.retryable : retryable,
  };
};

export const createAlixVoiceController = (options: AlixVoiceControllerOptions): AlixVoiceController => {
  const { session } = options;
  const microphone: AlixMicrophone = options.microphone ?? createAlixMicrophone();
  const player: AlixPlayer | undefined = options.player === null ? undefined : (options.player ?? createAlixPlayer());
  const sampleRate = options.sampleRate ?? 16000;

  let destroyed = false;
  let handsFree = options.handsFree ?? session.getState().mode === "hands_free";
  let fallback: AlixVoiceFallback | undefined = options.unavailable === true ? "text_input_only" : undefined;
  let notice: AlixVoiceNotice | undefined;
  let confirmationRequired = false;
  let supersededPreparationId: string | undefined;
  let capturing = false; // the microphone is (being) opened for this controller
  let startTicket = 0;
  let levels: number[] = [...SILENT];
  let lastPhase: AlixVoicePhase = "idle";
  let lastRecording = false;
  let speakingLocal = false;
  let pendingConnect: Promise<void> | undefined;
  let view: AlixVoiceView;

  const listeners = new Set<() => void>();
  const eventListeners = new Set<AlixVoiceListener>();
  const emit = (event: AlixVoiceEvent): void => {
    for (const listener of [...eventListeners]) {
      try {
        listener(event);
      } catch {
        // a host's listener failing never reaches the session
      }
    }
  };

  const phaseOf = (): AlixVoicePhase => {
    const state = session.getState();
    if (state.connection === "closed") return "ended";
    if (fallback === "text_input_only" || fallback === "connect_failed") return "unavailable";
    if (state.connection === "reconnecting") return "reconnecting";
    if (state.connection === "connecting") return "connecting";
    if (state.recording === "recording") return "listening";
    if (state.playback === "playing" || speakingLocal) return "speaking";
    if (state.recording === "processing" || state.turn === "running") return "processing";
    return capturing ? "listening" : "idle";
  };

  const compute = (): AlixVoiceView => {
    const state = session.getState();
    const phase = phaseOf();
    if (phase !== lastPhase) {
      const live = (name: AlixVoicePhase): boolean => name === "listening" || name === "speaking";
      if (!live(phase) || !live(lastPhase)) levels = [...SILENT];
      lastPhase = phase;
    }
    const response = state.response;
    return {
      phase,
      handsFree,
      micOpen: capturing,
      levels: phase === "listening" || phase === "speaking" ? levels.slice() : SILENT,
      ...(state.transcript !== undefined && { transcript: { text: state.transcript.text, interim: state.transcript.interim, corrected: state.transcript.corrected } }),
      ...(response !== undefined && { reply: { text: response.text, spoken: response.speech === "queued" } }),
      ...(fallback !== undefined && { fallback }),
      textReplies: state.session?.speech === false || state.degraded.includes("verified_text_only"),
      confirmationRequired,
      ...(supersededPreparationId !== undefined && { supersededPreparationId }),
      ...(notice !== undefined && { notice }),
      ...(state.lastError !== undefined && { error: { code: state.lastError.code, message: state.lastError.message, retryable: state.lastError.retryable } }),
    };
  };

  const publish = (): void => {
    if (destroyed) return;
    view = compute();
    for (const listener of [...listeners]) listener();
  };

  const pushLevel = (level: number): void => {
    levels = [...levels.slice(1), barHeight(level)];
  };

  // ---- the microphone ----
  const onChunk = (samples: Float32Array, rate: number): void => {
    if (!capturing || destroyed) return;
    try {
      session.pushAudio(voice.float32ToPcm16(voice.resampleFloat32(samples, rate, sampleRate)));
    } catch {
      // a chunk the session refuses (the connection moved on) is dropped, never retried
    }
  };

  const closeMicrophone = (): void => {
    startTicket += 1; // a start still waiting for permission is abandoned
    capturing = false;
    microphone.stop();
  };

  // ---- the connection ----
  const connect = (): Promise<void> => {
    if (destroyed || fallback === "text_input_only") return Promise.resolve();
    const state = session.getState().connection;
    if (state === "open") return Promise.resolve();
    if (state === "closed") return Promise.reject(new Error("the voice session is closed"));
    if (pendingConnect !== undefined) return pendingConnect;
    pendingConnect = session
      .connect()
      .then(() => {
        if (fallback === "connect_failed") fallback = undefined;
      })
      .catch((error: unknown) => {
        fallback = "connect_failed";
        emit({ type: "fallback", reason: "connect_failed" });
        emit({ type: "error", ...failureOf(error, "the voice connection could not be opened", true) });
        throw error;
      })
      .finally(() => {
        pendingConnect = undefined;
        publish();
      });
    publish();
    return pendingConnect;
  };

  // ---- what the session says ----
  const onSessionEvent = (event: voice.VoiceEvent): void => {
    switch (event.type) {
      case "state": {
        const recording = event.state.recording === "recording";
        if (recording !== lastRecording) {
          lastRecording = recording;
          emit({ type: "recording", recording });
        }
        if (event.state.connection === "closed") {
          closeMicrophone();
          player?.cancel();
          emit({ type: "ended", reason: "closed" });
        }
        break;
      }
      case "input.level":
        pushLevel(event.level);
        break;
      case "audio.out":
        player?.enqueue(event.format, event.data);
        speakingLocal = player?.isPlaying() ?? false;
        pushLevel(voice.pcmLevel(event.data));
        break;
      case "transcript.interim":
        emit({ type: "transcript", text: event.text, final: false, corrected: false });
        break;
      case "transcript.final":
      case "transcript.corrected":
        // A new turn begins: what the last one asked of the person is stale.
        confirmationRequired = false;
        supersededPreparationId = undefined;
        notice = undefined;
        emit({ type: "transcript", text: event.text, final: true, corrected: event.type === "transcript.corrected" });
        break;
      case "response":
        emit({ type: "response", text: event.text, spoken: event.speech === "queued" });
        break;
      case "turn.completed":
        confirmationRequired = event.confirmation !== undefined;
        supersededPreparationId = event.superseded_preparation_id;
        emit({
          type: "turn",
          turn: {
            turnId: event.turn_id,
            act: event.act,
            failed: event.failed,
            confirmationRequired,
            ...(event.preparation_id !== undefined && { preparationId: event.preparation_id }),
            ...(event.superseded_preparation_id !== undefined && { supersededPreparationId: event.superseded_preparation_id }),
          },
        });
        break;
      case "playback.cancelled":
        player?.cancel();
        speakingLocal = false;
        break;
      case "error":
        if (event.effect === "text_input_only") {
          fallback = "text_input_only";
          closeMicrophone();
          emit({ type: "fallback", reason: "text_input_only" });
        }
        emit({ type: "error", code: event.error.code, message: event.error.message, retryable: event.error.retryable });
        break;
      case "notice":
        if (event.code === "connection_lost") {
          player?.cancel();
          speakingLocal = false;
          // Hands free keeps listening through the renewal; a push-to-talk utterance was cut off with the connection.
          if (!handsFree) {
            closeMicrophone();
            notice = "renewed";
          }
        } else if (event.code === "capture_limit") {
          notice = "capture_limit";
        } else if (event.code === "replaced" || event.code === "reconnect_failed") {
          closeMicrophone();
          emit({ type: "ended", reason: event.code });
        }
        break;
      default:
        break;
    }
    publish();
  };

  const offSession = session.on(onSessionEvent);
  const offIdle = player?.onIdle(() => {
    speakingLocal = false;
    publish();
  });

  // ---- public ----
  const interrupt: AlixVoiceController["interrupt"] = () => {
    const speaking = view.phase === "speaking";
    player?.cancel();
    speakingLocal = false;
    if (session.getState().playback === "playing") {
      try {
        session.cancelPlayback();
      } catch {
        // the connection is gone: the speech is already over
      }
    }
    if (speaking) emit({ type: "interrupt" });
    publish();
  };

  const start: AlixVoiceController["start"] = async () => {
    if (destroyed || capturing) return;
    const phase = view.phase;
    if (phase === "unavailable" || phase === "ended" || phase === "reconnecting") return;
    const ticket = ++startTicket;
    notice = undefined;
    try {
      await connect();
    } catch {
      return;
    }
    if (ticket !== startTicket || destroyed) return;
    if (view.phase === "speaking") interrupt(); // barge-in: the speech stops before the microphone opens
    capturing = true;
    try {
      session.setMode(handsFree ? "hands_free" : "push_to_talk");
      await microphone.start(onChunk);
    } catch {
      capturing = false;
      fallback = "microphone_unavailable";
      emit({ type: "fallback", reason: "microphone_unavailable" });
      publish();
      return;
    }
    if (ticket !== startTicket || destroyed) {
      microphone.stop();
      return;
    }
    if (fallback === "microphone_unavailable") fallback = undefined;
    if (!handsFree) {
      try {
        session.startCapture(options.language === undefined ? undefined : { language: options.language });
      } catch {
        closeMicrophone();
      }
    }
    publish();
  };

  const stop: AlixVoiceController["stop"] = () => {
    const wasOpen = capturing;
    closeMicrophone();
    if (session.getState().recording === "recording") {
      try {
        session.endCapture(); // the transcript follows; a hands-free utterance in progress is finished the same way
      } catch {
        // the capture ended on its own (the limit, the endpointer)
      }
    }
    if (wasOpen) publish();
  };

  const cancel: AlixVoiceController["cancel"] = () => {
    closeMicrophone();
    if (session.getState().recording === "recording") {
      try {
        session.cancelCapture();
      } catch {
        // already over
      }
    }
    publish();
  };

  const setHandsFree: AlixVoiceController["setHandsFree"] = (on) => {
    if (on === handsFree) return;
    stop();
    handsFree = on;
    try {
      session.setMode(on ? "hands_free" : "push_to_talk");
    } catch {
      // the mode is set again when the microphone opens
    }
    publish();
  };

  const correct: AlixVoiceController["correct"] = (text) => {
    const corrected = text.trim();
    const current = session.getState().transcript;
    if (corrected === "" || current === undefined || current.interim || corrected === current.text.trim()) return false;
    try {
      player?.cancel();
      speakingLocal = false;
      session.correctTranscript(corrected);
    } catch (error) {
      emit({ type: "error", ...failureOf(error, "the correction could not be sent", false) });
      return false;
    }
    emit({ type: "correct", text: corrected });
    publish();
    return true;
  };

  const retry: AlixVoiceController["retry"] = () => {
    if (fallback !== "connect_failed") return;
    fallback = undefined;
    void connect().catch(() => undefined);
  };

  const destroy: AlixVoiceController["destroy"] = () => {
    if (destroyed) return;
    if (capturing && session.getState().recording === "recording") {
      try {
        session.cancelCapture(); // leaving the screen never sends what was half said
      } catch {
        // already over
      }
    }
    closeMicrophone();
    if (session.getState().playback === "playing") {
      try {
        session.cancelPlayback();
      } catch {
        // already over
      }
    }
    destroyed = true;
    offSession();
    offIdle?.();
    player?.close();
    listeners.clear();
    eventListeners.clear();
  };

  view = compute();
  if (options.autoConnect !== false && fallback === undefined && session.getState().connection === "idle") void connect().catch(() => undefined);

  return {
    getView: () => view,
    subscribe: (listener) => {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    },
    on: (listener) => {
      eventListeners.add(listener);
      return () => {
        eventListeners.delete(listener);
      };
    },
    connect,
    start,
    stop,
    cancel,
    interrupt,
    setHandsFree,
    correct,
    retry,
    destroy,
  };
};
