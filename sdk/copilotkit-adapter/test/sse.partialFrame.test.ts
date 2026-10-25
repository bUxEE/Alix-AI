import type { BaseEvent, RunAgentInput } from "@ag-ui/client";
import { describe, expect, it } from "vitest";
import { AlixAIAgent } from "../src/agent.js";
import { readSseFrames } from "../src/sse.js";
import type { SseFrame } from "../src/sse.js";

// The gateway writes `id:` before `data:`, so a connection that dies inside a frame has already sent the id of an event that
// never arrived whole. Such a frame is never dispatched: the reconnect replays it whole from the last complete event.

const body = (text: string): ReadableStream<Uint8Array> =>
  new ReadableStream<Uint8Array>({
    start: (controller) => {
      controller.enqueue(new TextEncoder().encode(text));
      controller.close();
    },
  });

const eventStream = (text: string): Response => new Response(body(text), { status: 200, headers: { "Content-Type": "text/event-stream" } });

const frame = (id: string | undefined, event: Record<string, unknown>): string => `${id === undefined ? "" : `id: ${id}\n`}data: ${JSON.stringify(event)}\n\n`;

const THREAD = "ses_partial";
const RUN = "run-partial-1";
const started = { type: "RUN_STARTED", threadId: THREAD, runId: RUN };
const finished = { type: "RUN_FINISHED", threadId: THREAD, runId: RUN };
const start = { type: "TEXT_MESSAGE_START", messageId: "m1", role: "assistant" };
const content = { type: "TEXT_MESSAGE_CONTENT", messageId: "m1", delta: "Hello" };
const end = { type: "TEXT_MESSAGE_END", messageId: "m1" };

const input: RunAgentInput = {
  threadId: THREAD,
  runId: RUN,
  state: {},
  messages: [{ id: "u1", role: "user", content: "hi" }],
  tools: [],
  context: [],
  forwardedProps: {},
};

const collect = (agent: AlixAIAgent): Promise<BaseEvent[]> =>
  new Promise((resolve, reject) => {
    const seen: BaseEvent[] = [];
    agent.run(input).subscribe({ next: (event) => seen.push(event), error: reject, complete: () => resolve(seen) });
  });

/** An agent over a hand-made gateway: the first answer is `first`, every reconnect is answered by `replay(lastEventId)`. */
const agentOver = (first: string, replay: (lastEventId: string | undefined) => string): { agent: AlixAIAgent; lastEventIds: Array<string | undefined> } => {
  const lastEventIds: Array<string | undefined> = [];
  const agent = new AlixAIAgent({
    baseUrl: "http://127.0.0.1:9",
    getContextToken: (request) => `${request.sessionId}.token`,
    sleep: () => Promise.resolve(),
    fetch: async (url, init) => {
      if (init.method === "POST") return eventStream(first);
      expect(url).toBe(`http://127.0.0.1:9/v1/agui/threads/${THREAD}/runs/${RUN}/events`);
      const last = (init.headers as Record<string, string>)["Last-Event-ID"];
      lastEventIds.push(last);
      return eventStream(replay(last));
    },
  });
  return { agent, lastEventIds };
};

describe("a frame cut at the end of the body", () => {
  it("is never dispatched by the reader", async () => {
    const frames: SseFrame[] = [];
    for await (const item of readSseFrames(body("id: 1\ndata: whole\n\nid: 2\ndata: cut"))) frames.push(item);
    expect(frames).toEqual([{ id: "1", data: "whole" }]);
  });

  it("is replayed whole by the reconnect, so the run continues and no event is lost", async () => {
    const cut = `id: 2\ndata: ${JSON.stringify(content).slice(0, 30)}`;
    const { agent, lastEventIds } = agentOver(frame("1", started) + frame(undefined, start) + cut, (last) =>
      last === "1" ? frame(undefined, start) + frame("2", content) + frame(undefined, end) + frame("3", finished) : "",
    );
    const events = await collect(agent);
    expect(events.map((event) => event.type)).toEqual(["RUN_STARTED", "TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_END", "RUN_FINISHED"]);
    expect(lastEventIds).toEqual(["1"]);
  });

  it("does not fail the run when the cut frame is the terminal event", async () => {
    const { agent, lastEventIds } = agentOver(frame("1", started) + frame("2", finished).slice(0, 40), (last) => (last === "1" ? frame("2", finished) : ""));
    const events = await collect(agent);
    expect(events).toEqual([started, finished]);
    expect(lastEventIds).toEqual(["1"]);
  });
});
