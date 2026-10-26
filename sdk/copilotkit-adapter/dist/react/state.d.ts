import type { ReactCustomMessageRenderer } from "@copilotkit/react-core/v2";
import type { ReactElement } from "react";
import type { AlixAIPending } from "../activities.js";
export interface AlixAIPendingLabels {
    /** The status of a goal that waits for the person's confirmation. */
    confirmation: string;
    /** The status of a goal that waits for an answer; the name of the field follows. */
    clarification: string;
    /** The status of any other goal; its phase follows when it has one. */
    goal: string;
    collected: string;
    missing: string;
}
export declare const DEFAULT_PENDING_LABELS: AlixAIPendingLabels;
export interface AlixAIPendingCardProps {
    /** From `parsePendingState` or `useAlixAIPendingState`. Nothing is drawn without it. */
    pending: AlixAIPending | undefined;
    labels?: Partial<AlixAIPendingLabels>;
}
/** A one-glance status of the active goal: what it waits for, what it has and what it still needs. */
export declare const AlixAIPendingCard: ({ pending, labels }: AlixAIPendingCardProps) => ReactElement | null;
/**
 * The pending goal of the AlixAI session, re-read whenever the shared state changes; undefined while there is none or when the
 * state is not what AlixAI sends. Inside `<CopilotKitProvider>`; `agentId` defaults to CopilotKit's default agent.
 */
export declare const useAlixAIPendingState: (options?: {
    agentId?: string;
}) => AlixAIPending | undefined;
/** The card, wired to the agent's state: `<AlixAIPendingState />` anywhere inside the provider, in or outside the chat. */
export declare const AlixAIPendingState: ({ agentId, labels }: {
    agentId?: string;
    labels?: Partial<AlixAIPendingLabels>;
}) => ReactElement | null;
/**
 * The chat's state renderer, for the provider's `renderCustomMessages` (or `alixaiProviderProps(props, { showState: true })`):
 * it draws the pending goal under the last message, the v2 counterpart of `useCoAgentStateRender`.
 */
export declare const createAlixAIStateRenderer: (options?: {
    agentId?: string;
}) => ReactCustomMessageRenderer;
//# sourceMappingURL=state.d.ts.map