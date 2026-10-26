import type { CSSProperties } from "react";

// The names of the design tokens (src/styles/_tokens.scss is the source of their values). A host overrides them in its own
// stylesheet, or for one subtree with `themeStyle`. test/styles.test.ts keeps this list and the stylesheet in step.

export const ALIXAI_TOKENS = [
  "surface",
  "surface-raised",
  "surface-sunken",
  "border",
  "border-strong",
  "text",
  "text-muted",
  "accent",
  "accent-hover",
  "on-accent",
  "accent-soft",
  "ok",
  "ok-soft",
  "warn",
  "warn-soft",
  "danger",
  "danger-hover",
  "danger-soft",
  "on-danger",
  "focus",
  "chart-1",
  "chart-2",
  "chart-3",
  "chart-4",
  "chart-5",
  "chart-6",
  "scrim",
  "font",
  "font-mono",
  "text-xs",
  "text-sm",
  "text-md",
  "text-lg",
  "leading",
  "space-1",
  "space-2",
  "space-3",
  "space-4",
  "space-5",
  "space-6",
  "radius-sm",
  "radius-md",
  "radius-lg",
  "target",
  "ease-out",
  "ease-in-out",
  "duration-press",
  "duration-fade",
  "duration-enter",
  "z-fullscreen",
  "shadow",
] as const;

export type AlixToken = (typeof ALIXAI_TOKENS)[number];

/** Inline style that overrides tokens for one subtree: `<div style={themeStyle({ accent: "oklch(0.5 0.15 30)" })}>`. */
export const themeStyle = (overrides: Partial<Record<AlixToken, string>>): CSSProperties => {
  const style: Record<string, string> = {};
  for (const [name, value] of Object.entries(overrides)) if (value !== undefined) style[`--alixai-${name}`] = value;
  return style as CSSProperties;
};
