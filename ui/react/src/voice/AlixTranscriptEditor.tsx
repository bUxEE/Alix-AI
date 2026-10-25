import { forwardRef, useId, useImperativeHandle, useRef, useState } from "react";
import type { FormEvent, KeyboardEvent, ReactElement, ReactNode } from "react";
import { useAlixVoiceText } from "./messages.js";
import type { AlixVoiceMessageOverrides } from "./messages.js";

// What the microphone heard, shown as editable text BEFORE anything is confirmed. Speech recognition is a proposal like any other
// model output: the person corrects it here, and only `onConfirm` (with the corrected text) sends it. `onDiscard` drops it.
// The editor changes no state of its own. In Voice Mode, `onConfirm` becomes a correction of the transcript (a new turn that
// replaces the earlier one); standalone, the host decides what to do with the text.
//
// Keyboard: Ctrl or Cmd + Enter sends, Escape discards. `original` is what the recogniser heard: when it differs from the text,
// "Back to what I heard" restores it.

export interface AlixTranscriptEditorProps {
  /** Controlled text. Without it the editor keeps its own, starting at `defaultValue`. */
  value?: string;
  defaultValue?: string;
  onChange?: (text: string) => void;
  onConfirm?: (text: string) => void;
  onDiscard?: () => void;
  disabled?: boolean;
  /** What the recogniser heard. Shown as the way back when the text has been edited. */
  original?: string;
  /** The text is already a correction, not the recogniser's own words. */
  corrected?: boolean;
  /** Replaces the heading. */
  label?: ReactNode;
  /** Replaces the line under the heading. */
  hint?: ReactNode;
  /** Replaces the text of the send button. */
  submitLabel?: ReactNode;
  messages?: AlixVoiceMessageOverrides;
}

export interface AlixTranscriptEditorHandle {
  focus: () => void;
}

export const AlixTranscriptEditor = forwardRef<AlixTranscriptEditorHandle, AlixTranscriptEditorProps>(
  ({ value, defaultValue = "", onChange, onConfirm, onDiscard, disabled = false, original, corrected = false, label, hint, submitLabel, messages }, ref): ReactElement => {
    const t = useAlixVoiceText(messages);
    const uid = useId();
    const field = useRef<HTMLTextAreaElement>(null);
    const [own, setOwn] = useState(defaultValue);
    const text = value ?? own;
    const rows = Math.min(8, Math.max(2, text.split("\n").length));
    const edited = original !== undefined && text !== original;

    useImperativeHandle(ref, () => ({ focus: () => field.current?.focus() }), []);

    const change = (next: string): void => {
      setOwn(next);
      onChange?.(next);
    };
    const confirm = (): void => {
      const corrected = text.trim();
      if (corrected !== "" && !disabled) onConfirm?.(corrected);
    };
    const submit = (event: FormEvent): void => {
      event.preventDefault();
      confirm();
    };
    const keyDown = (event: KeyboardEvent<HTMLTextAreaElement>): void => {
      if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
        event.preventDefault();
        confirm();
      } else if (event.key === "Escape") {
        event.preventDefault();
        onDiscard?.();
      }
    };

    return (
      <form className="alix-voice-editor" data-alixai="transcript-editor" onSubmit={submit}>
        <label htmlFor={uid} className="alix-voice-editor__label">
          <span className="alix-voice-editor__title">
            {label ?? t("voice.transcript")}
            {corrected && <span className="alix-voice-editor__tag">{t("voice.corrected")}</span>}
          </span>
          <span className="alix-voice-editor__hint">{hint ?? t("voice.transcriptHint")}</span>
        </label>
        <textarea
          ref={field}
          id={uid}
          className="alix-voice-editor__field no-validation"
          rows={rows}
          value={text}
          disabled={disabled}
          onChange={(event) => change(event.target.value)}
          onKeyDown={keyDown}
        />
        <div className="alix-voice-editor__actions">
          {edited && (
            <button
              type="button"
              className="alix-voice-editor__button"
              data-variant="quiet"
              disabled={disabled}
              onClick={() => {
                change(original ?? "");
                field.current?.focus();
              }}
            >
              {t("voice.reset")}
            </button>
          )}
          <span className="alix-voice-editor__spacer" />
          <button type="button" className="alix-voice-editor__button" disabled={disabled} onClick={() => onDiscard?.()}>
            {t("common.discard")}
          </button>
          <button type="submit" className="alix-voice-editor__button" data-variant="primary" disabled={disabled || text.trim() === ""}>
            {submitLabel ?? t("common.send")}
          </button>
        </div>
      </form>
    );
  },
);
AlixTranscriptEditor.displayName = "AlixTranscriptEditor";
