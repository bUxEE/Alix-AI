// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { CopilotKit, useCopilotChat, useCopilotChatInternal } from "@copilotkit/react-core";
import { CopilotChat, CopilotPopup, CopilotSidebar } from "@copilotkit/react-ui";
import { Role, TextMessage } from "@copilotkit/runtime-client-gql";
import { afterEach, describe, expect, it } from "vitest";
import type { ReactElement } from "react";
import { withoutCloud } from "../src/telemetry.js";
import { startStack } from "./helpers/stack.js";
import type { Stack } from "./helpers/stack.js";
import { text, waitForText } from "./helpers/ui.js";

// The established CopilotKit components (@copilotkit/react-ui: chat, sidebar, popup) and the headless hook useCopilotChat
// of @copilotkit/react-core, against the same loopback stack: real CopilotRuntime, AlixAIAgent, golden streams.

let stack: Stack | undefined;
afterEach(async () => {
  cleanup();
  await new Promise((resolve) => setTimeout(resolve, 50));
  await stack?.close();
  stack = undefined;
});

const provider = (s: Stack, children: ReactElement): ReactElement => (
  <CopilotKit {...withoutCloud({ runtimeUrl: s.runtimeUrl, threadId: s.threadId })}>{children}</CopilotKit>
);

// The question is sent as soon as the page is up, without waiting for its connect to be answered: the runner holds a connect that a run
// overlaps and reads it again after the run (src/runner.ts, ui.lateConnect.test.tsx).
const typeAndSend = async (message: string): Promise<void> => {
  const input = await screen.findByPlaceholderText(/type a message/i);
  fireEvent.change(input, { target: { value: message } });
  fireEvent.keyDown(input, { key: "Enter" });
};

describe("react-ui components", () => {
  it.each([
    ["chat", (): ReactElement => <CopilotChat />],
    ["sidebar", (): ReactElement => <CopilotSidebar defaultOpen />],
    ["popup", (): ReactElement => <CopilotPopup defaultOpen />],
  ])("%s: the question goes to AlixAI as an AG-UI run and the verified answer comes back", async (_name, component) => {
    stack = await startStack();
    render(provider(stack, component()));
    await typeAndSend("show item item-1");
    await waitForText(/The item is named gadget\./);
    const run = stack.gateway.requests[0];
    expect(run?.path).toBe("/v1/agui");
    expect(run?.body?.["threadId"]).toBe(stack.threadId);
    expect(run?.body?.["messages"]?.at(-1)).toMatchObject({ role: "user", content: "show item item-1" });
    expect(stack.tokenRequests.filter((request) => request.operation !== "agui.connect")[0]).toMatchObject({ operation: "agui.run", sessionId: stack.threadId, resume: false });
  });
});

describe("headless: useCopilotChat", () => {
  const Headless = (): ReactElement => {
    // In @copilotkit/react-core 1.76.0 useCopilotChat() returns visibleMessages as undefined (its wrapper reads a member the
    // internal hook no longer returns; compat/matrix.json notes it), so the messages come from the internal hook.
    const { appendMessage, isLoading } = useCopilotChat();
    const { messages } = useCopilotChatInternal();
    return (
      <div>
        <button type="button" onClick={() => void appendMessage(new TextMessage({ role: Role.User, content: "show item item-1" }))}>
          ask
        </button>
        <span data-testid="loading">{String(isLoading)}</span>
        <ul>
          {messages.map((message) => (
            <li key={message.id} data-role={message.role}>
              {typeof message.content === "string" ? message.content : ""}
            </li>
          ))}
        </ul>
      </div>
    );
  };

  it("appendMessage runs the agent and the messages hold the streamed answer", async () => {
    stack = await startStack();
    render(provider(stack, <Headless />));
    fireEvent.click(screen.getByText("ask"));
    await waitForText(/The item is named gadget\./);
    await waitFor(() => expect(screen.getByTestId("loading").textContent).toBe("false"), { timeout: 8000 });
    expect(stack.gateway.requests).toHaveLength(1);
    expect(text()).toContain("show item item-1");
  });
});
