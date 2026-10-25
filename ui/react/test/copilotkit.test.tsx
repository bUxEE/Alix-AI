// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import type { ReactElement } from "react";
import { HttpAgent } from "@ag-ui/client";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ALIXAI_ACTIVITY, blockActivityType } from "@alixai/copilotkit-adapter";
import { AlixChatPanel, alixKitComponents, alixProviderProps } from "../src/copilotkit.js";
import { actionCardBlock, attachment, chartBlock, citation, tableBlock } from "./fixtures.js";

afterEach(cleanup);

type Renderer = { activityType: string; content: { "~standard": { validate: (value: unknown) => { value?: unknown; issues?: unknown } } }; render: (props: { content: unknown; agent: undefined }) => ReactElement | null };

const renderers = (props: object = {}, options = {}): Renderer[] => (alixProviderProps(props, options) as unknown as { renderActivityMessages: Renderer[] }).renderActivityMessages;

describe("the kit as CopilotKit renderers", () => {
  it("registers a renderer for every activity AlixAI sends", () => {
    const types = renderers().map((renderer) => renderer.activityType);
    for (const expected of [
      blockActivityType("table"),
      blockActivityType("chart"),
      blockActivityType("diagram"),
      blockActivityType("action_card"),
      blockActivityType("clarification_choices"),
      blockActivityType("citation_list"),
      ALIXAI_ACTIVITY.citations,
      ALIXAI_ACTIVITY.attachments,
      ALIXAI_ACTIVITY.progress,
    ]) {
      expect(types).toContain(expected);
    }
  });

  it("draws a validated payload with the kit's components", () => {
    const byType = new Map(renderers().map((renderer) => [renderer.activityType, renderer]));
    const draw = (type: string, content: unknown): HTMLElement => {
      const renderer = byType.get(type)!;
      const parsed = renderer.content["~standard"].validate(content) as { value: unknown };
      const { container } = render(<>{renderer.render({ content: parsed.value, agent: undefined })}</>);
      return container;
    };
    expect(draw(blockActivityType("table"), tableBlock()).querySelector(".alix-table")).not.toBeNull();
    expect(draw(blockActivityType("chart"), chartBlock()).querySelector(".alix-chart")).not.toBeNull();
    expect(draw(blockActivityType("action_card"), actionCardBlock()).querySelector(".alix-action-card")).not.toBeNull();
    expect(draw(ALIXAI_ACTIVITY.citations, { citations: [citation()] }).querySelector(".alix-citations")).not.toBeNull();
    expect(draw(ALIXAI_ACTIVITY.attachments, { files: [attachment()] }).querySelector(".alix-files")).not.toBeNull();
  });

  it("refuses a payload that is not what AlixAI sends, so nothing is drawn on trust", () => {
    const table = renderers().find((renderer) => renderer.activityType === blockActivityType("table"))!;
    expect("issues" in (table.content["~standard"].validate({ type: "table", columns: "x" }) as object)).toBe(true);
    const citations = renderers().find((renderer) => renderer.activityType === ALIXAI_ACTIVITY.citations)!;
    expect("issues" in (citations.content["~standard"].validate({ citations: [{ display_name: 3 }] }) as object)).toBe(true);
  });

  it("shows progress only when asked, and lets a host component win", () => {
    const progress = (options: object): string => {
      const element = renderers({}, options).find((renderer) => renderer.activityType === ALIXAI_ACTIVITY.progress)!.render({ content: { stage: "Reading" }, agent: undefined });
      const { container, unmount } = render(<>{element}</>);
      const html = container.innerHTML;
      unmount();
      return html;
    };
    expect(progress({})).toBe("");
    expect(progress({ showProgress: true })).toContain("Reading");
    const own = () => <p data-testid="own">own</p>;
    expect(alixKitComponents({ components: { [blockActivityType("table")]: own as never } })[blockActivityType("table")]).toBe(own);
  });

  it("removes cloud keys and keeps the inspector and dev console off", () => {
    const props = alixProviderProps({ runtimeUrl: "/api/copilotkit", publicApiKey: "ck_live_x", publicLicenseKey: "lic", licenseToken: "tok" }) as unknown as Record<string, unknown>;
    expect(props["runtimeUrl"]).toBe("/api/copilotkit");
    for (const key of ["publicApiKey", "publicLicenseKey", "licenseToken"]) expect(key in props).toBe(false);
    expect(props["enableInspector"]).toBe(false);
    expect(props["showDevConsole"]).toBe(false);
  });
});

describe("the chat panel", () => {
  it("is CopilotKit's chat inside a themed, labelled region, in the language asked for", async () => {
    // A loopback address nothing listens on: the network guard of this suite allows only loopback, and the chat shows without a runtime.
    render(<AlixChatPanel runtimeUrl="http://127.0.0.1:9/api/copilotkit" agentId="default" locale="it" theme="dark" />);
    const region = screen.getByRole("region", { name: "Assistente AlixAI" });
    expect(region.getAttribute("data-alixai-theme")).toBe("dark");
    expect(region.getAttribute("lang")).toBe("it");
    expect(region.classList.contains("alix-root")).toBe(true);
    await waitFor(() => expect(screen.getByPlaceholderText("Chiedi qualcosa")).toBeTruthy());
  });

  it("draws a bare address in the assistant's text as text, and a link the core allowed as a link", async () => {
    // The core reads Markdown as CommonMark, where a bare address is text: it lets "https://evil.example/login" through even when
    // it allows no link scheme at all. Streamdown's GFM would make it a link; the kit draws it as the text it is.
    const content =
      "Sign in at https://evil.example/login or https://evil.example, write to help@evil.example or visit www.evil.example. " +
      "See [the docs](<https://example.com/a>), <https://example.com/b> and [example.com](<http://example.com>).";
    const agent = new HttpAgent({ url: "http://127.0.0.1:9/agui", initialMessages: [{ id: "m1", role: "assistant", content }] });
    const errors = vi.spyOn(console, "error").mockImplementation(() => undefined);
    try {
      render(<AlixChatPanel runtimeUrl="http://127.0.0.1:9/api/copilotkit" providerProps={{ selfManagedAgents: { default: agent } }} />);
      const paragraph = await waitFor(() => {
        const found = [...document.querySelectorAll("p")].find((p) => p.textContent?.startsWith("Sign in at"));
        expect(found).toBeTruthy();
        return found as HTMLParagraphElement;
      });
      expect(paragraph.textContent).toBe(
        "Sign in at https://evil.example/login or https://evil.example, write to help@evil.example or visit www.evil.example. See the docs, https://example.com/b and example.com.",
      );
      // Only the bare addresses are text: what the core wrote as a link (`[label](<dest>)` or `<url>`) stays one.
      const links = [...paragraph.querySelectorAll("a")];
      expect(links.map((link) => [link.textContent, link.getAttribute("href")])).toEqual([
        ["the docs", "https://example.com/a"],
        ["https://example.com/b", "https://example.com/b"],
        ["example.com", "http://example.com/"],
      ]);
      expect(links[0]?.getAttribute("rel")).toContain("noreferrer");
      expect(links[0]?.getAttribute("target")).toBe("_blank");
    } finally {
      errors.mockRestore();
    }
  });

  it("is safe on its own: links only to http, https and mailto, no image and no raw HTML, whatever reached it", async () => {
    // The core strips HTML and images and allows only safe links; the kit does not count on it. This text is what a core that
    // let everything through would send.
    const content =
      "Start [script](javascript:alert(1)) [data](data:text/html;base64,PHNjcmlwdD4=) [rel](/admin) " +
      "![pixel](https://evil.example/p.png) <img src=x onerror=alert(1)> <a href=\"https://evil.example/raw\">raw</a> " +
      "[ok](<https://example.com/ok>) [mail](<mailto:help@example.com>) end";
    const agent = new HttpAgent({ url: "http://127.0.0.1:9/agui", initialMessages: [{ id: "m1", role: "assistant", content }] });
    const errors = vi.spyOn(console, "error").mockImplementation(() => undefined);
    try {
      render(<AlixChatPanel runtimeUrl="http://127.0.0.1:9/api/copilotkit" providerProps={{ selfManagedAgents: { default: agent } }} />);
      const paragraph = await waitFor(() => {
        const found = [...document.querySelectorAll("p")].find((p) => p.textContent?.startsWith("Start"));
        expect(found).toBeTruthy();
        return found as HTMLParagraphElement;
      });
      const links = [...paragraph.querySelectorAll("a")];
      expect(links.map((link) => [link.textContent, link.getAttribute("href")])).toEqual([
        ["ok", "https://example.com/ok"],
        ["mail", "mailto:help@example.com"],
      ]);
      expect(document.querySelectorAll("img").length).toBe(0);
      expect(document.querySelector("[onerror]")).toBeNull();
      expect(paragraph.textContent).toContain("script");
      expect(paragraph.textContent).toContain("pixel");
      expect(paragraph.innerHTML).not.toContain("javascript:");
      expect(paragraph.innerHTML).not.toContain("evil.example");
    } finally {
      errors.mockRestore();
    }
  });

  it("keeps the renderers it gives CopilotKit stable across renders, so drawn activities keep their state", async () => {
    const errors = vi.spyOn(console, "error").mockImplementation(() => undefined);
    try {
      const onOpenCitation = (): void => undefined;
      const { rerender } = render(<AlixChatPanel runtimeUrl="http://127.0.0.1:9/api/copilotkit" onOpenCitation={onOpenCitation} />);
      await waitFor(() => expect(screen.getByRole("region")).toBeTruthy());
      rerender(<AlixChatPanel runtimeUrl="http://127.0.0.1:9/api/copilotkit" onOpenCitation={onOpenCitation} />);
      rerender(<AlixChatPanel runtimeUrl="http://127.0.0.1:9/api/copilotkit" onOpenCitation={onOpenCitation} />);
      expect(errors.mock.calls.map((call) => String(call[0])).filter((message) => /must be a stable array/.test(message))).toEqual([]);
    } finally {
      errors.mockRestore();
    }
  });
});
