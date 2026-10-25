import { fireEvent, screen, waitFor } from "@testing-library/react";
import { expect } from "vitest";
import { CopilotChat, CopilotKitProvider } from "@copilotkit/react-core/v2";
import type { ReactElement, ReactNode } from "react";
import { alixaiProviderProps, useAlixAIConfirmation, useAlixAISuggestions } from "../../src/react/index.js";
import type { AlixAIRendererOptions } from "../../src/react/index.js";
import type { Stack } from "./stack.js";

// The CopilotKit page of the tests: the v2 provider and chat with the AlixAI helpers, pointed at the loopback runtime.

const Helpers = ({ children }: { children?: ReactNode }): ReactElement => {
  useAlixAIConfirmation();
  useAlixAISuggestions();
  return <>{children}</>;
};

export const Page = ({
  stack,
  children,
  renderers,
}: {
  stack: Stack;
  children?: ReactNode;
  renderers?: AlixAIRendererOptions;
}): ReactElement => (
  <CopilotKitProvider {...alixaiProviderProps({ runtimeUrl: stack.runtimeUrl, agentId: "default" }, renderers)}>
    <Helpers>
      {children}
      <CopilotChat agentId="default" threadId={stack.threadId} />
    </Helpers>
  </CopilotKitProvider>
);

/**
 * Waits until the page has joined its thread: the connect run (history and state of the session) has been answered and applied. The runner
 * no longer lets a late connect wipe a run (src/runner.ts, ui.lateConnect.test.tsx sends without joining); the tests that go through here
 * still join, so that no connect is still in flight when their stack is closed (the page's fetch of a connection cut under it rejects
 * outside any test).
 */
const alreadyJoined = new WeakSet<Stack>();
export const joined = async (stack: Stack): Promise<void> => {
  if (alreadyJoined.has(stack)) return;
  await waitFor(() => expect(stack.gateway.connects.length).toBeGreaterThan(0), { timeout: 8000 });
  await new Promise((resolve) => setTimeout(resolve, 200));
  alreadyJoined.add(stack);
};

/** Types into the chat input and presses send, once the send button is enabled. */
export const send = async (text: string, stack?: Stack): Promise<void> => {
  // The page joins its thread first (the connect run of the gateway), unless the test does not care: a connect that lands after a
  // run has started replaces what that run set, and a test that stops or inspects the run must not be at the mercy of the order.
  if (stack !== undefined) await joined(stack);
  const input = await screen.findByRole("textbox");
  fireEvent.change(input, { target: { value: text } });
  await waitFor(() => expect((screen.getByTestId("copilot-send-button") as HTMLButtonElement).disabled).toBe(false), { timeout: 8000 });
  fireEvent.click(screen.getByTestId("copilot-send-button"));
};

export const text = (): string => document.body.textContent ?? "";

export const waitForText = (pattern: RegExp, timeout = 10000): Promise<void> =>
  waitFor(
    () => {
      if (!pattern.test(text())) throw new Error(`the page does not show ${pattern}`);
    },
    { timeout },
  );

/** Every place a browser could keep something: the DOM, the storage areas and the cookie string. */
export const browserState = (): string =>
  [document.documentElement.outerHTML, JSON.stringify({ ...window.localStorage }), JSON.stringify({ ...window.sessionStorage }), document.cookie].join("\n");

/** Waits until no run is in flight: the send button is only enabled while there is something to send or stop. */
export const idle = (): Promise<void> =>
  waitFor(() => expect((screen.getByTestId("copilot-send-button") as HTMLButtonElement).disabled).toBe(true), { timeout: 8000 });

export interface Traffic {
  readonly url: string;
  readonly status: number;
  /** Every response header, one per line. */
  readonly headers: string;
  readonly body: Promise<string>;
}

/** Records everything the page's fetch received from the runtime: the browser's whole view of the server. */
export const recordTraffic = (): { entries: Traffic[]; restore: () => void } => {
  const original = globalThis.fetch;
  const entries: Traffic[] = [];
  globalThis.fetch = (async (input: Parameters<typeof fetch>[0], init?: Parameters<typeof fetch>[1]) => {
    const response = await original(input, init);
    entries.push({
      url: input instanceof Request ? input.url : String(input),
      status: response.status,
      headers: [...response.headers].map(([name, value]) => `${name}: ${value}`).join("\n"),
      body: response.clone().text().catch(() => ""),
    });
    return response;
  }) as typeof fetch;
  return { entries, restore: () => void (globalThis.fetch = original) };
};
