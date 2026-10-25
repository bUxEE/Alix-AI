// @vitest-environment jsdom
import { act, cleanup, render } from "@testing-library/react";
import { renderToString } from "react-dom/server";
import type * as OrbModule from "@alixai/voice-orb";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AlixProvider } from "../src/index.js";
import { AlixVoiceMode, AlixVoiceOrb } from "../src/voice/index.js";

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

const orbElement = (): HTMLElement => document.querySelector('[data-alixai="voice-orb"]') as HTMLElement;

afterEach(() => {
  cleanup();
  orbs.length = 0;
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("AlixVoiceOrb", () => {
  it("paints with Canvas 2D where there is no WebGL, and is an image named after its state", () => {
    withCanvas2d();
    const { rerender } = render(<AlixVoiceOrb state="listening" size={96} />);
    const root = orbElement();
    expect(root.getAttribute("role")).toBe("img");
    expect(root.getAttribute("aria-label")).toBe("Voice assistant, listening");
    expect(root.getAttribute("data-renderer")).toBe("2d");
    expect(root.style.width).toBe("96px");
    expect(root.querySelector("canvas")?.getAttribute("aria-hidden")).toBe("true");
    rerender(<AlixVoiceOrb state="listening" label="Alix" />);
    expect(root.getAttribute("aria-label")).toBe("Alix");
  });

  it("names its state in Italian", () => {
    render(
      <AlixProvider locale="it">
        <AlixVoiceOrb state="thinking" />
      </AlixProvider>,
    );
    expect(orbElement().getAttribute("aria-label")).toBe("Assistente vocale, sta pensando");
  });

  it("hands state, level and colours to the orb, and releases it on unmount", () => {
    withCanvas2d();
    const view = render(<AlixVoiceOrb state="idle" level={0} />);
    const orb = latest();
    expect(orb.options.state).toBe("idle");
    expect(orb.options.reducedMotion).toBe(false);
    view.rerender(<AlixVoiceOrb state="speaking" level={0.6} />);
    expect(orb.setState).toHaveBeenLastCalledWith("speaking");
    expect(orb.setLevel).toHaveBeenLastCalledWith(0.6);
    view.rerender(<AlixVoiceOrb state="speaking" level={0.6} colors={{ accent: "rebeccapurple" }} />);
    expect(orb.setColors).toHaveBeenLastCalledWith(expect.objectContaining({ accent: "rebeccapurple" }));
    expect(document.querySelector("canvas")).not.toBeNull();
    view.unmount();
    expect(orb.destroy).toHaveBeenCalledTimes(1);
    expect(document.querySelector("canvas")).toBeNull();
  });

  it("holds still under reduced motion: the prop, an ancestor's attribute or the system", async () => {
    withCanvas2d();
    const view = render(<AlixVoiceOrb />);
    view.rerender(<AlixVoiceOrb reducedMotion />);
    expect(latest().setReducedMotion).toHaveBeenLastCalledWith(true);
    view.unmount();

    render(
      <div data-reduced-motion="true">
        <AlixVoiceOrb />
      </div>,
    );
    expect(latest().options.reducedMotion).toBe(true);
    cleanup();

    vi.stubGlobal("matchMedia", (query: string) => ({
      matches: query.includes("reduce"),
      media: query,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
    }));
    await act(async () => {
      render(<AlixVoiceOrb />);
    });
    expect(latest().setReducedMotion).toHaveBeenLastCalledWith(true);
  });

  it("renders on a server without touching the browser", () => {
    const html = renderToString(<AlixVoiceOrb state="muted" />);
    expect(html).toContain('role="img"');
    expect(html).toContain("Voice assistant, paused");
    expect(html).not.toContain("<canvas");
  });
});

describe("AlixVoiceMode and the orb", () => {
  it("shows the orb by default, fed by the phase and the newest level", () => {
    withCanvas2d();
    const view = render(<AlixVoiceMode state="listening" levels={[0.1, 0.2, 0.7]} />);
    const orb = latest();
    expect(orbElement().getAttribute("data-state")).toBe("listening");
    expect(orb.options.level).toBe(0.7);
    view.rerender(<AlixVoiceMode state="processing" levels={[0.2, 0.4]} />);
    expect(orb.setState).toHaveBeenLastCalledWith("thinking");
    expect(orb.setLevel).toHaveBeenLastCalledWith(0.4);
    view.rerender(<AlixVoiceMode state="unavailable" />);
    expect(orb.setState).toHaveBeenLastCalledWith("muted");
    view.unmount();
    expect(orb.destroy).toHaveBeenCalled();
  });

  it("lets renderWaveform replace the orb, with the levels as before", () => {
    render(<AlixVoiceMode state="listening" levels={[0.2, 0.9]} renderWaveform={(levels) => <span className="host-bars">{levels.join(",")}</span>} />);
    expect(orbElement()).toBeNull();
    expect(document.querySelector(".host-bars")?.textContent).toBe("0.2,0.9");
    expect(orbs).toHaveLength(0);
  });
});
