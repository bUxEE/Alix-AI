import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative } from "node:path";

import { describe, expect, it } from "vitest";

import * as sdk from "../src/index.js";
import { REPO_ROOT, SDK_ROOT } from "./helpers/repo.js";

// The conventions of the SDK (the M1-19 brief) as tests, so they cannot erode.

const walk = (dir: string): string[] =>
  readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? walk(path) : [path];
  });

const sources = (dir: string, skip: (path: string) => boolean = () => false): string[] =>
  walk(join(SDK_ROOT, dir)).filter((p) => p.endsWith(".ts") && !skip(p.replace(/\\/g, "/")));

/** The code of a TypeScript file with comments, strings, template literals and regex literals blanked out. */
const codeOnly = (text: string): string => {
  let out = "";
  let i = 0;
  const blank = (s: string): string => s.replace(/[^\n]/g, " ");
  while (i < text.length) {
    const c = text[i] as string;
    const next = text[i + 1];
    if (c === "/" && next === "/") {
      const end = text.indexOf("\n", i);
      const stop = end === -1 ? text.length : end;
      out += blank(text.slice(i, stop));
      i = stop;
    } else if (c === "/" && next === "*") {
      const end = text.indexOf("*/", i + 2);
      const stop = end === -1 ? text.length : end + 2;
      out += blank(text.slice(i, stop));
      i = stop;
    } else if (c === '"' || c === "'") {
      let j = i + 1;
      while (j < text.length && text[j] !== c) j += text[j] === "\\" ? 2 : 1;
      out += blank(text.slice(i, j + 1));
      i = j + 1;
    } else if (c === "`") {
      // Template literals: blank the text, keep nothing of the ${} bodies (none of them hold a keyword we look for).
      let j = i + 1;
      let depth = 0;
      while (j < text.length && !(text[j] === "`" && depth === 0)) {
        if (text[j] === "\\") j += 2;
        else {
          if (text[j] === "$" && text[j + 1] === "{") depth += 1;
          else if (text[j] === "}" && depth > 0) depth -= 1;
          j += 1;
        }
      }
      out += blank(text.slice(i, j + 1));
      i = j + 1;
    } else if (c === "/" && /[(,=:[!&|?{};]\s*$/.test(out.slice(-3)) && next !== " ") {
      // A regex literal: skip to its closing slash.
      let j = i + 1;
      let inClass = false;
      while (j < text.length && (text[j] !== "/" || inClass)) {
        if (text[j] === "\\") j += 2;
        else {
          if (text[j] === "[") inClass = true;
          else if (text[j] === "]") inClass = false;
          j += 1;
        }
      }
      out += blank(text.slice(i, j + 1));
      i = j + 1;
    } else {
      out += c;
      i += 1;
    }
  }
  return out;
};

const handWritten = (path: string): boolean => !path.includes("/src/gen/");

describe("the scanner is really scanning (negative controls)", () => {
  it("sees a function keyword in code, not in strings, comments, templates or regex literals", () => {
    expect(codeOnly("function f() {}")).toMatch(/\bfunction\b/);
    expect(codeOnly("export async function* g() { yield 1; }")).toMatch(/\byield\b/);
    const quiet = "const a = 'function'; const b = \"yield\"; // function\n/* function */ const c = `function ${1}`;";
    expect(codeOnly(quiet)).not.toMatch(/function|yield/);
    expect(codeOnly("const r = /function/g; const d = a / b;")).not.toMatch(/function/);
    expect(codeOnly("class Foo {}")).toMatch(/class Foo/);
  });
});

describe("code style", () => {
  const files = [...sources("src", (p) => !handWritten(p)), ...sources("test")];

  it("scans a meaningful number of files", () => {
    expect(files.length).toBeGreaterThan(20);
  });

  it("uses arrow functions only: no `function` keyword and no generators", () => {
    const offenders: string[] = [];
    for (const file of files) {
      const code = codeOnly(readFileSync(file, "utf8"));
      for (const [pattern, what] of [[/\bfunction\b/, "function keyword"], [/\byield\b/, "yield"], [/\basync\s*\*/, "async generator"]] as const) {
        if (pattern.test(code)) offenders.push(`${relative(SDK_ROOT, file)}: ${what}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it("declares no class but the error type", () => {
    const classes: string[] = [];
    for (const file of files) {
      for (const match of codeOnly(readFileSync(file, "utf8")).matchAll(/\bclass\s+(\w+)/g)) classes.push(`${relative(SDK_ROOT, file)}: ${match[1]}`);
    }
    expect(classes.map((c) => c.split(": ")[1])).toEqual(["AlixAIError"]);
  });
});

describe("zero runtime dependencies, and a platform-neutral core", () => {
  const library = sources("src", (p) => !handWritten(p));

  it("declares no dependencies of any kind that ship", () => {
    for (const path of [join(SDK_ROOT, "package.json")]) {
      const pkg = JSON.parse(readFileSync(path, "utf8")) as Record<string, unknown>;
      for (const key of ["dependencies", "peerDependencies", "optionalDependencies", "bundledDependencies"]) {
        expect(Object.keys((pkg[key] as object | undefined) ?? {}), key).toEqual([]);
      }
    }
  });

  it("imports nothing but its own files: no package, and no node: module statically", () => {
    const offenders: string[] = [];
    for (const file of library) {
      const text = readFileSync(file, "utf8");
      for (const match of text.matchAll(/(?:^|\n)\s*(?:import|export)\s[^;]*?from\s+["']([^"']+)["']/g)) {
        const specifier = match[1] as string;
        if (!specifier.startsWith(".")) offenders.push(`${relative(SDK_ROOT, file)}: ${specifier}`);
      }
      if (/\brequire\s*\(/.test(codeOnly(text))) offenders.push(`${relative(SDK_ROOT, file)}: require()`);
    }
    expect(offenders).toEqual([]);
  });

  it("uses no Node-only global outside src/testing (the core also runs in browsers)", () => {
    const offenders: string[] = [];
    for (const file of library.filter((p) => !p.replace(/\\/g, "/").includes("/src/testing/"))) {
      const code = codeOnly(readFileSync(file, "utf8"));
      for (const pattern of [/\bBuffer\b/, /\bprocess\./, /\b__dirname\b/, /import\(\s*["']node:/, /\bnode:/]) {
        if (pattern.test(code) || pattern.test(readFileSync(file, "utf8").replace(/\/\/.*$/gm, ""))) offenders.push(`${relative(SDK_ROOT, file)}: ${pattern}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it("reaches Node only from the fake server's listen(), through a dynamic import", () => {
    const fake = readFileSync(join(SDK_ROOT, "src", "testing", "fake-server.ts"), "utf8");
    expect(fake).toMatch(/await import\("node:http"\)/);
    expect(fake).not.toMatch(/from\s+["']node:/);
  });
});

describe("package metadata required by CI and the M1-19 brief", () => {
  const pkg = JSON.parse(readFileSync(join(SDK_ROOT, "package.json"), "utf8")) as Record<string, any>;
  const root = JSON.parse(readFileSync(join(REPO_ROOT, "package.json"), "utf8")) as Record<string, any>;

  it("pins the package manager to an exact pnpm version, in the SDK and in the workspace root", () => {
    expect(pkg["packageManager"]).toMatch(/^pnpm@\d+\.\d+\.\d+$/);
    expect(root["packageManager"]).toBe(pkg["packageManager"]);
  });

  it("has typecheck, test and build scripts, and a committed lockfile", () => {
    for (const script of ["typecheck", "test", "build"]) expect(pkg["scripts"][script], script).toBeTypeOf("string");
    expect(existsSync(join(REPO_ROOT, "pnpm-lock.yaml"))).toBe(true);
  });

  it("is ESM, named @alixai/sdk, and exposes the root and the testing entry", () => {
    expect(pkg["name"]).toBe("@alixai/sdk");
    expect(pkg["type"]).toBe("module");
    expect(Object.keys(pkg["exports"]).sort()).toEqual([".", "./package.json", "./testing"]);
    expect(pkg["exports"]["./testing"]["import"]).toBe("./dist/testing/index.js");
    expect(Number.parseInt(String(pkg["engines"]["node"]).replace(/\D+/, ""), 10)).toBeGreaterThanOrEqual(22);
  });

  it("the workspace root is private and drives the SDK's scripts", () => {
    expect(root["private"]).toBe(true);
    for (const script of ["typecheck", "test", "build"]) expect(root["scripts"][script]).toContain("@alixai/sdk");
  });
});

describe("the public surface", () => {
  it("has the runtime exports the docs describe, and nothing accidental", () => {
    expect(Object.keys(sdk).sort()).toEqual(
      [
        "AlixAIError",
        "CONFIRMATION_TOKEN_HEADER",
        "DEFAULT_CLIENT_CAPABILITIES",
        "ERROR_CODES",
        "OPERATIONS",
        "TURN_EVENT_TYPES",
        "attachmentKind",
        "blocksOfType",
        "citationLink",
        "createClient",
        "decision",
        "doneEvent",
        "exports",
        "isAlixAIError",
        "isErrorCode",
        "isRetryableCode",
        "me",
        "memory",
        "providers",
        "rag",
        "turnText",
        "viewTurn",
        "voice",
      ].sort(),
    );
  });

  it("re-exports the namespace stubs of later milestones, so they fill only their own folder", () => {
    const index = readFileSync(join(SDK_ROOT, "src", "index.ts"), "utf8");
    for (const area of ["rag", "voice", "providers", "decision", "me", "memory", "exports"]) {
      expect(existsSync(join(SDK_ROOT, "src", area, "index.ts")), `src/${area}/index.ts`).toBe(true);
      expect(index).toContain(`export * as ${area} from "./${area}/index.js";`);
      expect(typeof (sdk as Record<string, unknown>)[area]).toBe("object");
    }
  });

  it("ships the generated types under `types` and flat re-exports of the common ones", () => {
    const index = readFileSync(join(SDK_ROOT, "src", "index.ts"), "utf8");
    expect(index).toContain('export type * as types from "./gen/index.js";');
    expect(index).toContain("TurnEvent");
  });
});
