import { useAgent } from "@copilotkit/react-core/v2";
import { useEffect, useState } from "react";
import type { MemoryProposal, MemoryProposalStatus } from "../components/Memory.js";

const EVENT_NAME = "alixai.memory_proposal";

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

const parseProposal = (value: unknown): MemoryProposal | undefined => {
  if (!isRecord(value)) return undefined;
  const memory = isRecord(value["memory"]) ? value["memory"] : value;
  if (typeof memory["id"] !== "string" || typeof memory["text"] !== "string") return undefined;
  return {
    id: memory["id"],
    text: memory["text"],
    ...(typeof memory["expires_at"] === "string" && { expiresAt: memory["expires_at"] }),
    ...(typeof memory["expiresAt"] === "string" && { expiresAt: memory["expiresAt"] }),
  };
};

export interface AlixMemoryProposalState {
  proposal: MemoryProposal | null;
  status: MemoryProposalStatus;
  setStatus: (status: MemoryProposalStatus) => void;
  clear: () => void;
}

/** Listens for `alixai.memory_proposal` CUSTOM events when the chat panel enables memory. */
export const useAlixMemoryProposal = (enabled: boolean): AlixMemoryProposalState => {
  const { agent } = useAgent();
  const [proposal, setProposal] = useState<MemoryProposal | null>(null);
  const [status, setStatus] = useState<MemoryProposalStatus>("pending");

  useEffect(() => {
    if (!enabled) {
      setProposal(null);
      return undefined;
    }
    const subscription = agent.subscribe({
      onRunStartedEvent: () => {
        /* keep the last proposal until a new one arrives */
      },
      onCustomEvent: ({ event }) => {
        if (event.name !== EVENT_NAME) return;
        const next = parseProposal(event.value);
        if (next === undefined) return;
        const expired = next.expiresAt !== undefined && Date.parse(next.expiresAt) <= Date.now();
        setProposal(next);
        setStatus(expired ? "expired" : "pending");
      },
    });
    return () => subscription.unsubscribe();
  }, [agent, enabled]);

  return {
    proposal,
    status,
    setStatus,
    clear: () => setProposal(null),
  };
};
