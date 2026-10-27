import { describe, expect, it } from "vitest";

import { AlixAIError, blocksOfType, citationLink, createClient, isAlixAIError, turnText, viewTurn } from "../src/index.js";
import type { Citation, TurnEvent } from "../src/index.js";
import { askChoice, prepareAction, reply } from "../src/testing/index.js";
import { collect, createHost } from "./helpers/host.js";

// A conversation over a session: what was said, stop, regenerate, edit-and-resubmit, resume and switching (master plan 15.1). The API has
// no route that rewinds or re-runs a turn, so a regenerate or an edit is a NEW turn, and the guards keep it from doing anything twice.

const BOOK = { capability_id: "demo.book", summary: "Book the 10:00 slot", text: "Shall I book it?" };

const caught = async (promise: Promise<unknown>): Promise<AlixAIError> => {
  try {
    await promise;
  } catch (error) {
    expect(isAlixAIError(error), `not an AlixAIError: ${String(error)}`).toBe(true);
    return error as AlixAIError;
  }
  throw new Error("expected a failure");
};

const requests = (host: ReturnType<typeof createHost>): Array<string | undefined> => host.fake.requests.map((r) => r.operation);

describe("a conversation records what was said", () => {
  it("opens the session and records each turn: input, events, status, the turn id", async () => {
    const host = createHost();
    const conversation = host.client.conversation("ses-1");
    expect((await conversation.open()).status).toBe("active");
    const stream = conversation.send("hello");
    expect(conversation.active?.status).toBe("streaming");
    const events = await collect(stream);
    expect(conversation.active).toBeUndefined();
    const [record] = conversation.turns;
    expect(record).toMatchObject({ index: 0, input: { kind: "text", text: "hello", attachments: [] }, status: "done", supersededBy: undefined });
    expect(record?.events).toEqual(events);
    expect(record?.turnId).toBe(events[0]?.turn_id);
    expect(conversation.view(record!).text).toBe("fake: hello");
    expect(conversation.transcript).toEqual(conversation.turns);
  });

  it("records the confirmation secret of a prepared action beside the turn, for the user's client", async () => {
    const host = createHost();
    host.fake.onTurn(() => prepareAction(BOOK));
    const conversation = host.client.conversation("ses-1");
    await collect(conversation.send("book it"));
    const record = conversation.turns[0]!;
    await Promise.resolve();
    expect(record.confirmation).toMatchObject({ preparation_id: expect.stringMatching(/^prep_/), token: expect.stringMatching(/^cfm_/) });
    expect(conversation.view(record)).toMatchObject({ preparedAction: { capability_id: "demo.book" }, finishReason: "awaiting_confirmation" });
  });

  it("result() records too, and a clarification is answered with a choice", async () => {
    const host = createHost();
    host.fake.onTurn((turn) =>
      "choice_id" in turn.input
        ? reply(`you chose ${turn.input.choice_id}`)
        : askChoice("doctor", "Which doctor?", [{ choice_id: "dr-a", label: "Dr A" }, { choice_id: "dr-b", label: "Dr B" }]),
    );
    const conversation = host.client.conversation("ses-1");
    const first = await conversation.send("book a doctor").result();
    expect(first.events.at(-1)).toMatchObject({ finish_reason: "awaiting_input" });
    const choices = conversation.view(conversation.turns[0]!).clarification?.choices;
    expect(choices?.map((c) => c.choice_id)).toEqual(["dr-a", "dr-b"]);
    await conversation.choose("dr-b").result();
    expect(conversation.turns.map((t) => [t.input.kind, t.status])).toEqual([["text", "done"], ["choice", "done"]]);
    expect(conversation.view(conversation.turns[1]!).text).toBe("you chose dr-b");
  });

  it("does not take a second turn while one is streaming", async () => {
    const host = createHost();
    const conversation = host.client.conversation("ses-1");
    const stream = conversation.send("one");
    expect(() => conversation.send("two")).toThrow(expect.objectContaining({ code: "conflict", details: { reason: "turn_in_flight" } }));
    await stream.result();
    await conversation.send("two").result();
    expect(conversation.turns).toHaveLength(2);
  });

  it("a failed stream is a failed record", async () => {
    const host = createHost();
    host.fake.failNext({ operation: "sendTurn", status: 503, error: { code: "unavailable", message: "down", retryable: true } });
    const conversation = host.client.conversation("ses-1");
    expect((await caught(conversation.send("x").result())).code).toBe("unavailable");
    expect(conversation.turns[0]).toMatchObject({ status: "failed", error: { code: "unavailable" } });
    expect(conversation.active).toBeUndefined();
  });
});

describe("stop and resume", () => {
  it("stop abandons the stream client-side, records it as stopped, and frees the conversation at once", async () => {
    const host = createHost();
    const conversation = host.client.conversation("ses-1");
    const stream = conversation.send("hello");
    const iterator = stream[Symbol.asyncIterator]();
    await iterator.next();
    expect(conversation.stop()).toBe(true);
    expect(conversation.stop()).toBe(false);
    expect(conversation.turns[0]?.status).toBe("stopped");
    await expect(iterator.next()).rejects.toMatchObject({ name: "AbortError" });
    expect(conversation.turns[0]?.status).toBe("stopped");
    expect(conversation.active).toBeUndefined();
    expect(host.fake.session("ses-1")?.turn_count).toBe(1); // the server ran it to the end all the same
  });

  it("resume reads the rest of the stopped turn from the cursor, once, and the turn is then done", async () => {
    const host = createHost();
    const conversation = host.client.conversation("ses-1");
    const stream = conversation.send("hello");
    const iterator = stream[Symbol.asyncIterator]();
    await iterator.next();
    await iterator.next();
    conversation.stop();
    const rest = await collect(conversation.resume());
    const record = conversation.turns[0]!;
    expect(rest[0]?.seq).toBe(2);
    expect(record.events.map((e) => e.seq)).toEqual(record.events.map((_, i) => i));
    expect(record).toMatchObject({ status: "done" });
    expect(conversation.view(record).text).toBe("fake: hello");
    expect(requests(host)).toEqual(["sendTurn", "streamTurnEvents"]); // a read: the turn was not run again
    expect(() => conversation.resume()).toThrow(expect.objectContaining({ details: { reason: "turn_complete" } }));
  });

  it("resume has nothing to read before the first event names the turn", async () => {
    const host = createHost();
    expect(() => host.client.conversation("ses-1").resume()).toThrow(expect.objectContaining({ code: "conflict", details: { reason: "nothing_to_resume" } }));
  });

  it("a conversation can reconnect by itself: streams take the resume policy", async () => {
    const host = createHost();
    host.fake.failNext({ operation: "sendTurn", truncateAfterEvents: 2 });
    const conversation = host.client.conversation("ses-1", { stream: { resume: { backoffMs: 0 } } });
    await collect(conversation.send("hello"));
    expect(conversation.turns[0]).toMatchObject({ status: "done" });
    expect(conversation.turns[0]?.events.at(-1)?.type).toBe("done");
  });
});

describe("regenerate and edit-and-resubmit", () => {
  it("regenerate sends the text again as a NEW turn and folds the old one away", async () => {
    const host = createHost();
    let n = 0;
    host.fake.onTurn((turn) => reply(`answer ${(n += 1)} to ${"text" in turn.input ? turn.input.text : ""}`));
    const conversation = host.client.conversation("ses-1");
    await conversation.send({ text: "what is the warranty?", language: "it" }).result();
    const stream = await conversation.regenerate();
    await stream.result();
    expect(conversation.turns.map((t) => [t.index, t.supersededBy])).toEqual([[0, 1], [1, undefined]]);
    expect(conversation.transcript.map((t) => conversation.view(t).text)).toEqual(["answer 2 to what is the warranty?"]);
    expect(host.fake.requests.filter((r) => r.operation === "sendTurn").map((r) => r.body)).toMatchObject([
      { text: "what is the warranty?", language: "it", client_capabilities: { contract: "1.5" } },
      { text: "what is the warranty?", language: "it", client_capabilities: { contract: "1.5" } },
    ]);
    expect(host.fake.session("ses-1")?.turn_count).toBe(2); // the session is append-only
  });

  it("edit-and-resubmit sends the new text, keeps the language, and replaces only the latest turn", async () => {
    const host = createHost();
    const conversation = host.client.conversation("ses-1");
    await conversation.send("first").result();
    await conversation.send({ text: "secnd", language: "en" }).result();
    await (await conversation.editAndResubmit("second")).result();
    expect(conversation.transcript.map((t) => (t.input.kind === "text" ? t.input.text : ""))).toEqual(["first", "second"]);
    expect(conversation.turns.map((t) => t.supersededBy)).toEqual([undefined, 2, undefined]);
    expect(host.fake.requests.filter((r) => r.operation === "sendTurn").at(-1)?.body).toMatchObject({ text: "second", language: "en", client_capabilities: expect.objectContaining({ contract: "1.5" }) });
  });

  it("is refused after an action was committed: sending its text again could run it again, and nothing is sent", async () => {
    const host = createHost();
    host.fake.onTurn(() => prepareAction({ capability_id: "demo.lookup", summary: "Look it up", risk_level: 0 }));
    const conversation = host.client.conversation("ses-1");
    const result = await conversation.send("do it").result();
    expect(result.events.some((e) => e.type === "receipt")).toBe(true);
    const before = host.fake.requests.length;
    expect(await caught(conversation.regenerate())).toMatchObject({ code: "conflict", details: { reason: "turn_committed" } });
    expect(await caught(conversation.editAndResubmit("do it again"))).toMatchObject({ details: { reason: "turn_committed" } });
    expect(host.fake.requests).toHaveLength(before);
    expect(conversation.turns[0]?.supersededBy).toBeUndefined();
  });

  it("is refused while a prepared action waits for the user, unless the host says discardPending: then the preparation is REJECTED first", async () => {
    const host = createHost();
    host.fake.onTurn((turn) => ("text" in turn.input && turn.input.text === "again" ? reply("fine") : prepareAction(BOOK)));
    const conversation = host.client.conversation("ses-1");
    await conversation.send("book it").result();
    expect(await caught(conversation.regenerate())).toMatchObject({ code: "conflict", details: { reason: "pending_preparation" } });
    expect(host.fake.preparations()[0]?.state).toBe("prepared");
    const stream = await conversation.editAndResubmit("again", { discardPending: true });
    await stream.result();
    expect(host.fake.preparations()[0]?.state).toBe("rejected"); // nothing was ever sent to the application
    expect(conversation.view(conversation.transcript.at(-1)!).text).toBe("fine");
    expect(requests(host)).toEqual(["sendTurn", "getAction", "getAction", "rejectAction", "sendTurn"]);
  });

  it("is refused once the action was committed by a confirmation, even if the turn itself shows no receipt", async () => {
    const host = createHost();
    host.fake.onTurn(() => prepareAction(BOOK));
    const conversation = host.client.conversation("ses-1");
    const result = await conversation.send("book it").result();
    await host.client.confirmAction(result.confirmation!.preparation_id, result.confirmation!.token);
    expect(await caught(conversation.regenerate({ discardPending: true }))).toMatchObject({ details: { reason: "turn_committed" } });
  });

  it("is refused for a turn that was not read to its end: what it did is unknown, so resume it first", async () => {
    const host = createHost();
    const conversation = host.client.conversation("ses-1");
    const iterator = conversation.send("hello")[Symbol.asyncIterator]();
    await iterator.next();
    conversation.stop();
    expect(await caught(conversation.regenerate())).toMatchObject({ details: { reason: "turn_incomplete" } });
    await collect(conversation.resume());
    await expect((await conversation.regenerate()).result()).resolves.toBeDefined();
  });

  it("only a text turn can be sent again, and there must be a turn", async () => {
    const host = createHost();
    host.fake.onTurn((turn) => ("choice_id" in turn.input ? reply("ok") : askChoice("f", "Which?", [{ choice_id: "a", label: "A" }, { choice_id: "b", label: "B" }])));
    const conversation = host.client.conversation("ses-1");
    expect(await caught(conversation.regenerate())).toMatchObject({ details: { reason: "no_turn" } });
    await conversation.send("x").result();
    await conversation.choose("a").result();
    expect(await caught(conversation.regenerate())).toMatchObject({ code: "validation_failed" });
  });

  it("a message with attachments keeps its references on the record", async () => {
    const host = createHost();
    const conversation = host.client.conversation("ses-1");
    const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 1, 2, 3]);
    const stream = conversation.send({ text: "see", attachments: [{ data: png, name: "a.png", mimeType: "image/png" }] });
    await stream.result();
    const refs = await stream.attachments;
    expect(conversation.turns[0]?.input).toMatchObject({ kind: "text", attachments: refs });
    expect(refs).toHaveLength(1);
  });
});

describe("switching conversations", () => {
  it("is opening another conversation: each has its own bound client, its own record, and tells the token provider its session", async () => {
    const host = createHost();
    const tokens: Array<[string, string | undefined]> = [];
    const client = createClient({
      baseUrl: host.fake.baseUrl,
      fetch: host.fake.fetch,
      getContextToken: (request) => {
        tokens.push([request.operation, request.sessionId]);
        return host.fake.issueToken({ session_id: request.sessionId ?? "ses-1", capability_scopes: ["history.read", "history.search", "demo.lookup"] });
      },
    });
    const work = client.conversation("ses-work");
    const home = client.conversation("ses-home");
    await work.open();
    await home.open();
    await work.send("quarterly report").result();
    await home.send("shopping list").result();
    expect(work.turns).toHaveLength(1);
    expect(home.turns).toHaveLength(1);
    expect(work.view(work.turns[0]!).text).toBe("fake: quarterly report");
    expect(home.view(home.turns[0]!).text).toBe("fake: shopping list");
    // Find a conversation by what was said in it (a search whose token names the session it is run from), then switch to it.
    const found = await home.client.history.search({ text: "shopping" });
    expect(found.hits.map((h) => h.session_id)).toEqual(["ses-home", "ses-home"]);
    const page = await client.conversation(found.hits[0]!.session_id).client.history.segments(found.hits[0]!.session_id);
    expect(page.turns.map((t) => t.body)).toEqual(["shopping list", "fake: shopping list"]);
    expect(tokens.filter(([op]) => op === "createSession").map(([, session]) => session)).toEqual(["ses-work", "ses-home"]);
    expect(tokens.every(([, session]) => session !== undefined)).toBe(true);
    expect(await home.end()).toEqual({ session_id: "ses-home", status: "ended" });
    expect(work.client).not.toBe(home.client);
  });

  it("a bound client sends the session it is bound to, and nothing else changes about its requests", async () => {
    const host = createHost();
    const bound = host.client.forSession("ses-1");
    await bound.createSession();
    await bound.getSession("ses-1");
    expect(host.tokenRequests).toEqual([
      { operation: "createSession", sessionId: "ses-1" },
      { operation: "getSession", sessionId: "ses-1" },
    ]);
    expect(() => host.client.forSession("../x")).toThrow(expect.objectContaining({ code: "validation_failed" }));
    expect(() => host.client.conversation("")).toThrow(expect.objectContaining({ code: "validation_failed" }));
  });
});

describe("viewTurn: what a UI renders from", () => {
  it("folds a turn's events into text, blocks, the prepared action and its panel, the directive and how it ended", async () => {
    const host = createHost();
    host.fake.onTurn(() => prepareAction(BOOK));
    const result = await host.client.sendTurn("ses-1", { text: "book it" }).result();
    const view = viewTurn(result.events);
    expect(view).toMatchObject({
      turnId: result.turn_id,
      text: "Shall I book it?",
      preparedAction: { capability_id: "demo.book", state: "prepared" },
      confirmationPanel: { type: "confirmation_panel", preparation_id: result.confirmation?.preparation_id },
      directive: { act: "request_confirmation" },
      finishReason: "awaiting_confirmation",
      errors: [],
    });
    expect(blocksOfType(view.blocks, "confirmation_panel")).toHaveLength(1);
    expect(blocksOfType(view.blocks, "table")).toEqual([]);
    expect(view.progress.map((p) => p.stage)).toEqual(["interpreting", "tool_call"]);
  });

  it("a partial turn is a valid input; an error and a receipt are visible", async () => {
    const host = createHost();
    host.fake.onTurn(() => prepareAction({ capability_id: "demo.lookup", summary: "Look it up", risk_level: 0 }));
    const committed = viewTurn((await host.client.sendTurn("ses-1", { text: "x" }).result()).events);
    expect(committed.receipt).toMatchObject({ status: "committed" });
    expect(committed.finishReason).toBe("completed");
    const events = (await host.client.sendTurn("ses-1", { text: "x" }).result()).events;
    expect(viewTurn(events.slice(0, 2))).toMatchObject({ done: undefined, finishReason: undefined });
    const denied = createHost({ capability_scopes: [] });
    denied.fake.onTurn(() => prepareAction(BOOK));
    const refused = viewTurn((await denied.client.sendTurn("ses-1", { text: "x" }).result()).events);
    expect(refused).toMatchObject({ finishReason: "failed", errors: [{ code: "permission_denied" }], directive: { act: "refuse" } });
  });

  const citation = (over: Partial<Citation>): Citation =>
    ({
      source_id: "src_01J0000000000000000000000A",
      display_name: "Handbook",
      version: "v3",
      availability: "available",
      staleness: "current",
      open_ref: { kind: "file", file_id: "fil_01J0000000000000000000000A" },
      claims: ["The warranty is 24 months."],
      ...over,
    }) as Citation;

  it("turns a citation into source-link data: a label, where in the source, how to open it, and whether it can be trusted", () => {
    expect(citationLink(citation({ locator: { page: 3 } }))).toEqual({
      label: "Handbook",
      sourceId: "src_01J0000000000000000000000A",
      claims: ["The warranty is 24 months."],
      locator: { page: 3 },
      place: "page 3",
      open: { kind: "file", file_id: "fil_01J0000000000000000000000A" },
      available: true,
      stale: false,
      staleness: "current",
    });
    expect(citationLink(citation({ locator: { section: "Warranty" } })).place).toBe("section Warranty");
    expect(citationLink(citation({ locator: { record: "r-12" } })).place).toBe("record r-12");
    expect(citationLink(citation({})).place).toBeUndefined();
    // A source that cannot be opened has nothing to open, and a stale one says so.
    expect(citationLink(citation({ availability: "removed" }))).toMatchObject({ available: false, open: undefined });
    expect(citationLink(citation({ staleness: "stale" }))).toMatchObject({ stale: true });
    expect(citationLink(citation({ staleness: "unknown" }))).toMatchObject({ stale: false, staleness: "unknown" });
  });

  it("collects citations and typed blocks from a stream", () => {
    const event = (seq: number, body: Record<string, unknown>): TurnEvent => ({ turn_id: "trn_x", seq, ...body }) as unknown as TurnEvent;
    const view = viewTurn([
      event(0, { type: "token", text: "See " }),
      event(1, { type: "token", text: "[1]." }),
      event(2, { type: "citation", citation: citation({}) }),
      event(3, { type: "block", block: { type: "table", columns: [], rows: [] } }),
      event(4, { type: "block", block: { type: "code", language: "ts", code: "1" } }),
      event(5, { type: "done", finish_reason: "completed" }),
    ]);
    expect(view.text).toBe("See [1].");
    expect(view.textReplaced).toBe(false);
    expect(view.citations.map((c) => c.display_name)).toEqual(["Handbook"]);
    expect(blocksOfType(view.blocks, "code").map((b) => b.code)).toEqual(["1"]);
    expect(view.blocks.map((b) => b.type)).toEqual(["table", "code"]);
  });

  it("resets text on text_replaced and sets textReplaced", () => {
    const event = (seq: number, body: Record<string, unknown>): TurnEvent => ({ turn_id: "trn_x", seq, ...body }) as unknown as TurnEvent;
    const events = [
      event(0, { type: "token", text: "shown " }),
      event(1, { type: "token", text: "text" }),
      event(2, { type: "text_replaced", reason: "verification_failed" }),
      event(3, { type: "token", text: "template" }),
      event(4, { type: "done", finish_reason: "completed" }),
    ];
    const view = viewTurn(events);
    expect(view).toMatchObject({ text: "template", textReplaced: true, finishReason: "completed" });
    expect(turnText(events)).toBe("template");
  });
});
