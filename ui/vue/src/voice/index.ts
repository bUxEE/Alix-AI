// The voice surface of @alixai/vue (M7-03). The components are also exported from the package root; this file adds what a host
// needs to wire them to a voice session (`voice.createVoiceSession` of @alixai/sdk): the composable, the controller, the browser's
// microphone and loudspeaker, and the types.

export { default as AlixVoiceMode } from "./AlixVoiceMode.vue";
export { default as AlixVoiceButton } from "./AlixVoiceButton.vue";
export { default as AlixTranscriptEditor } from "./AlixTranscriptEditor.vue";
export { default as AlixVoiceOrb } from "./AlixVoiceOrb.vue";
export { voiceOrbStateFor } from "@alixai/voice-orb";
export type { VoiceOrbColors, VoiceOrbState } from "@alixai/voice-orb";
export { useAlixVoice } from "./useAlixVoice";
export type { AlixVoice } from "./useAlixVoice";
export { createAlixVoiceController } from "./controller";
export { createAlixMicrophone, createAlixPlayer } from "./audio-io";
export { VOICE_MESSAGES, useAlixVoiceText } from "./messages";
export type { AlixVoiceMessageKey } from "./messages";
export type * from "./types";
