import type { AlixAIError } from "../errors.js";
import type { WebSocketLike } from "../ws.js";
import type {
  AudioFormat,
  CaptureCancelled,
  CaptureStarted,
  Effect,
  Error as VoiceErrorEvent,
  PlaybackCancelled,
  PlaybackDone,
  PlaybackStarted,
  Response as VoiceResponseEvent,
  ServerEvent,
  SessionStarted,
  SpeakerHint,
  TranscriptCorrected,
  TranscriptFinal,
  TranscriptInterim,
  TurnCompleted,
} from "../gen/voice/index.js";

// The wire types are the generated ones (schemas/voice); everything below is what the client adds on top of them: the options, the
// state a host renders from, and the events of the client itself.

export type {
  AudioFormat,
  CaptureCancelled,
  CaptureStarted,
  ClientMessage,
  Effect,
  PlaybackCancelled,
  PlaybackDone,
  PlaybackStarted,
  ServerEvent,
  SessionStarted,
  SpeakerHint,
  TranscriptCorrected,
  TranscriptFinal,
  TranscriptInterim,
  TurnCompleted,
  TurnCompletedConfirmation,
} from "../gen/voice/index.js";
export type { VoiceErrorEvent, VoiceResponseEvent };

/** The audio the client sends unless it is told otherwise: raw PCM, signed 16-bit little-endian, 16 kHz, mono (what the voice-activity detector and the recogniser take). */
export const DEFAULT_AUDIO_FORMAT: AudioFormat = { encoding: "pcm_s16le", sample_rate: 16000, channels: 1 };

/**
 * `push_to_talk`: the host decides when an utterance starts and ends (`startCapture`, `pushAudio`, `endCapture`).
 * `hands_free`: the host streams the microphone continuously with `pushAudio` and the client finds the utterances itself, with a
 * small silence endpointer, because the service takes its turns manually (no server-side voice-activity detection).
 */
export type VoiceMode = "push_to_talk" | "hands_free";

/** `idle` before `connect`, `connecting` while a socket opens, `open`, `reconnecting` while a fresh token is being used to replace a connection that ended, `closed` for good. */
export type VoiceConnectionState = "idle" | "connecting" | "open" | "reconnecting" | "closed";

/** `recording`: audio is being streamed. `processing`: the utterance ended and its transcript has not arrived (or was refused). */
export type RecordingState = "idle" | "recording" | "processing";

/** `playing`: verified speech is arriving and is delivered as `audio.out` events. */
export type PlaybackStateName = "idle" | "playing";

/** `running` from the final transcript until `turn.completed`. */
export type TurnStateName = "idle" | "running";

/** What the host knows about the connection, from `session.started`. */
export type VoiceSessionInfo = Omit<SessionStarted, "type">;

/** The latest transcript of the user's speech, as the host should show it. */
export interface VoiceTranscript {
  utteranceId: string;
  text: string;
  /** True while it is the recogniser's current hypothesis; false once it is final or corrected. */
  interim: boolean;
  /** True when the user replaced the recogniser's text with their own. */
  corrected: boolean;
  language?: string;
  /** A hint only: never an identity and never a permission. */
  speakerHint?: SpeakerHint;
}

/** The latest verified response of the core. Its `text` is always shown; `speech` says whether it is also spoken. */
export interface VoiceResponse {
  utteranceId: string;
  turnId: string;
  text: string;
  source: VoiceResponseEvent["source"];
  speech: VoiceResponseEvent["speech"];
  speechReason?: VoiceResponseEvent["speech_reason"];
}

/** The latest failure of the client or of the core, for a host that shows one line. */
export interface VoiceLastError {
  code: AlixAIError["code"];
  message: string;
  retryable: boolean;
  /** The degradation that applies: type instead, read instead of listen, end each capture yourself. */
  effect?: Effect;
}

/**
 * Everything a host needs to draw a recording and playback UI. A snapshot: `getState()` returns the current one and a `state`
 * event carries each new one. It never holds a confirmation secret (that is in the `turn.completed` event only).
 */
export interface VoiceState {
  connection: VoiceConnectionState;
  mode: VoiceMode;
  recording: RecordingState;
  playback: PlaybackStateName;
  turn: TurnStateName;
  /** The utterance being captured, or the latest one the core accepted. */
  utteranceId?: string;
  /** The turn whose speech is playing, while `playback` is `playing`. */
  playbackTurnId?: string;
  /** What `session.started` said, while connected. */
  session?: VoiceSessionInfo;
  transcript?: VoiceTranscript;
  response?: VoiceResponse;
  /** The degradations the core reported on this connection (a voice component is down). Text keeps working. */
  degraded: Effect[];
  /** 0 while connected; the attempt number while `reconnecting`. */
  reconnectAttempt: number;
  lastError?: VoiceLastError;
}

/** Why the client says something that is not a wire event. */
export type VoiceNoticeCode =
  /** A message of the core did not match the schema and was ignored. */
  | "malformed_event"
  /** Audio arrived outside a playback, or in a form that cannot be played, and was dropped. */
  | "audio_dropped"
  /** The connection ended while an utterance or a playback was live: both are over, their ids are stale. */
  | "connection_lost"
  /** The capture reached the most audio one utterance may carry and was ended. */
  | "capture_limit"
  /** The service reported that another connection took over this session: this client does not fight for it. */
  | "replaced"
  /** The bounded number of reconnection attempts is spent. */
  | "reconnect_failed";

/**
 * Everything a listener receives: every event of the core exactly as it came (validated), plus the client's own: `state` on every
 * change, `audio.out` for each chunk of verified speech, `input.level` for each chunk of microphone audio the client was given,
 * and `notice` for the rare things a host may want to show.
 */
export type VoiceEvent =
  | ServerEvent
  | { type: "state"; state: VoiceState }
  | { type: "audio.out"; playback_id: string; turn_id: string; format: AudioFormat; data: Uint8Array }
  | { type: "input.level"; level: number; capturing: boolean }
  | { type: "notice"; code: VoiceNoticeCode; message: string };

export type VoiceListener = (event: VoiceEvent) => void;

/** What the host's token provider is told: the voice socket needs one fresh single-use token per connection, reconnections included. */
export interface VoiceTokenRequest {
  operation: "openVoiceSession";
  sessionId: string;
}

/** Supplies a fresh single-use context token for one connection. Called for every connection attempt, never cached. Return the bare token. */
export type VoiceTokenProvider = (request: VoiceTokenRequest) => string | Promise<string>;

/** The part of a WebSocket the voice client uses: the shared socket interface plus `send`. Node's, the browser's, Deno's, Bun's and the `ws` package's all satisfy it. */
export interface VoiceSocketLike extends WebSocketLike {
  send: (data: string | ArrayBuffer | ArrayBufferView) => void;
  binaryType?: string;
}

/** What the client asks a factory to open. There is no subprotocol; the token is in `headers`, never in the URL. */
export interface VoiceSocketInit {
  protocols: string[];
  headers: Record<string, string>;
}

/**
 * Opens a client WebSocket with headers. A browser's WebSocket cannot set `Authorization`, so the host supplies this factory
 * (one line on Node 22+, Deno, Bun or with the `ws` package, see src/ws.ts) or a same-origin proxy that adds the header.
 */
export type VoiceSocketFactory = (url: string, init: VoiceSocketInit) => VoiceSocketLike;

export interface ReconnectOptions {
  /** How many consecutive attempts to replace a lost connection (each one uses a fresh single-use token). Default 3; 0 turns reconnection off. */
  maxAttempts?: number;
  /** The delay before the first attempt after a network loss, doubled for each further one, in milliseconds. Default 250. An expired credential is replaced at once. */
  baseDelayMs?: number;
  /** The longest delay, in milliseconds. Default 5000. */
  maxDelayMs?: number;
}

/** The silence endpointer of hands-free mode. A level is the RMS of a chunk, 0 to 1. */
export interface EndpointerOptions {
  /** A chunk at or above this level is speech. Default 0.02. */
  speechLevel?: number;
  /** Speech this long starts an utterance, in milliseconds. Default 60. */
  minSpeechMs?: number;
  /** Silence this long ends it, in milliseconds. Default 800. */
  silenceMs?: number;
  /** Audio from before the speech was detected that is sent with it, so the first word is not clipped, in milliseconds. Default 300. */
  preRollMs?: number;
  /** The longest utterance, in milliseconds. Default 30000. */
  maxUtteranceMs?: number;
  /**
   * Whether speech while the core is speaking starts an utterance (barge-in). Default false: a microphone next to a loudspeaker hears
   * the reply, and the client has no echo cancellation. `cancelPlayback()` stops speech at any time either way.
   */
  bargeIn?: boolean;
}

export interface VoiceOptions {
  /** The AlixAI service, e.g. "https://alixai.example.com" or "http://127.0.0.1:8080". A path prefix is kept. */
  baseUrl: string;
  /** The conversation session the voice conversation shares with text. It must be the session the context token names. */
  sessionId: string;
  /** Supplies a fresh single-use context token for each connection. */
  getContextToken: VoiceTokenProvider;
  /** Opens the socket (see `VoiceSocketFactory`). There is no other transport: without it `connect` fails closed. */
  webSocket?: VoiceSocketFactory;
  /** Default `push_to_talk`. */
  mode?: VoiceMode;
  /** The user's language (BCP 47): the recognition hint and the language of the reply when the core supports it. */
  language?: string;
  /** `false` asks for text-only replies on this connection. Default true. */
  speak?: boolean;
  /** The audio the host's `pushAudio` and `submitAudio` carry. Default 16 kHz mono 16-bit PCM. */
  format?: AudioFormat;
  reconnect?: ReconnectOptions;
  endpointer?: EndpointerOptions;
  /** How long to wait for `session.started` after the socket opens, in milliseconds. Default 10000. */
  openTimeoutMs?: number;
  /** Allow plain http to a host that is not loopback. The context token is a bearer credential: leave this off. */
  allowInsecureHttp?: boolean;
}

/** One voice conversation on the session. Created by `createVoiceSession`; nothing is opened until `connect`. */
export interface VoiceSession {
  /** Opens the connection and resolves with `session.started`. Rejects with a typed error and does not retry: only a connection that was open is re-opened. */
  connect: () => Promise<VoiceSessionInfo>;
  /** Ends the conversation: live captures and playbacks are over and the socket is closed. Idempotent. */
  close: () => void;
  /** The current state. */
  getState: () => VoiceState;
  /** Listens to every event; returns the function that stops listening. A listener that throws does not affect the others or the connection. */
  on: (listener: VoiceListener) => () => void;
  /** Changes the mode. Leaving `hands_free` ends the capture the endpointer started. */
  setMode: (mode: VoiceMode) => void;

  /** Starts an utterance and streams its audio with `pushAudio`. A capture that starts while speech plays cancels the speech (barge-in). */
  startCapture: (options?: { language?: string }) => void;
  /**
   * Gives the client microphone audio: raw PCM 16-bit little-endian in the session's format (see `float32ToPcm16` for a Web Audio
   * buffer). Push to talk: streamed while a capture is active, otherwise dropped. Hands free: the endpointer starts and ends the
   * utterances. Returns whether the chunk was kept. Throws only for a chunk that is not whole samples.
   */
  pushAudio: (pcm: Int16Array | Uint8Array | ArrayBuffer) => boolean;
  /** No more audio: the transcript is finalised and enters the turn pipeline. */
  endCapture: () => void;
  /** Discards the capture in progress: nothing is transcribed and nothing is kept. */
  cancelCapture: () => void;
  /** Sends a whole utterance in one message (at most `max_audio_bytes`). */
  submitAudio: (pcm: Int16Array | Uint8Array | ArrayBuffer, options?: { language?: string }) => void;
  /** Replaces the transcript of the latest utterance (or the named one) with the user's text, before anything is confirmed. A later utterance makes it `stale`. */
  correctTranscript: (text: string, utteranceId?: string) => void;
  /** Stops the speech at once: no further audio is delivered, without waiting for the core. */
  cancelPlayback: () => void;
}
