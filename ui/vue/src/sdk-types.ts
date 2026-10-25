import type { RichBlock } from "@alixai/sdk";

// The SDK's wire types the kit renders, named once. Everything here is derived from the public SDK (and so from the frozen v1
// schemas): the kit declares no wire shape of its own beyond what AG-UI adds around them (see agui/types.ts).

export type { ActionReceipt, Citation, CitationLink, Effect, Fact, FileRef, RichBlock, Choice } from "@alixai/sdk";

export type ChartBlock = Extract<RichBlock, { type: "chart" }>;
export type TableBlock = Extract<RichBlock, { type: "table" }>;
export type DiagramBlock = Extract<RichBlock, { type: "diagram" }>;
export type ActionCardBlock = Extract<RichBlock, { type: "action_card" }>;
export type ClarificationChoicesBlock = Extract<RichBlock, { type: "clarification_choices" }>;
export type ConfirmationPanelBlock = Extract<RichBlock, { type: "confirmation_panel" }>;
export type ChartChannel = NonNullable<ChartBlock["encoding"]["x"]>;
export type ChartRecord = ChartBlock["data"][number];
export type TableCell = TableBlock["rows"][number][number];
export type PermittedAction = ActionCardBlock["actions"][number];
export type { SettingsAuditFinding, SettingsAuditStatus, SettingsProviderHealth, SettingsEffective } from "@alixai/sdk";
export type UserSettings = import("@alixai/sdk").SettingsUserRecord["settings"];
export type SourceStatus = import("@alixai/sdk").rag.SourceStatus;
