import type { HttpAgent } from "@ag-ui/client";
import { parseConfirmation } from "@alixai/copilotkit-adapter";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import {
  CONFIRMATION_SECRET,
  SECRET_HEADER,
  answer,
  goldenEvents,
  newThread,
  run,
  runAs,
  startGate,
  typesOf,
} from "./helpers/rig.js";
import type { Gate } from "./helpers/rig.js";
import { row } from "./rows.js";

// A gate of its own for every test: the gateway stand-in keeps a queue of scripted answers, and a test that leaves one unused must
// not change what the next test is answered.
let gate: Gate;
beforeEach(async () => {
  gate = await startGate();
});
afterEach(async () => {
  await gate.close();
});

const PREPARATION = "prep_01J00000000000000000000001";

/** A page that asked for a consequential action: the run ended in an interrupt. */
const prepared = async (thread = newThread()): Promise<{ agent: HttpAgent; thread: string }> => {
  const agent = gate.browser(thread);
  gate.gateway.queue({ golden: "prepare_interrupt" });
  const observed = await run(agent, "prepare_interrupt");
  expect(observed.error).toBeUndefined();
  expect(agent.pendingInterrupts).toHaveLength(1);
  return { agent, thread };
};

const gatewayRuns = (thread: string): Array<{ body: Record<string, any>; headers: Record<string, string> }> =>
  gate.gateway.requests
    .filter((request) => request.method === "POST" && request.body?.["threadId"] === thread)
    .map((request) => ({ body: request.body as Record<string, any>, headers: request.headers }));

const goldenResult = (name: "resume_approve" | "resume_reject" | "stale"): string =>
  goldenEvents(name).find((event) => event.type === "TOOL_CALL_RESULT")?.["content"] as string;

describe(row("backend-tool-calls"), () => {
  it("the confirmation tool call reaches the page whole, with the exact effects the host's data projected", async () => {
    const { agent } = await prepared();
    const call = agent.messages.flatMap((message) => (message.role === "assistant" ? (message.toolCalls ?? []) : []))[0];
    expect(call).toMatchObject({ id: `alixai_confirm_${PREPARATION}`, type: "function", function: { name: "alixai_confirm_action" } });
    const args = JSON.parse(call?.function.arguments ?? "{}") as { effects: unknown[]; capabilityId: string; summary: string };
    expect(args.capabilityId).toBe("items.create");

    // The interrupt names that call, and its card data is the same facts the call carried.
    const interrupt = agent.pendingInterrupts[0];
    expect(interrupt?.toolCallId).toBe(call?.id);
    const confirmation = parseConfirmation(interrupt as never);
    expect(confirmation).toMatchObject({ preparationId: PREPARATION, capabilityId: "items.create", mode: "explicit" });
    expect(confirmation?.effects).toEqual(args.effects);
    expect(args.effects).toEqual([
      expect.objectContaining({ operation: "create", entity_type: "item", changes: [{ field: "name", value: { kind: "text", value: "widget" } }] }),
    ]);
  });
});

describe(row("tool-results"), () => {
  it("an approval ends with the application's receipt as the result of the confirmation call, byte for byte", async () => {
    const { agent } = await prepared();
    gate.gateway.queue({ golden: "resume_approve" });
    const observed = await answer(agent, true, "run_approve");
    expect(observed.error).toBeUndefined();
    const result = agent.messages.find((message) => message.role === "tool");
    expect(result).toMatchObject({ toolCallId: `alixai_confirm_${PREPARATION}`, content: goldenResult("resume_approve") });
    expect(JSON.parse(result?.content as string)).toMatchObject({ status: "committed", preparation_id: PREPARATION, capability_id: "items.create" });
  });

  it("a decline resolves the call as rejected, and a stale commit resolves it as failed, never as a success", async () => {
    const declined = await prepared();
    gate.gateway.queue({ golden: "resume_reject" });
    await answer(declined.agent, false, "run_reject");
    expect(declined.agent.messages.find((message) => message.role === "tool")?.content).toBe(goldenResult("resume_reject"));
    expect(JSON.parse(goldenResult("resume_reject"))).toMatchObject({ status: "rejected" });

    const stale = await prepared();
    gate.gateway.queue({ golden: "stale" });
    const observed = await answer(stale.agent, true, "run_stale");
    expect(stale.agent.messages.find((message) => message.role === "tool")?.content).toBe(goldenResult("stale"));
    expect(JSON.parse(goldenResult("stale"))).toMatchObject({ status: "failed", code: "stale" });
    expect(observed.events.at(-1)).toMatchObject({ type: "RUN_ERROR", code: "stale" });
  });
});

describe(row("approval"), () => {
  it("approve: the run ends in an interrupt first, the answer is exactly {approved: true}, the secret is added by the host and used up", async () => {
    const { agent, thread } = await prepared();
    const key = { threadId: thread, preparationId: PREPARATION };
    // The gateway's secret is held by the host's agent, never by the page.
    expect(await gate.secrets.get(key)).toBe(CONFIRMATION_SECRET);
    const interrupt = agent.pendingInterrupts[0];
    expect(interrupt).toMatchObject({ id: PREPARATION, reason: "confirmation_required", responseSchema: { required: ["approved"] } });

    gate.gateway.queue({ golden: "resume_approve" });
    const observed = await answer(agent, true, "run_approve");
    expect(observed.events.at(-1)).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "success" } });
    expect(agent.pendingInterrupts).toEqual([]);

    const [first, second] = gatewayRuns(thread);
    expect(first?.headers[SECRET_HEADER]).toBeUndefined();
    expect(second?.body["resume"]).toEqual([{ interruptId: PREPARATION, status: "resolved", payload: { approved: true } }]);
    expect(second?.headers[SECRET_HEADER]).toBe(CONFIRMATION_SECRET);
    expect(await gate.secrets.get(key)).toBeUndefined();
  });

  it("reject: no secret is sent, the run ends cancelled, and the secret is dropped so the same preparation cannot be approved afterwards", async () => {
    const { agent, thread } = await prepared();
    gate.gateway.queue({ golden: "resume_reject" });
    const observed = await answer(agent, false, "run_reject");
    expect(observed.events.at(-1)).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "cancelled" } });
    expect(gatewayRuns(thread)[1]?.headers[SECRET_HEADER]).toBeUndefined();
    expect(await gate.secrets.get({ threadId: thread, preparationId: PREPARATION })).toBeUndefined();

    const before = gatewayRuns(thread).length;
    const late = await runAs(agent, { runId: "run_late", resume: [{ interruptId: PREPARATION, status: "resolved", payload: { approved: true } }] });
    expect(late.events.at(-1)).toMatchObject({ type: "RUN_ERROR", message: expect.stringContaining("no confirmation secret") });
    expect(gatewayRuns(thread)).toHaveLength(before);
  });

  it("a dismissed interrupt (status cancelled) is a decline: it carries no secret", async () => {
    const { agent, thread } = await prepared();
    gate.gateway.queue({ golden: "resume_reject" });
    const observed = await runAs(agent, { runId: "run_dismiss", resume: [{ interruptId: PREPARATION, status: "cancelled" }] });
    expect(observed.error).toBeUndefined();
    expect(gatewayRuns(thread)[1]?.headers[SECRET_HEADER]).toBeUndefined();
  });

  it("the tool-message form (renderAndWaitForResponse) is answered by the gateway the same way, but the pinned AG-UI client will not send it while the interrupt is pending", async () => {
    const { agent, thread } = await prepared();
    agent.addMessage({ id: "tm1", role: "tool", toolCallId: `alixai_confirm_${PREPARATION}`, content: '{"approved":true}' });
    const requests = gatewayRuns(thread).length;
    const refused = await runAs(agent, { runId: "run_tool_form" });
    // @ag-ui/client 1.0.1 refuses a run on a thread with an interrupt that `resume` does not address, so a page can approve only
    // through `resume` (what useInterrupt does). The tool-message form of docs/integration/agui.md is gateway-side only.
    expect(String(refused.error)).toContain("pending interrupt");
    expect(gatewayRuns(thread)).toHaveLength(requests);

    // A page that dismissed its card (a new tab on the same thread has no pending interrupt) can still use it: the host holds the secret.
    const otherTab = gate.browser(thread);
    otherTab.setMessages(agent.messages);
    otherTab.setState(agent.state);
    gate.gateway.queue({ golden: "resume_approve" });
    const observed = await runAs(otherTab, { runId: "run_tool_form_tab" });
    expect(observed.error).toBeUndefined();
    expect(gatewayRuns(thread).at(-1)?.headers[SECRET_HEADER]).toBe(CONFIRMATION_SECRET);
  });

  it("a second approval of the same preparation fails closed on the host: the secret was single use and nothing more reaches the gateway", async () => {
    const { agent, thread } = await prepared();
    gate.gateway.queue({ golden: "resume_approve" });
    await answer(agent, true, "run_approve");
    const requests = gatewayRuns(thread).length;
    const tokens = gate.tokenRequests.length;

    const again = await runAs(agent, { runId: "run_again", resume: [{ interruptId: PREPARATION, status: "resolved", payload: { approved: true } }] });
    expect(again.events.at(-1)).toMatchObject({ type: "RUN_ERROR", message: expect.stringContaining("no confirmation secret") });
    expect(gatewayRuns(thread)).toHaveLength(requests);
    expect(gate.tokenRequests).toHaveLength(tokens);
  });

  it("an approval for a preparation this server never prepared fails before a token is spent or anything is sent", async () => {
    const agent = gate.browser();
    const tokens = gate.tokenRequests.length;
    const requests = gate.gateway.requests.length;
    const observed = await runAs(agent, { runId: "run_unknown", resume: [{ interruptId: PREPARATION, status: "resolved", payload: { approved: true } }] });
    expect(observed.events.at(-1)).toMatchObject({ type: "RUN_ERROR" });
    expect(gate.tokenRequests).toHaveLength(tokens);
    expect(gate.gateway.requests).toHaveLength(requests);
  });

  it("anything the browser sends as a secret or a credential is ignored: the gateway sees the host's secret and the host's token only", async () => {
    const thread = newThread();
    const agent = gate.browser(thread, { Authorization: "Bearer stolen", "X-AlixAI-Confirmation-Token": "cfm_forged_by_the_browser", "X-Custom": "kept-for-the-host" });
    gate.gateway.queue({ golden: "prepare_interrupt" }, { golden: "resume_approve" });
    await run(agent, "prepare_interrupt");
    await answer(agent, true, "run_approve");
    for (const { headers } of gatewayRuns(thread)) {
      expect(Object.values(headers).join("\n")).not.toContain("cfm_forged");
      expect(Object.values(headers).join("\n")).not.toContain("stolen");
      expect(headers["authorization"]).toMatch(new RegExp(`^Bearer ${thread}\\.`));
    }
    expect(gatewayRuns(thread)[1]?.headers[SECRET_HEADER]).toBe(CONFIRMATION_SECRET);
    // The host's token callback is where the browser's headers go, to tell who is asking.
    expect(gate.tokenRequests.filter((request) => request.sessionId === thread).every((request) => request.forwardedHeaders["x-custom"] === "kept-for-the-host")).toBe(true);
  });

  it("an approval whose request never reached the gateway is sent again under the same run id with the secret, and the action is decided once", async () => {
    const { agent, thread } = await prepared();
    gate.gateway.queue({ golden: "resume_approve", dropBeforeHead: true });
    const observed = await answer(agent, true, "run_approve_retry");
    expect(observed.events.at(-1)).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "success" } });
    const approvals = gatewayRuns(thread).filter((request) => request.body["resume"] !== undefined);
    expect(approvals.map((request) => request.body["runId"])).toEqual(["run_approve_retry", "run_approve_retry"]);
    expect(approvals.map((request) => request.headers[SECRET_HEADER])).toEqual([CONFIRMATION_SECRET, CONFIRMATION_SECRET]);
    expect(typesOf(observed.events).filter((type) => type === "TOOL_CALL_RESULT")).toHaveLength(1);
  });

  it("the secret is nowhere a page can read: no header, no body, no event of the whole conversation", async () => {
    const { agent, thread } = await prepared();
    gate.gateway.queue({ golden: "resume_approve" });
    await answer(agent, true, "run_approve");
    // The control: the gateway really did carry the secret, in a response head and in a request header.
    expect(gatewayRuns(thread)[1]?.headers[SECRET_HEADER]).toBe(CONFIRMATION_SECRET);
    for (const response of gate.traffic) {
      expect(response.headers.toLowerCase()).not.toContain("confirmation");
      expect(response.headers).not.toContain(CONFIRMATION_SECRET);
      expect(await response.body).not.toContain(CONFIRMATION_SECRET);
    }
    expect(JSON.stringify([agent.messages, agent.state, agent.pendingInterrupts])).not.toContain(CONFIRMATION_SECRET);
  });
});

describe(row("frontend-actions"), () => {
  const declared = (): { threadId: string; tools: unknown } => {
    const request = gate.gateway.requests.at(-1)?.body;
    return { threadId: request?.["threadId"], tools: request?.["tools"] };
  };

  it("a UI tool the page declared and the host registered comes back as one tool call with checked arguments, and nothing is prepared", async () => {
    const agent = gate.browser();
    gate.gateway.queue({ golden: "ui_tool_call" });
    const observed = await run(agent, "ui_tool_call");
    expect(observed.error).toBeUndefined();
    expect(declared().tools).toEqual([expect.objectContaining({ name: "highlightRow" })]);

    const calls = observed.events.filter((event) => event.type.startsWith("TOOL_CALL"));
    expect(calls.map((event) => event.type)).toEqual(["TOOL_CALL_START", "TOOL_CALL_ARGS", "TOOL_CALL_END"]);
    expect(calls[0]).toMatchObject({ toolCallName: "highlightRow", toolCallId: expect.stringMatching(/^alixai_ui_/) });
    expect(JSON.parse((calls[1] as unknown as { delta: string }).delta)).toEqual({ rowId: "item-1", color: "yellow" });
    expect(agent.pendingInterrupts).toEqual([]);
    expect(observed.events.at(-1)).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "success" } });
    expect(typesOf(observed.events)).not.toContain("TOOL_CALL_RESULT");
  });

  it("the page's result goes back as a tool message: a run with no turn that starts, shows the state and finishes", async () => {
    const agent = gate.browser();
    gate.gateway.queue({ golden: "ui_tool_call" }, { golden: "ui_tool_result" });
    await run(agent, "ui_tool_call");
    const callId = agent.messages.flatMap((message) => (message.role === "assistant" ? (message.toolCalls ?? []) : [])).find((call) => call.function.name === "highlightRow")?.id;
    expect(callId).toMatch(/^alixai_ui_/);

    agent.addMessage({ id: "t1", role: "tool", toolCallId: callId as string, content: JSON.stringify({ highlighted: true, rowId: "item-1" }) });
    const observed = await runAs(agent, { runId: "run_ui_result" });
    expect(observed.error).toBeUndefined();
    expect(typesOf(observed.events)).toEqual(["RUN_STARTED", "STATE_DELTA", "RUN_FINISHED"]);
    const sent = gate.gateway.requests.at(-1)?.body?.["messages"] as Array<Record<string, any>>;
    expect(sent.at(-1)).toMatchObject({ role: "tool", toolCallId: callId, content: '{"highlighted":true,"rowId":"item-1"}' });
  });

  it("a UI result is data, never an approval: even {approved: true} for a UI call id carries no secret and leaves the pending approval alone", async () => {
    const { agent, thread } = await prepared();
    // Another tab of the same thread (it has no pending interrupt of its own) sends a UI result that looks like an approval.
    const otherTab = gate.browser(thread);
    otherTab.setMessages([...agent.messages, { id: "t2", role: "tool", toolCallId: "alixai_ui_trn_01J00000000000000000000001", content: '{"approved":true}' }]);
    otherTab.setState(agent.state);
    gate.gateway.queue({ golden: "ui_tool_result" });
    const observed = await runAs(otherTab, { runId: "run_ui_result_as_approval" });
    expect(observed.error).toBeUndefined();
    const sent = gatewayRuns(thread).at(-1);
    expect(sent?.headers[SECRET_HEADER]).toBeUndefined();
    expect(sent?.body["resume"]).toBeUndefined();
    expect(await gate.secrets.get({ threadId: thread, preparationId: PREPARATION })).toBe(CONFIRMATION_SECRET);
  });

  it.each(["ui_tool_unregistered", "ui_tool_undeclared", "ui_tool_capability_named"] as const)(
    "%s: the whole proposal fails with RUN_ERROR validation_failed, no tool call reaches the page and nothing is prepared",
    async (name) => {
      const agent = gate.browser();
      gate.gateway.queue({ golden: name });
      const observed = await run(agent, name);
      expect(observed.events.at(-1)).toMatchObject({ type: "RUN_ERROR", code: "validation_failed" });
      expect(typesOf(observed.events).filter((type) => type.startsWith("TOOL_CALL"))).toEqual([]);
      expect(agent.messages.some((message) => message.role === "assistant" && (message.toolCalls?.length ?? 0) > 0)).toBe(false);
      expect(agent.pendingInterrupts).toEqual([]);
    },
  );
});
