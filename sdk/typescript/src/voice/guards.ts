import { isErrorCode } from "../errors.js";
import type { AudioFormat, ServerEvent } from "./types.js";

// Hand-written guards for what the core sends. The SDK never acts on a message it cannot recognise: an unknown type, a missing or
// mistyped member, or an id of the wrong shape makes the whole message `undefined`, and the client reports a `malformed_event`
// notice and goes on. The generated types describe the schema; these check the data against it (the cross-field rules included).

const UTTERANCE_ID = /^utt_[0-9A-HJKMNP-TV-Z]{26}$/;
const PLAYBACK_ID = /^pbk_[0-9A-HJKMNP-TV-Z]{26}$/;
const VOICE_SESSION_ID = /^vss_[0-9A-HJKMNP-TV-Z]{26}$/;
const LANGUAGE = /^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$/;

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const isString = (value: unknown): value is string => typeof value === "string";
const isNonEmpty = (value: unknown): value is string => typeof value === "string" && value.length > 0;
const isPositiveInt = (value: unknown): value is number => typeof value === "number" && Number.isInteger(value) && value >= 1;
const isId = (pattern: RegExp, value: unknown): value is string => typeof value === "string" && pattern.test(value);
const isLanguage = (value: unknown): value is string => typeof value === "string" && value.length <= 35 && LANGUAGE.test(value);
const oneOf = <T extends string>(values: readonly T[], value: unknown): value is T => typeof value === "string" && (values as readonly string[]).includes(value);

/** An optional member: absent is fine, present must pass the check. */
const optional = (record: Record<string, unknown>, key: string, check: (value: unknown) => boolean): boolean =>
  record[key] === undefined || check(record[key]);

const EFFECTS = ["text_input_only", "verified_text_only", "manual_turn_taking"] as const;
const CAPTURE_CANCEL_REASONS = ["client", "no_speech", "stt_failed", "invalid_audio", "retention_failed", "overloaded", "pack_changed", "session_closed"] as const;
const PLAYBACK_CANCEL_REASONS = ["client", "barge_in", "error", "pack_changed", "session_closed"] as const;
const RESPONSE_SOURCES = ["model", "template"] as const;
const RESPONSE_SPEECH = ["queued", "skipped"] as const;
const SPEECH_REASONS = ["speech_disabled", "tts_unavailable", "verification_failed", "superseded", "pack_changed"] as const;

export const isAudioFormat = (value: unknown): value is AudioFormat =>
  isRecord(value) &&
  value["encoding"] === "pcm_s16le" &&
  typeof value["sample_rate"] === "number" &&
  Number.isInteger(value["sample_rate"]) &&
  value["sample_rate"] >= 8000 &&
  value["sample_rate"] <= 48000 &&
  typeof value["channels"] === "number" &&
  Number.isInteger(value["channels"]) &&
  value["channels"] >= 1 &&
  value["channels"] <= 2;

const isSpeakerHint = (value: unknown): boolean =>
  isRecord(value) && isNonEmpty(value["label"]) && typeof value["confidence"] === "number" && value["confidence"] >= 0 && value["confidence"] <= 1;

const isErrorBody = (value: unknown): boolean =>
  isRecord(value) && isErrorCode(value["code"]) && isString(value["message"]) && typeof value["retryable"] === "boolean" && optional(value, "details", isRecord);

// The confirmation secret: the schema asks for a non-empty string (at most 512). Its exact shape is checked where it is used, by
// the main client's confirm call, so a secret of a shape this SDK does not know yet never costs the host the whole turn.
const isConfirmation = (value: unknown): boolean =>
  isRecord(value) && isNonEmpty(value["preparation_id"]) && isNonEmpty(value["token"]) && value["token"].length <= 512;

/** A text message of the core as a ServerEvent, or `undefined` when it is not one this SDK knows or it does not match its schema. */
export const parseServerEvent = (text: string): ServerEvent | undefined => {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return undefined;
  }
  if (!isRecord(raw)) return undefined;
  const e = raw;
  const utterance = isId(UTTERANCE_ID, e["utterance_id"]);
  const turn = isNonEmpty(e["turn_id"]);
  switch (e["type"]) {
    case "session.started":
      return isId(VOICE_SESSION_ID, e["voice_session_id"]) &&
        isNonEmpty(e["session_id"]) &&
        typeof e["speech"] === "boolean" &&
        typeof e["retain_raw_audio"] === "boolean" &&
        isPositiveInt(e["max_chunk_bytes"]) &&
        isPositiveInt(e["max_audio_bytes"]) &&
        optional(e, "language", isLanguage) &&
        optional(e, "pack_id", isNonEmpty)
        ? (raw as unknown as ServerEvent)
        : undefined;
    case "capture.started":
      return utterance ? (raw as unknown as ServerEvent) : undefined;
    case "capture.cancelled":
      return utterance && oneOf(CAPTURE_CANCEL_REASONS, e["reason"]) ? (raw as unknown as ServerEvent) : undefined;
    case "transcript.interim":
      return utterance && isPositiveInt(e["seq"]) && isString(e["text"]) ? (raw as unknown as ServerEvent) : undefined;
    case "transcript.final":
      return utterance && isNonEmpty(e["text"]) && optional(e, "language", isLanguage) && optional(e, "speaker_hint", isSpeakerHint)
        ? (raw as unknown as ServerEvent)
        : undefined;
    case "transcript.corrected":
      return utterance && isNonEmpty(e["text"]) && isNonEmpty(e["previous_text"]) ? (raw as unknown as ServerEvent) : undefined;
    case "turn.completed": {
      if (!(utterance && turn && isNonEmpty(e["act"]) && typeof e["failed"] === "boolean")) return undefined;
      if (!(optional(e, "code", isErrorCode) && optional(e, "preparation_id", isNonEmpty))) return undefined;
      if (!(optional(e, "corrects_turn_id", isNonEmpty) && optional(e, "superseded_preparation_id", isNonEmpty))) return undefined;
      if (e["confirmation"] !== undefined) {
        // The secret is only ever the secret of the preparation the same message names.
        if (!isConfirmation(e["confirmation"])) return undefined;
        if ((e["confirmation"] as Record<string, unknown>)["preparation_id"] !== e["preparation_id"]) return undefined;
      }
      return raw as unknown as ServerEvent;
    }
    case "response":
      return utterance &&
        turn &&
        isNonEmpty(e["text"]) &&
        oneOf(RESPONSE_SOURCES, e["source"]) &&
        oneOf(RESPONSE_SPEECH, e["speech"]) &&
        optional(e, "speech_reason", (v) => oneOf(SPEECH_REASONS, v))
        ? (raw as unknown as ServerEvent)
        : undefined;
    case "playback.started":
      return isId(PLAYBACK_ID, e["playback_id"]) && turn && isAudioFormat(e["format"]) ? (raw as unknown as ServerEvent) : undefined;
    case "playback.done":
      return isId(PLAYBACK_ID, e["playback_id"]) && turn ? (raw as unknown as ServerEvent) : undefined;
    case "playback.cancelled":
      return isId(PLAYBACK_ID, e["playback_id"]) && turn && oneOf(PLAYBACK_CANCEL_REASONS, e["reason"]) ? (raw as unknown as ServerEvent) : undefined;
    case "error":
      return isErrorBody(e["error"]) &&
        optional(e, "effect", (v) => oneOf(EFFECTS, v)) &&
        optional(e, "utterance_id", (v) => isId(UTTERANCE_ID, v)) &&
        optional(e, "turn_id", isNonEmpty)
        ? (raw as unknown as ServerEvent)
        : undefined;
    default:
      return undefined;
  }
};
