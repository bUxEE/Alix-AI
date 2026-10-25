import { createColorResolver, DEFAULT_BASE, derivePalette } from "./color.js";
import type { OrbBaseColors, VoiceOrbColors } from "./color.js";
import {
  advancePhase,
  clamp01,
  frameOf,
  LEVEL_ATTACK_MS,
  LEVEL_RELEASE_MS,
  PULSE_RATE,
  retarget,
  settledTransition,
  smoothLevel,
  heldLevel,
  LEVEL_HOLD_MS,
  STATE_PARAMS,
  STILL_ATTACK_MS,
  STILL_RELEASE_MS,
  stepTransition,
  transitionDone,
  transitionParams,
} from "./motion.js";
import type { OrbClock, OrbTransition, VoiceOrbState } from "./motion.js";
import { createCanvasRenderer, createWebglRenderer } from "./renderers.js";
import type { OrbRenderer } from "./renderers.js";

// The orb itself: one canvas inside a host element, a frame loop, and the renderer behind it.
//
// Costs are capped. The backing buffer follows the element's size times the device pixel ratio, but the ratio is capped (2 by
// default) and so is the buffer (512 pixels a side). Frames are capped at 60 a second, and the loop stops altogether while the
// element is off screen or the tab is hidden. With reduced motion the orb draws only when something changed, then stops.
//
// A canvas that gave out a WebGL context can never give a 2D one, so the orb owns its canvases: when the GPU drops the context it
// paints with Canvas 2D on a second canvas until the context comes back, then removes it.

export type VoiceOrbRenderer = "webgl" | "2d" | "none";

export interface VoiceOrbOptions {
  state?: VoiceOrbState;
  /** The voice level, 0 to 1 (already perceptual: the kits' bar height). A live feed: a level not refreshed for 250 ms fades out. */
  level?: number;
  colors?: VoiceOrbColors;
  /** Hold still: no breathing, no currents, no ripple. The state still shows through size, colour and brightness. */
  reducedMotion?: boolean;
  /** The highest device pixel ratio honoured. Default 2. */
  maxDpr?: number;
  /** The largest backing buffer, in device pixels a side. Default 512. */
  maxBufferSize?: number;
  /** Frames a second at most. Default 60. */
  maxFps?: number;
  /** `2d` skips WebGL. Default `auto`. */
  renderer?: "auto" | "2d";
  /** Called when the renderer changes (the WebGL context was lost or came back). */
  onRendererChange?: (renderer: VoiceOrbRenderer) => void;
}

export interface VoiceOrb {
  setState: (state: VoiceOrbState) => void;
  /** Every time a level arrives (a level not refreshed for 250 ms fades out). */
  setLevel: (level: number) => void;
  /** Replaces the colours (pass the theme's tokens again when the theme changes). Missing ones fall back to the defaults. */
  setColors: (colors: VoiceOrbColors) => void;
  setReducedMotion: (on: boolean) => void;
  /** Measures the host again. Called by itself when the host is resized, where ResizeObserver exists. */
  resize: () => void;
  /** Stops the loop, releases the GPU context and removes the canvases. */
  destroy: () => void;
  readonly renderer: VoiceOrbRenderer;
  readonly state: VoiceOrbState;
}

const GL_ATTRIBUTES: WebGLContextAttributes = {
  alpha: true,
  premultipliedAlpha: true,
  antialias: false,
  depth: false,
  stencil: false,
  preserveDrawingBuffer: false,
  powerPreference: "low-power",
};

type FrameRequest = (callback: (time: number) => void) => number;

export const createVoiceOrb = (host: HTMLElement, options: VoiceOrbOptions = {}): VoiceOrb => {
  const maxDpr = Math.max(0.5, options.maxDpr ?? 2);
  const maxBuffer = Math.max(16, Math.round(options.maxBufferSize ?? 512));
  const minFrameMs = 1000 / Math.max(1, options.maxFps ?? 60);
  const win = typeof window === "undefined" ? undefined : window;
  const requestFrame: FrameRequest = win?.requestAnimationFrame?.bind(win) ?? ((callback) => setTimeout(() => callback(Date.now()), 16) as unknown as number);
  const cancelFrame = (handle: number): void => {
    if (win?.cancelAnimationFrame !== undefined) win.cancelAnimationFrame(handle);
    else clearTimeout(handle);
  };
  const resolve = createColorResolver();

  let state: VoiceOrbState = options.state ?? "idle";
  let transition: OrbTransition = settledTransition(STATE_PARAMS[state]);
  let target = clamp01(options.level ?? 0);
  let level = target;
  let levelAge = 0; // ms since the level was last set
  let reduced = options.reducedMotion === true;
  let clock: OrbClock = { breath: 0, flow: 0, pulse: 0 };
  let base: OrbBaseColors = { ...DEFAULT_BASE };
  let width = 0;
  let height = 0;
  let destroyed = false;
  let visible = true;
  let hidden = typeof document !== "undefined" && document.hidden === true;
  let handle: number | undefined;
  let last = 0;
  let dirty = true;

  // ---- canvases and renderers ----
  const newCanvas = (): HTMLCanvasElement => {
    const canvas = document.createElement("canvas");
    canvas.setAttribute("aria-hidden", "true");
    canvas.style.display = "block";
    canvas.style.width = "100%";
    canvas.style.height = "100%";
    canvas.width = Math.max(1, width);
    canvas.height = Math.max(1, height);
    host.appendChild(canvas);
    return canvas;
  };

  let main = newCanvas();
  let fallback: HTMLCanvasElement | undefined; // the 2D canvas used while the WebGL context is lost
  let gl: WebGLRenderingContext | undefined;
  let renderer: OrbRenderer | undefined;
  let kind: VoiceOrbRenderer = "none";

  const announce = (next: VoiceOrbRenderer): void => {
    if (next === kind) return;
    kind = next;
    options.onRendererChange?.(next);
  };

  const canvas2d = (canvas: HTMLCanvasElement): OrbRenderer | undefined => {
    try {
      const context = canvas.getContext("2d");
      return context === null || context === undefined ? undefined : createCanvasRenderer(context);
    } catch {
      return undefined;
    }
  };

  const webgl = (canvas: HTMLCanvasElement): { context: WebGLRenderingContext; renderer: OrbRenderer } | undefined => {
    try {
      const context = (canvas.getContext("webgl", GL_ATTRIBUTES) ?? canvas.getContext("experimental-webgl", GL_ATTRIBUTES)) as WebGLRenderingContext | null;
      if (context === null || context === undefined) return undefined;
      const built = createWebglRenderer(context);
      if (built === undefined) {
        context.getExtension("WEBGL_lose_context")?.loseContext();
        return undefined;
      }
      return { context, renderer: built };
    } catch {
      return undefined;
    }
  };

  const onLost = (event: Event): void => {
    event.preventDefault(); // asks the browser to restore the context
    renderer = undefined;
    main.style.display = "none";
    fallback ??= newCanvas();
    renderer = canvas2d(fallback);
    announce(renderer === undefined ? "none" : "2d");
    dirty = true;
    schedule();
  };

  const onRestored = (): void => {
    const built = gl === undefined ? undefined : createWebglRenderer(gl);
    if (built === undefined) return;
    renderer = built;
    fallback?.remove();
    fallback = undefined;
    main.style.display = "block";
    announce("webgl");
    dirty = true;
    schedule();
  };

  const attach = (): void => {
    const made = options.renderer === "2d" ? undefined : webgl(main);
    if (made !== undefined) {
      gl = made.context;
      renderer = made.renderer;
      main.addEventListener("webglcontextlost", onLost);
      main.addEventListener("webglcontextrestored", onRestored);
      announce("webgl");
      return;
    }
    // A canvas that held a WebGL context (one whose program failed) cannot paint in 2D: start again on a fresh one.
    if (options.renderer !== "2d") {
      main.remove();
      main = newCanvas();
    }
    renderer = canvas2d(main);
    announce(renderer === undefined ? "none" : "2d");
  };

  // ---- size ----
  const resize = (): void => {
    if (destroyed) return;
    const box = host.getBoundingClientRect();
    const dpr = Math.min(maxDpr, Math.max(0.5, win?.devicePixelRatio ?? 1));
    const side = (css: number): number => (css > 0 ? Math.min(maxBuffer, Math.max(1, Math.round(css * dpr))) : 0);
    const nextWidth = side(box.width || host.clientWidth);
    const nextHeight = side(box.height || host.clientHeight);
    if (nextWidth === width && nextHeight === height) return;
    width = nextWidth;
    height = nextHeight;
    for (const canvas of [main, fallback]) {
      if (canvas === undefined) continue;
      canvas.width = Math.max(1, width);
      canvas.height = Math.max(1, height);
    }
    dirty = true;
    schedule();
  };

  // ---- the loop ----
  const goal = (): number => heldLevel(target, levelAge);
  // A level that may still go stale keeps the loop alive until it has (even under reduced motion).
  const settled = (): boolean => transitionDone(transition) && Math.abs(level - goal()) < 0.002 && !(target > 0 && levelAge <= LEVEL_HOLD_MS);
  // Nothing to paint with (no WebGL, no Canvas 2D): no loop at all.
  const needsFrame = (): boolean => renderer !== undefined && (!reduced || dirty || !settled());

  const draw = (): void => {
    if (renderer === undefined || width === 0 || height === 0) return;
    const params = transitionParams(transition);
    const frame = frameOf(params, level, clock, reduced);
    try {
      renderer.draw(frame, derivePalette(base, params.alert, params.saturation, params.brightness), width, height);
    } catch {
      // a context lost between two frames: the lost event switches the renderer
    }
  };

  const tick = (now: number): void => {
    handle = undefined;
    if (destroyed || !visible || hidden) return;
    const elapsed = last === 0 ? minFrameMs : now - last;
    if (elapsed < minFrameMs - 2) {
      schedule();
      return;
    }
    last = now;
    const dt = Math.min(elapsed, 100); // after a stall, ease on instead of leaping
    const params = transitionParams(transition);
    transition = stepTransition(transition, dt);
    levelAge += dt;
    level = reduced ? smoothLevel(level, goal(), dt, STILL_ATTACK_MS, STILL_RELEASE_MS) : smoothLevel(level, goal(), dt, LEVEL_ATTACK_MS, LEVEL_RELEASE_MS);
    if (!reduced) {
      clock = {
        breath: advancePhase(clock.breath, params.breathRate, dt),
        flow: advancePhase(clock.flow, params.flow, dt),
        pulse: advancePhase(clock.pulse, PULSE_RATE, dt),
      };
    }
    draw();
    dirty = false;
    if (needsFrame()) schedule();
  };

  // Paused (tab hidden, orb off screen): no loop, but the orb still shows its current state. It is painted once, still, with
  // its cross-fade and level already settled, so an orb made in a background tab is never blank and a change made while
  // paused is visible as soon as anyone looks.
  const paintStill = (): void => {
    transition = stepTransition(transition, Number.MAX_SAFE_INTEGER);
    level = goal();
    draw();
    dirty = false;
  };

  // Declared after `tick`, which it schedules; everything that calls it runs only once the orb is built.
  const schedule = (): void => {
    if (destroyed) return;
    if (!visible || hidden) {
      // only what changes the look (a new state, colours, size, the first frame); a level reading alone is not worth it
      if (dirty || !transitionDone(transition)) paintStill();
      return;
    }
    if (handle !== undefined || !needsFrame()) return;
    handle = requestFrame(tick);
  };

  const pause = (): void => {
    if (handle !== undefined) cancelFrame(handle);
    handle = undefined;
    last = 0;
  };

  // ---- visibility ----
  const onVisibility = (): void => {
    hidden = document.hidden;
    if (hidden) pause();
    schedule();
  };
  if (typeof document !== "undefined") document.addEventListener("visibilitychange", onVisibility);
  const intersection =
    typeof IntersectionObserver === "function"
      ? new IntersectionObserver((entries) => {
          const entry = entries[entries.length - 1];
          if (entry === undefined) return;
          visible = entry.isIntersecting;
          if (!visible) pause();
          schedule();
        })
      : undefined;
  intersection?.observe(host);
  const sizing = typeof ResizeObserver === "function" ? new ResizeObserver(() => resize()) : undefined;
  sizing?.observe(host);

  // ---- public ----
  const setColors = (colors: VoiceOrbColors): void => {
    base = {
      accent: resolve(colors.accent) ?? DEFAULT_BASE.accent,
      surface: resolve(colors.surface) ?? DEFAULT_BASE.surface,
      error: resolve(colors.error) ?? DEFAULT_BASE.error,
    };
    dirty = true;
    schedule();
  };

  attach();
  if (options.colors !== undefined) setColors(options.colors);
  resize();
  schedule();

  return {
    setState: (next) => {
      if (next === state || STATE_PARAMS[next] === undefined) return;
      state = next;
      transition = retarget(transition, STATE_PARAMS[next]);
      schedule();
    },
    setLevel: (next) => {
      target = clamp01(next);
      levelAge = 0; // the same value again is still a fresh reading
      schedule();
    },
    setColors,
    setReducedMotion: (on) => {
      if (on === reduced) return;
      reduced = on;
      dirty = true;
      schedule();
    },
    resize,
    destroy: () => {
      if (destroyed) return;
      destroyed = true;
      pause();
      if (typeof document !== "undefined") document.removeEventListener("visibilitychange", onVisibility);
      intersection?.disconnect();
      sizing?.disconnect();
      main.removeEventListener("webglcontextlost", onLost);
      main.removeEventListener("webglcontextrestored", onRestored);
      renderer?.dispose();
      renderer = undefined;
      try {
        gl?.getExtension("WEBGL_lose_context")?.loseContext(); // frees the GPU context now, not at garbage collection
      } catch {
        // already lost
      }
      main.remove();
      fallback?.remove();
    },
    get renderer() {
      return kind;
    },
    get state() {
      return state;
    },
  };
};
