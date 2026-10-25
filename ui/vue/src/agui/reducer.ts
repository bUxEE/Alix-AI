import { applyPatch, readPatch } from "./json-patch";
import {
  ALIXAI_ACTIVITY,
  ALIXAI_CUSTOM_EVENT,
  CONFIRM_CALL_PREFIX,
  CONTINUE_TOOL,
  UI_CALL_PREFIX,
  blockTypeOfActivity,
  isRecord,
  parseAttachments,
  parseBlock,
  parseCitations,
  parseConfirmation,
  parseInterrupt,
  parsePendingState,
  parseProgress,
  parseReceipt,
  parseSuggestions,
} from "./parse";
import type { AguiEvent, ChatItem, ChatState, RunOutcome } from "./types";

// The reducer folds AG-UI events into what the panel renders. It mutates the state it is given (the controller keeps it
// reactive), reads every member defensively and ignores what it does not know: an event type, an activity type, a block of the
// wrong shape. Nothing it stores is markup. A run's events arrive in order and exactly once (the client guarantees both).

export const createChatState = (): ChatState => ({
  items: [],
  threadId: undefined,
  runId: undefined,
  running: false,
  step: undefined,
  progress: {},
  shared: undefined,
  pending: undefined,
  suggestions: [],
  interrupts: [],
  outcome: undefined,
  error: undefined,
  uiCalls: [],
  internal: { toolNames: {}, toolArgs: {}, activities: {} },
});

const str = (value: unknown): string | undefined => (typeof value === "string" ? value : undefined);

const itemIndex = (state: ChatState, id: string): number => state.items.findIndex((item) => item.id === id);

const upsert = (state: ChatState, item: ChatItem): void => {
  const at = itemIndex(state, item.id);
  if (at === -1) state.items.push(item);
  else state.items[at] = item;
};

const closeStreaming = (state: ChatState): void => {
  for (const item of state.items) if (item.kind === "message") item.streaming = false;
};

/** Shows one activity (snapshot, or a snapshot after a patch) as its timeline entry or transient state. */
const showActivity = (state: ChatState, id: string, activityType: string, content: unknown): void => {
  if (activityType === ALIXAI_ACTIVITY.progress) {
    const progress = parseProgress(content);
    if (progress !== undefined) state.progress[progress.stage] = progress;
    return;
  }
  if (activityType === ALIXAI_ACTIVITY.citations) {
    const citations = parseCitations(content);
    if (citations !== undefined) upsert(state, { kind: "citations", id, citations });
    return;
  }
  if (activityType === ALIXAI_ACTIVITY.attachments) {
    const files = parseAttachments(content);
    if (files !== undefined) upsert(state, { kind: "attachments", id, files });
    return;
  }
  if (activityType === ALIXAI_ACTIVITY.receipt) {
    const receipt = parseReceipt(content);
    if (receipt !== undefined) upsert(state, { kind: "receipt", id, receipt });
    return;
  }
  const blockType = blockTypeOfActivity(activityType);
  if (blockType !== undefined) {
    const block = parseBlock(blockType, content);
    if (block !== undefined) {
      const at = itemIndex(state, id);
      const answered = at !== -1 && state.items[at]?.kind === "block" ? (state.items[at] as { answered: boolean }).answered : false;
      upsert(state, { kind: "block", id, blockType, block, answered });
    }
  }
};

const parseJson = (text: string | undefined): unknown => {
  if (text === undefined) return undefined;
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
};

const outcomeOf = (value: unknown): RunOutcome => {
  const type = isRecord(value) ? str(value["type"]) : undefined;
  return type === "interrupt" ? "interrupt" : type === "cancelled" ? "cancelled" : "success";
};

/** Applies one event. Unknown event types change nothing. */
export const applyEvent = (state: ChatState, event: AguiEvent): void => {
  switch (event.type) {
    case "RUN_STARTED": {
      state.threadId = str(event["threadId"]) ?? state.threadId;
      state.runId = str(event["runId"]);
      state.running = true;
      state.step = undefined;
      state.progress = {};
      state.suggestions = [];
      state.interrupts = [];
      state.outcome = undefined;
      state.error = undefined;
      state.uiCalls = [];
      state.internal = { toolNames: {}, toolArgs: {}, activities: {} };
      return;
    }
    case "RUN_FINISHED": {
      closeStreaming(state);
      state.running = false;
      state.step = undefined;
      const outcome = event["outcome"];
      state.outcome = outcomeOf(outcome);
      if (state.outcome === "interrupt" && isRecord(outcome) && Array.isArray(outcome["interrupts"])) {
        for (const raw of outcome["interrupts"]) {
          const interrupt = parseInterrupt(raw);
          if (interrupt === undefined) continue;
          state.interrupts.push(interrupt);
          const confirmation = parseConfirmation(interrupt);
          if (confirmation !== undefined) {
            upsert(state, { kind: "confirmation", id: `confirm_${confirmation.preparationId}`, confirmation, interruptId: interrupt.id, status: "pending" });
          }
        }
      }
      return;
    }
    case "RUN_ERROR": {
      closeStreaming(state);
      state.running = false;
      state.step = undefined;
      state.outcome = "error";
      const message = str(event["message"]) ?? "";
      const code = str(event["code"]);
      state.error = { message, ...(code !== undefined && { code }) };
      state.items.push({ kind: "error", id: `error_${state.items.length}`, message, ...(code !== undefined && { code }) });
      return;
    }
    case "STEP_STARTED": {
      state.step = str(event["stepName"]);
      return;
    }
    case "STEP_FINISHED": {
      if (state.step === str(event["stepName"])) state.step = undefined;
      return;
    }
    case "TEXT_MESSAGE_START": {
      const id = str(event["messageId"]);
      if (id === undefined) return;
      const role = event["role"] === "user" ? "user" : "assistant";
      upsert(state, { kind: "message", id, role, text: "", streaming: true });
      return;
    }
    case "TEXT_MESSAGE_CONTENT": {
      const id = str(event["messageId"]);
      const delta = str(event["delta"]);
      if (id === undefined || delta === undefined) return;
      const item = state.items[itemIndex(state, id)];
      if (item?.kind === "message") item.text += delta;
      else state.items.push({ kind: "message", id, role: "assistant", text: delta, streaming: true });
      return;
    }
    case "TEXT_MESSAGE_END": {
      const item = state.items[itemIndex(state, str(event["messageId"]) ?? "")];
      if (item?.kind === "message") item.streaming = false;
      return;
    }
    case "MESSAGES_SNAPSHOT": {
      if (!Array.isArray(event["messages"])) return;
      const items: ChatItem[] = [];
      for (const message of event["messages"]) {
        if (!isRecord(message)) continue;
        const id = str(message["id"]);
        const role = message["role"];
        const content = message["content"];
        if (id === undefined || (role !== "user" && role !== "assistant") || typeof content !== "string") continue;
        items.push({ kind: "message", id, role, text: content, streaming: false });
      }
      state.items = items;
      return;
    }
    case "ACTIVITY_SNAPSHOT": {
      const id = str(event["messageId"]);
      const type = str(event["activityType"]);
      if (id === undefined || type === undefined) return;
      state.internal.activities[id] = { type, content: event["content"] };
      showActivity(state, id, type, event["content"]);
      return;
    }
    case "ACTIVITY_DELTA": {
      const id = str(event["messageId"]);
      const type = str(event["activityType"]);
      const ops = readPatch(event["patch"]);
      if (id === undefined || type === undefined || ops === undefined) return;
      const known = state.internal.activities[id];
      if (known === undefined || known.type !== type) return;
      const patched = applyPatch(known.content, ops);
      if (patched === undefined) return;
      known.content = patched.value;
      showActivity(state, id, type, patched.value);
      return;
    }
    case "STATE_SNAPSHOT": {
      state.shared = event["snapshot"];
      state.pending = parsePendingState(state.shared);
      return;
    }
    case "STATE_DELTA": {
      const ops = readPatch(event["delta"]);
      if (ops === undefined) return;
      const patched = applyPatch(state.shared ?? {}, ops);
      if (patched === undefined) return;
      state.shared = patched.value;
      state.pending = parsePendingState(state.shared);
      return;
    }
    case "CUSTOM": {
      if (event["name"] === ALIXAI_CUSTOM_EVENT.suggestions) state.suggestions = parseSuggestions(event["value"]);
      return;
    }
    case "TOOL_CALL_START": {
      const id = str(event["toolCallId"]);
      const name = str(event["toolCallName"]);
      if (id === undefined || name === undefined) return;
      state.internal.toolNames[id] = name;
      state.internal.toolArgs[id] = "";
      return;
    }
    case "TOOL_CALL_ARGS": {
      const id = str(event["toolCallId"]);
      const delta = str(event["delta"]);
      if (id === undefined || delta === undefined || state.internal.toolArgs[id] === undefined) return;
      state.internal.toolArgs[id] += delta;
      return;
    }
    case "TOOL_CALL_END": {
      const id = str(event["toolCallId"]);
      if (id === undefined) return;
      const name = state.internal.toolNames[id];
      const args = parseJson(state.internal.toolArgs[id]);
      if (name === CONTINUE_TOOL) {
        const summary = isRecord(args) ? (str(args["summary"]) ?? "") : "";
        const preparationId = isRecord(args) ? str(args["preparationId"]) : undefined;
        state.items.push({ kind: "continue", id, summary, ...(preparationId !== undefined && { preparationId }) });
      } else if (id.startsWith(UI_CALL_PREFIX) && name !== undefined) {
        state.uiCalls.push({ id, name, args });
      }
      return;
    }
    case "TOOL_CALL_RESULT": {
      const id = str(event["toolCallId"]);
      if (id === undefined || !id.startsWith(CONFIRM_CALL_PREFIX)) return;
      const receipt = parseReceipt(parseJson(str(event["content"])));
      if (receipt !== undefined) upsert(state, { kind: "receipt", id: `receipt_${receipt.receipt_id}`, receipt });
      return;
    }
    default:
      return;
  }
};
