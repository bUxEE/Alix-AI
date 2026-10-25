import { describe, expect, it, vi } from "vitest";
import { AguiHttpError, CONFIRMATION_TOKEN_HEADER, createAguiClient } from "../src/agui/client";
import { applyPatch } from "../src/agui/json-patch";
import { parseBlock, parseConfirmation, parsePendingState } from "../src/agui/parse";
import { applyEvent, createChatState } from "../src/agui/reducer";
import { readSse } from "../src/agui/sse";
import type { AguiEvent, ChatState } from "../src/agui/types";
import { ev, runFinished, runStarted, sseResponse } from "./helpers";

const SECRET = "cfm_ThisIsTheConfirmationSecretOfTheRun0123456789";

const fold = (events: AguiEvent[]): ChatState => {
  const state = createChatState();
  for (const event of events) applyEvent(state, event);
  return state;
};

const confirmation = {
  preparationId: "prep_1",
  capabilityId: "appointments.book",
  summary: "Book Dr. Rossi on Tuesday at 10:30",
  effects: [{ operation: "create", entity_type: "appointment", description: "New appointment", changes: [], external: false }],
  riskLevel: 1,
  expiresAt: "2030-01-01T10:00:00Z",
  confirmation: { mode: "explicit" },
};

describe("JSON patch", () => {
  it("adds, replaces and removes, appends with /-, and never mutates its input", () => {
    const source = { a: { b: [1] }, c: 1 };
    const out = applyPatch(source, [
      { op: "add", path: "/a/b/-", value: 2 },
      { op: "replace", path: "/c", value: 3 },
      { op: "add", path: "/d", value: { e: 1 } },
      { op: "remove", path: "/a/b/0" },
    ]);
    expect(out?.value).toEqual({ a: { b: [2] }, c: 3, d: { e: 1 } });
    expect(source).toEqual({ a: { b: [1] }, c: 1 });
  });

  it("refuses prototype paths, unknown operations and paths that do not exist", () => {
    expect(applyPatch({}, [{ op: "add", path: "/__proto__/polluted", value: 1 }])).toBeUndefined();
    expect(applyPatch({}, [{ op: "add", path: "/constructor/prototype/x", value: 1 }])).toBeUndefined();
    expect(applyPatch({}, [{ op: "move", path: "/a", value: 1 }])).toBeUndefined();
    expect(applyPatch({ a: 1 }, [{ op: "replace", path: "/missing", value: 1 }])).toBeUndefined();
    expect(applyPatch({ a: [] }, [{ op: "add", path: "/a/5", value: 1 }])).toBeUndefined();
    expect(({} as Record<string, unknown>)["polluted"]).toBeUndefined();
  });
});

describe("parsers", () => {
  it("read a confirmation only from a confirmation_required interrupt of the exact shape", () => {
    expect(parseConfirmation({ id: "prep_1", reason: "confirmation_required", metadata: confirmation })?.preparationId).toBe("prep_1");
    expect(parseConfirmation({ id: "prep_1", reason: "other", metadata: confirmation })).toBeUndefined();
    expect(parseConfirmation({ id: "prep_1", reason: "confirmation_required", metadata: { ...confirmation, riskLevel: "high" } })).toBeUndefined();
    expect(parseConfirmation({ id: "prep_1", reason: "confirmation_required" })).toBeUndefined();
  });

  it("reject a block of the wrong type or shape, and the pending state with an unknown kind", () => {
    expect(parseBlock("table", { type: "chart" })).toBeUndefined();
    expect(parseBlock("table", { type: "table", columns: [] })).toBeUndefined();
    expect(parseBlock("table", { type: "table", columns: [], rows: [] })).toBeDefined();
    expect(parsePendingState({ alixai: { pending: { kind: "weird", capabilityId: "x", phase: "p" } } })).toBeUndefined();
    expect(parsePendingState({ alixai: { pending: { kind: "goal", capabilityId: "x", phase: "p" } } })?.kind).toBe("goal");
  });
});

describe("the reducer", () => {
  it("folds a whole run: steps, streamed text, progress patches, citations, a block, state and the confirmation interrupt", () => {
    const state = fold([
      runStarted(),
      ev("STATE_SNAPSHOT", { snapshot: { alixai: {} } }),
      ev("STEP_STARTED", { stepName: "retrieve" }),
      ev("ACTIVITY_SNAPSHOT", { messageId: "act_1", activityType: "alixai.progress", content: { stage: "retrieve", state: "progress" } }),
      ev("ACTIVITY_DELTA", { messageId: "act_1", activityType: "alixai.progress", patch: [{ op: "add", path: "/percent", value: 40 }] }),
      ev("TEXT_MESSAGE_START", { messageId: "msg_1", role: "assistant" }),
      ev("TEXT_MESSAGE_CONTENT", { messageId: "msg_1", delta: "Hello " }),
      ev("TEXT_MESSAGE_CONTENT", { messageId: "msg_1", delta: "world" }),
      ev("TEXT_MESSAGE_END", { messageId: "msg_1" }),
      ev("ACTIVITY_SNAPSHOT", {
        messageId: "act_2",
        activityType: "alixai.citations",
        content: { citations: [{ source_id: "src_1", display_name: "Policy", open_ref: { kind: "source" } }] },
      }),
      ev("ACTIVITY_DELTA", {
        messageId: "act_2",
        activityType: "alixai.citations",
        patch: [{ op: "add", path: "/citations/-", value: { source_id: "src_2", display_name: "Handbook", open_ref: { kind: "source" } } }],
      }),
      ev("ACTIVITY_SNAPSHOT", { messageId: "act_3", activityType: "alixai.block.table", content: { type: "table", columns: [], rows: [] } }),
      ev("STATE_DELTA", { delta: [{ op: "add", path: "/alixai/pending", value: { kind: "confirmation", capabilityId: "appointments.book", phase: "prepared" } }] }),
      ev("CUSTOM", { name: "alixai.suggestions", value: { suggestions: [{ title: "Cancel it", message: "Cancel the appointment" }] } }),
      ev("STEP_FINISHED", { stepName: "retrieve" }),
      runFinished({ type: "interrupt", interrupts: [{ id: "prep_1", reason: "confirmation_required", metadata: confirmation }] }),
    ]);
    expect(state.progress["retrieve"]).toMatchObject({ state: "progress", percent: 40 });
    expect(state.step).toBeUndefined();
    const kinds = state.items.map((item) => item.kind);
    expect(kinds).toEqual(["message", "citations", "block", "confirmation"]);
    expect(state.items[0]).toMatchObject({ text: "Hello world", streaming: false });
    expect(state.items[1]).toMatchObject({ kind: "citations" });
    expect((state.items[1] as { citations: unknown[] }).citations).toHaveLength(2);
    expect(state.items[3]).toMatchObject({ kind: "confirmation", interruptId: "prep_1", status: "pending" });
    expect(state.pending?.capabilityId).toBe("appointments.book");
    expect(state.suggestions).toHaveLength(1);
    expect(state.outcome).toBe("interrupt");
    expect(state.running).toBe(false);
  });

  it("never turns a malformed or unknown activity into content, and ignores unknown event types", () => {
    const state = fold([
      runStarted(),
      ev("ACTIVITY_SNAPSHOT", { messageId: "a", activityType: "alixai.block.chart", content: { type: "chart" } }),
      ev("ACTIVITY_SNAPSHOT", { messageId: "b", activityType: "someone.else", content: { anything: true } }),
      ev("ACTIVITY_DELTA", { messageId: "zzz", activityType: "alixai.citations", patch: [{ op: "add", path: "/x", value: 1 }] }),
      ev("REASONING_START", {}),
      ev("RAW", { event: "x" }),
      runFinished(),
    ]);
    expect(state.items).toEqual([]);
    expect(state.outcome).toBe("success");
  });

  it("ends a run with RUN_ERROR as an error entry and never a success outcome", () => {
    const state = fold([runStarted(), ev("TEXT_MESSAGE_START", { messageId: "m" }), ev("TEXT_MESSAGE_CONTENT", { messageId: "m", delta: "partial" }), ev("RUN_ERROR", { message: "model unavailable", code: "unavailable" })]);
    expect(state.outcome).toBe("error");
    expect(state.error).toEqual({ message: "model unavailable", code: "unavailable" });
    expect(state.items.map((item) => item.kind)).toEqual(["message", "error"]);
    expect(state.items[0]).toMatchObject({ streaming: false });
  });

  it("shows the receipt carried as the result of the confirmation call, and a continue-in-app hand-off", () => {
    const receipt = { receipt_id: "rcpt_1", preparation_id: "prep_1", capability_id: "appointments.book", status: "committed", records: [], facts: [], errors: [], audit_ref: "aud_1", issued_at: "2030-01-01T09:00:00Z" };
    const state = fold([
      runStarted(),
      ev("TOOL_CALL_START", { toolCallId: "alixai_confirm_prep_1", toolCallName: "alixai_confirm_action" }),
      ev("TOOL_CALL_RESULT", { messageId: "m_r", toolCallId: "alixai_confirm_prep_1", content: JSON.stringify(receipt) }),
      ev("TOOL_CALL_START", { toolCallId: "alixai_continue_prep_2", toolCallName: "alixai_continue_in_app" }),
      ev("TOOL_CALL_ARGS", { toolCallId: "alixai_continue_prep_2", delta: '{"summary":"Needs a stronger check"}' }),
      ev("TOOL_CALL_END", { toolCallId: "alixai_continue_prep_2" }),
      runFinished(),
    ]);
    expect(state.items.map((item) => item.kind)).toEqual(["receipt", "continue"]);
    expect(state.items[1]).toMatchObject({ summary: "Needs a stronger check" });
  });

  it("collects a UI tool call of the host's tool with its arguments", () => {
    const state = fold([
      runStarted(),
      ev("TOOL_CALL_START", { toolCallId: "alixai_ui_turn_1", toolCallName: "highlightRow" }),
      ev("TOOL_CALL_ARGS", { toolCallId: "alixai_ui_turn_1", delta: '{"row":' }),
      ev("TOOL_CALL_ARGS", { toolCallId: "alixai_ui_turn_1", delta: "3}" }),
      ev("TOOL_CALL_END", { toolCallId: "alixai_ui_turn_1" }),
      runFinished(),
    ]);
    expect(state.uiCalls).toEqual([{ id: "alixai_ui_turn_1", name: "highlightRow", args: { row: 3 } }]);
  });

  it("replaces the timeline with the history of a connect run", () => {
    const state = fold([
      runStarted(),
      ev("MESSAGES_SNAPSHOT", { messages: [{ id: "m1", role: "user", content: "hi" }, { id: "m2", role: "assistant", content: "hello" }, { id: "m3", role: "tool", toolCallId: "x", content: "{}" }] }),
      runFinished(),
    ]);
    expect(state.items.map((item) => (item.kind === "message" ? item.role : item.kind))).toEqual(["user", "assistant"]);
  });
});

describe("the AG-UI client", () => {
  const input = { threadId: "ses_1", runId: "run_1", messages: [] };
  const noSleep = { sleep: async () => undefined };

  it("posts the run with the host's headers and delivers every event once", async () => {
    const seen: AguiEvent[] = [];
    const calls: Array<{ url: string; init: RequestInit }> = [];
    const client = createAguiClient({
      endpoint: "/v1/agui/",
      headers: ({ operation }) => ({ Authorization: `Bearer token-for-${operation}` }),
      fetch: async (url, init) => {
        calls.push({ url: String(url), init: init ?? {} });
        return sseResponse([
          ["1", runStarted()],
          ["2", ev("TEXT_MESSAGE_START", { messageId: "m" })],
          ["3", runFinished()],
        ]);
      },
    });
    const result = await client.run(input, { onEvent: (event) => seen.push(event) });
    expect(result).toMatchObject({ terminal: true, aborted: false, reconnects: 0 });
    expect(seen.map((event) => event.type)).toEqual(["RUN_STARTED", "TEXT_MESSAGE_START", "RUN_FINISHED"]);
    expect(calls[0]?.url).toBe("/v1/agui");
    expect(calls[0]?.init.method).toBe("POST");
    expect((calls[0]?.init.headers as Record<string, string>)["Authorization"]).toBe("Bearer token-for-agui.run");
    expect(JSON.parse(String(calls[0]?.init.body))).toEqual(input);
  });

  it("reconnects a cut stream from the last id and delivers each event exactly once, asking for a fresh token each time", async () => {
    const seen: string[] = [];
    const tokens: string[] = [];
    const calls: Array<{ url: string; method: string; last: string | undefined }> = [];
    let issued = 0;
    const client = createAguiClient({
      endpoint: "/v1/agui",
      ...noSleep,
      headers: () => {
        issued += 1;
        tokens.push(`t${issued}`);
        return { Authorization: `Bearer t${issued}` };
      },
      fetch: async (url, init) => {
        const headers = (init?.headers ?? {}) as Record<string, string>;
        calls.push({ url: String(url), method: init?.method ?? "GET", last: headers["Last-Event-ID"] });
        if (calls.length === 1) {
          // id 1 closes the first group; the next group (text start, content) is cut before its id arrives.
          return sseResponse(
            [
              ["1", runStarted()],
              [undefined, ev("TEXT_MESSAGE_START", { messageId: "m" })],
              ["2", ev("TEXT_MESSAGE_CONTENT", { messageId: "m", delta: "Hel" })],
              [undefined, ev("TEXT_MESSAGE_CONTENT", { messageId: "m", delta: "lo" })],
            ],
            { cutAfter: 4 },
          );
        }
        // The server restarts at the TurnEvent after cursor 2: only events with seq > 2.
        return sseResponse([
          [undefined, ev("TEXT_MESSAGE_CONTENT", { messageId: "m", delta: "lo" })],
          ["3", ev("TEXT_MESSAGE_END", { messageId: "m" })],
          ["4", runFinished()],
        ]);
      },
    });
    let reconnecting = 0;
    const result = await client.run(input, {
      onEvent: (event) => seen.push(`${event.type}:${String(event["delta"] ?? "")}`),
      onReconnecting: () => {
        reconnecting += 1;
      },
    });
    expect(result).toMatchObject({ terminal: true, reconnects: 1 });
    expect(reconnecting).toBe(1);
    expect(seen).toEqual(["RUN_STARTED:", "TEXT_MESSAGE_START:", "TEXT_MESSAGE_CONTENT:Hel", "TEXT_MESSAGE_CONTENT:lo", "TEXT_MESSAGE_END:", "RUN_FINISHED:"]);
    expect(calls[1]).toEqual({ url: "/v1/agui/threads/ses_1/runs/run_1/events", method: "GET", last: "2" });
    expect(tokens).toEqual(["t1", "t2"]);
  });

  // The gateway writes `id:` before `data:`, so a connection that dies inside a frame has already sent the id of an event that
  // never arrived whole. Such a frame is never dispatched, and a frame whose data is not an event never moves the cursor.
  const raw = (text: string): Response =>
    new Response(
      new ReadableStream<Uint8Array>({
        start: (controller) => {
          controller.enqueue(new TextEncoder().encode(text));
          controller.close();
        },
      }),
      { status: 200, headers: { "Content-Type": "text/event-stream" } },
    );
  const frame = (id: string | undefined, event: AguiEvent): string => `${id === undefined ? "" : `id: ${id}\n`}data: ${JSON.stringify(event)}\n\n`;

  it("resumes a stream cut inside a frame from the last whole event, so the cut event is not lost", async () => {
    const lasts: Array<string | undefined> = [];
    const content = ev("TEXT_MESSAGE_CONTENT", { messageId: "m", delta: "Hel" });
    for (const tail of [`id: 2\ndata: ${JSON.stringify(content).slice(0, 30)}`, "id: 2\ndata: {not an event\n\n"]) {
      const seen: string[] = [];
      const client = createAguiClient({
        endpoint: "/v1/agui",
        ...noSleep,
        fetch: async (_url, init) => {
          const last = ((init?.headers ?? {}) as Record<string, string>)["Last-Event-ID"];
          lasts.push(last);
          if (last === undefined) return raw(frame("1", runStarted()) + frame(undefined, ev("TEXT_MESSAGE_START", { messageId: "m" })) + tail);
          // The server replays every TurnEvent after the cursor.
          return raw(last === "1" ? frame(undefined, ev("TEXT_MESSAGE_START", { messageId: "m" })) + frame("2", content) + frame("3", runFinished()) : frame("3", runFinished()));
        },
      });
      const result = await client.run(input, { onEvent: (event) => seen.push(`${event.type}:${String(event["delta"] ?? "")}`) });
      expect(result).toMatchObject({ terminal: true, reconnects: 1 });
      expect(seen).toEqual(["RUN_STARTED:", "TEXT_MESSAGE_START:", "TEXT_MESSAGE_CONTENT:Hel", "RUN_FINISHED:"]);
    }
    expect(lasts).toEqual([undefined, "1", undefined, "1"]);
  });

  it("does not end the run on a terminal event cut in the middle: the reconnect delivers it whole", async () => {
    const finished = runFinished({ type: "interrupt", interrupts: [{ id: "prep_1", reason: "confirmation_required", metadata: confirmation }] });
    const seen: AguiEvent[] = [];
    const client = createAguiClient({
      endpoint: "/v1/agui",
      ...noSleep,
      fetch: async (_url, init) => {
        const last = ((init?.headers ?? {}) as Record<string, string>)["Last-Event-ID"];
        if (last === undefined) return raw(frame("1", runStarted()) + frame("2", finished).slice(0, 80));
        // A cursor at the end of the run: nothing is left to send.
        return last === "2" ? new Response(null, { status: 204 }) : raw(frame("2", finished));
      },
    });
    const result = await client.run(input, { onEvent: (event) => seen.push(event) });
    expect(result).toMatchObject({ terminal: true, reconnects: 1 });
    expect(seen.map((event) => event.type)).toEqual(["RUN_STARTED", "RUN_FINISHED"]);
    expect(seen[1]).toEqual(finished);
  });

  it("never dispatches the unterminated last frame of a stream", async () => {
    const frames: string[] = [];
    await readSse(raw("data: whole\n\ndata: cut").body as ReadableStream<Uint8Array>, (item) => frames.push(item.data));
    expect(frames).toEqual(["whole"]);
  });

  it("posts again with the same run id when the first connection dies before any id arrived", async () => {
    let attempt = 0;
    const bodies: string[] = [];
    const client = createAguiClient({
      endpoint: "/v1/agui",
      ...noSleep,
      fetch: async (_url, init) => {
        attempt += 1;
        bodies.push(String(init?.body));
        if (attempt === 1) throw new TypeError("network down");
        return sseResponse([["1", runStarted()], ["2", runFinished()]]);
      },
    });
    const result = await client.run(input, { onEvent: () => undefined });
    expect(result.reconnects).toBe(1);
    expect(new Set(bodies).size).toBe(1);
  });

  it("gives up after the allowed reconnects and does not retry a refusal", async () => {
    const failing = createAguiClient({ endpoint: "/v1/agui", ...noSleep, maxReconnects: 2, fetch: async () => Promise.reject(new TypeError("down")) });
    await expect(failing.run(input, { onEvent: () => undefined })).rejects.toBeInstanceOf(TypeError);

    let calls = 0;
    const refused = createAguiClient({
      endpoint: "/v1/agui",
      ...noSleep,
      fetch: async () => {
        calls += 1;
        return new Response(JSON.stringify({ error: { code: "scope_violation", message: "not your thread" } }), { status: 403 });
      },
    });
    await expect(refused.run(input, { onEvent: () => undefined })).rejects.toMatchObject({ status: 403, code: "scope_violation", message: "not your thread" });
    expect(calls).toBe(1);
    expect(new AguiHttpError(503, undefined, "x").retryable).toBe(true);
    expect(new AguiHttpError(403, undefined, "x").retryable).toBe(false);
  });

  it("treats 429 as a refusal for now, waits as long as Retry-After asks (up to 60 seconds) and gives up after the allowed retries", async () => {
    const random = vi.spyOn(Math, "random").mockReturnValue(0);
    try {
      const waits: number[] = [];
      const answers = [
        new Response(JSON.stringify({ error: { code: "rate_limited", message: "slow down" } }), { status: 429, headers: { "Retry-After": "30" } }),
        new Response(null, { status: 503, headers: { "Retry-After": "3600" } }),
        sseResponse([
          ["1", runStarted()],
          ["2", runFinished()],
        ]),
      ];
      const client = createAguiClient({ endpoint: "/v1/agui", sleep: async (ms) => void waits.push(ms), fetch: async () => answers.shift() as Response });
      expect(await client.run(input, { onEvent: () => undefined })).toMatchObject({ terminal: true, reconnects: 2 });
      expect(waits).toEqual([30_000, 60_000]);

      let calls = 0;
      const limited = createAguiClient({
        endpoint: "/v1/agui",
        ...noSleep,
        maxReconnects: 2,
        fetch: async () => {
          calls += 1;
          return new Response(null, { status: 429 });
        },
      });
      await expect(limited.run(input, { onEvent: () => undefined })).rejects.toMatchObject({ status: 429 });
      expect(calls).toBe(3);
      expect(new AguiHttpError(429, undefined, "x").retryable).toBe(true);
    } finally {
      random.mockRestore();
    }
  });

  it("adds a little jitter to the default reconnect delay", async () => {
    const random = vi.spyOn(Math, "random").mockReturnValue(0.99);
    try {
      const waits: number[] = [];
      const answers = [sseResponse([["1", runStarted()]]), sseResponse([["2", runFinished()]])];
      const client = createAguiClient({ endpoint: "/v1/agui", sleep: async (ms) => void waits.push(ms), fetch: async () => answers.shift() as Response });
      expect(await client.run(input, { onEvent: () => undefined })).toMatchObject({ terminal: true, reconnects: 1 });
      expect(waits).toEqual([300 + Math.floor(300 * 0.2 * 0.99)]);
    } finally {
      random.mockRestore();
    }
  });

  it("stops quietly when the caller aborts", async () => {
    const controller = new AbortController();
    const client = createAguiClient({
      endpoint: "/v1/agui",
      fetch: async () => {
        controller.abort();
        throw new DOMException("aborted", "AbortError");
      },
    });
    await expect(client.run(input, { onEvent: () => undefined, signal: controller.signal })).resolves.toMatchObject({ aborted: true });
  });

  it("never reads, keeps or sends the confirmation secret: it is in no event, no state, no request header", async () => {
    const requestHeaders: Array<Record<string, string>> = [];
    const seen: AguiEvent[] = [];
    const client = createAguiClient({
      endpoint: "/v1/agui",
      // A host callback that returns the secret header has it dropped before the request.
      headers: () => ({ Authorization: "Bearer t", "X-AlixAI-Confirmation-Token": SECRET }),
      fetch: async (_url, init) => {
        requestHeaders.push((init?.headers ?? {}) as Record<string, string>);
        return sseResponse(
          [
            ["1", runStarted()],
            ["2", runFinished({ type: "interrupt", interrupts: [{ id: "prep_1", reason: "confirmation_required", metadata: confirmation }] })],
          ],
          { headers: { [CONFIRMATION_TOKEN_HEADER]: SECRET } },
        );
      },
    });
    const state = createChatState();
    await client.run(input, {
      onEvent: (event) => {
        seen.push(event);
        applyEvent(state, event);
      },
    });
    expect(JSON.stringify(requestHeaders).toLowerCase()).not.toContain("confirmation-token");
    expect(JSON.stringify(requestHeaders)).not.toContain(SECRET);
    expect(JSON.stringify(seen)).not.toContain(SECRET);
    expect(JSON.stringify(state)).not.toContain(SECRET);
  });

  it("cancels a run on the route of the run and never throws", async () => {
    const urls: string[] = [];
    const client = createAguiClient({
      endpoint: "/v1/agui",
      fetch: async (url) => {
        urls.push(String(url));
        return new Response(null, { status: 204 });
      },
    });
    expect(await client.cancel("ses_1", "run 1")).toBe(true);
    expect(urls).toEqual(["/v1/agui/threads/ses_1/runs/run%201/cancel"]);
    const broken = createAguiClient({ endpoint: "/v1/agui", fetch: async () => Promise.reject(new TypeError("x")) });
    expect(await broken.cancel("a", "b")).toBe(false);
  });
});
