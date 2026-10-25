// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { parsePendingState } from "../src/activities.js";
import { AlixAIPendingCard } from "../src/react/index.js";
import { golden } from "./helpers/golden.js";
import { startStack } from "./helpers/stack.js";
import type { Stack } from "./helpers/stack.js";
import { joined, Page, send, text, waitForText } from "./helpers/ui.js";

// The state renderer of docs/integration/agui.md section 12 (agent state rendering): the pending goal of the shared state,
// read through an allow-list parser and drawn as text.

type Loose = any;

let stack: Stack | undefined;
afterEach(async () => {
  cleanup();
  await new Promise((resolve) => setTimeout(resolve, 50));
  await stack?.close();
  stack = undefined;
});

/** The pending object the core sent in a golden stream (the value of its STATE_DELTA replace of /alixai/pending). */
const pendingOf = (name: Parameters<typeof golden>[0]): Loose => {
  const delta = golden(name).meta.response.events.find((event: Loose) => event.type === "STATE_DELTA" && event.delta.some((op: Loose) => op.path === "/alixai/pending")) as Loose;
  return structuredClone(delta.delta.find((op: Loose) => op.path === "/alixai/pending").value);
};

const stateWith = (pending: unknown): unknown => ({ alixai: { pending } });

describe("parsePendingState", () => {
  it("reads the pending goal the core projects: a question, the fields it has and the fields it still needs", () => {
    expect(parsePendingState(stateWith(pendingOf("clarification")))).toEqual({
      kind: "clarification",
      capabilityId: "items.create",
      phase: "",
      collectedFields: [
        { name: "name", origin: "user_turn" },
        { name: "size", origin: "user_turn" },
      ],
      missingFields: ["quantity"],
      question: { field: "quantity", answerKind: "text", choices: [] },
      expiresAt: "2026-10-02T12:30:00Z",
    });
  });

  it("reads a confirmation and the choices of a question", () => {
    const pending = { kind: "confirmation", capabilityId: "items.create", phase: "prepared", collectedFields: [], missingFields: [], preparationId: "prep_1" };
    expect(parsePendingState(stateWith(pending))).toMatchObject({ kind: "confirmation", preparationId: "prep_1" });
    const asked = { ...pending, kind: "clarification", question: { field: "size", answerKind: "choice", choices: [{ id: "small", label: "Small" }] } };
    expect(parsePendingState(stateWith(asked))?.question?.choices).toEqual([{ id: "small", label: "Small" }]);
  });

  it("reads nothing when there is no goal, and never reads the members the page keeps for itself", () => {
    expect(parsePendingState(stateWith(null))).toBeUndefined();
    expect(parsePendingState({})).toBeUndefined();
    expect(parsePendingState(undefined)).toBeUndefined();
    expect(parsePendingState({ pending: pendingOf("clarification"), mine: { pending: pendingOf("clarification") } })).toBeUndefined();
  });

  it("drops what the projection does not name, so a value or a secret that rode along reaches no renderer", () => {
    const rich = { ...pendingOf("clarification"), slotValues: { name: "widget" }, confirmationToken: "cfm_secret", roles: ["admin"] };
    const parsed = parsePendingState(stateWith(rich));
    expect(JSON.stringify(parsed)).not.toMatch(/widget|cfm_secret|admin/);
  });

  it("fails closed on a shape that is not exactly the projection", () => {
    const base = pendingOf("clarification");
    const hostile: unknown[] = [
      [],
      "pending",
      { ...base, kind: "admin" },
      { ...base, kind: undefined },
      { ...base, capabilityId: 3 },
      { ...base, phase: null },
      { ...base, collectedFields: "name" },
      { ...base, collectedFields: [{ name: "name" }] },
      { ...base, collectedFields: [null] },
      { ...base, missingFields: [1] },
      { ...base, question: "quantity" },
      { ...base, question: { field: "quantity" } },
      { ...base, question: { field: "size", answerKind: "choice", choices: [{ id: "a" }] } },
      { ...base, question: { field: "size", answerKind: "choice", choices: "small" } },
    ];
    for (const pending of hostile) expect(parsePendingState(stateWith(pending)), JSON.stringify(pending)).toBeUndefined();
    expect(parsePendingState({ alixai: [] })).toBeUndefined();
    expect(parsePendingState({ alixai: "pending" })).toBeUndefined();
  });
});

describe("the pending card", () => {
  it("says what the goal waits for, as text", () => {
    render(<AlixAIPendingCard pending={parsePendingState(stateWith(pendingOf("clarification")))} />);
    expect(screen.getByText("Waiting for quantity")).toBeTruthy();
    expect(screen.getByText("Collected: name, size")).toBeTruthy();
    expect(screen.getByText("Still needed: quantity")).toBeTruthy();
    expect(document.querySelector('[data-alixai="pending-state"]')?.getAttribute("data-kind")).toBe("clarification");
  });

  it("draws nothing without a pending goal, and its labels can be replaced", () => {
    const { container, rerender } = render(<AlixAIPendingCard pending={undefined} />);
    expect(container.innerHTML).toBe("");
    rerender(<AlixAIPendingCard pending={parsePendingState(stateWith({ ...pendingOf("clarification"), kind: "confirmation" }))} labels={{ confirmation: "Attendo conferma" }} />);
    expect(screen.getByText("Attendo conferma")).toBeTruthy();
  });

  it("renders every value as text: markup in a field name is shown, never created", () => {
    const hostile = { ...pendingOf("clarification"), collectedFields: [{ name: '<img src=x onerror="window.__pwned=1">', origin: "user_turn" }], capabilityId: "<script>window.__pwned=1</script>" };
    render(<AlixAIPendingCard pending={parsePendingState(stateWith(hostile))} />);
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("script")).toBeNull();
    expect(screen.getByText('<script>window.__pwned=1</script>')).toBeTruthy();
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined();
  });
});

describe("in the chat (showState)", () => {
  it("draws the pending goal under the last message when a run ends in a question, and follows the state when it changes", async () => {
    stack = await startStack();
    stack.gateway.queue({ golden: "clarification" }, { golden: "plain_answer" });
    render(<Page stack={stack} renderers={{ showState: true }} />);
    expect(document.querySelector('[data-alixai="pending-state"]')).toBeNull();
    await joined(stack);

    await send("create an item named widget, small");
    await waitForText(/How many\?/);
    await waitFor(() => expect(screen.getAllByText("Waiting for quantity")).toHaveLength(1), { timeout: 10000 });

    // A later run replaces the state: the question is gone, and so is the card of the question (one card, under the last message).
    await waitFor(() => expect((screen.getByTestId("copilot-send-button") as HTMLButtonElement).disabled).toBe(true), { timeout: 8000 });
    await send("show item item-1");
    await waitForText(/The item is named gadget\./);
    await waitFor(() => expect(text()).not.toContain("Waiting for quantity"), { timeout: 10000 });
    expect(document.querySelectorAll('[data-alixai="pending-state"]').length).toBeLessThanOrEqual(1);
  });

  it("is off unless asked for: the default page shows no state card", async () => {
    stack = await startStack();
    stack.gateway.queue({ golden: "clarification" });
    render(<Page stack={stack} />);
    await joined(stack);
    await send("create an item named widget, small");
    await waitForText(/How many\?/);
    expect(document.querySelector('[data-alixai="pending-state"]')).toBeNull();
    expect(text()).not.toContain("Waiting for quantity");
  });
});
