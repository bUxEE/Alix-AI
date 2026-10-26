// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { AlixMemoryList, AlixMemoryProposal, AlixProvider } from "../src/index.js";

afterEach(cleanup);

describe("AlixMemoryProposal", () => {
  it("calls onConfirm once", () => {
    const onConfirm = vi.fn();
    const onReject = vi.fn();
    render(
      <AlixProvider>
        <AlixMemoryProposal
          proposal={{ id: "mem_01J0000000000000000000000A", text: "Prefer dark mode" }}
          onConfirm={onConfirm}
          onReject={onReject}
        />
      </AlixProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Remember" }));
    expect(onConfirm).toHaveBeenCalledWith("mem_01J0000000000000000000000A");
    expect(onReject).not.toHaveBeenCalled();
  });

  it("disables actions when expired", () => {
    const onConfirm = vi.fn();
    render(
      <AlixProvider>
        <AlixMemoryProposal
          proposal={{ id: "mem_01J0000000000000000000000A", text: "Prefer dark mode", expiresAt: "2000-01-01T00:00:00Z" }}
          onConfirm={onConfirm}
          onReject={vi.fn()}
        />
      </AlixProvider>,
    );
    expect(screen.getByText(/expired|scaduta/i)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remember" }));
    expect(onConfirm).not.toHaveBeenCalled();
  });
});

describe("AlixMemoryList", () => {
  it("shows empty state and emits delete", () => {
    const onDelete = vi.fn();
    const { rerender } = render(
      <AlixProvider>
        <AlixMemoryList
          items={[]}
          onDelete={onDelete}
        />
      </AlixProvider>,
    );
    expect(screen.getByText(/No memories|Nessuna memoria/i)).toBeTruthy();
    rerender(
      <AlixProvider>
        <AlixMemoryList
          items={[{ id: "mem_01J0000000000000000000000A", text: "Prefer dark mode", status: "active" }]}
          onDelete={onDelete}
        />
      </AlixProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: /Delete|Elimina/i }));
    expect(onDelete).toHaveBeenCalledWith("mem_01J0000000000000000000000A");
  });
});
