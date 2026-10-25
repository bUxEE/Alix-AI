// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useAgentContext, useFrontendTool } from "@copilotkit/react-core/v2";
import { z } from "zod";
import { afterEach, describe, expect, it } from "vitest";
import { SECRET_HEADER, CONFIRMATION_SECRET } from "./helpers/fake-gateway.js";
import { browserState, idle, Page, recordTraffic, send, text, waitForText } from "./helpers/ui.js";
import { startStack } from "./helpers/stack.js";
import type { Stack } from "./helpers/stack.js";

// CopilotKit's v2 chat (CopilotChat of @copilotkit/react-core/v2) in jsdom, talking to the real CopilotRuntime and
// AlixAIAgent over loopback, with the fake gateway replaying the golden streams of M3-25. What the page shows is what a
// person would see; what the gateway received is what AlixAI would have received.

let stack: Stack | undefined;
const start = async (): Promise<Stack> => (stack = await startStack());

afterEach(async () => {
  cleanup();
  await new Promise((resolve) => setTimeout(resolve, 50));
  await stack?.close();
  stack = undefined;
});

describe("messages, blocks and activities", () => {
  it("streams the verified answer into the chat", async () => {
    const s = await start();
    render(<Page stack={s} />);
    await send("show item item-1", s);
    await waitForText(/The item is named gadget\./);
    expect(s.gateway.requests[0]?.body?.["messages"]?.at(-1)).toMatchObject({ role: "user", content: "show item item-1" });
    expect(s.gateway.requests[0]?.headers["authorization"]).toMatch(/^Bearer ses_ui_01\./);
  });

  it("renders a table block", async () => {
    const s = await start();
    s.gateway.queue({ golden: "table" });
    render(<Page stack={s} />);
    await send("free slots", s);
    const table = await screen.findByRole("table", {}, { timeout: 10000 });
    expect(within(table).getByText("Free slots on 14 October")).toBeTruthy();
    expect(within(table).getAllByText("Dr. Rossi", { selector: "td" }).length).toBe(2);
  });

  it("renders a chart block with its description and data", async () => {
    const s = await start();
    s.gateway.queue({ golden: "chart" });
    render(<Page stack={s} />);
    await send("appointments per weekday", s);
    await waitForText(/Appointments per weekday/);
    expect(document.querySelector('[data-alixai="chart"] svg')).not.toBeNull();
    expect(text()).toContain("Mon");
  });

  it("renders the form of a clarification and answers a choice with forwardedProps.alixai.choiceId, no text", async () => {
    const s = await start();
    s.gateway.queue({ golden: "forms" }, { golden: "choice_answer" });
    render(<Page stack={s} />);
    await send("create an item named widget with quantity 3", s);
    const small = await screen.findByRole("button", { name: "small" }, { timeout: 10000 });
    // The person answers after the run has ended, as a person would.
    await idle();
    fireEvent.click(small);
    await screen.findByRole("button", { name: "Approve" }, { timeout: 10000 });
    const second = s.gateway.requests[1]?.body;
    expect(second?.["forwardedProps"]).toEqual({ alixai: { choiceId: "small" } });
    expect(second?.["resume"]).toBeUndefined();
  });

  it("renders citations with their locator and claims", async () => {
    const s = await start();
    render(<Page stack={s} />);
    await send("what is the warranty on the Model X kettle", s);
    await waitForText(/Warranty terms/);
    expect(text()).toContain("Section 2");
    expect(document.querySelector('[data-alixai="citations"] li')?.getAttribute("data-availability")).toBe("available");
  });

  it("renders the attachment preview of the user's message", async () => {
    const s = await start();
    s.gateway.queue({ golden: "attachments_preview" });
    render(<Page stack={s} renderers={{ renderPreview: (file) => <span data-testid="preview">{file.fileId}</span> }} />);
    await send("show item item-1", s);
    await waitForText(/photo\.png/);
    expect(screen.getByTestId("preview").textContent).toBe("fil_01J00000000000000000000001");
  });

  it("shows the explaining text of a refused request and the run ends in error", async () => {
    const s = await start();
    render(<Page stack={s} />);
    await send("what is the weather like in Rome tomorrow", s);
    await waitForText(/isn't something I can do in this application/);
  });

  it("offers the deterministic suggestions as pills and sends the picked one as an ordinary message", async () => {
    const s = await start();
    render(<Page stack={s} />);
    await send("what can you do", s);
    const pill = await screen.findByText("Get an item", {}, { timeout: 10000 });
    s.gateway.queue({ golden: "plain_answer" });
    fireEvent.click(pill);
    await waitFor(() => expect(s.gateway.requests.length).toBe(2), { timeout: 10000 });
    expect(s.gateway.requests[1]?.body?.["messages"]?.at(-1)).toMatchObject({ role: "user", content: "Get an item" });
  });
});

describe("human approval", () => {
  it("approve: the card shows the exact effects, the run resumes with the secret added server side, and the secret never reaches the page", async () => {
    const s = await start();
    const traffic = recordTraffic();
    render(<Page stack={s} />);
    await send("create an item named widget with quantity 3", s);
    const approve = await screen.findByRole("button", { name: "Approve" }, { timeout: 10000 });
    expect(text()).toContain('Create an item named "widget".');
    fireEvent.click(approve);
    await waitForText(/Done\. The item widget was created\./);
    const [prepare, resume] = s.gateway.requests;
    expect(prepare?.headers[SECRET_HEADER]).toBeUndefined();
    expect(resume?.body?.["resume"]).toEqual([{ interruptId: "prep_01J00000000000000000000001", status: "resolved", payload: { approved: true } }]);
    expect(resume?.headers[SECRET_HEADER]).toBe(CONFIRMATION_SECRET);
    expect(browserState()).not.toContain(CONFIRMATION_SECRET);
    // Everything the browser received from the server: every response, every header, the whole stream.
    traffic.restore();
    // (The recorder wraps the one global fetch, which the server-side agent shares here: only the runtime's answers are the browser's.)
    const browser = traffic.entries.filter((entry) => entry.url.startsWith(s.runtimeUrl));
    expect(browser.filter((entry) => entry.url.includes("/agent/default/run")).length).toBeGreaterThanOrEqual(2);
    expect(traffic.entries.some((entry) => entry.headers.includes(CONFIRMATION_SECRET))).toBe(true);
    for (const entry of browser) {
      expect(entry.headers.toLowerCase()).not.toContain("confirmation");
      expect(`${entry.headers}
${await entry.body}`).not.toContain(CONFIRMATION_SECRET);
    }
    // The page joins its thread through the runtime's connect route (history from the gateway), then runs and resumes.
    expect(s.tokenRequests.filter((request) => request.operation !== "agui.connect").map((request) => request.resume)).toEqual([false, true]);
    expect(s.tokenRequests.filter((request) => request.operation === "agui.connect").every((request) => request.sessionId === s.threadId)).toBe(true);
  });

  it("decline: no secret is sent and the run ends cancelled", async () => {
    const s = await start();
    render(<Page stack={s} />);
    await send("create an item named widget with quantity 3", s);
    fireEvent.click(await screen.findByRole("button", { name: "Decline" }, { timeout: 10000 }));
    await waitFor(() => expect(s.gateway.requests.length).toBe(2), { timeout: 10000 });
    expect(s.gateway.requests[1]?.body?.["resume"]?.[0]?.payload).toEqual({ approved: false });
    expect(s.gateway.requests[1]?.headers[SECRET_HEADER]).toBeUndefined();
    await waitForText(/stopped that request/);
  });

  it("a stale preparation: the run ends in error and the page says so", async () => {
    const s = await start();
    s.gateway.queue({ golden: "prepare_interrupt" }, { golden: "stale" });
    render(<Page stack={s} />);
    await send("create an item named widget with quantity 3", s);
    fireEvent.click(await screen.findByRole("button", { name: "Approve" }, { timeout: 10000 }));
    await waitForText(/information is out of date/);
  });
});

describe("client data is passed through as data", () => {
  const Declares = (): null => {
    useAgentContext({ description: "The row the user selected", value: "item-1 (gadget)" });
    useFrontendTool({
      name: "highlightRow",
      description: "Highlights a row",
      parameters: z.object({ rowId: z.string(), color: z.string().optional() }),
      handler: async () => {
        highlighted.push("called");
        return JSON.stringify({ highlighted: true, rowId: "item-1" });
      },
    });
    return null;
  };
  const highlighted: string[] = [];

  it("sends readable context and declared tools to the gateway untouched, and a frontend tool round trip answers with a tool message", async () => {
    highlighted.length = 0;
    const s = await start();
    s.gateway.queue({ golden: "ui_tool_call" });
    render(
      <Page stack={s}>
        <Declares />
      </Page>,
    );
    await send("show item item-1", s);
    await waitFor(() => expect(highlighted).toEqual(["called"]), { timeout: 10000 });
    await waitFor(() => expect(s.gateway.requests.length).toBe(2), { timeout: 10000 });
    const [first, second] = s.gateway.requests;
    expect(first?.body?.["context"]).toEqual(expect.arrayContaining([{ description: "The row the user selected", value: "item-1 (gadget)" }]));
    expect((first?.body?.["tools"] as Array<{ name: string }>).map((tool) => tool.name)).toContain("highlightRow");
    // The result of the page's own tool goes back as a tool message for the call's id, never as an approval and never with a secret.
    const last = (second?.body?.["messages"] as Array<Record<string, any>>).at(-1);
    expect(last).toMatchObject({ role: "tool", toolCallId: "alixai_ui_trn_01J00000000000000000000001" });
    expect(second?.body?.["resume"]).toBeUndefined();
    expect(second?.headers[SECRET_HEADER]).toBeUndefined();
  });
});

describe("cancel and reconnect", () => {
  it("stop: the page stops reading and the gateway is asked to cancel the run", async () => {
    const s = await start();
    s.gateway.queue({ golden: "plain_answer", holdAfterFrames: 16 });
    render(<Page stack={s} />);
    await send("show item item-1", s);
    await waitForText(/The item is named gadget/);
    // While a run is in flight the send button is the stop button.
    fireEvent.click(screen.getByTestId("copilot-send-button"));
    await waitFor(() => expect(s.gateway.cancelled.length).toBe(1), { timeout: 10000 });
    expect(s.tokenRequests.map((request) => request.operation)).toContain("agui.cancel");
  });

  it("a cut stream is picked up again from its cursor and the answer is complete once", async () => {
    const s = await start();
    s.gateway.queue({ golden: "plain_answer", cutAfterFrames: 12 });
    render(<Page stack={s} />);
    await send("show item item-1", s);
    await waitForText(/The item is named gadget\./);
    expect(text().split("The item is named gadget.").length - 1).toBe(1);
    expect(s.gateway.requests.map((request) => request.method)).toEqual(["POST", "GET"]);
  });
});
