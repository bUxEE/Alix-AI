import type { AbstractAgent } from "@ag-ui/client";
import { useAgent, useConfigureSuggestions, useCopilotKit, useInterrupt, useRenderTool } from "@copilotkit/react-core/v2";
import type { ReactActivityMessageRenderer, ReactCustomMessageRenderer } from "@copilotkit/react-core/v2";
import { useEffect, useState } from "react";
import type { ComponentType, ReactElement } from "react";
import {
  ALIXAI_ACTIVITY,
  ALIXAI_BLOCK_TYPES,
  ALIXAI_CUSTOM_EVENT,
  blockActivityType,
  parseAttachments,
  parseBlock,
  parseCitations,
  parseProgress,
  parseSuggestions,
} from "../activities.js";
import type { AlixAIAttachment, AlixAIBlock, AlixAIBlockType, AlixAIInterrupt, AlixAIProgress, AlixAISuggestion } from "../activities.js";
import { CONFIRM_TOOL_NAME } from "../agent.js";
import { withoutCloud } from "../telemetry.js";
import {
  AlixAIActionCard,
  AlixAIAttachmentPreview,
  AlixAIChart,
  AlixAICitationList,
  AlixAIChoices,
  AlixAIDiagram,
  AlixAIProgressLine,
  AlixAITable,
} from "./blocks.js";
import type { AlixAIAttachmentPreviewProps } from "./blocks.js";
import { AlixAIConfirmationCard } from "./confirmation.js";
import type { AlixAIConfirmationLabels } from "./confirmation.js";
import { anySchema, guardSchema } from "./schema.js";
import { createAlixAIStateRenderer } from "./state.js";

// The CopilotKit side, on the names of the pinned release (@copilotkit/react-core/v2; compat/matrix.json records which
// versions were tested): useInterrupt for the human approval, useRenderTool for the confirmation tool call,
// useConfigureSuggestions and the agent's custom events for suggestions, and the provider's renderActivityMessages for the
// structured blocks.

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
export const useAlixAIConfirmation = (options: UseAlixAIConfirmationOptions = {}): void => {
  useInterrupt({
    render:
      options.render ??
      ((props) => (
        <AlixAIConfirmationCard
          interrupt={props.interrupt as AlixAIInterrupt | null}
          resolve={props.resolve}
          cancel={props.cancel}
          {...(options.labels !== undefined && { labels: options.labels })}
        />
      )),
  });
  useRenderTool({
    name: CONFIRM_TOOL_NAME,
    parameters: anySchema,
    render: (props) => {
      if (props.status !== "complete") return null;
      let outcome = "";
      try {
        const parsed: unknown = JSON.parse(props.result);
        outcome = typeof parsed === "object" && parsed !== null && typeof (parsed as { status?: unknown }).status === "string" ? (parsed as { status: string }).status : "";
      } catch {
        outcome = "";
      }
      return outcome === "" ? null : (
        <p className="alixai-confirmation-outcome" data-alixai="confirmation-outcome" data-status={outcome}>
          {outcome}
        </p>
      );
    },
  });
};

/**
 * The follow-up suggestions of the last run (the `alixai.suggestions` custom event: computed from the capabilities the
 * caller may use, no model call) as CopilotKit's suggestion pills. Picking one sends its `message` as an ordinary user message.
 */
export const useAlixAISuggestions = (): readonly AlixAISuggestion[] => {
  const { agent } = useAgent();
  const [suggestions, setSuggestions] = useState<AlixAISuggestion[]>([]);
  useEffect(() => {
    const subscription = agent.subscribe({
      onRunStartedEvent: () => setSuggestions([]),
      onCustomEvent: ({ event }) => {
        if (event.name === ALIXAI_CUSTOM_EVENT.suggestions) setSuggestions(parseSuggestions(event.value));
      },
    });
    return () => subscription.unsubscribe();
  }, [agent]);
  useConfigureSuggestions({ suggestions: suggestions.map((s) => ({ title: s.title, message: s.message })), available: "always" }, [suggestions]);
  return suggestions;
};

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

const ChoicesActivity = ({ block, agent }: { block: AlixAIBlock<"clarification_choices">; agent: AgentOf | undefined }): ReactElement | null => {
  const { copilotkit } = useCopilotKit();
  if (agent === undefined) return null;
  const target = agent as unknown as Parameters<typeof copilotkit.runAgent>[0]["agent"];
  return (
    <AlixAIChoices
      block={block}
      onChoose={(choiceId) => void copilotkit.runAgent({ agent: target, forwardedProps: { alixai: { choiceId } } })}
      onFreeText={(text) => {
        target.addMessage({ id: crypto.randomUUID(), role: "user", content: text });
        void copilotkit.runAgent({ agent: target });
      }}
    />
  );
};

/**
 * Activity renderers for the provider's `renderActivityMessages`: the validated blocks (`alixai.block.<type>`: table, chart,
 * diagram, action card, clarification form, citation list; the confirmation panel is the interrupt card's job), the citations,
 * the attachments and, if asked, the progress. A payload that is not what AlixAI sends is refused by the schema and renders nothing.
 */
export const createAlixAIActivityRenderers = (options: AlixAIRendererOptions = {}): ReactActivityMessageRenderer<any>[] => {
  const scope = options.agentId === undefined ? {} : { agentId: options.agentId };
  const custom = options.components ?? {};
  const renderers: ReactActivityMessageRenderer<any>[] = [];
  const add = <T,>(activityType: string, guard: (value: unknown) => T | undefined, what: string, Default: ComponentType<ActivityComponentProps<T>>): void => {
    const Component = (custom[activityType] as unknown as ComponentType<ActivityComponentProps<T>> | undefined) ?? Default;
    renderers.push({
      activityType,
      ...scope,
      content: guardSchema(guard, what),
      render: ({ content, agent }) => <Component content={content as T} agent={agent as AgentOf | undefined} />,
    });
  };

  const blockDefaults: { [K in AlixAIBlockType]: ComponentType<ActivityComponentProps<AlixAIBlock<K>>> } = {
    table: ({ content }) => <AlixAITable block={content} />,
    chart: ({ content }) => <AlixAIChart block={content} />,
    diagram: ({ content }) => <AlixAIDiagram block={content} />,
    action_card: ({ content }) => <AlixAIActionCard block={content} />,
    clarification_choices: ({ content, agent }) => <ChoicesActivity block={content} agent={agent} />,
    citation_list: ({ content }) => <AlixAICitationList citations={content.citations} {...(options.onOpenCitation !== undefined && { onOpen: options.onOpenCitation })} />,
    confirmation_panel: () => null,
  };
  for (const type of ALIXAI_BLOCK_TYPES) {
    add(blockActivityType(type), (value) => parseBlock(type, value), `${type} block`, blockDefaults[type] as ComponentType<ActivityComponentProps<AlixAIBlock>>);
  }
  add(ALIXAI_ACTIVITY.citations, (value) => parseCitations(value), "citations", ({ content }) => (
    <AlixAICitationList citations={content} {...(options.onOpenCitation !== undefined && { onOpen: options.onOpenCitation })} />
  ));
  add<AlixAIAttachment[]>(ALIXAI_ACTIVITY.attachments, (value) => parseAttachments(value), "attachments", ({ content }) => (
    <AlixAIAttachmentPreview files={content} {...(options.renderPreview !== undefined && { renderPreview: options.renderPreview })} />
  ));
  add<AlixAIProgress>(ALIXAI_ACTIVITY.progress, (value) => parseProgress(value), "progress", ({ content }) =>
    options.showProgress === true ? <AlixAIProgressLine progress={content} /> : null,
  );
  return renderers;
};

/**
 * Props for `<CopilotKitProvider>` (or `<CopilotKit>`): no cloud keys, the inspector and dev console off, and the AlixAI
 * activity renderers in front of any you pass. `<CopilotKitProvider {...alixaiProviderProps({ runtimeUrl, agentId })}>`.
 */
export const alixaiProviderProps = <T extends object>(
  props: T,
  options: AlixAIRendererOptions = {},
): ReturnType<typeof withoutCloud<T>> & { renderActivityMessages: ReactActivityMessageRenderer<any>[]; renderCustomMessages?: ReactCustomMessageRenderer[] } => ({
  ...withoutCloud(props),
  renderActivityMessages: [
    ...createAlixAIActivityRenderers(options),
    ...((props as { renderActivityMessages?: ReactActivityMessageRenderer<any>[] }).renderActivityMessages ?? []),
  ],
  ...(options.showState === true && {
    renderCustomMessages: [
      createAlixAIStateRenderer(options.agentId === undefined ? {} : { agentId: options.agentId }),
      ...((props as { renderCustomMessages?: ReactCustomMessageRenderer[] }).renderCustomMessages ?? []),
    ],
  }),
});
