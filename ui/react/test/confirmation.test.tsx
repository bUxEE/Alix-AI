// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AlixConfirmationInterrupt, AlixConfirmationSheet, AlixProvider } from "../src/index.js";
import { interrupt } from "./fixtures.js";

afterEach(cleanup);

describe("the confirmation sheet", () => {
  it("shows the projected effects and reports an approval once", () => {
    const onApprove = vi.fn();
    const onDecline = vi.fn();
    render(<AlixConfirmationSheet interrupt={interrupt()} onApprove={onApprove} onDecline={onDecline} />);
    expect(screen.getByText("Create a visit")).toBeTruthy();
    expect(screen.getByText("Ilaria Bellandi", { selector: "dd" })).toBeTruthy();
    // A fact's own display text wins over the canonical value.
    expect(screen.getByText("EUR 48,50", { selector: "dd" })).toBeTruthy();
    expect(screen.getByText("Leaves your application")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(onApprove).toHaveBeenCalledTimes(1);
    // One interrupt, one answer: the buttons are gone and a second answer cannot happen.
    expect(screen.queryByRole("button", { name: "Decline" })).toBeNull();
    expect(onDecline).not.toHaveBeenCalled();
    expect(screen.getByRole("status").textContent).toBe("Your answer was sent.");
  });

  it("reports a decline", () => {
    const onApprove = vi.fn();
    const onDecline = vi.fn();
    render(<AlixConfirmationSheet interrupt={interrupt()} onApprove={onApprove} onDecline={onDecline} />);
    fireEvent.click(screen.getByRole("button", { name: "Decline" }));
    expect(onDecline).toHaveBeenCalledTimes(1);
    expect(onApprove).not.toHaveBeenCalled();
  });

  it("shows only a decline for an interrupt it cannot read (fail closed)", () => {
    const onApprove = vi.fn();
    const onDecline = vi.fn();
    render(<AlixConfirmationSheet interrupt={interrupt({ metadata: { summary: 3 } })} onApprove={onApprove} onDecline={onDecline} />);
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Decline" }));
    expect(onDecline).toHaveBeenCalledTimes(1);
  });

  it("renders nothing without an interrupt", () => {
    const { container } = render(<AlixConfirmationSheet interrupt={null} onApprove={vi.fn()} onDecline={vi.fn()} />);
    expect(container.innerHTML).toBe("");
  });

  it("marks a high risk or a delete as destructive in words and in the approve button", () => {
    render(<AlixConfirmationSheet interrupt={interrupt({ riskLevel: 3, operation: "delete" })} onApprove={vi.fn()} onDecline={vi.fn()} />);
    expect(screen.getByText("Risk level: High risk")).toBeTruthy();
    expect(screen.getByText("Delete")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Approve" }).getAttribute("data-tone")).toBe("danger");
  });

  it("is reachable by keyboard and by a screen reader", () => {
    render(<AlixConfirmationSheet interrupt={interrupt()} onApprove={vi.fn()} onDecline={vi.fn()} />);
    const sheet = screen.getByRole("region", { name: "Please confirm" });
    // The run waits for this decision: focus moves to the sheet, whose description is the summary.
    expect(document.activeElement).toBe(sheet);
    expect(sheet.getAttribute("aria-describedby")).toBe(screen.getByText(/Book a 30 minute visit/).id);
    for (const button of screen.getAllByRole("button")) expect(button.tagName).toBe("BUTTON");
  });

  it("speaks Italian when asked", () => {
    render(
      <AlixProvider locale="it-IT">
        <AlixConfirmationSheet interrupt={interrupt()} onApprove={vi.fn()} onDecline={vi.fn()} />
      </AlixProvider>,
    );
    expect(screen.getByRole("button", { name: "Approva" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Rifiuta" })).toBeTruthy();
    expect(screen.getByText("Livello di rischio: Con conseguenze")).toBeTruthy();
  });
});

describe("the confirmation wired to the interrupt", () => {
  it("resolves with exactly {approved: true} or {approved: false} and the interrupt id", () => {
    const resolve = vi.fn();
    render(<AlixConfirmationInterrupt interrupt={interrupt()} resolve={resolve} />);
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(resolve).toHaveBeenCalledTimes(1);
    expect(resolve).toHaveBeenCalledWith({ approved: true }, "prep_01J00000000000000000000001");
    cleanup();
    const again = vi.fn();
    render(<AlixConfirmationInterrupt interrupt={interrupt()} resolve={again} />);
    fireEvent.click(screen.getByRole("button", { name: "Decline" }));
    expect(again).toHaveBeenCalledWith({ approved: false }, "prep_01J00000000000000000000001");
  });

  it("answers each interrupt separately, even when the same slot is reused", () => {
    const resolve = vi.fn();
    const first = interrupt();
    const second = { ...interrupt(), id: "prep_01J00000000000000000000002" };
    const { rerender } = render(<AlixConfirmationInterrupt interrupt={first} resolve={resolve} />);
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    rerender(<AlixConfirmationInterrupt interrupt={second} resolve={resolve} />);
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    expect(resolve).toHaveBeenNthCalledWith(2, { approved: true }, "prep_01J00000000000000000000002");
  });

  it("dismisses an unreadable interrupt through cancel, or as a decline when there is no cancel", () => {
    const resolve = vi.fn();
    const cancel = vi.fn();
    render(<AlixConfirmationInterrupt interrupt={interrupt({ metadata: 1 })} resolve={resolve} cancel={cancel} />);
    fireEvent.click(screen.getByRole("button", { name: "Decline" }));
    expect(cancel).toHaveBeenCalledWith("prep_01J00000000000000000000001");
    expect(resolve).not.toHaveBeenCalled();
    cleanup();
    render(<AlixConfirmationInterrupt interrupt={interrupt({ metadata: 1 })} resolve={resolve} />);
    fireEvent.click(screen.getByRole("button", { name: "Decline" }));
    expect(resolve).toHaveBeenCalledWith({ approved: false }, "prep_01J00000000000000000000001");
  });
});
