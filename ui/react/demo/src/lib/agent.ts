import { HttpAgent } from "@ag-ui/client";
import { useEffect, useState } from "react";
import { DEMO_PATHS } from "@alixai/demo-mock";

// One AG-UI agent per conversation, created once and kept: CopilotKit wants stable provider props, and a conversation's messages
// live on its agent. The page talks to the mock's `/v1/agui` directly (the self-managed agent of CopilotKit); a real host puts its
// CopilotRuntime with AlixAIAgent in between, which holds the confirmation secret.

const agents = new Map<string, HttpAgent>();
const props = new WeakMap<HttpAgent, { selfManagedAgents: { default: HttpAgent } }>();

export const agentFor = (threadId: string): HttpAgent => {
  let agent = agents.get(threadId);
  if (agent === undefined) {
    agent = new HttpAgent({ url: DEMO_PATHS.agui, threadId });
    agents.set(threadId, agent);
  }
  return agent;
};

/** The `providerProps` of AlixChatPanel for an agent: the same object every time, so the panel never rebuilds its renderers. */
export const providerPropsFor = (agent: HttpAgent): { selfManagedAgents: { default: HttpAgent } } => {
  let value = props.get(agent);
  if (value === undefined) {
    value = { selfManagedAgents: { default: agent } };
    props.set(agent, value);
  }
  return value;
};

/** Forgets a conversation's agent (after the conversation was deleted). */
export const dropAgent = (threadId: string): void => {
  agents.delete(threadId);
};

export interface AgentStatus {
  readonly running: boolean;
  /** A run ended in an interrupt that is not answered yet: AG-UI refuses a new run until it is. */
  readonly confirmationOpen: boolean;
}

const read = (agent: HttpAgent): AgentStatus => ({ running: agent.isRunning, confirmationOpen: (agent.pendingInterrupts?.length ?? 0) > 0 });

/** Follows whether the agent is running and whether a confirmation is open. */
export const useAgentStatus = (agent: HttpAgent): AgentStatus => {
  const [status, setStatus] = useState<AgentStatus>(() => read(agent));
  useEffect(() => {
    // The agent settles its flags after the callbacks: read them on the next task.
    const later = (): void => {
      setTimeout(() => setStatus(read(agent)), 0);
    };
    const subscription = agent.subscribe({
      onRunInitialized: () => {
        setStatus((current) => ({ ...current, running: true }));
      },
      onRunFinishedEvent: later,
      onRunErrorEvent: later,
      onRunFailed: later,
      onRunFinalized: later,
    });
    setStatus(read(agent));
    return () => subscription.unsubscribe();
  }, [agent]);
  return status;
};
