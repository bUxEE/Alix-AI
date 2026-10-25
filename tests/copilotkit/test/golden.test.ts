import { readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { CONFIRMATION_SECRET, GOLDEN_NAMES, goldenEvents, normalised, received, replay, startGate } from "./helpers/rig.js";
import type { Gate } from "./helpers/rig.js";

// The backbone of the gate: every golden stream of the Go gateway (tests/agui/testdata, recorded by M3-25) replayed through the
//
//   real @ag-ui/client  ->  real @copilotkit/runtime  ->  AlixAIAgent  ->  gateway
//
// chain. For each one: the real client accepted the stream (its verifyEvents and its state machine run inside runAgent), it
// received the golden events, the gateway was sent what the page sent (the runtime injected nothing), and nothing the browser
// could read held the confirmation secret.

let gate: Gate;
beforeAll(async () => {
  gate = await startGate();
});
afterAll(async () => {
  await gate.close();
});

const stripped = (body: Record<string, any> | undefined): Record<string, unknown> => {
  const { protocolVersion: _protocol, ...rest } = body ?? {};
  return JSON.parse(JSON.stringify(rest)) as Record<string, unknown>;
};

describe("every golden stream through the real client, the real runtime and AlixAIAgent", () => {
  it.each(GOLDEN_NAMES)("%s", async (name) => {
    const { thread, observed, sent } = await replay(gate, name);

    // The real client accepted the stream: its lifecycle rules and its state machine ran over every event.
    expect(observed.error).toBeUndefined();
    // It received what the gateway recorded (the runtime adds the browser's own input to RUN_STARTED, which received() removes).
    expect(received(observed.events, thread)).toEqual(normalised(goldenEvents(name), thread));

    // The gateway got what the page sent: the runtime and the adapter inject no tool, no context and no message of their own.
    if (sent !== undefined) {
      const body = gate.gateway.requests.map((request) => request.body).filter((candidate) => candidate?.["threadId"] === thread).at(-1);
      expect(stripped(body)).toEqual(stripped(sent));
    }

    // Nothing the browser could read held the confirmation secret: not a header, not a body, not the replayed events.
    for (const response of gate.traffic) {
      expect(response.headers).not.toContain(CONFIRMATION_SECRET);
      expect(await response.body).not.toContain(CONFIRMATION_SECRET);
    }
  });

  it("covers every golden file of tests/agui/testdata, so a new golden stream is a failing test until it is replayed here", () => {
    const files = readdirSync(fileURLToPath(new URL("../../agui/testdata/", import.meta.url)))
      .filter((file) => file.endsWith(".json"))
      .map((file) => file.slice(0, -".json".length))
      .sort();
    expect(files).toEqual([...GOLDEN_NAMES].sort());
  });
});
