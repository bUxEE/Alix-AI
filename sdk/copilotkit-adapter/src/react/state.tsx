import { UseAgentUpdate, useAgent } from "@copilotkit/react-core/v2";
import type { ReactCustomMessageRenderer } from "@copilotkit/react-core/v2";
import type { ComponentType, ReactElement } from "react";
import { parsePendingState } from "../activities.js";
import type { AlixAIPending } from "../activities.js";

// Agent state rendering (the `useCoAgentStateRender` row of docs/integration/agui.md section 12): where the conversation
// stands, drawn from the shared state the core keeps under `alixai` (`STATE_SNAPSHOT` and `STATE_DELTA`). v2 has no
// `useCoAgentStateRender`; the equivalents are `useAgent` (a page reads the state) and the provider's `renderCustomMessages`
// (the chat draws it under the last message), and this file offers both. The state is the allow-list projection of the session:
// names of fields and the question the core asked, never a value, a secret or a permission. Everything is rendered as text.

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

export const DEFAULT_PENDING_LABELS: AlixAIPendingLabels = {
  confirmation: "Waiting for your confirmation",
  clarification: "Waiting for",
  goal: "Goal",
  collected: "Collected",
  missing: "Still needed",
};

export interface AlixAIPendingCardProps {
  /** From `parsePendingState` or `useAlixAIPendingState`. Nothing is drawn without it. */
  pending: AlixAIPending | undefined;
  labels?: Partial<AlixAIPendingLabels>;
}

const statusOf = (pending: AlixAIPending, text: AlixAIPendingLabels): string => {
  if (pending.kind === "confirmation") return text.confirmation;
  if (pending.kind === "clarification") return pending.question === undefined ? text.goal : `${text.clarification} ${pending.question.field}`;
  return pending.phase === "" ? text.goal : `${text.goal}: ${pending.phase}`;
};

/** A one-glance status of the active goal: what it waits for, what it has and what it still needs. */
export const AlixAIPendingCard = ({ pending, labels }: AlixAIPendingCardProps): ReactElement | null => {
  if (pending === undefined) return null;
  const text = { ...DEFAULT_PENDING_LABELS, ...labels };
  return (
    <section className="alixai-pending" data-alixai="pending-state" data-kind={pending.kind} data-phase={pending.phase} aria-live="polite">
      <p data-alixai="pending-status">{statusOf(pending, text)}</p>
      <p data-alixai="pending-capability">{pending.capabilityId}</p>
      {pending.collectedFields.length > 0 && (
        <p data-alixai="pending-collected">
          {text.collected}: {pending.collectedFields.map((field) => field.name).join(", ")}
        </p>
      )}
      {pending.missingFields.length > 0 && (
        <p data-alixai="pending-missing">
          {text.missing}: {pending.missingFields.join(", ")}
        </p>
      )}
    </section>
  );
};

/**
 * The pending goal of the AlixAI session, re-read whenever the shared state changes; undefined while there is none or when the
 * state is not what AlixAI sends. Inside `<CopilotKitProvider>`; `agentId` defaults to CopilotKit's default agent.
 */
export const useAlixAIPendingState = (options: { agentId?: string } = {}): AlixAIPending | undefined => {
  const { agent } = useAgent({ ...(options.agentId !== undefined && { agentId: options.agentId }), updates: [UseAgentUpdate.OnStateChanged] });
  return parsePendingState(agent.state);
};

/** The card, wired to the agent's state: `<AlixAIPendingState />` anywhere inside the provider, in or outside the chat. */
export const AlixAIPendingState = ({ agentId, labels }: { agentId?: string; labels?: Partial<AlixAIPendingLabels> }): ReactElement | null => {
  const pending = useAlixAIPendingState(agentId === undefined ? {} : { agentId });
  return <AlixAIPendingCard pending={pending} {...(labels !== undefined && { labels })} />;
};

type InChatProps = NonNullable<ReactCustomMessageRenderer["render"]> extends ComponentType<infer Props> ? Props : never;

const InChat = ({ message, position, agentId }: InChatProps): ReactElement | null => {
  const { agent } = useAgent({ agentId, updates: [UseAgentUpdate.OnStateChanged, UseAgentUpdate.OnMessagesChanged] });
  // Under the last message of the conversation, once: the state is where the conversation stands now, not where an earlier run left it.
  if (position !== "after" || agent.messages[agent.messages.length - 1]?.id !== message.id) return null;
  return <AlixAIPendingCard pending={parsePendingState(agent.state)} />;
};

/**
 * The chat's state renderer, for the provider's `renderCustomMessages` (or `alixaiProviderProps(props, { showState: true })`):
 * it draws the pending goal under the last message, the v2 counterpart of `useCoAgentStateRender`.
 */
export const createAlixAIStateRenderer = (options: { agentId?: string } = {}): ReactCustomMessageRenderer => ({
  ...(options.agentId !== undefined && { agentId: options.agentId }),
  render: InChat,
});
