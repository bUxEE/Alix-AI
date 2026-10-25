import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";
import { h } from "vue";
import type { ChatItem } from "../src/agui/types";
import { AlixChatPanel, AlixMessageStream } from "../src/index";
import type { ConfirmationPanelBlock } from "../src/sdk-types";
import { ev, runFinished, runStarted, scriptedClient } from "./helpers";

// The `confirmation_panel` block is drawn with the kit's confirmation sheet, and a prepared action is presented once: the
// panel and the interrupt (or the step-up hand-over) for the same preparation never give two answerable surfaces.

const effects: ConfirmationPanelBlock["effects"] = [
  {
    operation: "create",
    entity_type: "work_order",
    description: "New P2 work order: replace the HVAC filters on level 3",
    changes: [{ field: "priority", value: { kind: "text", value: "P2" } }],
    external: false,
  },
];

const panel = (preparationId: string, mode: "explicit" | "step_up" = "explicit"): ConfirmationPanelBlock => ({
  type: "confirmation_panel",
  preparation_id: preparationId,
  summary: "Create a P2 work order at Harbor Point",
  effects,
  risk_level: mode === "step_up" ? 3 : 2,
  expires_at: "2030-01-01T10:00:00Z",
  confirmation: { mode, bindings: [] },
} as unknown as ConfirmationPanelBlock);

const panelItem = (preparationId: string, mode: "explicit" | "step_up" = "explicit"): ChatItem => ({
  kind: "block",
  id: `act_panel_${preparationId}`,
  blockType: "confirmation_panel",
  block: panel(preparationId, mode),
  answered: false,
});

const confirmationItem = (preparationId: string): ChatItem => ({
  kind: "confirmation",
  id: `confirm_${preparationId}`,
  interruptId: preparationId,
  status: "pending",
  confirmation: {
    preparationId,
    capabilityId: "workorders.create",
    summary: "Create a P2 work order at Harbor Point",
    effects,
    riskLevel: 2,
    expiresAt: "2030-01-01T10:00:00Z",
    mode: "explicit",
  },
});

const buttons = (wrapper: ReturnType<typeof mount>, label: string) => wrapper.findAll("button").filter((button) => button.text() === label);

describe("confirmation_panel", () => {
  it("is drawn with the summary, the effects and the risk, and offers no answer before its interrupt arrives", () => {
    const wrapper = mount(AlixMessageStream, { props: { items: [panelItem("prep_1")], running: true } });
    expect(wrapper.text()).toContain("Create a P2 work order at Harbor Point");
    expect(wrapper.text()).toContain("New P2 work order: replace the HVAC filters on level 3");
    expect(wrapper.text()).toContain("Risk level 2");
    expect(buttons(wrapper, "Confirm")).toHaveLength(0);
    expect(wrapper.text()).toContain("You can confirm this once the request is ready.");
  });

  it("takes over the interrupt's sheet: one Confirm, answered with the confirmation entry's id", async () => {
    const items: ChatItem[] = [
      panelItem("prep_1"),
      { kind: "message", id: "a1", role: "assistant", text: "Please confirm it.", streaming: false },
      confirmationItem("prep_1"),
    ];
    const wrapper = mount(AlixMessageStream, { props: { items } });
    expect(buttons(wrapper, "Confirm")).toHaveLength(1);
    expect(buttons(wrapper, "Decline")).toHaveLength(1);
    expect(wrapper.findAll("section[role='group']")).toHaveLength(1);
    await buttons(wrapper, "Confirm")[0]?.trigger("click");
    expect(wrapper.emitted("approve")).toEqual([["confirm_prep_1"]]);
    // The sheet sits where the panel is: before the assistant's text.
    const html = wrapper.html();
    expect(html.indexOf("Risk level 2")).toBeLessThan(html.indexOf("Please confirm it."));
  });

  it("shows the answered state of the bound confirmation", () => {
    const answered = { ...(confirmationItem("prep_1") as Extract<ChatItem, { kind: "confirmation" }>), status: "approved" as const };
    const wrapper = mount(AlixMessageStream, { props: { items: [panelItem("prep_1"), answered] } });
    expect(buttons(wrapper, "Confirm")).toHaveLength(0);
    expect(wrapper.text()).toContain("Confirmed");
  });

  it("presents a step-up action once: the panel's sheet hands over with the continue entry's id", async () => {
    const items: ChatItem[] = [
      { kind: "continue", id: "alixai_confirm_prep_9", summary: "End the cleaning contract", preparationId: "prep_9" },
      panelItem("prep_9", "step_up"),
    ];
    const wrapper = mount(AlixMessageStream, { props: { items } });
    const cta = buttons(wrapper, "Continue in the application");
    expect(cta).toHaveLength(1);
    expect(wrapper.text()).toContain("Risk level 3");
    await cta[0]?.trigger("click");
    expect(wrapper.emitted("continue-in-app")).toEqual([["alixai_confirm_prep_9"]]);
  });

  it("leaves every entry as before when the host draws the blocks itself", () => {
    const wrapper = mount(AlixMessageStream, {
      props: { items: [panelItem("prep_1"), confirmationItem("prep_1")] },
      slots: { block: () => h("p", { class: "host-block" }, "host block") },
    });
    expect(wrapper.find(".host-block").exists()).toBe(true);
    expect(buttons(wrapper, "Confirm")).toHaveLength(1);
  });

  it("uses the host's confirmation slot for the bound sheet", () => {
    const wrapper = mount(AlixMessageStream, {
      props: { items: [panelItem("prep_1"), confirmationItem("prep_1")] },
      slots: { confirmation: (scope: Record<string, unknown>) => h("p", { class: "host-confirm" }, (scope["item"] as { id: string }).id) },
    });
    expect(wrapper.findAll(".host-confirm").map((node) => node.text())).toEqual(["confirm_prep_1"]);
  });

  it("over a real stream: the block, then the interrupt, give one sheet that resumes the interrupt", async () => {
    const meta = { preparationId: "prep_1", capabilityId: "workorders.create", summary: "Create a P2 work order at Harbor Point", effects, riskLevel: 2, expiresAt: "2030-01-01T10:00:00Z", confirmation: { mode: "explicit" } };
    const client = scriptedClient((input) => {
      if (input.messages.length === 0 && input.resume === undefined) return [runStarted(), runFinished()];
      if (input.resume !== undefined) return [runStarted(), runFinished()];
      return [
        runStarted(),
        ev("ACTIVITY_SNAPSHOT", { messageId: "act_panel", activityType: "alixai.block.confirmation_panel", content: panel("prep_1") }),
        runFinished({ type: "interrupt", interrupts: [{ id: "prep_1", reason: "confirmation_required", metadata: meta }] }),
      ];
    });
    const wrapper = mount(AlixChatPanel, { props: { client, threadId: "ses_1" } });
    await flushPromises();
    await wrapper.find("textarea").setValue("replace the filters");
    await wrapper.find("form").trigger("submit");
    await flushPromises();
    expect(buttons(wrapper, "Confirm")).toHaveLength(1);
    await buttons(wrapper, "Confirm")[0]?.trigger("click");
    await flushPromises();
    expect(client.inputs.at(-1)?.resume).toEqual([{ interruptId: "prep_1", status: "resolved", payload: { approved: true } }]);
    expect(wrapper.text()).toContain("Confirmed");
    wrapper.unmount();
  });
});
