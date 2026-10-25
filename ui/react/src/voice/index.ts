// The voice surface of @alixai/react (M7-03): Voice Mode, the talk button and the transcript editor, the hook that wires them to a
// voice session (`voice.createVoiceSession` of @alixai/sdk), the controller behind it, the browser's microphone and loudspeaker, and
// the words in English and Italian. Styles: `@use "../voice/voice";` in the kit's stylesheet (src/voice/_voice.scss).

export { AlixVoiceMode } from "./AlixVoiceMode.js";
export type { AlixVoiceControlsContext, AlixVoiceModeProps } from "./AlixVoiceMode.js";
export { AlixVoiceButton, AlixVoiceIcon } from "./AlixVoiceButton.js";
export type { AlixVoiceButtonProps } from "./AlixVoiceButton.js";
export { AlixTranscriptEditor } from "./AlixTranscriptEditor.js";
export { AlixVoiceOrb } from "./AlixVoiceOrb.js";
export type { AlixVoiceOrbProps } from "./AlixVoiceOrb.js";
export { voiceOrbStateFor } from "@alixai/voice-orb";
export type { VoiceOrbColors, VoiceOrbState } from "@alixai/voice-orb";
export type { AlixTranscriptEditorHandle, AlixTranscriptEditorProps } from "./AlixTranscriptEditor.js";
export { useAlixVoice } from "./useAlixVoice.js";
export type { AlixVoice } from "./useAlixVoice.js";
export { createAlixVoiceController } from "./controller.js";
export { createAlixMicrophone, createAlixPlayer } from "./audio-io.js";
export { VOICE_MESSAGES, resolveVoiceMessages, useAlixVoiceText, voiceEn, voiceIt } from "./messages.js";
export type { AlixVoiceMessageKey, AlixVoiceMessageOverrides, AlixVoiceMessages, AlixVoiceText } from "./messages.js";
export type * from "./types.js";
