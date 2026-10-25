import { useCopilotKit } from "@copilotkit/react-core/v2";
import type { HttpAgent } from "@ag-ui/client";
import { useCallback, useEffect, useState } from "react";
import type { ReactElement } from "react";
import { AlixChatPanel } from "@alixai/react/copilotkit";
import type { AlixKitOptions } from "@alixai/react/copilotkit";
import { DEMO_PATHS, demoPrompts } from "@alixai/demo-mock";
import type { Citation } from "@alixai/sdk";
import { agentFor, providerPropsFor, useAgentStatus } from "../lib/agent";
import { renderPreview } from "../lib/preview";
import { useShell } from "../shell/context";
import { Icon } from "../shell/Icon";
import { Segmented } from "../shell/Segmented";

type Variant = "inline" | "sidebar" | "popup";
type Send = (text: string) => void;

// The reconnect scenario needs a client that resumes a cut stream (the Vue kit's); CopilotKit's HttpAgent does not, so the React
// rail leaves it out. The mock marks it `kits: ["vue"]`.
const PROMPTS = demoPrompts.filter((prompt) => prompt.kits === undefined || prompt.kits.includes("react"));

/**
 * Lives inside the chat's CopilotKit provider. It hands the page a way to send a message (the rail), and loads the conversation's
 * history with a connect run (no message: the mock answers with a snapshot of the thread). CopilotKit empties the agent's messages
 * when a chat mounts on a thread (switching layout remounts it), and the HttpAgent has no connect of its own, so the history is
 * loaded again, once per mount, whenever the agent is left empty.
 */
const ChatBridge = ({ agent, onReady }: { agent: HttpAgent; onReady: (send: Send | undefined) => void }): null => {
  const { copilotkit } = useCopilotKit();
  useEffect(() => {
    const target = agent as unknown as Parameters<typeof copilotkit.runAgent>[0]["agent"];
    let loaded = false;
    const load = (): void => {
      if (loaded || agent.isRunning || agent.messages.length > 0) return;
      loaded = true;
      void copilotkit.runAgent({ agent: target }).catch(() => undefined);
    };
    onReady((text) => {
      agent.addMessage({ id: crypto.randomUUID(), role: "user", content: text });
      void copilotkit.runAgent({ agent: target }).catch(() => undefined);
    });
    load();
    const subscription = agent.subscribe({
      onMessagesChanged: ({ messages }) => {
        if (messages.length === 0) setTimeout(load, 0);
      },
    });
    return () => {
      subscription.unsubscribe();
      onReady(undefined);
    };
  }, [agent, copilotkit, onReady]);
  return null;
};

export const ChatSection = (): ReactElement => {
  const { lang, theme, text, threadId, notify } = useShell();
  const [variant, setVariant] = useState<Variant>("inline");
  const [progress, setProgress] = useState(true);
  const [send, setSend] = useState<Send | undefined>(undefined);
  const agent = agentFor(threadId);
  const status = useAgentStatus(agent);
  const onReady = useCallback((next: Send | undefined) => setSend(() => next), []);
  const onAction = useCallback<NonNullable<AlixKitOptions["onAction"]>>((action) => notify("onAction", action.label ?? action.action), [notify]);
  const onOpenCitation = useCallback((citation: Citation) => notify("onOpenCitation", `${citation.display_name} ${citation.version}`), [notify]);
  const onContinueInApp = useCallback<NonNullable<AlixKitOptions["onContinueInApp"]>>(
    (request) => notify("onContinueInApp", request.summary ?? request.preparationId ?? ""),
    [notify],
  );
  const blocked = send === undefined || status.running || status.confirmationOpen;

  return (
    <div className="demo-chat">
      <div className="demo-toolbar">
        <Segmented
          label={text.chat.variant}
          value={variant}
          options={[
            { value: "inline", label: text.chat.variants.inline },
            { value: "sidebar", label: text.chat.variants.sidebar },
            { value: "popup", label: text.chat.variants.popup },
          ]}
          onChange={setVariant}
        />
        <label className="demo-check">
          <input type="checkbox" className="no-validation" checked={progress} onChange={(event) => setProgress(event.target.checked)} />
          {text.chat.progress}
        </label>
        <p className="demo-toolbar__meta">
          {text.chat.thread} <code>{threadId}</code>
        </p>
      </div>
      <div className="demo-chat__grid">
        <div className="demo-frame demo-chat__frame" data-variant={variant}>
          {variant !== "inline" && (
            <div className="demo-empty">
              <Icon name="chat" size={28} />
              <p>{text.chat.elsewhere[variant]}</p>
            </div>
          )}
          <AlixChatPanel
            key={`${threadId}:${variant}:${progress ? "p" : ""}`}
            runtimeUrl={DEMO_PATHS.copilotRuntime}
            providerProps={providerPropsFor(agent)}
            threadId={threadId}
            locale={lang}
            theme={theme}
            variant={variant}
            defaultOpen
            showProgress={progress}
            renderPreview={renderPreview}
            onAction={onAction}
            onOpenCitation={onOpenCitation}
            onContinueInApp={onContinueInApp}
          >
            <ChatBridge agent={agent} onReady={onReady} />
          </AlixChatPanel>
        </div>
        <aside className="demo-rail" aria-labelledby="demo-rail-title">
          <h2 id="demo-rail-title" className="demo-rail__title">
            {text.chat.tryThese}
          </h2>
          {status.confirmationOpen ? (
            <p className="demo-note" data-tone="warn" role="status">
              {text.chat.confirmationOpen}
            </p>
          ) : status.running ? (
            <p className="demo-note" role="status">
              <span className="demo-pulse" aria-hidden="true" />
              {text.chat.running}
            </p>
          ) : null}
          <ul className="demo-rail__list">
            {PROMPTS.map((prompt) => (
              <li key={prompt.id}>
                <button type="button" className="demo-prompt" disabled={blocked} onClick={() => send?.(prompt.prompt[lang])}>
                  <span className="demo-prompt__title">{prompt.title[lang]}</span>
                  <span className="demo-prompt__shows">{prompt.shows[lang]}</span>
                </button>
              </li>
            ))}
          </ul>
          <p className="demo-rail__foot">{text.chat.reconnectNote}</p>
        </aside>
      </div>
    </div>
  );
};
