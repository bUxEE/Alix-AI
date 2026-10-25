import type { VoiceOrbColors } from "./color.js";

// Reading a kit's design tokens for the orb, and noticing when the theme changes. The kits theme through custom properties on an
// ancestor (`data-theme`, `data-alixai-theme`, a class) or the system colour scheme; both are watched here, and a burst of changes
// costs one read in the next frame.

/** The custom property that holds each colour, e.g. `{ accent: "--alix-accent", surface: "--alix-surface", error: "--alix-danger" }`. */
export interface VoiceOrbTokenNames {
  accent: string;
  surface: string;
  error: string;
}

/** The token values as the element sees them. A token that is not set is left out, so the orb's default applies. */
export const readCssColors = (element: Element, names: VoiceOrbTokenNames): VoiceOrbColors => {
  if (typeof getComputedStyle !== "function") return {};
  const style = getComputedStyle(element);
  const colors: VoiceOrbColors = {};
  for (const key of ["accent", "surface", "error"] as const) {
    const value = style.getPropertyValue(names[key]).trim();
    if (value !== "") colors[key] = value;
  }
  return colors;
};

const WATCHED = ["class", "style", "data-theme", "data-alixai-theme", "data-reduced-motion"];

/**
 * Calls `onChange` (at most once a frame) when the colour scheme of the system changes or an ancestor of `element` changes its
 * class, style or theme attributes. Returns the function that stops watching.
 */
export const watchPresentation = (element: Element, onChange: () => void): (() => void) => {
  if (typeof window === "undefined") return () => undefined;
  let pending: number | undefined;
  let stopped = false;
  const fire = (): void => {
    if (pending !== undefined || stopped) return;
    const run = (): void => {
      pending = undefined;
      if (!stopped) onChange();
    };
    pending = typeof window.requestAnimationFrame === "function" ? window.requestAnimationFrame(run) : (setTimeout(run, 16) as unknown as number);
  };
  const media = typeof window.matchMedia === "function" ? window.matchMedia("(prefers-color-scheme: dark)") : undefined;
  media?.addEventListener?.("change", fire);
  const observer = typeof MutationObserver === "function" ? new MutationObserver(fire) : undefined;
  for (let node: Element | null = element.parentElement; node !== null && observer !== undefined; node = node.parentElement) {
    observer.observe(node, { attributes: true, attributeFilter: WATCHED });
  }
  return () => {
    stopped = true;
    media?.removeEventListener?.("change", fire);
    observer?.disconnect();
    if (pending !== undefined && typeof window.cancelAnimationFrame === "function") window.cancelAnimationFrame(pending);
  };
};
