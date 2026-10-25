import type { ActionReceipt, Citation, RichBlock } from "@alixai/sdk";
import type { AlixAttachment, AlixBlock, AlixBlockType, AlixConfirmation, AlixInterrupt, AlixPending, AlixProgress, AlixSuggestion } from "./types";

// The names and shapes AlixAI puts into AG-UI activities, custom events and interrupts (docs/integration/agui.md sections 2, 6, 7).
// Everything that arrives here is data from a stream: every parser checks the shape and returns undefined for anything else, so
// a renderer shows nothing rather than something wrong. None of it is markup; renderers show it as text.

export const ALIXAI_ACTIVITY = Object.freeze({
  attachments: "alixai.attachments",
  progress: "alixai.progress",
  citations: "alixai.citations",
  receipt: "alixai.receipt",
  blockPrefix: "alixai.block.",
} as const);

export const ALIXAI_CUSTOM_EVENT = Object.freeze({
  directive: "alixai.directive",
  suggestions: "alixai.suggestions",
} as const);

export const CONFIRM_TOOL = "alixai_confirm_action";
export const CONTINUE_TOOL = "alixai_continue_in_app";
export const CONFIRM_CALL_PREFIX = "alixai_confirm_";
export const UI_CALL_PREFIX = "alixai_ui_";

export const BLOCK_TYPES: readonly AlixBlockType[] = [
  "table",
  "chart",
  "diagram",
  "citation_list",
  "action_card",
  "clarification_choices",
  "confirmation_panel",
];

export const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

const isString = (value: unknown): value is string => typeof value === "string";

/** The block type an `alixai.block.<type>` activity names, or undefined for any other activity type. */
export const blockTypeOfActivity = (activityType: string): AlixBlockType | undefined => {
  if (!activityType.startsWith(ALIXAI_ACTIVITY.blockPrefix)) return undefined;
  const type = activityType.slice(ALIXAI_ACTIVITY.blockPrefix.length);
  return (BLOCK_TYPES as readonly string[]).includes(type) ? (type as AlixBlockType) : undefined;
};

/** Reads the metadata of a `confirmation_required` interrupt. Anything that is not exactly that is undefined. */
export const parseConfirmation = (interrupt: AlixInterrupt | undefined | null): AlixConfirmation | undefined => {
  if (interrupt === undefined || interrupt === null || interrupt.reason !== "confirmation_required") return undefined;
  const meta = interrupt.metadata;
  if (!isRecord(meta)) return undefined;
  const { preparationId, capabilityId, summary, effects, riskLevel, expiresAt, confirmation } = meta;
  if (!isString(preparationId) || !isString(capabilityId) || !isString(summary) || !isString(expiresAt)) return undefined;
  if (!Array.isArray(effects) || !effects.every(isRecord)) return undefined;
  if (typeof riskLevel !== "number" || !Number.isFinite(riskLevel)) return undefined;
  const mode = isRecord(confirmation) && isString(confirmation["mode"]) ? confirmation["mode"] : "explicit";
  return { preparationId, capabilityId, summary, effects: effects as unknown as AlixConfirmation["effects"], riskLevel, expiresAt, mode };
};

/** Reads an interrupt of a `RUN_FINISHED` outcome. */
export const parseInterrupt = (value: unknown): AlixInterrupt | undefined => {
  if (!isRecord(value) || !isString(value["id"])) return undefined;
  return {
    id: value["id"],
    ...(isString(value["reason"]) && { reason: value["reason"] }),
    ...(isString(value["message"]) && { message: value["message"] }),
    ...(isString(value["toolCallId"]) && { toolCallId: value["toolCallId"] }),
    ...(isString(value["expiresAt"]) && { expiresAt: value["expiresAt"] }),
    ...(value["metadata"] !== undefined && { metadata: value["metadata"] }),
  };
};

export const parseAttachments = (content: unknown): AlixAttachment[] | undefined => {
  if (!isRecord(content) || !Array.isArray(content["files"])) return undefined;
  const files: AlixAttachment[] = [];
  for (const file of content["files"]) {
    if (!isRecord(file) || !isString(file["fileId"]) || !isString(file["name"]) || !isString(file["mimeType"])) return undefined;
    if (typeof file["sizeBytes"] !== "number" || typeof file["previewable"] !== "boolean") return undefined;
    files.push({
      fileId: file["fileId"],
      name: file["name"],
      mimeType: file["mimeType"],
      sizeBytes: file["sizeBytes"],
      previewable: file["previewable"],
      ...(isString(file["expiresAt"]) && { expiresAt: file["expiresAt"] }),
    });
  }
  return files;
};

export const parseCitations = (content: unknown): Citation[] | undefined => {
  if (!isRecord(content) || !Array.isArray(content["citations"])) return undefined;
  const ok = content["citations"].every(
    (citation) => isRecord(citation) && isString(citation["source_id"]) && isString(citation["display_name"]) && isRecord(citation["open_ref"]),
  );
  return ok ? (content["citations"] as Citation[]) : undefined;
};

export const parseProgress = (content: unknown): AlixProgress | undefined => {
  if (!isRecord(content) || !isString(content["stage"])) return undefined;
  return {
    stage: content["stage"],
    ...(isString(content["state"]) && { state: content["state"] }),
    ...(isString(content["capabilityId"]) && { capabilityId: content["capabilityId"] }),
    ...(typeof content["percent"] === "number" && { percent: content["percent"] }),
  };
};

const arrayOf = (value: unknown): value is unknown[] => Array.isArray(value);

/** Reads the content of an `alixai.block.<type>` activity; the block must be of the type its activity names and of the shape that type has. */
export const parseBlock = <T extends AlixBlockType>(type: T, content: unknown): AlixBlock<T> | undefined => {
  if (!isRecord(content) || content["type"] !== type) return undefined;
  const ok = (() => {
    switch (type) {
      case "table":
        return arrayOf(content["columns"]) && arrayOf(content["rows"]);
      case "chart":
        return isString(content["mark"]) && arrayOf(content["data"]) && isRecord(content["encoding"]) && isString(content["description"]);
      case "diagram":
        return isString(content["source"]) && isString(content["description"]);
      case "citation_list":
        return parseCitations(content) !== undefined;
      case "action_card":
        return isString(content["title"]) && arrayOf(content["facts"]) && arrayOf(content["actions"]);
      case "clarification_choices":
        return arrayOf(content["choices"]) && isString(content["field"]);
      case "confirmation_panel":
        return isString(content["preparation_id"]) && isString(content["summary"]) && arrayOf(content["effects"]);
      default:
        return false;
    }
  })();
  return ok ? (content as unknown as AlixBlock<T>) : undefined;
};

/** Reads the receipt of a committed (or refused) action: the activity content, or the result of the confirmation's tool call. */
export const parseReceipt = (content: unknown): ActionReceipt | undefined => {
  if (!isRecord(content) || !isString(content["receipt_id"]) || !isString(content["status"]) || !isString(content["preparation_id"])) return undefined;
  if (!arrayOf(content["facts"])) return undefined;
  return content as unknown as ActionReceipt;
};

/** Reads the value of the `alixai.suggestions` custom event. Picking one sends `message` as an ordinary user message. */
export const parseSuggestions = (value: unknown): AlixSuggestion[] => {
  if (!isRecord(value) || !Array.isArray(value["suggestions"])) return [];
  const out: AlixSuggestion[] = [];
  for (const item of value["suggestions"]) {
    if (!isRecord(item) || !isString(item["title"]) || !isString(item["message"])) continue;
    out.push({ title: item["title"], message: item["message"], ...(isString(item["capabilityId"]) && { capabilityId: item["capabilityId"] }) });
  }
  return out;
};

const PENDING_KINDS: readonly string[] = ["confirmation", "clarification", "goal"];

const listOf = <T>(value: unknown, read: (item: unknown) => T | undefined): T[] | undefined => {
  if (value === undefined || value === null) return [];
  if (!Array.isArray(value)) return undefined;
  const out: T[] = [];
  for (const item of value) {
    const parsed = read(item);
    if (parsed === undefined) return undefined;
    out.push(parsed);
  }
  return out;
};

/**
 * Reads the pending goal from the shared state of the run: `state.alixai.pending`. An allow-list, like the projection that produced
 * it: a member that is not named here is dropped, and a state that is not exactly this shape is undefined. The page's own state
 * members are never read.
 */
export const parsePendingState = (state: unknown): AlixPending | undefined => {
  if (!isRecord(state) || !isRecord(state["alixai"])) return undefined;
  const pending = state["alixai"]["pending"];
  if (!isRecord(pending)) return undefined;
  const { kind, capabilityId, phase, collectedFields, missingFields, question, preparationId, expiresAt } = pending;
  if (!isString(kind) || !PENDING_KINDS.includes(kind) || !isString(capabilityId) || !isString(phase)) return undefined;
  const collected = listOf(collectedFields, (field) =>
    isRecord(field) && isString(field["name"]) && isString(field["origin"]) ? { name: field["name"], origin: field["origin"] } : undefined,
  );
  const missing = listOf(missingFields, (field) => (isString(field) ? field : undefined));
  if (collected === undefined || missing === undefined) return undefined;
  let asked: AlixPending["question"];
  if (question !== undefined && question !== null) {
    if (!isRecord(question) || !isString(question["field"]) || !isString(question["answerKind"])) return undefined;
    const choices = listOf(question["choices"], (choice) =>
      isRecord(choice) && isString(choice["id"]) && isString(choice["label"]) ? { id: choice["id"], label: choice["label"] } : undefined,
    );
    if (choices === undefined) return undefined;
    asked = { field: question["field"], answerKind: question["answerKind"], choices };
  }
  return {
    kind: kind as AlixPending["kind"],
    capabilityId,
    phase,
    collectedFields: collected,
    missingFields: missing,
    ...(asked !== undefined && { question: asked }),
    ...(isString(preparationId) && { preparationId }),
    ...(isString(expiresAt) && { expiresAt }),
  };
};

/** A canonical value of the application's facts as plain text (never markup). */
export const formatCanonicalValue = (value: unknown): string => {
  if (!isRecord(value)) return value === null || value === undefined ? "" : String(value);
  const inner = value["value"];
  if (value["kind"] === "money" && isString(value["currency"])) return `${String(inner)} ${value["currency"]}`;
  return inner === null || inner === undefined ? "" : String(inner);
};

export type { RichBlock };
