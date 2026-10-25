#!/usr/bin/env node
// Vendors the JSON Schemas of the pinned AG-UI release (ADR 0035, task M3-24).
//
//   cd tools/aguischema && npm ci && node generate.mjs          # writes schemas/agui
//   node tools/aguischema/generate.mjs --from <dir>             # <dir> holds node_modules with the pinned packages
//   node tools/aguischema/generate.mjs --check [--from <dir>]   # exit 1 when schemas/agui differs from a fresh run
//
// The AG-UI protocol defines its wire format as zod schemas in @ag-ui/core. This script loads the installed,
// pinned release (package.json is the pin; the installed versions must match it exactly), registers every exported
// schema under its export name, converts the registry with zod's own JSON Schema converter (draft 2020-12, input
// side) and writes one self-contained bundle, schemas/agui/ag-ui-core.json, plus schemas/agui/VERSION with the
// package version, the npm integrity hash of the tarball, the converter version and the sha256 of every file written.
//
// The bundle has no top-level "type": every schema is a member of $defs (Event, RunAgentInput, Message, Tool,
// Context, JsonPatchOperation, RunStartedEvent, ...), referenced as urn:ag-ui:core:<version>#/$defs/<Name>. The file
// deliberately does not end in .schema.json: tools/schemagen and tools/schemacheck own that suffix and its urn:alixai
// id convention, and these are another party's schemas, not AlixAI contracts.
//
// Output is deterministic: names are sorted and nothing carries a date or a path. Nothing is fetched here; fetching
// the pinned package is the caller's `npm ci`.

import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const BUNDLE = "ag-ui-core.json";

const parseArgs = (argv) => {
  const out = { check: false };
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === "--check") out.check = true;
    else if (a === "--from") out.from = argv[++i];
    else if (a === "--out") out.out = argv[++i];
    else throw new Error(`unknown argument ${a}`);
  }
  return out;
};

const readJSON = (p) => JSON.parse(readFileSync(p, "utf8"));
const sha256 = (s) => createHash("sha256").update(s).digest("hex");
const sortKeys = (v) => {
  if (Array.isArray(v)) return v.map(sortKeys);
  if (v && typeof v === "object") {
    return Object.fromEntries(Object.keys(v).sort().map((k) => [k, sortKeys(v[k])]));
  }
  return v;
};

const args = parseArgs(process.argv.slice(2));
const from = path.resolve(args.from ?? here);
const outDir = path.resolve(args.out ?? path.join(here, "..", "..", "schemas", "agui"));

const pin = readJSON(path.join(here, "package.json")).dependencies;
const coreDir = path.join(from, "node_modules", "@ag-ui", "core");
const zodDir = path.join(from, "node_modules", "zod");
for (const d of [coreDir, zodDir]) {
  if (!existsSync(path.join(d, "package.json"))) {
    throw new Error(`${d} is not installed: run \`npm ci\` in tools/aguischema (or pass --from <dir with node_modules>)`);
  }
}
const corePkg = readJSON(path.join(coreDir, "package.json"));
const zodPkg = readJSON(path.join(zodDir, "package.json"));
if (corePkg.version !== pin["@ag-ui/core"] || zodPkg.version !== pin.zod) {
  throw new Error(
    `installed @ag-ui/core ${corePkg.version} / zod ${zodPkg.version} differ from the pin ` +
      `${pin["@ag-ui/core"]} / ${pin.zod} in tools/aguischema/package.json`,
  );
}

// The npm integrity of the tarball comes from the lockfile that installed it.
const lockPath = path.join(from, "package-lock.json");
if (!existsSync(lockPath)) throw new Error(`${lockPath} is missing: install with npm so the integrity hash is recorded`);
const lockEntry = readJSON(lockPath).packages?.["node_modules/@ag-ui/core"];
if (!lockEntry?.integrity || lockEntry.version !== corePkg.version) {
  throw new Error("package-lock.json has no integrity hash for the installed @ag-ui/core");
}

const coreSchemasFile = path.join(coreDir, "dist", "schemas.mjs");
const coreSource = readFileSync(coreSchemasFile, "utf8");
const schemas = await import(pathToFileURL(coreSchemasFile).href);
// The schemas import "zod/v4": converting with that same module keeps one zod instance.
const { z } = await import(pathToFileURL(path.join(zodDir, "v4", "index.js")).href);
const protocolVersion = schemas.PROTOCOL_VERSION;
if (typeof protocolVersion !== "string") throw new Error("@ag-ui/core exports no PROTOCOL_VERSION");

// Register every exported zod schema under its export name without the Schema suffix. An alias of an already
// registered schema object is skipped, so each schema keeps one name.
const registry = z.registry();
const names = [];
const seen = new Set();
for (const key of Object.keys(schemas).sort()) {
  const value = schemas[key];
  if (!key.endsWith("Schema") || !value || typeof value !== "object" || !value._zod) continue;
  if (seen.has(value)) continue;
  seen.add(value);
  const id = key.slice(0, -"Schema".length);
  registry.add(value, { id });
  names.push(id);
}

const converted = z.toJSONSchema(registry, {
  target: "draft-2020-12",
  io: "input",
  unrepresentable: "any",
  uri: (id) => `#/$defs/${id}`,
}).schemas;

const defs = {};
for (const id of names.sort()) {
  const s = { ...converted[id] };
  delete s.$schema;
  delete s.$id; // a fragment $id is not legal inside a 2020-12 bundle
  defs[id] = sortKeys(s);
}
for (const required of [
  "Event", "RunAgentInput", "Message", "Tool", "Context", "JsonPatchOperation", "ResumeEntry", "Interrupt",
  "RunStartedEvent", "RunFinishedEvent", "RunErrorEvent", "StepStartedEvent", "StepFinishedEvent",
  "TextMessageStartEvent", "TextMessageContentEvent", "TextMessageEndEvent", "TextMessageChunkEvent",
  "ToolCallStartEvent", "ToolCallArgsEvent", "ToolCallEndEvent", "ToolCallResultEvent", "ToolCallChunkEvent",
  "StateSnapshotEvent", "StateDeltaEvent", "MessagesSnapshotEvent", "ActivitySnapshotEvent", "ActivityDeltaEvent",
  "RawEvent", "CustomEvent", "ReasoningStartEvent", "ReasoningMessageStartEvent", "ReasoningMessageContentEvent",
  "ReasoningMessageEndEvent", "ReasoningMessageChunkEvent", "ReasoningEndEvent", "ReasoningEncryptedValueEvent",
  "SubagentStartedEvent", "SubagentFinishedEvent", "SubagentErrorEvent",
]) {
  if (!defs[required]) throw new Error(`the pinned release has no ${required} schema: the protocol changed, review before vendoring`);
}

const id = `urn:ag-ui:core:${corePkg.version}`;
const bundle = {
  $schema: "https://json-schema.org/draft/2020-12/schema",
  $id: id,
  title: `AG-UI core schemas, @ag-ui/core ${corePkg.version}, protocol ${protocolVersion}`,
  description:
    "Vendored by tools/aguischema/generate.mjs from the pinned @ag-ui/core release (see VERSION). Generated: do not edit. " +
    "Every AG-UI schema is a member of $defs. Members declared as any-typed are open (additionalProperties {}), as the " +
    "release defines them; the zod refinements that reject null or undefined on any-typed members are not expressible in " +
    "JSON Schema and are asserted by the Go tests of internal/agui.",
  $defs: defs,
};
const bundleText = `${JSON.stringify(bundle, null, 2)}\n`;

// Refinements and transforms that JSON Schema cannot carry; recorded so a release that adds one is noticed.
const count = (re) => (coreSource.match(re) ?? []).length;
const refinements = count(/\.refine\(/g) + count(/\.superRefine\(/g) + count(/\.check\(/g);
const transforms = count(/\.transform\(/g);

const version =
  [
    "# Generated by tools/aguischema/generate.mjs. Do not edit by hand.",
    `package: @ag-ui/core`,
    `version: ${corePkg.version}`,
    `protocol_version: ${protocolVersion}`,
    `resolved: ${lockEntry.resolved}`,
    `integrity: ${lockEntry.integrity}`,
    `converter: zod ${zodPkg.version} (z.toJSONSchema, target draft-2020-12, io input)`,
    `schema_id: ${id}`,
    `definitions: ${names.length}`,
    `refinements: ${refinements}`,
    `transforms: ${transforms}`,
    `file: ${BUNDLE} sha256:${sha256(bundleText)}`,
  ].join("\n") + "\n";

const files = [
  [BUNDLE, bundleText],
  ["VERSION", version],
];

if (args.check) {
  let drift = false;
  for (const [name, text] of files) {
    const p = path.join(outDir, name);
    const have = existsSync(p) ? readFileSync(p, "utf8") : null;
    if (have !== text) {
      console.error(`aguischema: ${path.relative(process.cwd(), p)} differs from a fresh run`);
      drift = true;
    }
  }
  if (drift) process.exit(1);
  console.log(`aguischema: ok - ${names.length} definitions, @ag-ui/core ${corePkg.version}`);
} else {
  mkdirSync(outDir, { recursive: true });
  for (const [name, text] of files) writeFileSync(path.join(outDir, name), text);
  console.log(`aguischema: wrote ${files.length} files to ${path.relative(process.cwd(), outDir) || "."} - ${names.length} definitions, @ag-ui/core ${corePkg.version}`);
}
