// The orb's colours. The kits hand over their design tokens (the accent, the surface the orb sits on, the danger colour) as CSS
// colour strings; the browser turns them into sRGB (`createColorResolver`), and the pure functions below derive the sphere's
// palette from them, so the orb follows the theme without a colour of its own.

/** An sRGB colour, each channel 0 to 1. */
export type Rgb = readonly [number, number, number];

/** CSS colour strings, any syntax the browser understands (oklch, hex, named, color-mix ...). */
export interface VoiceOrbColors {
  /** The colour of the orb. Default: the kits' pine accent. */
  accent?: string;
  /** The surface behind the orb: it decides between the light and the dark rendering. Default: a near-white surface. */
  surface?: string;
  /** The colour of the error state. Default: the kits' danger red. */
  error?: string;
}

export interface OrbBaseColors {
  accent: Rgb;
  surface: Rgb;
  error: Rgb;
}

/** What a frame paints with. `deep` is the shaded side, `light` the lit side and the inner currents, `halo` the glow around it. */
export interface OrbPalette {
  deep: Rgb;
  mid: Rgb;
  light: Rgb;
  halo: Rgb;
  /** Halo opacity at full glow: stronger on a dark surface, where light carries; softer on a light one. */
  haloAlpha: number;
  dark: boolean;
}

// oklch(0.47 0.1 165), oklch(0.996 0.003 85) and oklch(0.5 0.19 27) of the Vue kit, in sRGB.
export const DEFAULT_BASE: Readonly<OrbBaseColors> = Object.freeze({
  accent: [0, 0.423, 0.3] as Rgb,
  surface: [0.999, 0.994, 0.986] as Rgb,
  error: [0.717, 0.099, 0.112] as Rgb,
});

// Never pure black or pure white: the shade and the highlight are tinted.
const SHADE: Rgb = [0.04, 0.06, 0.07];
const HIGHLIGHT: Rgb = [0.97, 0.99, 0.98];

const clamp = (value: number): number => Math.min(1, Math.max(0, value));

export const mix = (a: Rgb, b: Rgb, t: number): Rgb => [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t, a[2] + (b[2] - a[2]) * t];

const linear = (channel: number): number => (channel <= 0.04045 ? channel / 12.92 : ((channel + 0.055) / 1.055) ** 2.4);

/** WCAG relative luminance. */
export const luminance = (color: Rgb): number => 0.2126 * linear(color[0]) + 0.7152 * linear(color[1]) + 0.0722 * linear(color[2]);

/** Saturation (0 grey, 1 as is) and brightness (a multiplier), clamped to sRGB. */
export const adjust = (color: Rgb, saturation: number, brightness: number): Rgb => {
  const grey = 0.2126 * color[0] + 0.7152 * color[1] + 0.0722 * color[2];
  return [clamp((grey + (color[0] - grey) * saturation) * brightness), clamp((grey + (color[1] - grey) * saturation) * brightness), clamp((grey + (color[2] - grey) * saturation) * brightness)];
};

/** The palette of one frame: the accent (or, as `alert` rises, the error colour), shaded for the surface it sits on. */
export const derivePalette = (base: Readonly<OrbBaseColors>, alert: number, saturation: number, brightness: number): OrbPalette => {
  const dark = luminance(base.surface) < 0.25;
  const hue = mix(base.accent, base.error, clamp(alert));
  // On a dark surface the shaded side sinks toward the surface, so the sphere glows out of it; on a light one it deepens.
  // The body is a touch lighter than the accent and the highlight close to white: it reads as light, not as a painted ball.
  const deep = dark ? mix(hue, base.surface, 0.42) : mix(hue, SHADE, 0.14);
  const mid = mix(hue, HIGHLIGHT, dark ? 0.06 : 0.14);
  const light = mix(hue, HIGHLIGHT, dark ? 0.62 : 0.78);
  const tone = (color: Rgb): Rgb => adjust(color, saturation, brightness);
  return { deep: tone(deep), mid: tone(mid), light: tone(light), halo: tone(dark ? mix(hue, HIGHLIGHT, 0.2) : hue), haloAlpha: dark ? 0.42 : 0.3, dark };
};

/** `rgb(r g b / a)` for Canvas 2D. */
export const cssRgb = (color: Rgb, alpha = 1): string =>
  `rgb(${Math.round(clamp(color[0]) * 255)} ${Math.round(clamp(color[1]) * 255)} ${Math.round(clamp(color[2]) * 255)} / ${clamp(alpha).toFixed(3)})`;

/**
 * Turns CSS colour strings into sRGB with the browser's own parser: the colour is painted on a private 1 by 1 canvas and read back.
 * Every syntax the browser knows works, and the orb carries no colour parser. Returns undefined for a value the browser rejects,
 * or where there is no Canvas 2D (a server, a test without canvas).
 */
export const createColorResolver = (): ((value: string | undefined) => Rgb | undefined) => {
  let context: CanvasRenderingContext2D | null | undefined;
  const cache = new Map<string, Rgb | undefined>();
  return (value) => {
    const text = value?.trim() ?? "";
    if (text === "") return undefined;
    if (cache.has(text)) return cache.get(text);
    let result: Rgb | undefined;
    try {
      if (context === undefined) {
        const canvas = typeof document === "undefined" ? undefined : document.createElement("canvas");
        if (canvas !== undefined) {
          canvas.width = 1;
          canvas.height = 1;
        }
        context = canvas?.getContext("2d", { willReadFrequently: true }) ?? null;
      }
      if (context !== null) {
        // An invalid colour leaves fillStyle as it was: two different sentinels tell a rejected value from a real one.
        context.fillStyle = "#010203";
        context.fillStyle = text;
        const first = context.fillStyle;
        context.fillStyle = "#040506";
        context.fillStyle = text;
        if (first !== "#010203" || context.fillStyle !== "#040506") {
          context.clearRect(0, 0, 1, 1);
          context.fillRect(0, 0, 1, 1);
          const pixel = context.getImageData(0, 0, 1, 1).data;
          if ((pixel[3] ?? 0) > 0) result = [(pixel[0] ?? 0) / 255, (pixel[1] ?? 0) / 255, (pixel[2] ?? 0) / 255];
        }
      }
    } catch {
      result = undefined;
    }
    cache.set(text, result);
    return result;
  };
};
