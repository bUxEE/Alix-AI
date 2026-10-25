import { AlixAIAgent } from "@alixai/copilotkit-adapter";
import * as aguiClient from "@ag-ui/client";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { CONFIRMATION_SECRET, SECRET_HEADER, answer, connect, golden, goldenEvents, newThread, normalised, pageRunOf, received, run, runAs, startGate, startHost, startUnguardedHost, stop, typesOf } from "./helpers/rig.js";
import type { Gate } from "./helpers/rig.js";
import { runFinished, runStarted, startScriptedGateway } from "./helpers/scripted-gateway.js";
import type { ScriptedGateway } from "./helpers/scripted-gateway.js";
import { row } from "./rows.js";

let gate: Gate;
let scripted: ScriptedGateway | undefined;
beforeEach(async () => {
  gate = await startGate();
});
afterEach(async () => {
  await gate.close();
  await scripted?.close();
  scripted = undefined;
});

const until = async (condition: () => boolean, what: string): Promise<void> => {
  const deadline = Date.now() + 5000;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
};

const posts = (thread: string): Array<{ path: string; headers: Record<string, string>; body: Record<string, any> | undefined }> =>
  gate.gateway.requests.filter((request) => request.body?.["threadId"] === thread || request.path.includes(`/threads/${thread}/`));

describe(row("run-control"), () => {
  it("stop: the page's Stop reaches the gateway as a cancel, and the run ends with the cancelled outcome", async () => {
    const thread = newThread();
    const agent = gate.browser(thread);
    gate.gateway.queue({ golden: "plain_answer", holdAfterFrames: 4 });
    const page = pageRunOf("plain_answer");
    agent.setMessages(page.messages);
    agent.setState(page.state);
    const seen: string[] = [];
    const running = runAs(agent, page.parameters, { onEvent: ({ event }) => void seen.push(event.type) });
    await until(() => seen.length >= 4, "the first frames of the run");

    // What CopilotKit's own client posts to the runtime when the user presses Stop.
    const stopped = await stop(gate, thread, "run_plain");
    expect(stopped).toMatchObject({ status: 200, body: { stopped: true } });
    const observed = await running;
    expect(observed.events.at(-1)).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "cancelled" } });
    // The runtime tells the agent to stop and the agent tells the gateway: that call is made after the page already has its answer.
    await until(() => gate.gateway.cancelled.length > 0, "the gateway to be asked to cancel");
    expect(gate.gateway.cancelled).toEqual(["run_plain"]);
    expect(gate.gateway.requests.map((request) => `${request.method} ${request.path}`)).toContain(`POST /v1/agui/threads/${thread}/runs/run_plain/cancel`);
    // Nothing was answered: the run produced no assistant text.
    expect(agent.messages.some((message) => message.role === "assistant")).toBe(false);
  });

  it("a page that only closes the connection cancels the gateway's run too, once, and needs no stop route", async () => {
    const thread = newThread();
    const agent = gate.browser(thread);
    gate.gateway.queue({ golden: "plain_answer", holdAfterFrames: 4 });
    const page = pageRunOf("plain_answer");
    agent.setMessages(page.messages);
    agent.setState(page.state);
    const seen: string[] = [];
    const running = runAs(agent, page.parameters, { onEvent: ({ event }) => void seen.push(event.type) });
    await until(() => seen.length >= 4, "the first frames of the run");
    agent.abortRun(); // the browser's fetch is aborted: the socket closes, and no request reaches the runtime's stop route
    await running;
    await until(() => gate.gateway.cancelled.length > 0, "the gateway to be asked to cancel after the disconnect");
    await new Promise((resolve) => setTimeout(resolve, 300));
    expect(gate.gateway.cancelled).toEqual(["run_plain"]);
    expect(gate.runtimeRequests.filter((request) => request.includes("/stop/"))).toEqual([]);
    expect(gate.tokenRequests.filter((request) => request.operation === "agui.cancel")).toMatchObject([{ sessionId: thread, runId: "run_plain" }]);
  });

  it("a run that ends on its own is never cancelled by the disconnect that follows it", async () => {
    const agent = gate.browser();
    gate.gateway.queue({ golden: "plain_answer" });
    const observed = await run(agent, "plain_answer");
    expect(observed.events.at(-1)).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "success" } });
    agent.abortRun();
    await new Promise((resolve) => setTimeout(resolve, 400));
    expect(gate.gateway.cancelled).toEqual([]);
    expect(gate.tokenRequests.filter((request) => request.operation === "agui.cancel")).toEqual([]);
  });

  it("retry: a request that never reached the gateway is sent again under the same run id, with a fresh token, and the answer arrives once", async () => {
    const thread = newThread();
    const agent = gate.browser(thread);
    gate.gateway.queue({ golden: "plain_answer", dropBeforeHead: true });
    const observed = await run(agent, "plain_answer");
    expect(observed.error).toBeUndefined();
    expect(received(observed.events, thread)).toEqual(normalised(goldenEvents("plain_answer"), thread));
    expect(posts(thread).map((request) => request.body?.["runId"])).toEqual(["run_plain", "run_plain"]);
    const tokens = gate.tokenRequests.filter((request) => request.sessionId === thread);
    expect(tokens).toHaveLength(2);
    expect(agent.messages.filter((message) => message.role === "assistant")).toHaveLength(1);
  });

  it("regenerate: the same user message sent again under a new run id is a new turn on the same thread", async () => {
    const thread = newThread();
    const agent = gate.browser(thread);
    gate.gateway.queue({ golden: "plain_answer" }, { golden: "plain_answer" });
    await run(agent, "plain_answer");
    // What the page's Regenerate does: drop the answer, keep the question, run again.
    agent.setMessages(agent.messages.filter((message) => message.role === "user"));
    const again = await runAs(agent, { runId: "run_regenerated" });
    expect(again.error).toBeUndefined();
    expect(posts(thread).map((request) => request.body?.["runId"])).toEqual(["run_plain", "run_regenerated"]);
    expect(posts(thread)[1]?.body?.["messages"]).toEqual([{ id: "m1", role: "user", content: "show item item-1" }]);
    expect(agent.messages.filter((message) => message.role === "assistant")).toHaveLength(1);
  });

  const cuts = [1, 2, 5, 12, 19] as const;
  it.each(cuts)("reconnect: a stream cut after %i frames is picked up from its cursor, and the page sees every event exactly once", async (cut) => {
    const thread = newThread();
    const agent = gate.browser(thread);
    gate.gateway.queue({ golden: "plain_answer", cutAfterFrames: cut });
    const observed = await run(agent, "plain_answer");
    expect(observed.error).toBeUndefined();
    expect(received(observed.events, thread)).toEqual(normalised(goldenEvents("plain_answer"), thread));
    const read = posts(thread).filter((request) => request.path.endsWith("/events"));
    expect(read).toHaveLength(1);
    // The cursor is the id of the last turn-event boundary the page had (only the last frame of a group carries an id), and the
    // frames after it are resent and checked against what was already delivered, not repeated.
    const cursor = golden("plain_answer").frames.slice(0, cut).map((frame) => frame.id).filter((id) => id !== undefined).at(-1);
    expect(read[0]?.headers["last-event-id"]).toBe(cursor);
  });

  it("reconnect in the middle of the confirmation call: the interrupt still arrives whole and the approval still works", async () => {
    const thread = newThread();
    const agent = gate.browser(thread);
    const cut = goldenEvents("prepare_interrupt").findIndex((event) => event.type === "TOOL_CALL_ARGS") + 1;
    gate.gateway.queue({ golden: "prepare_interrupt", cutAfterFrames: cut }, { golden: "resume_approve" });
    const prepared = await run(agent, "prepare_interrupt");
    expect(prepared.error).toBeUndefined();
    expect(received(prepared.events, thread)).toEqual(normalised(goldenEvents("prepare_interrupt"), thread));
    expect(agent.pendingInterrupts).toHaveLength(1);

    const approved = await answer(agent, true, "run_approve");
    expect(approved.events.at(-1)).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "success" } });
    expect(posts(thread).at(-1)?.headers[SECRET_HEADER]).toBe(CONFIRMATION_SECRET);
  });
});

describe(row("history"), () => {
  it("connect goes through the agent: the host's callback is asked for the caller, the gateway's connect run answers with the session's history, and nothing is read from the runtime's memory", async () => {
    const thread = newThread();
    const agent = gate.browser(thread);
    gate.gateway.queue({ golden: "plain_answer" });
    await run(agent, "plain_answer");
    const tokens = gate.tokenRequests.length;

    const joined = await connect(gate, thread, { "X-User": "alice", Authorization: "Bearer page-session" });
    expect(joined.status).toBe(200);
    expect(joined.events.map((event) => event["type"])).toEqual(["RUN_STARTED", "MESSAGES_SNAPSHOT", "STATE_DELTA", "RUN_FINISHED"]);
    // The history is the gateway's (the AlixAI session): the user's message and the verified answer, with no `input` echo of the browser's request.
    expect(joined.events[0]).toMatchObject({ type: "RUN_STARTED", threadId: thread });
    expect(joined.events[0]?.["input"]).toBeUndefined();
    expect(joined.events[1]?.["messages"]).toMatchObject([
      { role: "user", content: "show item item-1" },
      { role: "assistant", content: "The item is named gadget." },
    ]);
    expect(joined.events.at(-1)).toMatchObject({ type: "RUN_FINISHED", result: { connect: true }, outcome: { type: "success" } });
    // The callback was asked once, as a connect, for this thread, with the headers the browser sent; the gateway saw the connect run.
    expect(gate.tokenRequests).toHaveLength(tokens + 1);
    expect(gate.tokenRequests.at(-1)).toMatchObject({
      operation: "agui.connect",
      sessionId: thread,
      resume: false,
      forwardedHeaders: { "x-user": "alice", authorization: "Bearer page-session" },
    });
    expect(gate.gateway.connects).toHaveLength(1);
    expect(gate.gateway.connects[0]?.body).toMatchObject({ threadId: thread, messages: [] });
    expect(gate.gateway.connects[0]?.body?.["resume"]).toBeUndefined();
  });

  it("a restarted host (a runtime with an empty memory, the same gateway) still shows the session's history", async () => {
    const thread = newThread();
    const agent = gate.browser(thread);
    gate.gateway.queue({ golden: "plain_answer" });
    await run(agent, "plain_answer");

    // Its own token sequence: the gateway's tokens are single use, and a new process starts counting again.
    const restarted = await startHost(gate.gateway.url, { getContextToken: (request) => `${request.sessionId}.after_restart` });
    try {
      const joined = await connect(restarted, thread);
      expect(joined.events[1]).toMatchObject({ type: "MESSAGES_SNAPSHOT", messages: [{ role: "user" }, { role: "assistant", content: "The item is named gadget." }] });
    } finally {
      await restarted.close();
    }
  });

  it("a connect to a thread nothing was said in is an empty history from the gateway, not an error", async () => {
    const joined = await connect(gate, newThread());
    expect(joined.events.map((event) => event["type"])).toEqual(["RUN_STARTED", "MESSAGES_SNAPSHOT", "STATE_DELTA", "RUN_FINISHED"]);
    expect(joined.events[1]?.["messages"]).toEqual([]);
  });

  it("AlixAIAgent's own connect run asks the gateway for the thread and the real client applies the history and the state (SYNTHETIC stream built from internal/agui/connect.go)", async () => {
    scripted = await startScriptedGateway(
      [
        runStarted(),
        {
          type: "MESSAGES_SNAPSHOT",
          messages: [
            { id: "msg_trn_01J00000000000000000000001_user", role: "user", content: "show item item-1" },
            { id: "msg_trn_01J00000000000000000000001_1", role: "assistant", content: "The item is named gadget." },
          ],
        },
        { type: "STATE_SNAPSHOT", snapshot: { alixai: { pending: null } } },
        runFinished({ connect: true }),
      ],
      { ids: false },
    );
    const thread = newThread();
    const agent = new AlixAIAgent({ baseUrl: scripted.url, getContextToken: (request) => `${request.sessionId}.1`, sleep: () => Promise.resolve() });
    agent.threadId = thread;
    agent.setMessages([{ id: "stale_local", role: "user", content: "a message the page held that the session does not" }]);
    const result = await agent.connectAgent({ runId: "run_connect" });
    expect(result.result).toEqual({ connect: true });

    expect(scripted.requests[0]).toMatchObject({ threadId: thread, runId: "run_connect", messages: [] });
    expect(scripted.requests[0]?.["resume"]).toBeUndefined();
    expect(agent.messages).toEqual([
      { id: "msg_trn_01J00000000000000000000001_user", role: "user", content: "show item item-1" },
      { id: "msg_trn_01J00000000000000000000001_1", role: "assistant", content: "The item is named gadget." },
    ]);
    expect(agent.state).toEqual({ alixai: { pending: null } });
  });
});

describe(row("history"), () => {
  it("LIMIT: a page that uses a self-managed HttpAgent (a Go or Python host's proxy) has no connect at all: @ag-ui/client 1.0.1 does not implement it", async () => {
    // Nothing listens at this address: a request would fail, and connectAgent answers with an empty result without making one.
    const agent = new aguiClient.HttpAgent({ url: "http://127.0.0.1:9/agui/", threadId: newThread() });
    agent.setMessages([{ id: "m1", role: "user", content: "held by the page" }]);
    await expect(agent.connectAgent()).resolves.toEqual({ result: undefined, newMessages: [] });
    expect(agent.messages).toEqual([{ id: "m1", role: "user", content: "held by the page" }]);
  });
});

describe(row("thread-isolation"), () => {
  const PREPARATION = "prep_01J00000000000000000000001";

  it("thread B cannot approve or decline thread A's interrupt, and A's approval is untouched by the attempts", async () => {
    const a = gate.browser(newThread());
    const b = gate.browser(newThread());
    gate.gateway.queue({ golden: "prepare_interrupt" });
    await run(a, "prepare_interrupt");
    const requestsBefore = gate.gateway.requests.length;

    // Approving: B's host holds no secret for (B, preparation): refused before a token is spent or a request is made.
    const approve = await runAs(b, { runId: "run_b_approve", resume: [{ interruptId: PREPARATION, status: "resolved", payload: { approved: true } }] });
    expect(approve.events.at(-1)).toMatchObject({ type: "RUN_ERROR", message: expect.stringContaining("no confirmation secret") });
    expect(gate.gateway.requests).toHaveLength(requestsBefore);

    // Declining needs no secret, so it reaches the gateway, which answers with the identical not-resolved error.
    const decline = await runAs(b, { runId: "run_b_decline", resume: [{ interruptId: PREPARATION, status: "resolved", payload: { approved: false } }] });
    expect(decline.events.at(-1)).toMatchObject({ type: "RUN_ERROR", message: "the thread or the interrupt does not resolve for this caller" });
    expect(typesOf(decline.events)).not.toContain("TOOL_CALL_RESULT");

    // A is as it was: its interrupt is pending, its secret is held, and it can still approve.
    expect(a.pendingInterrupts).toHaveLength(1);
    gate.gateway.queue({ golden: "resume_approve" });
    const approved = await answer(a, true, "run_a_approve");
    expect(approved.events.at(-1)).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "success" } });
  });

  // SECURITY (M3-28 finding, fixed in M3-39): CopilotRuntime answered /connect from its own memory, so naming a thread was enough.
  describe("connect is authenticated like a run", () => {
    const owners: Record<string, string> = {};

    /** The host under test, behind a callback that authenticates by header. */
    const startAliceGate = async (): Promise<void> => {
      await gate.close();
      let counter = 0;
      gate = await startGate({
        getContextToken: ({ sessionId, forwardedHeaders }) => {
          counter += 1;
          // A callback that authenticates the user but mints for the wrong session: the gateway's own check is the second line.
          if (forwardedHeaders["x-user"] === "mallory") return `ses_mallory.${counter}`;
          if (owners[sessionId] === undefined || owners[sessionId] !== forwardedHeaders["x-user"]) throw new Error("not your session");
          return `${sessionId}.${counter}`;
        },
      });
    };

    /** Alice's browser runs a thread with a pending confirmation (its effects must never leave) through the runtime at `runtimeUrl`. */
    const aliceRuns = async (runtimeUrl: string): Promise<string> => {
      const thread = newThread();
      owners[thread] = "alice";
      const alice = new aguiClient.HttpAgent({ url: `${runtimeUrl}/agent/default/run`, threadId: thread, headers: { "X-User": "alice" } });
      gate.gateway.queue({ golden: "prepare_interrupt" });
      await run(alice, "prepare_interrupt");
      expect(alice.pendingInterrupts).toHaveLength(1);
      return thread;
    };

    /** Alice's thread, run through the host under test. */
    const aliceThread = async (): Promise<string> => {
      await startAliceGate();
      return aliceRuns(gate.runtimeUrl);
    };

    /** Anything of Alice's thread: the tool call and its effects, the capability, the preparation, her message. */
    const leaked = (text: string): boolean => /TOOL_CALL|widget|items\.create|prep_01J/.test(text);

    it("CONTROL: a runtime with CopilotKit's default runner, which served Alice's run itself, hands the same probe her confirmation effects with no callback and no gateway", async () => {
      await startAliceGate();
      const unguarded = await startUnguardedHost(gate.alixai);
      try {
        const thread = await aliceRuns(unguarded.runtimeUrl);
        const requests = gate.gateway.requests.length + gate.gateway.connects.length;
        const tokens = gate.tokenRequests.length;
        const intruder = await connect(unguarded, thread);
        expect(leaked(intruder.text)).toBe(true);
        expect(intruder.events.some((event) => event["type"] === "TOOL_CALL_ARGS")).toBe(true);
        expect(await (await fetch(`${unguarded.runtimeUrl}/threads`)).text()).toContain(thread);
        expect(leaked(await (await fetch(`${unguarded.runtimeUrl}/threads/${thread}/events`)).text())).toBe(true);
        expect(gate.gateway.requests.length + gate.gateway.connects.length).toBe(requests);
        expect(gate.tokenRequests).toHaveLength(tokens);
      } finally {
        await unguarded.close();
      }
    });

    it("another CopilotRuntime in the same process (CopilotKit's default runner, whose store every in-memory runner shares) reads nothing of a thread the AlixAI runner ran", async () => {
      const thread = await aliceThread();
      const other = await startUnguardedHost(gate.alixai);
      try {
        const joined = await connect(other, thread);
        expect(joined.events).toEqual([]);
        expect(leaked(joined.text)).toBe(false);
        expect(await (await fetch(`${other.runtimeUrl}/threads`)).text()).not.toContain(thread);
        expect(leaked(await (await fetch(`${other.runtimeUrl}/threads/${thread}/events`)).text())).toBe(false);
        expect(leaked(await (await fetch(`${other.runtimeUrl}/threads/${thread}/messages`)).text())).toBe(false);
      } finally {
        await other.close();
      }
    });

    it("Bob (another user) cannot join Alice's thread: the callback refuses him, the gateway is never asked, and not one event of her thread leaves", async () => {
      const thread = await aliceThread();
      const asked = gate.gateway.connects.length;
      const bob = await connect(gate, thread, { "X-User": "bob" });
      expect(bob.status).toBe(200);
      expect(bob.events.map((event) => event["type"])).toEqual(["RUN_STARTED", "RUN_ERROR"]);
      expect(bob.events[1]).toMatchObject({ type: "RUN_ERROR", code: "unauthenticated" });
      expect(leaked(bob.text)).toBe(false);
      expect(bob.text).not.toContain("not your session");
      expect(gate.gateway.connects).toHaveLength(asked);
      expect(gate.tokenRequests.at(-1)).toMatchObject({ operation: "agui.connect", sessionId: thread, forwardedHeaders: { "x-user": "bob" } });
    });

    it("an anonymous request (no identity header at all) is refused the same way", async () => {
      const thread = await aliceThread();
      const anonymous = await connect(gate, thread);
      expect(anonymous.events.map((event) => event["type"])).toEqual(["RUN_STARTED", "RUN_ERROR"]);
      expect(leaked(anonymous.text)).toBe(false);
    });

    it("a host callback that authenticates but mints a token for the WRONG session gets nowhere: the gateway's scope check refuses and nothing of Alice's thread is replayed", async () => {
      const thread = await aliceThread();
      const refused = await connect(gate, thread, { "X-User": "mallory" });
      expect(refused.events.map((event) => event["type"])).toEqual(["RUN_STARTED", "RUN_ERROR"]);
      expect(refused.events[1]).toMatchObject({ code: "scope_violation", message: "the thread or the interrupt does not resolve for this caller" });
      expect(leaked(refused.text)).toBe(false);
      expect(gate.gateway.connects.at(-1)?.headers["authorization"]).toMatch(/^Bearer ses_mallory\./);
    });

    it("each thread replays only itself: Bob's own thread holds none of Alice's, and Alice's holds hers", async () => {
      const aliceId = await aliceThread();
      const bobId = newThread();
      owners[bobId] = "bob";
      const bob = gate.browser(bobId, { "X-User": "bob" });
      gate.gateway.queue({ golden: "plain_answer" });
      await run(bob, "plain_answer");

      const own = await connect(gate, bobId, { "X-User": "bob" });
      expect(own.events[1]).toMatchObject({ type: "MESSAGES_SNAPSHOT", messages: [{ role: "user", content: "show item item-1" }, { role: "assistant" }] });
      expect(leaked(own.text)).toBe(false);
      const hers = await connect(gate, aliceId, { "X-User": "alice" });
      expect(hers.events[1]?.["messages"]).toMatchObject([{ role: "user", content: "create an item named widget with quantity 3" }, { role: "assistant" }]);
    });

    it("the runtime's in-memory thread routes are off: no listing of threads, no events, no messages, no state, and clear wipes nothing", async () => {
      const thread = await aliceThread();
      for (const path of ["/threads", `/threads/${thread}/events`, `/threads/${thread}/messages`, `/threads/${thread}/state`]) {
        const response = await fetch(`${gate.runtimeUrl}${path}`);
        const body = await response.text();
        expect(response.status, path).toBeGreaterThanOrEqual(400);
        expect(leaked(body), path).toBe(false);
        expect(body, path).not.toContain(thread);
      }
      const info = (await (await fetch(`${gate.runtimeUrl}/info`)).json()) as { threads?: unknown };
      expect(JSON.stringify(info)).not.toContain(thread);
      expect((gate.alixai.runner as { ɵsupportsLocalThreadEndpoints?: boolean }).ɵsupportsLocalThreadEndpoints).not.toBe(true);
    });
  });
});

describe(row("transports"), () => {
  it("SSE: the page's stream is text/event-stream from the runtime, and the adapter asks the gateway for the same", async () => {
    const agent = gate.browser();
    gate.gateway.queue({ golden: "plain_answer" });
    await run(agent, "plain_answer");
    expect((gate.traffic[0]?.headers ?? "").toLowerCase()).toContain("content-type: text/event-stream");
    expect(gate.gateway.requests[0]?.headers["accept"]).toBe("text/event-stream");
  });

  it("WebSocket cannot be shown with a CopilotKit or AG-UI client of the pinned releases: @ag-ui/client has none (the gateway side is proven in Go by TestAGUIWebSocketMatchesSSE)", () => {
    expect(Object.keys(aguiClient).filter((name) => /web.?socket|^ws/i.test(name))).toEqual([]);
  });
});
