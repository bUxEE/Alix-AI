import { flushPromises, mount } from "@vue/test-utils";
import { describe, expect, it } from "vitest";
import type { AguiClient, RunHandlers } from "../src/agui/client";
import { AlixChatPanel, AlixProvider } from "../src/index";
import { ev, runFinished, runStarted } from "./helpers";
import { h } from "vue";

// While a stage runs, the panel names it and, when the stream reports how far it is (`alixai.progress` with `percent`), shows
// the percentage in words and as a progress bar.

const heldClient = (): { client: AguiClient; emit: (...events: ReturnType<typeof ev>[]) => void; finish: () => void } => {
  let handlers: RunHandlers | undefined;
  let done: (() => void) | undefined;
  const client: AguiClient = {
    run: (input, runHandlers) => {
      if (input.messages.length === 0) {
        runHandlers.onEvent(runStarted(), undefined);
        runHandlers.onEvent(runFinished(), undefined);
        return Promise.resolve({ terminal: true, aborted: false, reconnects: 0 });
      }
      handlers = runHandlers;
      return new Promise((resolve) => {
        done = () => resolve({ terminal: true, aborted: false, reconnects: 0 });
      });
    },
    cancel: async () => true,
    capabilities: async () => ({}),
  };
  return {
    client,
    emit: (...events) => {
      for (const event of events) handlers?.onEvent(event, undefined);
    },
    finish: () => {
      handlers?.onEvent(runFinished(), undefined);
      done?.();
    },
  };
};

const progress = (members: Record<string, unknown>) =>
  ev("ACTIVITY_SNAPSHOT", { messageId: "act_progress", activityType: "alixai.progress", content: { stage: "tool_call", state: "running", ...members } });

const start = async (locale = "en") => {
  const held = heldClient();
  const wrapper = mount(AlixProvider, { props: { locale }, slots: { default: () => h(AlixChatPanel, { client: held.client, threadId: "ses_1" }) } });
  await flushPromises();
  await wrapper.find("textarea").setValue("export the report");
  await wrapper.find("form").trigger("submit");
  await flushPromises();
  held.emit(runStarted(), ev("STEP_STARTED", { stepName: "tool_call" }));
  await flushPromises();
  return { held, wrapper };
};

describe("AlixChatPanel progress", () => {
  it("names the stage only, with no bar, while no percentage is known", async () => {
    const { held, wrapper } = await start();
    held.emit(progress({}));
    await flushPromises();
    expect(wrapper.text()).toContain("Working on it: Asking the application");
    expect(wrapper.find("[role='progressbar']").exists()).toBe(false);
    held.finish();
    wrapper.unmount();
  });

  it("shows the percentage in words and as a bar, follows its updates, and clears when the run ends", async () => {
    const { held, wrapper } = await start();
    held.emit(progress({ percent: 31 }));
    await flushPromises();
    expect(wrapper.text()).toContain("Working on it: Asking the application, 31%");
    const bar = wrapper.find("[role='progressbar']");
    expect(bar.attributes("aria-valuenow")).toBe("31");
    expect(bar.attributes("aria-label")).toBe("Progress");
    held.emit(ev("ACTIVITY_DELTA", { messageId: "act_progress", activityType: "alixai.progress", patch: [{ op: "replace", path: "/percent", value: 78.6 }] }));
    await flushPromises();
    expect(wrapper.find("[role='progressbar']").attributes("aria-valuenow")).toBe("79");
    held.finish();
    await flushPromises();
    expect(wrapper.find("[role='progressbar']").exists()).toBe(false);
    wrapper.unmount();
  });

  it("says it in Italian", async () => {
    const { held, wrapper } = await start("it");
    held.emit(progress({ percent: 140 }));
    await flushPromises();
    expect(wrapper.text()).toContain("Ci sto lavorando: Interrogazione dell'applicazione, 100%");
    held.finish();
    wrapper.unmount();
  });
});

describe("AlixChatPanel progress words", () => {
  it("shows a stage the schema does not name as it is, made readable", async () => {
    const { held, wrapper } = await start();
    held.emit(ev("STEP_STARTED", { stepName: "custom_host-step" }));
    await flushPromises();
    expect(wrapper.text()).toContain("Working on it: custom host step");
    wrapper.unmount();
  });
});
