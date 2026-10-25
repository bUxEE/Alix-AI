// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createVoiceOrb, readCssColors, watchPresentation } from "../src/index.js";

// jsdom has no WebGL and no Canvas 2D. The tests hand the orb fake contexts and drive its frames by hand.

interface Fake2d {
  draws: number;
  context: Record<string, unknown>;
}

const fake2d = (): Fake2d => {
  const state: Fake2d = { draws: 0, context: {} };
  const gradient = { addColorStop: () => undefined };
  state.context = {
    fillStyle: "",
    clearRect: () => {
      state.draws += 1;
    },
    fillRect: () => undefined,
    createRadialGradient: () => gradient,
    save: () => undefined,
    restore: () => undefined,
    beginPath: () => undefined,
    moveTo: () => undefined,
    lineTo: () => undefined,
    closePath: () => undefined,
    clip: () => undefined,
  };
  return state;
};

/** A WebGL context that accepts everything: every method is a no-op that succeeds. */
const fakeGl = (): { context: WebGLRenderingContext; calls: string[]; lost: { value: boolean } } => {
  const calls: string[] = [];
  const lost = { value: false };
  const loseContext = vi.fn();
  const context = new Proxy({} as Record<string, unknown>, {
    get: (_target, name: string) => {
      if (name === "isContextLost") return () => lost.value;
      if (name === "getExtension") return (extension: string) => (extension === "WEBGL_lose_context" ? { loseContext } : null);
      if (name === "getShaderParameter" || name === "getProgramParameter") return () => true;
      if (name === "getAttribLocation") return () => 0;
      if (/^[A-Z_]+$/.test(name)) return 1;
      return (...args: unknown[]) => {
        calls.push(name);
        return args.length >= 0 ? {} : undefined;
      };
    },
  }) as unknown as WebGLRenderingContext;
  return { context, calls, lost };
};

let frames: ((time: number) => void)[] = [];
let now = 0;
const runFrames = (count: number, step = 17): void => {
  for (let i = 0; i < count; i += 1) {
    const pending = frames;
    frames = [];
    now += step;
    for (const callback of pending) callback(now);
  }
};

const host = (): HTMLElement => {
  const element = document.createElement("span");
  element.getBoundingClientRect = () => ({ width: 100, height: 100, top: 0, left: 0, right: 100, bottom: 100, x: 0, y: 0, toJSON: () => ({}) });
  document.body.appendChild(element);
  return element;
};

beforeEach(() => {
  frames = [];
  now = 0;
  vi.stubGlobal("requestAnimationFrame", (callback: (time: number) => void) => frames.push(callback));
  vi.stubGlobal("cancelAnimationFrame", () => {
    frames = [];
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  document.body.innerHTML = "";
});

describe("createVoiceOrb", () => {
  it("falls back to Canvas 2D where there is no WebGL, and draws", () => {
    const paint = fake2d();
    vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation(((kind: string) => (kind === "2d" ? paint.context : null)) as never);
    vi.stubGlobal("devicePixelRatio", 3);
    const element = host();
    const orb = createVoiceOrb(element, { maxDpr: 2 });
    expect(orb.renderer).toBe("2d");
    const canvas = element.querySelector("canvas") as HTMLCanvasElement;
    expect(canvas.getAttribute("aria-hidden")).toBe("true");
    expect(canvas.width).toBe(200);
    runFrames(3);
    expect(paint.draws).toBeGreaterThanOrEqual(2);
    orb.destroy();
  });

  it("caps the backing buffer and the device pixel ratio", () => {
    vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation(((kind: string) => (kind === "2d" ? fake2d().context : null)) as never);
    vi.stubGlobal("devicePixelRatio", 4);
    const element = host();
    element.getBoundingClientRect = () => ({ width: 400, height: 400, top: 0, left: 0, right: 400, bottom: 400, x: 0, y: 0, toJSON: () => ({}) });
    const orb = createVoiceOrb(element);
    expect((element.querySelector("canvas") as HTMLCanvasElement).width).toBe(512);
    orb.destroy();
    const small = host();
    const capped = createVoiceOrb(small, { maxDpr: 1.5 });
    expect((small.querySelector("canvas") as HTMLCanvasElement).width).toBe(150);
    capped.destroy();
  });

  it("draws with WebGL, paints in 2D while the context is lost, and goes back when it is restored", () => {
    const gl = fakeGl();
    const paint = fake2d();
    // The first canvas the orb makes holds the WebGL context; any later one can only give a 2D context.
    let first: HTMLCanvasElement | undefined;
    let made = 0;
    const create = document.createElement.bind(document);
    vi.spyOn(document, "createElement").mockImplementation(((tag: string) => {
      const element = create(tag);
      if (element instanceof HTMLCanvasElement) {
        const own = made === 0;
        made += 1;
        if (own) first = element;
        element.getContext = ((kind: string) => (own ? (kind === "webgl" ? gl.context : null) : kind === "2d" ? paint.context : null)) as never;
      }
      return element;
    }) as never);
    const changes: string[] = [];
    const element = host();
    const orb = createVoiceOrb(element, { onRendererChange: (next) => changes.push(next) });
    expect(orb.renderer).toBe("webgl");
    runFrames(2);
    expect(gl.calls).toContain("drawArrays");

    const lost = new Event("webglcontextlost", { cancelable: true });
    gl.lost.value = true;
    first?.dispatchEvent(lost);
    expect(lost.defaultPrevented).toBe(true);
    expect(orb.renderer).toBe("2d");
    expect(element.querySelectorAll("canvas")).toHaveLength(2);
    expect(first?.style.display).toBe("none");
    runFrames(2);
    expect(paint.draws).toBeGreaterThan(0);

    gl.lost.value = false;
    first?.dispatchEvent(new Event("webglcontextrestored"));
    expect(orb.renderer).toBe("webgl");
    expect(element.querySelectorAll("canvas")).toHaveLength(1);
    expect(changes).toEqual(["webgl", "2d", "webgl"]);
    orb.destroy();
    expect(element.querySelectorAll("canvas")).toHaveLength(0);
  });

  it("shows its state while paused: made in a hidden tab it paints once, a new state repaints, a level alone does not", () => {
    const paint = fake2d();
    vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation(((kind: string) => (kind === "2d" ? paint.context : null)) as never);
    Object.defineProperty(document, "hidden", { configurable: true, value: true });
    try {
      const orb = createVoiceOrb(host());
      expect(paint.draws).toBe(1); // never blank, though no frame was ever scheduled
      expect(frames).toHaveLength(0);
      orb.setState("listening");
      expect(paint.draws).toBe(2);
      orb.setLevel(0.7);
      expect(paint.draws).toBe(2);
      expect(frames).toHaveLength(0);
      orb.destroy();
    } finally {
      Object.defineProperty(document, "hidden", { configurable: true, value: false });
    }
  });

  it("stops drawing when destroyed, when the tab is hidden, and when settled under reduced motion", () => {
    const paint = fake2d();
    vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation(((kind: string) => (kind === "2d" ? paint.context : null)) as never);
    const element = host();
    const orb = createVoiceOrb(element);
    runFrames(2);

    Object.defineProperty(document, "hidden", { configurable: true, value: true });
    document.dispatchEvent(new Event("visibilitychange"));
    const whileHidden = paint.draws;
    runFrames(5);
    expect(paint.draws).toBe(whileHidden);
    expect(frames).toHaveLength(0);
    Object.defineProperty(document, "hidden", { configurable: true, value: false });
    document.dispatchEvent(new Event("visibilitychange"));
    runFrames(2);
    expect(paint.draws).toBeGreaterThan(whileHidden);

    orb.setReducedMotion(true);
    runFrames(10);
    expect(frames).toHaveLength(0); // nothing changes: the loop has stopped
    orb.setLevel(0.6);
    expect(frames).toHaveLength(1); // a new level wakes it up
    runFrames(400); // the level lands, goes stale and fades: then the loop stops again
    expect(frames).toHaveLength(0);

    orb.setReducedMotion(false);
    orb.destroy();
    const after = paint.draws;
    runFrames(5);
    expect(paint.draws).toBe(after);
    expect(element.querySelector("canvas")).toBeNull();
  });

  it("changes state and level without throwing, and reports the state", () => {
    vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation((() => null) as never);
    const orb = createVoiceOrb(host(), { state: "thinking" });
    expect(orb.renderer).toBe("none");
    expect(orb.state).toBe("thinking");
    orb.setState("speaking");
    orb.setLevel(4);
    orb.setColors({ accent: "oklch(0.5 0.1 190)" });
    runFrames(3);
    expect(orb.state).toBe("speaking");
    orb.destroy();
    orb.destroy();
  });
});

describe("theme helpers", () => {
  it("read the token values an element sees, and leave unset ones out", () => {
    const element = host();
    element.style.setProperty("--brand", "oklch(0.5 0.1 190)");
    expect(readCssColors(element, { accent: "--brand", surface: "--nothing", error: "--nothing" })).toEqual({ accent: "oklch(0.5 0.1 190)" });
  });

  it("notice a theme attribute changing on an ancestor, once per frame", async () => {
    const parent = document.createElement("div");
    const element = host();
    parent.appendChild(element);
    document.body.appendChild(parent);
    const onChange = vi.fn();
    const stop = watchPresentation(element, onChange);
    parent.setAttribute("data-theme", "dark");
    parent.setAttribute("class", "x");
    await Promise.resolve();
    runFrames(1);
    expect(onChange).toHaveBeenCalledTimes(1);
    stop();
    parent.setAttribute("data-theme", "light");
    await Promise.resolve();
    runFrames(1);
    expect(onChange).toHaveBeenCalledTimes(1);
  });
});
