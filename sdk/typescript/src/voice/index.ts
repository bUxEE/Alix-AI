// Voice client surface (realtime voice conversations on a session, transcripts, verified speech). src/index.ts re-exports this file as
// the `voice` namespace, so everything below is `voice.<name>` to a host.
//
//   const session = voice.createVoiceSession({ baseUrl, sessionId, getContextToken, webSocket });
//   session.on((event) => { if (event.type === "state") render(event.state); });
//   await session.connect();
//   session.startCapture();          // push to talk: stream the microphone with session.pushAudio(pcm) ...
//   session.endCapture();            // ... and the transcript arrives as transcript.interim / transcript.final events
//   session.correctTranscript("book Tuesday, not Thursday");
//
// Voice never confirms anything: a prepared action is confirmed on the host's screen through the main client, with the secret that
// `turn.completed.confirmation` carries.

export { createVoiceSession } from "./client.js";
export { DEFAULT_AUDIO_FORMAT } from "./types.js";
export { float32ToPcm16, pcmLevel, resampleFloat32 } from "./audio.js";
export { encodeWav16kMono, DICTATION_SAMPLE_RATE_HZ } from "./wav.js";
export { createDictationClient, dictations } from "./dictation.js";
export type {
  CreateDictationOptions,
  DictationClient,
  DictationClientOptions,
  DictationContextTokenProvider,
  DictationContextTokenRequest,
} from "./dictation.js";
export { DICTATION_OPERATIONS } from "./dictation-operations.js";
export type { DictationOperationId, DictationOperationSpec } from "./dictation-operations.js";
export { isDictationTranscript, isTranscriptionCapabilities } from "./dictation-guards.js";
export { parseServerEvent } from "./guards.js";
export { createFakeVoiceTransport } from "./testing.js";
export type { FakeVoiceTransport } from "./testing.js";
export type { DictationTranscript } from "../gen/voice/dictation-transcript.js";
export type { TranscriptionCapabilities, TranscriptionCapabilitiesLocality } from "../gen/voice/transcription-capabilities.js";
export type {
  AudioFormat,
  CaptureCancelled,
  CaptureStarted,
  ClientMessage,
  Effect,
  EndpointerOptions,
  PlaybackCancelled,
  PlaybackDone,
  PlaybackStarted,
  PlaybackStateName,
  ReconnectOptions,
  RecordingState,
  ServerEvent,
  SessionStarted,
  SpeakerHint,
  TranscriptCorrected,
  TranscriptFinal,
  TranscriptInterim,
  TurnCompleted,
  TurnCompletedConfirmation,
  TurnStateName,
  VoiceConnectionState,
  VoiceErrorEvent,
  VoiceEvent,
  VoiceLastError,
  VoiceListener,
  VoiceMode,
  VoiceNoticeCode,
  VoiceOptions,
  VoiceResponse,
  VoiceResponseEvent,
  VoiceSession,
  VoiceSessionInfo,
  VoiceSocketFactory,
  VoiceSocketInit,
  VoiceSocketLike,
  VoiceState,
  VoiceTokenProvider,
  VoiceTokenRequest,
  VoiceTranscript,
} from "./types.js";
