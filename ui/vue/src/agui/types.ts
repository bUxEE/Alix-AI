import type { ActionReceipt, Citation, Effect, RichBlock } from "@alixai/sdk";

// The AG-UI wire shapes this kit reads and writes, narrowed to what AlixAI's /v1/agui emits (docs/integration/agui.md) and checked
// against the pinned release (@ag-ui/core 1.0.1, schemas/agui/ag-ui-core.json). Events are parsed defensively: a member that is
// not of the expected type is dropped, never trusted. The kit has no dependency on @ag-ui/* or CopilotKit.

/** One AG-UI event as it arrives: a `type` and whatever members that type carries. Narrowed by the reducer, never trusted. */
export interface AguiEvent {
  readonly type: string;
  readonly [member: string]: unknown;
}

/** A pre-uploaded file the user's message refers to. Inline bytes and URLs are refused by the gateway and never sent. */
export interface AguiFileSource {
  readonly type: "file";
  readonly value: string;
}

export type AguiContentPart =
  | { readonly type: "text"; readonly text: string }
  | { readonly type: "image" | "audio" | "video" | "document"; readonly source: AguiFileSource };

export type AguiMessage =
  | { readonly id: string; readonly role: "user"; readonly content: string | readonly AguiContentPart[] }
  | { readonly id: string; readonly role: "tool"; readonly toolCallId: string; readonly content: string };

/** The answer to an interrupt. The payload of an approval is exactly `{approved: boolean}`; the secret is the host relay's. */
export interface AguiResumeEntry {
  readonly interruptId: string;
  readonly status: "resolved" | "cancelled";
  readonly payload?: { readonly approved: boolean };
}

export interface RunAgentInput {
  readonly threadId: string;
  readonly runId: string;
  readonly messages: readonly AguiMessage[];
  readonly state?: unknown;
  readonly tools?: readonly unknown[];
  readonly context?: readonly { readonly description: string; readonly value: string }[];
  readonly forwardedProps?: { readonly alixai?: { readonly choiceId?: string; readonly language?: string } };
  readonly resume?: readonly AguiResumeEntry[];
}

// ---- what the stream says, after parsing ----

export type AlixBlockType =
  | "table"
  | "chart"
  | "diagram"
  | "citation_list"
  | "action_card"
  | "clarification_choices"
  | "confirmation_panel"
  | "kpi"
  | "timeline"
  | "record_card"
  | "provenance"
  | "draft";

export type AlixBlock<T extends AlixBlockType = AlixBlockType> = Extract<RichBlock, { type: T }>;

/** What the confirmation card shows: projected by the core from the prepared action, never from the model's text. */
export interface AlixConfirmation {
  readonly preparationId: string;
  readonly capabilityId: string;
  readonly summary: string;
  readonly effects: readonly Effect[];
  readonly riskLevel: number;
  readonly expiresAt: string;
  /** `explicit` (the user approves here) or `step_up` (the application's own interface must complete it). */
  readonly mode: string;
}

export interface AlixInterrupt {
  readonly id: string;
  readonly reason?: string;
  readonly message?: string;
  readonly toolCallId?: string;
  readonly expiresAt?: string;
  readonly metadata?: unknown;
}

export interface AlixAttachment {
  readonly fileId: string;
  readonly name: string;
  readonly mimeType: string;
  readonly sizeBytes: number;
  readonly previewable: boolean;
  readonly expiresAt?: string;
}

export interface AlixProgress {
  readonly stage: string;
  readonly state?: string;
  readonly capabilityId?: string;
  readonly percent?: number;
}

export interface AlixSuggestion {
  readonly title: string;
  readonly message: string;
  readonly capabilityId?: string;
}

/** What the active goal waits for: the client-safe projection of the session (`state.alixai.pending`). */
export interface AlixPending {
  readonly kind: "confirmation" | "clarification" | "goal";
  readonly capabilityId: string;
  readonly phase: string;
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

/** A call of a UI tool the host registered: an instruction to the client's screen, never to the application. */
export interface AlixUiCall {
  readonly id: string;
  readonly name: string;
  readonly args: unknown;
}

export type ConfirmationStatus = "pending" | "submitting" | "approved" | "declined" | "dismissed" | "failed";

/** Status of a memory proposal card while the host confirms or rejects. */
export type MemoryProposalStatus = "pending" | "submitting" | "remembered" | "forgotten" | "expired" | "failed";

/** What the memory proposal card shows (from `alixai.memory_proposal`). */
export interface MemoryProposalView {
  readonly id: string;
  readonly text: string;
  readonly expiresAt?: string;
}

/** One row of `AlixMemoryList` (host-owned data). */
export interface MemoryListItem {
  readonly id: string;
  readonly text: string;
  readonly status: "proposed" | "active" | "rejected";
}

/** One entry of the conversation timeline, in the order the stream produced it. */
export type ChatItem =
  | { kind: "message"; id: string; role: "user" | "assistant"; text: string; streaming: boolean; turnId?: string }
  | { kind: "attachments"; id: string; files: AlixAttachment[]; turnId?: string }
  | { kind: "block"; id: string; blockType: AlixBlockType; block: RichBlock; answered: boolean; turnId?: string }
  | { kind: "citations"; id: string; citations: Citation[]; turnId?: string }
  | { kind: "receipt"; id: string; receipt: ActionReceipt; turnId?: string }
  | { kind: "confirmation"; id: string; confirmation: AlixConfirmation; interruptId: string; status: ConfirmationStatus; turnId?: string }
  | { kind: "memory_proposal"; id: string; proposal: MemoryProposalView; status: MemoryProposalStatus; turnId?: string }
  | { kind: "continue"; id: string; summary: string; preparationId?: string; turnId?: string }
  | { kind: "error"; id: string; message: string; code?: string; turnId?: string };

export type RunOutcome = "success" | "interrupt" | "cancelled" | "error";

export interface ChatState {
  items: ChatItem[];
  threadId: string | undefined;
  runId: string | undefined;
  running: boolean;
  /** When true, `alixai.memory_proposal` CUSTOM events become timeline items (opt-in; default false). */
  memoryEnabled: boolean;
  /** The pipeline stage that is running now (`STEP_STARTED` without its `STEP_FINISHED`). */
  step: string | undefined;
  progress: Record<string, AlixProgress>;
  shared: unknown;
  pending: AlixPending | undefined;
  suggestions: AlixSuggestion[];
  interrupts: AlixInterrupt[];
  outcome: RunOutcome | undefined;
  error: { message: string; code?: string } | undefined;
  uiCalls: AlixUiCall[];
  /** Working data of the reducer (open tool calls, activity content for patches). Not for rendering. */
  internal: {
    toolNames: Record<string, string>;
    toolArgs: Record<string, string>;
    activities: Record<string, { type: string; content: unknown }>;
  };
}

/** One conversation in a switcher. The public API has no "list my conversations" operation: a conversation is a session the host minted, so the host supplies these. */
export interface ConversationSummary {
  /** The session id (the AG-UI thread id). */
  id: string;
  title?: string;
  /** A short preview of the last message, as text. */
  preview?: string;
  /** RFC 3339 time of the last activity. */
  updatedAt?: string;
}
