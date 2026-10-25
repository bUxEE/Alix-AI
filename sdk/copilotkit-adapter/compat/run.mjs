#!/usr/bin/env node
// Runs the adapter suite AND the compatibility gate (tests/copilotkit, @alixai/copilotkit-gate) against every entry of
// compat/matrix.json.
//
//   node compat/run.mjs                  every entry, both suites
//   node compat/run.mjs --only <id>      one entry
//   node compat/run.mjs --list           the entries and what would run
//
// The primary entry is the one installed in the workspace: both suites are run in place. Every other entry gets a scratch
// directory under the system temp directory with the same layout as the repository (the adapter and the gate, the SDK sources,
// the golden streams, the vendored schema version, the pages the gate reads, a workspace file that denies the @scarf/scarf
// install script), its packages installed at the versions of the entry, and the same two suites run there. Nothing in the
// repository changes. The suites refuse any non-loopback connection; only the install step talks to the package registry.

import { spawnSync } from "node:child_process";
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const pkgDir = resolve(here, "..");
const repo = resolve(pkgDir, "../..");
const matrix = JSON.parse(readFileSync(join(here, "matrix.json"), "utf8"));
const pkg = JSON.parse(readFileSync(join(pkgDir, "package.json"), "utf8"));
const gateDir = join(repo, "tests", "copilotkit");
const gatePkg = JSON.parse(readFileSync(join(gateDir, "package.json"), "utf8"));

const args = process.argv.slice(2);
const flag = (name) => args.includes(`--${name}`);
const only = args.includes("--only") ? args[args.indexOf("--only") + 1] : undefined;
const keep = flag("keep");

const entries = matrix.entries.filter((entry) => only === undefined || entry.id === only);
if (entries.length === 0) {
  console.error(`no matrix entry ${only ?? ""}`);
  process.exit(2);
}
if (!entries.every((entry) => /^[a-z0-9.-]+$/.test(entry.id))) {
  console.error("matrix entry ids are lower-case letters, digits, dots and dashes");
  process.exit(2);
}

if (flag("list")) {
  for (const entry of entries) {
    console.log(`${entry.id}\t${entry.id === matrix.primary ? "workspace" : "scratch install"}\t${entry.status}`);
  }
  process.exit(0);
}

const pnpm = (cwd, pnpmArgs, env = {}) =>
  spawnSync("pnpm", pnpmArgs, { cwd, stdio: "inherit", shell: process.platform === "win32", env: { ...process.env, ...env } }).status ?? 1;

const SUITES = ["adapter", "gate"];

// A scratch install has no transform cache (and a busy machine, other builds running, is the same): with every test file starting a worker at once, the first imports of CopilotKit take longer than
// vitest's fixed 60 s worker start timeout on a busy machine ("Timeout waiting for worker to respond"). Fewer workers at a time avoid it.
const VITEST_ARGS = ["--maxWorkers=3"];

/** One entry: the exit code of each suite. A suite that could not start (the install failed) counts as failed. */
const run = (entry) => {
  const env = { ALIXAI_COMPAT_ENTRY: entry.id };
  if (entry.id === matrix.primary) {
    return { adapter: pnpm(pkgDir, ["exec", "vitest", "run", ...VITEST_ARGS], env), gate: pnpm(gateDir, ["exec", "vitest", "run", ...VITEST_ARGS], env) };
  }

  const root = mkdtempSync(join(tmpdir(), `alixai-compat-${entry.id}-`));
  try {
    const adapter = join(root, "sdk", "copilotkit-adapter");
    const gate = join(root, "tests", "copilotkit");
    mkdirSync(adapter, { recursive: true });
    mkdirSync(gate, { recursive: true });
    for (const name of ["src", "test"]) cpSync(join(pkgDir, name), join(adapter, name), { recursive: true });
    for (const name of ["tsconfig.json", "vitest.config.ts"]) cpSync(join(pkgDir, name), join(adapter, name));
    cpSync(join(pkgDir, "compat"), join(adapter, "compat"), { recursive: true });
    cpSync(join(repo, "sdk", "typescript", "src"), join(root, "sdk", "typescript", "src"), { recursive: true });
    cpSync(join(repo, "tests", "agui", "testdata"), join(root, "tests", "agui", "testdata"), { recursive: true });
    cpSync(join(repo, "tsconfig.base.json"), join(root, "tsconfig.base.json"));
    mkdirSync(join(root, "schemas", "agui"), { recursive: true });
    cpSync(join(repo, "schemas", "agui", "VERSION"), join(root, "schemas", "agui", "VERSION"));

    // The gate: its sources, and what it reads of the repository (the pages it checks itself against and the source trees its
    // static scan covers).
    for (const name of ["test"]) cpSync(join(gateDir, name), join(gate, name), { recursive: true });
    for (const name of ["tsconfig.json", "vitest.config.ts"]) cpSync(join(gateDir, name), join(gate, name));
    mkdirSync(join(root, "docs", "integration"), { recursive: true });
    for (const name of ["agui.md", "copilotkit-compatibility.md"]) cpSync(join(repo, "docs", "integration", name), join(root, "docs", "integration", name));
    for (const tree of [["sdk", "python", "src"], ["pkg", "sdk-go", "agui"]]) {
      cpSync(join(repo, ...tree), join(root, ...tree), { recursive: true, filter: (source) => !/__pycache__|\.pyc$/.test(source) });
    }

    const scratchPackage = structuredClone(pkg);
    delete scratchPackage.dependencies; // @alixai/sdk is aliased to its sources by vitest.config.ts
    delete scratchPackage.scripts;
    for (const [name, version] of Object.entries(entry.packages)) scratchPackage.devDependencies[name] = version;
    writeFileSync(join(adapter, "package.json"), `${JSON.stringify(scratchPackage, null, 2)}\n`);

    const scratchGate = structuredClone(gatePkg);
    delete scratchGate.scripts;
    for (const [name, version] of Object.entries(entry.packages)) if (name in scratchGate.devDependencies) scratchGate.devDependencies[name] = version;
    writeFileSync(join(gate, "package.json"), `${JSON.stringify(scratchGate, null, 2)}\n`);

    const workspace = [
      "packages:",
      "  - sdk/copilotkit-adapter",
      "  - tests/copilotkit",
      ...(entry.installWith?.minimumReleaseAge === undefined ? [] : [`minimumReleaseAge: ${entry.installWith.minimumReleaseAge}`]),
      "allowBuilds:",
      "  '@scarf/scarf': false",
      "",
    ].join("\n");
    writeFileSync(join(root, "pnpm-workspace.yaml"), workspace);

    console.log(`\n== ${entry.id}: installing in ${root}`);
    const installed = pnpm(root, ["install", "--no-frozen-lockfile", "--prefer-offline"]);
    if (installed !== 0) return { adapter: installed, gate: installed };
    console.log(`== ${entry.id}: running the adapter suite`);
    // The heaviest file first and alone: it warms the transform cache the other files then share (see VITEST_ARGS).
    const warmed = pnpm(adapter, ["exec", "vitest", "run", "test/ui.v2.test.tsx", ...VITEST_ARGS], env);
    const adapterCode = warmed !== 0 ? warmed : pnpm(adapter, ["exec", "vitest", "run", ...VITEST_ARGS], env);
    console.log(`== ${entry.id}: running the gate (tests/copilotkit)`);
    return { adapter: adapterCode, gate: pnpm(gate, ["exec", "vitest", "run", ...VITEST_ARGS], env) };
  } finally {
    if (!keep) rmSync(root, { recursive: true, force: true });
  }
};

const results = entries.map((entry) => ({ id: entry.id, codes: run(entry) }));
console.log("\nCopilotKit and AG-UI version matrix");
for (const { id, codes } of results) {
  console.log(`  ${SUITES.map((suite) => `${suite} ${codes[suite] === 0 ? "pass" : "FAIL"}`).join("  ")}  ${id}`);
}
process.exit(results.every(({ codes }) => SUITES.every((suite) => codes[suite] === 0)) ? 0 : 1);
