import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import type { KeyboardEvent, ReactElement, ReactNode } from "react";
import type { voice } from "@alixai/sdk";
import { voiceOrbStateFor } from "@alixai/voice-orb";
import { AlixTranscriptEditor } from "./AlixTranscriptEditor.js";
import type { AlixTranscriptEditorHandle } from "./AlixTranscriptEditor.js";
import { AlixVoiceButton, AlixVoiceIcon } from "./AlixVoiceButton.js";
import { AlixVoiceOrb } from "./AlixVoiceOrb.js";
import { useAlixVoiceText } from "./messages.js";
import type { AlixVoiceMessageKey, AlixVoiceMessageOverrides } from "./messages.js";
import type {
  AlixMicrophone,
  AlixPlayer,
  AlixVoiceEvent,
  AlixVoiceFallback,
  AlixVoicePhase,
  AlixVoiceReply,
  AlixVoiceTurn,
} from "./types.js";
import { useAlixVoice } from "./useAlixVoice.js";

// Voice Mode: a calm, full-attention surface for talking with the assistant, in two ways to use it.
//
//   * Pass a `session` (from `voice.createVoiceSession`, made by the host: only the host has the token provider and the socket
//     factory). The component opens the microphone, plays the verified speech, and shows everything from the session's own state.
//   * Or pass `state`, `levels`, `transcript` and `reply` yourself and handle `onStart` and `onStop`: the component then owns no audio.
//
// It shows the state in words (never colour alone), the voice orb (it swells with the microphone level while listening, swirls
// while the assistant thinks and pulses with its speech; `renderWaveform` replaces it), the live transcript (which the person
// can correct before anything is confirmed), and the typed reply, always, whether it is also spoken or not. Turn taking, not full duplex: pressing
// talk while the assistant speaks stops the speech first. A spoken "yes" confirms nothing: a prepared action is confirmed with the
// confirmation sheet on screen, and the component says so. Where voice is not offered (no voice pack, no socket, no microphone) it
// falls back to text: `renderFallback` is where the host puts its typed input.

export interface AlixVoiceControlsContext {
  state: "idle" | "listening" | "processing" | "unavailable";
  mode: "toggle" | "push-to-talk";
  handsFree: boolean;
  start: () => void;
  stop: () => void;
  cancel: () => void;
  end: () => void;
}

export interface AlixVoiceModeProps {
  state?: AlixVoicePhase;
  /** Voice levels, 0 to 1, oldest first: the orb follows the newest. With a `session` the component reads them itself. */
  levels?: readonly number[];
  /** The transcript so far, shown as captions. */
  transcript?: string;
  /** The transcript is still the recogniser's guess. */
  interim?: boolean;
  /** The typed reply of the assistant. With a `session` this is the verified response of the last turn. */
  reply?: string;
  session?: voice.VoiceSession;
  microphone?: AlixMicrophone;
  /** `null` plays nothing: replies are only shown. */
  player?: AlixPlayer | null;
  /** The language of the person's speech (BCP 47). */
  language?: string;
  /** Open the connection when the component appears. Default true. */
  autoConnect?: boolean;
  /** The host already knows speech input is not offered here (its own 503 `text_input_only`). */
  unavailable?: boolean;
  /** Let the person correct the transcript. Default true. */
  editable?: boolean;
  handsFree?: boolean;
  defaultHandsFree?: boolean;
  onHandsFreeChange?: (on: boolean) => void;
  onStart?: () => void;
  onStop?: () => void;
  onCancel?: () => void;
  onEnd?: () => void;
  /** The voice session is over for good: the host supplies a fresh `session`. */
  onRestart?: () => void;
  onInterrupt?: () => void;
  onCorrect?: (text: string) => void;
  onRecordingChange?: (recording: boolean) => void;
  onTranscript?: (transcript: { text: string; final: boolean; corrected: boolean }) => void;
  onResponse?: (reply: AlixVoiceReply) => void;
  /** A turn finished. It says whether an action now awaits confirmation on screen; the secret that confirms it is not here. */
  onTurn?: (turn: AlixVoiceTurn) => void;
  onFallback?: (reason: AlixVoiceFallback) => void;
  onError?: (error: { code: string; message: string; retryable: boolean }) => void;
  /** Replaces the orb: called with the levels (bar heights, 0 to 1, oldest first). */
  renderWaveform?: (levels: readonly number[]) => ReactNode;
  renderTranscript?: (text: string) => ReactNode;
  renderReply?: (reply: AlixVoiceReply) => ReactNode;
  /** The host's typed input, for when voice is not offered. Called with the reason, or undefined while voice works. */
  renderFallback?: (reason: AlixVoiceFallback | undefined) => ReactNode;
  renderControls?: (context: AlixVoiceControlsContext) => ReactNode;
  messages?: AlixVoiceMessageOverrides;
}

const BARS = 24;

const useStillMotion = (): boolean => {
  const [still, setStill] = useState(false);
  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return undefined;
    const media = window.matchMedia("(prefers-reduced-motion: reduce)");
    const update = (): void => setStill(media.matches);
    update();
    media.addEventListener?.("change", update);
    return () => media.removeEventListener?.("change", update);
  }, []);
  return still;
};

export const AlixVoiceMode = ({
  state = "idle",
  levels = [],
  transcript = "",
  interim = false,
  reply = "",
  session,
  microphone,
  player,
  language,
  autoConnect = true,
  unavailable = false,
  editable = true,
  handsFree: handsFreeProp,
  defaultHandsFree = false,
  onHandsFreeChange,
  onStart,
  onStop,
  onCancel,
  onEnd,
  onRestart,
  onInterrupt,
  onCorrect,
  onRecordingChange,
  onTranscript,
  onResponse,
  onTurn,
  onFallback,
  onError,
  renderWaveform,
  renderTranscript,
  renderReply,
  renderFallback,
  renderControls,
  messages,
}: AlixVoiceModeProps): ReactElement => {
  const t = useAlixVoiceText(messages);
  const uid = useId();
  const stillQuery = useStillMotion();
  const sectionRef = useRef<HTMLElement>(null);
  const editorRef = useRef<AlixTranscriptEditorHandle>(null);
  const editButtonRef = useRef<HTMLButtonElement>(null);
  const [ownHandsFree, setOwnHandsFree] = useState(defaultHandsFree);
  const handsFree = handsFreeProp ?? ownHandsFree;
  const [attributeStill, setAttributeStill] = useState(false);
  useEffect(() => {
    setAttributeStill(sectionRef.current?.closest('[data-reduced-motion="true"]') != null);
  }, []);
  const still = stillQuery || attributeStill;

  // Discrete events of the controller become callbacks. None of them carries audio or a confirmation secret.
  const handlers = useRef({ onRecordingChange, onTranscript, onResponse, onTurn, onInterrupt, onCorrect, onFallback, onError });
  handlers.current = { onRecordingChange, onTranscript, onResponse, onTurn, onInterrupt, onCorrect, onFallback, onError };
  const onEvent = useCallback((event: AlixVoiceEvent): void => {
    const current = handlers.current;
    switch (event.type) {
      case "recording":
        current.onRecordingChange?.(event.recording);
        break;
      case "transcript":
        current.onTranscript?.({ text: event.text, final: event.final, corrected: event.corrected });
        break;
      case "response":
        current.onResponse?.({ text: event.text, spoken: event.spoken });
        break;
      case "turn":
        current.onTurn?.(event.turn);
        break;
      case "interrupt":
        current.onInterrupt?.();
        break;
      case "correct":
        current.onCorrect?.(event.text);
        break;
      case "fallback":
        current.onFallback?.(event.reason);
        break;
      case "error":
        current.onError?.({ code: event.code, message: event.message, retryable: event.retryable });
        break;
      default:
        break;
    }
  }, []);

  const options = useMemo(
    () =>
      session === undefined
        ? undefined
        : {
            session,
            autoConnect,
            unavailable,
            handsFree,
            ...(microphone !== undefined && { microphone }),
            ...(player !== undefined && { player }),
            ...(language !== undefined && { language }),
          },
    // Only a different session, microphone or player rebuilds the controller (see useAlixVoice); the rest is read when it is made.
    [session, microphone, player],
  );
  const { view, controller } = useAlixVoice(options, onEvent);

  const changeHandsFree = (on: boolean): void => {
    setOwnHandsFree(on);
    onHandsFreeChange?.(on);
  };
  useEffect(() => {
    controller?.setHandsFree(handsFree);
  }, [controller, handsFree]);

  // ---- what to draw: the controller's view when there is a session, the props otherwise ----
  const phase: AlixVoicePhase = view?.phase ?? (unavailable ? "unavailable" : state);
  const transcriptText = view?.transcript?.text ?? transcript;
  const transcriptInterim = view?.transcript?.interim ?? interim;
  const transcriptCorrected = view?.transcript?.corrected ?? false;
  const replyText = view?.reply?.text ?? reply;
  const replySpoken = view?.reply?.spoken ?? false;
  const fallback: AlixVoiceFallback | undefined = view?.fallback ?? (phase === "unavailable" ? "text_input_only" : undefined);
  const textOnly = fallback === "text_input_only" || fallback === "connect_failed";
  const speaking = phase === "speaking";
  const source = view?.levels ?? levels;
  const bars: readonly number[] = still
    ? Array.from({ length: BARS }, () => (phase === "listening" || phase === "speaking" ? 0.4 : 0.08))
    : source.length > 0
      ? source
      : Array.from({ length: BARS }, () => 0.08);

  // The orb follows the newest level (already shaped like a bar height by the controller) and the phase, through the mapping both
  // kits share.
  const orbLevel = source.length > 0 ? (source[source.length - 1] ?? 0) : 0;
  const orbState = voiceOrbStateFor(phase, fallback);

  const buttonState: AlixVoiceControlsContext["state"] = (() => {
    if (phase === "unavailable" || phase === "ended") return "unavailable";
    if (view?.micOpen === true && handsFree) return "listening"; // hands free stays on until the person stops it
    if (phase === "listening") return "listening";
    if (phase === "processing" || phase === "connecting" || phase === "reconnecting") return "processing";
    return "idle"; // including "speaking": pressing talk interrupts
  })();
  const buttonMode = handsFree ? "toggle" : "push-to-talk";
  const noticeKey: AlixVoiceMessageKey | undefined =
    view?.notice === "renewed" ? "voice.renewed" : view?.notice === "capture_limit" ? "voice.captureLimit" : undefined;

  // ---- actions ----
  const start = (): void => {
    onStart?.();
    void controller?.start();
  };
  const stop = (): void => {
    onStop?.();
    controller?.stop();
  };
  const cancel = (): void => {
    onCancel?.();
    controller?.cancel();
  };
  const interrupt = (): void => {
    if (controller === undefined) onInterrupt?.();
    else controller.interrupt();
  };

  // ---- correcting the transcript ----
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const canEdit = editable && transcriptText !== "" && !transcriptInterim && phase !== "unavailable";
  const edit = (): void => {
    setDraft(transcriptText);
    setEditing(true);
  };
  useEffect(() => {
    if (editing) editorRef.current?.focus();
  }, [editing]);
  const closeEditor = (): void => {
    setEditing(false);
    window.setTimeout(() => editButtonRef.current?.focus(), 0);
  };
  const correct = (text: string): void => {
    if (controller === undefined) onCorrect?.(text);
    else controller.correct(text);
    closeEditor();
  };
  // A new transcript (the next utterance, or the core's reading of a correction) replaces whatever was being edited.
  useEffect(() => {
    setEditing(false);
  }, [transcriptText]);

  const keyDown = (event: KeyboardEvent): void => {
    if (event.key === "Escape" && speaking) {
      event.preventDefault();
      interrupt();
    }
  };

  const hint = t(handsFree ? "voice.keyHint.handsFree" : "voice.keyHint.hold");
  const hintId = `${uid}-hint`;
  const announcement = transcriptText !== "" && !transcriptInterim ? t("voice.heard", { text: transcriptText }) : "";
  const controlsContext: AlixVoiceControlsContext = { state: buttonState, mode: buttonMode, handsFree, start, stop, cancel, end: () => onEnd?.() };

  return (
    <section
      ref={sectionRef}
      className="alix-voice"
      data-alixai="voice-mode"
      data-phase={phase}
      aria-label={t("voice.mode.title")}
      aria-busy={phase === "processing" || phase === "connecting"}
      onKeyDown={keyDown}
    >
      <p className="alix-voice__status" role="status">
        <span className="alix-voice__dot" data-phase={phase} aria-hidden="true" />
        {t(`voice.state.${phase}` as AlixVoiceMessageKey)}
      </p>

      {textOnly ? (
        <p className="alix-voice__banner" role="status">
          {t("voice.fallback.text")}
          {fallback === "connect_failed" && (
            <button type="button" className="alix-voice__button" data-variant="quiet" onClick={() => controller?.retry()}>
              {t("common.retry")}
            </button>
          )}
        </p>
      ) : (
        fallback === "microphone_unavailable" && (
          <p className="alix-voice__banner" data-tone="warn" role="status">
            {t("voice.fallback.microphone")}
          </p>
        )
      )}
      {phase === "reconnecting" && (
        <p className="alix-voice__banner" data-tone="warn" role="status">
          {t("common.reconnecting")}
        </p>
      )}
      {phase === "ended" && (
        <div className="alix-voice__banner" role="status">
          {t("voice.sessionEnded")}
          <button type="button" className="alix-voice__button" onClick={() => onRestart?.()}>
            {t("voice.restart")}
          </button>
        </div>
      )}
      {noticeKey !== undefined && (
        <p className="alix-voice__banner" role="status">
          {t(noticeKey)}
        </p>
      )}

      {renderWaveform?.(bars) ?? <AlixVoiceOrb state={orbState} level={orbLevel} reducedMotion={still} size={168} messages={messages} />}

      {speaking && (
        <button type="button" className="alix-voice__button" onClick={interrupt}>
          <AlixVoiceIcon name="stop" size={16} />
          {t("voice.interrupt")}
        </button>
      )}

      {transcriptText !== "" && (
        <div className="alix-voice__block">
          <p className="alix-voice__caption" aria-live="off" {...(transcriptInterim && { "data-interim": "" })}>
            {renderTranscript?.(transcriptText) ?? transcriptText}
          </p>
          {transcriptCorrected && <span className="alix-voice__tag">{t("voice.corrected")}</span>}
          {canEdit && !editing && (
            <button ref={editButtonRef} type="button" className="alix-voice__button" data-variant="quiet" onClick={edit}>
              {t("voice.edit")}
            </button>
          )}
        </div>
      )}
      {editing && (
        <AlixTranscriptEditor
          ref={editorRef}
          value={draft}
          original={transcriptText}
          corrected={transcriptCorrected}
          submitLabel={t("voice.sendCorrection")}
          messages={messages}
          onChange={setDraft}
          onConfirm={correct}
          onDiscard={closeEditor}
        />
      )}
      <p className="alix-voice__sr" aria-live="polite">
        {announcement}
      </p>

      {replyText !== "" && (
        <div className="alix-voice__block" data-align="start">
          <p className="alix-voice__label">{t("voice.reply")}</p>
          <div className="alix-voice__reply" aria-live="polite">
            {renderReply?.({ text: replyText, spoken: replySpoken }) ?? replyText}
          </div>
        </div>
      )}
      {view?.textReplies === true && !textOnly && <p className="alix-voice__note">{t("voice.textReplies")}</p>}

      {view?.confirmationRequired === true && (
        <p className="alix-voice__banner" data-tone="warn" role="status">
          {t("voice.confirmOnScreen")}
        </p>
      )}
      {view?.supersededPreparationId !== undefined && (
        <p className="alix-voice__banner" role="status">
          {t("voice.superseded")}
        </p>
      )}

      {renderFallback?.(fallback)}

      {renderControls?.(controlsContext) ?? (
        <>
          <div className="alix-voice__controls">
            <label className="alix-voice__check">
              <input
                type="checkbox"
                className="no-validation"
                checked={handsFree}
                disabled={textOnly || phase === "ended"}
                onChange={(event) => changeHandsFree(event.target.checked)}
              />
              {t("voice.handsFree")}
            </label>
            <AlixVoiceButton
              size="lg"
              mode={buttonMode}
              state={buttonState}
              aria-describedby={hintId}
              messages={messages}
              onStart={start}
              onStop={stop}
              onCancel={cancel}
            />
            <button type="button" className="alix-voice__button" onClick={() => onEnd?.()}>
              {t("voice.end")}
            </button>
          </div>
          {!textOnly && phase !== "ended" && (
            <p id={hintId} className="alix-voice__hint">
              {hint}
            </p>
          )}
        </>
      )}
    </section>
  );
};
