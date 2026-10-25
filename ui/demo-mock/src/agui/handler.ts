import type { IncomingMessage, ServerResponse } from "node:http";
import type { Choice, FileRef } from "@alixai/sdk";
import { type FileStore } from "../files/store";
import { apiError, BodyTooLargeError, isRecord, readJson, sendError, sendJson, wait } from "../http";
import { newId } from "../ids";
import { detectLocale, matchScenario, type DemoLocale } from "../prompts";
import {
  SCENARIOS,
  approved,
  attachmentsActivity,
  chosen,
  declined,
  expired,
  filesReceived,
  help,
  type ScenarioContext,
  type ScenarioResult,
} from "./scenarios";
import type { HistoryMessage, PreparedAction, RecordedRun, Thread, ThreadStore } from "./threads";
import { createTurn, type AguiEvent, type Frame } from "./turn";

// POST /v1/agui and its companions (schemas/api/paths/agui.yaml): a run streamed as server-sent events with the resume cursor in
// `id:`, the events of a run read again from a cursor, cancel, and the capabilities document. The input is read the way the gateway
// reads it (docs/integration/agui.md section 3): exactly one of a resume, a choice or a user message is answered; `messages: []`
// with neither is a connect run that replays the thread's history; a tool message answers a confirmation or a UI tool call.
//
// The mock keeps no confirmation secret: a demo page talks to it directly, without the host relay that would hold one, so an
// approval is accepted without the X-AlixAI-Confirmation-Token header the real gateway requires.

const MAX_BODY = 1024 * 1024;
const THREAD_ID = /^[A-Za-z0-9_.-]{1,128}$/;
const CONFIRM_PREFIX = "alixai_confirm_";
const UI_PREFIX = "alixai_ui_";
const MAX_FILES = 4;

export interface AguiHandlerOptions {
  readonly pace: number;
  readonly threads: ThreadStore;
  readonly files: FileStore;
}

interface RunInput {
  readonly threadId: string;
  readonly runId: string;
  readonly messages: readonly unknown[];
  readonly state: unknown;
  readonly resume: readonly unknown[];
  readonly language: string | undefined;
  readonly choiceId: string | undefined;
}

interface HttpRefusal {
  readonly status: number;
  readonly error: ReturnType<typeof apiError>;
}

const refuse = (status: number, code: Parameters<typeof apiError>[0], message: string, details?: Record<string, unknown>): HttpRefusal => ({ status, error: apiError(code, message, details) });

const str = (value: unknown): string | undefined => (typeof value === "string" ? value : undefined);

const parseInput = (body: unknown): RunInput | HttpRefusal => {
  if (!isRecord(body)) return refuse(400, "validation_failed", "the body is not a RunAgentInput object");
  const threadId = str(body["threadId"]);
  const runId = str(body["runId"]);
  if (threadId === undefined || !THREAD_ID.test(threadId)) return refuse(400, "validation_failed", "threadId is missing or malformed", { field: "threadId" });
  if (runId === undefined || !THREAD_ID.test(runId)) return refuse(400, "validation_failed", "runId is missing or malformed", { field: "runId" });
  const messages = body["messages"] ?? [];
  if (!Array.isArray(messages) || messages.length > 200) return refuse(400, "validation_failed", "messages must be an array of at most 200", { field: "messages" });
  const resume = body["resume"] ?? [];
  if (!Array.isArray(resume) || resume.length > 1) return refuse(400, "validation_failed", "resume holds at most one entry", { field: "resume" });
  const forwarded = isRecord(body["forwardedProps"]) && isRecord(body["forwardedProps"]["alixai"]) ? body["forwardedProps"]["alixai"] : {};
  return { threadId, runId, messages, state: body["state"], resume, language: str(forwarded["language"]), choiceId: str(forwarded["choiceId"]) };
};

/** The text and the file ids of a user message (`content` is a string or AG-UI content parts). */
const readUserMessage = (message: Record<string, unknown>): { text: string; fileIds: string[] } => {
  const content = message["content"];
  if (typeof content === "string") return { text: content.trim(), fileIds: [] };
  const texts: string[] = [];
  const fileIds: string[] = [];
  if (Array.isArray(content)) {
    for (const part of content) {
      if (!isRecord(part)) continue;
      if (part["type"] === "text" && typeof part["text"] === "string") texts.push(part["text"]);
      else if (isRecord(part["source"]) && part["source"]["type"] === "file" && typeof part["source"]["value"] === "string") fileIds.push(part["source"]["value"]);
    }
  }
  return { text: texts.join("\n").trim(), fileIds };
};

const localeOf = (language: string | undefined): DemoLocale | undefined => {
  if (language === undefined) return undefined;
  const tag = language.toLowerCase();
  return tag.startsWith("it") ? "it" : tag.startsWith("en") ? "en" : undefined;
};

const fold = (text: string): string => text.normalize("NFD").replace(/[̀-ͯ]/g, "").toLowerCase();

/** A typed answer to a pending clarification: a choice whose label the text contains, or "the first" / "1". */
const choiceFromText = (text: string, choices: readonly Choice[]): Choice | undefined => {
  const folded = fold(text);
  const byLabel = choices.find((choice) => folded.includes(fold(choice.label)) || fold(choice.label).split(/[\s,]+/).filter((word) => word.length > 2).every((word) => folded.includes(word)));
  if (byLabel !== undefined) return byLabel;
  const ordinal = [/\b(1|first|prima|primo)\b/, /\b(2|second|seconda|secondo)\b/, /\b(3|third|terza|terzo)\b/].findIndex((pattern) => pattern.test(folded));
  return ordinal === -1 ? undefined : choices[ordinal];
};

interface BuiltRun {
  readonly frames: Frame[];
  readonly cutAfter: number | undefined;
  /** Puts the thread back as it was before the run (a cancelled run leaves nothing behind). */
  readonly undo: () => void;
}

const samePending = (a: unknown, b: unknown): boolean => JSON.stringify(a) === JSON.stringify(b);

const buildRun = (input: RunInput, thread: Thread, options: AguiHandlerOptions): BuiltRun | HttpRefusal => {
  const now = Date.now();
  const turnId = newId("trn", now);
  const turn = createTurn({ pace: options.pace });
  const last = input.messages[input.messages.length - 1];
  const lastRecord = isRecord(last) ? last : undefined;
  const before = thread.pending;
  const historyBefore = thread.history.length;
  const preparedBefore = new Set(thread.prepared.keys());
  const clarificationBefore = thread.clarification;
  const stateFrame = (pending: unknown): AguiEvent =>
    isRecord(input.state) ? { type: "STATE_DELTA", delta: [{ op: "add", path: "/alixai", value: { pending } }] } : { type: "STATE_SNAPSHOT", snapshot: { alixai: { pending } } };

  const undo = (): void => {
    thread.pending = before;
    thread.history.splice(historyBefore);
    for (const id of thread.prepared.keys()) if (!preparedBefore.has(id)) thread.prepared.delete(id);
    thread.clarification = clarificationBefore;
  };

  const done = (cutAfter?: number): BuiltRun => ({ frames: turn.frames(), cutAfter, undo });

  // ---- a run with no turn: connect, or the result of a UI tool call ----
  const resumeEntry = input.resume[0];
  const toolAnswer = lastRecord?.["role"] === "tool" ? str(lastRecord["toolCallId"]) : undefined;
  if (resumeEntry === undefined && input.choiceId === undefined && (input.messages.length === 0 || toolAnswer?.startsWith(UI_PREFIX) === true)) {
    turn.add({ type: "RUN_STARTED", threadId: input.threadId, runId: input.runId });
    if (input.messages.length === 0) turn.add({ type: "MESSAGES_SNAPSHOT", messages: thread.history.map((message) => ({ ...message })) });
    turn.add(stateFrame(before), { type: "RUN_FINISHED", threadId: input.threadId, runId: input.runId, outcome: { type: "success" } });
    turn.commit(input.messages.length === 0 ? 120 : 40);
    return done();
  }

  // ---- what is answered ----
  let answer: { kind: "resume"; interruptId: string; approved: boolean } | { kind: "choice"; choiceId: string } | { kind: "message"; text: string; fileIds: string[]; messageId: string | undefined };
  if (resumeEntry !== undefined || toolAnswer?.startsWith(CONFIRM_PREFIX) === true) {
    if (input.choiceId !== undefined) return refuse(400, "validation_failed", "a run answers a resume or a choice, not both");
    let interruptId: string | undefined;
    let payload: unknown;
    let status: unknown = "resolved";
    if (resumeEntry !== undefined) {
      if (!isRecord(resumeEntry)) return refuse(400, "validation_failed", "a resume entry is an object");
      interruptId = str(resumeEntry["interruptId"]);
      status = resumeEntry["status"];
      payload = resumeEntry["payload"];
    } else {
      interruptId = toolAnswer?.slice(CONFIRM_PREFIX.length);
      try {
        payload = JSON.parse(str(lastRecord?.["content"]) ?? "");
      } catch {
        payload = undefined;
      }
    }
    if (interruptId === undefined) return refuse(400, "validation_failed", "the resume names no interrupt");
    let approvedAnswer: boolean;
    if (status === "cancelled") approvedAnswer = false;
    else if (status === "resolved" && isRecord(payload) && Object.keys(payload).length === 1 && typeof payload["approved"] === "boolean") approvedAnswer = payload["approved"];
    else return refuse(400, "validation_failed", "an answer to a confirmation is exactly {\"approved\": true} or {\"approved\": false}");
    answer = { kind: "resume", interruptId, approved: approvedAnswer };
  } else if (input.choiceId !== undefined) {
    answer = { kind: "choice", choiceId: input.choiceId };
  } else if (lastRecord?.["role"] === "user") {
    const read = readUserMessage(lastRecord);
    answer = { kind: "message", ...read, messageId: str(lastRecord["id"]) };
  } else if (lastRecord?.["role"] === "tool") {
    return refuse(400, "validation_failed", "a tool result is accepted only for a call of this agent", { reason: "unknown_tool_call" });
  } else {
    return refuse(400, "validation_failed", "the last message must be the user's", { reason: "no_user_message" });
  }

  // ---- checks that refuse before anything streams ----
  let attachments: FileRef[] = [];
  if (answer.kind === "message") {
    if (answer.fileIds.length > MAX_FILES) return refuse(400, "validation_failed", `a message carries at most ${MAX_FILES} files`, { reason: "too_many_files" });
    if (answer.text === "") return refuse(400, "validation_failed", answer.fileIds.length > 0 ? "a message with files needs text" : "the message is empty", { reason: "empty_message" });
    const refs = answer.fileIds.map((id) => options.files.get(id));
    if (refs.some((ref) => ref === undefined)) return refuse(403, "scope_violation", "an attached file does not resolve in the caller's scope");
    attachments = refs as FileRef[];
  }
  const prepared = answer.kind === "resume" ? thread.prepared.get(answer.interruptId) : undefined;
  if (answer.kind === "resume") {
    if (prepared === undefined) return refuse(403, "scope_violation", "the request is outside the caller's scope");
    if (prepared.status !== "pending") return refuse(409, "conflict", "this confirmation was already answered", { reason: "already_answered" });
  }

  // ---- the language of the reply ----
  const match = answer.kind === "message" ? matchScenario(answer.text) : { id: undefined, locale: undefined };
  const locale: DemoLocale =
    localeOf(input.language) ??
    (answer.kind === "message" ? (match.locale ?? detectLocale(answer.text)) : answer.kind === "resume" && prepared !== undefined ? prepared.locale : (thread.clarification?.locale ?? thread.locale));
  thread.locale = locale;

  turn.add({ type: "RUN_STARTED", threadId: input.threadId, runId: input.runId });
  const ctx: ScenarioContext = { turn, locale, thread, turnId, now, files: options.files, userText: answer.kind === "message" ? answer.text : "" };
  if (attachments.length > 0) turn.add(attachmentsActivity(ctx, attachments));
  turn.add(stateFrame(before));

  // ---- the end of every turn: the pending goal, the suggestions, the terminal event, the history ----
  const finish = (result: ScenarioResult, undoRun: () => void, cut: boolean): BuiltRun => {
    if (!samePending(result.pending, before)) turn.add({ type: "STATE_DELTA", delta: [{ op: "replace", path: "/alixai/pending", value: result.pending }] });
    thread.pending = result.pending;
    const outcome = result.outcome;
    if (outcome.type === "success") {
      if (outcome.suggestions !== undefined && outcome.suggestions.length > 0) turn.add({ type: "CUSTOM", name: "alixai.suggestions", value: { suggestions: outcome.suggestions } });
      turn.add({ type: "RUN_FINISHED", threadId: input.threadId, runId: input.runId, result: { finishReason: outcome.finishReason, turnId }, outcome: { type: "success" } });
    } else if (outcome.type === "interrupt") {
      turn.add({ type: "RUN_FINISHED", threadId: input.threadId, runId: input.runId, result: { finishReason: "awaiting_confirmation", turnId }, outcome: { type: "interrupt", interrupts: [outcome.interrupt] } });
    } else if (outcome.type === "cancelled") {
      turn.add({ type: "RUN_FINISHED", threadId: input.threadId, runId: input.runId, result: { finishReason: "cancelled", turnId }, outcome: { type: "cancelled" } });
    } else {
      turn.add({ type: "RUN_ERROR", message: outcome.message, code: outcome.code });
    }
    turn.commit(80);
    const said = turn.transcript();
    if (said !== "") thread.history.push({ id: `msg_${turnId}_1`, role: "assistant", content: said });
    thread.updatedAt = new Date(now).toISOString();
    const frames = turn.frames();
    return { frames, cutAfter: cut ? Math.floor(frames.length / 2) : undefined, undo: undoRun };
  };

  // ---- the turn ----
  if (answer.kind === "resume") {
    const action = prepared as PreparedAction;
    let result: ScenarioResult;
    if (!answer.approved) {
      action.status = "declined";
      result = declined(ctx, action);
    } else if (Date.parse(action.expiresAt) <= now) {
      action.status = "declined";
      result = expired(ctx, action);
    } else {
      action.status = "approved";
      result = approved(ctx, action);
    }
    return finish(
      result,
      () => {
        undo();
        action.status = "pending";
      },
      false,
    );
  }
  if (answer.kind === "choice") {
    const choice = thread.clarification?.choices.find((entry) => entry.choice_id === answer.choiceId);
    if (choice !== undefined) return finish(chosen(ctx, choice, "choice"), undo, false);
    const message = locale === "it" ? "Non c'è una domanda in attesa di questa risposta. Ripeti la richiesta." : "There is no question waiting for that answer. Please ask again.";
    turn.text(`msg_${turnId}_1`, message);
    return finish({ outcome: { type: "error", message, code: "validation_failed" }, pending: before }, undo, false);
  }
  const typed = answer;
  const pendingQuestion = thread.clarification;
  const typedChoice = match.id === undefined && pendingQuestion !== undefined ? choiceFromText(typed.text, pendingQuestion.choices) : undefined;
  let result: ScenarioResult;
  if (typedChoice !== undefined) result = chosen(ctx, typedChoice, "user_turn");
  else if (match.id === undefined && attachments.length > 0) result = filesReceived(ctx, attachments);
  else if (match.id === undefined) result = help(ctx, true);
  else if (match.id === "help") result = help(ctx, false);
  else result = SCENARIOS[match.id](ctx);
  const userMessage: HistoryMessage = { id: typed.messageId ?? `msg_${turnId}_u`, role: "user", content: typed.text };
  thread.history.push(userMessage);
  if (thread.title === "" || thread.title === "New conversation") thread.title = typed.text.slice(0, 60);
  return finish(result, undo, match.id === "reconnect");
};

// ---- streaming ----

const TERMINAL = new Set(["RUN_FINISHED", "RUN_ERROR"]);

const frameText = (frame: Frame): string => `${frame.id === undefined ? "" : `id: ${frame.id}\n`}data: ${JSON.stringify(frame.event)}\n\n`;

/** The events that close what was open when a run is cancelled, ending with RUN_FINISHED and the cancelled outcome. */
const cancellationTail = (frames: readonly Frame[], at: number, threadId: string, runId: string): Frame[] => {
  const messages = new Set<string>();
  const tools = new Set<string>();
  const steps: string[] = [];
  let lastId = -1;
  for (const frame of frames.slice(0, at)) {
    const { event } = frame;
    if (frame.id !== undefined) lastId = Math.max(lastId, Number(frame.id));
    const id = str(event["messageId"]) ?? str(event["toolCallId"]) ?? str(event["stepName"]) ?? "";
    if (event.type === "TEXT_MESSAGE_START") messages.add(id);
    else if (event.type === "TEXT_MESSAGE_END") messages.delete(id);
    else if (event.type === "TOOL_CALL_START") tools.add(id);
    else if (event.type === "TOOL_CALL_END") tools.delete(id);
    else if (event.type === "STEP_STARTED") steps.push(id);
    else if (event.type === "STEP_FINISHED") steps.splice(steps.lastIndexOf(id), 1);
  }
  const events: AguiEvent[] = [
    ...[...messages].map((messageId) => ({ type: "TEXT_MESSAGE_END", messageId })),
    ...[...tools].map((toolCallId) => ({ type: "TOOL_CALL_END", toolCallId })),
    ...steps.reverse().map((stepName) => ({ type: "STEP_FINISHED", stepName })),
    { type: "RUN_FINISHED", threadId, runId, outcome: { type: "cancelled" } },
  ];
  return events.map((event, index) => ({ id: index === events.length - 1 ? String(lastId + 1) : undefined, event, delayMs: 0 }));
};

interface LiveRun extends RecordedRun {
  readonly threadId: string;
  readonly undo: () => void;
}

const finalizeCancel = (run: LiveRun, at: number): void => {
  const tail = cancellationTail(run.frames, at, run.threadId, run.runId);
  run.frames.splice(at, run.frames.length - at, ...tail);
  run.cancelRequested = false;
  run.undo();
};

/** Writes the frames of a run from `from`, pacing them; the first delivery of a cut run stops halfway by dropping the connection. */
const stream = async (req: IncomingMessage, res: ServerResponse, run: LiveRun, from: number): Promise<void> => {
  const controller = new AbortController();
  run.writer = controller;
  const onClose = (): void => controller.abort();
  res.on("close", onClose);
  res.writeHead(200, {
    "Content-Type": "text/event-stream; charset=utf-8",
    "Cache-Control": "no-cache, no-transform",
    Connection: "keep-alive",
    "X-Accel-Buffering": "no",
  });
  res.flushHeaders();
  try {
    for (let index = from; index < run.frames.length; index += 1) {
      if (run.cancelRequested) finalizeCancel(run, index);
      const frame = run.frames[index] as Frame;
      await wait(frame.delayMs, controller.signal);
      if (controller.signal.aborted) return;
      if (run.cutAfter !== undefined && index === run.cutAfter) {
        run.cutAfter = undefined;
        req.socket.destroy();
        return;
      }
      res.write(frameText(frame));
      run.written = Math.max(run.written, index + 1);
      if (TERMINAL.has(frame.event.type)) run.finished = true;
    }
    res.end();
  } finally {
    res.off("close", onClose);
    if (run.writer === controller) run.writer = undefined;
  }
};

// ---- the capabilities document ----

const EMITTED = [
  "RUN_STARTED",
  "RUN_FINISHED",
  "RUN_ERROR",
  "STEP_STARTED",
  "STEP_FINISHED",
  "TEXT_MESSAGE_START",
  "TEXT_MESSAGE_CONTENT",
  "TEXT_MESSAGE_END",
  "TOOL_CALL_START",
  "TOOL_CALL_ARGS",
  "TOOL_CALL_END",
  "TOOL_CALL_RESULT",
  "STATE_SNAPSHOT",
  "STATE_DELTA",
  "MESSAGES_SNAPSHOT",
  "ACTIVITY_SNAPSHOT",
  "ACTIVITY_DELTA",
  "CUSTOM",
];

const capabilities = (): Record<string, unknown> => ({
  protocolVersion: "1.0",
  package: "@ag-ui/core@1.0.1",
  eventsEmitted: EMITTED,
  eventsNotEmitted: ["TEXT_MESSAGE_CHUNK", "TOOL_CALL_CHUNK", "REASONING_START", "REASONING_MESSAGE_START", "REASONING_MESSAGE_CONTENT", "REASONING_MESSAGE_END", "REASONING_END", "RAW"],
  notClaimed: [
    "reasoning_events",
    "encrypted_reasoning",
    "subagent_events",
    "raw_events",
    "chunk_events",
    "client_context",
    "client_tool_calls",
    "voice_full_duplex",
    "predictive_state_updates",
    "inline_attachments",
    "model_generated_suggestions",
  ],
  agent: {
    identity: {
      name: "AlixAI",
      type: "application-bound assistant",
      provider: "self-hosted",
      version: "demo-mock",
      description: "Prepares actions, shows their exact effects and commits them only after the user's explicit confirmation.",
    },
    transport: { streaming: true, websocket: false, resumable: true, httpBinary: false, pushNotifications: false },
    tools: {
      supported: true,
      clientProvided: false,
      parallelCalls: false,
      items: [
        { name: "alixai_confirm_action", description: "Shows the exact effects of a prepared action and waits for the user's approval." },
        { name: "alixai_continue_in_app", description: "Hands the user to the application's own authenticated interface." },
      ],
    },
    state: { snapshots: true, deltas: true, persistentState: true, memory: false },
    output: { structuredOutput: true, supportedMimeTypes: ["text/plain", "text/markdown"] },
    multiAgent: { supported: false },
    reasoning: { supported: false, streaming: false, encrypted: false },
    multimodal: { input: { image: true, file: true, audio: false, video: false, pdf: false }, output: { image: false, audio: false } },
    execution: { codeExecution: false },
    humanInTheLoop: { supported: true, approvals: true, interrupts: true, approveWithEdits: false, feedback: false, interventions: false },
  },
  pending: [],
});

// ---- routes ----

export interface AguiHandler {
  handle: (req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams) => Promise<boolean>;
}

export const createAguiHandler = (options: AguiHandlerOptions): AguiHandler => {
  const runs = new Map<string, LiveRun>();
  const key = (threadId: string, runId: string): string => `${threadId}\u0000${runId}`;

  const run = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    let body: unknown;
    try {
      body = await readJson(req, MAX_BODY);
    } catch (error) {
      if (error instanceof BodyTooLargeError) sendError(res, 413, "validation_failed", "the run input is larger than 1 MiB");
      else sendError(res, 400, "validation_failed", "the body could not be read");
      return;
    }
    const input = parseInput(body);
    if ("error" in input) {
      sendJson(res, input.status, input.error);
      return;
    }
    // The same run id on the same thread is the recorded run, never a second turn.
    const recorded = runs.get(key(input.threadId, input.runId));
    if (recorded !== undefined) {
      if (recorded.writer !== undefined) {
        sendError(res, 409, "conflict", "this run is still streaming");
        return;
      }
      await stream(req, res, recorded, 0);
      return;
    }
    const thread = options.threads.get(input.threadId);
    const built = buildRun(input, thread, options);
    if ("error" in built) {
      sendJson(res, built.status, built.error);
      return;
    }
    const live: LiveRun = {
      threadId: input.threadId,
      runId: input.runId,
      frames: built.frames,
      writer: undefined,
      written: 0,
      finished: false,
      cancelRequested: false,
      cutAfter: built.cutAfter,
      undo: built.undo,
    };
    runs.set(key(input.threadId, input.runId), live);
    thread.runs.set(input.runId, live);
    await stream(req, res, live, 0);
  };

  const events = async (req: IncomingMessage, res: ServerResponse, threadId: string, runId: string, query: URLSearchParams): Promise<void> => {
    const found = runs.get(key(threadId, runId));
    if (found === undefined) {
      sendError(res, 403, "scope_violation", "the run is not visible in the caller's scope", { reason: "turn_not_visible" });
      return;
    }
    if (found.writer !== undefined) {
      sendError(res, 409, "conflict", "the run's first request is still running");
      return;
    }
    const header = req.headers["last-event-id"];
    const cursor = query.get("cursor") ?? (typeof header === "string" ? header : undefined);
    let from = 0;
    if (cursor !== undefined && cursor !== "") {
      if (!/^\d+$/.test(cursor)) {
        sendError(res, 400, "validation_failed", "the cursor is malformed", { field: "cursor" });
        return;
      }
      const at = found.frames.findIndex((frame) => frame.id === cursor);
      if (at === -1) {
        sendError(res, 400, "validation_failed", "the cursor is ahead of the stream", { field: "cursor" });
        return;
      }
      from = at + 1;
    }
    if (from >= found.frames.length) {
      res.writeHead(204, { "Cache-Control": "no-store" });
      res.end();
      return;
    }
    await stream(req, res, found, from);
  };

  const cancel = (res: ServerResponse, threadId: string, runId: string): void => {
    const found = runs.get(key(threadId, runId));
    if (found === undefined) {
      sendError(res, 403, "scope_violation", "the run is not visible in the caller's scope", { reason: "turn_not_visible" });
      return;
    }
    if (found.finished || found.written >= found.frames.length) {
      sendJson(res, 200, { thread_id: threadId, run_id: runId, status: "finished" });
      return;
    }
    found.cancelRequested = true;
    // No request is reading it: the run ends here, at what was delivered.
    if (found.writer === undefined) finalizeCancel(found, found.written);
    sendJson(res, 200, { thread_id: threadId, run_id: runId, status: "cancelling" });
  };

  const handle = async (req: IncomingMessage, res: ServerResponse, path: string, query: URLSearchParams): Promise<boolean> => {
    const method = req.method ?? "GET";
    if (path === "/v1/agui" && method === "POST") {
      await run(req, res);
      return true;
    }
    if (path === "/v1/agui/capabilities" && method === "GET") {
      sendJson(res, 200, capabilities());
      return true;
    }
    const match = /^\/v1\/agui\/threads\/([^/]+)\/runs\/([^/]+)\/(events|cancel)$/.exec(path);
    if (match === null) return false;
    const threadId = decodeURIComponent(match[1] as string);
    const runId = decodeURIComponent(match[2] as string);
    if (match[3] === "events" && method === "GET") await events(req, res, threadId, runId, query);
    else if (match[3] === "cancel" && method === "POST") cancel(res, threadId, runId);
    else return false;
    return true;
  };

  return { handle };
};
