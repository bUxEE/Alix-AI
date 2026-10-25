import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";
import { h } from "vue";
import type { ChatItem } from "../src/agui/types";
import { AlixChatPanel } from "../src/index";
import { ev, runFinished, runStarted, scriptedClient } from "./helpers";

// The panel passes the timeline's slots through to AlixMessageStream, so a host can draw one kind of entry (image attachments,
// for one) without rebuilding the chat from useAlixChat. A slot the host does not fill keeps the stream's own rendering.

const attachments = {
  files: [{ fileId: "file_plan", name: "plan.png", mimeType: "image/png", sizeBytes: 2048, previewable: true }],
};

const client = () =>
  scriptedClient((input) =>
    input.messages.length === 0
      ? [runStarted(), runFinished()]
      : [
          runStarted(),
          ev("ACTIVITY_SNAPSHOT", { messageId: "act_files", activityType: "alixai.attachments", content: attachments }),
          ev("TEXT_MESSAGE_START", { messageId: "a1" }),
          ev("TEXT_MESSAGE_CONTENT", { messageId: "a1", delta: "Here is the plan." }),
          ev("TEXT_MESSAGE_END", { messageId: "a1" }),
          runFinished(),
        ],
  );

const send = async (wrapper: ReturnType<typeof mount>): Promise<void> => {
  await wrapper.find("textarea").setValue("show the plan");
  await wrapper.find("form").trigger("submit");
  await flushPromises();
};

describe("AlixChatPanel forwards the timeline's slots", () => {
  it("keeps the default file card when the host fills no attachments slot", async () => {
    const wrapper = mount(AlixChatPanel, { props: { client: client(), threadId: "ses_1" } });
    await flushPromises();
    await send(wrapper);
    expect(wrapper.find("figure").text()).toContain("plan.png");
    expect(wrapper.find(".host-image").exists()).toBe(false);
    wrapper.unmount();
  });

  it("hands the host's attachments slot the entry, and leaves the other entries to the stream", async () => {
    const seen: ChatItem[] = [];
    const wrapper = mount(AlixChatPanel, {
      props: { client: client(), threadId: "ses_1" },
      slots: {
        attachments: (scope: Record<string, unknown>) => {
          const item = scope["item"] as ChatItem;
          seen.push(item);
          return h("img", { class: "host-image", alt: item.kind === "attachments" ? item.files[0]?.name : "" });
        },
      },
    });
    await flushPromises();
    await send(wrapper);
    expect(wrapper.find("img.host-image").attributes("alt")).toBe("plan.png");
    expect(seen.at(-1)).toMatchObject({ kind: "attachments", id: "act_files" });
    expect(wrapper.text()).toContain("Here is the plan.");
    wrapper.unmount();
  });

  it("forwards the empty state and does not leak its own slots into the stream", async () => {
    const wrapper = mount(AlixChatPanel, {
      props: { client: client(), threadId: "ses_1" },
      slots: { empty: () => h("p", { class: "host-empty" }, "Ask about your sites"), title: () => h("h2", { class: "host-title" }, "Northwind") },
    });
    await flushPromises();
    expect(wrapper.find(".host-empty").text()).toBe("Ask about your sites");
    expect(wrapper.findAll(".host-title")).toHaveLength(1);
    wrapper.unmount();
  });
});
