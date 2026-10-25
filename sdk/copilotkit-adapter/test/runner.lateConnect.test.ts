import { afterEach, describe, expect, it } from "vitest";
import type { BaseEvent, RunAgentInput } from "@ag-ui/client";
import type { Observable } from "rxjs";
import { createAlixAIRuntime } from "../src/runtime.js";
import type { AlixAIRuntime } from "../src/runtime.js";
import { createMemorySecretStore } from "../src/secrets.js";
import type { AlixAIContextTokenRequest } from "../src/agent.js";
import { mintTokens } from "./helpers/agent-rig.js";
import { startFakeGateway } from "./helpers/fake-gateway.js";
import type { FakeGateway, FakeGatewayOptions } from "./helpers/fake-gateway.js";
import { golden } from "./helpers/golden.js";

// AlixAIRunner.connect against the fake gateway: a connect that a run overlaps is not applied on top of the run. It is held until the run
// is over and read again, so the page gets the history that includes the run (src/runner.ts, ui.lateConnect.test.tsx for the page).

let gateway: FakeGateway | undefined;
afterEach(async () => {
  await gateway?.close();
  gateway = undefined;
});

const lateConnectWait = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

const lateConnectUntil = async (condition: () => boolean, what: string): Promise<void> => {
  for (let waited = 0; !condition(); waited += 10) {
    if (waited > 5000) throw new Error(`timed out waiting for ${what}`);
    await lateConnectWait(10);
  }
};

const lateConnectSetup = async (
  options: FakeGatewayOptions = {},
  beforeToken?: (request: AlixAIContextTokenRequest) => void,
): Promise<{ alixai: AlixAIRuntime; gateway: FakeGateway; tokenRequests: AlixAIContextTokenRequest[] }> => {
  gateway = await startFakeGateway({ rewriteIds: true, ...options });
  const tokenRequests: AlixAIContextTokenRequest[] = [];
  const mint = mintTokens(tokenRequests);
  const alixai = createAlixAIRuntime({
    baseUrl: gateway.url,
    getContextToken: (request) => {
      beforeToken?.(request);
      return mint(request);
    },
    secrets: createMemorySecretStore(),
    sleep: () => Promise.resolve(),
  });
  return { alixai, gateway, tokenRequests };
};

interface LateConnectCollected {
  readonly events: BaseEvent[];
  done: boolean;
  unsubscribe: () => void;
}

const lateConnectCollect = (source: Observable<BaseEvent>): LateConnectCollected => {
  const collected: LateConnectCollected = { events: [], done: false, unsubscribe: () => undefined };
  const subscription = source.subscribe({
    next: (event) => void collected.events.push(event),
    complete: () => void (collected.done = true),
    error: () => void (collected.done = true),
  });
  collected.unsubscribe = () => subscription.unsubscribe();
  return collected;
};

const lateConnectRun = (alixai: AlixAIRuntime, threadId: string, runId: string): Observable<BaseEvent> => {
  const body = golden("plain_answer").meta.request?.body as unknown as RunAgentInput;
  // The runtime hands the runner an agent that already holds the page's messages and thread, as this does.
  const agent = alixai.agent.clone();
  agent.threadId = threadId;
  agent.setMessages(body.messages);
  return alixai.runner.run({ threadId, agent, input: { ...body, threadId, runId } });
};

const lateConnectTypes = (events: readonly BaseEvent[]): string[] => events.map((event) => event.type);
const lateConnectSnapshot = (events: readonly BaseEvent[]): string => JSON.stringify(events.filter((event) => event.type === "MESSAGES_SNAPSHOT"));

describe("AlixAIRunner: a connect answered after a run has started", () => {
  it("lateConnect: a connect read before the run is read again after it, and replays the history that includes the run", async () => {
    const s = await lateConnectSetup({ connectDelayMs: 300 });
    const connect = lateConnectCollect(s.alixai.runner.connect({ threadId: "ses_late_01" }));
    await lateConnectUntil(() => s.gateway.connects.length === 1, "the first connect read");
    const run = lateConnectCollect(lateConnectRun(s.alixai, "ses_late_01", "run_late_01"));
    await lateConnectUntil(() => connect.done && run.done, "the connect and the run");

    expect(s.gateway.connects).toHaveLength(2);
    expect(s.tokenRequests.filter((request) => request.operation === "agui.connect")).toHaveLength(2);
    expect(lateConnectTypes(connect.events)).toEqual(["RUN_STARTED", "MESSAGES_SNAPSHOT", "STATE_DELTA", "RUN_FINISHED"]);
    expect(lateConnectSnapshot(connect.events)).toContain("The item is named gadget.");
    expect(run.events.at(-1)?.type).toBe("RUN_FINISHED");
  });

  it("lateConnect: a connect that finds a run in flight waits for it, then answers with the history that includes it", async () => {
    const s = await lateConnectSetup();
    s.gateway.queue({ golden: "plain_answer", holdAfterFrames: 3 });
    const run = lateConnectCollect(lateConnectRun(s.alixai, "ses_late_02", "run_late_02"));
    await lateConnectUntil(() => s.gateway.requests.length === 1, "the run to reach the gateway");

    const connect = lateConnectCollect(s.alixai.runner.connect({ threadId: "ses_late_02" }));
    await lateConnectUntil(() => s.gateway.connects.length === 1, "the first connect read");
    await lateConnectWait(200);
    expect(connect.done).toBe(false);
    expect(connect.events).toEqual([]);

    expect(await s.alixai.runner.stop({ threadId: "ses_late_02" })).toBe(true);
    await lateConnectUntil(() => connect.done && run.done, "the connect and the run");
    expect(s.gateway.connects).toHaveLength(2);
    expect(lateConnectSnapshot(connect.events)).toContain("show item item-1");
    expect(lateConnectTypes(connect.events)).toEqual(["RUN_STARTED", "MESSAGES_SNAPSHOT", "STATE_DELTA", "RUN_FINISHED"]);
  });

  it("lateConnect: a genuine reconnect (no run in flight) is answered at once, with one read, and replays the history and state", async () => {
    const s = await lateConnectSetup();
    const run = lateConnectCollect(lateConnectRun(s.alixai, "ses_late_07", "run_late_07"));
    await lateConnectUntil(() => run.done, "the run");

    const connect = lateConnectCollect(s.alixai.runner.connect({ threadId: "ses_late_07" }));
    await lateConnectUntil(() => connect.done, "the reconnect");
    expect(s.gateway.connects).toHaveLength(1);
    expect(lateConnectTypes(connect.events)).toEqual(["RUN_STARTED", "MESSAGES_SNAPSHOT", "STATE_DELTA", "RUN_FINISHED"]);
    expect(lateConnectSnapshot(connect.events)).toContain("show item item-1");
    expect(lateConnectSnapshot(connect.events)).toContain("The item is named gadget.");
  });

  it("lateConnect: a connect that a run overlaps on every read answers, after three reads, without history or state", async () => {
    const s = await lateConnectSetup({ connectDelayMs: 250 });
    const connect = lateConnectCollect(s.alixai.runner.connect({ threadId: "ses_late_08" }));
    for (let read = 1; read <= 3; read += 1) {
      await lateConnectUntil(() => s.gateway.connects.length === read, `connect read ${String(read)}`);
      const run = lateConnectCollect(lateConnectRun(s.alixai, "ses_late_08", `run_late_08_${String(read)}`));
      await lateConnectUntil(() => run.done, `run ${String(read)}`);
    }
    await lateConnectUntil(() => connect.done, "the connect");
    expect(s.gateway.connects).toHaveLength(3);
    expect(s.tokenRequests.filter((request) => request.operation === "agui.connect")).toHaveLength(3);
    expect(lateConnectTypes(connect.events)).toEqual(["RUN_STARTED", "RUN_FINISHED"]);
  });

  it("lateConnect: a run on another thread does not hold a connect, and nothing of that thread is in its answer", async () => {
    const s = await lateConnectSetup();
    s.gateway.queue({ golden: "plain_answer", holdAfterFrames: 3 });
    const run = lateConnectCollect(lateConnectRun(s.alixai, "ses_late_03", "run_late_03"));
    await lateConnectUntil(() => s.gateway.requests.length === 1, "the run to reach the gateway");

    const connect = lateConnectCollect(s.alixai.runner.connect({ threadId: "ses_late_04" }));
    await lateConnectUntil(() => connect.done, "the connect of the other thread");
    expect(s.gateway.connects).toHaveLength(1);
    expect(JSON.stringify(connect.events)).not.toContain("show item item-1");
    expect(JSON.stringify(connect.events)).not.toContain("ses_late_03");

    await s.alixai.runner.stop({ threadId: "ses_late_03" });
    await lateConnectUntil(() => run.done, "the run");
  });

  it("lateConnect: a connect the browser abandons while it waits is not read again", async () => {
    const s = await lateConnectSetup();
    s.gateway.queue({ golden: "plain_answer", holdAfterFrames: 3 });
    const run = lateConnectCollect(lateConnectRun(s.alixai, "ses_late_05", "run_late_05"));
    await lateConnectUntil(() => s.gateway.requests.length === 1, "the run to reach the gateway");
    const connect = lateConnectCollect(s.alixai.runner.connect({ threadId: "ses_late_05" }));
    await lateConnectUntil(() => s.gateway.connects.length === 1, "the first connect read");
    await lateConnectWait(100);
    connect.unsubscribe();

    await s.alixai.runner.stop({ threadId: "ses_late_05" });
    await lateConnectUntil(() => run.done, "the run");
    await lateConnectWait(200);
    expect(s.gateway.connects).toHaveLength(1);
    expect(connect.events).toEqual([]);
  });

  it("lateConnect: a connect the host refuses still answers one RUN_ERROR at once while a run is in flight, and reads nothing", async () => {
    const s = await lateConnectSetup({}, (request) => {
      if (request.operation === "agui.connect") throw new Error("secret detail of the host check");
    });
    s.gateway.queue({ golden: "plain_answer", holdAfterFrames: 3 });
    const run = lateConnectCollect(lateConnectRun(s.alixai, "ses_late_06", "run_late_06"));
    await lateConnectUntil(() => s.gateway.requests.length === 1, "the run to reach the gateway");

    const connect = lateConnectCollect(s.alixai.runner.connect({ threadId: "ses_late_06" }));
    await lateConnectUntil(() => connect.done, "the refused connect");
    expect(lateConnectTypes(connect.events)).toEqual(["RUN_STARTED", "RUN_ERROR"]);
    expect(JSON.stringify(connect.events)).not.toContain("secret detail");
    expect(s.gateway.connects).toEqual([]);

    await s.alixai.runner.stop({ threadId: "ses_late_06" });
    await lateConnectUntil(() => run.done, "the run");
  });
});
