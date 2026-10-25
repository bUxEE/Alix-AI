import { mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";
import { h } from "vue";
import type { ChatItem } from "../src/agui/types";
import { AlixMessageStream } from "../src/index";
import type { DiagramBlock } from "../src/sdk-types";

// A host with a diagram renderer fills the stream's `diagram` slot (forwarded by the panel); without one the block keeps its
// text alternative and its source as text, and nothing else in the timeline changes.

const block = {
  type: "diagram",
  language: "mermaid",
  requires_host_renderer: true,
  title: "Work-order process",
  description: "A request is triaged, scheduled and closed.",
  source: "flowchart LR\n  A[Request] --> B[Triage]",
} as unknown as DiagramBlock;

const items: ChatItem[] = [
  { kind: "block", id: "act_diagram", blockType: "diagram", block, answered: false },
  { kind: "message", id: "a1", role: "assistant", text: "Here it is.", streaming: false },
];

describe("diagram slot", () => {
  it("keeps the source as text when the host has no renderer", () => {
    const wrapper = mount(AlixMessageStream, { props: { items } });
    expect(wrapper.find("pre code").text()).toContain("flowchart LR");
    expect(wrapper.text()).toContain("A request is triaged, scheduled and closed.");
  });

  it("hands the block to the host's renderer, inside the kit's figure with its title", () => {
    const wrapper = mount(AlixMessageStream, {
      props: { items },
      slots: { diagram: (scope: Record<string, unknown>) => h("svg", { class: "host-diagram", "data-source": (scope["block"] as DiagramBlock).source }) },
    });
    expect(wrapper.find(".host-diagram").attributes("data-source")).toContain("A[Request]");
    expect(wrapper.find("figcaption").text()).toBe("Work-order process");
    expect(wrapper.find("pre").exists()).toBe(false);
    expect(wrapper.text()).toContain("Here it is.");
  });
});
