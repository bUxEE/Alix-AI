// @alixai/react: the AlixAI parts of a CopilotKit chat, as React components (master plan 15.4, ADR 0035).
//
// This entry has no CopilotKit and no browser import, so it renders on a server and works without CopilotKit (it does need the
// adapter and, through it, @ag-ui/client and rxjs, which are peer dependencies). The CopilotKit wiring
// (AlixChatPanel, alixProviderProps, useAlixConfirmation, alixKitComponents) is in `@alixai/react/copilotkit`.
//
// CopilotKit is the chat. It provides the chat, the sidebar, the popup and the headless hooks, and with them the message
// stream, the input, the conversation threads and the attachment queue. This kit is what AlixAI adds: the confirmation
// sheet, citations, file previews, the chart, table, diagram and clarification blocks, source health and settings
// panels, registered as CopilotKit generative-UI renderers, with design tokens, English and Italian, and no telemetry.
// Mapping of the 15.4 component list:
//   AlixChatPanel (/copilotkit)  -> CopilotChat / CopilotSidebar / CopilotPopup, themed and wired (ChatPanel)
//   AlixMessageStream            -> CopilotKit's message view (not re-implemented)
//   AlixConversationList         -> AlixConversationList (CopilotKit's thread drawer is the alternative)
//   AlixAttachmentComposer       -> AlixAttachmentComposer (data in, events out)
//   AlixFilePreview              -> AlixFilePreview, AlixFileList
//   AlixCitationList, AlixChartBlock, AlixActionCard, AlixConfirmationSheet, AlixSourceHealthPanel, AlixSettingsPanel
//   AlixVoiceMode, AlixVoiceButton, AlixTranscriptEditor -> M7-03 (src/voice), with AlixVoiceOrb (@alixai/voice-orb)
//   AlixIntegrationAuditPanel    -> AlixIntegrationAuditPanel (GET /v1/settings/integration-audit and /providers, as the Vue kit)
//   AlixReceiptCard              -> the outcome of a confirmed or queued action (the confirmation result, `alixai.receipt`)
//   AlixConfirmationPanel        -> the `confirmation_panel` block; AlixContinueInApp -> the `alixai_continue_in_app` step-up call

export { AlixConfirmationInterrupt, AlixConfirmationSheet, AlixEffectList, AlixRiskMeter } from "./components/Confirmation.js";
export type { AlixConfirmationInterruptProps, AlixConfirmationSheetProps } from "./components/Confirmation.js";
export { AlixActionCard, AlixChartBlock, AlixClarificationForm, AlixDiagramBlock, AlixProgressLine, AlixTableBlock } from "./components/Blocks.js";
export type { AlixActionCardProps, AlixChartBlockProps, AlixClarificationFormProps, AlixDiagramBlockProps, AlixTableBlockProps } from "./components/Blocks.js";
export { AlixCitationList } from "./components/Citations.js";
export type { AlixCitationListProps } from "./components/Citations.js";
export { AlixAttachmentComposer, AlixFileList, AlixFilePreview, fileKindOf } from "./components/Files.js";
export type { AlixAttachmentComposerProps, AlixFileKind, AlixFileListProps, AlixFilePreviewProps } from "./components/Files.js";
export { AlixSettingsPanel, AlixSourceHealthPanel } from "./components/Panels.js";
export { AlixIntegrationAuditPanel } from "./components/Audit.js";
export type { AlixIntegrationAuditPanelProps } from "./components/Audit.js";
export { AlixConfirmationOutcome, AlixReceiptCard, parseReceipt } from "./components/Receipt.js";
export type { AlixConfirmationOutcomeProps, AlixReceiptCardProps } from "./components/Receipt.js";
export { AlixConfirmationPanel, AlixContinueInApp, parseContinueInApp } from "./components/Prepared.js";
export type { AlixConfirmationPanelProps, AlixContinueInAppProps, AlixContinueInAppRequest } from "./components/Prepared.js";
export { AlixLoadable, AlixSkeleton, useAlixLoadState } from "./components/Loadable.js";
export type { AlixLoadState, AlixLoadableProps } from "./components/Loadable.js";
export type { AlixSettingsPanelProps, AlixSourceHealthPanelProps } from "./components/Panels.js";
export { AlixConversationList } from "./components/ConversationList.js";
export type { AlixConversationItem, AlixConversationListProps } from "./components/ConversationList.js";
export { AlixMemoryList, AlixMemoryProposal } from "./components/Memory.js";
export type {
  AlixMemoryListProps,
  AlixMemoryProposalProps,
  MemoryListItem,
  MemoryProposal,
  MemoryProposalStatus,
} from "./components/Memory.js";
export { AlixProvider, formatBytes, formatDateTime, formatNumber, useAlixIntl, useAlixText } from "./i18n/context.js";
export type { AlixIntl, AlixProviderProps } from "./i18n/context.js";
export { BUILT_IN_MESSAGES, en, fill, it, resolveMessages } from "./i18n/messages.js";
export type { AlixMessages, DeepPartial } from "./i18n/messages.js";
export { ALIXAI_TOKENS, themeStyle } from "./tokens.js";
export type { AlixToken } from "./tokens.js";
export * from "./voice/index.js";
export { AlixDictationButton, useAlixDictation, insertAtCaret, insertIntoCopilotTextarea, createAlixMicrophone } from "./dictation/index.js";
export type {
  AlixDictation,
  AlixDictationButtonProps,
  AlixCaptureMicrophone,
  DictationPhase,
  UseAlixDictationOptions,
  VoiceModeOption,
} from "./dictation/index.js";
