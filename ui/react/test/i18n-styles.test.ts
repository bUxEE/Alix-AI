import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { compile } from "sass";
import { describe, expect, it } from "vitest";
import { ALIXAI_TOKENS, en, fill, it as italian, resolveMessages, themeStyle } from "../src/index.js";

const stylesDir = fileURLToPath(new URL("../src/styles/", import.meta.url));
const sources = readdirSync(stylesDir).filter((name) => name.endsWith(".scss"));

const shape = (value: unknown): unknown =>
  Array.isArray(value) ? value.map(shape) : typeof value === "object" && value !== null ? Object.fromEntries(Object.entries(value).map(([key, inner]) => [key, shape(inner)])) : typeof value;

describe("the words", () => {
  it("has the same keys in English and Italian, and every Italian string is filled", () => {
    expect(shape(italian)).toEqual(shape(en));
    const walk = (value: unknown): string[] => (typeof value === "string" ? [value] : Object.values(value as object).flatMap(walk));
    for (const text of walk(italian)) expect(text.trim()).not.toBe("");
  });

  it("uses no em dash and no emoji in either language", () => {
    const text = JSON.stringify([en, italian]);
    expect(text).not.toMatch(/—|–/);
    expect(text).not.toMatch(/\p{Extended_Pictographic}/u);
  });

  it("picks the language from a tag, falls back to English, and lets a host reword a single word", () => {
    expect(resolveMessages("it-IT").confirmation.approve).toBe("Approva");
    expect(resolveMessages("de").confirmation.approve).toBe("Approve");
    expect(resolveMessages(undefined).confirmation.approve).toBe("Approve");
    const reworded = resolveMessages("it", { confirmation: { approve: "Conferma" } });
    expect(reworded.confirmation.approve).toBe("Conferma");
    expect(reworded.confirmation.decline).toBe("Rifiuta");
    // The built-in text is not changed by an override.
    expect(resolveMessages("it").confirmation.approve).toBe("Approva");
  });

  it("does not let an override reach the prototype", () => {
    resolveMessages("en", JSON.parse('{"__proto__": {"polluted": true}}'));
    expect(({} as Record<string, unknown>)["polluted"]).toBeUndefined();
  });

  it("fills named values and leaves an unknown one visible", () => {
    expect(fill("{a} and {b}", { a: 1 })).toBe("1 and {b}");
  });
});

describe("the styles", () => {
  it("compile with Sass and use only @use, never @import", () => {
    for (const name of sources) expect(readFileSync(join(stylesDir, name), "utf8")).not.toMatch(/@import/);
    const css = compile(join(stylesDir, "index.scss")).css;
    expect(css.length).toBeGreaterThan(1000);
    expect(css).toContain(".alix-sheet");
    expect(css).toContain(".alix-citations");
  });

  it("defines every design token the TypeScript list names, and the list names every token defined", () => {
    const source = readFileSync(join(stylesDir, "_tokens.scss"), "utf8");
    const defined = new Set([...source.matchAll(/--alixai-([a-z0-9-]+):/g)].map((match) => match[1]));
    expect([...defined].sort()).toEqual([...ALIXAI_TOKENS].sort());
  });

  it("reads colour, size and time only from tokens", () => {
    const css = compile(join(stylesDir, "index.scss")).css;
    // The token definitions are the one place a colour literal may appear.
    const body = css
      .split("\n")
      .filter((line) => !/^\s*--alixai-[a-z0-9-]+:/.test(line))
      .join("\n");
    expect(body).not.toMatch(/oklch\(/);
    expect(body).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    expect(body).not.toMatch(/\brgba?\(/);
  });

  it("honours reduced motion, hover only on real pointers, and never animates with transition: all", () => {
    const css = compile(join(stylesDir, "index.scss")).css;
    expect(css).toContain("prefers-reduced-motion: reduce");
    expect(css).toContain("(hover: hover) and (pointer: fine)");
    expect(css).not.toMatch(/transition:\s*all/);
    expect(css).toContain(":focus-visible");
    expect(css).toContain("prefers-color-scheme: dark");
    // No coloured side stripe on a card, and no gradient text.
    expect(css).not.toMatch(/border-(left|right):\s*[2-9]px/);
    expect(css).not.toContain("background-clip: text");
  });

  it("turns token overrides into custom properties for one subtree", () => {
    expect(themeStyle({ accent: "oklch(0.5 0.15 30)", "radius-md": "2px" })).toEqual({ "--alixai-accent": "oklch(0.5 0.15 30)", "--alixai-radius-md": "2px" });
  });
});
