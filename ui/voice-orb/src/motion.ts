// The orb's motion, as pure functions of time: what each state looks like, how one state fades into the next, how the voice level
// is smoothed, and the shape of the orb on a given frame. Nothing here touches the DOM, so all of it is unit tested and both
// renderers (WebGL and Canvas 2D) draw from the same numbers.

/** What the orb shows. The kits map their voice phases onto these (`voiceOrbStateFor`). */
export type VoiceOrbState = "idle" | "listening" | "thinking" | "speaking" | "muted" | "error";

export const VOICE_ORB_STATES: readonly VoiceOrbState[] = Object.freeze(["idle", "listening", "thinking", "speaking", "muted", "error"]);

/** The look of one state. Every field is a number, so two states blend field by field. */
export interface OrbParams {
  /** Body radius at rest, as a share of half the canvas. */
  scale: number;
  /** Breathing depth, as a share of the radius. */
  breath: number;
  /** Breathing speed, radians per second. */
  breathRate: number;
  /** How far the voice level swells the radius (share of half the canvas at level 1). */
  levelGain: number;
  /** Edge wobble at rest, as a share of the radius. */
  wobble: number;
  /** Extra edge wobble per unit of voice level: the ripple. */
  ripple: number;
  /** Speed of the inner currents, radians per second. */
  flow: number;
  /** Twist of the inner currents toward the centre: the "thinking" swirl. */
  swirl: number;
  /** Depth of the synthetic syllable pulse while the assistant speaks (0 off). */
  pulse: number;
  /** Strength of the halo, 0 to 1. */
  glow: number;
  /** 0 grey to 1 full colour. */
  saturation: number;
  brightness: number;
  /** 0 the accent palette, 1 the error palette. */
  alert: number;
}

// Idle breathes slowly (one breath in about 4.5 s). Listening is the largest and most reactive. Thinking is smaller and calmer
// but its currents twist into a slow swirl, so it reads differently from idle even in a still frame. Speaking pulses with the
// reply. Muted is small, grey and nearly still; error takes the danger colour and stays quiet.
export const STATE_PARAMS: Readonly<Record<VoiceOrbState, Readonly<OrbParams>>> = Object.freeze({
  idle: { scale: 0.58, breath: 0.03, breathRate: 1.4, levelGain: 0, wobble: 0.014, ripple: 0, flow: 0.35, swirl: 0.18, pulse: 0, glow: 0.45, saturation: 1, brightness: 1, alert: 0 },
  listening: { scale: 0.6, breath: 0.012, breathRate: 1.6, levelGain: 0.14, wobble: 0.018, ripple: 0.045, flow: 0.8, swirl: 0.22, pulse: 0, glow: 0.75, saturation: 1, brightness: 1.04, alert: 0 },
  thinking: { scale: 0.55, breath: 0.02, breathRate: 1, levelGain: 0, wobble: 0.012, ripple: 0, flow: 0.55, swirl: 1.1, pulse: 0, glow: 0.5, saturation: 0.92, brightness: 0.98, alert: 0 },
  speaking: { scale: 0.58, breath: 0.01, breathRate: 1.4, levelGain: 0.12, wobble: 0.015, ripple: 0.03, flow: 0.7, swirl: 0.28, pulse: 0.3, glow: 0.65, saturation: 1, brightness: 1.03, alert: 0 },
  muted: { scale: 0.5, breath: 0.012, breathRate: 0.9, levelGain: 0, wobble: 0.006, ripple: 0, flow: 0.12, swirl: 0.1, pulse: 0, glow: 0.12, saturation: 0.12, brightness: 0.92, alert: 0 },
  error: { scale: 0.52, breath: 0.016, breathRate: 0.9, levelGain: 0, wobble: 0.008, ripple: 0, flow: 0.2, swirl: 0.1, pulse: 0, glow: 0.3, saturation: 0.85, brightness: 0.96, alert: 1 },
});

const KEYS = Object.keys(STATE_PARAMS.idle) as (keyof OrbParams)[];

export const clamp01 = (value: number): number => (Number.isFinite(value) ? Math.min(1, Math.max(0, value)) : 0);

/** Ease in and out (cubic): a state change starts and lands softly, never with a jump. */
export const easeInOutCubic = (t: number): number => {
  const x = clamp01(t);
  return x < 0.5 ? 4 * x * x * x : 1 - (-2 * x + 2) ** 3 / 2;
};

export const lerp = (a: number, b: number, t: number): number => a + (b - a) * t;

export const lerpParams = (from: Readonly<OrbParams>, to: Readonly<OrbParams>, t: number): OrbParams => {
  const out = { ...from };
  for (const key of KEYS) out[key] = lerp(from[key], to[key], t);
  return out;
};

/** A cross-fade between two looks. A new state mid-fade starts from wherever the fade has got to. */
export interface OrbTransition {
  from: Readonly<OrbParams>;
  to: Readonly<OrbParams>;
  elapsed: number;
  duration: number;
}

export const TRANSITION_MS = 650;

export const settledTransition = (params: Readonly<OrbParams>): OrbTransition => ({ from: params, to: params, elapsed: 0, duration: 0 });

export const transitionParams = (transition: OrbTransition): OrbParams =>
  transition.duration <= 0 ? { ...transition.to } : lerpParams(transition.from, transition.to, easeInOutCubic(transition.elapsed / transition.duration));

export const retarget = (transition: OrbTransition, to: Readonly<OrbParams>, duration = TRANSITION_MS): OrbTransition => ({
  from: transitionParams(transition),
  to,
  elapsed: 0,
  duration,
});

export const stepTransition = (transition: OrbTransition, dtMs: number): OrbTransition => ({
  ...transition,
  elapsed: Math.min(transition.duration, transition.elapsed + Math.max(0, dtMs)),
});

export const transitionDone = (transition: OrbTransition): boolean => transition.elapsed >= transition.duration;

// The voice level rises fast and falls slowly, like a VU meter: a syllable lands at once, and silence settles in.
export const LEVEL_ATTACK_MS = 70;
export const LEVEL_RELEASE_MS = 320;
// With reduced motion the level only brightens the orb, and slowly: no flicker.
export const STILL_ATTACK_MS = 260;
export const STILL_RELEASE_MS = 720;

/** One step of exponential smoothing toward `target`. Time based: two 16 ms steps equal one 32 ms step. */
export const smoothLevel = (current: number, target: number, dtMs: number, attackMs = LEVEL_ATTACK_MS, releaseMs = LEVEL_RELEASE_MS): number => {
  const goal = clamp01(target);
  if (!(dtMs > 0)) return current;
  const tau = goal > current ? attackMs : releaseMs;
  return goal + (current - goal) * Math.exp(-dtMs / Math.max(1, tau));
};

// A level that is not refreshed goes stale: speech audio arrives in bursts ahead of playback, so the last value would otherwise
// hold the orb swollen and still for the rest of a reply. Levels arrive every 85 to 100 ms while there is sound; after this long
// without one the orb lets the level fall (with the release time), and the syllable pulse of the speaking state takes over.
export const LEVEL_HOLD_MS = 250;

/** The level to aim for, given how long ago it was last set. */
export const heldLevel = (target: number, ageMs: number, holdMs = LEVEL_HOLD_MS): number => (ageMs > holdMs ? 0 : clamp01(target));

// Phases wrap at 20 pi. Every multiplier the shader applies to the flow phase has one decimal, so the wrap is seamless, and the
// number stays small enough for a GPU's float precision over a long conversation.
export const PHASE_WRAP = 20 * Math.PI;

/** Advances a phase by `speed` radians per second. Speeds change smoothly, so the motion never jumps when a state changes. */
export const advancePhase = (phase: number, speed: number, dtMs: number): number => {
  const next = phase + (speed * Math.max(0, dtMs)) / 1000;
  return next >= PHASE_WRAP ? next % PHASE_WRAP : next;
};

/** The clock of the syllable pulse runs at one turn a second. */
export const PULSE_RATE = 2 * Math.PI;

/** The pulse of a speaking reply, 0 to 1: about two syllables a second, slowly varying, never a metronome. */
export const syllable = (phase: number): number => (0.5 + 0.5 * Math.sin(phase * 2.2)) * (0.65 + 0.35 * Math.sin(phase * 0.3 + 1.7));

/** The orb's edge: a sum of slow waves around the circle, -1 to 1. The shader computes the same function. */
export const edgeWave = (angle: number, flow: number): number =>
  Math.sin(3 * angle + flow * 1.3) * 0.5 + Math.sin(5 * angle - flow * 1.7) * 0.3 + Math.sin(2 * angle + flow * 0.7 + 1.1) * 0.2;

/** What one frame draws, in canvas units where half the canvas is 1. */
export interface OrbFrame {
  radius: number;
  /** Edge wobble, as a share of the radius. */
  wobble: number;
  flow: number;
  swirl: number;
  /** Halo opacity before the theme's weighting. */
  glow: number;
}

export interface OrbClock {
  /** Breathing phase, radians. */
  breath: number;
  /** Inner currents and edge, radians (wraps at PHASE_WRAP). */
  flow: number;
  /** Syllable pulse, radians. */
  pulse: number;
}

export const MAX_RADIUS = 0.76;

/** The frame for the current look, voice level and clock. Under reduced motion nothing moves: the level only brightens the halo. */
export const frameOf = (params: Readonly<OrbParams>, level: number, clock: Readonly<OrbClock>, reducedMotion: boolean): OrbFrame => {
  const voice = clamp01(level);
  if (reducedMotion) {
    return {
      radius: params.scale,
      wobble: 0,
      flow: 0,
      swirl: params.swirl,
      glow: clamp01(params.glow + voice * (params.levelGain + params.ripple) * 2.5),
    };
  }
  const heard = Math.max(voice, params.pulse * syllable(clock.pulse));
  const radius = params.scale * (1 + params.breath * Math.sin(clock.breath)) + params.levelGain * heard;
  return {
    radius: Math.min(MAX_RADIUS, radius),
    wobble: params.wobble + params.ripple * heard,
    flow: clock.flow,
    swirl: params.swirl,
    glow: clamp01(params.glow + heard * 0.2),
  };
};

/**
 * The orb state for a voice phase of the kits (`AlixVoicePhase`) and their text fallback. A microphone that cannot be used or a
 * connection that failed is an error (the person can fix it or retry); a deployment with no voice, or a session that ended, is
 * muted. Both kits use this one mapping, so they always agree.
 */
export const voiceOrbStateFor = (phase: string, fallback?: string): VoiceOrbState => {
  if (fallback === "microphone_unavailable" || fallback === "connect_failed") return "error";
  switch (phase) {
    case "listening":
      return "listening";
    case "speaking":
      return "speaking";
    case "processing":
    case "connecting":
    case "reconnecting":
      return "thinking";
    case "unavailable":
    case "ended":
      return "muted";
    default:
      return "idle";
  }
};
