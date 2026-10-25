// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { HttpAgent } from "@ag-ui/client";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AlixChatPanel, CONTINUE_IN_APP_TOOL_NAME } from "../src/copilotkit.js";

afterEach(cleanup);

// The chat draws the result of the confirmation call (a receipt, or one line) and the step-up call (the "continue in the
// application" card) from the conversation's own messages. A loopback address nothing listens on: the network guard of this suite
// allows only loopback, and nothing here starts a run.

const RUNTIME = "http://127.0.0.1:9/api/copilotkit";

const receipt = {
  receipt_id: "rcpt_1",
  preparation_id: "prep_1",
  capability_id: "work_orders.create",
  status: "committed",
  records: [{ entity_type: "work_order", entity_id: "WO-2318" }],
  facts: [{ field: "work_order", value: { kind: "identifier", value: "WO-2318" } }],
  errors: [],
  audit_ref: "aud_1",
  issued_at: "2026-10-11T09:30:00Z",
};

const call = (id: string, name: string, args: unknown) => ({
  id: `m_${id}`,
  role: "assistant" as const,
  content: "",
  toolCalls: [{ id, type: "function" as const, function: { name, arguments: JSON.stringify(args) } }],
});

const chatWith = (messages: unknown[], props: { onContinueInApp?: (request: unknown) => void } = {}): void => {
  const agent = new HttpAgent({ url: "http://127.0.0.1:9/agui", initialMessages: messages as never });
  render(<AlixChatPanel runtimeUrl={RUNTIME} providerProps={{ selfManagedAgents: { default: agent } }} {...props} />);
};

describe("what the chat draws after a prepared action", () => {
  it("draws the receipt the confirmation call returned", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => undefined);
    try {
      chatWith([
        { id: "u1", role: "user", content: "Replace the filters" },
        call("alixai_confirm_prep_1", "alixai_confirm_action", { preparationId: "prep_1" }),
        { id: "t1", role: "tool", toolCallId: "alixai_confirm_prep_1", content: JSON.stringify(receipt) },
      ]);
      const card = await waitFor(() => screen.getByRole("region", { name: "Result" }));
      expect(card.getAttribute("data-status")).toBe("committed");
      expect(card.textContent).toContain("WO-2318");
    } finally {
      errors.mockRestore();
    }
  });

  it("says in one line that a declined action changed nothing", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => undefined);
    try {
      chatWith([
        call("alixai_confirm_prep_2", "alixai_confirm_action", { preparationId: "prep_2" }),
        { id: "t2", role: "tool", toolCallId: "alixai_confirm_prep_2", content: JSON.stringify({ preparationId: "prep_2", status: "rejected" }) },
      ]);
      await waitFor(() => expect(screen.getByText("Declined. Nothing was changed.")).toBeTruthy());
    } finally {
      errors.mockRestore();
    }
  });

  it("hands a step-up action to the application, which never answers the call", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => undefined);
    const onContinueInApp = vi.fn();
    try {
      chatWith([call("alixai_confirm_prep_3", CONTINUE_IN_APP_TOOL_NAME, { preparationId: "prep_3", capabilityId: "contracts.terminate", summary: "End the contract" })], { onContinueInApp });
      const button = await waitFor(() => screen.getByRole("button", { name: "Continue in the application" }));
      fireEvent.click(button);
      expect(onContinueInApp).toHaveBeenCalledWith({ preparationId: "prep_3", capabilityId: "contracts.terminate", summary: "End the contract" });
    } finally {
      errors.mockRestore();
    }
  });

  it("puts CopilotKit's own parts in dark mode with the panel", () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => undefined);
    try {
      render(<AlixChatPanel runtimeUrl={RUNTIME} theme="dark" />);
      expect(screen.getByRole("region").classList.contains("dark")).toBe(true);
      cleanup();
      render(<AlixChatPanel runtimeUrl={RUNTIME} theme="light" />);
      expect(screen.getByRole("region").classList.contains("dark")).toBe(false);
    } finally {
      errors.mockRestore();
    }
  });
});
