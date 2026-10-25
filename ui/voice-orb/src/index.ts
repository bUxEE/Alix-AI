// @alixai/voice-orb: the audio-reactive voice orb of the AlixAI UI kits (@alixai/vue's and @alixai/react's AlixVoiceOrb).
//
// One small orb with no framework and no runtime dependency: a WebGL 1 fragment shader, with Canvas 2D gradients where WebGL is
// missing or its context is lost. It breathes when idle, swells and ripples with the person's voice while listening, swirls
// calmly while the assistant thinks, pulses with the reply while it speaks, greys out when muted and turns to the danger colour on
// an error. The kits wrap it as a component; a host may use it directly:
//
//   const orb = createVoiceOrb(element, { colors: readCssColors(element, { accent: "--brand", surface: "--bg", error: "--red" }) });
//   orb.setState("listening");
//   orb.setLevel(0.4);   // every time a level arrives
//   orb.destroy();       // when the element goes away

export { createVoiceOrb } from "./orb.js";
export type { VoiceOrb, VoiceOrbOptions, VoiceOrbRenderer } from "./orb.js";
export { readCssColors, watchPresentation } from "./theme.js";
export type { VoiceOrbTokenNames } from "./theme.js";
export {
  advancePhase,
  clamp01,
  easeInOutCubic,
  edgeWave,
  frameOf,
  heldLevel,
  lerpParams,
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
} from "./motion.js";
export type { OrbClock, OrbFrame, OrbParams, OrbTransition, VoiceOrbState } from "./motion.js";
export { adjust, cssRgb, DEFAULT_BASE, derivePalette, luminance, mix } from "./color.js";
export type { OrbBaseColors, OrbPalette, Rgb, VoiceOrbColors } from "./color.js";
