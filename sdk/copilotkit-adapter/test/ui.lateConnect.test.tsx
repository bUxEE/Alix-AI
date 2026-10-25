// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { CopilotKit } from "@copilotkit/react-core";
import { CopilotChat } from "@copilotkit/react-ui";
import { afterEach, describe, expect, it } from "vitest";
import type { ReactElement } from "react";
import { withoutCloud } from "../src/telemetry.js";
import { startStack } from "./helpers/stack.js";
import type { Stack } from "./helpers/stack.js";
import { text, waitForText } from "./helpers/ui.js";

// A message sent in the instant after the page loads, before the answer to the page's connect arrives (the gateway's history read is
// slow: the fake gateway holds the answer). The connect answer replays the thread's history and state; applied on top of the run that
// has started meanwhile it used to replace the run's messages, so the answer never showed. These tests do NOT wait for the join.

let stack: Stack | undefined;
afterEach(async () => {
  cleanup();
  await new Promise((resolve) => setTimeout(resolve, 50));
  await stack?.close();
  stack = undefined;
});

const lateConnectDelayMs = 700;
const lateConnectRunDelayMs = 300;

describe("lateConnect: a connect answered after the first message", () => {
  it("react-ui chat: the question sent before the join still shows its verified answer", async () => {
    stack = await startStack({ connectDelayMs: lateConnectDelayMs, runDelayMs: lateConnectRunDelayMs });
    render(
      <CopilotKit {...withoutCloud({ runtimeUrl: stack.runtimeUrl, threadId: stack.threadId })}>
        <CopilotChat />
      </CopilotKit>,
    );
    const input = await screen.findByPlaceholderText(/type a message/i);
    fireEvent.change(input, { target: { value: "show item item-1" } });
    fireEvent.keyDown(input, { key: "Enter" });
    await waitForText(/The item is named gadget\./);
    // The connect the run overlapped is read a second time, after the run; once that answer has been sent (no runtime response is
    // left open) and the page has had a moment to apply it, the run's messages are still there.
    const s = stack;
    await waitFor(() => expect(s.gateway.connects).toHaveLength(2), { timeout: 8000 });
    await waitFor(() => expect(s.lateConnectOpen()).toBe(0), { timeout: 8000 });
    await new Promise((resolve) => setTimeout(resolve, 300));
    expect(s.gateway.connects).toHaveLength(2);
    expect(text()).toContain("The item is named gadget.");
    expect(text()).toContain("show item item-1");
  });
});
