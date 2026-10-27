import { useAgent } from "@copilotkit/react-core/v2";
import { useEffect } from "react";

const EVENT_NAME = "alixai.text_replaced";

type AgentMessages = {
  messages?: readonly { id?: string; role?: string }[];
  setMessages?: (messages: unknown[]) => void;
};

/**
 * Listens for ADR 0046 `alixai.text_replaced` CUSTOM events. When the agent exposes
 * `setMessages`, the last assistant message is dropped so the template tokens that
 * follow supersede it (never left half). No motion is added (reduced-motion safe).
 */
export const useAlixTextReplaced = (enabled = true): void => {
  const { agent } = useAgent();

  useEffect(() => {
    if (!enabled) return undefined;
    const subscription = agent.subscribe({
      onCustomEvent: ({ event }) => {
        if (event.name !== EVENT_NAME) return;
        const a = agent as unknown as AgentMessages;
        const messages = a.messages;
        if (!Array.isArray(messages) || typeof a.setMessages !== "function") return;
        for (let i = messages.length - 1; i >= 0; i--) {
          if (messages[i]?.role !== "assistant") continue;
          const id = messages[i]?.id;
          a.setMessages(messages.filter((m) => m?.id !== id) as unknown[]);
          break;
        }
      },
    });
    return () => subscription.unsubscribe();
  }, [agent, enabled]);
};
