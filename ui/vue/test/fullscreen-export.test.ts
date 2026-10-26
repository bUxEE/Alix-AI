import { mount } from "@vue/test-utils";
import { describe, expect, it, vi } from "vitest";
import { h, nextTick, ref } from "vue";

import { AlixChatPanel, AlixExportMenu, AlixMessageStream, AlixProvider, type AlixExporter } from "../src/index";
import type { ChatItem } from "../src/agui/types";
import { scriptedClient } from "./helpers";

const wrap = (component: unknown, props: Record<string, unknown> = {}, slots: Record<string, unknown> = {}) =>
  mount(AlixProvider, {
    attachTo: document.body,
    slots: { default: () => h(component as object, props, slots) },
  });

describe("fullscreen overlay", () => {
  it("toggles fullscreen without remounting the composer draft", async () => {
    const client = scriptedClient(() => []);
    const wrapper = wrap(AlixChatPanel, { client, threadId: "ses_1", autoConnect: false });
    const panel = wrapper.findComponent(AlixChatPanel);
    const textarea = panel.find("textarea");
    await textarea.setValue("keep me");
    expect((textarea.element as HTMLTextAreaElement).value).toBe("keep me");

    const toggle = panel.find('button[aria-label="Full screen"]');
    expect(toggle.exists()).toBe(true);
    await toggle.trigger("click");
    await nextTick();
    // Teleport moves the section to document.body while fullscreen.
    expect(document.querySelector("section.alix-chat-fullscreen")).toBeTruthy();
    expect((document.querySelector("section.alix-chat-fullscreen textarea") as HTMLTextAreaElement).value).toBe("keep me");

    await document.querySelector<HTMLButtonElement>('button[aria-label="Exit full screen"]')!.click();
    await nextTick();
    expect(document.querySelector("section.alix-chat-fullscreen")).toBeNull();
    expect((panel.find("textarea").element as HTMLTextAreaElement).value).toBe("keep me");
  });

  it("exits on Escape and restores focus to the toggle", async () => {
    const client = scriptedClient(() => []);
    const wrapper = wrap(AlixChatPanel, { client, threadId: "ses_1", autoConnect: false });
    const panel = wrapper.findComponent(AlixChatPanel);
    const toggle = panel.find('button[aria-label="Full screen"]');
    (toggle.element as HTMLButtonElement).focus();
    await toggle.trigger("click");
    await nextTick();
    const section = document.querySelector("section.alix-chat-fullscreen");
    expect(section).toBeTruthy();
    section!.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await nextTick();
    expect(document.querySelector("section.alix-chat-fullscreen")).toBeNull();
    expect(document.activeElement).toBe(panel.find('button[aria-label="Full screen"]').element);
  });

  it("supports v-model:fullscreen", async () => {
    const client = scriptedClient(() => []);
    const fullscreen = ref(false);
    const Host = {
      setup: () => () =>
        h(AlixProvider, null, {
          default: () =>
            h(AlixChatPanel, {
              client,
              threadId: "ses_1",
              autoConnect: false,
              fullscreen: fullscreen.value,
              "onUpdate:fullscreen": (value: boolean) => {
                fullscreen.value = value;
              },
            }),
        }),
    };
    const wrapper = mount(Host, { attachTo: document.body });
    await wrapper.find('button[aria-label="Full screen"]').trigger("click");
    expect(fullscreen.value).toBe(true);
    wrapper.unmount();
  });
});

describe("export menus", () => {
  const tableItem: ChatItem = {
    kind: "block",
    id: "blk_table",
    blockType: "table",
    answered: false,
    turnId: "run_1",
    block: {
      type: "table",
      columns: [{ key: "n", label: "N", kind: "number" }],
      rows: [[1]],
    },
  };

  it("hides download actions without an exporter", () => {
    const wrapper = wrap(AlixMessageStream, {
      items: [tableItem],
      exports: true,
      sessionId: "ses_1",
    });
    expect(wrapper.find('[aria-label="Download this data"]').exists()).toBe(false);
  });

  it("shows loading and error states on the control", async () => {
    const exporter: AlixExporter = vi.fn(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20));
      throw new Error("boom");
    });
    const wrapper = wrap(AlixExportMenu, {
      exporter,
      label: "Download this data",
      formats: ["csv", "xlsx"],
      params: { sessionId: "ses_1", turnId: "run_1", blockIds: ["blk_table"] },
    });
    await wrapper.find("button").trigger("click");
    await wrapper.find('[role="menuitem"]').trigger("click");
    expect(wrapper.find("button").attributes("aria-busy")).toBe("true");
    await new Promise((resolve) => setTimeout(resolve, 40));
    await nextTick();
    expect(wrapper.find("[role='alert']").exists()).toBe(true);
  });

  it("calls the host exporter and download for a file id", async () => {
    const download = vi.fn(async () => undefined);
    const exporter: AlixExporter = vi.fn(async () => ({ fileId: "file_1", name: "rows.csv" }));
    const wrapper = wrap(AlixMessageStream, {
      items: [tableItem],
      exports: true,
      exporter,
      download,
      sessionId: "ses_1",
    });
    await wrapper.find('[aria-label="Download this data"]').trigger("click");
    await wrapper.findAll('[role="menuitem"]')[0]!.trigger("click");
    await nextTick();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(exporter).toHaveBeenCalledWith({ sessionId: "ses_1", turnId: "run_1", blockIds: ["blk_table"], format: "csv" });
    expect(download).toHaveBeenCalledWith("file_1");
  });
});
