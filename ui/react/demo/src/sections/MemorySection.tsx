import { useState } from "react";
import type { ReactElement } from "react";
import { AlixMemoryList, AlixMemoryProposal, type MemoryListItem, type MemoryProposalStatus } from "@alixai/react";
import { DEMO_PATHS, requestJson, useResource } from "../lib/api";
import { fill } from "../lib/text";
import { useShell } from "../shell/context";

export const MemorySection = (): ReactElement => {
  const { text, notify } = useShell();
  const list = useResource((signal) =>
    requestJson<{ items: Array<{ id: string; text: string; status: MemoryListItem["status"] }> }>(DEMO_PATHS.memory, { signal }).then((body) =>
      body.items.map((item) => ({ id: item.id, text: item.text, status: item.status })),
    ),
  );
  const [status, setStatus] = useState<MemoryProposalStatus>("pending");
  const proposal = {
    id: "mem_01JDEMO000000000000000000A",
    text: "Prefer dark mode",
    expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
  };

  const onConfirm = (): void => {
    setStatus("remembered");
    notify("memory", fill(text.memory.confirmed, { id: proposal.id }));
  };
  const onReject = (): void => {
    setStatus("forgotten");
    notify("memory", fill(text.memory.rejected, { id: proposal.id }));
  };

  const onDelete = async (id: string): Promise<void> => {
    await requestJson<void>(`${DEMO_PATHS.memory}/${encodeURIComponent(id)}`, { method: "DELETE" });
    notify("memory", fill(text.memory.deleted, { id }));
    list.reload();
  };

  const onSave = async (item: MemoryListItem, next: string): Promise<void> => {
    await requestJson<void>(`${DEMO_PATHS.memory}/${encodeURIComponent(item.id)}`, {
      method: "PATCH",
      body: JSON.stringify({ text: next }),
    });
    notify("memory", fill(text.memory.saved, { id: item.id }));
    list.reload();
  };

  return (
    <div className="demo-split">
      <div className="demo-frame demo-frame--pad demo-stack">
        <h2 className="demo-rail__title">{text.memory.proposal}</h2>
        <AlixMemoryProposal
          proposal={proposal}
          status={status}
          onConfirm={onConfirm}
          onReject={onReject}
        />
      </div>
      <div className="demo-frame demo-frame--pad">
        <AlixMemoryList
          items={list.data}
          loading={list.loading}
          failed={list.error !== undefined}
          onDelete={(id) => void onDelete(id)}
          onSave={(item, next) => void onSave(item, next)}
          onRetry={() => list.reload()}
        />
      </div>
    </div>
  );
};
