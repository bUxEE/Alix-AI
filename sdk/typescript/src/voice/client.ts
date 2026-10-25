import { isAlixAIError, sdkError } from "../errors.js";
import type { AlixAIError } from "../errors.js";
import { contextToken, normalizeBaseUrl, pathId } from "../validate.js";
import { openSocket, webSocketUrl } from "../ws.js";
import { bytesToBase64, pcm16ToBytes, pcmDurationMs, pcmLevel } from "./audio.js";
import { isAudioFormat, parseServerEvent } from "./guards.js";
import { DEFAULT_AUDIO_FORMAT } from "./types.js";
import type {
  AudioFormat,
  ClientMessage,
  ServerEvent,
  VoiceEvent,
  VoiceListener,
  VoiceMode,
  VoiceNoticeCode,
  VoiceOptions,
  VoiceSession,
  VoiceSessionInfo,
  VoiceSocketLike,
  VoiceState,
} from "./types.js";

// The voice client: one WebSocket per voice conversation (GET /v1/sessions/{session_id}/voice, schemas/voice).
//
// What it owns, and the rules it keeps:
//   * Identity. The scope is the context token, whole. It is minted by the host for each connection (a reconnection too: the token
//     is single use), sent only as `Authorization: Bearer` through the host's socket factory, and never put in a URL, a message, a
//     state or an error. No message the client sends names a tenant, a workspace, a principal, a role or a scope.
//   * No confirmation. The six messages of the protocol are the only ones this client can send and none of them confirms, rejects
//     or chooses: a spoken "yes" is only text. `turn.completed.confirmation` reaches the host in the event, for the host's own
//     confirm control (the main client's `confirm`), and is kept out of the state snapshot.
//   * Fail closed. A message of the core that does not match the schema is ignored with a notice, never acted on; audio outside a
//     playback is dropped; a cancelled playback delivers nothing more, at once, without waiting for the core.
//   * A connection lives until its token expires, at most 15 minutes. When it ends for that reason (or because the network or the
//     service went away) the client opens a new one with a fresh token, a bounded number of times. A connection that another one
//     replaced is NOT reopened, or two tabs would take the session from each other for ever. Whatever was live (a capture, a
//     playback, a running turn) is over when a connection ends and the host is told.
//   * Turn taking is manual (the service's voice-activity detector is not used and takes 16 kHz mono only). Push to talk is the
//     host's `startCapture` / `endCapture`; hands free is a small energy endpointer here that ends the utterance on silence.
//   * Nothing here claims full duplex: speech and capture alternate, and a capture that starts while speech plays cancels it.

const OPEN_TIMEOUT_MS = 10_000;
const DEFAULT_RECONNECT = { maxAttempts: 3, baseDelayMs: 250, maxDelayMs: 5000 };
const DEFAULT_ENDPOINTER = { speechLevel: 0.02, minSpeechMs: 60, silenceMs: 800, preRollMs: 300, maxUtteranceMs: 30_000, bargeIn: false };
const LANGUAGE = /^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$/;
const UTTERANCE_ID = /^utt_[0-9A-HJKMNP-TV-Z]{26}$/;
const MAX_CORRECTION_LENGTH = 2000;

const closedError = (): AlixAIError =>
  sdkError("unavailable", "the voice session is closed", { retryable: false, details: { reason: "closed" } });

const invalid = (message: string, argument: string, reason?: string): AlixAIError =>
  sdkError("validation_failed", message, { details: { argument, ...(reason !== undefined && { reason }) } });

const language = (value: unknown, argument = "language"): string | undefined => {
  if (value === undefined) return undefined;
  if (typeof value !== "string" || value.length > 35 || !LANGUAGE.test(value)) throw invalid("the language is not a BCP 47 language tag", argument);
  return value;
};

const isMode = (value: unknown): value is VoiceMode => value === "push_to_talk" || value === "hands_free";

/** Whether a connection that ended with this close code and reason is replaced by a new one (with a fresh token). */
const reopens = (code: number, reason: string): boolean => {
  if (reason === "replaced") return false; // another connection took the session: do not fight for it
  return code === 1001 || code === 1006 || code === 1012 || code === 1013;
};

interface Capture {
  /** Started by the endpointer (hands free) rather than by the host. */
  auto: boolean;
  utterance?: string;
  bytes: number;
  ms: number;
  silenceMs: number;
}

interface ActivePlayback {
  id: string;
  turnId: string;
  format: AudioFormat;
}

/**
 * Creates a voice conversation on a session. Nothing is validated against the network and nothing is opened until `connect()`;
 * everything that can be refused locally (the base URL, the session id, the language, the mode, the format) is refused here, before a
 * single-use token is spent.
 */
export const createVoiceSession = (options: VoiceOptions): VoiceSession => {
  const baseUrl = normalizeBaseUrl(options.baseUrl, options.allowInsecureHttp === true);
  const sessionId = pathId("sessionId", options.sessionId);
  if (typeof options.getContextToken !== "function") throw invalid("getContextToken is required", "getContextToken");
  const connectLanguage = language(options.language);
  if (options.speak !== undefined && typeof options.speak !== "boolean") throw invalid("speak is true or false", "speak");
  if (options.mode !== undefined && !isMode(options.mode)) throw invalid("mode is push_to_talk or hands_free", "mode");
  const format = options.format ?? DEFAULT_AUDIO_FORMAT;
  if (!isAudioFormat(format)) throw invalid("format must be pcm_s16le, 8000 to 48000 Hz, one or two channels", "format");
  const reconnect = { ...DEFAULT_RECONNECT, ...options.reconnect };
  const endpointer = { ...DEFAULT_ENDPOINTER, ...options.endpointer };
  const openTimeoutMs = options.openTimeoutMs ?? OPEN_TIMEOUT_MS;
  const frameBytes = 2 * format.channels;

  const query = new URLSearchParams();
  if (connectLanguage !== undefined) query.set("language", connectLanguage);
  if (options.speak === false) query.set("speak", "false");
  const url = webSocketUrl(`${baseUrl}/v1/sessions/${sessionId}/voice${query.size > 0 ? `?${query.toString()}` : ""}`);

  // ---- state and listeners ----
  let state: VoiceState = Object.freeze({
    connection: "idle",
    mode: options.mode ?? "push_to_talk",
    recording: "idle",
    playback: "idle",
    turn: "idle",
    degraded: [],
    reconnectAttempt: 0,
  } satisfies VoiceState);
  const listeners = new Set<VoiceListener>();

  const emit = (event: VoiceEvent): void => {
    for (const listener of [...listeners]) {
      try {
        listener(event);
      } catch {
        // a listener's failure is the host's: it never reaches the connection or the other listeners
      }
    }
  };

  /** Applies a patch; returns whether anything changed. The `state` event is announced by the caller, after the event that caused it. */
  const apply = (patch: Partial<VoiceState>): boolean => {
    let changed = false;
    for (const key of Object.keys(patch) as (keyof VoiceState)[]) {
      if (state[key] !== patch[key]) changed = true;
    }
    if (changed) state = Object.freeze({ ...state, ...patch });
    return changed;
  };
  const announce = (changed: boolean): void => {
    if (changed) emit({ type: "state", state });
  };
  const update = (patch: Partial<VoiceState>): void => announce(apply(patch));
  const notice = (code: VoiceNoticeCode, message: string): void => emit({ type: "notice", code, message });

  // ---- connection bookkeeping ----
  const lifecycle = new AbortController();
  let socket: VoiceSocketLike | undefined;
  let generation = 0;
  let closedByHost = false;
  let connectPromise: Promise<VoiceSessionInfo> | undefined;

  // ---- what is live on the current connection (none of it survives the connection) ----
  let capture: Capture | undefined;
  let processing: { utterance?: string } | undefined;
  let turnUtterance: string | undefined;
  let correctable: string | undefined;
  let playback: ActivePlayback | undefined;
  let muted = false;
  let strayAudioNoticed = false;
  let lastInterim: { utterance: string; seq: number } | undefined;
  let preRoll: Uint8Array[] = [];
  let preRollBytes = 0;
  let speechMs = 0;

  const resetEndpointer = (): void => {
    preRoll = [];
    preRollBytes = 0;
    speechMs = 0;
  };

  const abandonLive = (): void => {
    const hadLive = capture !== undefined || processing !== undefined || playback !== undefined || state.turn === "running";
    capture = undefined;
    processing = undefined;
    turnUtterance = undefined;
    correctable = undefined;
    playback = undefined;
    muted = false;
    lastInterim = undefined;
    resetEndpointer();
    update({ recording: "idle", playback: "idle", playbackTurnId: undefined, turn: "idle", utteranceId: undefined });
    if (hadLive) {
      notice("connection_lost", "the connection ended: the live capture, playback and turn are over and their ids are stale; read the turn from its event stream");
    }
  };

  const isOpen = (): boolean => socket !== undefined && state.connection === "open";

  const requireOpen = (): VoiceSocketLike => {
    if (state.connection === "closed") throw closedError();
    if (socket === undefined || state.connection !== "open") {
      throw sdkError("unavailable", state.connection === "reconnecting" ? "the voice connection is being re-established" : "the voice connection is not open", {
        retryable: state.connection === "reconnecting",
        details: { reason: state.connection },
      });
    }
    return socket;
  };

  const sendMessage = (message: ClientMessage): void => {
    requireOpen().send(JSON.stringify(message));
  };

  const session = (): VoiceSessionInfo => {
    const info = state.session;
    if (info === undefined) throw sdkError("unavailable", "the voice connection is not open", { details: { reason: state.connection } });
    return info;
  };

  // ---- playback: stopped locally at once, without waiting for the core ----
  const stopPlaybackLocally = (): void => {
    if (playback === undefined) return;
    muted = true; // chunks that are already on the wire are dropped until the core confirms (playback.cancelled / done)
    update({ playback: "idle", playbackTurnId: undefined });
  };

  // ---- events of the core ----
  const onServerEvent = (event: ServerEvent): void => {
    let patch: Partial<VoiceState> = {};
    switch (event.type) {
      case "session.started":
        // Only the first message of a connection is `session.started`; a second one changes nothing.
        return;
      case "capture.started":
        if (capture !== undefined && capture.utterance === undefined) capture.utterance = event.utterance_id;
        else if (processing !== undefined && processing.utterance === undefined) processing.utterance = event.utterance_id;
        patch = { utteranceId: event.utterance_id };
        break;
      case "capture.cancelled":
        if (capture?.utterance === event.utterance_id) {
          capture = undefined;
          patch = { recording: "idle" };
        } else if (processing !== undefined && (processing.utterance === event.utterance_id || processing.utterance === undefined)) {
          processing = undefined;
          if (capture === undefined) patch = { recording: "idle" };
        }
        if (state.transcript?.interim === true && state.transcript.utteranceId === event.utterance_id) patch = { ...patch, transcript: undefined };
        break;
      case "transcript.interim": {
        const stale =
          (lastInterim?.utterance === event.utterance_id && event.seq <= lastInterim.seq) ||
          (state.transcript?.utteranceId === event.utterance_id && !state.transcript.interim);
        if (stale) return; // an interim that is not newer than what is shown is dropped, and never forwarded
        lastInterim = { utterance: event.utterance_id, seq: event.seq };
        patch = { transcript: { utteranceId: event.utterance_id, text: event.text, interim: true, corrected: false } };
        break;
      }
      case "transcript.final":
        if (processing !== undefined && (processing.utterance === event.utterance_id || processing.utterance === undefined)) {
          processing = undefined;
          if (capture === undefined) patch = { recording: "idle" };
        }
        correctable = event.utterance_id;
        turnUtterance = event.utterance_id;
        patch = {
          ...patch,
          turn: "running",
          utteranceId: event.utterance_id,
          transcript: {
            utteranceId: event.utterance_id,
            text: event.text,
            interim: false,
            corrected: false,
            ...(event.language !== undefined && { language: event.language }),
            ...(event.speaker_hint !== undefined && { speakerHint: event.speaker_hint }),
          },
        };
        break;
      case "transcript.corrected":
        turnUtterance = event.utterance_id;
        patch = {
          turn: "running",
          transcript: { utteranceId: event.utterance_id, text: event.text, interim: false, corrected: true },
        };
        break;
      case "turn.completed":
        turnUtterance = undefined;
        // The secret in `event.confirmation` is for the host's listener only: it is not stored here.
        patch = { turn: "idle" };
        break;
      case "response":
        patch = {
          response: {
            utteranceId: event.utterance_id,
            turnId: event.turn_id,
            text: event.text,
            source: event.source,
            speech: event.speech,
            ...(event.speech_reason !== undefined && { speechReason: event.speech_reason }),
          },
        };
        break;
      case "playback.started":
        playback = { id: event.playback_id, turnId: event.turn_id, format: event.format };
        muted = false;
        strayAudioNoticed = false;
        patch = { playback: "playing", playbackTurnId: event.turn_id };
        break;
      case "playback.done":
      case "playback.cancelled":
        if (playback?.id === event.playback_id) {
          playback = undefined;
          muted = false;
          patch = { playback: "idle", playbackTurnId: undefined };
        }
        break;
      case "error": {
        if (event.utterance_id !== undefined) {
          if (processing !== undefined && (processing.utterance === event.utterance_id || processing.utterance === undefined) && capture === undefined) {
            processing = undefined;
            patch = { recording: "idle" };
          }
          if (turnUtterance === event.utterance_id) {
            turnUtterance = undefined;
            patch = { ...patch, turn: "idle" };
          }
        }
        patch = {
          ...patch,
          lastError: {
            code: event.error.code,
            message: event.error.message,
            retryable: event.error.retryable,
            ...(event.effect !== undefined && { effect: event.effect }),
          },
          ...(event.effect !== undefined && !state.degraded.includes(event.effect) && { degraded: [...state.degraded, event.effect] }),
        };
        break;
      }
    }
    const changed = apply(patch);
    emit(event);
    announce(changed);
  };

  const onBinary = (bytes: Uint8Array): void => {
    const current = playback;
    if (current === undefined) {
      if (!strayAudioNoticed) {
        strayAudioNoticed = true;
        notice("audio_dropped", "audio arrived outside a playback and was dropped");
      }
      return;
    }
    if (muted) return; // the playback was cancelled here: nothing more is delivered, whatever is still on the wire
    emit({ type: "audio.out", playback_id: current.id, turn_id: current.turnId, format: current.format, data: bytes });
  };

  const onBinaryMessage = (data: unknown): void => {
    if (data instanceof ArrayBuffer) onBinary(new Uint8Array(data));
    else if (ArrayBuffer.isView(data)) onBinary(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
    else if (typeof Blob !== "undefined" && data instanceof Blob) {
      // A socket that did not honour binaryType = "arraybuffer". Decoded in arrival order is not guaranteed, so the playback that was
      // live on arrival must still be the live one when the bytes are ready.
      const arrival = playback;
      void data.arrayBuffer().then((buffer) => {
        if (playback === arrival) onBinary(new Uint8Array(buffer));
      });
    } else notice("audio_dropped", "a binary message of an unknown kind was dropped");
  };

  const onTextMessage = (text: string): void => {
    const event = parseServerEvent(text);
    if (event === undefined) {
      notice("malformed_event", "a message of the core did not match its schema and was ignored");
      return;
    }
    onServerEvent(event);
  };

  // ---- opening one connection ----
  const mintToken = async (): Promise<string> => {
    let raw: unknown;
    try {
      raw = await options.getContextToken({ operation: "openVoiceSession", sessionId });
    } catch (cause) {
      if (lifecycle.signal.aborted) throw lifecycle.signal.reason;
      throw sdkError("unauthenticated", "getContextToken failed, so the voice connection was not opened", {
        cause,
        details: { operation: "openVoiceSession", reason: "token_provider_failed" },
      });
    }
    const token = contextToken(raw);
    if (lifecycle.signal.aborted) throw lifecycle.signal.reason; // do not spend a token on a session that was closed meanwhile
    return token;
  };

  const attempt = async (): Promise<VoiceSessionInfo> => {
    const factory = options.webSocket;
    if (factory === undefined) {
      throw sdkError("unavailable", "the voice socket needs a webSocket factory that can set the Authorization header (a browser's WebSocket cannot): pass the `webSocket` option", {
        retryable: false,
        details: { reason: "no_websocket_factory" },
      });
    }
    const token = await mintToken();
    const opened = (await openSocket(factory, url, { protocols: [], headers: { Authorization: `Bearer ${token}` } }, lifecycle.signal)) as VoiceSocketLike;
    const mine = ++generation;
    socket = opened;
    try {
      opened.binaryType = "arraybuffer";
    } catch {
      // a socket without the property: binary messages are handled in whatever form they arrive
    }
    return new Promise<VoiceSessionInfo>((resolve, reject) => {
      let started = false;
      const timer = setTimeout(() => {
        if (started) return;
        reject(sdkError("timeout", "the voice service did not answer after the connection opened", { details: { reason: "session_started_timeout" } }));
        try {
          opened.close(1000, "timeout");
        } catch {
          // nothing is left to release
        }
      }, openTimeoutMs);

      const onMessage = (event: { data?: unknown }): void => {
        if (mine !== generation) return;
        if (!started) {
          const first = typeof event.data === "string" ? parseServerEvent(event.data) : undefined;
          if (first?.type !== "session.started") {
            notice("malformed_event", "the first message of the voice connection was not session.started and was ignored");
            return;
          }
          started = true;
          clearTimeout(timer);
          const { type: _type, ...info } = first;
          const changed = apply({ connection: "open", session: info, reconnectAttempt: 0, degraded: [] });
          emit(first);
          announce(changed);
          resolve(info);
          return;
        }
        if (typeof event.data === "string") onTextMessage(event.data);
        else onBinaryMessage(event.data);
      };

      const onClose = (event: { code?: number; reason?: string }): void => {
        opened.removeEventListener("message", onMessage as (event: never) => void);
        opened.removeEventListener("close", onClose as (event: never) => void);
        if (mine !== generation) return;
        clearTimeout(timer);
        socket = undefined;
        const code = event.code ?? 1006;
        const reason = event.reason ?? "";
        if (!started) {
          reject(sdkError("unavailable", "the voice connection closed before it started", { details: { reason: "closed_before_started", code } }));
          return;
        }
        onConnectionEnded(code, reason);
      };

      opened.addEventListener("message", onMessage as (event: never) => void);
      opened.addEventListener("close", onClose as (event: never) => void);
    });
  };

  // ---- a connection that was open ended ----
  const sleep = (ms: number): Promise<boolean> =>
    new Promise<boolean>((resolve) => {
      if (lifecycle.signal.aborted) {
        resolve(false);
        return;
      }
      const done = (ok: boolean): void => {
        clearTimeout(timer);
        lifecycle.signal.removeEventListener("abort", onAbort);
        resolve(ok);
      };
      const onAbort = (): void => done(false);
      const timer = setTimeout(() => done(true), ms);
      lifecycle.signal.addEventListener("abort", onAbort, { once: true });
    });

  const reopen = async (immediate: boolean): Promise<void> => {
    let last: unknown;
    for (let n = 1; n <= reconnect.maxAttempts; n += 1) {
      update({ connection: "reconnecting", reconnectAttempt: n });
      const delay = n === 1 && immediate ? 0 : Math.min(reconnect.maxDelayMs, reconnect.baseDelayMs * 2 ** (n - 1));
      if (!(await sleep(delay)) || closedByHost) return;
      try {
        await attempt(); // a fresh token for each attempt: the one of the connection that ended is spent
        return;
      } catch (error) {
        if (closedByHost) return;
        last = error;
        if (isAlixAIError(error) && !error.retryable) break;
      }
    }
    if (closedByHost) return;
    const error = isAlixAIError(last) ? last : sdkError("unavailable", "the voice connection could not be re-established");
    notice("reconnect_failed", "the voice connection could not be re-established");
    update({ connection: "closed", reconnectAttempt: 0, lastError: { code: error.code, message: error.message, retryable: error.retryable } });
  };

  const onConnectionEnded = (code: number, reason: string): void => {
    abandonLive();
    if (closedByHost) return;
    if (reason === "replaced") notice("replaced", "another connection took over this voice session; it is not reopened");
    if (reconnect.maxAttempts > 0 && reopens(code, reason)) {
      void reopen(reason === "credential_expired");
      return;
    }
    update({ connection: "closed", session: undefined });
  };

  // ---- public: connect and close ----
  const connect: VoiceSession["connect"] = () => {
    if (state.connection === "closed") return Promise.reject(closedError());
    if (connectPromise !== undefined) return connectPromise;
    if (isOpen() && state.session !== undefined) return Promise.resolve(session());
    if (state.connection === "reconnecting") {
      return Promise.reject(sdkError("conflict", "the voice connection is being re-established", { details: { reason: "reconnecting" } }));
    }
    update({ connection: "connecting" });
    connectPromise = attempt()
      .catch((error: unknown) => {
        if (closedByHost) throw closedError();
        const failure = isAlixAIError(error) ? error : sdkError("unavailable", "the voice connection could not be opened", { cause: error });
        update({
          connection: "idle",
          lastError: { code: failure.code, message: failure.message, retryable: failure.retryable },
        });
        throw failure;
      })
      .finally(() => {
        connectPromise = undefined;
      });
    return connectPromise;
  };

  const close: VoiceSession["close"] = () => {
    if (closedByHost) return;
    closedByHost = true;
    lifecycle.abort(closedError());
    const current = socket;
    socket = undefined;
    generation += 1; // whatever the old socket still says is no longer heard
    abandonLive();
    if (current !== undefined) {
      try {
        current.close(1000, "client_closed");
      } catch {
        // already closed
      }
    }
    update({ connection: "closed", session: undefined, reconnectAttempt: 0 });
  };

  // ---- public: capture ----
  const beginCapture = (auto: boolean, requestedLanguage: string | undefined): void => {
    session(); // throws when the connection is not open
    if (capture !== undefined) throw sdkError("conflict", "a capture is already in progress", { details: { reason: "capture_active" } });
    stopPlaybackLocally(); // barge-in: the core cancels the speech too
    sendMessage({ type: "capture.start", format, ...(requestedLanguage !== undefined && { language: requestedLanguage }) });
    capture = { auto, bytes: 0, ms: 0, silenceMs: 0 };
    lastInterim = undefined;
    update({ recording: "recording", utteranceId: undefined });
  };

  const startCapture: VoiceSession["startCapture"] = (opts) => {
    beginCapture(false, language(opts?.language));
  };

  const toBytes = (pcm: unknown): Uint8Array => {
    let bytes: Uint8Array;
    if (pcm instanceof Int16Array) bytes = pcm16ToBytes(pcm);
    else if (pcm instanceof Uint8Array) bytes = pcm;
    else if (pcm instanceof ArrayBuffer) bytes = new Uint8Array(pcm);
    else throw invalid("audio must be an Int16Array, a Uint8Array or an ArrayBuffer of PCM 16-bit little-endian samples", "pcm");
    if (bytes.length % frameBytes !== 0) throw invalid("audio must be whole frames of 16-bit samples", "pcm", "partial_sample");
    return bytes;
  };

  const endCapture: VoiceSession["endCapture"] = () => {
    const current = capture;
    if (current === undefined) throw sdkError("conflict", "no capture is in progress", { details: { reason: "no_capture" } });
    sendMessage({ type: "capture.end" });
    capture = undefined;
    processing = { ...(current.utterance !== undefined && { utterance: current.utterance }) };
    resetEndpointer();
    update({ recording: "processing" });
  };

  const cancelCapture: VoiceSession["cancelCapture"] = () => {
    if (capture === undefined) return;
    sendMessage({ type: "capture.cancel" });
    capture = undefined;
    resetEndpointer();
    update({ recording: processing === undefined ? "idle" : "processing", transcript: state.transcript?.interim === true ? undefined : state.transcript });
  };

  /** Streams audio of the running capture in messages of at most `max_chunk_bytes`, never past `max_audio_bytes`. */
  const streamAudio = (bytes: Uint8Array): boolean => {
    const current = capture;
    if (current === undefined || !isOpen()) return false;
    const info = session();
    if (current.bytes + bytes.length > info.max_audio_bytes) {
      notice("capture_limit", "the capture reached the most audio one utterance may carry and was ended");
      endCapture();
      return false;
    }
    const piece = Math.max(frameBytes, info.max_chunk_bytes - (info.max_chunk_bytes % frameBytes));
    const open = requireOpen();
    for (let offset = 0; offset < bytes.length; offset += piece) open.send(bytes.subarray(offset, offset + piece));
    current.bytes += bytes.length;
    return true;
  };

  const pushAudio: VoiceSession["pushAudio"] = (pcm) => {
    const bytes = toBytes(pcm);
    const level = pcmLevel(bytes);
    const duration = pcmDurationMs(bytes.length, format.sample_rate, format.channels);
    emit({ type: "input.level", level, capturing: capture !== undefined });

    // Push to talk, or a capture the host started by hand: the host decides where the utterance ends.
    if (state.mode === "push_to_talk" || (capture !== undefined && !capture.auto)) return streamAudio(bytes);
    if (!isOpen()) return false;

    // Hands free: the endpointer finds the utterances.
    if (capture === undefined) {
      if (playback !== undefined && !muted && !endpointer.bargeIn) {
        resetEndpointer(); // the microphone hears the loudspeaker: not a user
        return false;
      }
      speechMs = level >= endpointer.speechLevel ? speechMs + duration : 0;
      preRoll.push(bytes.slice()); // the caller may reuse its buffer
      preRollBytes += bytes.length;
      const budget = Math.ceil(((endpointer.preRollMs + endpointer.minSpeechMs) / 1000) * format.sample_rate) * frameBytes;
      while (preRoll.length > 1 && preRollBytes - (preRoll[0] as Uint8Array).length >= budget) preRollBytes -= (preRoll.shift() as Uint8Array).length;
      if (speechMs < endpointer.minSpeechMs) return false;
      const pending = preRoll;
      resetEndpointer();
      try {
        beginCapture(true, connectLanguage);
      } catch {
        return false;
      }
      for (const chunk of pending) {
        if (!streamAudio(chunk)) return false;
        if (capture !== undefined) (capture as Capture).ms += pcmDurationMs(chunk.length, format.sample_rate, format.channels);
      }
      return true;
    }
    const kept = streamAudio(bytes);
    const live = capture as Capture | undefined;
    if (live === undefined) return kept; // the limit ended it
    live.ms += duration;
    live.silenceMs = level < endpointer.speechLevel ? live.silenceMs + duration : 0;
    if (live.silenceMs >= endpointer.silenceMs || live.ms >= endpointer.maxUtteranceMs) endCapture();
    return kept;
  };

  const submitAudio: VoiceSession["submitAudio"] = (pcm, opts) => {
    const bytes = toBytes(pcm);
    const lang = language(opts?.language);
    if (bytes.length === 0) throw invalid("there is no audio to submit", "pcm", "empty");
    const info = session();
    if (bytes.length > info.max_audio_bytes) {
      throw invalid("the audio is longer than one utterance may be", "pcm", "audio_too_long");
    }
    if (capture !== undefined) throw sdkError("conflict", "a capture is in progress: end or cancel it first", { details: { reason: "capture_active" } });
    stopPlaybackLocally();
    sendMessage({ type: "audio.submit", format, audio: bytesToBase64(bytes), ...(lang !== undefined && { language: lang }) });
    processing = {};
    update({ recording: "processing" });
  };

  // ---- public: correction, playback, mode ----
  const correctTranscript: VoiceSession["correctTranscript"] = (text, utteranceId) => {
    if (typeof text !== "string" || text.trim() === "" || text.length > MAX_CORRECTION_LENGTH) {
      throw invalid(`the corrected text is 1 to ${MAX_CORRECTION_LENGTH} characters`, "text");
    }
    const target = utteranceId ?? correctable;
    if (target === undefined) throw sdkError("conflict", "there is no utterance of this connection to correct", { details: { reason: "nothing_to_correct" } });
    if (!UTTERANCE_ID.test(target)) throw invalid("utteranceId is not an utterance id", "utteranceId");
    stopPlaybackLocally(); // a correction runs a new turn: the speech of the old one is over
    sendMessage({ type: "transcript.correct", utterance_id: target, text });
  };

  const cancelPlayback: VoiceSession["cancelPlayback"] = () => {
    stopPlaybackLocally();
    if (isOpen()) sendMessage({ type: "playback.cancel" });
  };

  const setMode: VoiceSession["setMode"] = (mode) => {
    if (!isMode(mode)) throw invalid("mode is push_to_talk or hands_free", "mode");
    if (state.mode === "hands_free" && mode !== "hands_free") {
      if (capture?.auto === true) endCapture();
      resetEndpointer();
    }
    update({ mode });
  };

  const on: VoiceSession["on"] = (listener) => {
    listeners.add(listener);
    return () => {
      listeners.delete(listener);
    };
  };

  return {
    connect,
    close,
    getState: () => state,
    on,
    setMode,
    startCapture,
    pushAudio,
    endCapture,
    cancelCapture,
    submitAudio,
    correctTranscript,
    cancelPlayback,
  };
};
