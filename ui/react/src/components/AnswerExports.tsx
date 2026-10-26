import { useCopilotKit } from "@copilotkit/react-core/v2";
import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactElement, RefObject } from "react";
import { createPortal } from "react-dom";
import { useAlixExports } from "../exports-context.js";
import { useAlixText } from "../i18n/context.js";
import { AlixExportMenu } from "./ExportMenu.js";

type AgentMessage = { id?: string; role?: string };

const MOUNT_CLASS = "alix-answer-export-mount";

/**
 * Answer-level download menus (HTML / Markdown). Mount points are kit-owned nodes appended into CopilotKit bubbles;
 * React never creates or removes them — only this effect does, and it removes every mount on cleanup.
 */
export const ChatAnswerExports = ({ panelRef }: { panelRef: RefObject<HTMLElement | null> }): ReactElement | null => {
  const exports = useAlixExports();
  const text = useAlixText().export;
  const { copilotkit } = useCopilotKit();
  const [hosts, setHosts] = useState<Record<string, HTMLElement>>({});
  const mountsRef = useRef(new Map<string, HTMLElement>());

  const turns = useMemo(() => {
    if (!exports.enabled || exports.exporter === undefined) return [] as { turnId: string; blockIds: string[] }[];
    const agents = (copilotkit as { agents?: Record<string, { messages?: readonly AgentMessage[] }> }).agents;
    const messages = agents === undefined ? [] : Object.values(agents).flatMap((agent) => agent.messages ?? []);
    const result: { turnId: string; blockIds: string[] }[] = [];
    let turnId: string | undefined;
    let blockIds: string[] = [];
    const flush = (): void => {
      if (turnId === undefined) return;
      result.push({ turnId, blockIds });
      turnId = undefined;
      blockIds = [];
    };
    for (const message of messages) {
      if (message.role === "user") {
        flush();
        continue;
      }
      if (message.role === "assistant" && typeof message.id === "string" && turnId === undefined) turnId = message.id;
      if (typeof message.id === "string" && message.role !== "user" && message.role !== "assistant" && turnId !== undefined) {
        blockIds.push(message.id);
      }
    }
    flush();
    return result;
  }, [copilotkit, exports.enabled, exports.exporter]);

  useEffect(() => {
    const mounts = mountsRef.current;
    const clearMounts = (): void => {
      for (const mount of mounts.values()) mount.remove();
      mounts.clear();
      setHosts({});
    };

    if (!exports.enabled || exports.exporter === undefined) {
      clearMounts();
      return undefined;
    }
    const panel = panelRef.current;
    if (panel === null) return undefined;

    let observer: MutationObserver | undefined;

    const sync = (): void => {
      observer?.disconnect();
      const keep = new Set<string>();
      const bubbles = Array.from(
        panel.querySelectorAll<HTMLElement>('[data-role="assistant"], [data-message-role="assistant"], .copilotKitAssistantMessage'),
      );
      const next: Record<string, HTMLElement> = {};
      turns.forEach((turn, index) => {
        keep.add(turn.turnId);
        const byId = panel.querySelector<HTMLElement>(`[data-message-id="${CSS.escape(turn.turnId)}"]`);
        const bubble = byId ?? bubbles[Math.min(index, Math.max(0, bubbles.length - 1))];
        if (bubble === undefined) return;
        let mount = mounts.get(turn.turnId);
        if (mount === undefined || !mount.isConnected || mount.parentElement !== bubble) {
          mount?.remove();
          mount = document.createElement("div");
          mount.className = MOUNT_CLASS;
          mount.dataset.alixOwned = "true";
          bubble.appendChild(mount);
          mounts.set(turn.turnId, mount);
        }
        next[turn.turnId] = mount;
      });
      for (const [id, mount] of [...mounts.entries()]) {
        if (keep.has(id)) continue;
        mount.remove();
        mounts.delete(id);
      }
      setHosts((previous) => {
        const prevKeys = Object.keys(previous);
        const nextKeys = Object.keys(next);
        if (prevKeys.length === nextKeys.length && nextKeys.every((key) => previous[key] === next[key])) return previous;
        return next;
      });
      observer?.observe(panel, { childList: true, subtree: true });
    };

    observer = new MutationObserver(sync);
    sync();
    return () => {
      observer?.disconnect();
      clearMounts();
    };
  }, [exports.enabled, exports.exporter, panelRef, turns]);

  if (!exports.enabled || exports.exporter === undefined || exports.sessionId === "") return null;

  return (
    <>
      {turns.map((turn) => {
        const host = hosts[turn.turnId];
        if (host === undefined || !host.isConnected) return null;
        return createPortal(
          <AlixExportMenu
            key={turn.turnId}
            exporter={exports.exporter!}
            {...(exports.download !== undefined && { download: exports.download })}
            label={text.answer}
            formats={["html", "md"]}
            params={{
              sessionId: exports.sessionId,
              turnId: turn.turnId,
              blockIds: turn.blockIds,
            }}
          />,
          host,
        );
      })}
    </>
  );
};
