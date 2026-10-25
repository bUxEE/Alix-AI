import { readFileSync, readdirSync, statSync } from "node:fs";
import { fileURLToPath } from "node:url";
import type { BaseEvent } from "@ag-ui/client";
import * as adapter from "@alixai/copilotkit-adapter";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { GOLDEN_NAMES, replay, startGate } from "./helpers/rig.js";
import type { Gate, GoldenName } from "./helpers/rig.js";
import { row } from "./rows.js";

// What never reaches a CopilotKit page, shown over EVERY golden stream as the real client received it through the runtime:
// the rows of the matrix that are not emitted by design or not claimed. A test that finds nothing would pass on an empty
// stream, so the first test shows the streams are not empty.

let gate: Gate;
const streams = new Map<GoldenName, BaseEvent[]>();
const messages = new Map<GoldenName, unknown[]>();

beforeAll(async () => {
  gate = await startGate();
  for (const name of GOLDEN_NAMES) {
    const { observed, agent } = await replay(gate, name);
    streams.set(name, observed.events);
    messages.set(name, agent.messages);
  }
});
afterAll(async () => {
  await gate.close();
});

const all = (): BaseEvent[] => [...streams.values()].flat();
const typesIn = (): Set<string> => new Set(all().map((event) => event.type));

/** Every member name of every event and message, at any depth. */
const memberNames = (): Set<string> => {
  const names = new Set<string>();
  const walk = (value: unknown): void => {
    if (Array.isArray(value)) value.forEach(walk);
    else if (typeof value === "object" && value !== null) {
      for (const [key, inner] of Object.entries(value)) {
        names.add(key);
        walk(inner);
      }
    }
  };
  walk([...streams.values(), ...messages.values()]);
  return names;
};

describe("the streams under test", () => {
  it("are not empty: 22 golden streams, every emitted event type of the mapping, and the client held their messages", () => {
    expect(streams.size).toBe(22);
    expect(all().length).toBeGreaterThan(300);
    for (const type of ["RUN_STARTED", "RUN_FINISHED", "RUN_ERROR", "TEXT_MESSAGE_START", "TOOL_CALL_START", "TOOL_CALL_RESULT", "STATE_SNAPSHOT", "STATE_DELTA", "ACTIVITY_SNAPSHOT", "ACTIVITY_DELTA", "CUSTOM", "STEP_STARTED"]) {
      expect(typesIn().has(type), type).toBe(true);
    }
  });
});

describe(row("reasoning"), () => {
  it("no reasoning event, no reasoning message and no encrypted reasoning value ever reaches the page", () => {
    expect([...typesIn()].filter((type) => type.startsWith("REASONING"))).toEqual([]);
    expect([...messages.values()].flat().filter((message) => (message as { role?: string }).role === "reasoning")).toEqual([]);
    expect(memberNames().has("encryptedValue")).toBe(false);
  });
});

describe(row("subagents"), () => {
  it("there is one agent: no sub-agent event and no sub-agent attribution on any event or message", () => {
    expect([...typesIn()].filter((type) => type.startsWith("SUBAGENT"))).toEqual([]);
    expect(memberNames().has("subagentRunId")).toBe(false);
  });
});

describe(row("raw"), () => {
  it("no provider event is relayed: no RAW event, no rawEvent member, no usage figures, and only the explicit forms of text and tool-call events", () => {
    const types = typesIn();
    for (const forbidden of ["RAW", "TEXT_MESSAGE_CHUNK", "TOOL_CALL_CHUNK", "REASONING_MESSAGE_CHUNK"]) expect(types.has(forbidden), forbidden).toBe(false);
    const names = memberNames();
    for (const forbidden of ["rawEvent", "usage", "timestamp"]) expect(names.has(forbidden), forbidden).toBe(false);
  });
});

describe(row("predictive-state"), () => {
  it("state changes come from the session's state at the start and the end of a run, never from streamed tool arguments, and no custom event predicts state", () => {
    for (const [name, events] of streams) {
      let inCall = false;
      for (const event of events) {
        if (event.type === "TOOL_CALL_START") inCall = true;
        if (event.type === "TOOL_CALL_END") inCall = false;
        if (inCall) expect(event.type, name).not.toMatch(/^STATE_/);
        if (event.type === "CUSTOM") expect((event as { name?: string }).name, name).toMatch(/^alixai\.(directive|suggestions)$/);
      }
    }
  });
});

describe(row("voice"), () => {
  const read = (path: string): string => readFileSync(fileURLToPath(new URL(`../../../${path}`, import.meta.url)), "utf8");

  it("full duplex voice is not claimed: the integration pages say so, until M4 proves it with a test", () => {
    expect(read("docs/integration/agui.md")).toMatch(/\*\*Full duplex voice is not claimed\*\*/);
    expect(read("docs/integration/copilotkit-compatibility.md")).toMatch(/not claimed until M4/i);
  });

  it("nothing in a stream or in the adapter implies it: no audio part in any message, no voice export", () => {
    expect(JSON.stringify([...messages.values()])).not.toMatch(/"type":"(audio|input_audio)"|audio\//);
    expect(Object.keys(adapter).filter((name) => /voice|audio|speech|realtime|duplex/i.test(name))).toEqual([]);
  });
});

describe("no telemetry in the code AlixAI ships for CopilotKit hosts", () => {
  // The SDKs and the adapter carry no analytics or tracing client: the only mentions are the adapter's own switches that turn
  // CopilotKit's telemetry off.
  const roots = ["sdk/typescript/src", "sdk/copilotkit-adapter/src", "sdk/python/src", "pkg/sdk-go/agui"];
  const allowed = new Set(["sdk/copilotkit-adapter/src/telemetry.ts", "sdk/copilotkit-adapter/src/telemetry-off.ts"]);
  const pattern = /opentelemetry|posthog|segment\.io|@sentry|sentry[_-]sdk|datadog|mixpanel|amplitude\.com|@scarf|telemetry\.copilotkit|\.copilotkit\.ai|cloud\.copilotkit|publicApiKey/i;

  const files = (relative: string): string[] => {
    const base = fileURLToPath(new URL(`../../../${relative}`, import.meta.url));
    return readdirSync(base, { recursive: true, encoding: "utf8" })
      .filter((entry) => /\.(ts|tsx|py|go|mjs|js)$/.test(entry) && statSync(`${base}/${entry}`).isFile() && !/_test\.go$|\.d\.ts$/.test(entry))
      .map((entry) => `${relative}/${entry.replaceAll("\\", "/")}`);
  };

  it("finds no analytics, tracing or cloud client in the SDK and adapter sources", () => {
    const scanned = roots.flatMap(files);
    expect(scanned.length).toBeGreaterThan(20);
    const offenders = scanned.filter((file) => !allowed.has(file) && pattern.test(readFileSync(fileURLToPath(new URL(`../../../${file}`, import.meta.url)), "utf8")));
    expect(offenders).toEqual([]);
  });
});
