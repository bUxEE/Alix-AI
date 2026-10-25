// @vitest-environment node
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

// The owner's rules for this kit, checked mechanically so a file added months from now cannot quietly break them. They hold for
// the kit (src/) and for its demo app (demo/) alike.

const root = fileURLToPath(new URL("..", import.meta.url));
const kitRoot = join(root, "src");
const demoRoot = join(root, "demo");

const walk = (dir: string): string[] =>
  readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? walk(path) : [path];
  });

const kitFiles = walk(kitRoot);
const demoFiles = walk(demoRoot);
const files = [...kitFiles, ...demoFiles];
const vue = files.filter((file) => file.endsWith(".vue"));
const scripts = files.filter((file) => file.endsWith(".ts") || file.endsWith(".vue"));
const styles = files.filter((file) => file.endsWith(".scss") || file.endsWith(".css"));
const name = (file: string): string => relative(root, file).replace(/\\/g, "/");
const read = (file: string): string => readFileSync(file, "utf8");

describe("conventions", () => {
  it("has no plain CSS, only SCSS", () => {
    expect(styles.filter((file) => file.endsWith(".css")).map(name)).toEqual([]);
    expect(styles.length).toBeGreaterThan(3);
    expect(demoFiles.filter((file) => file.endsWith(".scss")).length).toBeGreaterThan(0);
  });

  it("uses @use, never @import", () => {
    const offenders = [...styles, ...vue].filter((file) => /@import\b/.test(read(file))).map(name);
    expect(offenders).toEqual([]);
    for (const file of styles.filter((candidate) => /^(src|demo)\/styles\//.test(name(candidate)))) {
      const text = read(file);
      const firstRule = text.split("\n").find((line) => /^\s*[.@\w$:&-]/.test(line) && !line.trim().startsWith("//"));
      if (text.includes("@use")) expect(firstRule, name(file)).toMatch(/^@use/);
    }
  });

  it("declares every style block as SCSS", () => {
    const offenders = vue.filter((file) => /<style(?![^>]*lang="scss")/.test(read(file))).map(name);
    expect(offenders).toEqual([]);
  });

  it("writes every component with <script setup>", () => {
    const offenders = vue.filter((file) => /<script\b/.test(read(file)) && !/<script setup\b/.test(read(file))).map(name);
    expect(offenders).toEqual([]);
  });

  it("uses arrow functions only: no `function` declarations or expressions", () => {
    const offenders = scripts.filter((file) => /(^|[^\w.$])function\b\s*\*?\s*[\w$]*\s*\(/m.test(read(file).replace(/\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "").replace(/(["'`])(?:\\.|(?!\1).)*\1/g, '""'))).map(name);
    expect(offenders).toEqual([]);
  });

  it("never injects HTML: no v-html, innerHTML or insertAdjacentHTML", () => {
    const offenders = scripts.filter((file) => /v-html|innerHTML|outerHTML|insertAdjacentHTML|document\.write/.test(read(file).replace(/\/\/.*$/gm, ""))).map(name);
    expect(offenders).toEqual([]);
  });

  it("marks every input without validation with no-validation", () => {
    const offenders: string[] = [];
    for (const file of vue) {
      for (const match of read(file).matchAll(/<(input|textarea|select)\b[^>]*?>/gs)) {
        if (!match[0].includes("no-validation")) offenders.push(`${name(file)}: ${match[0].slice(0, 60)}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it("puts each template attribute on its own line, in the owner's order", () => {
    const rank = (attribute: string): number => {
      if (/^(v-if|v-else-if|v-else|v-for)\b/.test(attribute)) return 0;
      if (/^v-show\b/.test(attribute)) return 1;
      if (/^v-model\b/.test(attribute)) return 2;
      if (/^:/.test(attribute) || /^v-bind:/.test(attribute)) return 3;
      if (/^(v-bind\b|v-bind=)/.test(attribute)) return 6;
      if (/^(@|v-on)/.test(attribute)) return 7;
      if (/^#|^v-slot/.test(attribute)) return 4;
      if (/="/.test(attribute)) return 4;
      return 5;
    };
    const offenders: string[] = [];
    for (const file of vue) {
      const template = /<template>([\s\S]*)<\/template>\s*(<style|$)/.exec(read(file))?.[1] ?? "";
      for (const match of template.matchAll(/<([A-Za-z][\w-]*)((?:\s+[^<>]*?)?)\s*\/?>/gs)) {
        const raw = match[2] ?? "";
        if (!raw.includes("\n")) continue;
        const lines = raw.split("\n").map((line) => line.trim()).filter((line) => line !== "");
        const attributes = lines.flatMap((line) => line.match(/(?:[:@#]?[\w.:-]+(?:\[[^\]]+\])?(?:="[^"]*")?)/g) ?? []);
        let last = -1;
        for (const line of lines) {
          const first = line.match(/^[:@#]?[\w.:-]+(?:\[[^\]]+\])?(?:="[^"]*")?/)?.[0] ?? "";
          const level = rank(first);
          // Boolean attributes may share a line; everything else is alone.
          if (level !== 5 && attributes.length > 0 && (line.match(/="/g) ?? []).length > 1) offenders.push(`${name(file)}: two attributes on one line: ${line.slice(0, 70)}`);
          if (level < last && !(last === 5 && level >= 5)) offenders.push(`${name(file)}: <${match[1]}> ${first} comes after a later group`);
          last = Math.max(last, level);
        }
      }
    }
    expect(offenders).toEqual([]);
  });

  it("writes no layout media or container queries outside the shared utility stylesheet", () => {
    const offenders = [...styles, ...vue]
      .filter((file) => !name(file).endsWith("_utilities.scss"))
      .filter((file) => /@container|@media\s*\(\s*(min|max)-width/.test(read(file)))
      .map(name);
    expect(offenders).toEqual([]);
  });

  it("imports skeletons and loaders statically", () => {
    const offenders = vue.filter((file) => /defineAsyncComponent|import\(\s*["'][^"']*(Skeleton|Loader|Loadable)/.test(read(file))).map(name);
    expect(offenders).toEqual([]);
  });

  it("keeps every global class and custom property under the alix prefix", () => {
    const offenders: string[] = [];
    for (const file of styles) {
      for (const match of read(file).matchAll(/^\s*(?:&\.|\.)([a-z][\w-]*)\s*[{,]/gm)) {
        if (!(match[1] ?? "").startsWith("alix-")) offenders.push(`${name(file)}: .${match[1]}`);
      }
      for (const match of read(file).matchAll(/--([a-z][\w-]*)\s*:/g)) {
        if (!(match[1] ?? "").startsWith("alix-")) offenders.push(`${name(file)}: --${match[1]}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it("uses only the packages it declares, and no telemetry or CopilotKit", () => {
    const code = (file: string): string => read(file).replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|[^:])\/\/.*$/gm, "$1");
    const importsOf = (list: string[]): Set<string> =>
      new Set([...list.map(code).join("\n").matchAll(/(?:from\s+|import\s+)["']([^."'][^"']*)["']/g)].map((match) => match[1] as string));
    expect(scripts.map(code).join("\n")).not.toMatch(/@copilotkit|@ag-ui|sentry|posthog|google-analytics|segment\.io|mixpanel/i);
    for (const specifier of importsOf(scripts.filter((file) => file.startsWith(kitRoot)))) {
      expect(["vue", "vue/server-renderer", "@alixai/sdk", "@alixai/voice-orb"], specifier).toContain(specifier);
    }
    // The demo is a host of the kit: it imports the kit by its package name, and the mock by a relative path.
    for (const specifier of importsOf(scripts.filter((file) => file.startsWith(demoRoot)))) {
      expect(["vue", "@alixai/sdk", "@alixai/vue", "@alixai/vue/styles"], specifier).toContain(specifier);
    }
  });

  it("keeps the demo on the local mock: it names no other server", () => {
    const offenders = scripts
      .filter((file) => file.startsWith(demoRoot))
      .filter((file) => /\b(https?|wss?):\/\/(?!www\.w3\.org\/2000\/svg)/.test(read(file)))
      .map(name);
    expect(offenders).toEqual([]);
  });
});
