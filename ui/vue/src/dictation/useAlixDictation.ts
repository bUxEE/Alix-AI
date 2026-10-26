import { isAlixAIError, voice } from "@alixai/sdk";
import { computed, onBeforeUnmount, onMounted, ref, shallowRef, type Ref } from "vue";
import { useAlix } from "../i18n";
import { createAlixMicrophone } from "./capture";

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
  phase: Ref<DictationPhase>;
  elapsedSeconds: Ref<number>;
  errorMessage: Ref<string>;
  capabilities: Ref<voice.TranscriptionCapabilities | null>;
  available: Ref<boolean>;
  known: Ref<boolean>;
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
  const { t } = useAlix();
  const phase = ref<DictationPhase>("idle");
  const elapsedSeconds = ref(0);
  const errorMessage = ref("");
  const capabilities = shallowRef<voice.TranscriptionCapabilities | null>(null);
  const known = ref(false);
  const available = computed(() => capabilities.value?.available === true);

  const mic = options.microphone ?? createAlixMicrophone();
  const chunks: Float32Array[] = [];
  let sampleRate = 16000;
  let tick: ReturnType<typeof setInterval> | undefined;
  let maxTimer: ReturnType<typeof setTimeout> | undefined;
  let startedAt = 0;
  let abort: AbortController | undefined;

  const clearTimers = (): void => {
    if (tick !== undefined) clearInterval(tick);
    if (maxTimer !== undefined) clearTimeout(maxTimer);
    tick = undefined;
    maxTimer = undefined;
  };

  const resetCapture = (): void => {
    clearTimers();
    mic.stop();
    chunks.length = 0;
    abort?.abort();
    abort = undefined;
  };

  const friendlyError = (error: unknown): string => {
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
  };

  const loadCapabilities = async (): Promise<void> => {
    try {
      capabilities.value = await options.client.capabilities();
    } catch {
      capabilities.value = {
        available: false,
        locality: "none",
        max_seconds: 60,
        sample_rate_hz: 16000,
        format: "audio/wav",
        languages: [],
      };
    } finally {
      known.value = true;
    }
  };

  const cancel = (): void => {
    resetCapture();
    phase.value = "idle";
    errorMessage.value = "";
    elapsedSeconds.value = 0;
  };

  const stop = async (): Promise<void> => {
    if (phase.value !== "recording") return;
    clearTimers();
    mic.stop();
    const samples = concatFloat32(chunks);
    chunks.length = 0;
    if (samples.length < sampleRate * 0.15) {
      phase.value = "error";
      errorMessage.value = t("dictation.error.silent");
      return;
    }
    phase.value = "transcribing";
    abort = new AbortController();
    try {
      const wav = voice.encodeWav16kMono(samples, sampleRate);
      const language = options.language?.();
      const result = await options.client.create(options.sessionId(), wav, {
        ...(language !== undefined && language !== "" && { language }),
        signal: abort.signal,
      });
      const text = result.text.trim();
      if (text !== "") options.onTranscript(text);
      phase.value = "idle";
      errorMessage.value = "";
      elapsedSeconds.value = 0;
    } catch (error) {
      if (abort.signal.aborted) {
        phase.value = "idle";
        return;
      }
      phase.value = "error";
      errorMessage.value = friendlyError(error);
    } finally {
      abort = undefined;
    }
  };

  const start = async (): Promise<void> => {
    if (phase.value === "recording" || phase.value === "transcribing" || phase.value === "requesting") return;
    if (!available.value) {
      phase.value = "error";
      errorMessage.value = t("dictation.error.unavailable");
      return;
    }
    errorMessage.value = "";
    phase.value = "requesting";
    chunks.length = 0;
    try {
      await mic.start((samples, rate) => {
        sampleRate = rate;
        chunks.push(new Float32Array(samples));
      });
    } catch (error) {
      phase.value = "error";
      errorMessage.value = friendlyError(error);
      mic.stop();
      return;
    }
    phase.value = "recording";
    startedAt = Date.now();
    elapsedSeconds.value = 0;
    tick = setInterval(() => {
      elapsedSeconds.value = Math.floor((Date.now() - startedAt) / 1000);
    }, 250);
    const maxSeconds = capabilities.value?.max_seconds ?? 60;
    maxTimer = setTimeout(() => {
      void stop();
    }, maxSeconds * 1000);
  };

  onMounted(() => {
    void loadCapabilities();
  });
  onBeforeUnmount(() => {
    resetCapture();
  });

  return { phase, elapsedSeconds, errorMessage, capabilities, available, known, start, stop, cancel };
};
