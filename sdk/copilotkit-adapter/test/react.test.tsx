// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  ALIXAI_BLOCK_TYPES,
  approvalPayload,
  blockActivityType,
  formatCanonicalValue,
  parseAttachments,
  parseBlock,
  parseCitations,
  parseConfirmation,
  parseSuggestions,
} from "../src/activities.js";
import { AlixAIChoices, AlixAIConfirmationCard, AlixAITable, createAlixAIActivityRenderers } from "../src/react/index.js";
import { golden } from "./helpers/golden.js";
import type { GoldenName } from "./helpers/golden.js";

afterEach(cleanup);

type Loose = any;

const interrupt = (): Loose => {
  const finished = golden("prepare_interrupt").meta.response.events.at(-1) as Loose;
  return structuredClone(finished.outcome.interrupts[0]);
};

const eventOf = (name: GoldenName, match: (event: Loose) => boolean): Loose => golden(name).meta.response.events.find((event) => match(event));

describe("the confirmation card", () => {
  it("shows the effects the core projected and answers exactly {approved: true} once", () => {
    const resolve = vi.fn();
    render(<AlixAIConfirmationCard interrupt={interrupt()} resolve={resolve} />);
    expect(screen.getByText('Create an item named "widget".')).toBeTruthy();
    expect(screen.getByText("widget", { selector: "dd" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(resolve).toHaveBeenCalledWith(approvalPayload(true), "prep_01J00000000000000000000001");
    // A second click after the answer does nothing: one interrupt, one answer.
    fireEvent.click(screen.getByRole("button", { name: "Decline" }));
    expect(resolve).toHaveBeenCalledTimes(1);
  });

  it("decline answers {approved: false}", () => {
    const resolve = vi.fn();
    render(<AlixAIConfirmationCard interrupt={interrupt()} resolve={resolve} />);
    fireEvent.click(screen.getByRole("button", { name: "Decline" }));
    expect(resolve).toHaveBeenCalledWith({ approved: false }, "prep_01J00000000000000000000001");
  });

  it("fails closed: an interrupt that is not a confirmation shows no effects and offers only the decline", () => {
    const resolve = vi.fn();
    render(<AlixAIConfirmationCard interrupt={{ id: "prep_x", reason: "something_else", metadata: { summary: "approve me" } }} resolve={resolve} />);
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByText("approve me")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Decline" }));
    expect(resolve).toHaveBeenCalledWith({ approved: false }, "prep_x");
  });

  it("renders every value as text: markup in a summary or an effect is shown, never created", () => {
    const hostile = interrupt();
    hostile.metadata.summary = '<img src=x onerror="window.__pwned=1">';
    hostile.metadata.effects[0].description = "<script>window.__pwned=1</script>";
    render(<AlixAIConfirmationCard interrupt={hostile} resolve={vi.fn()} />);
    expect(document.querySelector("img")).toBeNull();
    expect(document.querySelector("script")).toBeNull();
    expect(screen.getByText('<img src=x onerror="window.__pwned=1">')).toBeTruthy();
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined();
  });

  it("has no place for a secret and prints none", () => {
    render(<AlixAIConfirmationCard interrupt={interrupt()} resolve={vi.fn()} />);
    expect(document.body.innerHTML).not.toMatch(/cfm_|confirmation[-_ ]token/i);
  });
});

describe("parsers", () => {
  it("read the golden payloads", () => {
    expect(parseConfirmation(interrupt())).toMatchObject({ preparationId: "prep_01J00000000000000000000001", capabilityId: "items.create", riskLevel: 2, mode: "explicit" });
    const citations = eventOf("read_with_citations", (e) => e.activityType === "alixai.citations");
    expect(parseCitations(citations.content)).toHaveLength(1);
    const attachments = eventOf("attachments_preview", (e) => e.activityType === "alixai.attachments");
    expect(parseAttachments(attachments.content)?.[0]).toMatchObject({ fileId: "fil_01J00000000000000000000001", previewable: true });
    const suggestions = eventOf("suggestions", (e) => e.name === "alixai.suggestions");
    expect(parseSuggestions(suggestions.value).map((s) => s.title)).toEqual(["Create an item", "Get an item"]);
  });

  it("refuse what is not exactly AlixAI's payload", () => {
    expect(parseConfirmation({ id: "p", reason: "confirmation_required", metadata: { summary: "x" } })).toBeUndefined();
    expect(parseConfirmation(undefined)).toBeUndefined();
    expect(parseCitations({ citations: [{ nope: 1 }] })).toBeUndefined();
    expect(parseAttachments({ files: [{ fileId: 1 }] })).toBeUndefined();
    expect(parseSuggestions({ suggestions: [{ title: "only a title" }, 4, null] })).toEqual([]);
    expect(parseBlock("table", { type: "chart" })).toBeUndefined();
    expect(parseBlock("table", { type: "table", columns: "x", rows: [] })).toBeUndefined();
  });

  it("format canonical values as plain text", () => {
    expect(formatCanonicalValue({ kind: "money", value: "12.50", currency: "EUR" })).toBe("12.50 EUR");
    expect(formatCanonicalValue({ kind: "text", value: "widget" })).toBe("widget");
    expect(formatCanonicalValue(null)).toBe("");
  });
});

describe("activity renderers", () => {
  it("cover every block type, the citations, the attachments and the progress, and refuse a payload of another shape", () => {
    const renderers = createAlixAIActivityRenderers();
    const types = renderers.map((renderer) => renderer.activityType);
    for (const type of ALIXAI_BLOCK_TYPES) expect(types).toContain(blockActivityType(type));
    expect(types).toEqual(expect.arrayContaining(["alixai.citations", "alixai.attachments", "alixai.progress"]));
    const table = renderers.find((renderer) => renderer.activityType === "alixai.block.table");
    const validate = (value: unknown): unknown => (table?.content as Loose)["~standard"].validate(value);
    expect(validate({ type: "table", columns: [], rows: [] })).toHaveProperty("value");
    expect(validate({ type: "chart" })).toHaveProperty("issues");
  });

  it("render a table and a choice form as text and buttons", () => {
    const table = eventOf("table", (e) => e.activityType === "alixai.block.table");
    render(<AlixAITable block={parseBlock("table", table.content)!} />);
    expect(screen.getByRole("table")).toBeTruthy();
    cleanup();
    const forms = eventOf("forms", (e) => e.activityType === "alixai.block.clarification_choices");
    const chose = vi.fn();
    render(<AlixAIChoices block={parseBlock("clarification_choices", forms.content)!} onChoose={chose} onFreeText={vi.fn()} />);
    fireEvent.click(screen.getByRole("button", { name: "medium" }));
    expect(chose).toHaveBeenCalledWith("medium");
  });
});
