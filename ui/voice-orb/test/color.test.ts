import { describe, expect, it } from "vitest";
import { adjust, cssRgb, DEFAULT_BASE, derivePalette, luminance, mix } from "../src/color.js";
import type { Rgb } from "../src/color.js";

const DARK_SURFACE: Rgb = [0.125, 0.117, 0.099];

describe("the palette", () => {
  it("is shaded for a light surface: a deeper shade, a lighter highlight, a soft halo", () => {
    const palette = derivePalette(DEFAULT_BASE, 0, 1, 1);
    expect(palette.dark).toBe(false);
    expect(luminance(palette.deep)).toBeLessThan(luminance(palette.mid));
    expect(luminance(palette.light)).toBeGreaterThan(luminance(palette.mid));
    expect(luminance(palette.mid)).toBeGreaterThan(luminance(DEFAULT_BASE.accent));
    expect(palette.haloAlpha).toBeLessThan(derivePalette({ ...DEFAULT_BASE, surface: DARK_SURFACE }, 0, 1, 1).haloAlpha);
  });

  it("glows out of a dark surface", () => {
    const palette = derivePalette({ ...DEFAULT_BASE, surface: DARK_SURFACE }, 0, 1, 1);
    expect(palette.dark).toBe(true);
    expect(palette.haloAlpha).toBeGreaterThan(0.35);
  });

  it("never paints pure black or pure white", () => {
    for (const surface of [DEFAULT_BASE.surface, DARK_SURFACE]) {
      const palette = derivePalette({ accent: [1, 1, 1], surface, error: [0, 0, 0] }, 0, 1, 1);
      expect(palette.light).not.toEqual([1, 1, 1]);
      const shaded = derivePalette({ accent: [0, 0, 0], surface, error: [0, 0, 0] }, 0, 1, 1);
      expect(luminance(shaded.light)).toBeGreaterThan(0.3);
    }
  });

  it("turns to the error colour as the alert rises, and greys out with saturation", () => {
    const alarmed = derivePalette(DEFAULT_BASE, 1, 1, 1).mid;
    expect(alarmed[0]).toBeGreaterThan(alarmed[1] + 0.3);
    const grey = derivePalette(DEFAULT_BASE, 0, 0, 1).mid;
    expect(grey[0]).toBeCloseTo(grey[1], 6);
    expect(grey[1]).toBeCloseTo(grey[2], 6);
  });
});

describe("colour helpers", () => {
  it("mix, adjust and write CSS", () => {
    expect(mix([0, 0, 0], [1, 0.5, 0.2], 0.5)).toEqual([0.5, 0.25, 0.1]);
    expect(adjust([0.5, 0.5, 0.5], 0.3, 2)).toEqual([1, 1, 1]);
    expect(adjust([0.2, 0.4, 0.6], 1, 1)).toEqual([0.2, 0.4, 0.6]);
    expect(cssRgb([1, 0, 0.5], 0.25)).toBe("rgb(255 0 128 / 0.250)");
    expect(luminance([1, 1, 1])).toBeCloseTo(1);
    expect(luminance([0, 0, 0])).toBe(0);
  });
});
