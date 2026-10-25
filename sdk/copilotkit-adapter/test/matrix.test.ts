import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { describe, expect, it } from "vitest";

// compat/matrix.json names the CopilotKit and AG-UI releases the suite is tested against. This keeps it honest: the
// entry under test (the primary one, or ALIXAI_COMPAT_ENTRY when compat/run.mjs sets it) is exactly what is installed.

const require = createRequire(import.meta.url);
const matrix = JSON.parse(readFileSync(`${process.cwd()}/compat/matrix.json`, "utf8")) as {
  primary: string;
  agui: { protocol: string; implementedRelease: string };
  entries: Array<{ id: string; status: string; packages: Record<string, string> }>;
};
const entryId = process.env["ALIXAI_COMPAT_ENTRY"] ?? matrix.primary;
const entry = matrix.entries.find((candidate) => candidate.id === entryId);
const pkg = JSON.parse(readFileSync(`${process.cwd()}/package.json`, "utf8")) as { devDependencies: Record<string, string> };

const installed = (name: string): string => (JSON.parse(readFileSync(require.resolve(`${name}/package.json`), "utf8")) as { version: string }).version;

describe("compat/matrix.json", () => {
  it("has the entry under test, and the primary entry is one of its entries", () => {
    expect(entry, `no entry ${entryId}`).toBeDefined();
    expect(matrix.entries.map((candidate) => candidate.id)).toContain(matrix.primary);
  });

  it("lists exactly the packages the suite depends on, for every entry", () => {
    const keys = Object.keys(matrix.entries[0]?.packages ?? {}).sort();
    for (const candidate of matrix.entries) expect(Object.keys(candidate.packages).sort()).toEqual(keys);
    expect(keys).toEqual(expect.arrayContaining(["@ag-ui/client", "@ag-ui/core", "@copilotkit/react-core", "@copilotkit/react-ui", "@copilotkit/runtime"]));
  });

  it("matches what is installed, version for version", () => {
    for (const [name, version] of Object.entries(entry?.packages ?? {})) {
      if (/^\d/.test(version)) expect(installed(name), name).toBe(version);
    }
  });

  it("matches the pins of package.json when the primary entry is under test", () => {
    if (entryId !== matrix.primary) return;
    for (const [name, version] of Object.entries(entry?.packages ?? {})) expect(pkg.devDependencies[name], name).toBe(version);
  });

  it("names the AG-UI release the Go core implements", () => {
    expect(matrix.agui.implementedRelease).toBe(`@ag-ui/core ${installed("@ag-ui/core")}`);
    const vendored = readFileSync(`${process.cwd()}/../../schemas/agui/VERSION`, "utf8");
    expect(vendored).toContain(installed("@ag-ui/core"));
  });
});
