import { afterEach, describe, expect, it } from "vitest";
import { newThread, run, runAs, say, startGate, startHost, typesOf } from "./helpers/rig.js";
import type { Gate, Host } from "./helpers/rig.js";
import { row } from "./rows.js";
import { runFinished, runStarted, startScriptedGateway, textMessage } from "./helpers/scripted-gateway.js";
import type { ScriptedGateway } from "./helpers/scripted-gateway.js";

let gate: Gate | undefined;
let host: Host | undefined;
let scripted: ScriptedGateway | undefined;
afterEach(async () => {
  await gate?.close();
  await host?.close();
  await scripted?.close();
  gate = host = scripted = undefined;
});

describe(row("chat"), () => {
  it("the runtime advertises the AlixAI agent to a CopilotKit page", async () => {
    gate = await startGate();
    const response = await fetch(`${gate.runtimeUrl}/info`);
    expect(response.status).toBe(200);
    const info = (await response.json()) as { agents?: Record<string, { name?: string; className?: string }> };
    expect(Object.keys(info.agents ?? {})).toEqual(["default"]);
  });

  it("headless: a run through the runtime leaves the verified answer in the client's messages and nothing pending", async () => {
    gate = await startGate();
    const agent = gate.browser();
    gate.gateway.queue({ golden: "plain_answer" });
    const observed = await run(agent, "plain_answer");
    expect(observed.error).toBeUndefined();
    expect(observed.result?.result).toMatchObject({ finishReason: "completed" });
    expect(agent.messages.map((message) => message.role)).toEqual(["user", "activity", "assistant"]);
    expect(agent.messages.at(-1)).toMatchObject({ role: "assistant", content: "The item is named gadget." });
    expect(agent.pendingInterrupts).toEqual([]);
  });

  it("a thread goes on: the second run sends the conversation so far and asks for a new turn under a new run id", async () => {
    gate = await startGate();
    const thread = newThread();
    const agent = gate.browser(thread);
    gate.gateway.queue({ golden: "plain_answer" }, { golden: "suggestions" });
    await run(agent, "plain_answer");
    const second = await say(agent, "what can you do", "run_second");
    expect(second.error).toBeUndefined();
    const bodies = gate.gateway.requests.map((request) => request.body).filter((body) => body?.["threadId"] === thread);
    expect(bodies.map((body) => body?.["runId"])).toEqual(["run_plain", "run_second"]);
    // The first turn's answer is part of what the page sends (the AG-UI client leaves its own activity messages out); the last
    // message is the new user message.
    expect(bodies[1]?.["messages"].map((message: { role: string }) => message.role)).toEqual(["user", "assistant", "user"]);
    expect(bodies[1]?.["messages"].at(-1)).toMatchObject({ role: "user", content: "what can you do" });
  });
});

describe(row("streaming"), () => {
  it("fragments reach the page while the gateway is still producing the rest: the stream is not buffered by the runtime", async () => {
    scripted = await startScriptedGateway([runStarted(), ...textMessage("msg_s_1", ["The ", "item ", "is ", "gadget."], 1), runFinished()]);
    host = await startHost(scripted.url);
    const agent = host.browser();
    agent.setMessages([{ id: "u1", role: "user", content: "show item item-1" }]);
    agent.setState({});

    const fragments: string[] = [];
    let firstFragment!: () => void;
    const seenFirst = new Promise<void>((resolve) => (firstFragment = resolve));
    const running = runAs(
      agent,
      { runId: "run_stream" },
      {
        onTextMessageContentEvent: ({ event }) => {
          fragments.push(event.delta);
          firstFragment();
        },
      },
    );

    // The gateway is paused after the first fragment: the page has it, and the run is still going.
    await seenFirst;
    expect(fragments).toEqual(["The "]);
    expect(agent.isRunning).toBe(true);
    expect(agent.messages.some((message) => message.role === "assistant")).toBe(true);

    scripted.release();
    const observed = await running;
    expect(observed.error).toBeUndefined();
    expect(fragments).toEqual(["The ", "item ", "is ", "gadget."]);
    expect(agent.messages.at(-1)).toMatchObject({ id: "msg_s_1", role: "assistant", content: "The item is gadget." });
    expect(typesOf(observed.events)).toEqual([
      "RUN_STARTED",
      "TEXT_MESSAGE_START",
      "TEXT_MESSAGE_CONTENT",
      "TEXT_MESSAGE_CONTENT",
      "TEXT_MESSAGE_CONTENT",
      "TEXT_MESSAGE_CONTENT",
      "TEXT_MESSAGE_END",
      "RUN_FINISHED",
    ]);
  });

  it("a golden answer is one message: start, one content event, end, with the same message id", async () => {
    gate = await startGate();
    const agent = gate.browser();
    gate.gateway.queue({ golden: "plain_answer" });
    const observed = await run(agent, "plain_answer");
    const text = observed.events.filter((event) => event.type.startsWith("TEXT_MESSAGE"));
    expect(text.map((event) => event.type)).toEqual(["TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_END"]);
    expect(new Set(text.map((event) => (event as { messageId?: string }).messageId)).size).toBe(1);
  });
});

describe(row("lifecycle"), () => {
  it("a run starts first, finishes last, and every step that starts finishes, in order", async () => {
    gate = await startGate();
    const agent = gate.browser();
    gate.gateway.queue({ golden: "plain_answer" });
    const observed = await run(agent, "plain_answer");
    const types = typesOf(observed.events);
    expect(types[0]).toBe("RUN_STARTED");
    expect(types.at(-1)).toBe("RUN_FINISHED");
    const open: string[] = [];
    for (const event of observed.events) {
      const step = (event as { stepName?: string }).stepName;
      if (event.type === "STEP_STARTED") open.push(step as string);
      if (event.type === "STEP_FINISHED") expect(open.pop()).toBe(step);
    }
    expect(open).toEqual([]);
    expect(types.filter((type) => type === "STEP_STARTED").length).toBeGreaterThan(0);
  });

  it("a refused request: the explaining text is shown first, then RUN_ERROR with the core's code, and never a success-looking finish", async () => {
    gate = await startGate();
    const agent = gate.browser();
    gate.gateway.queue({ golden: "error" });
    const page = await run(agent, "error");
    const finish = page.events.at(-1) as { type: string; message?: string; code?: string };
    expect(finish.type).toBe("RUN_ERROR");
    expect(finish.code).toBe("unknown_capability");
    expect(typesOf(page.events)).not.toContain("RUN_FINISHED");
    expect(agent.messages.at(-1)).toMatchObject({ role: "assistant", content: expect.stringContaining("not able to do that") });
    expect(finish.message).toContain("isn't something I can do");
  });

  it("a request the gateway refuses before any stream ends in RUN_ERROR with the gateway's own message, not in a finish", async () => {
    gate = await startGate();
    const agent = gate.browser();
    gate.gateway.queue({ golden: "plain_answer", fail: { status: 400, code: "validation_failed", message: "the run input is malformed" } });
    const observed = await run(agent, "plain_answer");
    const last = observed.events.at(-1) as { type: string; message?: string };
    expect(last.type).toBe("RUN_ERROR");
    // The typed AlixAI code does not survive CopilotRuntime (it reports its own code for a run that failed before streaming),
    // so the message is what a page can show; docs/integration/copilotkit-compatibility.md lists this as a limit.
    expect(last.message).toContain("the run input is malformed");
    expect(typesOf(observed.events)).not.toContain("RUN_FINISHED");
    expect(agent.messages.filter((message) => message.role === "assistant")).toEqual([]);
  });
});
