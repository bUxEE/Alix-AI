import { useState } from "react";
import type { ReactElement } from "react";
import { AlixConversationList, AlixLoadable, AlixSkeleton, formatDateTime, useAlixIntl, useAlixLoadState } from "@alixai/react";
import { DEMO_PATHS, requestJson, useResource } from "../lib/api";
import { dropAgent } from "../lib/agent";
import { useShell } from "../shell/context";
import { Icon } from "../shell/Icon";

interface Conversation {
  readonly id: string;
  readonly title?: string;
  readonly preview?: string;
  readonly updatedAt?: string;
}

const ListSkeleton = (): ReactElement => (
  <div className="demo-stack" aria-hidden="true">
    {[0, 1, 2, 3].map((row) => (
      <div key={row} className="demo-stack demo-stack--tight">
        <AlixSkeleton width={row % 2 === 0 ? "70%" : "55%"} />
        <AlixSkeleton width="35%" height="0.625rem" />
      </div>
    ))}
  </div>
);

export const ConversationsSection = (): ReactElement => {
  const { text, threadId, openThread, notify } = useShell();
  const intl = useAlixIntl();
  const list = useResource((signal) => requestJson<{ conversations: Conversation[] }>(DEMO_PATHS.conversations, { signal }).then((body) => body.conversations));
  const state = useAlixLoadState(list.loading, list.error !== undefined, list.data !== undefined);
  const [selected, setSelected] = useState(threadId);
  const [busy, setBusy] = useState(false);
  const chosen = list.data?.find((item) => item.id === selected);

  const create = async (): Promise<void> => {
    setBusy(true);
    try {
      const created = await requestJson<Conversation>(DEMO_PATHS.conversations, { method: "POST" });
      setSelected(created.id);
      notify("onNew", text.conversations.created);
      list.reload();
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: string): Promise<void> => {
    setBusy(true);
    try {
      await requestJson<void>(`${DEMO_PATHS.conversations}/${encodeURIComponent(id)}`, { method: "DELETE" });
      dropAgent(id);
      list.reload();
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="demo-split">
      <div className="demo-frame demo-frame--pad">
        <AlixLoadable state={state} skeleton={<ListSkeleton />}>
          {list.error !== undefined && !state.hasLoadedOnce ? (
            <p className="demo-note" data-tone="danger" role="alert">
              {text.conversations.failed}
            </p>
          ) : (
            <AlixConversationList
              items={(list.data ?? []).map((item) => ({ id: item.id, ...(item.title !== undefined && { title: item.title }), ...(item.updatedAt !== undefined && { updatedAt: item.updatedAt }) }))}
              activeId={selected}
              onSelect={setSelected}
              onNew={() => void create()}
            />
          )}
        </AlixLoadable>
      </div>
      <div className="demo-frame demo-frame--pad demo-detail">
        {chosen === undefined ? (
          <div className="demo-empty">
            <Icon name="conversations" size={28} />
            <p>{text.conversations.openLead}</p>
          </div>
        ) : (
          <>
            <p className="demo-eyebrow">
              <code>{chosen.id}</code>
            </p>
            <h2 className="demo-detail__title">{chosen.title ?? intl.messages.conversations.untitled}</h2>
            {chosen.updatedAt !== undefined && <p className="demo-muted">{formatDateTime(intl, chosen.updatedAt)}</p>}
            {chosen.preview !== undefined && <p className="demo-detail__preview">{chosen.preview}</p>}
            <div className="demo-row">
              <button type="button" className="demo-button" data-variant="primary" onClick={() => openThread(chosen.id)}>
                {chosen.id === threadId ? text.conversations.current : text.conversations.open}
                <Icon name="arrow" size={16} />
              </button>
              <button type="button" className="demo-button" data-variant="quiet" disabled={busy} onClick={() => void remove(chosen.id)}>
                {text.conversations.delete}
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  );
};
