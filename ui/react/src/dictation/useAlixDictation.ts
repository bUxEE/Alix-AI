import { isAlixAIError, voice } from "@alixai/sdk";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createAlixMicrophone } from "./capture.js";
import { useAlixDictationText } from "./messages.js";

export type DictationPhase = "idle" | "requesting" | "recording" | "transcribing" | "error";

export type VoiceModeOption = "off" | "dictation" | "conversation";

export interface UseAlixDictationOptions {
  sessionId: () => string;
  client: voice.DictationClient;
  language?: () => string | undefined;
  /** Called with the recognised text; the host inserts it into the composer and must not send. */
  onTranscript: (text: string) => void;
  /** Optional microphone; defaults to createAlixMicrophone(). */
  microphone?: ReturnType<typeof createAlixMicrophone>;
}

export interface AlixDictation {
  phase: DictationPhase;
  elapsedSeconds: number;
  errorMessage: string;
  capabilities: voice.TranscriptionCapabilities | null;
  available: boolean;
  known: boolean;
  start: () => Promise<void>;
  stop: () => Promise<void>;
  cancel: () => void;
}

const concatFloat32 = (chunks: Float32Array[]): Float32Array => {
  const total = chunks.reduce((n, c) => n + c.length, 0);
  const out = new Float32Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
};

export const useAlixDictation = (options: UseAlixDictationOptions): AlixDictation => {
  const t = useAlixDictationText();
  const [phase, setPhase] = useState<DictationPhase>("idle");
  const [elapsedSeconds, setElapsedSeconds] = useState(0);
  const [errorMessage, setErrorMessage] = useState("");
  const [capabilities, setCapabilities] = useState<voice.TranscriptionCapabilities | null>(null);
  const [known, setKnown] = useState(false);
  const phaseRef = useRef(phase);
  phaseRef.current = phase;

  const mic = useMemo(() => options.microphone ?? createAlixMicrophone(), [options.microphone]);
  const chunksRef = useRef<Float32Array[]>([]);
  const sampleRateRef = useRef(16000);
  const tickRef = useRef<ReturnType<typeof setInterval> | undefined>(undefined);
  const maxTimerRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const startedAtRef = useRef(0);
  const abortRef = useRef<AbortController | undefined>(undefined);
  const optionsRef = useRef(options);
  optionsRef.current = options;
  const capabilitiesRef = useRef(capabilities);
  capabilitiesRef.current = capabilities;

  const available = capabilities?.available === true;

  const clearTimers = useCallback((): void => {
    if (tickRef.current !== undefined) clearInterval(tickRef.current);
    if (maxTimerRef.current !== undefined) clearTimeout(maxTimerRef.current);
    tickRef.current = undefined;
    maxTimerRef.current = undefined;
  }, []);

  const resetCapture = useCallback((): void => {
    clearTimers();
    mic.stop();
    chunksRef.current = [];
    abortRef.current?.abort();
    abortRef.current = undefined;
  }, [clearTimers, mic]);

  const friendlyError = useCallback(
    (error: unknown): string => {
      if (error instanceof DOMException && (error.name === "NotAllowedError" || error.name === "PermissionDeniedError")) {
        return t("dictation.error.permission");
      }
      if (isAlixAIError(error)) {
        if (error.code === "unavailable" || error.code === "permission_denied" || error.code === "resource_exhausted") {
          return t("dictation.error.unavailable");
        }
        if (error.code === "validation_failed" && (error.details?.reason === "silent" || error.details?.reason === "too_short")) {
          return t("dictation.error.silent");
        }
      }
      return t("dictation.error.generic");
    },
    [t],
  );

  useEffect(() => {
    const loadCapabilities = async (): Promise<void> => {
      try {
        setCapabilities(await optionsRef.current.client.capabilities());
      } catch {
        setCapabilities({
          available: false,
          locality: "none",
          max_seconds: 60,
          sample_rate_hz: 16000,
          format: "audio/wav",
          languages: [],
        });
      } finally {
        setKnown(true);
      }
    };
    void loadCapabilities();
  }, [options.client]);

  const cancel = useCallback((): void => {
    resetCapture();
    setPhase("idle");
    setErrorMessage("");
    setElapsedSeconds(0);
  }, [resetCapture]);

  const stop = useCallback(async (): Promise<void> => {
    if (phaseRef.current !== "recording") return;
    clearTimers();
    mic.stop();
    const sampleRate = sampleRateRef.current;
    const samples = concatFloat32(chunksRef.current);
    chunksRef.current = [];
    if (samples.length < sampleRate * 0.15) {
      setPhase("error");
      setErrorMessage(t("dictation.error.silent"));
      return;
    }
    setPhase("transcribing");
    abortRef.current = new AbortController();
    const abort = abortRef.current;
    try {
      const wav = voice.encodeWav16kMono(samples, sampleRate);
      const language = optionsRef.current.language?.();
      const result = await optionsRef.current.client.create(optionsRef.current.sessionId(), wav, {
        ...(language !== undefined && language !== "" && { language }),
        signal: abort.signal,
      });
      const text = result.text.trim();
      if (text !== "") optionsRef.current.onTranscript(text);
      setPhase("idle");
      setErrorMessage("");
      setElapsedSeconds(0);
    } catch (error) {
      if (abort.signal.aborted) {
        setPhase("idle");
        return;
      }
      setPhase("error");
      setErrorMessage(friendlyError(error));
    } finally {
      abortRef.current = undefined;
    }
  }, [clearTimers, friendlyError, mic, t]);

  const stopRef = useRef(stop);
  stopRef.current = stop;

  const start = useCallback(async (): Promise<void> => {
    const current = phaseRef.current;
    if (current === "recording" || current === "transcribing" || current === "requesting") return;
    if (capabilitiesRef.current?.available !== true) {
      setPhase("error");
      setErrorMessage(t("dictation.error.unavailable"));
      return;
    }
    setErrorMessage("");
    setPhase("requesting");
    chunksRef.current = [];
    try {
      await mic.start((samples, rate) => {
        sampleRateRef.current = rate;
        chunksRef.current.push(new Float32Array(samples));
      });
    } catch (error) {
      setPhase("error");
      setErrorMessage(friendlyError(error));
      mic.stop();
      return;
    }
    setPhase("recording");
    startedAtRef.current = Date.now();
    setElapsedSeconds(0);
    tickRef.current = setInterval(() => {
      setElapsedSeconds(Math.floor((Date.now() - startedAtRef.current) / 1000));
    }, 250);
    const maxSeconds = capabilitiesRef.current?.max_seconds ?? 60;
    maxTimerRef.current = setTimeout(() => {
      void stopRef.current();
    }, maxSeconds * 1000);
  }, [friendlyError, mic, t]);

  useEffect(() => () => resetCapture(), [resetCapture]);

  return { phase, elapsedSeconds, errorMessage, capabilities, available, known, start, stop, cancel };
};
