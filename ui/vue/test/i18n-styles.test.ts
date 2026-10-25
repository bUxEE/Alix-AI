// @vitest-environment node
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { en } from "../src/i18n/en";
import { it as italian } from "../src/i18n/it";
import { createAlixConfig } from "../src/i18n";
import { buildChart, niceTicks } from "../src/chart";
import { parseMarkdown } from "../src/markdown";
import { readPath, writePath } from "../src/settings-fields";

const placeholders = (text: string): string[] => [...text.matchAll(/\{(\w+)\}/g)].map((match) => match[1] as string).sort();

describe("localisation", () => {
  it("English and Italian define the same keys with the same placeholders, and Italian is really Italian", () => {
    expect(Object.keys(italian).sort()).toEqual(Object.keys(en).sort());
    for (const [key, text] of Object.entries(en)) {
      expect(placeholders(italian[key as keyof typeof en]), key).toEqual(placeholders(text));
      expect(italian[key as keyof typeof en].trim(), key).not.toBe("");
    }
    const same = Object.keys(en).filter((key) => italian[key as keyof typeof en] === en[key as keyof typeof en]);
    expect(same.length / Object.keys(en).length).toBeLessThan(0.1);
  });

  it("resolves a locale from the full tag to its language to English, lets a host override, and pluralises", () => {
    const base = createAlixConfig({ locale: "it-IT" });
    expect(base.t("common.send")).toBe("Invia");
    expect(createAlixConfig({ locale: "fr" }).t("common.send")).toBe("Send");
    const custom = createAlixConfig({ locale: "it", messages: { it: { "common.send": "Spedisci" }, fr: { "common.send": "Envoyer" } } });
    expect(custom.t("common.send")).toBe("Spedisci");
    expect(createAlixConfig({ locale: "fr", messages: { fr: { "common.send": "Envoyer" } } }).t("common.send")).toBe("Envoyer");
    expect(base.plural("citations.count", 1)).toBe("1 fonte");
    expect(base.plural("citations.count", 3)).toBe("3 fonti");
    expect(createAlixConfig().plural("citations.count", 1)).toBe("1 source");
    expect(base.t("composer.tooMany", { max: 4 })).toBe("Puoi allegare al massimo 4 file.");
  });

  it("formats sizes and dates in the locale", () => {
    expect(createAlixConfig().formatSize(2048)).toBe("2 KB");
    expect(createAlixConfig().formatSize(3 * 1024 * 1024 + 512 * 1024)).toBe("3.5 MB");
    expect(createAlixConfig({ locale: "it" }).formatSize(1536)).toBe("1,5 KB");
    expect(createAlixConfig({ timeZone: "UTC" }).formatTime("2030-01-01T10:30:00Z")).toMatch(/10:30/);
    expect(createAlixConfig().formatDateTime("not a date")).toBe("");
  });
});

describe("stylesheet", () => {
  const require = createRequire(import.meta.url);
  const sass = require("sass") as typeof import("sass");
  const css = sass.compile(fileURLToPath(new URL("../src/styles/index.scss", import.meta.url))).css;

  it("compiles with modern Sass and defines tokens only under .alix-root", () => {
    expect(css).toContain(".alix-root");
    expect(css).toContain("--alix-accent:");
    expect(css).not.toMatch(/(^|\n):root\s*\{/);
  });

  it("themes through tokens: dark by scheme or attribute, more contrast, and reduced motion by media query or attribute", () => {
    expect(css).toMatch(/prefers-color-scheme:\s*dark/);
    expect(css).toContain('[data-theme=dark]');
    expect(css).toMatch(/prefers-contrast:\s*more/);
    expect(css).toMatch(/prefers-reduced-motion:\s*reduce/);
    expect(css).toContain('[data-reduced-motion=true]');
  });

  it("provides prefixed utilities with responsive variants generated in the shared stylesheet", () => {
    for (const rule of [".alix-d-flex", ".alix-ga-3", ".alix-pa-4", ".alix-d-md-flex", ".alix-flex-md-row", ".alix-col-md-6", ".alix-sr-only"]) expect(css).toContain(rule);
    expect(css).not.toMatch(/\.d-flex\b|\.ga-3\b/);
    expect(css).toMatch(/\.alix-ga-3\s*\{\s*gap:\s*12px/);
  });
});

describe("pure helpers", () => {
  it("parses markdown into a closed tree and survives an unfinished stream", () => {
    const blocks = parseMarkdown("## Title\n- one\n- two\n\n```js\nlet x = 1\n");
    expect(blocks.map((block) => block.kind)).toEqual(["heading", "list", "code"]);
    expect(blocks[2]).toMatchObject({ kind: "code", language: "js", text: "let x = 1" });
    expect(parseMarkdown("2 * 3 * 4 = 24 and snake_case_name")).toEqual([{ kind: "paragraph", inline: [{ kind: "text", text: "2 * 3 * 4 = 24 and snake_case_name" }] }]);
  });

  it("reads the backslash escapes the core's sanitiser writes, and keeps a backslash inside code", () => {
    // Each source is what internal/blocks SanitizeMarkdown returns for ordinary model text.
    const inline = (source: string): unknown => parseMarkdown(source).map((block) => ("inline" in block ? block.inline : block));
    expect(inline("use snake\\_case and 2 \\* 3 = 6")).toEqual([[{ kind: "text", text: "use snake_case and 2 * 3 = 6" }]]);
    expect(inline("a \\[b\\]  \\| \\~d\\~ \\\\ back `x\\_y`")).toEqual([
      [
        { kind: "text", text: "a [b]  | ~d~ \\ back " },
        { kind: "code", text: "x\\_y" },
      ],
    ]);
    expect(inline("**bold\\_x** *it\\*al*")).toEqual([
      [
        { kind: "bold", text: "bold_x" },
        { kind: "text", text: " " },
        { kind: "italic", text: "it*al" },
      ],
    ]);
    expect(inline("\\# esc\n\n1\\. not list\n\n\\- not a bullet \\<tag\\> \\&amp;")).toEqual([
      [{ kind: "text", text: "# esc" }],
      [{ kind: "text", text: "1. not list" }],
      [{ kind: "text", text: "- not a bullet <tag> &amp;" }],
    ]);
    // A hard line break is written as a backslash at the end of the line.
    expect(inline("line one\\\nline two")).toEqual([[{ kind: "text", text: "line one line two" }]]);
    expect(inline("ends in a backslash \\\\\nnext")).toEqual([[{ kind: "text", text: "ends in a backslash \\ next" }]]);
    // A backslash before anything but ASCII punctuation is a backslash.
    expect(inline("C:\\temp \\é")).toEqual([[{ kind: "text", text: "C:\\temp \\é" }]]);
  });

  it("reads the links the core's sanitiser writes, with the address in angle brackets, and keeps only safe ones", () => {
    const inline = (source: string, links = true): unknown => parseMarkdown(source, links).map((block) => ("inline" in block ? block.inline : block));
    // What SanitizeMarkdown returns for "see [the docs](https://example.com/a_b?x=1&y=2) and [a_b \] c](https://example.com/q)".
    expect(inline("see [the docs](<https://example.com/a_b?x=1&y=2>) and [a\\_b \\] c](<https://example.com/q>)")).toEqual([
      [
        { kind: "text", text: "see " },
        { kind: "link", text: "the docs", href: "https://example.com/a_b?x=1&y=2" },
        { kind: "text", text: " and " },
        { kind: "link", text: "a_b ] c", href: "https://example.com/q" },
      ],
    ]);
    // And for "auto <https://example.com/x> and <me@example.com>".
    expect(inline("auto <https://example.com/x> and <mailto:me@example.com>")).toEqual([
      [
        { kind: "text", text: "auto " },
        { kind: "link", text: "https://example.com/x", href: "https://example.com/x" },
        { kind: "text", text: " and " },
        { kind: "link", text: "mailto:me@example.com", href: "mailto:me@example.com" },
      ],
    ]);
    // The allowlist is unchanged: anything but http, https and mailto is text, and so is every link when links are off.
    const asText = (source: string, links = true): string[] =>
      parseMarkdown(source, links).flatMap((block) => ("inline" in block ? block.inline : [])).map((part) => `${part.kind}:${part.text}`);
    expect(asText("[x](<javascript:alert(1)>) [y](<data:text/html,hi>) <javascript:alert(1)>").join("|")).toBe("text:x|text: |text:y|text: |text:javascript:alert(1)");
    expect(asText("[the docs](<https://example.com/>) <https://example.com/x>", false).join("|")).toBe("text:the docs|text: |text:https://example.com/x");
  });

  it("reads a heading without its closing hashes, and the text of a heading and a fence in linear time", () => {
    const heading = (line: string): unknown => {
      const [block] = parseMarkdown(line);
      return block?.kind === "heading" ? [block.level, block.inline.map((part) => part.text).join("")] : block?.kind;
    };
    expect(heading("## Title ##")).toEqual([2, "Title"]);
    expect(heading("# a #b")).toEqual([1, "a #b"]);
    expect(heading("# Title # #")).toEqual([1, "Title #"]);
    expect(heading("## Title##")).toEqual([2, "Title"]);
    expect(heading("# a  #  ")).toEqual([1, "a"]);
    expect(heading("#\tTitle\t")).toEqual([1, "Title"]);
    expect(heading("#### deep")).toEqual([4, "deep"]);
    expect(heading("# ###")).toEqual([1, "#"]);
    expect(heading("# C#")).toEqual([1, "C"]);
    expect(heading("##### five")).toBe("paragraph");
    expect(parseMarkdown("```c++\nx\n```")).toEqual([{ kind: "code", language: "c++", text: "x" }]);
    expect(parseMarkdown("``` js x")[0]?.kind).toBe("paragraph");

    // Untrusted model text, parsed again on every streamed token: a backtracking pattern froze the page for seconds here.
    const started = performance.now();
    expect(heading(`# a${" ".repeat(2000)}b`)).toEqual([1, `a${" ".repeat(2000)}b`]);
    expect(parseMarkdown(`\`\`\`${" ".repeat(30000)}!`)[0]?.kind).toBe("paragraph");
    expect(performance.now() - started).toBeLessThan(200);
  });

  it("draws bars, lines and pies from the declared data and refuses what does not fit", () => {
    const x = { field: "day", type: "nominal" as const };
    const y = { field: "n", type: "quantitative" as const };
    const bar = buildChart({ type: "chart", description: "d", mark: "bar", data: [{ day: "a", n: 2 }, { day: "b", n: -1 }], encoding: { x, y } });
    expect(bar.valid && bar.series[0]?.bars).toHaveLength(2);
    const line = buildChart({ type: "chart", description: "d", mark: "line", data: [{ day: "2030-01-01", n: 2 }, { day: "2030-01-02", n: 5 }], encoding: { x: { ...x, type: "temporal" }, y } });
    expect(line.series[0]?.line).toMatch(/^M/);
    const pie = buildChart({ type: "chart", description: "d", mark: "pie", data: [{ k: "a", v: 1 }, { k: "b", v: 3 }], encoding: { theta: { field: "v", type: "quantitative" }, color: { field: "k", type: "nominal" } } });
    expect(pie.slices.map((slice) => slice.share)).toEqual([0.25, 0.75]);
    expect(buildChart({ type: "chart", description: "d", mark: "pie", data: [{ k: "a", v: 0 }], encoding: { theta: { field: "v", type: "quantitative" }, color: { field: "k", type: "nominal" } } }).valid).toBe(false);
    expect(buildChart({ type: "chart", description: "d", mark: "bar", data: [{ day: "a", n: "many" }], encoding: { x, y } }).valid).toBe(false);
    expect(niceTicks(0, 9)).toEqual([0, 2, 4, 6, 8]);
  });

  it("reads and writes nested user settings without touching the original and drops empty groups", () => {
    const original = { voice: { enabled: true } };
    const next = writePath(original, "voice.enabled", undefined);
    expect(next).toEqual({});
    expect(original).toEqual({ voice: { enabled: true } });
    expect(readPath(writePath({}, "accessibility.text_scale_pct", 150), "accessibility.text_scale_pct")).toBe(150);
  });
});
