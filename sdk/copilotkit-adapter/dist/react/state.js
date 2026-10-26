import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { UseAgentUpdate, useAgent } from "@copilotkit/react-core/v2";
import { parsePendingState } from "../activities.js";
export const DEFAULT_PENDING_LABELS = {
    confirmation: "Waiting for your confirmation",
    clarification: "Waiting for",
    goal: "Goal",
    collected: "Collected",
    missing: "Still needed",
};
const statusOf = (pending, text) => {
    if (pending.kind === "confirmation")
        return text.confirmation;
    if (pending.kind === "clarification")
        return pending.question === undefined ? text.goal : `${text.clarification} ${pending.question.field}`;
    return pending.phase === "" ? text.goal : `${text.goal}: ${pending.phase}`;
};
/** A one-glance status of the active goal: what it waits for, what it has and what it still needs. */
export const AlixAIPendingCard = ({ pending, labels }) => {
    if (pending === undefined)
        return null;
    const text = { ...DEFAULT_PENDING_LABELS, ...labels };
    return (_jsxs("section", { className: "alixai-pending", "data-alixai": "pending-state", "data-kind": pending.kind, "data-phase": pending.phase, "aria-live": "polite", children: [_jsx("p", { "data-alixai": "pending-status", children: statusOf(pending, text) }), _jsx("p", { "data-alixai": "pending-capability", children: pending.capabilityId }), pending.collectedFields.length > 0 && (_jsxs("p", { "data-alixai": "pending-collected", children: [text.collected, ": ", pending.collectedFields.map((field) => field.name).join(", ")] })), pending.missingFields.length > 0 && (_jsxs("p", { "data-alixai": "pending-missing", children: [text.missing, ": ", pending.missingFields.join(", ")] }))] }));
};
/**
 * The pending goal of the AlixAI session, re-read whenever the shared state changes; undefined while there is none or when the
 * state is not what AlixAI sends. Inside `<CopilotKitProvider>`; `agentId` defaults to CopilotKit's default agent.
 */
export const useAlixAIPendingState = (options = {}) => {
    const { agent } = useAgent({ ...(options.agentId !== undefined && { agentId: options.agentId }), updates: [UseAgentUpdate.OnStateChanged] });
    return parsePendingState(agent.state);
};
/** The card, wired to the agent's state: `<AlixAIPendingState />` anywhere inside the provider, in or outside the chat. */
export const AlixAIPendingState = ({ agentId, labels }) => {
    const pending = useAlixAIPendingState(agentId === undefined ? {} : { agentId });
    return _jsx(AlixAIPendingCard, { pending: pending, ...(labels !== undefined && { labels }) });
};
const InChat = ({ message, position, agentId }) => {
    const { agent } = useAgent({ agentId, updates: [UseAgentUpdate.OnStateChanged, UseAgentUpdate.OnMessagesChanged] });
    // Under the last message of the conversation, once: the state is where the conversation stands now, not where an earlier run left it.
    if (position !== "after" || agent.messages[agent.messages.length - 1]?.id !== message.id)
        return null;
    return _jsx(AlixAIPendingCard, { pending: parsePendingState(agent.state) });
};
/**
 * The chat's state renderer, for the provider's `renderCustomMessages` (or `alixaiProviderProps(props, { showState: true })`):
 * it draws the pending goal under the last message, the v2 counterpart of `useCoAgentStateRender`.
 */
export const createAlixAIStateRenderer = (options = {}) => ({
    ...(options.agentId !== undefined && { agentId: options.agentId }),
    render: InChat,
});
//# sourceMappingURL=state.js.map