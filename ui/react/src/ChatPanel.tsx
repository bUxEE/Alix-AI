import { CopilotChat, CopilotKitProvider, CopilotPopup, CopilotSidebar } from "@copilotkit/react-core/v2";
import type { voice } from "@alixai/sdk";
import { isValidElement, useEffect, useMemo, useRef, useState } from "react";
import { createPortal } from "react-dom";
import type { ComponentProps, CSSProperties, ReactElement, ReactNode, RefObject } from "react";
import { useAlixAISuggestions } from "@alixai/copilotkit-adapter/react";
import { ChatAnswerExports } from "./components/AnswerExports.js";
import { AlixMemoryProposal } from "./components/Memory.js";
import { AlixDictationButton, insertIntoCopilotTextarea, type VoiceModeOption } from "./dictation/index.js";
import type { AlixDownload, AlixExporter } from "./export.js";
import { AlixExportsProvider } from "./exports-context.js";
import { useAlixMemoryProposal } from "./hooks/useAlixMemoryProposal.js";
import { useFullscreen } from "./hooks/useFullscreen.js";
import { AlixProvider, useAlixText } from "./i18n/context.js";
import type { AlixProviderProps } from "./i18n/context.js";
import { alixProviderProps, useAlixConfirmation } from "./register.js";
import type { AlixKitOptions } from "./register.js";
import { AlixVoiceButton } from "./voice/AlixVoiceButton.js";

// The panel is CopilotKit's chat. What the kit adds around it: the provider props with no cloud and no telemetry, the AlixAI
// renderers, the confirmation sheet for interrupts, the follow-up suggestions of the core, the theme tokens, and the
// language. The chat, the sidebar and the popup themselves are CopilotKit's (ADR 0035: no competing chat UI).

type ProviderProps = ComponentProps<typeof CopilotKitProvider>;

export interface AlixChatPanelProps extends Pick<AlixProviderProps, "locale" | "messages" | "timeZone">, Omit<AlixKitOptions, "agentId"> {
  /** The CopilotRuntime endpoint of the host's backend (where `AlixAIAgent` runs). The browser never talks to AlixAI directly. */
  runtimeUrl: string;
  agentId?: string;
  threadId?: string;
  /** `inline` fills its container; `sidebar` and `popup` are CopilotKit's. */
  variant?: "inline" | "sidebar" | "popup";
  /** Force a theme. `auto` follows the visitor's colour scheme. */
  theme?: "auto" | "light" | "dark";
  /** The sidebar or popup starts open. */
  defaultOpen?: boolean;
  /** More props for CopilotKit's provider (headers, credentials, properties). Cloud keys are always removed. */
  providerProps?: Omit<ProviderProps, "runtimeUrl" | "children">;
  /**
   * Voice surface on the composer. Default `off` (chat only).
   * `dictation` shows record-to-text; `conversation` shows the talk button for voice-chat mode.
   */
  voice?: VoiceModeOption;
  /** Required when `voice` is `dictation`: the SDK dictation client for this deployment. */
  dictationClient?: voice.DictationClient;
  /** BCP 47 hint for dictation transcription. */
  language?: string;
  onVoiceStart?: () => void;
  onVoiceStop?: () => void;
  onVoiceCancel?: () => void;
  /** Replaces the default dictation control when `voice` is `dictation`. */
  dictationButton?: ReactNode;
  /** Replaces the default talk control when `voice` is `conversation`. */
  voiceButton?: ReactNode;
  /**
   * When true, render `AlixMemoryProposal` for `alixai.memory_proposal` CUSTOM events.
   * Default false (1.1 compatibility). The kit never calls AlixAI.
   */
  memory?: boolean;
  onMemoryConfirm?: (id: string) => void | Promise<void>;
  onMemoryReject?: (id: string) => void | Promise<void>;
  /**
   * Controlled fullscreen. Omit for uncontrolled (internal state). Pair with `onFullscreenChange`.
   */
  fullscreen?: boolean;
  /** Uncontrolled initial fullscreen. Default false. */
  defaultFullscreen?: boolean;
  onFullscreenChange?: (value: boolean) => void;
  /**
   * Overrides `--alixai-z-fullscreen` / `--alix-fullscreen-z` (default 2400).
   * Body-teleported dialogs/menus from inside the panel must use a higher z-index.
   */
  fullscreenZIndex?: number;
  /** Extra controls in the kit-owned fullscreen header row (beside the fullscreen toggle). */
  headerActions?: ReactNode;
  /**
   * When true, show download menus on table/chart blocks and assistant answers.
   * Default false (1.1 compatibility). Requires `exporter`.
   */
  exports?: boolean;
  exporter?: AlixExporter;
  download?: AlixDownload;
  onDownload?: AlixDownload;
  children?: ReactNode;
}

// The visible text of a link, whatever elements its label is made of.
const textOf = (node: ReactNode): string => {
  if (typeof node === "string" || typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map(textOf).join("");
  if (isValidElement<{ children?: ReactNode }>(node)) return textOf(node.props.children);
  return "";
};

const normalisedUrl = (value: string): string => {
  try {
    return new URL(value).href;
  } catch {
    return value;
  }
};

// An autolink is a link whose text is its own address: "https://x", "help@x" (mailto:) or "www.x" (http://). Streamdown
// rewrites the href to its normalised form, so both sides are compared normalised.
const isAutolink = (href: string | undefined, text: string): boolean => {
  if (href === undefined || text === "") return false;
  const target = normalisedUrl(href);
  return [text, `mailto:${text}`, `http://${text}`].some((candidate) => normalisedUrl(candidate) === target);
};

type SourceSpan = { position?: { start?: { offset?: number }; end?: { offset?: number } } };

// A GFM literal autolink (a bare address) spans exactly its own text in the source; what the core writes as a link spans more:
// `<url>` adds its brackets and `[label](<dest>)` its syntax. Without a source position, any autolink counts as bare.
const isBareAddress = (node: unknown, href: string | undefined, text: string): boolean => {
  if (!isAutolink(href, text)) return false;
  const pos = (node as SourceSpan | undefined)?.position;
  const start = pos?.start?.offset;
  const end = pos?.end?.offset;
  return typeof start !== "number" || typeof end !== "number" || end - start === text.length;
};

// The address a link may carry: the Vue kit's rule (ui/vue/src/markdown.ts), http, https or mailto with no control character or
// space. Everything else (javascript:, data:, a relative path, Streamdown's placeholder of a link still streaming) is its text.
const SAFE_HREF = /^(https?:\/\/|mailto:)/i;
const isSafeHref = (href: string | undefined): href is string => href !== undefined && SAFE_HREF.test(href.trim()) && !/[\u0000-\u001f\s]/.test(href.trim());

// CopilotKit draws assistant text with Streamdown, whose GFM autolinks turn a bare address in the model's text ("sign in at
// https://evil.example/login") into a link, even when the core allowed no link at all: the core reads Markdown as CommonMark,
// where a bare address is text. A bare address is drawn as the text it is; a link the core allowed (`[label](<dest>)` or
// `<url>`) stays a link, drawn as Streamdown draws one, and only to an address isSafeHref allows. The kit does not lean on the
// core's sanitiser for this: Streamdown's own defaults allow every scheme.
const MarkdownLink = ({ children, className, href, node, ...props }: ComponentProps<"a"> & { node?: unknown }): ReactElement => {
  if (!isSafeHref(href) || isBareAddress(node, href, textOf(children))) return <>{children}</>;
  return (
    <a
      className={["wrap-anywhere font-medium text-primary underline", className].filter(Boolean).join(" ")}
      data-streamdown="link"
      href={/^mailto:/i.test(href.trim()) ? href.trim() : normalisedUrl(href.trim())}
      rel="noreferrer"
      target="_blank"
      {...props}
    >
      {children}
    </a>
  );
};

// No image is drawn, as in the Vue kit: an image in the assistant's text would load an address the model chose. Its alternative
// text stays, as text.
const MarkdownImage = ({ alt }: ComponentProps<"img"> & { node?: unknown }): ReactElement => <>{alt ?? ""}</>;

// One object for every render: CopilotKit memoises its slots, and a new one would draw every message again. Streamdown's default
// plugins parse raw HTML (rehype-raw) and harden links with every scheme allowed; the kit drops them, so raw HTML in the text is
// never an element (`allowDangerousHtml: false` drops it before any plugin runs) and the only links and images are the two
// components above.
const markdownPlugins: never[] = [];
const messageView = {
  assistantMessage: {
    markdownRenderer: {
      components: { a: MarkdownLink, img: MarkdownImage },
      rehypePlugins: markdownPlugins,
      remarkRehypeOptions: { allowDangerousHtml: false },
    },
  },
};

const ChatComposerVoice = ({
  panelRef,
  voice,
  threadId,
  dictationClient,
  language,
  onVoiceStart,
  onVoiceStop,
  onVoiceCancel,
  dictationButton,
  voiceButton,
}: {
  panelRef: RefObject<HTMLElement | null>;
  voice: VoiceModeOption;
  threadId?: string;
  dictationClient?: voice.DictationClient;
  language?: string;
  onVoiceStart?: () => void;
  onVoiceStop?: () => void;
  onVoiceCancel?: () => void;
  dictationButton?: ReactNode;
  voiceButton?: ReactNode;
}): ReactElement | null => {
  const [host, setHost] = useState<HTMLElement | null>(null);

  useEffect(() => {
    if (voice === "off") {
      setHost(null);
      return undefined;
    }
    const panel = panelRef.current;
    if (panel === null) return undefined;
    let mount: HTMLSpanElement | undefined;
    const attach = (): void => {
      const send = panel.querySelector('[data-testid="copilot-send-button"]');
      if (!(send instanceof HTMLElement) || !(send.parentElement instanceof HTMLElement)) {
        setHost(null);
        return;
      }
      if (mount === undefined || !mount.isConnected) {
        mount = document.createElement("span");
        mount.className = "alix-composer-voice-mount";
        send.parentElement.insertBefore(mount, send);
      }
      setHost(mount);
    };
    attach();
    const observer = new MutationObserver(attach);
    observer.observe(panel, { childList: true, subtree: true });
    return () => {
      observer.disconnect();
      mount?.remove();
      setHost(null);
    };
  }, [panelRef, voice, threadId, dictationClient, language]);

  if (voice === "off" || host === null) return null;

  const sessionId = threadId ?? "";
  const dictation =
    voice === "dictation" && dictationClient !== undefined && sessionId !== ""
      ? (dictationButton ?? (
          <AlixDictationButton
            client={dictationClient}
            language={language}
            sessionId={sessionId}
            onTranscript={(text) => insertIntoCopilotTextarea(panelRef.current, text)}
          />
        ))
      : null;

  const talk =
    voice === "conversation"
      ? (voiceButton ?? (
          <AlixVoiceButton
            onStart={onVoiceStart}
            onStop={onVoiceStop}
            onCancel={onVoiceCancel}
          />
        ))
      : null;

  const node = dictation ?? talk;
  if (node === null) return null;
  return createPortal(node, host);
};

const Chat = ({
  agentId,
  threadId,
  variant,
  defaultOpen,
  onContinueInApp,
  memory = false,
  onMemoryConfirm,
  onMemoryReject,
}: Pick<
  AlixChatPanelProps,
  "agentId" | "threadId" | "variant" | "defaultOpen" | "onContinueInApp" | "memory" | "onMemoryConfirm" | "onMemoryReject"
>): ReactElement => {
  const text = useAlixText().chat;
  useAlixConfirmation(onContinueInApp === undefined ? {} : { onContinueInApp });
  useAlixAISuggestions();
  const memoryState = useAlixMemoryProposal(memory);
  const labels = {
    chatInputPlaceholder: text.placeholder,
    modalHeaderTitle: text.title,
    welcomeMessageText: text.welcome,
    chatDisclaimerText: text.disclaimer,
    chatToggleOpenLabel: text.open,
    chatToggleCloseLabel: text.close,
    chatInputToolbarAddButtonLabel: text.attach,
    assistantMessageToolbarCopyMessageLabel: text.copy,
  };
  const common = { agentId: agentId ?? "default", labels, messageView, ...(threadId !== undefined && { threadId }) };
  const shell =
    variant === "sidebar" ? (
      <CopilotSidebar {...common} {...(defaultOpen !== undefined && { defaultOpen })} />
    ) : variant === "popup" ? (
      <CopilotPopup {...common} {...(defaultOpen !== undefined && { defaultOpen })} />
    ) : (
      <CopilotChat {...common} />
    );

  const answer = async (kind: "confirm" | "reject"): Promise<void> => {
    if (memoryState.proposal === null) return;
    const handler = kind === "confirm" ? onMemoryConfirm : onMemoryReject;
    if (handler === undefined) return;
    memoryState.setStatus("submitting");
    try {
      await handler(memoryState.proposal.id);
      memoryState.setStatus(kind === "confirm" ? "remembered" : "forgotten");
    } catch {
      memoryState.setStatus("failed");
    }
  };

  return (
    <>
      {shell}
      {memory && memoryState.proposal !== null ? (
        <div className="alix-pa-3">
          <AlixMemoryProposal
            proposal={memoryState.proposal}
            status={memoryState.status}
            onConfirm={() => void answer("confirm")}
            onReject={() => void answer("reject")}
          />
        </div>
      ) : null}
    </>
  );
};

const MaximizeIcon = ({ exit }: { exit: boolean }): ReactElement => (
  <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
    {exit ? (
      <>
        <path d="M8 3v5H3" />
        <path d="M16 3v5h5" />
        <path d="M8 21v-5H3" />
        <path d="M16 21v-5h5" />
      </>
    ) : (
      <>
        <path d="M8 3H3v5" />
        <path d="M16 3h5v5" />
        <path d="M8 21H3v-5" />
        <path d="M16 21h5v-5" />
      </>
    )}
  </svg>
);

export const AlixChatPanel = ({
  runtimeUrl,
  agentId = "default",
  threadId,
  variant = "inline",
  theme = "auto",
  defaultOpen,
  locale,
  messages,
  timeZone,
  providerProps,
  children,
  onOpenCitation,
  onAction,
  renderPreview,
  showProgress,
  showState,
  onContinueInApp,
  components,
  voice = "off",
  dictationClient,
  language,
  onVoiceStart,
  onVoiceStop,
  onVoiceCancel,
  dictationButton,
  voiceButton,
  memory = false,
  onMemoryConfirm,
  onMemoryReject,
  fullscreen,
  defaultFullscreen = false,
  onFullscreenChange,
  fullscreenZIndex,
  headerActions,
  exports: exportsEnabled = false,
  exporter,
  download,
  onDownload,
}: AlixChatPanelProps): ReactElement => {
  const panelRef = useRef<HTMLDivElement>(null);
  const toggleRef = useRef<HTMLButtonElement>(null);
  const downloadHandler = download ?? onDownload;
  const { isFullscreen, toggleFullscreen } = useFullscreen({
    ...(fullscreen !== undefined && { fullscreen }),
    defaultFullscreen,
    ...(onFullscreenChange !== undefined && { onFullscreenChange }),
    rootRef: panelRef,
    toggleRef,
  });
  // CopilotKit wants its renderer arrays stable: a new array on every render is an error and remounts every drawn activity.
  const provider = useMemo(
    () =>
      alixProviderProps(
        { ...providerProps, runtimeUrl },
        {
          agentId,
          ...(onOpenCitation !== undefined && { onOpenCitation }),
          ...(onAction !== undefined && { onAction }),
          ...(renderPreview !== undefined && { renderPreview }),
          ...(showProgress !== undefined && { showProgress }),
          ...(showState !== undefined && { showState }),
          ...(components !== undefined && { components }),
          ...(exportsEnabled && { exports: true }),
          ...(exporter !== undefined && { exporter }),
          ...(downloadHandler !== undefined && { download: downloadHandler }),
        },
      ) as unknown as Omit<ProviderProps, "children">,
    [providerProps, runtimeUrl, agentId, onOpenCitation, onAction, renderPreview, showProgress, showState, components, exportsEnabled, exporter, downloadHandler],
  );
  const exportsValue = useMemo(
    () => ({
      enabled: exportsEnabled === true && exporter !== undefined,
      sessionId: threadId ?? "",
      ...(exporter !== undefined && { exporter }),
      ...(downloadHandler !== undefined && { download: downloadHandler }),
    }),
    [exportsEnabled, exporter, downloadHandler, threadId],
  );

  // Fullscreen is pure styling (`data-fullscreen` / fixed inset). The panel node stays where React rendered it —
  // never appendChild/insertBefore — so unmount and sibling inserts cannot throw NotFoundError.
  return (
    <AlixProvider {...(locale !== undefined && { locale })} {...(messages !== undefined && { messages })} {...(timeZone !== undefined && { timeZone })}>
      <AlixExportsProvider value={exportsValue}>
        <Panel
          ref={panelRef}
          theme={theme}
          fullscreen={isFullscreen}
          {...(fullscreenZIndex !== undefined && { fullscreenZIndex })}
          headerActions={headerActions}
          toggleRef={toggleRef}
          onToggleFullscreen={toggleFullscreen}
          {...(locale !== undefined && { locale })}
        >
          <CopilotKitProvider {...provider}>
            <Chat
              agentId={agentId}
              variant={variant}
              memory={memory}
              {...(threadId !== undefined && { threadId })}
              {...(defaultOpen !== undefined && { defaultOpen })}
              {...(onContinueInApp !== undefined && { onContinueInApp })}
              {...(onMemoryConfirm !== undefined && { onMemoryConfirm })}
              {...(onMemoryReject !== undefined && { onMemoryReject })}
            />
            <ChatComposerVoice
              panelRef={panelRef}
              voice={voice}
              {...(threadId !== undefined && { threadId })}
              {...(dictationClient !== undefined && { dictationClient })}
              {...(language !== undefined && { language })}
              {...(onVoiceStart !== undefined && { onVoiceStart })}
              {...(onVoiceStop !== undefined && { onVoiceStop })}
              {...(onVoiceCancel !== undefined && { onVoiceCancel })}
              {...(dictationButton !== undefined && { dictationButton })}
              {...(voiceButton !== undefined && { voiceButton })}
            />
            <ChatAnswerExports panelRef={panelRef} />
            {children}
          </CopilotKitProvider>
        </Panel>
      </AlixExportsProvider>
    </AlixProvider>
  );
};

// Whether the visitor's colour scheme is dark. False on a server and in the first client render, so both renders agree.
const useSystemDark = (): boolean => {
  const [dark, setDark] = useState(false);
  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return undefined;
    const media = window.matchMedia("(prefers-color-scheme: dark)");
    const update = (): void => setDark(media.matches);
    update();
    media.addEventListener?.("change", update);
    return () => media.removeEventListener?.("change", update);
  }, []);
  return dark;
};

// CopilotKit's own stylesheet switches its dark variants (the input, the menus) on a `.dark` ancestor, not on the colour scheme:
// the panel carries that class whenever its tokens are dark, so CopilotKit's parts and the AlixAI parts change together.
const Panel = ({
  ref,
  theme,
  locale,
  children,
  fullscreen,
  fullscreenZIndex,
  headerActions,
  toggleRef,
  onToggleFullscreen,
}: {
  ref?: RefObject<HTMLDivElement | null>;
  theme: "auto" | "light" | "dark";
  locale?: string;
  children: ReactNode;
  fullscreen: boolean;
  fullscreenZIndex?: number;
  headerActions?: ReactNode;
  toggleRef: RefObject<HTMLButtonElement | null>;
  onToggleFullscreen: () => void;
}): ReactElement => {
  const text = useAlixText().chat;
  const systemDark = useSystemDark();
  const dark = theme === "dark" || (theme === "auto" && systemDark);
  const className = [dark ? "alix-root alix-panel dark" : "alix-root alix-panel", fullscreen ? "alix-panel--fullscreen" : ""]
    .filter(Boolean)
    .join(" ");
  const style: CSSProperties | undefined =
    fullscreenZIndex === undefined
      ? undefined
      : ({ ["--alixai-z-fullscreen"]: String(fullscreenZIndex), ["--alix-fullscreen-z"]: String(fullscreenZIndex) } as CSSProperties);
  return (
    <div
      ref={ref}
      className={className}
      role={fullscreen ? "dialog" : "region"}
      aria-label={text.label}
      {...(style !== undefined && { style })}
      {...(fullscreen ? { "aria-modal": true, tabIndex: -1, "data-fullscreen": "" } : {})}
      {...(theme !== "auto" && { "data-alixai-theme": theme })}
      {...(locale !== undefined && { lang: locale })}
    >
      <div className="alix-panel__chrome">
        {headerActions}
        <button
          ref={toggleRef}
          type="button"
          className="alix-panel__fullscreen"
          aria-label={fullscreen ? text.fullscreenExit : text.fullscreen}
          aria-pressed={fullscreen}
          onClick={onToggleFullscreen}
        >
          <MaximizeIcon exit={fullscreen} />
        </button>
      </div>
      {children}
    </div>
  );
};
