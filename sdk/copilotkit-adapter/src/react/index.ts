// @alixai/copilotkit-adapter/react: typed helpers for CopilotKit's React components (needs react and @copilotkit/react-core).
export { AlixAIConfirmationCard, DEFAULT_CONFIRMATION_LABELS } from "./confirmation.js";
export type { AlixAIConfirmationCardProps, AlixAIConfirmationLabels } from "./confirmation.js";
export {
  AlixAIActionCard,
  AlixAIAttachmentPreview,
  AlixAIChart,
  AlixAIChoices,
  AlixAICitationList,
  AlixAIDiagram,
  AlixAIDraft,
  AlixAIKpi,
  AlixAIProgressLine,
  AlixAIProvenance,
  AlixAIRecordCard,
  AlixAITable,
  AlixAITimeline,
} from "./blocks.js";
export type { AlixAIAttachmentPreviewProps, AlixAIChoicesProps } from "./blocks.js";
export { alixaiProviderProps, createAlixAIActivityRenderers, useAlixAIConfirmation, useAlixAISuggestions } from "./hooks.js";
export type { ActivityComponentProps, AlixAIRendererOptions, UseAlixAIConfirmationOptions } from "./hooks.js";
export { guardSchema } from "./schema.js";
export { AlixAIPendingCard, AlixAIPendingState, DEFAULT_PENDING_LABELS, createAlixAIStateRenderer, useAlixAIPendingState } from "./state.js";
export type { AlixAIPendingCardProps, AlixAIPendingLabels } from "./state.js";
