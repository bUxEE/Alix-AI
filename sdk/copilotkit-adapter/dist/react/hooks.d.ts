import type { AbstractAgent } from "@ag-ui/client";
import { useInterrupt } from "@copilotkit/react-core/v2";
import type { ReactActivityMessageRenderer, ReactCustomMessageRenderer } from "@copilotkit/react-core/v2";
import type { ComponentType } from "react";
import type { AlixAISuggestion } from "../activities.js";
import { withoutCloud } from "../telemetry.js";
import { AlixAICitationList } from "./blocks.js";
import type { AlixAIAttachmentPreviewProps } from "./blocks.js";
import type { AlixAIConfirmationLabels } from "./confirmation.js";
export interface UseAlixAIConfirmationOptions {
    labels?: Partial<AlixAIConfirmationLabels>;
    /** Replaces the card. It receives the render props of `useInterrupt`; answer with `resolve({ approved: true | false })`. */
    render?: Parameters<typeof useInterrupt>[0]["render"];
}
/**
 * Renders the confirmation card in the chat when a run ends in an interrupt, and answers it: Approve resolves the
 * interrupt with {"approved": true}, Decline with {"approved": false}; CopilotKit then resumes the run. The secret
 * the gateway issued is not involved: it is attached by AlixAIAgent in the host backend. The confirmation tool call
 * (alixai_confirm_action) is rendered as a one-line status, so CopilotKit has a renderer for it.
 */
export declare const useAlixAIConfirmation: (options?: UseAlixAIConfirmationOptions) => void;
/**
 * The follow-up suggestions of the last run (the `alixai.suggestions` custom event: computed from the capabilities the
 * caller may use, no model call) as CopilotKit's suggestion pills. Picking one sends its `message` as an ordinary user message.
 */
export declare const useAlixAISuggestions: () => readonly AlixAISuggestion[];
type AgentOf = AbstractAgent;
export interface ActivityComponentProps<T> {
    content: T;
    agent: AgentOf | undefined;
}
export interface AlixAIRendererOptions {
    agentId?: string;
    /** Replaces the renderer of an activity type (for example `alixai.block.table`) with your own component. */
    components?: Readonly<Record<string, ComponentType<ActivityComponentProps<never>>>>;
    /** The previews of attachments: see AlixAIAttachmentPreview. */
    renderPreview?: AlixAIAttachmentPreviewProps["renderPreview"];
    /** Show the progress activities (off by default: the chat shows the answer, not the pipeline). */
    showProgress?: boolean;
    /** Draw the pending goal (what the conversation waits for) under the last message of the chat, from the shared state (off by default). */
    showState?: boolean;
    /** Called when the person opens a citation (the host resolves the opaque reference under the user's own scope). */
    onOpenCitation?: Parameters<typeof AlixAICitationList>[0]["onOpen"];
}
/**
 * Activity renderers for the provider's `renderActivityMessages`: the validated blocks (`alixai.block.<type>`: table, chart,
 * diagram, action card, clarification form, citation list; the confirmation panel is the interrupt card's job), the citations,
 * the attachments and, if asked, the progress. A payload that is not what AlixAI sends is refused by the schema and renders nothing.
 */
export declare const createAlixAIActivityRenderers: (options?: AlixAIRendererOptions) => ReactActivityMessageRenderer<any>[];
/**
 * Props for `<CopilotKitProvider>` (or `<CopilotKit>`): no cloud keys, the inspector and dev console off, and the AlixAI
 * activity renderers in front of any you pass. `<CopilotKitProvider {...alixaiProviderProps({ runtimeUrl, agentId })}>`.
 */
export declare const alixaiProviderProps: <T extends object>(props: T, options?: AlixAIRendererOptions) => ReturnType<typeof withoutCloud<T>> & {
    renderActivityMessages: ReactActivityMessageRenderer<any>[];
    renderCustomMessages?: ReactCustomMessageRenderer[];
};
export {};
//# sourceMappingURL=hooks.d.ts.map