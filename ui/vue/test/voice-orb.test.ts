import { mount } from "@vue/test-utils";
import type * as OrbModule from "@alixai/voice-orb";
import { h } from "vue";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AlixProvider, AlixVoiceMode, AlixVoiceOrb } from "../src/index";

// The orb component over the real @alixai/voice-orb, spied on: jsdom has no WebGL, so the orb takes its Canvas 2D path with a
// fake context, and every call the component makes to the orb is recorded.

interface Recorded {
  options: OrbModule.VoiceOrbOptions;
  setState: ReturnType<typeof vi.fn>;
  setLevel: ReturnType<typeof vi.fn>;
  setColors: ReturnType<typeof vi.fn>;
  setReducedMotion: ReturnType<typeof vi.fn>;
  destroy: ReturnType<typeof vi.fn>;
}

const orbs = vi.hoisted(() => [] as Recorded[]);

vi.mock("@alixai/voice-orb", async (importOriginal) => {
  const real = await importOriginal<typeof OrbModule>();
  return {
    ...real,
    createVoiceOrb: (host: HTMLElement, options: OrbModule.VoiceOrbOptions) => {
      const orb = real.createVoiceOrb(host, options);
      const recorded: Recorded = {
        options,
        setState: vi.fn(orb.setState),
        setLevel: vi.fn(orb.setLevel),
        setColors: vi.fn(orb.setColors),
        setReducedMotion: vi.fn(orb.setReducedMotion),
        destroy: vi.fn(orb.destroy),
      };
      orbs.push(recorded);
      return {
        ...recorded,
        resize: orb.resize,
        get renderer() {
          return orb.renderer;
        },
        get state() {
          return orb.state;
        },
      };
    },
  };
});

const fake2d = (): Record<string, unknown> => ({
  clearRect: () => undefined,
  fillRect: () => undefined,
  createRadialGradient: () => ({ addColorStop: () => undefined }),
  save: () => undefined,
  restore: () => undefined,
  beginPath: () => undefined,
  moveTo: () => undefined,
  lineTo: () => undefined,
  closePath: () => undefined,
  clip: () => undefined,
});

const withCanvas2d = (): void => {
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation(((kind: string) => (kind === "2d" ? fake2d() : null)) as never);
};

const latest = (): Recorded => {
  const orb = orbs[orbs.length - 1];
  if (orb === undefined) throw new Error("no orb was created");
  return orb;
};

afterEach(() => {
  orbs.length = 0;
  vi.restoreAllMocks();
  document.body.innerHTML = "";
});

describe("AlixVoiceOrb", () => {
  it("paints with Canvas 2D where there is no WebGL, and is an image named after its state", async () => {
    withCanvas2d();
    const wrapper = mount(AlixVoiceOrb, { props: { state: "listening", size: 96 }, attachTo: document.body });
    await wrapper.vm.$nextTick();
    const root = wrapper.find(".alix-voice-orb");
    expect(root.attributes("role")).toBe("img");
    expect(root.attributes("aria-label")).toBe("Voice assistant, listening");
    expect(root.attributes("data-renderer")).toBe("2d");
    expect(root.attributes("style")).toContain("width: 96px");
    expect(root.find("canvas").attributes("aria-hidden")).toBe("true");
    await wrapper.setProps({ label: "Alix" });
    expect(root.attributes("aria-label")).toBe("Alix");
    wrapper.unmount();
  });

  it("names its state in Italian", () => {
    const wrapper = mount(AlixProvider, { props: { locale: "it" }, slots: { default: () => h(AlixVoiceOrb, { state: "thinking" }) } });
    expect(wrapper.find(".alix-voice-orb").attributes("aria-label")).toBe("Assistente vocale, sta pensando");
  });

  it("hands state, level and colours to the orb, and releases it on unmount", async () => {
    withCanvas2d();
    const wrapper = mount(AlixVoiceOrb, { props: { state: "idle", level: 0 }, attachTo: document.body });
    const orb = latest();
    expect(orb.options.state).toBe("idle");
    expect(orb.options.reducedMotion).toBe(false);
    await wrapper.setProps({ state: "speaking", level: 0.6 });
    expect(orb.setState).toHaveBeenLastCalledWith("speaking");
    expect(orb.setLevel).toHaveBeenLastCalledWith(0.6);
    await wrapper.setProps({ colors: { accent: "rebeccapurple" } });
    expect(orb.setColors).toHaveBeenLastCalledWith(expect.objectContaining({ accent: "rebeccapurple" }));
    expect(wrapper.find("canvas").exists()).toBe(true);
    wrapper.unmount();
    expect(orb.destroy).toHaveBeenCalledTimes(1);
    expect(document.querySelector("canvas")).toBeNull();
  });

  it("holds still under reduced motion: the prop, the provider or the system", async () => {
    withCanvas2d();
    const wrapper = mount(AlixVoiceOrb, { props: { reducedMotion: false }, attachTo: document.body });
    await wrapper.setProps({ reducedMotion: true });
    expect(latest().setReducedMotion).toHaveBeenLastCalledWith(true);
    wrapper.unmount();

    mount(AlixProvider, { props: { reducedMotion: true }, slots: { default: () => h(AlixVoiceOrb) }, attachTo: document.body });
    expect(latest().options.reducedMotion).toBe(true);

    // jsdom has no matchMedia: the system's preference is a stub here.
    vi.stubGlobal("matchMedia", (query: string) => ({
      matches: query.includes("reduce"),
      media: query,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
    }));
    const system = mount(AlixVoiceOrb, { attachTo: document.body });
    await system.vm.$nextTick();
    expect(latest().setReducedMotion).toHaveBeenLastCalledWith(true);
    vi.unstubAllGlobals();
  });
});

describe("AlixVoiceMode and the orb", () => {
  it("shows the orb by default, fed by the phase and the newest level", async () => {
    withCanvas2d();
    const wrapper = mount(AlixVoiceMode, { props: { state: "listening", levels: [0.1, 0.2, 0.7] }, attachTo: document.body });
    const orb = latest();
    expect(wrapper.find(".alix-voice-orb").attributes("data-state")).toBe("listening");
    expect(orb.options.level).toBe(0.7);
    await wrapper.setProps({ state: "processing", levels: [0.2, 0.4] });
    expect(orb.setState).toHaveBeenLastCalledWith("thinking");
    expect(orb.setLevel).toHaveBeenLastCalledWith(0.4);
    await wrapper.setProps({ state: "unavailable" });
    expect(orb.setState).toHaveBeenLastCalledWith("muted");
    wrapper.unmount();
    expect(orb.destroy).toHaveBeenCalled();
  });

  it("lets the waveform slot replace the orb, with the levels as before", () => {
    const wrapper = mount(AlixVoiceMode, {
      props: { state: "listening", levels: [0.2, 0.9] },
      slots: { waveform: (scope: { levels: readonly number[] }) => h("span", { class: "host-bars" }, scope.levels.join(",")) },
    });
    expect(wrapper.find(".alix-voice-orb").exists()).toBe(false);
    expect(wrapper.find(".host-bars").text()).toBe("0.2,0.9");
    expect(orbs).toHaveLength(0);
  });
});
