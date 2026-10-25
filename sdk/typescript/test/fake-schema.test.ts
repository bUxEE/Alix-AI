import { describe, expect, it } from "vitest";

import type { TurnEvent } from "../src/index.js";
import { askChoice, failTurn, prepareAction, reply } from "../src/testing/index.js";
import { createHost } from "./helpers/host.js";
import { actionReceiptSchema, describeErrors, errorSchema, preparedActionSchema, turnEventSchema } from "./helpers/schemas.js";

// The fake server is only useful to a host if what it sends is what the real service sends. Every
// event it produces, and every action document and error, is validated against the repository's
// JSON Schemas (the same ones the Go core's output is checked against).

const expectValidEvents = (label: string, events: TurnEvent[]): void => {
  expect(events.length, label).toBeGreaterThan(1);
  for (const event of events) {
    expect(turnEventSchema(event), `${label}: ${event.type} #${event.seq}: ${describeErrors(turnEventSchema)}\n${JSON.stringify(event, null, 1)}`).toBe(true);
  }
  expect(events.at(-1)?.type, `${label}: the last event`).toBe("done");
};

const BOOK = { capability_id: "demo.book", summary: "Book the 10:00 slot on 2026-10-02", text: "Shall I book it?" };

describe("the fake server's events validate against schemas/conversation/turn-event", () => {
  it("a plain reply", async () => {
    const { client } = createHost();
    const result = await client.sendTurn("ses-1", { text: "hello" }).result();
    expectValidEvents("reply", result.events);
  });

  it("a clarification with choices, then the chosen answer", async () => {
    const { client, fake } = createHost();
    fake.onTurn((turn) =>
      "choice_id" in turn.input
        ? reply(`you chose ${turn.input.choice_id}`)
        : askChoice("doctor", "Which doctor?", [
            { choice_id: "dr-a", label: "Dr A" },
            { choice_id: "dr-b", label: "Dr B", value: { kind: "text", value: "Dr B" } },
          ]),
    );
    expectValidEvents("clarify", (await client.sendTurn("ses-1", { text: "book a doctor" }).result()).events);
    expectValidEvents("choice", (await client.sendTurn("ses-1", { choice_id: "dr-a" }).result()).events);
  });

  it("a prepared action awaiting confirmation, a confirmation and a rejection (as turn inputs)", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction(BOOK));
    const prepared = await client.sendTurn("ses-1", { text: "book it" }).result();
    expectValidEvents("prepare", prepared.events);
    const grant = prepared.confirmation;
    expect(grant).toBeDefined();
    const confirmed = await client.sendTurn("ses-1", { confirmation: grant! }).result();
    expectValidEvents("confirm", confirmed.events);

    const second = await client.sendTurn("ses-1", { text: "again" }).result();
    const rejected = await client.sendTurn("ses-1", { reject: { preparation_id: second.confirmation!.preparation_id } }).result();
    expectValidEvents("reject", rejected.events);
  });

  it("a confirmation that is refused (replayed) is an error event and a failed done", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction(BOOK));
    const grant = (await client.sendTurn("ses-1", { text: "book it" }).result()).confirmation!;
    await client.sendTurn("ses-1", { confirmation: grant }).result();
    const replayed = await client.sendTurn("ses-1", { confirmation: grant }).result();
    expectValidEvents("replayed", replayed.events);
    expect(replayed.events.find((e) => e.type === "error")).toMatchObject({ error: { code: "replayed" } });
  });

  it("a level 3 action (step-up), a level 1 action committed in the turn, and every commit outcome", async () => {
    const { client, fake } = createHost();
    fake.onTurn((turn) => {
      const text = "text" in turn.input ? turn.input.text : "";
      if (text === "step-up") return prepareAction({ ...BOOK, risk_level: 3 });
      if (text === "level1") return prepareAction({ ...BOOK, risk_level: 1 });
      return prepareAction({ ...BOOK, commit: { status: text as "rejected" | "partial" | "uncertain" | "queued" } });
    });
    expectValidEvents("step-up", (await client.sendTurn("ses-1", { text: "step-up" }).result()).events);
    expectValidEvents("level 1", (await client.sendTurn("ses-1", { text: "level1" }).result()).events);
    for (const status of ["rejected", "partial", "uncertain", "queued"]) {
      const prepared = await client.sendTurn("ses-1", { text: status }).result();
      const confirmed = await client.sendTurn("ses-1", { confirmation: prepared.confirmation! }).result();
      expectValidEvents(`commit ${status}`, confirmed.events);
    }
  });

  it("a failed turn", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => failTurn("unavailable", "the model runtime is not answering"));
    const result = await client.sendTurn("ses-1", { text: "hello" }).result();
    expectValidEvents("failed", result.events);
    expect(result.events.at(-1)).toMatchObject({ type: "done", finish_reason: "failed" });
  });

  it("a long reply is split into token events within the schema's limit", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => reply("word ".repeat(2000)));
    const result = await client.sendTurn("ses-1", { text: "long" }).result();
    expectValidEvents("long", result.events);
    expect(result.events.filter((e) => e.type === "token").length).toBeGreaterThan(1);
  });
});

describe("the fake server's other documents validate against their schemas", () => {
  it("prepared actions and receipts", async () => {
    const { client, fake } = createHost();
    fake.onTurn(() => prepareAction(BOOK));
    const grant = (await client.sendTurn("ses-1", { text: "book" }).result()).confirmation!;
    const action = await client.getAction(grant.preparation_id);
    expect(preparedActionSchema(action), describeErrors(preparedActionSchema)).toBe(true);
    const outcome = await client.confirmAction(grant.preparation_id, grant.token);
    expect(preparedActionSchema(outcome.preparation), describeErrors(preparedActionSchema)).toBe(true);
    expect(actionReceiptSchema(outcome.receipt), describeErrors(actionReceiptSchema)).toBe(true);
    const receipt = await client.getActionReceipt(grant.preparation_id);
    expect(actionReceiptSchema(receipt), describeErrors(actionReceiptSchema)).toBe(true);
  });

  it("every error response is a valid Error document", async () => {
    const { client, fake } = createHost();
    const errors: unknown[] = [];
    const capture = async (promise: Promise<unknown>): Promise<void> => {
      try {
        await promise;
      } catch (error) {
        const e = error as { code: string; message: string; retryable: boolean; details?: unknown };
        errors.push({ code: e.code, message: e.message, retryable: e.retryable, ...(e.details !== undefined && { details: e.details }) });
      }
    };
    await capture(client.getSession("ses-other")); // scope_violation
    await capture(client.getActionReceipt("prep_01J0000000000000000000000A")); // scope_violation
    fake.setApplicationStatus("quarantined");
    await capture(client.createSession()); // quarantined
    fake.setApplicationStatus("active");
    fake.failNext({ operation: "getSession", status: 503, error: { code: "unavailable", message: "down", retryable: true } });
    await capture(client.getSession("ses-1")); // unavailable
    expect(errors.length).toBe(4);
    for (const error of errors) expect(errorSchema(error), `${JSON.stringify(error)} ${describeErrors(errorSchema)}`).toBe(true);
  });
});
