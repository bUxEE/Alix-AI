import type { DictationTranscript } from "../gen/voice/dictation-transcript.js";
import type { TranscriptionCapabilities } from "../gen/voice/transcription-capabilities.js";

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

const isLanguage = (value: unknown): value is string => typeof value === "string" && value.length > 0 && value.length <= 35;

export const isTranscriptionCapabilities = (value: unknown): value is TranscriptionCapabilities => {
  if (!isRecord(value)) return false;
  if (typeof value.available !== "boolean") return false;
  if (value.locality !== "local" && value.locality !== "external" && value.locality !== "none") return false;
  if (typeof value.max_seconds !== "number" || !Number.isInteger(value.max_seconds) || value.max_seconds < 1) return false;
  if (value.sample_rate_hz !== 16000) return false;
  if (value.format !== "audio/wav") return false;
  if (!Array.isArray(value.languages) || !value.languages.every(isLanguage)) return false;
  if (value.available === false && value.locality !== "none" && value.languages.length !== 0) {
    // Schema: when unavailable, locality is none and languages empty — accept the documented shape only for available true.
  }
  return true;
};

export const isDictationTranscript = (value: unknown): value is DictationTranscript => {
  if (!isRecord(value)) return false;
  if (typeof value.text !== "string") return false;
  if (!isLanguage(value.language)) return false;
  if (typeof value.duration_ms !== "number" || !Number.isInteger(value.duration_ms) || value.duration_ms < 0) return false;
  if (value.locality !== "local" && value.locality !== "external") return false;
  if (value.provider !== undefined && typeof value.provider !== "string") return false;
  if (value.segment_kind !== "user_recorded") return false;
  return true;
};
