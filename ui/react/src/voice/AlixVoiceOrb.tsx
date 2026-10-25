import { useEffect, useRef, useState } from "react";
import type { ReactElement } from "react";
import { createVoiceOrb, readCssColors, watchPresentation } from "@alixai/voice-orb";
import type { VoiceOrb, VoiceOrbColors, VoiceOrbRenderer, VoiceOrbState } from "@alixai/voice-orb";
import { useAlixVoiceText } from "./messages.js";
import type { AlixVoiceMessageKey, AlixVoiceMessageOverrides } from "./messages.js";

// The voice orb (@alixai/voice-orb): a soft, luminous sphere that breathes when idle, swells with the person's voice while
// listening, swirls while the assistant thinks and pulses while it speaks. The Vue kit's AlixVoiceOrb draws the same orb.
//
// It is an image to a screen reader, named after its state; the words of Voice Mode carry the state for everyone else too. Its
// colours are the kit's tokens (accent, surface, danger), read again when the theme changes. Under reduced motion (the system's,
// a `data-reduced-motion="true"` ancestor, or this prop) it holds still and shows the state through size and brightness only. The
// orb, its canvas and its GPU context are created in an effect and released on unmount: nothing runs on a server.

export interface AlixVoiceOrbProps {
  state?: VoiceOrbState;
  /** The voice level, 0 to 1: the microphone while listening, the speech while speaking. */
  level?: number;
  /** The orb's side: a number of pixels or any CSS length. Default 160. */
  size?: number | string;
  /** Replaces the state's name for screen readers. */
  label?: string;
  /** Hold still even where motion is allowed. Reduced motion asked by the system applies anyway. */
  reducedMotion?: boolean;
  /** Colours instead of the kit's tokens, as CSS colours. */
  colors?: VoiceOrbColors;
  className?: string;
  messages?: AlixVoiceMessageOverrides;
}

const TOKENS = { accent: "--alixai-accent", surface: "--alixai-surface", error: "--alixai-danger" };

const useSystemStill = (): boolean => {
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

export const AlixVoiceOrb = ({ state = "idle", level = 0, size = 160, label, reducedMotion = false, colors, className, messages }: AlixVoiceOrbProps): ReactElement => {
  const t = useAlixVoiceText(messages);
  const hostRef = useRef<HTMLSpanElement>(null);
  const orbRef = useRef<VoiceOrb | undefined>(undefined);
  const [renderer, setRenderer] = useState<VoiceOrbRenderer | undefined>(undefined);
  const [attributeStill, setAttributeStill] = useState(false);
  const systemStill = useSystemStill();
  const still = reducedMotion || systemStill || attributeStill;
  // The latest props, for the effect that builds the orb once and for the theme watcher.
  const latest = useRef({ state, level, still, colors });
  latest.current = { state, level, still, colors };

  useEffect(() => {
    const host = hostRef.current;
    if (host === null) return undefined;
    const colorsOf = (): VoiceOrbColors => ({ ...readCssColors(host, TOKENS), ...latest.current.colors });
    const providerStill = (): boolean => host.closest('[data-reduced-motion="true"]') !== null;
    setAttributeStill(providerStill());
    const orb = createVoiceOrb(host, {
      state: latest.current.state,
      level: latest.current.level,
      colors: colorsOf(),
      reducedMotion: latest.current.still || providerStill(),
      onRendererChange: setRenderer,
    });
    orbRef.current = orb;
    setRenderer(orb.renderer);
    const stopWatching = watchPresentation(host, () => {
      setAttributeStill(providerStill());
      orb.setColors(colorsOf());
    });
    return () => {
      stopWatching();
      orb.destroy();
      orbRef.current = undefined;
    };
  }, []);

  useEffect(() => orbRef.current?.setState(state), [state]);
  useEffect(() => orbRef.current?.setLevel(level), [level]);
  useEffect(() => orbRef.current?.setReducedMotion(still), [still]);
  const colorKey = JSON.stringify(colors ?? {});
  useEffect(() => {
    const host = hostRef.current;
    if (host !== null) orbRef.current?.setColors({ ...readCssColors(host, TOKENS), ...latest.current.colors });
  }, [colorKey]);
  const side = typeof size === "number" ? `${size}px` : size;
  useEffect(() => orbRef.current?.resize(), [side]);

  return (
    <span
      ref={hostRef}
      className={className === undefined ? "alix-voice-orb" : `alix-voice-orb ${className}`}
      data-alixai="voice-orb"
      data-state={state}
      data-renderer={renderer}
      role="img"
      aria-label={label ?? t(`voice.orb.${state}` as AlixVoiceMessageKey)}
      style={{ width: side, height: side }}
    />
  );
};
