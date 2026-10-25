import { computed, getCurrentScope, onScopeDispose, reactive, ref, toValue, type ComputedRef, type MaybeRefOrGetter, type Ref } from "vue";
import { AguiHttpError, type AguiClient } from "../agui/client";
import { applyEvent, createChatState } from "../agui/reducer";
import type { AguiContentPart, AguiMessage, AlixUiCall, ChatItem, ChatState, ConfirmationStatus, RunAgentInput } from "../agui/types";
import { useLoadState, type LoadState } from "./useLoadState";

// The chat controller: it owns one thread's timeline and drives the runs that change it. It is the Vue counterpart of what
// CopilotKit's `useCopilotChat` does for the React kit, over the same AG-UI protocol.
//
// Authority stays where it is. The controller proposes nothing and decides nothing: a message becomes a run, an approval becomes
// a `resume` entry with exactly `{approved: boolean}`, a choice becomes `forwardedProps.alixai.choiceId`. The confirmation secret
// is the host relay's (see agui/client.ts), and a UI action runs only in the host's own `onUiAction`.

export type AttachmentKind = "image" | "audio" | "video" | "document";

export interface OutgoingAttachment {
  /** The id returned by the files API (`POST /v1/files`). Inline bytes and URLs are never sent. */
  readonly fileId: string;
  readonly kind: AttachmentKind;
  readonly name?: string;
}

export interface SendOptions {
  readonly attachments?: readonly OutgoingAttachment[];
}

/** The runs a thread allows: at most four files per message, the engine's own limit. */
export const MAX_ATTACHMENTS = 4;

export interface AlixChatOptions {
  client: AguiClient;
  /** The AlixAI session id: the host mints the context token with the session set to this thread. */
  threadId: MaybeRefOrGetter<string>;
  /** A BCP 47 tag the turn is answered in; the gateway falls back to the application's language. */
  language?: MaybeRefOrGetter<string | undefined>;
  /** Readable context of the page. Untrusted data to the core: fenced, bounded, never an instruction. */
  context?: MaybeRefOrGetter<readonly { description: string; value: string }[] | undefined>;
  /** Runs a UI action the model proposed (a tool the host registered). Its return value goes back as the tool result. */
  onUiAction?: (call: AlixUiCall) => unknown | Promise<unknown>;
  /** Ids for runs and messages. Defaults to `crypto.randomUUID`; replaceable for tests. */
  createId?: (prefix: string) => string;
}

export interface AlixChat {
  state: ChatState;
  items: ComputedRef<ChatItem[]>;
  /** True from the moment a run is requested until it ends. */
  running: Ref<boolean>;
  /** True while the client is reconnecting a cut stream. */
  reconnecting: Ref<boolean>;
  /** The loading of the history (`connect`): skeleton the first time, overlay after. */
  load: LoadState;
  send: (text: string, options?: SendOptions) => Promise<boolean>;
  /** Answers the pending clarification with one of its choices. */
  choose: (itemId: string, choiceId: string) => Promise<boolean>;
  approve: (itemId: string) => Promise<boolean>;
  decline: (itemId: string) => Promise<boolean>;
  /** The user closed the confirmation without answering: answered as a decline, nothing is approved. */
  dismiss: (itemId: string) => Promise<boolean>;
  stop: () => Promise<void>;
  /** Asks the same question again as a new run (after an error). */
  retry: () => Promise<boolean>;
  /** Loads the thread's history and state (a connect run). */
  connect: () => Promise<boolean>;
  reset: () => void;
}

const defaultId = (prefix: string): string => {
  const uuid = globalThis.crypto?.randomUUID?.() ?? `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 10)}`;
  return `${prefix}_${uuid}`;
};

const partsOf = (text: string, attachments: readonly OutgoingAttachment[]): string | AguiContentPart[] => {
  if (attachments.length === 0) return text;
  return [{ type: "text", text }, ...attachments.map((file) => ({ type: file.kind, source: { type: "file" as const, value: file.fileId } }))];
};

export const useAlixChat = (options: AlixChatOptions): AlixChat => {
  const createId = options.createId ?? defaultId;
  const state = reactive(createChatState()) as ChatState;
  const running = ref(false);
  const reconnecting = ref(false);
  const load = useLoadState();
  let controller: AbortController | undefined;
  let lastSend: { text: string; options: SendOptions | undefined } | undefined;
  // Bumped by `reset` and when the owner's scope is disposed. A run started in an earlier epoch is stale: it was aborted, and
  // the state it would touch (the timeline, `running`, the controller, the load) now belongs to the next thread's run.
  let epoch = 0;

  const items = computed(() => state.items);

  const baseInput = (runId: string): Omit<RunAgentInput, "messages"> => {
    const language = toValue(options.language);
    const context = toValue(options.context);
    return {
      threadId: toValue(options.threadId),
      runId,
      ...(context !== undefined && context.length > 0 && { context }),
      ...(language !== undefined && { forwardedProps: { alixai: { language } } }),
    };
  };

  const pushError = (message: string, code?: string): void => {
    state.running = false;
    state.outcome = "error";
    state.error = { message, ...(code !== undefined && { code }) };
    state.items.push({ kind: "error", id: `error_${state.items.length}_${createId("e")}`, message, ...(code !== undefined && { code }) });
  };

  const setConfirmation = (itemId: string, status: ConfirmationStatus): void => {
    const item = state.items.find((entry) => entry.id === itemId);
    if (item?.kind === "confirmation") item.status = status;
  };

  /** Runs one request to its end. Returns true when the run finished without a transport or HTTP failure (false when it went stale). */
  const execute = async (input: RunAgentInput): Promise<boolean> => {
    if (running.value) return false;
    running.value = true;
    const started = epoch;
    const live = (): boolean => started === epoch;
    const own = new AbortController();
    controller = own;
    let ok = true;
    try {
      const result = await options.client.run(input, {
        signal: own.signal,
        onEvent: (event) => {
          if (live()) applyEvent(state, event);
        },
        onReconnecting: () => {
          if (live()) reconnecting.value = true;
        },
      });
      if (!result.terminal && !result.aborted && live()) {
        ok = false;
        pushError("The stream ended before the run did.");
      }
    } catch (error) {
      ok = false;
      if (live()) pushError(error instanceof Error ? error.message : "The request failed.", error instanceof AguiHttpError ? error.code : undefined);
    } finally {
      if (live()) {
        reconnecting.value = false;
        running.value = false;
        state.running = false;
        for (const item of state.items) if (item.kind === "message") item.streaming = false;
        controller = undefined;
      }
    }
    if (!live()) return false;
    if (ok && options.onUiAction !== undefined && state.uiCalls.length > 0) await answerUiCalls();
    return ok;
  };

  // A UI action is an instruction to the page. The host runs it (or refuses it); the result goes back as data, in a run that has no turn.
  const answerUiCalls = async (): Promise<void> => {
    const calls = state.uiCalls.splice(0);
    const started = epoch;
    for (const call of calls) {
      let content: string;
      try {
        content = JSON.stringify((await options.onUiAction?.(call)) ?? null);
      } catch (error) {
        content = JSON.stringify({ error: error instanceof Error ? error.message : "failed" });
      }
      if (started !== epoch) return;
      const message: AguiMessage = { id: createId("msg"), role: "tool", toolCallId: call.id, content };
      await execute({ ...baseInput(createId("run")), messages: [message] });
    }
  };

  const send = async (text: string, sendOptions?: SendOptions): Promise<boolean> => {
    const body = text.trim();
    const attachments = sendOptions?.attachments ?? [];
    if (body === "" || running.value || attachments.length > MAX_ATTACHMENTS) return false;
    lastSend = { text: body, options: sendOptions };
    const runId = createId("run");
    const messageId = createId("msg");
    state.items.push({ kind: "message", id: messageId, role: "user", text: body, streaming: false });
    return execute({ ...baseInput(runId), messages: [{ id: messageId, role: "user", content: partsOf(body, attachments) }] });
  };

  const choose = async (itemId: string, choiceId: string): Promise<boolean> => {
    if (running.value) return false;
    const item = state.items.find((entry) => entry.id === itemId);
    if (item?.kind === "block") item.answered = true;
    const language = toValue(options.language);
    const input = baseInput(createId("run"));
    return execute({ ...input, messages: [], forwardedProps: { alixai: { choiceId, ...(language !== undefined && { language }) } } });
  };

  // A confirmation resolves once, with exactly {approved: boolean}. A dismissal is a decline: the interrupt is answered, never left
  // open, and nothing is approved. (The sheet shows it as dismissed.)
  const resume = async (itemId: string, answer: "approve" | "decline" | "dismiss"): Promise<boolean> => {
    if (running.value) return false;
    const item = state.items.find((entry) => entry.id === itemId);
    if (item?.kind !== "confirmation" || (item.status !== "pending" && item.status !== "failed")) return false;
    // A confirmation of another mode (step up) is never answered here: the application's own interface completes it.
    if (item.confirmation.mode !== "explicit") return false;
    item.status = "submitting";
    const entry = { interruptId: item.interruptId, status: "resolved" as const, payload: { approved: answer === "approve" } };
    const started = epoch;
    const ok = await execute({ ...baseInput(createId("run")), messages: [], resume: [entry] });
    if (started !== epoch) return false;
    const failed = !ok || state.outcome === "error";
    setConfirmation(itemId, failed ? "failed" : answer === "approve" ? "approved" : answer === "decline" ? "declined" : "dismissed");
    return !failed;
  };

  const approve = (itemId: string): Promise<boolean> => resume(itemId, "approve");
  const decline = (itemId: string): Promise<boolean> => resume(itemId, "decline");
  const dismiss = (itemId: string): Promise<boolean> => resume(itemId, "dismiss");

  const stop = async (): Promise<void> => {
    const runId = state.runId;
    const threadId = toValue(options.threadId);
    controller?.abort();
    if (runId !== undefined) await options.client.cancel(threadId, runId);
  };

  const retry = async (): Promise<boolean> => {
    if (lastSend === undefined || running.value) return false;
    // The failed attempt's error and its optimistic message are replaced by the new attempt.
    const last = state.items[state.items.length - 1];
    if (last?.kind === "error") state.items.pop();
    const previous = state.items[state.items.length - 1];
    if (previous?.kind === "message" && previous.role === "user" && previous.text === lastSend.text) state.items.pop();
    return send(lastSend.text, lastSend.options);
  };

  const connect = async (): Promise<boolean> => {
    if (running.value) return false;
    load.begin();
    const started = epoch;
    const ok = await execute({ ...baseInput(createId("run")), messages: [] });
    if (started === epoch) load.end(ok && state.outcome !== "error");
    return ok;
  };

  const reset = (): void => {
    epoch += 1;
    controller?.abort();
    controller = undefined;
    Object.assign(state, createChatState());
    running.value = false;
    reconnecting.value = false;
    load.reset();
    lastSend = undefined;
  };

  // The panel (or whatever owns the controller) went away: its run is aborted, and nothing it was about to do follows.
  if (getCurrentScope() !== undefined) {
    onScopeDispose(() => {
      epoch += 1;
      controller?.abort();
    });
  }

  return { state, items, running, reconnecting, load, send, choose, approve, decline, dismiss, stop, retry, connect, reset };
};
