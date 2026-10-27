import { describe, expect, it } from "vitest";

import { AlixAIError, doneEvent, isAlixAIError, turnText } from "../src/index.js";
import type { TurnEvent } from "../src/index.js";
import { askChoice, prepareAction, reply } from "../src/testing/index.js";
import { collect, createHost } from "./helpers/host.js";
import { done, frame, json, sse, stubbed, token, watchedSse } from "./helpers/stub.js";

const BOOK = { capability_id: "demo.book", summary: "Book the 10:00 slot", text: "Shall I book the 10:00 slot?" };
const CONFIRMATION_TOKEN = /^cfm_[A-Za-z0-9_-]{43}$/;

const caught = async (promise: Promise<unknown>): Promise<AlixAIError> => {
  try {
    await promise;
  } catch (error) {
    expect(isAlixAIError(error), `not an AlixAIError: ${String(error)}`).toBe(true);
    return error as AlixAIError;
  }
  throw new Error("expected a failure");
};

describe("sendTurn streams typed events", () => {
  it("yields the events in order, numbered from 0, ending with done", async () => {
    const { client } = createHost();
    const events = await collect(client.sendTurn("ses-1", { text: "hello" }));
    expect(events.map((e) => e.seq)).toEqual(events.map((_, i) => i));
    expect(events.at(-1)).toMatchObject({ type: "done", finish_reason: "completed" });
    expect(new Set(events.map((e) => e.turn_id)).size).toBe(1);
    expect(turnText(events)).toBe("fake: hello");
  });

  it("is an async iterable that narrows by event type", async () => {
    const { client } = createHost();
    let text = "";
    for await (const event of client.sendTurn("ses-1", { text: "narrow" })) {
      if (event.type === "token") text += event.text; // `text` exists only on a token event
    }
    expect(text).toBe("fake: narrow");
  });

  it("sends the request when sendTurn is called, before anything is iterated", async () => {
    const { client, fake } = createHost();
    const stream = client.sendTurn("ses-1", { text: "eager" });
    await stream.result();
    expect(fake.requests.filter((r) => r.operation === "sendTurn")).toHaveLength(1);
  });

  it("asks for text/event-stream and sends exactly the input", async () => {
    const { client, fake } = createHost();
    await client.sendTurn("ses-1", { text: "hello", language: "it" }).result();
    const request = fake.requests.find((r) => r.operation === "sendTurn");
    expect(request?.accept).toBe("text/event-stream");
    expect(request?.body).toMatchObject({ text: "hello", language: "it", client_capabilities: expect.objectContaining({ contract: "1.5" }) });
  });

  it("reads a byte-by-byte stream (the reader copes with any chunking)", async () => {
    const { client, fake } = createHost({}, { streamChunkBytes: 1 });
    fake.onTurn(() => reply("héllo 世界 😀"));
    const result = await client.sendTurn("ses-1", { text: "x" }).result();
    expect(turnText(result.events)).toBe("héllo 世界 😀");
    expect(doneEvent(result.events)?.finish_reason).toBe("completed");
  });

  it("result() reads the rest of a stream that was partly iterated", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => reply("answer"));
    const stream = client.sendTurn("ses-1", { text: "x" });
    const iterator = stream[Symbol.asyncIterator]();
    const first = await iterator.next();
    expect(first.done).toBe(false);
    const result = await stream.result();
    expect(result.events.length).toBeGreaterThan(2);
    expect(result.events[0]).toEqual(first.value);
  });

  it("delivers a clarification the user can answer with a choice", async () => {
    const { client, fake } = createHost();
    fake.onTurn((turn) =>
      "choice_id" in turn.input
        ? reply(`chose ${turn.input.choice_id}`)
        : askChoice("doctor", "Which doctor?", [
            { choice_id: "dr-a", label: "Dr A" },
            { choice_id: "dr-b", label: "Dr B" },
          ]),
    );
    const asked = await client.sendTurn("ses-1", { text: "book" }).result();
    expect(asked.events.some((e) => e.type === "clarification")).toBe(true);
    expect(doneEvent(asked.events)?.finish_reason).toBe("awaiting_input");
    const pending = await client.getPendingState("ses-1");
    expect(pending).toMatchObject({ kind: "clarification", question: { field: "doctor", answer_kind: "choice" } });
    const answered = await client.sendTurn("ses-1", { choice_id: "dr-a" }).result();
    expect(turnText(answered.events)).toBe("chose dr-a");
    expect(await client.getPendingState("ses-1")).toBeNull();
  });

  it("a turn that ended in a typed failure is delivered as events, not thrown", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction(BOOK));
    const grant = (await client.sendTurn("ses-1", { text: "book" }).result()).confirmation!;
    await client.sendTurn("ses-1", { confirmation: grant }).result();
    const again = await client.sendTurn("ses-1", { confirmation: grant }).result(); // the secret is spent
    expect(again.events.find((e) => e.type === "error")).toMatchObject({ error: { code: "replayed", retryable: false } });
    expect(doneEvent(again.events)?.finish_reason).toBe("failed");
  });
});

describe("the confirmation secret", () => {
  it("is read from the X-AlixAI-Confirmation-Token header of a stream and paired with the preparation", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction(BOOK));
    const stream = client.sendTurn("ses-1", { text: "book it" });
    const result = await stream.result();
    const grant = await stream.confirmation;
    expect(grant).toBeDefined();
    expect(grant?.token).toMatch(CONFIRMATION_TOKEN);
    expect(result.confirmation).toEqual(grant);
    const prepared = result.events.find((e) => e.type === "prepared_action");
    expect(prepared?.type === "prepared_action" && prepared.prepared_action.preparation_id).toBe(grant?.preparation_id);
    expect(doneEvent(result.events)).toMatchObject({ finish_reason: "awaiting_confirmation", preparation_id: grant?.preparation_id });
  });

  it("is read from the confirmation member of a JSON turn", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction(BOOK));
    const result = await client.sendTurnJson("ses-1", { text: "book it" });
    expect(result.confirmation?.token).toMatch(CONFIRMATION_TOKEN);
    const prepared = result.events.find((e) => e.type === "prepared_action");
    expect(prepared?.type === "prepared_action" && prepared.prepared_action.preparation_id).toBe(result.confirmation?.preparation_id);
    // Asked for JSON, the service answered JSON: no stream.
    expect(fake.requests.find((r) => r.operation === "sendTurn")?.accept).toBe("application/json");
  });

  it("is never inside a TurnEvent, an error message or the pending state", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction(BOOK));
    const result = await client.sendTurn("ses-1", { text: "book it" }).result();
    const secret = result.confirmation!.token;
    expect(JSON.stringify(result.events)).not.toContain(secret);
    expect(JSON.stringify(await client.getPendingState("ses-1"))).not.toContain(secret);
    expect(JSON.stringify(await client.getAction(result.confirmation!.preparation_id))).not.toContain(secret);
    expect(JSON.stringify(fake.requests)).not.toContain(secret); // the fake records bodies with tokens redacted
    const failure = await caught(client.confirmAction(result.confirmation!.preparation_id, secret.replace(/.$/, "x")));
    expect(failure.message).not.toContain(secret);
    expect(JSON.stringify(failure.details ?? {})).not.toContain(secret);
  });

  it("has no grant when the turn prepared nothing", async () => {
    const { client } = createHost();
    const stream = client.sendTurn("ses-1", { text: "hello" });
    await stream.result();
    expect(await stream.confirmation).toBeUndefined();
  });

  it("level 3: a request_step_up directive, no secret, finish_reason completed, and it cannot be confirmed through the API", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction({ ...BOOK, capability_id: "demo.close_account", risk_level: 3 }));
    const result = await client.sendTurn("ses-1", { text: "close my account" }).result();
    const directive = result.events.find((e) => e.type === "directive");
    expect(directive?.type === "directive" && directive.directive.act).toBe("request_step_up");
    expect(result.confirmation).toBeUndefined();
    expect(doneEvent(result.events)?.finish_reason).toBe("completed");
    const prepared = result.events.find((e) => e.type === "prepared_action");
    const id = prepared?.type === "prepared_action" ? prepared.prepared_action.preparation_id : "";
    const failure = await caught(client.confirmAction(id, `cfm_${"A".repeat(43)}`));
    expect(failure).toMatchObject({ code: "step_up_required", status: 403, retryable: false });
  });

  it("level 1: committed in the turn, a receipt event and no secret", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction({ ...BOOK, risk_level: 1 }));
    const result = await client.sendTurn("ses-1", { text: "update my phone" }).result();
    expect(result.events.find((e) => e.type === "receipt")).toMatchObject({ receipt: { status: "committed" } });
    expect(result.confirmation).toBeUndefined();
  });

  it("rejects a malformed confirmation header (the secret has a fixed shape)", async () => {
    const { client } = stubbed(() => sse([token(0), done(1)], { "X-AlixAI-Confirmation-Token": "not-a-secret" }));
    const failure = await caught(client.sendTurn("ses-1", { text: "x" }).result());
    expect(failure.code).toBe("internal");
    expect(failure.message).not.toContain("not-a-secret");
  });

  it("refuses a secret that arrives without a prepared action", async () => {
    const secret = `cfm_${"A".repeat(43)}`;
    const { client } = stubbed(() => sse([token(0), done(1)], { "X-AlixAI-Confirmation-Token": secret }));
    const failure = await caught(client.sendTurn("ses-1", { text: "x" }).result());
    expect(failure.code).toBe("internal");
    expect(failure.message).not.toContain(secret);
  });
});

describe("a stream that breaks the contract is a typed error, never a finished turn", () => {
  it("a stream truncated before done is uncertain_outcome, and the events before it were delivered", async () => {
    const { client } = stubbed(() => sse([token(0, "a"), token(1, "b")]));
    const stream = client.sendTurn("ses-1", { text: "x" });
    const seen: TurnEvent[] = [];
    let failure: unknown;
    try {
      for await (const event of stream) seen.push(event);
    } catch (error) {
      failure = error;
    }
    expect(seen.map((e) => e.seq)).toEqual([0, 1]);
    expect(failure).toMatchObject({ name: "AlixAIError", code: "uncertain_outcome", retryable: false });
    await expect(stream.confirmation).rejects.toMatchObject({ code: "uncertain_outcome" });
  });

  it("the fake can cut a stream short for the host's own tests", async () => {
    const { client, fake } = createHost();
    fake.failNext({ operation: "sendTurn", truncateAfterEvents: 2 });
    const failure = await caught(client.sendTurn("ses-1", { text: "x" }).result());
    expect(failure.code).toBe("uncertain_outcome");
  });

  it("a gap in seq is an error", async () => {
    const { client } = stubbed(() => sse([token(0), token(2), done(3)]));
    const failure = await caught(client.sendTurn("ses-1", { text: "x" }).result());
    expect(failure).toMatchObject({ code: "internal", details: { reason: "sequence_gap", expected: 1, got: 2 } });
  });

  it("a stream that does not start at seq 0 is an error", async () => {
    const { client } = stubbed(() => sse([token(1), done(2)]));
    expect((await caught(client.sendTurn("ses-1", { text: "x" }).result())).code).toBe("internal");
  });

  it("an unknown event type is an error", async () => {
    const { client } = stubbed(() => sse([token(0), frame(1, "hologram", { shape: "cube" }), done(2)]));
    const failure = await caught(client.sendTurn("ses-1", { text: "x" }).result());
    expect(failure).toMatchObject({ code: "internal", details: { reason: "malformed_event", event_type: "hologram" } });
  });

  it("an event of a known type that lacks its member is an error", async () => {
    const { client } = stubbed(() => sse([frame(0, "token", {}), done(1)]));
    expect((await caught(client.sendTurn("ses-1", { text: "x" }).result())).code).toBe("internal");
  });

  it("a frame whose event name or id disagrees with the event is an error", async () => {
    const wrongName = stubbed(() => sse([frame(0, "token", { text: "x" }, { event: "done" }), done(1)]));
    expect((await caught(wrongName.client.sendTurn("ses-1", { text: "x" }).result())).code).toBe("internal");
    const wrongId = stubbed(() => sse([frame(0, "token", { text: "x" }, { id: "7" }), done(1)]));
    expect((await caught(wrongId.client.sendTurn("ses-1", { text: "x" }).result())).code).toBe("internal");
  });

  it("a change of turn_id within a stream is an error", async () => {
    const other = `id: 1\nevent: token\ndata: ${JSON.stringify({ type: "token", turn_id: "trn_01J0000000000000000000000B", seq: 1, text: "x" })}\n\n`;
    const { client } = stubbed(() => sse([token(0), other, done(2)]));
    expect((await caught(client.sendTurn("ses-1", { text: "x" }).result())).code).toBe("internal");
  });

  it("an event after done is an error", async () => {
    // The reader stops at done, so this is only visible in the JSON form.
    const { client } = stubbed(() =>
      json(200, {
        turn_id: "trn_01J0000000000000000000000A",
        events: [
          { type: "done", turn_id: "trn_01J0000000000000000000000A", seq: 0, finish_reason: "completed" },
          { type: "token", turn_id: "trn_01J0000000000000000000000A", seq: 1, text: "late" },
        ],
      }),
    );
    expect((await caught(client.sendTurnJson("ses-1", { text: "x" }))).code).toBe("internal");
  });

  it("data that is not JSON is an error", async () => {
    const { client } = stubbed(() => sse("id: 0\nevent: token\ndata: {not json\n\n"));
    expect((await caught(client.sendTurn("ses-1", { text: "x" }).result())).code).toBe("internal");
  });

  it("done must carry a preparation_id exactly when it awaits confirmation", async () => {
    const { client } = stubbed(() => sse([done(0, "awaiting_confirmation")]));
    expect((await caught(client.sendTurn("ses-1", { text: "x" }).result())).code).toBe("internal");
  });

  it("a 200 with an unexpected content type is an error", async () => {
    const { client } = stubbed(() => new Response("<html></html>", { status: 200, headers: { "Content-Type": "text/html" } }));
    expect((await caught(client.sendTurn("ses-1", { text: "x" }).result())).code).toBe("internal");
  });

  it("a 200 JSON answer to a stream request is read as the JSON form of the turn", async () => {
    const turn = "trn_01J0000000000000000000000A";
    const { client } = stubbed(() =>
      json(200, {
        turn_id: turn,
        events: [
          { type: "token", turn_id: turn, seq: 0, text: "hi" },
          { type: "done", turn_id: turn, seq: 1, finish_reason: "completed" },
        ],
      }),
    );
    expect(turnText(await collect(client.sendTurn("ses-1", { text: "x" })))).toBe("hi");
    const again = await client.sendTurn("ses-1", { text: "x" }).result();
    expect(again.turn_id).toBe(turn);
    expect(again.events).toHaveLength(2);
  });

  it("the JSON form carries the confirmation secret in its confirmation member, also when it answers a stream request", async () => {
    const turn = "trn_01J0000000000000000000000A";
    const prep = "prep_01J0000000000000000000000A";
    const secret = "cfm_" + "B".repeat(43);
    const { client } = stubbed(() =>
      json(200, {
        turn_id: turn,
        events: [
          { type: "prepared_action", turn_id: turn, seq: 0, prepared_action: { preparation_id: prep } },
          { type: "done", turn_id: turn, seq: 1, finish_reason: "awaiting_confirmation", preparation_id: prep },
        ],
        confirmation: { preparation_id: prep, token: secret },
      }),
    );
    const result = await client.sendTurn("ses-1", { text: "x" }).result();
    expect(result.confirmation).toEqual({ preparation_id: prep, token: secret });
  });
});

describe("errors before any event", () => {
  it("any non-2xx response throws before anything is yielded, and rejects the grant", async () => {
    const { client, fake } = createHost();
    fake.failNext({ operation: "sendTurn", status: 503, error: { code: "unavailable", message: "the model runtime is down", retryable: true }, headers: { "Retry-After": "2" } });
    const stream = client.sendTurn("ses-1", { text: "x" });
    const iterator = stream[Symbol.asyncIterator]();
    await expect(iterator.next()).rejects.toMatchObject({ code: "unavailable", retryable: true, status: 503, retryAfterMs: 2000 });
    expect(await iterator.next()).toEqual({ done: true, value: undefined });
    await expect(stream.confirmation).rejects.toMatchObject({ code: "unavailable" });
  });

  it("a session that is not the token's is scope_violation, not a stream", async () => {
    const { client } = createHost();
    const failure = await caught(client.sendTurn("ses-2", { text: "x" }).result());
    expect(failure).toMatchObject({ code: "scope_violation", status: 403 });
  });

  it("a quarantined application serves no turns", async () => {
    const { client, fake } = createHost();
    fake.setApplicationStatus("quarantined");
    expect(await caught(client.sendTurn("ses-1", { text: "x" }).result())).toMatchObject({ code: "quarantined", status: 403 });
  });

  it("an ended session is expired (410)", async () => {
    const { client } = createHost();
    await client.createSession();
    await client.endSession("ses-1");
    expect(await caught(client.sendTurn("ses-1", { text: "x" }).result())).toMatchObject({ code: "expired", status: 410 });
  });

  it("invalid input is delivered through the stream (never thrown synchronously) and costs no token", async () => {
    const { client, tokenRequests } = createHost();
    const bad: unknown[] = [
      {},
      { text: "" },
      { text: "a", choice_id: "b" },
      { text: "a", extra: 1 },
      { confirmation: { preparation_id: "prep_x", token: "short" } },
      { confirmation: { preparation_id: "../x", token: `cfm_${"A".repeat(43)}` } },
      { reject: {} },
      { text: "a", language: "" },
      { confirmation: { preparation_id: "prep_x", token: `cfm_${"A".repeat(43)}` }, language: "en" },
      null,
      "text",
    ];
    for (const input of bad) {
      const stream = client.sendTurn("ses-1", input as never); // does not throw here
      expect((await caught(stream.result())).code, JSON.stringify(input)).toBe("validation_failed");
    }
    expect(tokenRequests).toHaveLength(0);
  });
});

describe("cancellation", () => {
  it("aborting the signal mid-stream ends iteration with the abort reason and releases the connection", async () => {
    const { client, fake } = createHost({}, { streamChunkBytes: 16 });
    fake.onTurn(() => reply("word ".repeat(100)));
    const controller = new AbortController();
    const stream = client.sendTurn("ses-1", { text: "x" }, { signal: controller.signal });
    const iterator = stream[Symbol.asyncIterator]();
    expect((await iterator.next()).done).toBe(false);
    controller.abort();
    await expect(iterator.next()).rejects.toMatchObject({ name: "AbortError" });
    expect(await iterator.next()).toEqual({ done: true, value: undefined });
  });

  it("aborting with a reason throws that reason", async () => {
    const { client } = createHost({}, { streamChunkBytes: 4 });
    const controller = new AbortController();
    const stream = client.sendTurn("ses-1", { text: "x" }, { signal: controller.signal });
    const iterator = stream[Symbol.asyncIterator]();
    await iterator.next();
    const reason = new Error("user pressed stop");
    controller.abort(reason);
    await expect(iterator.next()).rejects.toBe(reason);
  });

  it("stream.cancel() ends iteration with an AbortError", async () => {
    const { client } = createHost({}, { streamChunkBytes: 4 });
    const stream = client.sendTurn("ses-1", { text: "x" });
    const iterator = stream[Symbol.asyncIterator]();
    await iterator.next();
    stream.cancel();
    await expect(iterator.next()).rejects.toMatchObject({ name: "AbortError" });
    await expect(stream.confirmation).rejects.toMatchObject({ name: "AbortError" });
  });

  it("a signal that is already aborted sends nothing and spends no token", async () => {
    const { client, fake, tokenRequests } = createHost();
    const controller = new AbortController();
    controller.abort();
    await expect(client.sendTurn("ses-1", { text: "x" }, { signal: controller.signal }).result()).rejects.toMatchObject({ name: "AbortError" });
    await expect(client.createSession({ signal: controller.signal })).rejects.toMatchObject({ name: "AbortError" });
    expect(tokenRequests).toHaveLength(0);
    expect(fake.requests).toHaveLength(0);
  });

  it("breaking out of for-await releases the connection", async () => {
    const watched = watchedSse([token(0, "a"), token(1, "b"), token(2, "c"), done(3)]);
    const { client } = stubbed(() => watched.response);
    for await (const event of client.sendTurn("ses-1", { text: "x" })) {
      expect(event.seq).toBe(0);
      break;
    }
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(watched.cancelled()).toBe(true);
  });

  it("reading to done also releases the connection", async () => {
    // Frames after done keep the stream open, so the release is observable.
    const keepAlive = ": keep-alive\n\n";
    const watched = watchedSse([token(0), done(1), keepAlive, keepAlive]);
    const { client } = stubbed(() => watched.response);
    await client.sendTurn("ses-1", { text: "x" }).result();
    expect(watched.cancelled()).toBe(true);
  });

  it("a turn that outlives its deadline fails with a typed error (outcome unknown, so not retryable)", async () => {
    const { client } = (() => {
      let aborted: unknown;
      const stub = stubbed((_url, init) =>
        new Promise<Response>((_resolve, reject) => {
          init.signal?.addEventListener("abort", () => {
            aborted = init.signal?.reason;
            reject(aborted);
          });
        }),
      );
      return stub;
    })();
    const failure = await caught(client.sendTurn("ses-1", { text: "x" }, { timeoutMs: 20 }).result());
    expect(failure).toMatchObject({ code: "uncertain_outcome", retryable: false });
  });
});
