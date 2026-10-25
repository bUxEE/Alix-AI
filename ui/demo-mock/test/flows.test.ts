import { HttpAgent, type BaseEvent } from "@ag-ui/client";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { createAguiClient } from "../../vue/src/agui/client";
import { readSse } from "../../vue/src/agui/sse";
import type { ChatItem } from "../../vue/src/agui/types";
import { useAlixChat } from "../../vue/src/composables/useAlixChat";
import { DEMO_THREAD_ID, demoPrompt } from "../src/index";
import { expectSchema, kindsOf, runVue, SCHEMA, startMock, uid, userMessage, type RunningMock } from "./helpers";

// The multi-run flows: a confirmation approved, declined and answered twice; a clarification answered by a choice and by typing; the
// connect run that replays a thread; the cut stream the client resumes from its cursor; the same run id replayed; cancel.

let mock: RunningMock;
beforeAll(async () => {
  mock = await startMock({ pace: 0 });
});
afterAll(async () => {
  await mock.close();
});

const chatFor = (language = "en") =>
  useAlixChat({ client: createAguiClient({ endpoint: `${mock.base}/v1/agui`, sleep: async () => undefined }), threadId: uid("ses_flow"), language });

const find = <K extends ChatItem["kind"]>(items: readonly ChatItem[], kind: K): Extract<ChatItem, { kind: K }> | undefined =>
  items.find((item): item is Extract<ChatItem, { kind: K }> => item.kind === kind);

const lastText = (items: readonly ChatItem[]): string => {
  const messages = items.filter((item): item is Extract<ChatItem, { kind: "message" }> => item.kind === "message");
  return messages[messages.length - 1]?.text ?? "";
};

const post = (body: unknown): Promise<Response> =>
  fetch(`${mock.base}/v1/agui`, { method: "POST", headers: { "Content-Type": "application/json", Accept: "text/event-stream" }, body: JSON.stringify(body) });

describe("an action that needs confirmation", () => {
  it("prepares the action, and the approval commits it with a receipt", async () => {
    const chat = chatFor();
    expect(await chat.send(demoPrompt("confirmation").prompt.en)).toBe(true);
    const sheet = find(chat.state.items, "confirmation");
    expect(sheet?.status).toBe("pending");
    expect(sheet?.confirmation.riskLevel).toBe(2);
    expect(sheet?.confirmation.mode).toBe("explicit");
    expect(chat.state.pending?.kind).toBe("confirmation");
    expect(await chat.approve(sheet!.id)).toBe(true);
    expect(find(chat.state.items, "confirmation")?.status).toBe("approved");
    const receipt = find(chat.state.items, "receipt");
    expect(receipt?.receipt.status).toBe("committed");
    expect(receipt?.receipt.preparation_id).toBe(sheet!.confirmation.preparationId);
    expect(kindsOf(chat.state.items).slice(-2)).toEqual(["block:action_card", "message"]);
    expect(lastText(chat.state.items)).toMatch(/^Done\. Work order \*\*WO-\d+\*\*/);
    expect(chat.state.pending?.kind).toBe("goal");
  });

  it("declines without committing anything", async () => {
    const chat = chatFor("it");
    await chat.send(demoPrompt("confirmation").prompt.it);
    const sheet = find(chat.state.items, "confirmation");
    expect(await chat.decline(sheet!.id)).toBe(true);
    expect(find(chat.state.items, "confirmation")?.status).toBe("declined");
    expect(find(chat.state.items, "receipt")).toBeUndefined();
    expect(chat.state.outcome).toBe("cancelled");
    expect(lastText(chat.state.items)).toContain("Non è stato modificato nulla");
    expect(chat.state.pending).toBeUndefined();
  });

  it("answers a second answer to the same confirmation with 409, and an unknown one with 403", async () => {
    const threadId = uid("ses_twice");
    const first = await runVue(mock.base, { threadId, messages: [userMessage(demoPrompt("confirmation").prompt.en)] });
    const interruptId = first.state.interrupts[0]!.id;
    const resume = [{ interruptId, status: "resolved", payload: { approved: true } }];
    const approved = await runVue(mock.base, { threadId, resume: resume as never });
    expect(approved.state.outcome).toBe("success");
    const again = await post({ threadId, runId: uid("run"), messages: [], resume });
    expect(again.status).toBe(409);
    expectSchema(SCHEMA.error, await again.json());
    const unknown = await post({ threadId, runId: uid("run"), messages: [], resume: [{ ...resume[0], interruptId: "prep_01JNWDEM0F1XTVRE0000000099" }] });
    expect(unknown.status).toBe(403);
    expect(((await unknown.json()) as { code: string }).code).toBe("scope_violation");
    const notExactly = await post({ threadId, runId: uid("run"), messages: [], resume: [{ interruptId, status: "resolved", payload: { approved: "yes" } }] });
    expect(notExactly.status).toBe(400);
  });

  it("runs the same way through HttpAgent, resumed with an AG-UI resume entry", async () => {
    const agent = new HttpAgent({ url: `${mock.base}/v1/agui`, threadId: uid("ses_react") });
    agent.addMessage({ id: uid("msg"), role: "user", content: demoPrompt("confirmation").prompt.en });
    const first: BaseEvent[] = [];
    await agent.runAgent({}, { onEvent: ({ event }) => void first.push(event) });
    const finished = first[first.length - 1] as unknown as { outcome: { type: string; interrupts: { id: string }[] } };
    expect(finished.outcome.type).toBe("interrupt");
    const second: BaseEvent[] = [];
    await agent.runAgent(
      { resume: [{ interruptId: finished.outcome.interrupts[0]!.id, status: "resolved", payload: { approved: true } }] },
      { onEvent: ({ event }) => void second.push(event) },
    );
    expect(second.map((event) => event.type)).toContain("TOOL_CALL_RESULT");
    expect(second[second.length - 1]?.type).toBe("RUN_FINISHED");
  });

  it("hands a risk level 3 action to the application", async () => {
    const run = await runVue(mock.base, { threadId: uid("ses_step"), messages: [userMessage(demoPrompt("step_up").prompt.en)] });
    const handover = find(run.state.items, "continue");
    expect(handover?.summary).toContain("Brightline");
    expect(handover?.preparationId).toMatch(/^prep_/);
    expect(run.state.interrupts).toEqual([]);
  });
});

describe("a clarification", () => {
  it("is answered with a choice, which prepares the booking", async () => {
    const chat = chatFor();
    await chat.send(demoPrompt("clarification").prompt.en);
    const question = chat.state.items.find((item) => item.kind === "block" && item.blockType === "clarification_choices");
    expect(chat.state.pending?.question?.choices).toHaveLength(3);
    expect(await chat.choose(question!.id, "slot_2")).toBe(true);
    const sheet = find(chat.state.items, "confirmation");
    expect(sheet?.confirmation.capabilityId).toBe("inspections.book");
    await chat.approve(sheet!.id);
    expect(lastText(chat.state.items)).toMatch(/Inspection \*\*INS-\d+\*\* is booked/);
  });

  it("is answered by typing", async () => {
    const chat = chatFor();
    await chat.send(demoPrompt("clarification").prompt.en);
    await chat.send("the first one please");
    expect(find(chat.state.items, "confirmation")?.confirmation.capabilityId).toBe("inspections.book");
  });

  it("refuses a choice when nothing was asked", async () => {
    const run = await runVue(mock.base, { threadId: uid("ses_nochoice"), forwardedProps: { alixai: { choiceId: "slot_1" } } });
    expect(run.state.outcome).toBe("error");
    expect(run.state.error?.code).toBe("validation_failed");
  });
});

describe("history, reconnect, replay and cancel", () => {
  it("replays a thread's history on a connect run", async () => {
    const run = await runVue(mock.base, { threadId: DEMO_THREAD_ID });
    expect(run.events.map((event) => event.type)).toEqual(["RUN_STARTED", "MESSAGES_SNAPSHOT", "STATE_SNAPSHOT", "RUN_FINISHED"]);
    expect(kindsOf(run.state.items)).toEqual(["message", "message"]);
    const threadId = uid("ses_hist");
    await runVue(mock.base, { threadId, messages: [userMessage(demoPrompt("table").prompt.en)] });
    const again = await runVue(mock.base, { threadId });
    expect(again.state.items.map((item) => (item as { role: string }).role)).toEqual(["user", "assistant"]);
  });

  it("cuts the reconnect scenario once; the client resumes from its cursor and nothing is shown twice", async () => {
    const run = await runVue(mock.base, { threadId: uid("ses_cut"), messages: [userMessage(demoPrompt("reconnect").prompt.en)] });
    expect(run.reconnects).toBe(1);
    expect(run.result.terminal).toBe(true);
    const starts = run.events.filter((event) => event.type === "TEXT_MESSAGE_START").map((event) => event["messageId"]);
    expect(new Set(starts).size).toBe(starts.length);
    expect(kindsOf(run.state.items)).toEqual(["message", "block:table", "message"]);
    const [first, , second] = run.state.items as Extract<ChatItem, { kind: "message" }>[];
    expect(first?.text).toMatch(/^This answer is cut halfway on purpose: .* Here are the open work orders while we're at it:$/);
    expect(second?.text).toBe("If you saw a short *Reconnecting* notice, that was the cut. The table above arrived after it.");
  });

  it("replays a recorded run for the same run id, and reads it from a cursor", async () => {
    const body = { threadId: uid("ses_same"), runId: uid("run"), messages: [userMessage(demoPrompt("chart_bar").prompt.en)] };
    const once = await (await post(body)).text();
    const twice = await (await post(body)).text();
    expect(twice).toBe(once);
    const frames: { id: string | undefined; data: string }[] = [];
    const response = await fetch(`${mock.base}/v1/agui/threads/${body.threadId}/runs/${body.runId}/events`, { headers: { "Last-Event-ID": "3" } });
    await readSse(response.body!, (frame) => void frames.push(frame));
    const all: { id: string | undefined }[] = [];
    await readSse(new Response(once).body!, (frame) => void all.push(frame));
    expect(frames.length).toBe(all.length - (all.findIndex((frame) => frame.id === "3") + 1));
    const last = all[all.length - 1]!.id!;
    expect((await fetch(`${mock.base}/v1/agui/threads/${body.threadId}/runs/${body.runId}/events?cursor=${last}`)).status).toBe(204);
    expect((await fetch(`${mock.base}/v1/agui/threads/${body.threadId}/runs/${body.runId}/events?cursor=x`)).status).toBe(400);
    expect((await fetch(`${mock.base}/v1/agui/threads/${body.threadId}/runs/nope/events`)).status).toBe(403);
  });

  it("cancels a run in flight: what was open is closed and the run ends cancelled", async () => {
    const slow = await startMock({ pace: 1 });
    try {
      const threadId = uid("ses_cancel");
      const runId = uid("run");
      const controller = new AbortController();
      const client = createAguiClient({ endpoint: `${slow.base}/v1/agui` });
      let cursor: string | undefined;
      const running = client.run(
        { threadId, runId, messages: [userMessage(demoPrompt("long").prompt.en)] },
        {
          signal: controller.signal,
          onEvent: (event, at) => {
            cursor = at;
            if (event.type === "TEXT_MESSAGE_CONTENT") controller.abort();
          },
        },
      );
      expect((await running).aborted).toBe(true);
      const cancelled = await fetch(`${slow.base}/v1/agui/threads/${threadId}/runs/${runId}/cancel`, { method: "POST" });
      expect(await cancelled.json()).toEqual({ thread_id: threadId, run_id: runId, status: "cancelling" });
      const rest: { type: string }[] = [];
      const response = await fetch(`${slow.base}/v1/agui/threads/${threadId}/runs/${runId}/events`, { headers: cursor === undefined ? {} : { "Last-Event-ID": cursor } });
      await readSse(response.body!, (frame) => void rest.push(JSON.parse(frame.data) as { type: string }));
      expect(rest.map((event) => event.type)).toContain("TEXT_MESSAGE_END");
      expect(rest[rest.length - 1]).toMatchObject({ type: "RUN_FINISHED", outcome: { type: "cancelled" } });
      const again = await fetch(`${slow.base}/v1/agui/threads/${threadId}/runs/${runId}/cancel`, { method: "POST" });
      expect(((await again.json()) as { status: string }).status).toBe("finished");
    } finally {
      await slow.close();
    }
  });

  it("refuses a malformed run input with the typed error", async () => {
    const response = await post({ threadId: "bad thread id", runId: "r1", messages: [] });
    expect(response.status).toBe(400);
    expectSchema(SCHEMA.error, await response.json());
    const empty = await post({ threadId: uid("ses"), runId: uid("run"), messages: [{ id: "m", role: "user", content: "   " }] });
    expect(empty.status).toBe(400);
  });
});
