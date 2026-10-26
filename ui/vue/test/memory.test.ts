import { mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";
import { h, nextTick } from "vue";

import { AlixMemoryList, AlixMemoryProposal, AlixProvider, applyEvent, createChatState, parsers } from "../src/index";

const wrap = (component: unknown, props: Record<string, unknown> = {}) =>
  mount(AlixProvider, { slots: { default: () => h(component as object, props) } });

describe("AlixMemoryProposal", () => {
  it("emits confirm once and disables when expired", async () => {
    const wrapper = wrap(AlixMemoryProposal, {
      proposal: { id: "mem_01J0000000000000000000000A", text: "Prefer dark mode" },
      status: "pending",
    });
    const card = wrapper.findComponent(AlixMemoryProposal);
    await card.findAll("button")[0]!.trigger("click");
    expect(card.emitted("confirm")).toHaveLength(1);
    await card.findAll("button")[1]!.trigger("click");
    expect(card.emitted("reject")).toBeUndefined();

    const expired = wrap(AlixMemoryProposal, {
      proposal: { id: "mem_01J0000000000000000000000A", text: "Prefer dark mode", expiresAt: "2000-01-01T00:00:00Z" },
      status: "pending",
    });
    expect(expired.text()).toMatch(/expired|scaduta/i);
    const expiredCard = expired.findComponent(AlixMemoryProposal);
    await expiredCard.findAll("button")[0]!.trigger("click");
    expect(expiredCard.emitted("confirm")).toBeUndefined();
  });

  it("shows busy label while submitting", async () => {
    const wrapper = wrap(AlixMemoryProposal, {
      proposal: { id: "mem_01J0000000000000000000000A", text: "note" },
      status: "submitting",
    });
    await nextTick();
    expect(wrapper.text()).toMatch(/Saving|Salvataggio/);
  });
});

describe("AlixMemoryList", () => {
  it("shows empty state and emits delete", async () => {
    const empty = wrap(AlixMemoryList, { items: [], loading: false });
    expect(empty.text()).toMatch(/No memories|Nessuna memoria/);

    const wrapper = wrap(AlixMemoryList, {
      items: [{ id: "mem_01J0000000000000000000000A", text: "Prefer dark mode", status: "active" }],
      loading: false,
    });
    await wrapper.find('button[aria-label="Delete memory"]').trigger("click");
    expect(wrapper.findComponent(AlixMemoryList).emitted("delete")?.[0]).toEqual(["mem_01J0000000000000000000000A"]);
  });
});

describe("memory_proposal custom event", () => {
  it("is ignored when memory is off and rendered when on", () => {
    const off = createChatState();
    applyEvent(off, {
      type: "CUSTOM",
      name: "alixai.memory_proposal",
      value: { memory: { id: "mem_01J0000000000000000000000A", text: "note" } },
    });
    expect(off.items).toEqual([]);

    const on = createChatState({ memory: true });
    applyEvent(on, {
      type: "CUSTOM",
      name: "alixai.memory_proposal",
      value: { memory: { id: "mem_01J0000000000000000000000A", text: "note", expires_at: "2099-01-01T00:00:00Z" } },
    });
    expect(on.items).toHaveLength(1);
    expect(on.items[0]).toMatchObject({ kind: "memory_proposal", proposal: { id: "mem_01J0000000000000000000000A", text: "note" } });
  });

  it("parses the custom event value", () => {
    expect(parsers.parseMemoryProposal({ memory: { id: "mem_01J0000000000000000000000A", text: "x" } })).toEqual({
      id: "mem_01J0000000000000000000000A",
      text: "x",
    });
    expect(parsers.parseMemoryProposal({})).toBeUndefined();
  });
});
