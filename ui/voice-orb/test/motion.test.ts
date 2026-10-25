import { describe, expect, it } from "vitest";
import {
  advancePhase,
  clamp01,
  easeInOutCubic,
  edgeWave,
  frameOf,
  heldLevel,
  LEVEL_HOLD_MS,
  PHASE_WRAP,
  retarget,
  settledTransition,
  smoothLevel,
  STATE_PARAMS,
  stepTransition,
  syllable,
  TRANSITION_MS,
  transitionDone,
  transitionParams,
  VOICE_ORB_STATES,
  voiceOrbStateFor,
} from "../src/motion.js";

const clock = { breath: 0, flow: 0, pulse: 0 };

describe("level smoothing", () => {
  it("rises fast and falls slowly", () => {
    const up = smoothLevel(0, 1, 50);
    const down = smoothLevel(1, 0, 50);
    expect(up).toBeGreaterThan(0.45);
    expect(1 - down).toBeLessThan(0.2);
  });

  it("depends on elapsed time only: two 16 ms steps equal one 32 ms step", () => {
    for (const [from, to] of [
      [0, 0.8],
      [0.9, 0.1],
    ] as const) {
      expect(smoothLevel(smoothLevel(from, to, 16), to, 16)).toBeCloseTo(smoothLevel(from, to, 32), 10);
    }
  });

  it("clamps the target, ignores a zero or negative step and converges", () => {
    expect(smoothLevel(0.3, 5, 0)).toBe(0.3);
    expect(smoothLevel(0.3, 5, -10)).toBe(0.3);
    let level = 0;
    for (let i = 0; i < 200; i += 1) level = smoothLevel(level, 7, 16);
    expect(level).toBeCloseTo(1, 5);
    expect(smoothLevel(0.5, Number.NaN, 16)).toBeLessThan(0.5);
    expect(clamp01(-1)).toBe(0);
    expect(clamp01(Number.POSITIVE_INFINITY)).toBe(0);
  });
});

describe("stale levels", () => {
  it("hold a fresh level and let one that stopped arriving fall to silence", () => {
    expect(heldLevel(0.6, 0)).toBe(0.6);
    expect(heldLevel(0.6, LEVEL_HOLD_MS)).toBe(0.6);
    expect(heldLevel(0.6, LEVEL_HOLD_MS + 1)).toBe(0);
    expect(heldLevel(3, 10)).toBe(1);
    // A reply whose audio arrived in one burst: the orb settles back to the syllable pulse within about a second.
    let level = 0.45;
    let age = 0;
    for (let i = 0; i < 60; i += 1) {
      age += 16;
      level = smoothLevel(level, heldLevel(0.45, age), 16);
    }
    expect(level).toBeLessThan(0.05);
  });
});

describe("state cross-fades", () => {
  it("eases in and out between 0 and 1", () => {
    expect(easeInOutCubic(0)).toBe(0);
    expect(easeInOutCubic(1)).toBe(1);
    expect(easeInOutCubic(0.5)).toBeCloseTo(0.5);
    expect(easeInOutCubic(0.1)).toBeLessThan(0.1);
    expect(easeInOutCubic(2)).toBe(1);
  });

  it("fades from one state to the next over the transition time, never in one jump", () => {
    let fade = retarget(settledTransition(STATE_PARAMS.idle), STATE_PARAMS.listening);
    expect(transitionParams(fade).scale).toBeCloseTo(STATE_PARAMS.idle.scale);
    fade = stepTransition(fade, 16);
    const early = transitionParams(fade).scale;
    expect(early - STATE_PARAMS.idle.scale).toBeLessThan(0.01);
    fade = stepTransition(fade, TRANSITION_MS / 2 - 16);
    expect(transitionParams(fade).scale).toBeCloseTo((STATE_PARAMS.idle.scale + STATE_PARAMS.listening.scale) / 2, 5);
    expect(transitionDone(fade)).toBe(false);
    fade = stepTransition(fade, TRANSITION_MS);
    expect(transitionDone(fade)).toBe(true);
    expect(transitionParams(fade)).toEqual(STATE_PARAMS.listening);
  });

  it("starts a new fade from wherever the current one has got to", () => {
    let fade = stepTransition(retarget(settledTransition(STATE_PARAMS.idle), STATE_PARAMS.error), TRANSITION_MS / 2);
    const midway = transitionParams(fade);
    fade = retarget(fade, STATE_PARAMS.speaking);
    expect(transitionParams(fade)).toEqual(midway);
    expect(midway.alert).toBeGreaterThan(0.3);
    expect(midway.alert).toBeLessThan(0.7);
  });

  it("gives every state its own look", () => {
    expect(VOICE_ORB_STATES).toHaveLength(6);
    const looks = new Set(VOICE_ORB_STATES.map((state) => JSON.stringify(STATE_PARAMS[state])));
    expect(looks.size).toBe(6);
    // Thinking is told apart by its swirl, muted by its colour, error by the alert palette.
    expect(STATE_PARAMS.thinking.swirl).toBeGreaterThan(3 * STATE_PARAMS.idle.swirl);
    expect(STATE_PARAMS.muted.saturation).toBeLessThan(0.3);
    expect(STATE_PARAMS.error.alert).toBe(1);
    // Only listening and speaking react to the voice.
    for (const state of VOICE_ORB_STATES) expect(STATE_PARAMS[state].levelGain > 0).toBe(state === "listening" || state === "speaking");
  });
});

describe("frames", () => {
  it("swells and ripples with the level while listening, and stays inside the canvas", () => {
    const quiet = frameOf(STATE_PARAMS.listening, 0, clock, false);
    const loud = frameOf(STATE_PARAMS.listening, 1, clock, false);
    expect(loud.radius).toBeGreaterThan(quiet.radius + 0.1);
    expect(loud.wobble).toBeGreaterThan(quiet.wobble);
    expect(loud.radius * (1 + loud.wobble)).toBeLessThan(1);
  });

  it("ignores a stray level when the state does not listen", () => {
    expect(frameOf(STATE_PARAMS.thinking, 1, clock, false).radius).toBe(frameOf(STATE_PARAMS.thinking, 0, clock, false).radius);
  });

  it("pulses while speaking even when the speech level has gone flat", () => {
    const radii = Array.from({ length: 40 }, (_, i) => frameOf(STATE_PARAMS.speaking, 0, { ...clock, pulse: i * 0.2 }, false).radius);
    expect(Math.max(...radii) - Math.min(...radii)).toBeGreaterThan(0.02);
    for (let i = 0; i < 100; i += 1) {
      const value = syllable(i * 0.37);
      expect(value).toBeGreaterThanOrEqual(0);
      expect(value).toBeLessThanOrEqual(1);
    }
  });

  it("breathes when idle", () => {
    const inhale = frameOf(STATE_PARAMS.idle, 0, { ...clock, breath: Math.PI / 2 }, false);
    const exhale = frameOf(STATE_PARAMS.idle, 0, { ...clock, breath: -Math.PI / 2 }, false);
    expect(inhale.radius).toBeGreaterThan(exhale.radius);
  });

  it("holds still under reduced motion, and shows the level as brightness only", () => {
    const moving = { breath: 1.2, flow: 3.4, pulse: 2 };
    const quiet = frameOf(STATE_PARAMS.listening, 0, moving, true);
    const loud = frameOf(STATE_PARAMS.listening, 1, moving, true);
    expect(quiet.radius).toBe(loud.radius);
    expect(quiet.wobble).toBe(0);
    expect(quiet.flow).toBe(0);
    expect(loud.glow).toBeGreaterThan(quiet.glow);
    // The states stay readable without motion: different sizes and swirl.
    expect(frameOf(STATE_PARAMS.muted, 0, moving, true).radius).toBeLessThan(frameOf(STATE_PARAMS.listening, 0, moving, true).radius);
    expect(frameOf(STATE_PARAMS.thinking, 0, moving, true).swirl).toBeGreaterThan(frameOf(STATE_PARAMS.idle, 0, moving, true).swirl);
  });
});

describe("phases", () => {
  it("advance with time and wrap without a visible seam", () => {
    expect(advancePhase(0, Math.PI, 1000)).toBeCloseTo(Math.PI);
    const wrapped = advancePhase(PHASE_WRAP - 0.01, 1, 20);
    expect(wrapped).toBeCloseTo(0.01, 6);
    // Every multiplier of the flow in the edge has one decimal: the edge is the same on both sides of the wrap.
    for (const angle of [0, 1, 2.5, 4]) expect(edgeWave(angle, PHASE_WRAP + 0.3)).toBeCloseTo(edgeWave(angle, 0.3), 6);
    expect(advancePhase(1, 2, -50)).toBe(1);
  });
});

describe("voice phases", () => {
  it("map onto the orb states the same way in both kits", () => {
    expect(voiceOrbStateFor("idle")).toBe("idle");
    expect(voiceOrbStateFor("listening")).toBe("listening");
    expect(voiceOrbStateFor("speaking")).toBe("speaking");
    for (const phase of ["processing", "connecting", "reconnecting"]) expect(voiceOrbStateFor(phase)).toBe("thinking");
    expect(voiceOrbStateFor("unavailable", "text_input_only")).toBe("muted");
    expect(voiceOrbStateFor("ended")).toBe("muted");
    expect(voiceOrbStateFor("unavailable", "connect_failed")).toBe("error");
    expect(voiceOrbStateFor("idle", "microphone_unavailable")).toBe("error");
    expect(voiceOrbStateFor("something new")).toBe("idle");
  });
});
