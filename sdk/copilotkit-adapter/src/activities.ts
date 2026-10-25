import type { Citation, Effect, RichBlock } from "@alixai/sdk";

// The names and shapes AlixAI puts into AG-UI activities, custom events and interrupts (docs/integration/agui.md
// sections 2, 6 and 7). Everything that arrives here is data from a stream: the parsers check the shape and return
// undefined for anything else, so a renderer shows nothing rather than something wrong. None of it is markup; renderers
// show it as text.

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

export const ALIXAI_BLOCK_TYPES = [
  "table",
  "chart",
  "diagram",
  "citation_list",
  "action_card",
  "clarification_choices",
  "confirmation_panel",
] as const;

export type AlixAIBlockType = (typeof ALIXAI_BLOCK_TYPES)[number];
export type AlixAIBlock<T extends AlixAIBlockType = AlixAIBlockType> = Extract<RichBlock, { type: T }>;

/** The activity type of a validated block: `alixai.block.<type>`. */
export const blockActivityType = (type: AlixAIBlockType): string => `${ALIXAI_ACTIVITY.blockPrefix}${type}`;

/** The id prefix of the UI tool call of a turn (`alixai_ui_<turn id>`); its result goes back as a tool message. */
export const UI_TOOL_CALL_PREFIX = "alixai_ui_";

const isRecord = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);

const isString = (value: unknown): value is string => typeof value === "string";

// ---- the confirmation interrupt ----

/** What the confirmation card shows: projected by the core from the prepared action, never from the model's text. */
export interface AlixAIConfirmation {
  readonly preparationId: string;
  readonly capabilityId: string;
  readonly summary: string;
  readonly effects: readonly Effect[];
  readonly riskLevel: number;
  readonly expiresAt: string;
  readonly mode: string;
}

/** The interrupt of an AG-UI run that ended in `outcome: interrupt`, as the confirmation helpers read it. */
export interface AlixAIInterrupt {
  readonly id: string;
  readonly reason?: string;
  readonly message?: string;
  readonly toolCallId?: string;
  readonly expiresAt?: string;
  readonly metadata?: unknown;
}

/** Reads the metadata of a `confirmation_required` interrupt. Anything that is not exactly that is undefined. */
export const parseConfirmation = (interrupt: AlixAIInterrupt | undefined | null): AlixAIConfirmation | undefined => {
  if (interrupt === undefined || interrupt === null || interrupt.reason !== "confirmation_required") return undefined;
  const meta = interrupt.metadata;
  if (!isRecord(meta)) return undefined;
  const { preparationId, capabilityId, summary, effects, riskLevel, expiresAt, confirmation } = meta;
  if (!isString(preparationId) || !isString(capabilityId) || !isString(summary) || !isString(expiresAt)) return undefined;
  if (!Array.isArray(effects) || !effects.every(isRecord)) return undefined;
  if (typeof riskLevel !== "number" || !Number.isFinite(riskLevel)) return undefined;
  const mode = isRecord(confirmation) && isString(confirmation["mode"]) ? confirmation["mode"] : "explicit";
  return { preparationId, capabilityId, summary, effects: effects as unknown as Effect[], riskLevel, expiresAt, mode };
};

/** The one payload an approval carries: exactly `{"approved": true}`; a decline is `{"approved": false}`. */
export const approvalPayload = (approved: boolean): { approved: boolean } => ({ approved });

// ---- activities ----

export interface AlixAIAttachment {
  readonly fileId: string;
  readonly name: string;
  readonly mimeType: string;
  readonly sizeBytes: number;
  readonly previewable: boolean;
  readonly expiresAt?: string;
}

export const parseAttachments = (content: unknown): AlixAIAttachment[] | undefined => {
  if (!isRecord(content) || !Array.isArray(content["files"])) return undefined;
  const files: AlixAIAttachment[] = [];
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

export interface AlixAIProgress {
  readonly stage: string;
  readonly state?: string;
  readonly capabilityId?: string;
  readonly percent?: number;
}

export const parseProgress = (content: unknown): AlixAIProgress | undefined => {
  if (!isRecord(content) || !isString(content["stage"])) return undefined;
  return {
    stage: content["stage"],
    ...(isString(content["state"]) && { state: content["state"] }),
    ...(isString(content["capabilityId"]) && { capabilityId: content["capabilityId"] }),
    ...(typeof content["percent"] === "number" && { percent: content["percent"] }),
  };
};

/** Reads the content of an `alixai.block.<type>` activity; the block must be of the type its activity names. */
export const parseBlock = <T extends AlixAIBlockType>(type: T, content: unknown): AlixAIBlock<T> | undefined => {
  if (!isRecord(content) || content["type"] !== type) return undefined;
  switch (type) {
    case "table":
      return Array.isArray(content["columns"]) && Array.isArray(content["rows"]) ? (content as unknown as AlixAIBlock<T>) : undefined;
    case "chart":
      return isString(content["mark"]) && Array.isArray(content["data"]) && isRecord(content["encoding"]) ? (content as unknown as AlixAIBlock<T>) : undefined;
    case "clarification_choices":
      return Array.isArray(content["choices"]) && isString(content["field"]) ? (content as unknown as AlixAIBlock<T>) : undefined;
    default:
      return content as unknown as AlixAIBlock<T>;
  }
};

// ---- suggestions ----

export interface AlixAISuggestion {
  readonly title: string;
  readonly message: string;
  readonly capabilityId?: string;
}

/** Reads the value of the `alixai.suggestions` custom event. Picking one sends `message` as an ordinary user message. */
export const parseSuggestions = (value: unknown): AlixAISuggestion[] => {
  if (!isRecord(value) || !Array.isArray(value["suggestions"])) return [];
  const out: AlixAISuggestion[] = [];
  for (const item of value["suggestions"]) {
    if (!isRecord(item) || !isString(item["title"]) || !isString(item["message"])) continue;
    out.push({ title: item["title"], message: item["message"], ...(isString(item["capabilityId"]) && { capabilityId: item["capabilityId"] }) });
  }
  return out;
};

// ---- shared state ----

/** What the active goal waits for: the client-safe projection of the session (`{"alixai": {"pending": ...}}`, section 8 of agui.md). */
export interface AlixAIPending {
  readonly kind: "confirmation" | "clarification" | "goal";
  readonly capabilityId: string;
  readonly phase: string;
  /** Names and origins of the fields the goal has a value for; the values stay in the session. */
  readonly collectedFields: readonly { readonly name: string; readonly origin: string }[];
  readonly missingFields: readonly string[];
  readonly question?: {
    readonly field: string;
    readonly answerKind: string;
    readonly choices: readonly { readonly id: string; readonly label: string }[];
  };
  readonly preparationId?: string;
  readonly expiresAt?: string;
}

const PENDING_KINDS: readonly string[] = ["confirmation", "clarification", "goal"];

/**
 * Reads the pending goal from the shared state of the agent (`agent.state`, or the snapshot of a run): `state.alixai.pending`.
 * It is an allow-list, like the projection that produced it: a member that is not named here is dropped, and a state that is
 * not exactly this shape (an unknown kind, a field of the wrong type) is undefined, so a renderer shows nothing rather than
 * something wrong. The page's own state members are never read.
 */
export const parsePendingState = (state: unknown): AlixAIPending | undefined => {
  if (!isRecord(state) || !isRecord(state["alixai"])) return undefined;
  const pending = state["alixai"]["pending"];
  if (!isRecord(pending)) return undefined;
  const { kind, capabilityId, phase, collectedFields, missingFields, question, preparationId, expiresAt } = pending;
  if (!isString(kind) || !PENDING_KINDS.includes(kind) || !isString(capabilityId) || !isString(phase)) return undefined;

  const collected: { name: string; origin: string }[] = [];
  for (const field of collectedFields === undefined || collectedFields === null ? [] : Array.isArray(collectedFields) ? collectedFields : [undefined]) {
    if (!isRecord(field) || !isString(field["name"]) || !isString(field["origin"])) return undefined;
    collected.push({ name: field["name"], origin: field["origin"] });
  }
  const missing = missingFields === undefined || missingFields === null ? [] : missingFields;
  if (!Array.isArray(missing) || !missing.every(isString)) return undefined;

  let asked: AlixAIPending["question"];
  if (question !== undefined && question !== null) {
    if (!isRecord(question) || !isString(question["field"]) || !isString(question["answerKind"])) return undefined;
    const choices: { id: string; label: string }[] = [];
    const offered = question["choices"];
    for (const choice of offered === undefined || offered === null ? [] : Array.isArray(offered) ? offered : [undefined]) {
      if (!isRecord(choice) || !isString(choice["id"]) || !isString(choice["label"])) return undefined;
      choices.push({ id: choice["id"], label: choice["label"] });
    }
    asked = { field: question["field"], answerKind: question["answerKind"], choices };
  }
  return {
    kind: kind as AlixAIPending["kind"],
    capabilityId,
    phase,
    collectedFields: collected,
    missingFields: missing as string[],
    ...(asked !== undefined && { question: asked }),
    ...(isString(preparationId) && { preparationId }),
    ...(isString(expiresAt) && { expiresAt }),
  };
};

// ---- values ----

/** A canonical value of the application's facts as plain text (never markup). */
export const formatCanonicalValue = (value: unknown): string => {
  if (!isRecord(value)) return value === null || value === undefined ? "" : String(value);
  const inner = value["value"];
  if (value["kind"] === "money" && isString(value["currency"])) return `${String(inner)} ${value["currency"]}`;
  return inner === null || inner === undefined ? "" : String(inner);
};
