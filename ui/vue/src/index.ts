// @alixai/vue: optional Vue 3 UI kit for AlixAI (master plan 15.4, ADR 0035).
//
// A client of the public API and nothing more. It talks AG-UI to `/v1/agui` through a small client (no CopilotKit, no @ag-ui/*,
// no telemetry), renders the same AlixAI parts as the React kit (confirmation sheet, citations, blocks, receipts), and cannot
// bypass authorisation: an approval is a resume of the interrupt, the confirmation secret stays with the host relay, citations are
// never hidden and never opened by the kit, and routing is the host's. Import `@alixai/vue/styles` once for the design tokens.
//
// Mapping of the 15.4 component list:
//   AlixChatPanel, AlixMessageStream, AlixConversationList, AlixAttachmentComposer, AlixFilePreview, AlixCitationList,
//   AlixChartBlock, AlixActionCard, AlixConfirmationSheet, AlixSettingsPanel, AlixSourceHealthPanel, AlixIntegrationAuditPanel,
//   AlixVoiceMode, AlixVoiceButton, AlixTranscriptEditor (the three voice components are shells that M7-03 completes).

// ---- components ----
export { default as AlixChatPanel } from "./components/AlixChatPanel.vue";
export { default as AlixMessageStream } from "./components/AlixMessageStream.vue";
export { default as AlixConversationList } from "./components/AlixConversationList.vue";
export { default as AlixAttachmentComposer } from "./components/AlixAttachmentComposer.vue";
export { default as AlixFilePreview } from "./components/AlixFilePreview.vue";
export { default as AlixCitationList } from "./components/AlixCitationList.vue";
export { default as AlixActionCard } from "./components/AlixActionCard.vue";
export { default as AlixReceiptCard } from "./components/AlixReceiptCard.vue";
export { default as AlixConfirmationSheet } from "./components/AlixConfirmationSheet.vue";
export { default as AlixMemoryProposal } from "./components/AlixMemoryProposal.vue";
export { default as AlixMemoryList } from "./components/AlixMemoryList.vue";
export { default as AlixSettingsPanel } from "./components/AlixSettingsPanel.vue";
export { default as AlixSourceHealthPanel } from "./components/AlixSourceHealthPanel.vue";
export { default as AlixIntegrationAuditPanel } from "./components/AlixIntegrationAuditPanel.vue";
export { default as AlixProvider } from "./components/AlixProvider.vue";
export { default as AlixChartBlock } from "./components/blocks/AlixChartBlock.vue";
export { default as AlixTableBlock } from "./components/blocks/AlixTableBlock.vue";
export { default as AlixDiagramBlock } from "./components/blocks/AlixDiagramBlock.vue";
export { default as AlixClarificationChoices } from "./components/blocks/AlixClarificationChoices.vue";
export { default as AlixMarkdown } from "./components/blocks/AlixMarkdown.vue";
export { default as AlixLoadable } from "./components/common/AlixLoadable.vue";
export { default as AlixSectionLoader } from "./components/common/AlixSectionLoader.vue";
export { default as AlixSkeleton } from "./components/common/AlixSkeleton.vue";
export { default as AlixSwitch } from "./components/common/AlixSwitch.vue";
export { default as AlixVoiceMode } from "./voice/AlixVoiceMode.vue";
export { default as AlixVoiceButton } from "./voice/AlixVoiceButton.vue";
export { default as AlixTranscriptEditor } from "./voice/AlixTranscriptEditor.vue";
export { default as AlixVoiceOrb } from "./voice/AlixVoiceOrb.vue";
export { default as AlixDictationButton } from "./dictation/AlixDictationButton.vue";
export { useAlixDictation, insertAtCaret } from "./dictation";
export type { AlixDictation, DictationPhase, UseAlixDictationOptions, VoiceModeOption } from "./dictation";

// ---- the AG-UI client and the state it feeds ----
export { AguiHttpError, CONFIRMATION_TOKEN_HEADER, createAguiClient } from "./agui/client";
export type { AguiClient, AguiClientOptions, AguiOperation, AguiRequestInfo, RunHandlers, RunResult } from "./agui/client";
export { applyEvent, createChatState } from "./agui/reducer";
export { applyPatch } from "./agui/json-patch";
export * as parsers from "./agui/parse";
export type * from "./agui/types";

// ---- composables ----
export { useAlixChat, MAX_ATTACHMENTS } from "./composables/useAlixChat";
export type { AlixChat, AlixChatOptions, AttachmentKind, OutgoingAttachment, SendOptions } from "./composables/useAlixChat";
export { useAttachmentQueue } from "./composables/useAttachmentQueue";
export type { AttachmentQueue, PendingAttachment } from "./composables/useAttachmentQueue";
export { useLoadFromProps, useLoadState } from "./composables/useLoadState";
export type { LoadState } from "./composables/useLoadState";
export { useOnline, useReducedMotion } from "./composables/useBrowser";

// ---- localisation and helpers ----
export { BUILT_IN_MESSAGES, createAlixConfig, provideAlix, useAlix } from "./i18n";
export type { AlixConfig, AlixConfigOptions, MessageKey, Messages } from "./i18n";
export { isSafeHref, parseMarkdown } from "./markdown";
export { buildChart } from "./chart";
export { SETTINGS_SECTIONS } from "./settings-fields";
export type * from "./sdk-types";
export * from "./voice";
