import type { ReactElement } from "react";
import type { Citation } from "@alixai/sdk";
import type { AlixAIAttachment, AlixAIBlock, AlixAIProgress } from "../activities.js";
export declare const AlixAITable: ({ block }: {
    block: AlixAIBlock<"table">;
}) => ReactElement;
/**
 * A chart block: its description is the accessible name, the data is a table, and a bar chart also draws its bars. The
 * block is declarative data; there is no script and no URL in it to follow.
 */
export declare const AlixAIChart: ({ block }: {
    block: AlixAIBlock<"chart">;
}) => ReactElement;
export declare const AlixAIDiagram: ({ block }: {
    block: AlixAIBlock<"diagram">;
}) => ReactElement;
export declare const AlixAIActionCard: ({ block }: {
    block: AlixAIBlock<"action_card">;
}) => ReactElement;
export interface AlixAIChoicesProps {
    block: AlixAIBlock<"clarification_choices">;
    /** Called with the chosen choice id: the host runs the agent with `forwardedProps.alixai.choiceId` (no model reads it). */
    onChoose: (choiceId: string) => void;
    /** Called with free text, when the block allows it: sent as an ordinary user message. */
    onFreeText?: (text: string) => void;
    sendLabel?: string;
}
/** The form of a clarification: the question, its choices as buttons, and optional free text. A form is data, not executable UI. */
export declare const AlixAIChoices: ({ block, onChoose, onFreeText, sendLabel }: AlixAIChoicesProps) => ReactElement;
/** Citations with the opaque, scope-checked reference of each source. The reference is shown, never opened by this component. */
export declare const AlixAICitationList: ({ citations, onOpen, }: {
    citations: readonly Citation[];
    onOpen?: (citation: Citation) => void;
}) => ReactElement;
export interface AlixAIAttachmentPreviewProps {
    files: readonly AlixAIAttachment[];
    /**
     * Renders the preview of a previewable file, for example an `<img>` whose source your backend serves from AlixAI's files
     * service under the user's own scope. Default: the name only. The adapter never builds a file URL itself.
     */
    renderPreview?: (file: AlixAIAttachment) => ReactElement | null;
}
export declare const AlixAIAttachmentPreview: ({ files, renderPreview }: AlixAIAttachmentPreviewProps) => ReactElement;
export declare const AlixAIProgressLine: ({ progress }: {
    progress: AlixAIProgress;
}) => ReactElement;
//# sourceMappingURL=blocks.d.ts.map