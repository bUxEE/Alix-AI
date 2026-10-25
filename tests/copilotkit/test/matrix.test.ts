import { existsSync, readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { ROWS } from "./rows.js";
import type { RowStatus } from "./rows.js";

// The gate checks itself: the matrix it proves is the matrix agui.md publishes, every row has a test, the versions it ran
// against are the ones sdk/copilotkit-adapter/compat/matrix.json names, and docs/integration/copilotkit-compatibility.md says
// the same as the tests. The entry under test is the primary one, or ALIXAI_COMPAT_ENTRY when sdk/copilotkit-adapter/compat/run.mjs
// runs the gate against another entry of the matrix in a scratch install.

const here = (path: string): string => fileURLToPath(new URL(path, import.meta.url));
const repo = (path: string): string => here(`../../../${path}`);
const read = (path: string): string => readFileSync(repo(path), "utf8");

/** The first cell of every data row of section 12 of docs/integration/agui.md. */
const aguiRows = (): string[] => {
  const section = read("docs/integration/agui.md").split(/^## 12\. Feature matrix/m)[1]?.split(/^## 13\./m)[0] ?? "";
  return section
    .split("\n")
    .filter((line) => line.startsWith("| ") && !line.startsWith("| ---") && !line.startsWith("| CopilotKit feature"))
    .map((line) => (line.split("|")[1] ?? "").trim());
};

interface Matrix {
  primary: string;
  entries: Array<{ id: string; status: string; packages: Record<string, string> }>;
  agui: { protocol: string; implementedRelease: string };
}
const matrix = (): Matrix => JSON.parse(read("sdk/copilotkit-adapter/compat/matrix.json")) as Matrix;
const underTest = (): string => process.env["ALIXAI_COMPAT_ENTRY"] ?? matrix().primary;

describe("the feature matrix", () => {
  it("is the one agui.md publishes: every row of section 12 has an entry, and no entry is invented", () => {
    const published = aguiRows();
    expect(published).toHaveLength(24);
    expect(ROWS.filter((row) => row.inAgui).map((row) => row.feature)).toEqual(published);
  });

  it("has a describe block that proves each row, found by its row() title in the test files", () => {
    const sources = readdirSync(here("."))
      .filter((file) => /\.test\.tsx?$/.test(file) && file !== "matrix.test.ts")
      .map((file) => readFileSync(here(file), "utf8"))
      .join("\n");
    const proved = new Set([...sources.matchAll(/describe\(row\("([a-z-]+)"\)/g)].map((match) => match[1]));
    expect([...proved].sort()).toEqual(ROWS.map((row) => row.id).sort());
  });

  it("gives every row a status that says what it is: shown, shown with a limit, not emitted by design, or not claimed", () => {
    const statuses = new Set<RowStatus>(ROWS.map((row) => row.status));
    expect([...statuses].sort()).toEqual(["limited", "not-claimed", "not-emitted", "shown"]);
    expect(ROWS.filter((row) => row.status === "not-emitted").map((row) => row.id)).toEqual(["predictive-state", "reasoning", "subagents", "raw"]);
    expect(ROWS.filter((row) => row.status === "not-claimed").map((row) => row.id)).toEqual(["voice"]);
  });
});

describe("the versions under test", () => {
  const entryUnderTest = (): Record<string, string> => {
    const entry = matrix().entries.find((candidate) => candidate.id === underTest());
    if (entry === undefined) throw new Error(`the matrix has no entry ${underTest()}`);
    return entry.packages;
  };
  const pins = (): Record<string, string> => (JSON.parse(readFileSync(here("../package.json"), "utf8")) as { devDependencies: Record<string, string> }).devDependencies;

  it("are pinned in the gate's package.json exactly as the primary entry of compat/matrix.json names them", () => {
    if (underTest() !== matrix().primary) return; // a scratch install of another entry rewrites the pins
    const wanted = entryUnderTest();
    for (const name of ["@ag-ui/client", "@ag-ui/core", "@copilotkit/runtime", "@copilotkit/react-core", "react", "react-dom", "rxjs"]) {
      expect(pins()[name], name).toBe(wanted[name]);
    }
    expect(pins()["@alixai/copilotkit-adapter"]).toBe("workspace:*");
  });

  it("are the versions that are installed, for the entry under test", () => {
    const wanted = entryUnderTest();
    for (const name of ["@ag-ui/client", "@ag-ui/core", "@copilotkit/runtime", "@copilotkit/react-core", "react", "react-dom", "rxjs"]) {
      const installed = (JSON.parse(readFileSync(here(`../node_modules/${name}/package.json`), "utf8")) as { version: string }).version;
      expect(installed, name).toBe(wanted[name]);
    }
  });

  it("include the AG-UI release the Go core implements, and the gate is a member of the workspace", () => {
    expect(matrix().agui).toMatchObject({ protocol: "1.0", implementedRelease: "@ag-ui/core 1.0.1" });
    expect(read("schemas/agui/VERSION")).toContain("@ag-ui/core");
    expect(read("pnpm-workspace.yaml")).toMatch(/^ {2}- tests\/copilotkit$/m);
  });
});

describe("docs/integration/copilotkit-compatibility.md", () => {
  const doc = (): string => read("docs/integration/copilotkit-compatibility.md");
  const WORDS: Record<RowStatus, string> = {
    shown: "shown",
    limited: "shown with a limit",
    "not-emitted": "not emitted by design",
    "not-claimed": "not claimed",
    "not-shown": "not shown",
  };

  it("exists, and publishes one table row per matrix row with the same status the tests carry", () => {
    expect(existsSync(repo("docs/integration/copilotkit-compatibility.md"))).toBe(true);
    for (const row of ROWS) {
      const line = doc().split("\n").find((candidate) => candidate.startsWith(`| \`${row.id}\` |`));
      expect(line, row.id).toBeDefined();
      const cells = (line ?? "").split("|").map((cell) => cell.trim());
      expect(cells[2], row.id).toBe(row.feature);
      expect(cells[3], row.id).toBe(WORDS[row.status]);
      expect(cells[4], row.id).not.toBe("");
    }
  });

  it("publishes the version matrix: every entry of compat/matrix.json with its CopilotKit release, the AG-UI release and the protocol", () => {
    const text = doc();
    for (const entry of matrix().entries) {
      expect(text, entry.id).toContain(`\`${entry.id}\``);
      expect(text, entry.id).toContain(entry.packages["@copilotkit/runtime"] as string);
    }
    expect(text).toContain("1.0.1");
    expect(text).toContain("protocol 1.0");
  });

  it("has the quickstart of each host, and says that full duplex voice is not claimed until M4", () => {
    const text = doc();
    for (const heading of ["### Next.js", "### Express", "### Go host", "### Python host"]) expect(text, heading).toContain(heading);
    expect(text).toMatch(/full duplex voice is not claimed until M4 proves it/i);
  });
});
