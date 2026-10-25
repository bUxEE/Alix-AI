// @vitest-environment node
import { createSSRApp, h } from "vue";
import { renderToString } from "vue/server-renderer";
import { describe, expect, it } from "vitest";
import { AlixChatPanel, AlixConfirmationSheet, AlixConversationList, AlixMessageStream, AlixProvider, AlixSettingsPanel, createAguiClient } from "../src/index";

// SSR safety: in a plain Node environment (no window, no document, no navigator) every shipped surface renders on the server.
// Nothing may touch the browser at import time or in setup, and nothing may start a request: the history loads on the client.

describe("server-side rendering", () => {
  it("has no browser globals here", () => {
    expect(typeof window).toBe("undefined");
    expect(typeof document).toBe("undefined");
  });

  it("renders the chat panel without fetching anything", async () => {
    let requests = 0;
    const client = createAguiClient({
      endpoint: "/v1/agui",
      fetch: async () => {
        requests += 1;
        throw new Error("no request during server rendering");
      },
    });
    const html = await renderToString(createSSRApp({ render: () => h(AlixProvider, { locale: "it" }, { default: () => h(AlixChatPanel, { client, threadId: "ses_1" }) }) }));
    expect(html).toContain("Assistente");
    expect(html).toContain("alix-root");
    expect(requests).toBe(0);
  });

  it("renders the other surfaces with their default state", async () => {
    const confirmation = { preparationId: "p", capabilityId: "c", summary: "Book it", effects: [], riskLevel: 1, expiresAt: "2030-01-01T10:00:00Z", mode: "explicit" };
    for (const component of [
      h(AlixMessageStream, { items: [{ kind: "message", id: "m", role: "assistant", text: "**hi**", streaming: false }] }),
      h(AlixConfirmationSheet, { confirmation }),
      h(AlixConversationList, { loading: true }),
      h(AlixSettingsPanel, { loading: true }),
    ]) {
      const html = await renderToString(createSSRApp({ render: () => component }));
      expect(html.length).toBeGreaterThan(20);
    }
  });
});
