// @vitest-environment node
import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

// Dictation must stay free of the voice-conversation surface (orb, player, Voice Mode, controller).

const root = fileURLToPath(new URL("..", import.meta.url));
const dictationRoot = join(root, "src", "dictation");

const walk = (dir: string): string[] =>
  readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? walk(path) : [path];
  });

const files = walk(dictationRoot).filter((file) => /\.(ts|vue)$/.test(file));
const name = (file: string): string => relative(root, file).replace(/\\/g, "/");
const read = (file: string): string => readFileSync(file, "utf8");

const FORBIDDEN = [
  /AlixVoiceMode/,
  /AlixVoiceOrb/,
  /AlixTranscriptEditor/,
  /createAlixVoiceController/,
  /useAlixVoice/,
  /createAlixPlayer/,
  /from\s+["']\.\.\/voice\//,
  /from\s+["']@alixai\/voice-orb/,
  /_voice\.scss/,
  /\/controller(\.|["'])/,
];

describe("dictation bundle isolation", () => {
  it("exports a ./dictation package entry", () => {
    const pkg = JSON.parse(readFileSync(join(root, "package.json"), "utf8")) as { exports: Record<string, unknown> };
    expect(pkg.exports["./dictation"]).toBeTruthy();
  });

  it("does not import conversation-mode modules from the dictation folder", () => {
    const offenders: string[] = [];
    for (const file of files) {
      const text = read(file);
      for (const pattern of FORBIDDEN) {
        if (pattern.test(text)) offenders.push(`${name(file)} matches ${pattern}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it("keeps capture helper importable without the player", () => {
    const capture = read(join(dictationRoot, "capture.ts"));
    expect(capture).toContain("createAlixMicrophone");
    expect(capture).not.toContain("createAlixPlayer");
    expect(capture).not.toContain("AudioBufferSourceNode");
  });
});
