import { useCopilotKit, useInterrupt, useRenderTool } from "@copilotkit/react-core/v2";
import type { ReactActivityMessageRenderer } from "@copilotkit/react-core/v2";
import type { ComponentType, ReactElement } from "react";
import { alixaiProviderProps, guardSchema } from "@alixai/copilotkit-adapter/react";
import type { ActivityComponentProps, AlixAIRendererOptions } from "@alixai/copilotkit-adapter/react";
import { ALIXAI_ACTIVITY, CONFIRM_TOOL_NAME, blockActivityType } from "@alixai/copilotkit-adapter";
import type { AlixAIAttachment, AlixAIBlock, AlixAIInterrupt, AlixAIProgress } from "@alixai/copilotkit-adapter";
import type { ActionReceipt, Citation } from "@alixai/sdk";
import { AlixActionCard, AlixChartBlock, AlixClarificationForm, AlixDiagramBlock, AlixProgressLine, AlixTableBlock } from "./components/Blocks.js";
import { AlixCitationList } from "./components/Citations.js";
import { AlixConfirmationInterrupt } from "./components/Confirmation.js";
import { AlixFileList } from "./components/Files.js";
import type { AlixFilePreviewProps } from "./components/Files.js";
import { AlixConfirmationPanel, AlixContinueInApp, parseContinueInApp } from "./components/Prepared.js";
import type { AlixContinueInAppRequest } from "./components/Prepared.js";
import { AlixConfirmationOutcome, AlixReceiptCard, parseReceipt } from "./components/Receipt.js";
import type { AlixActionCardProps as ActionCardProps } from "./components/Blocks.js";

// The kit as CopilotKit generative-UI renderers. CopilotKit stays the chat (messages, input, sidebar, popup, headless); the
// AlixAI parts register into it through the adapter's `renderActivityMessages` and `useInterrupt` helpers, so a block the
// core validated is drawn by the kit and anything else renders nothing.

/** The step-up call of AlixAI: the AI path cannot complete the prepared action, the application can. */
export const CONTINUE_IN_APP_TOOL_NAME = "alixai_continue_in_app";

export interface AlixKitOptions {
  /** The agent the renderers belong to (omit for all). */
  agentId?: string;
  /** The person opened a citation: the host resolves the opaque reference under the user's own scope. */
  onOpenCitation?: (citation: Citation) => void;
  /** The person picked a permitted next action of an action card. The host routes it through the action protocol. */
  onAction?: ActionCardProps["onAction"];
  /** What to draw for a previewable attachment (the kit never builds a file URL). */
  renderPreview?: AlixFilePreviewProps["renderPreview"];
  /** Show the progress activities (off by default: the chat shows the answer, not the pipeline). */
  showProgress?: boolean;
  /** Draw the pending goal under the last message, from the shared state (off by default). */
  showState?: boolean;
  /**
   * The person chose to continue a step-up action in the application (`alixai_continue_in_app`). The host opens its own
   * authenticated interface; without it the step-up card only explains.
   */
  onContinueInApp?: (request: AlixContinueInAppRequest) => void;
  /** Your own component for an activity type (for example `alixai.block.table` or `alixai.receipt`); it wins over the kit's. */
  components?: AlixAIRendererOptions["components"];
}

type AgentLike = ActivityComponentProps<never>["agent"];

const ChoicesActivity = ({ block, agent }: { block: AlixAIBlock<"clarification_choices">; agent: AgentLike }): ReactElement | null => {
  const { copilotkit } = useCopilotKit();
  if (agent === undefined) return null;
  const target = agent as unknown as Parameters<typeof copilotkit.runAgent>[0]["agent"];
  return (
    <AlixClarificationForm
      block={block}
      onChoose={(choiceId) => void copilotkit.runAgent({ agent: target, forwardedProps: { alixai: { choiceId } } })}
      onFreeText={(text) => {
        target.addMessage({ id: crypto.randomUUID(), role: "user", content: text });
        void copilotkit.runAgent({ agent: target });
      }}
    />
  );
};

type Renderer<T> = ComponentType<ActivityComponentProps<T>>;

/** The kit's component for each activity type of AlixAI (a map for the adapter's `components` option). */
export const alixKitComponents = (options: AlixKitOptions = {}): Record<string, ComponentType<ActivityComponentProps<never>>> => {
  const table: Renderer<AlixAIBlock<"table">> = ({ content }) => <AlixTableBlock block={content} />;
  const chart: Renderer<AlixAIBlock<"chart">> = ({ content }) => <AlixChartBlock block={content} />;
  const diagram: Renderer<AlixAIBlock<"diagram">> = ({ content }) => <AlixDiagramBlock block={content} />;
  const action: Renderer<AlixAIBlock<"action_card">> = ({ content }) => <AlixActionCard block={content} {...(options.onAction !== undefined && { onAction: options.onAction })} />;
  const choices: Renderer<AlixAIBlock<"clarification_choices">> = ({ content, agent }) => <ChoicesActivity block={content} agent={agent} />;
  const citationBlock: Renderer<AlixAIBlock<"citation_list">> = ({ content }) => (
    <AlixCitationList citations={content.citations} {...(options.onOpenCitation !== undefined && { onOpenCitation: options.onOpenCitation })} />
  );
  const panel: Renderer<AlixAIBlock<"confirmation_panel">> = ({ content }) => <AlixConfirmationPanel block={content} />;
  const citations: Renderer<Citation[]> = ({ content }) => (
    <AlixCitationList citations={content} {...(options.onOpenCitation !== undefined && { onOpenCitation: options.onOpenCitation })} />
  );
  const attachments: Renderer<AlixAIAttachment[]> = ({ content }) => <AlixFileList files={content} {...(options.renderPreview !== undefined && { renderPreview: options.renderPreview })} />;
  const progress: Renderer<AlixAIProgress> = ({ content }) => (options.showProgress === true ? <AlixProgressLine progress={content} /> : null);
  const receipt: Renderer<ActionReceipt> = ({ content }) => <AlixReceiptCard receipt={content} />;
  const map: Record<string, unknown> = {
    [blockActivityType("table")]: table,
    [blockActivityType("chart")]: chart,
    [blockActivityType("diagram")]: diagram,
    [blockActivityType("action_card")]: action,
    [blockActivityType("clarification_choices")]: choices,
    [blockActivityType("citation_list")]: citationBlock,
    [blockActivityType("confirmation_panel")]: panel,
    [ALIXAI_ACTIVITY.citations]: citations,
    [ALIXAI_ACTIVITY.attachments]: attachments,
    [ALIXAI_ACTIVITY.progress]: progress,
    [ALIXAI_ACTIVITY.receipt]: receipt,
  };
  return { ...(map as Record<string, ComponentType<ActivityComponentProps<never>>>), ...(options.components ?? {}) };
};

/** The renderer of the `alixai.receipt` activity, which the adapter does not register: the kit adds it next to the adapter's. */
const receiptRenderer = (components: Record<string, ComponentType<ActivityComponentProps<never>>>, agentId: string | undefined): ReactActivityMessageRenderer<ActionReceipt> => {
  const Component = components[ALIXAI_ACTIVITY.receipt] as unknown as Renderer<ActionReceipt>;
  return {
    activityType: ALIXAI_ACTIVITY.receipt,
    ...(agentId !== undefined && { agentId }),
    content: guardSchema(parseReceipt, "receipt"),
    render: ({ content, agent }) => <Component content={content} agent={agent} />,
  };
};

/**
 * Props for `<CopilotKitProvider>` (or `<CopilotKit>`): no cloud keys, the inspector and dev console off (the adapter's
 * `alixaiProviderProps`), and the kit's components registered for the activity types AlixAI sends, the receipt included.
 * `<CopilotKitProvider {...alixProviderProps({ runtimeUrl, agentId })}>`
 */
export const alixProviderProps = <T extends object>(props: T, options: AlixKitOptions = {}): ReturnType<typeof alixaiProviderProps<T>> => {
  const components = alixKitComponents(options);
  const own = (props as { renderActivityMessages?: ReactActivityMessageRenderer<any>[] }).renderActivityMessages ?? [];
  return alixaiProviderProps(
    { ...props, renderActivityMessages: [receiptRenderer(components, options.agentId), ...own] },
    {
      ...(options.agentId !== undefined && { agentId: options.agentId }),
      ...(options.showState !== undefined && { showState: options.showState }),
      ...(options.showProgress !== undefined && { showProgress: options.showProgress }),
      ...(options.renderPreview !== undefined && { renderPreview: options.renderPreview }),
      ...(options.onOpenCitation !== undefined && { onOpenCitation: options.onOpenCitation }),
      components,
    },
  );
};

/** A schema that accepts any value: the tool arguments are only read by the card, which keeps what it understands. */
const anySchema = guardSchema((value: unknown) => value, "value");

const parseJson = (raw: string): unknown => {
  try {
    return JSON.parse(raw) as unknown;
  } catch {
    return undefined;
  }
};

/** What the confirmation call's result draws: the receipt of a committed action, or one line for any other ending. */
const ConfirmationResult = ({ result }: { result: string }): ReactElement | null => {
  const parsed = parseJson(result);
  const receipt = parseReceipt(parsed);
  if (receipt !== undefined) return <AlixReceiptCard receipt={receipt} />;
  const status = typeof parsed === "object" && parsed !== null && typeof (parsed as { status?: unknown }).status === "string" ? (parsed as { status: string }).status : "";
  return status === "" ? null : <AlixConfirmationOutcome status={status} />;
};

export interface UseAlixConfirmationOptions {
  /** The agent the renderers belong to (omit for all). */
  agentId?: string;
  /** The person chose to continue a step-up action in the application. Without it the step-up card only explains. */
  onContinueInApp?: (request: AlixContinueInAppRequest) => void;
}

/**
 * Renders the kit's confirmation sheet when a run ends in an interrupt and answers it ({"approved": true | false}), then
 * CopilotKit resumes the run. The confirmation call's result is drawn as the receipt card (or one line when it was declined or
 * failed), and the step-up call (`alixai_continue_in_app`) as the "continue in the application" card. Call it once, inside the
 * provider. The confirmation secret is never here: AlixAIAgent attaches it in the host's backend.
 */
export const useAlixConfirmation = (options: UseAlixConfirmationOptions = {}): void => {
  const scope = options.agentId === undefined ? {} : { agentId: options.agentId };
  const onContinue = options.onContinueInApp;
  useInterrupt({
    render: (props) => <AlixConfirmationInterrupt interrupt={props.interrupt as AlixAIInterrupt | null} resolve={props.resolve} cancel={props.cancel} />,
    ...scope,
  });
  useRenderTool(
    {
      name: CONFIRM_TOOL_NAME,
      parameters: anySchema,
      ...scope,
      render: (props) => (props.status === "complete" ? <ConfirmationResult result={props.result} /> : null),
    },
    [],
  );
  useRenderTool(
    {
      name: CONTINUE_IN_APP_TOOL_NAME,
      parameters: anySchema,
      ...scope,
      // The step-up call never gets a result (the run finishes as a success and the application takes over), so CopilotKit keeps
      // it "in progress": the card is drawn in every state, from the arguments the call carries.
      render: (props) => <AlixContinueInApp request={parseContinueInApp(props.parameters)} {...(onContinue !== undefined && { onContinue })} />,
    },
    [onContinue],
  );
};

