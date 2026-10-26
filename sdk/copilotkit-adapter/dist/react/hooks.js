import { jsx as _jsx } from "react/jsx-runtime";
import { useAgent, useConfigureSuggestions, useCopilotKit, useInterrupt, useRenderTool } from "@copilotkit/react-core/v2";
import { useEffect, useState } from "react";
import { ALIXAI_ACTIVITY, ALIXAI_BLOCK_TYPES, ALIXAI_CUSTOM_EVENT, blockActivityType, parseAttachments, parseBlock, parseCitations, parseProgress, parseSuggestions, } from "../activities.js";
import { CONFIRM_TOOL_NAME } from "../agent.js";
import { withoutCloud } from "../telemetry.js";
import { AlixAIActionCard, AlixAIAttachmentPreview, AlixAIChart, AlixAICitationList, AlixAIChoices, AlixAIDiagram, AlixAIProgressLine, AlixAITable, } from "./blocks.js";
import { AlixAIConfirmationCard } from "./confirmation.js";
import { anySchema, guardSchema } from "./schema.js";
import { createAlixAIStateRenderer } from "./state.js";
/**
 * Renders the confirmation card in the chat when a run ends in an interrupt, and answers it: Approve resolves the
 * interrupt with {"approved": true}, Decline with {"approved": false}; CopilotKit then resumes the run. The secret
 * the gateway issued is not involved: it is attached by AlixAIAgent in the host backend. The confirmation tool call
 * (alixai_confirm_action) is rendered as a one-line status, so CopilotKit has a renderer for it.
 */
export const useAlixAIConfirmation = (options = {}) => {
    useInterrupt({
        render: options.render ??
            ((props) => (_jsx(AlixAIConfirmationCard, { interrupt: props.interrupt, resolve: props.resolve, cancel: props.cancel, ...(options.labels !== undefined && { labels: options.labels }) }))),
    });
    useRenderTool({
        name: CONFIRM_TOOL_NAME,
        parameters: anySchema,
        render: (props) => {
            if (props.status !== "complete")
                return null;
            let outcome = "";
            try {
                const parsed = JSON.parse(props.result);
                outcome = typeof parsed === "object" && parsed !== null && typeof parsed.status === "string" ? parsed.status : "";
            }
            catch {
                outcome = "";
            }
            return outcome === "" ? null : (_jsx("p", { className: "alixai-confirmation-outcome", "data-alixai": "confirmation-outcome", "data-status": outcome, children: outcome }));
        },
    });
};
/**
 * The follow-up suggestions of the last run (the `alixai.suggestions` custom event: computed from the capabilities the
 * caller may use, no model call) as CopilotKit's suggestion pills. Picking one sends its `message` as an ordinary user message.
 */
export const useAlixAISuggestions = () => {
    const { agent } = useAgent();
    const [suggestions, setSuggestions] = useState([]);
    useEffect(() => {
        const subscription = agent.subscribe({
            onRunStartedEvent: () => setSuggestions([]),
            onCustomEvent: ({ event }) => {
                if (event.name === ALIXAI_CUSTOM_EVENT.suggestions)
                    setSuggestions(parseSuggestions(event.value));
            },
        });
        return () => subscription.unsubscribe();
    }, [agent]);
    useConfigureSuggestions({ suggestions: suggestions.map((s) => ({ title: s.title, message: s.message })), available: "always" }, [suggestions]);
    return suggestions;
};
const ChoicesActivity = ({ block, agent }) => {
    const { copilotkit } = useCopilotKit();
    if (agent === undefined)
        return null;
    const target = agent;
    return (_jsx(AlixAIChoices, { block: block, onChoose: (choiceId) => void copilotkit.runAgent({ agent: target, forwardedProps: { alixai: { choiceId } } }), onFreeText: (text) => {
            target.addMessage({ id: crypto.randomUUID(), role: "user", content: text });
            void copilotkit.runAgent({ agent: target });
        } }));
};
/**
 * Activity renderers for the provider's `renderActivityMessages`: the validated blocks (`alixai.block.<type>`: table, chart,
 * diagram, action card, clarification form, citation list; the confirmation panel is the interrupt card's job), the citations,
 * the attachments and, if asked, the progress. A payload that is not what AlixAI sends is refused by the schema and renders nothing.
 */
export const createAlixAIActivityRenderers = (options = {}) => {
    const scope = options.agentId === undefined ? {} : { agentId: options.agentId };
    const custom = options.components ?? {};
    const renderers = [];
    const add = (activityType, guard, what, Default) => {
        const Component = custom[activityType] ?? Default;
        renderers.push({
            activityType,
            ...scope,
            content: guardSchema(guard, what),
            render: ({ content, agent }) => _jsx(Component, { content: content, agent: agent }),
        });
    };
    const blockDefaults = {
        table: ({ content }) => _jsx(AlixAITable, { block: content }),
        chart: ({ content }) => _jsx(AlixAIChart, { block: content }),
        diagram: ({ content }) => _jsx(AlixAIDiagram, { block: content }),
        action_card: ({ content }) => _jsx(AlixAIActionCard, { block: content }),
        clarification_choices: ({ content, agent }) => _jsx(ChoicesActivity, { block: content, agent: agent }),
        citation_list: ({ content }) => _jsx(AlixAICitationList, { citations: content.citations, ...(options.onOpenCitation !== undefined && { onOpen: options.onOpenCitation }) }),
        confirmation_panel: () => null,
    };
    for (const type of ALIXAI_BLOCK_TYPES) {
        add(blockActivityType(type), (value) => parseBlock(type, value), `${type} block`, blockDefaults[type]);
    }
    add(ALIXAI_ACTIVITY.citations, (value) => parseCitations(value), "citations", ({ content }) => (_jsx(AlixAICitationList, { citations: content, ...(options.onOpenCitation !== undefined && { onOpen: options.onOpenCitation }) })));
    add(ALIXAI_ACTIVITY.attachments, (value) => parseAttachments(value), "attachments", ({ content }) => (_jsx(AlixAIAttachmentPreview, { files: content, ...(options.renderPreview !== undefined && { renderPreview: options.renderPreview }) })));
    add(ALIXAI_ACTIVITY.progress, (value) => parseProgress(value), "progress", ({ content }) => options.showProgress === true ? _jsx(AlixAIProgressLine, { progress: content }) : null);
    return renderers;
};
/**
 * Props for `<CopilotKitProvider>` (or `<CopilotKit>`): no cloud keys, the inspector and dev console off, and the AlixAI
 * activity renderers in front of any you pass. `<CopilotKitProvider {...alixaiProviderProps({ runtimeUrl, agentId })}>`.
 */
export const alixaiProviderProps = (props, options = {}) => ({
    ...withoutCloud(props),
    renderActivityMessages: [
        ...createAlixAIActivityRenderers(options),
        ...(props.renderActivityMessages ?? []),
    ],
    ...(options.showState === true && {
        renderCustomMessages: [
            createAlixAIStateRenderer(options.agentId === undefined ? {} : { agentId: options.agentId }),
            ...(props.renderCustomMessages ?? []),
        ],
    }),
});
//# sourceMappingURL=hooks.js.map