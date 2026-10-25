import { describe, expect, it } from "vitest";
import { effectScope } from "vue";
import { AguiHttpError, type AguiClient, type RunHandlers } from "../src/agui/client";
import type { AguiEvent, RunAgentInput } from "../src/agui/types";
import { useAlixChat } from "../src/composables/useAlixChat";
import { useAttachmentQueue } from "../src/composables/useAttachmentQueue";
import { useLoadState } from "../src/composables/useLoadState";
import { ev, flush, runFinished, runStarted, scriptedClient } from "./helpers";

/** A client whose runs stay open until the caller aborts them. `start` plays the first events of each run. */
const openClient = (start?: (handlers: RunHandlers) => void): { client: AguiClient; runs: Array<{ input: RunAgentInput; handlers: RunHandlers; signal: AbortSignal }> } => {
  const runs: Array<{ input: RunAgentInput; handlers: RunHandlers; signal: AbortSignal }> = [];
  const client: AguiClient = {
    run: (input, handlers) =>
      new Promise((resolve) => {
        const signal = handlers.signal as AbortSignal;
        runs.push({ input, handlers, signal });
        signal.addEventListener("abort", () => resolve({ terminal: false, aborted: true, reconnects: 0 }), { once: true });
        start?.(handlers);
      }),
    cancel: async () => true,
    capabilities: async () => ({}),
  };
  return { client, runs };
};

const confirmation = {
  preparationId: "prep_1",
  capabilityId: "appointments.book",
  summary: "Book Dr. Rossi",
  effects: [],
  riskLevel: 1,
  expiresAt: "2030-01-01T10:00:00Z",
  confirmation: { mode: "explicit" },
};

const interrupt = (mode = "explicit"): AguiEvent[] => [
  runStarted(),
  runFinished({ type: "interrupt", interrupts: [{ id: "prep_1", reason: "confirmation_required", metadata: { ...confirmation, confirmation: { mode } } }] }),
];

let ids = 0;
const options = { createId: (prefix: string): string => `${prefix}_${++ids}` };

describe("useAlixChat", () => {
  it("sends a user message as one run on the host's thread, shows it at once, and streams the reply", async () => {
    const client = scriptedClient(() => [runStarted(), ev("TEXT_MESSAGE_START", { messageId: "a1" }), ev("TEXT_MESSAGE_CONTENT", { messageId: "a1", delta: "Hi" }), ev("TEXT_MESSAGE_END", { messageId: "a1" }), runFinished()]);
    const chat = useAlixChat({ client, threadId: "ses_9", language: "it", ...options });
    expect(await chat.send("  hello  ")).toBe(true);
    const input = client.inputs[0] as RunAgentInput;
    expect(input.threadId).toBe("ses_9");
    expect(input.messages).toHaveLength(1);
    expect(input.messages[0]).toMatchObject({ role: "user", content: "hello" });
    expect(input.forwardedProps).toEqual({ alixai: { language: "it" } });
    expect(chat.items.value.map((item) => (item.kind === "message" ? `${item.role}:${item.text}` : item.kind))).toEqual(["user:hello", "assistant:Hi"]);
    expect(chat.running.value).toBe(false);
    expect(await chat.send("   ")).toBe(false);
  });

  it("sends attachments as file references only, at most four, with the text", async () => {
    const client = scriptedClient(() => [runStarted(), runFinished()]);
    const chat = useAlixChat({ client, threadId: "ses_1", ...options });
    const files = [1, 2, 3, 4].map((n) => ({ fileId: `file_${n}`, kind: "image" as const }));
    expect(await chat.send("look", { attachments: [...files, { fileId: "file_5", kind: "document" }] })).toBe(false);
    expect(client.inputs).toHaveLength(0);
    await chat.send("look", { attachments: [{ fileId: "file_1", kind: "image" }, { fileId: "file_2", kind: "document" }] });
    expect((client.inputs[0]?.messages[0] as { content: unknown }).content).toEqual([
      { type: "text", text: "look" },
      { type: "image", source: { type: "file", value: "file_1" } },
      { type: "document", source: { type: "file", value: "file_2" } },
    ]);
    expect(JSON.stringify(client.inputs)).not.toMatch(/"(data|url)"/);
  });

  it("approves with exactly {approved:true}, declines and dismisses with {approved:false}, and marks the sheet once", async () => {
    const client = scriptedClient((input) => (input.resume === undefined ? interrupt() : [runStarted(), runFinished()]));
    const chat = useAlixChat({ client, threadId: "ses_1", ...options });
    await chat.send("book it");
    const sheet = chat.items.value.find((item) => item.kind === "confirmation");
    expect(sheet).toMatchObject({ status: "pending", interruptId: "prep_1" });
    const id = (sheet as { id: string }).id;
    expect(await chat.approve(id)).toBe(true);
    expect(client.inputs[1]?.resume).toEqual([{ interruptId: "prep_1", status: "resolved", payload: { approved: true } }]);
    expect(client.inputs[1]?.messages).toEqual([]);
    expect(chat.items.value.find((item) => item.kind === "confirmation")).toMatchObject({ status: "approved" });
    // Resolved once: a second answer is refused before it reaches the client.
    expect(await chat.approve(id)).toBe(false);
    expect(await chat.decline(id)).toBe(false);
    expect(client.inputs).toHaveLength(2);

    const second = scriptedClient((input) => (input.resume === undefined ? interrupt() : [runStarted(), runFinished({ type: "cancelled" })]));
    const other = useAlixChat({ client: second, threadId: "ses_1", ...options });
    await other.send("book it");
    const otherId = (other.items.value.find((item) => item.kind === "confirmation") as { id: string }).id;
    await other.decline(otherId);
    expect(second.inputs[1]?.resume).toEqual([{ interruptId: "prep_1", status: "resolved", payload: { approved: false } }]);

    const third = scriptedClient((input) => (input.resume === undefined ? interrupt() : [runStarted(), runFinished({ type: "cancelled" })]));
    const closing = useAlixChat({ client: third, threadId: "ses_1", ...options });
    await closing.send("book it");
    await closing.dismiss((closing.items.value.find((item) => item.kind === "confirmation") as { id: string }).id);
    expect(third.inputs[1]?.resume).toEqual([{ interruptId: "prep_1", status: "resolved", payload: { approved: false } }]);
    expect(closing.items.value.find((item) => item.kind === "confirmation")).toMatchObject({ status: "dismissed" });
  });

  it("never answers a step-up confirmation here", async () => {
    const client = scriptedClient(() => interrupt("step_up"));
    const chat = useAlixChat({ client, threadId: "ses_1", ...options });
    await chat.send("do the big thing");
    const id = (chat.items.value.find((item) => item.kind === "confirmation") as { id: string }).id;
    expect(await chat.approve(id)).toBe(false);
    expect(client.inputs).toHaveLength(1);
  });

  it("lets a failed approval be tried again instead of looking approved", async () => {
    let fail = true;
    const client = scriptedClient((input) => {
      if (input.resume === undefined) return interrupt();
      return fail ? new AguiHttpError(409, "conflict", "busy") : [runStarted(), runFinished()];
    });
    const chat = useAlixChat({ client, threadId: "ses_1", ...options });
    await chat.send("book it");
    const id = (chat.items.value.find((item) => item.kind === "confirmation") as { id: string }).id;
    expect(await chat.approve(id)).toBe(false);
    expect(chat.items.value.find((item) => item.kind === "confirmation")).toMatchObject({ status: "failed" });
    fail = false;
    expect(await chat.approve(id)).toBe(true);
    expect(chat.items.value.find((item) => item.kind === "confirmation")).toMatchObject({ status: "approved" });
  });

  it("answers a clarification with the choice id in forwardedProps and no message", async () => {
    const client = scriptedClient((input) =>
      input.forwardedProps?.alixai?.choiceId === undefined
        ? [runStarted(), ev("ACTIVITY_SNAPSHOT", { messageId: "act_q", activityType: "alixai.block.clarification_choices", content: { type: "clarification_choices", field: "doctor", answer_kind: "choice", allow_free_text: false, choices: [{ choice_id: "dr_rossi", label: "Dr. Rossi", value: { kind: "text", value: "rossi" } }] } }), runFinished()]
        : [runStarted(), runFinished()],
    );
    const chat = useAlixChat({ client, threadId: "ses_1", ...options });
    await chat.send("book");
    await chat.choose("act_q", "dr_rossi");
    expect(client.inputs[1]?.messages).toEqual([]);
    expect(client.inputs[1]?.forwardedProps).toEqual({ alixai: { choiceId: "dr_rossi" } });
    expect(chat.items.value.find((item) => item.kind === "block")).toMatchObject({ answered: true });
  });

  it("shows a failed request as an error entry and retries the same question as a new run", async () => {
    let fail = true;
    const client = scriptedClient(() => (fail ? new AguiHttpError(503, "unavailable", "try later") : [runStarted(), runFinished()]));
    const chat = useAlixChat({ client, threadId: "ses_1", ...options });
    expect(await chat.send("hello")).toBe(false);
    expect(chat.items.value.map((item) => item.kind)).toEqual(["message", "error"]);
    fail = false;
    expect(await chat.retry()).toBe(true);
    expect(chat.items.value.map((item) => item.kind)).toEqual(["message"]);
    expect(client.inputs[1]?.runId).not.toBe(client.inputs[0]?.runId);
  });

  it("stops by aborting the request and cancelling the run on the gateway", async () => {
    const client = scriptedClient(() => [runStarted("run_x"), runFinished({ type: "cancelled" }, "run_x")]);
    const chat = useAlixChat({ client, threadId: "ses_1", ...options });
    await chat.send("hello");
    await chat.stop();
    expect(client.cancelled).toEqual([["ses_1", "run_x"]]);
  });

  it("loads the history with a connect run and keeps first load and refetch apart", async () => {
    const client = scriptedClient(() => [runStarted(), ev("MESSAGES_SNAPSHOT", { messages: [{ id: "m1", role: "user", content: "earlier" }] }), runFinished()]);
    const chat = useAlixChat({ client, threadId: "ses_1", ...options });
    expect(chat.load.isFirstLoad.value).toBe(false);
    await chat.connect();
    expect(client.inputs[0]?.messages).toEqual([]);
    expect(chat.items.value).toHaveLength(1);
    expect(chat.load.hasLoadedOnce.value).toBe(true);
  });

  it("runs the host's UI action and sends the result back as a tool message, not as an approval", async () => {
    const client = scriptedClient((input) =>
      input.messages[0]?.role === "tool"
        ? [runStarted(), runFinished()]
        : [runStarted(), ev("TOOL_CALL_START", { toolCallId: "alixai_ui_t1", toolCallName: "highlightRow" }), ev("TOOL_CALL_ARGS", { toolCallId: "alixai_ui_t1", delta: '{"row":2}' }), ev("TOOL_CALL_END", { toolCallId: "alixai_ui_t1" }), runFinished()],
    );
    const calls: unknown[] = [];
    const chat = useAlixChat({ client, threadId: "ses_1", onUiAction: (call) => (calls.push(call), { highlighted: true }), ...options });
    await chat.send("show row 2");
    expect(calls).toEqual([{ id: "alixai_ui_t1", name: "highlightRow", args: { row: 2 } }]);
    expect(client.inputs[1]?.messages[0]).toMatchObject({ role: "tool", toolCallId: "alixai_ui_t1", content: '{"highlighted":true}' });
    expect(client.inputs[1]?.resume).toBeUndefined();
  });

  it("leaves the new thread's run in charge when the run of the previous thread ends after a switch", async () => {
    let thread = "ses_a";
    const { client, runs } = openClient();
    const chat = useAlixChat({ client, threadId: () => thread, ...options });
    const first = chat.connect();
    // What AlixChatPanel's threadId watcher does.
    thread = "ses_b";
    chat.reset();
    const second = chat.connect();
    expect(await first).toBe(false);
    expect(runs[0]?.signal.aborted).toBe(true);
    expect(chat.running.value).toBe(true);
    expect(chat.load.isLoading.value).toBe(true);
    // A late event of the old run never reaches the new thread's timeline, and no second run starts next to B.
    runs[0]?.handlers.onEvent(ev("MESSAGES_SNAPSHOT", { messages: [{ id: "m1", role: "user", content: "thread a" }] }), undefined);
    expect(chat.items.value).toHaveLength(0);
    expect(await chat.send("hello")).toBe(false);
    expect(runs).toHaveLength(2);
    // Stop still reaches B.
    await chat.stop();
    expect(runs[1]?.signal.aborted).toBe(true);
    await second;
    expect(chat.running.value).toBe(false);
    expect(chat.load.isLoading.value).toBe(false);
  });

  it("aborts the run in flight when its scope is disposed, and starts nothing after", async () => {
    const { client, runs } = openClient((handlers) => {
      handlers.onEvent(runStarted(), undefined);
      handlers.onEvent(ev("TOOL_CALL_START", { toolCallId: "alixai_ui_t1", toolCallName: "highlightRow" }), undefined);
      handlers.onEvent(ev("TOOL_CALL_END", { toolCallId: "alixai_ui_t1" }), undefined);
    });
    const scope = effectScope();
    const chat = scope.run(() => useAlixChat({ client, threadId: "ses_1", onUiAction: () => ({ done: true }), ...options })) as ReturnType<typeof useAlixChat>;
    const pending = chat.send("show row 2");
    scope.stop();
    expect(runs[0]?.signal.aborted).toBe(true);
    await pending;
    await flush();
    expect(runs).toHaveLength(1);
  });
});

describe("useLoadState", () => {
  it("separates first load from refetch, and flips hasLoadedOnce on success only", () => {
    const load = useLoadState();
    load.begin();
    expect([load.isFirstLoad.value, load.isRefreshing.value]).toEqual([true, false]);
    load.end(false);
    expect(load.hasLoadedOnce.value).toBe(false);
    load.begin();
    expect([load.isFirstLoad.value, load.isRefreshing.value]).toEqual([true, false]);
    load.end(true);
    load.begin();
    expect([load.isFirstLoad.value, load.isRefreshing.value]).toEqual([false, true]);
    load.end(true);
    expect([load.isFirstLoad.value, load.isRefreshing.value]).toEqual([false, false]);
  });
});

describe("useAttachmentQueue", () => {
  it("uploads through the host, offers only finished files, and aborts a removed upload", async () => {
    const aborted: string[] = [];
    let release: (() => void) | undefined;
    const queue = useAttachmentQueue({
      uploader: (file, signal) =>
        new Promise((resolve, reject) => {
          signal.addEventListener("abort", () => {
            aborted.push(file.name);
            reject(new DOMException("aborted", "AbortError"));
          });
          if (file.name === "a.png") resolve({ fileId: "file_a" });
          else release = () => resolve({ fileId: "file_b" });
        }),
    });
    queue.add([new File(["x"], "a.png", { type: "image/png" }), new File(["y"], "b.pdf", { type: "application/pdf" })]);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(queue.files.value.map((file) => file.status)).toEqual(["ready", "uploading"]);
    expect(queue.outgoing.value).toEqual([{ fileId: "file_a", kind: "image", name: "a.png" }]);
    queue.remove("pending-2");
    expect(aborted).toEqual(["b.pdf"]);
    release?.();
    expect(queue.files.value).toHaveLength(1);
  });

  it("takes at most four files", () => {
    const queue = useAttachmentQueue({ uploader: () => new Promise(() => undefined) });
    queue.add(Array.from({ length: 6 }, (_, n) => new File(["x"], `f${n}.txt`)));
    expect(queue.files.value).toHaveLength(4);
    queue.clear();
    expect(queue.files.value).toHaveLength(0);
  });
});
