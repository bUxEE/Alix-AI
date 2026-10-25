import { afterEach, describe, expect, it } from "vitest";
import { AlixAIAgent } from "../src/agent.js";
import { startRig } from "./helpers/agent-rig.js";
import type { Rig } from "./helpers/agent-rig.js";
import { collect, goldenEvents, inputOf } from "./helpers/agent-rig.js";
import { CONFIRMATION_SECRET, SECRET_HEADER } from "./helpers/fake-gateway.js";
import { golden } from "./helpers/golden.js";
import type { GoldenName } from "./helpers/golden.js";

// The golden streams of M3-25 (tests/agui/testdata) replayed through AlixAIAgent and the real @ag-ui/client: the events
// that come out are the events the gateway recorded, and the request that went in is the request the golden recorded.

let rig: Rig | undefined;
afterEach(async () => {
  await rig?.close();
  rig = undefined;
});

const RUNS_WITH_A_REQUEST: GoldenName[] = [
  "attachments_preview",
  "choice_answer",
  "clarification",
  "client_context",
  "error",
  "forms",
  "plain_answer",
  "prepare_interrupt",
  "read_with_citations",
  "state_delta",
  "state_snapshot",
  "suggestions",
  "ui_tool_call",
  "ui_tool_capability_named",
  "ui_tool_result",
  "ui_tool_undeclared",
  "ui_tool_unregistered",
];

describe("AlixAIAgent replays the golden streams of M3-25", () => {
  it.each(RUNS_WITH_A_REQUEST)("%s: the request equals the golden request and the events equal the golden events", async (name) => {
    rig = await startRig();
    rig.gateway.queue({ golden: name });
    const events = await collect(rig.agent.run(inputOf(name)));
    expect(events).toEqual(goldenEvents(name));
    const sent = rig.gateway.requests[0];
    expect(sent?.method).toBe("POST");
    expect(sent?.path).toBe("/v1/agui");
    // The AG-UI client drops a null state from the outgoing input (omitOptionalNulls); the gateway reads absent and null alike.
    const expected = { ...golden(name).meta.request?.body };
    if (expected["state"] === null) delete expected["state"];
    expect(sent?.body).toEqual(expected);
    expect(sent?.headers["accept"]).toBe("text/event-stream");
    expect(sent?.headers["content-type"]).toBe("application/json");
  });

  it("messages: the plain answer arrives as an assistant message and the closing state through the real client pipeline", async () => {
    rig = await startRig();
    const input = inputOf("plain_answer");
    rig.agent.threadId = input.threadId;
    rig.agent.setMessages(input.messages);
    const result = await rig.agent.runAgent({ runId: input.runId });
    expect(result.result).toMatchObject({ finishReason: "completed" });
    const assistant = rig.agent.messages.filter((message) => message.role === "assistant");
    expect(assistant).toHaveLength(1);
    expect(String(assistant[0]?.content)).toContain("item");
    expect(rig.agent.pendingInterrupts).toEqual([]);
  });

  it("state: a snapshot and a delta are applied to the agent state, and the client's own members survive", async () => {
    rig = await startRig();
    const snapshot = inputOf("state_snapshot");
    rig.gateway.queue({ golden: "state_snapshot" });
    rig.agent.threadId = snapshot.threadId;
    rig.agent.setMessages(snapshot.messages);
    await rig.agent.runAgent({ runId: snapshot.runId });
    expect(rig.agent.state).toHaveProperty("alixai");

    const delta = inputOf("state_delta");
    const other = await startRig();
    other.gateway.queue({ golden: "state_delta" });
    try {
      other.agent.threadId = delta.threadId;
      other.agent.setMessages(delta.messages);
      other.agent.setState(delta.state);
      await other.agent.runAgent({ runId: delta.runId });
      expect(other.agent.state).toMatchObject({ draft: { title: "x" }, page: "inbox" });
      expect(other.agent.state).toHaveProperty("alixai");
    } finally {
      await other.close();
    }
  });

  it("errors: a refused request ends with RUN_ERROR, the explaining text is shown first, and the client reports the failure", async () => {
    rig = await startRig();
    const input = inputOf("error");
    rig.agent.threadId = input.threadId;
    rig.agent.setMessages(input.messages);
    const failures: string[] = [];
    rig.agent.subscribe({ onRunErrorEvent: ({ event }) => void failures.push(event.message) });
    await rig.agent.runAgent({ runId: input.runId });
    expect(failures).toHaveLength(1);
    expect(failures[0]).toMatch(/not able to do that/i);
    expect(rig.agent.messages.some((message) => message.role === "assistant" && String(message.content).includes("isn't something"))).toBe(true);
  });

  it("suggestions and attachments: the CUSTOM and ACTIVITY events reach the client unchanged", async () => {
    rig = await startRig();
    const suggestions = await collect(rig.agent.run(inputOf("suggestions")));
    expect(suggestions.find((event) => (event as { name?: string }).name === "alixai.suggestions")).toMatchObject({ type: "CUSTOM" });
    const other = await startRig();
    try {
      other.gateway.queue({ golden: "attachments_preview" });
      const attachments = await collect(other.agent.run(inputOf("attachments_preview")));
      expect(attachments.some((event) => (event as { activityType?: string }).activityType === "alixai.attachments")).toBe(true);
    } finally {
      await other.close();
    }
  });
});

describe("interrupt, approve and reject", () => {
  const prepare = async (r: Rig): Promise<void> => {
    await collect(r.agent.run(inputOf("prepare_interrupt")));
  };

  it("keeps the secret server-side per thread and preparation, attaches it to the approval and deletes it on use", async () => {
    rig = await startRig();
    await prepare(rig);
    const key = { threadId: "ses_test_01", preparationId: "prep_01J00000000000000000000001" };
    expect(await rig.secrets.get(key)).toBe(CONFIRMATION_SECRET);

    const events = await collect(rig.agent.run(inputOf("resume_approve")));
    expect(events).toEqual(goldenEvents("resume_approve"));
    const approval = rig.gateway.requests[1];
    expect(approval?.headers[SECRET_HEADER]).toBe(CONFIRMATION_SECRET);
    expect(approval?.body).toEqual(golden("resume_approve").meta.request?.body);
    expect(await rig.secrets.get(key)).toBeUndefined();
    // The resume asked the host for a token for the same session, as a resume.
    expect(rig.tokenRequests[1]).toMatchObject({ operation: "agui.run", sessionId: "ses_test_01", runId: "run_approve", resume: true });
  });

  it("reject: no secret is sent, the run ends with the cancelled outcome, and the secret is dropped", async () => {
    rig = await startRig();
    await prepare(rig);
    const events = await collect(rig.agent.run(inputOf("resume_reject")));
    expect(events).toEqual(goldenEvents("resume_reject"));
    expect(events[events.length - 1]).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "cancelled" } });
    expect(rig.gateway.requests[1]?.headers[SECRET_HEADER]).toBeUndefined();
    expect(await rig.secrets.get({ threadId: "ses_test_01", preparationId: "prep_01J00000000000000000000001" })).toBeUndefined();
  });

  it("a dismissed interrupt (status cancelled) is a decline: no secret is sent", async () => {
    rig = await startRig();
    await prepare(rig);
    const input = inputOf("resume_reject");
    await collect(rig.agent.run({ ...input, resume: [{ interruptId: "prep_01J00000000000000000000001", status: "cancelled" }] }));
    expect(rig.gateway.requests[1]?.headers[SECRET_HEADER]).toBeUndefined();
  });

  it("approves through a tool message for alixai_confirm_<id> (the renderAndWaitForResponse form) with the same secret rule", async () => {
    rig = await startRig();
    await prepare(rig);
    const input = inputOf("resume_approve");
    rig.gateway.queue({ golden: "resume_approve" });
    const events = await collect(
      rig.agent.run({
        ...input,
        resume: undefined,
        messages: [{ id: "tm1", role: "tool", toolCallId: "alixai_confirm_prep_01J00000000000000000000001", content: '{"approved":true}' }],
      }),
    );
    expect(events[events.length - 1]).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "success" } });
    expect(rig.gateway.requests[1]?.headers[SECRET_HEADER]).toBe(CONFIRMATION_SECRET);
  });

  it("an approval without a held secret fails before a token is spent or a request is made", async () => {
    rig = await startRig();
    const before = rig.tokenRequests.length;
    await expect(collect(rig.agent.run(inputOf("resume_approve")))).rejects.toMatchObject({ code: "confirmation_required" });
    expect(rig.tokenRequests.length).toBe(before);
    expect(rig.gateway.requests).toHaveLength(0);
  });

  it("a secret is bound to its thread: another thread cannot use it", async () => {
    rig = await startRig();
    await prepare(rig);
    const other = { ...inputOf("resume_approve"), threadId: "ses_other_02" };
    await expect(collect(rig.agent.run(other))).rejects.toMatchObject({ code: "confirmation_required" });
    expect(rig.gateway.requests).toHaveLength(1);
  });

  it("the run clones share one secret store: prepare on one clone, approve on another (what CopilotRuntime does per request)", async () => {
    rig = await startRig();
    await collect(rig.agent.clone().run(inputOf("prepare_interrupt")));
    const events = await collect(rig.agent.clone().run(inputOf("resume_approve")));
    expect(events).toEqual(goldenEvents("resume_approve"));
    expect(rig.gateway.requests[1]?.headers[SECRET_HEADER]).toBe(CONFIRMATION_SECRET);
  });

  it("stale: the commit is refused, RUN_ERROR ends the run, and nothing is left to approve again", async () => {
    rig = await startRig();
    await prepare(rig);
    rig.gateway.queue({ golden: "stale" });
    const events = await collect(rig.agent.run(inputOf("stale")));
    expect(events[events.length - 1]).toMatchObject({ type: "RUN_ERROR" });
    await expect(collect(rig.agent.run({ ...inputOf("stale"), runId: "run_again" }))).rejects.toMatchObject({ code: "confirmation_required" });
  });

  it("a second approval the gateway refuses with 409 surfaces as a typed conflict and uses the secret up", async () => {
    rig = await startRig();
    await prepare(rig);
    rig.gateway.queue({ golden: "resume_approve", fail: { status: 409, code: "conflict", message: "the preparation was already decided" } });
    await expect(collect(rig.agent.run(inputOf("resume_approve")))).rejects.toMatchObject({ code: "conflict", status: 409 });
    expect(await rig.secrets.get({ threadId: "ses_test_01", preparationId: "prep_01J00000000000000000000001" })).toBeUndefined();
  });

  it("keeps the secret on a 403: a request that does not resolve for its caller must not burn the owner's pending approval", async () => {
    rig = await startRig();
    await prepare(rig);
    rig.gateway.queue({ golden: "resume_approve", fail: { status: 403, code: "scope_violation", message: "the thread or the interrupt does not resolve for this caller" } });
    await expect(collect(rig.agent.run(inputOf("resume_approve")))).rejects.toMatchObject({ code: "scope_violation", status: 403 });
    expect(await rig.secrets.get({ threadId: "ses_test_01", preparationId: "prep_01J00000000000000000000001" })).toBe(CONFIRMATION_SECRET);
    const events = await collect(rig.agent.run({ ...inputOf("resume_approve"), runId: "run_approve_again" }));
    expect(events[events.length - 1]).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "success" } });
  });

  it("keeps the secret when the gateway says 428 or 401 (nothing was decided), so the user can try again with a new token", async () => {
    rig = await startRig();
    await prepare(rig);
    rig.gateway.queue({ golden: "resume_approve", fail: { status: 428, code: "confirmation_required" } });
    await expect(collect(rig.agent.run(inputOf("resume_approve")))).rejects.toMatchObject({ code: "confirmation_required", status: 428 });
    expect(await rig.secrets.get({ threadId: "ses_test_01", preparationId: "prep_01J00000000000000000000001" })).toBe(CONFIRMATION_SECRET);
    const events = await collect(rig.agent.run({ ...inputOf("resume_approve"), runId: "run_approve_2" }));
    expect(events[events.length - 1]).toMatchObject({ type: "RUN_FINISHED" });
  });
});

describe("AlixAIAgent type", () => {
  it("is an AG-UI HttpAgent, so CopilotRuntime and every AG-UI client treat it like one", async () => {
    const { HttpAgent } = await import("@ag-ui/client");
    rig = await startRig();
    expect(rig.agent).toBeInstanceOf(HttpAgent);
    expect(rig.agent.clone()).toBeInstanceOf(AlixAIAgent);
  });
});
