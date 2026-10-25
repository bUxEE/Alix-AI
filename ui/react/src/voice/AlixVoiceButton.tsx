import { useRef } from "react";
import type { KeyboardEvent, MouseEvent, PointerEvent, ReactElement, ReactNode } from "react";
import { useAlixVoiceText } from "./messages.js";
import type { AlixVoiceMessageOverrides } from "./messages.js";

// The microphone button. It reports start, stop and cancel and owns no audio: Voice Mode (or the host, with `useAlixVoice`)
// captures and sends.
//
// Two ways to use it. `toggle`: press to start, press again to stop. `push-to-talk`: hold to talk, release to stop, with the
// keyboard too (hold Space or Enter). Escape while listening cancels: nothing is sent. While listening the button is a pressed
// toggle button, so a screen reader hears the state. A screen reader's own "activate" is a click with no pointer and no key, so in
// push-to-talk it toggles instead (holding is not possible with a virtual cursor). Losing focus or the pointer while holding
// releases, so the microphone never stays open behind the person's back.

export interface AlixVoiceButtonProps {
  state?: "idle" | "listening" | "processing" | "unavailable";
  mode?: "toggle" | "push-to-talk";
  size?: "md" | "lg";
  onStart?: () => void;
  onStop?: () => void;
  onCancel?: () => void;
  /** Replaces the icon. */
  icon?: ReactNode;
  "aria-describedby"?: string;
  messages?: AlixVoiceMessageOverrides;
}

const MIC = ["M12 3a3 3 0 0 0-3 3v6a3 3 0 0 0 6 0V6a3 3 0 0 0-3-3z", "M5 11a7 7 0 0 0 14 0", "M12 18v3"];
const STOP = ["M7 7h10v10H7z"];

export const AlixVoiceIcon = ({ name, size = 20 }: { name: "mic" | "stop" | "check"; size?: number }): ReactElement => (
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
    {(name === "mic" ? MIC : name === "stop" ? STOP : ["M5 12.5l4.5 4.5L19 7.5"]).map((d) => (
      <path key={d} d={d} />
    ))}
  </svg>
);

export const AlixVoiceButton = ({
  state = "idle",
  mode = "toggle",
  size = "md",
  onStart,
  onStop,
  onCancel,
  icon,
  "aria-describedby": describedBy,
  messages,
}: AlixVoiceButtonProps): ReactElement => {
  const t = useAlixVoiceText(messages);
  const held = useRef(false);
  const listening = state === "listening";
  const inert = state === "unavailable" || state === "processing";
  const label = (() => {
    if (state === "unavailable") return t("voice.unavailable");
    if (state === "processing") return t("voice.processing");
    if (mode === "push-to-talk") return t("voice.hold");
    return listening ? t("voice.stop") : t("voice.start");
  })();

  const toggle = (): void => {
    if (listening) onStop?.();
    else onStart?.();
  };
  const click = (event: MouseEvent<HTMLButtonElement>): void => {
    if (inert) return;
    if (mode === "toggle" || event.detail === 0) toggle();
  };
  // The press is remembered here, not read back from `state`: the host may take a moment to open the microphone, and a release in
  // that moment must still be reported, or the microphone would open after the person let go.
  const down = (event?: PointerEvent<HTMLButtonElement>): void => {
    if (mode !== "push-to-talk" || inert || listening || held.current) return;
    if (event !== undefined && event.button > 0) return;
    try {
      // Keep receiving the pointer when the finger or mouse slides off the button: only lifting it releases.
      event?.currentTarget.setPointerCapture?.(event.pointerId);
    } catch {
      // a synthetic or already-released pointer
    }
    held.current = true;
    onStart?.();
  };
  const up = (): void => {
    if (!held.current) return;
    held.current = false;
    onStop?.();
  };
  const keyDown = (event: KeyboardEvent<HTMLButtonElement>): void => {
    if (event.key === "Escape" && (listening || held.current)) {
      event.preventDefault();
      held.current = false; // the release that follows is not a "send"
      onCancel?.();
      return;
    }
    if (mode === "push-to-talk" && (event.key === " " || event.key === "Enter")) {
      event.preventDefault();
      if (!event.repeat) down();
    }
  };
  const keyUp = (event: KeyboardEvent<HTMLButtonElement>): void => {
    if (mode === "push-to-talk" && (event.key === " " || event.key === "Enter")) {
      event.preventDefault();
      up();
    }
  };

  return (
    <button
      type="button"
      className="alix-voice__talk"
      data-alixai="voice-button"
      data-mode={mode}
      data-size={size}
      data-state={state}
      aria-label={label}
      aria-pressed={listening}
      aria-disabled={inert}
      aria-describedby={describedBy}
      aria-keyshortcuts={mode === "push-to-talk" ? "Space Enter" : undefined}
      onBlur={up}
      onClick={click}
      onContextMenu={(event) => {
        // A long press is how this button is used on a touch screen: no browser menu over it.
        if (mode === "push-to-talk") event.preventDefault();
      }}
      onKeyDown={keyDown}
      onKeyUp={keyUp}
      onPointerCancel={up}
      onPointerDown={down}
      onPointerUp={up}
    >
      {icon ?? <AlixVoiceIcon name={listening && mode === "toggle" ? "stop" : "mic"} size={size === "lg" ? 28 : 20} />}
    </button>
  );
};
