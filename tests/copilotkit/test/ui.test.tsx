// @vitest-environment jsdom
import "./helpers/jsdom-shims.js";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { CopilotKitProvider, CopilotPopup, CopilotSidebar } from "@copilotkit/react-core/v2";
import { HttpAgent } from "@ag-ui/client";
import { alixaiProviderProps, useAlixAIConfirmation, useAlixAIPendingState, useAlixAISuggestions } from "@alixai/copilotkit-adapter/react";
import type { AlixAIRendererOptions } from "@alixai/copilotkit-adapter/react";
import type { ReactElement, ReactNode } from "react";
import { afterEach, describe, expect, it } from "vitest";
import { newThread, run, startGate } from "./helpers/rig.js";
import type { Gate } from "./helpers/rig.js";
import { row } from "./rows.js";

// CopilotKit's own v2 components in jsdom (the sidebar, the popup and a state reader built on useAgent), talking to the real
// CopilotRuntime and AlixAIAgent over loopback, with the replay gateway serving the golden streams. The chat component and the
// blocks, the confirmation card, the suggestions and stop are shown in jsdom by the adapter suite (sdk/copilotkit-adapter/test
// ui.v2.test.tsx); the headless hook and the v1 components by ui.v1.test.tsx. This file adds what that suite does not render:
// the v2 sidebar and popup, and the pending goal read from CopilotKit's agent state.

let gate: Gate | undefined;
afterEach(async () => {
  cleanup();
  await new Promise((resolve) => setTimeout(resolve, 50));
  await gate?.close();
  gate = undefined;
});

const Helpers = ({ children }: { children?: ReactNode }): ReactElement => {
  useAlixAIConfirmation();
  useAlixAISuggestions();
  return <>{children}</>;
};

/** What a page built on the adapter's useAlixAIPendingState shows of the pending goal the core keeps in the shared state. */
const StateLine = (): ReactElement => {
  const pending = useAlixAIPendingState({ agentId: "default" });
  return <p data-testid="state-line">{pending?.question === undefined ? "no question pending" : `waiting for ${pending.question.field}`}</p>;
};

const Page = ({ gate: stack, shell, options }: { gate: Gate; shell: ReactElement; options?: AlixAIRendererOptions }): ReactElement => (
  <CopilotKitProvider {...alixaiProviderProps({ runtimeUrl: stack.runtimeUrl, agentId: "default" }, options)}>
    <Helpers>
      <StateLine />
      {shell}
    </Helpers>
  </CopilotKitProvider>
);

/**
 * Waits until the page has joined its thread: the connect run has been answered and applied. The gate's gateway stand-in answers every
 * connect with a state that has no pending goal (it keeps no session state), so a connect read again after a run (AlixAIRunner holds a
 * connect that a run overlaps) would reset the state the run set; the tests that assert on the state send after the join, as a real
 * gateway needs no such wait (sdk/copilotkit-adapter ui.lateConnect.test.tsx sends before it).
 */
const joined = async (stack: Gate): Promise<void> => {
  await waitFor(() => expect(stack.gateway.connects.length).toBeGreaterThan(0), { timeout: 8000 });
  await new Promise((resolve) => setTimeout(resolve, 200));
};

const text = (): string => document.body.textContent ?? "";

const waitForText = (pattern: RegExp): Promise<void> =>
  waitFor(
    () => {
      if (!pattern.test(text())) throw new Error(`the page does not show ${pattern}`);
    },
    { timeout: 10000 },
  );

const send = async (message: string): Promise<void> => {
  const input = await screen.findByRole("textbox");
  fireEvent.change(input, { target: { value: message } });
  await waitFor(() => expect((screen.getByTestId("copilot-send-button") as HTMLButtonElement).disabled).toBe(false), { timeout: 8000 });
  fireEvent.click(screen.getByTestId("copilot-send-button"));
};

describe(row("chat"), () => {
  it.each([
    ["sidebar", (thread: string): ReactElement => <CopilotSidebar agentId="default" threadId={thread} defaultOpen />],
    ["popup", (thread: string): ReactElement => <CopilotPopup agentId="default" threadId={thread} defaultOpen />],
  ] as const)("the v2 %s streams the verified answer, and the gateway got the user's message under the thread's session", async (_name, shell) => {
    gate = await startGate();
    const thread = newThread();
    render(<Page gate={gate} shell={shell(thread)} />);
    await send("show item item-1");
    await waitForText(/The item is named gadget\./);
    const request = gate.gateway.requests.find((candidate) => candidate.body?.["threadId"] === thread);
    expect(request?.body?.["messages"]?.at(-1)).toMatchObject({ role: "user", content: "show item item-1" });
    expect(request?.headers["authorization"]).toMatch(new RegExp(`^Bearer ${thread}\\.`));
  });
});

describe(row("chat"), () => {
  it("a page with no CopilotKit runtime reaches an AG-UI endpoint directly (selfManagedAgents), which is how a page uses the Go and Python proxies of a host", async () => {
    gate = await startGate();
    const thread = newThread();
    // The replay gateway stands in for the host's proxy: the same wire (POST of a RunAgentInput, server-sent events back).
    // The proxies themselves are proven by their own SDK suites, not by this one.
    const agent = new HttpAgent({ url: `${gate.gateway.url}/v1/agui`, headers: { Authorization: `Bearer ${thread}.1` }, threadId: thread });
    render(
      <CopilotKitProvider {...alixaiProviderProps({ agentId: "default", selfManagedAgents: { default: agent } })}>
        <CopilotSidebar agentId="default" threadId={thread} defaultOpen />
      </CopilotKitProvider>,
    );
    await send("show item item-1");
    await waitForText(/The item is named gadget\./);
    expect(gate.runtimeRequests).toEqual([]);
    expect(gate.gateway.requests[0]?.body?.["threadId"]).toBe(thread);
  });
});

describe(row("state-render"), () => {
  it("the adapter's useAlixAIPendingState shows the pending question from the shared state, as soon as the run ends in one", async () => {
    gate = await startGate();
    gate.gateway.queue({ golden: "clarification" });
    render(<Page gate={gate} shell={<CopilotSidebar agentId="default" threadId={newThread()} defaultOpen />} />);
    expect(screen.getByTestId("state-line").textContent).toBe("no question pending");
    await joined(gate);
    await send("create an item named widget, small");
    await waitForText(/How many\?/);
    await waitFor(() => expect(screen.getByTestId("state-line").textContent).toBe("waiting for quantity"), { timeout: 10000 });
  });

  it("the adapter's chat renderer (showState) draws the same state under the last message of the v2 sidebar", async () => {
    gate = await startGate();
    gate.gateway.queue({ golden: "clarification" });
    render(<Page gate={gate} options={{ showState: true }} shell={<CopilotSidebar agentId="default" threadId={newThread()} defaultOpen />} />);
    await joined(gate);
    await send("create an item named widget, small");
    await waitForText(/How many\?/);
    await waitFor(() => expect(document.querySelector('[data-alixai="pending-state"]')?.textContent).toContain("Waiting for quantity"), { timeout: 10000 });
    expect(document.querySelectorAll('[data-alixai="pending-state"]')).toHaveLength(1);
  });
});

describe(row("history"), () => {
  it("a v2 page that opens a thread with history loads it from the gateway through the runtime's connect route, with the host's callback asked for the page's thread", async () => {
    gate = await startGate();
    const thread = newThread();
    gate.gateway.queue({ golden: "plain_answer" });
    await run(gate.browser(thread), "plain_answer"); // an earlier visit: the session now has a turn
    const tokens = gate.tokenRequests.length;

    render(<Page gate={gate} shell={<CopilotSidebar agentId="default" threadId={thread} defaultOpen />} />);
    await waitForText(/The item is named gadget\./);
    expect(text()).toContain("show item item-1");
    expect(gate.gateway.connects.length).toBeGreaterThan(0);
    expect(gate.tokenRequests.slice(tokens).every((request) => request.operation === "agui.connect" && request.sessionId === thread)).toBe(true);
  });
});
