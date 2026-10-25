// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AlixActionCard, AlixChartBlock, AlixClarificationForm, AlixDiagramBlock, AlixProgressLine, AlixProvider, AlixTableBlock } from "../src/index.js";
import { actionCardBlock, chartBlock, choicesBlock, diagramBlock, tableBlock } from "./fixtures.js";

afterEach(cleanup);

describe("the table block", () => {
  it("is a labelled, focusable region with real headers, and never turns a cell into markup", () => {
    const { container } = render(<AlixTableBlock block={tableBlock()} />);
    const region = screen.getByRole("region", { name: "Open visits" });
    expect(region.getAttribute("tabindex")).toBe("0");
    expect(screen.getAllByRole("columnheader").map((cell) => cell.textContent)).toEqual(["Patient", "Minutes", "Paid"]);
    expect(screen.getByText("<img src=x onerror=alert(1)>")).toBeTruthy();
    expect(container.querySelector("img")).toBeNull();
    // Booleans are words, numbers follow the locale.
    expect(within(region).getAllByText("Yes").length).toBe(1);
    expect(screen.getByText("12,500")).toBeTruthy();
  });

  it("formats numbers and booleans in Italian", () => {
    render(
      <AlixProvider locale="it">
        <AlixTableBlock block={tableBlock()} />
      </AlixProvider>,
    );
    expect(screen.getByText("12.500")).toBeTruthy();
    expect(screen.getByText("Sì")).toBeTruthy();
  });
});

describe("the chart block", () => {
  it.each(["bar", "line", "area", "point", "pie"] as const)("draws a %s chart named by its description, with the data one click away", (mark) => {
    const { container } = render(<AlixChartBlock block={chartBlock(mark)} />);
    const drawing = screen.getByRole("img", { name: "Visits per week, rising from 12 to 31." });
    expect(drawing.tagName.toLowerCase()).toBe("svg");
    expect(container.querySelectorAll(".alix-chart__mark").length).toBeGreaterThan(0);
    const toggle = screen.getByRole("button", { name: "Show data" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(screen.getByRole("button", { name: "Hide data" }).getAttribute("aria-expanded")).toBe("true");
    expect(screen.getAllByRole("columnheader").map((cell) => cell.textContent)).toEqual(["week", "visits", "clinic"]);
  });

  it("names the series when the chart has more than one", () => {
    render(<AlixChartBlock block={chartBlock("bar")} />);
    expect(screen.getByRole("list", { name: "Series" }).textContent).toBe("NorthSouth");
  });

  it("draws an empty chart without throwing", () => {
    const block = { ...chartBlock("bar"), data: [] };
    expect(() => render(<AlixChartBlock block={block} />)).not.toThrow();
  });
});

describe("the other blocks", () => {
  it("shows a diagram as its source", () => {
    render(<AlixDiagramBlock block={diagramBlock()} />);
    expect(screen.getByLabelText("Diagram source").textContent).toBe("graph TD; A-->B");
  });

  it("shows an action card's facts", () => {
    render(<AlixActionCard block={actionCardBlock()} />);
    expect(screen.getByRole("region", { name: "Visit booked" })).toBeTruthy();
    expect(screen.getByText("48.50 EUR")).toBeTruthy();
    // No handler, no buttons: the card commits nothing by itself.
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("reports a permitted next action to the host", () => {
    const onAction = vi.fn();
    const block = { ...actionCardBlock(), actions: [{ action: "retry" }, { action: "continue_in_app", label: "Open the calendar" }] } as ReturnType<typeof actionCardBlock>;
    render(<AlixActionCard block={block} onAction={onAction} />);
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Open the calendar" }));
    expect(onAction).toHaveBeenCalledWith({ action: "continue_in_app", label: "Open the calendar" });
  });
});

describe("the clarification form", () => {
  it("reports the chosen choice id", () => {
    const onChoose = vi.fn();
    render(<AlixClarificationForm block={choicesBlock()} onChoose={onChoose} />);
    expect(screen.getByRole("group", { name: "Which slot suits you?" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "11:00" }));
    expect(onChoose).toHaveBeenCalledWith("slot_b");
  });

  it("sends free text only when the block allows it, trimmed, and not when empty", () => {
    const onFreeText = vi.fn();
    render(<AlixClarificationForm block={choicesBlock()} onChoose={vi.fn()} onFreeText={onFreeText} />);
    const send = screen.getByRole("button", { name: "Send" }) as HTMLButtonElement;
    expect(send.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Or write your own answer"), { target: { value: "  Friday morning " } });
    fireEvent.click(send);
    expect(onFreeText).toHaveBeenCalledWith("Friday morning");
    cleanup();
    render(<AlixClarificationForm block={choicesBlock(false)} onChoose={vi.fn()} onFreeText={onFreeText} />);
    expect(screen.queryByLabelText("Or write your own answer")).toBeNull();
  });
});

describe("the progress line", () => {
  it("names the stages the schema lists in the reader's language, and shows another stage as it is, made readable", () => {
    render(
      <>
        <AlixProgressLine progress={{ stage: "tool_call", percent: 40 }} />
        <AlixProgressLine progress={{ stage: "custom_host-step" }} />
      </>,
    );
    expect(screen.getByText("Asking the application 40%")).toBeTruthy();
    expect(screen.getByText("custom host step")).toBeTruthy();
    cleanup();
    render(
      <AlixProvider locale="it">
        <AlixProgressLine progress={{ stage: "generating" }} />
      </AlixProvider>,
    );
    expect(screen.getByText("Scrittura della risposta")).toBeTruthy();
  });
});
