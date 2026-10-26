import type { IncomingMessage, ServerResponse } from "node:http";
import { BodyTooLargeError, readBody, sendError, sendJson } from "../http";
import type { DemoLocale } from "../prompts";

// Dictation HTTP (record-to-text, 1.1.0): GET /v1/dictations/capabilities and
// POST /v1/sessions/{session_id}/dictations. Not the voice WebSocket.

const CAPABILITIES_PATH = "/v1/dictations/capabilities";
const CREATE_PATH = /^\/v1\/sessions\/([A-Za-z0-9_.-]{1,128})\/dictations$/;
/** ~60 s of PCM s16le mono 16 kHz plus WAV header. */
const MAX_BODY = 44 + 60 * 16000 * 2;

const SCRIPTED: Record<DemoLocale, string> = {
  en: "Book a fire-safety inspection at Riverside for Tuesday morning",
  it: "Prenota un'ispezione antincendio a Riverside per martedì mattina",
};

export type DictationMode = "available" | "disabled" | "unavailable";

export interface DictationHandler {
  handle: (req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams) => Promise<boolean>;
}

const localeOf = (language: string | undefined): DemoLocale =>
  language?.toLowerCase().startsWith("it") === true ? "it" : "en";

const contentType = (req: IncomingMessage): string =>
  (req.headers["content-type"] ?? "").split(";")[0]?.trim().toLowerCase() ?? "";

/** True when the WAV has no usable PCM, or the PCM samples are all near silence. */
const isSilentOrTooShort = (wav: Buffer): boolean => {
  if (wav.byteLength < 44 + 16000 * 0.15 * 2) return true;
  if (wav.toString("ascii", 0, 4) !== "RIFF" || wav.toString("ascii", 8, 12) !== "WAVE") return true;
  const pcm = wav.subarray(44);
  let peak = 0;
  for (let i = 0; i + 1 < pcm.length; i += 2) {
    const sample = Math.abs(pcm.readInt16LE(i));
    if (sample > peak) peak = sample;
    if (peak > 64) return false;
  }
  return true;
};

const durationMs = (wav: Buffer): number => Math.max(1, Math.floor((wav.byteLength - 44) / 32));

export const createDictationHandler = (mode: DictationMode = "available"): DictationHandler => {
  const handle = async (req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams): Promise<boolean> => {
    const method = req.method ?? "GET";
    if (method === "GET" && path === CAPABILITIES_PATH) {
      if (mode === "unavailable") {
        sendError(res, 503, "unavailable", "the server has no transcription service");
        return true;
      }
      const available = mode === "available";
      sendJson(res, 200, {
        available,
        locality: available ? "local" : "none",
        max_seconds: 60,
        sample_rate_hz: 16000,
        format: "audio/wav",
        languages: available ? ["en", "it"] : [],
      });
      return true;
    }

    const created = CREATE_PATH.exec(path);
    if (method === "POST" && created !== null) {
      if (mode !== "available") {
        sendError(res, 503, "unavailable", "the server has no transcription service");
        return true;
      }
      if (contentType(req) !== "audio/wav") {
        sendError(res, 415, "validation_failed", "Content-Type must be audio/wav");
        return true;
      }
      let body: Buffer;
      try {
        body = await readBody(req, MAX_BODY);
      } catch (error) {
        if (error instanceof BodyTooLargeError) {
          sendError(res, 413, "validation_failed", "the clip is longer than max_seconds");
          return true;
        }
        throw error;
      }
      if (body.byteLength < 44 || body.toString("ascii", 0, 4) !== "RIFF") {
        sendError(res, 400, "validation_failed", "the body is not a WAV", { reason: "malformed_wav" });
        return true;
      }
      if (isSilentOrTooShort(body)) {
        sendError(res, 400, "validation_failed", "the clip is silent or too short", { reason: "silent" });
        return true;
      }
      const language = query.get("language") ?? undefined;
      const locale = localeOf(language);
      sendJson(res, 200, {
        text: SCRIPTED[locale],
        language: language && language.length > 0 ? language : locale,
        duration_ms: durationMs(body),
        locality: "local",
        segment_kind: "user_recorded",
      });
      return true;
    }

    return false;
  };

  return { handle };
};
