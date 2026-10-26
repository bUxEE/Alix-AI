import { useCallback, useMemo } from "react";
import type { KeyboardEvent, ReactElement } from "react";
import type { voice } from "@alixai/sdk";
import { useAlixDictationText } from "./messages.js";
import { useAlixDictation } from "./useAlixDictation.js";

const MIC = ["M12 3a3 3 0 0 0-3 3v6a3 3 0 0 0 6 0V6a3 3 0 0 0-3-3z", "M5 11a7 7 0 0 0 14 0", "M12 18v3"];
const STOP = ["M7 7h10v10H7z"];

const DictationIcon = ({ name, size = 20 }: { name: "mic" | "stop"; size?: number }): ReactElement => (
  <svg
    width={size}
    height={size}
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    strokeWidth={1.75}
    strokeLinecap="round"
    strokeLinejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    {(name === "mic" ? MIC : STOP).map((d) => (
      <path
        key={d}
        d={d}
      />
    ))}
  </svg>
);

export interface AlixDictationButtonProps {
  sessionId: string;
  client: voice.DictationClient;
  language?: string;
  onTranscript: (text: string) => void;
  onTranscriptEvent?: (text: string) => void;
  size?: "md" | "lg";
}

export const AlixDictationButton = ({
  sessionId,
  client,
  language,
  onTranscript,
  onTranscriptEvent,
  size = "md",
}: AlixDictationButtonProps): ReactElement | null => {
  const t = useAlixDictationText();
  const dictation = useAlixDictation({
    sessionId: () => sessionId,
    client,
    language: () => language,
    onTranscript: (text) => {
      onTranscript(text);
      onTranscriptEvent?.(text);
    },
  });

  const recording = dictation.phase === "recording";
  const busy = dictation.phase === "requesting" || dictation.phase === "transcribing";

  const label = useMemo(() => {
    if (dictation.phase === "requesting") return t("dictation.requesting");
    if (dictation.phase === "transcribing") return t("dictation.transcribing");
    if (recording) return t("dictation.stop");
    return t("dictation.start");
  }, [dictation.phase, recording, t]);

  const live = useMemo(() => {
    if (recording) return t("dictation.recording", { seconds: dictation.elapsedSeconds });
    if (dictation.phase === "transcribing") return t("dictation.transcribing");
    if (dictation.phase === "error") return dictation.errorMessage;
    return "";
  }, [dictation.elapsedSeconds, dictation.errorMessage, dictation.phase, recording, t]);

  const toggle = useCallback((): void => {
    if (busy) return;
    if (recording) void dictation.stop();
    else void dictation.start();
  }, [busy, dictation, recording]);

  const onKeydown = useCallback(
    (event: KeyboardEvent<HTMLButtonElement>): void => {
      if (event.key === "Escape" && (recording || busy)) {
        event.preventDefault();
        dictation.cancel();
      }
    },
    [busy, dictation, recording],
  );

  if (!dictation.known || !dictation.available) return null;

  return (
    <span className="alix-dictation">
      <button
        type="button"
        className="alix-root alix-dictation__btn"
        data-alixai="dictation-button"
        data-size={size}
        data-state={dictation.phase}
        aria-label={label}
        aria-pressed={recording}
        aria-busy={busy}
        aria-disabled={busy}
        onClick={toggle}
        onKeyDown={onKeydown}
      >
        {busy ? (
          <span
            className="alix-loadable__spinner"
            aria-hidden="true"
          />
        ) : (
          <DictationIcon
            name={recording ? "stop" : "mic"}
            size={size === "lg" ? 28 : 20}
          />
        )}
        <span className="alix-dictation__sr">{label}</span>
      </button>
      {recording && (
        <span
          className="alix-dictation__elapsed"
          aria-hidden="true"
        >
          {dictation.elapsedSeconds}s
        </span>
      )}
      <span
        className="alix-dictation__sr"
        aria-live="polite"
      >
        {live}
      </span>
      {dictation.phase === "error" && dictation.errorMessage !== "" && (
        <p
          className="alix-dictation__error"
          role="alert"
        >
          {dictation.errorMessage}
        </p>
      )}
    </span>
  );
};
