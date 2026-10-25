import type { voice } from "@alixai/sdk";

// The shapes shared by the voice components, the controller that drives them and the audio ports they speak to. Nothing here
// knows Vue: the React kit carries the same file, so both kits show the same states and report the same events.

/**
 * What Voice Mode shows. `unavailable`: no speech input here (the deployment has no voice pack, the socket could not be opened);
 * the typed path keeps working. `ended`: the voice session is over for good (another connection took it, or the bounded
 * reconnection ran out) and the host has to supply a fresh one.
 */
export type AlixVoicePhase = "idle" | "connecting" | "listening" | "processing" | "speaking" | "reconnecting" | "unavailable" | "ended";

/** Why the person is asked to type: no voice pack or no socket, or the microphone cannot be used. */
export type AlixVoiceFallback = "text_input_only" | "connect_failed" | "microphone_unavailable";

/** A line the person may need once: the connection was renewed (a capture in progress is over), or one utterance hit the limit. */
export type AlixVoiceNotice = "renewed" | "capture_limit";

export interface AlixVoiceTranscript {
  text: string;
  /** True while it is the recogniser's current guess. */
  interim: boolean;
  /** True when the person replaced the recogniser's text. */
  corrected: boolean;
}

/** The verified reply. Its text is always shown; `spoken` says whether it is also read aloud. */
export interface AlixVoiceReply {
  text: string;
  spoken: boolean;
}

export interface AlixVoiceView {
  phase: AlixVoicePhase;
  handsFree: boolean;
  /** The microphone is open (hands free keeps it open between utterances). */
  micOpen: boolean;
  /** Bar heights, 0 to 1, oldest first. All zero unless listening or speaking. */
  levels: readonly number[];
  transcript?: AlixVoiceTranscript;
  reply?: AlixVoiceReply;
  fallback?: AlixVoiceFallback;
  /** Replies arrive as text only on this connection (Lite, or no speech synthesis). Voice input still works. */
  textReplies: boolean;
  /** The last turn prepared an action: the person confirms it on screen, never by voice. */
  confirmationRequired: boolean;
  /** The last correction replaced a turn that had prepared an action: that card is stale. */
  supersededPreparationId?: string;
  notice?: AlixVoiceNotice;
  error?: { code: string; message: string; retryable: boolean };
}

export interface AlixVoiceTurn {
  turnId: string;
  act: string;
  failed: boolean;
  /** The action awaiting confirmation, if the turn prepared one. The secret that confirms it is not carried here. */
  preparationId?: string;
  confirmationRequired: boolean;
  /** The correction replaced a turn that had prepared this action: retire its card. */
  supersededPreparationId?: string;
}

/** What the controller reports besides its view. None of it carries audio or a confirmation secret. */
export type AlixVoiceEvent =
  | { type: "recording"; recording: boolean }
  | { type: "transcript"; text: string; final: boolean; corrected: boolean }
  | { type: "response"; text: string; spoken: boolean }
  | { type: "turn"; turn: AlixVoiceTurn }
  | { type: "interrupt" }
  | { type: "correct"; text: string }
  | { type: "fallback"; reason: AlixVoiceFallback }
  | { type: "ended"; reason: "replaced" | "reconnect_failed" | "closed" }
  | { type: "error"; code: string; message: string; retryable: boolean };

export type AlixVoiceListener = (event: AlixVoiceEvent) => void;

/** Where microphone audio comes from. The default is the browser's (`createAlixMicrophone`); a host or a test supplies its own. */
export interface AlixMicrophone {
  /** Starts capturing and calls `onChunk` with mono samples in [-1, 1]. Rejects when the microphone cannot be used. */
  start: (onChunk: (samples: Float32Array, sampleRate: number) => void) => Promise<void>;
  /** Stops at once, also a start that is still waiting for permission. */
  stop: () => void;
}

/** Where verified speech goes. The default plays through Web Audio (`createAlixPlayer`). */
export interface AlixPlayer {
  enqueue: (format: voice.AudioFormat, data: Uint8Array) => void;
  /** Silences everything queued, at once. */
  cancel: () => void;
  close: () => void;
  isPlaying: () => boolean;
  /** Called when nothing is playing any more (it finished, or `cancel` ran). Returns the function that stops listening. */
  onIdle: (listener: () => void) => () => void;
}

export interface AlixVoiceControllerOptions {
  /** The voice conversation, created by the host (`voice.createVoiceSession`): only the host has the token provider and the socket factory. */
  session: voice.VoiceSession;
  microphone?: AlixMicrophone;
  /** `null` plays nothing (the replies are only shown). */
  player?: AlixPlayer | null;
  /** The language of the person's speech (BCP 47), a hint for the recogniser. */
  language?: string;
  /** The sample rate the session's audio format carries. Default 16000, the session's default. */
  sampleRate?: number;
  /** Open the connection when the controller is created. Default true. */
  autoConnect?: boolean;
  /** The host already knows speech input is not offered (a 503 `text_input_only` from its own check): never connect. */
  unavailable?: boolean;
  handsFree?: boolean;
}

export interface AlixVoiceController {
  getView: () => AlixVoiceView;
  /** Calls the listener after every change of the view. Returns the function that stops listening. */
  subscribe: (listener: () => void) => () => void;
  /** Listens to the discrete events (recording, transcript, response, turn, interrupt, correct, fallback, ended, error). */
  on: (listener: AlixVoiceListener) => () => void;
  connect: () => Promise<void>;
  /** Opens the microphone and starts listening: a push-to-talk capture, or the hands-free stream. */
  start: () => Promise<void>;
  /** Push to talk: ends the utterance. Hands free: closes the microphone and ends the utterance in progress. */
  stop: () => void;
  /** Throws the utterance in progress away. */
  cancel: () => void;
  /** Stops the speech now. */
  interrupt: () => void;
  setHandsFree: (on: boolean) => void;
  /** Replaces the last transcript with the person's text and runs a new turn. Returns false when there is nothing to send. */
  correct: (text: string) => boolean;
  /** Tries the connection again after it could not be opened. */
  retry: () => void;
  /** Releases the microphone, the player and the listeners. The session is the host's: it is not closed. */
  destroy: () => void;
}
