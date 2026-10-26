// @alixai/sdk: the TypeScript client of the AlixAI public API (master plan 15.1).
//
// This file is complete. Later milestones fill their own folder (src/rag, src/voice, src/providers,
// src/decision), and their exports appear here as namespaces without anyone editing this file.

// ---- the client ----
export { createClient } from "./client.js";
export type { AlixAIClient, ClientOptions, ResumeOptions, StreamEventsOptions, StreamOptions } from "./client.js";
export type { CallOptions, ContextTokenProvider, ContextTokenRequest } from "./transport.js";

// ---- turns, streaming and reconnecting ----
export { CONFIRMATION_TOKEN_HEADER } from "./turn.js";
export type { TurnStream, TurnTransport } from "./turn.js";
export { TURN_EVENT_TYPES, doneEvent, turnText } from "./events.js";
// The WebSocket transport is opt-in through a host-supplied factory (a browser WebSocket cannot send the Authorization header).
export type { StreamTransport, WebSocketFactory, WebSocketInit, WebSocketLike } from "./ws.js";

// ---- messages with attachments, files ----
export { attachmentKind } from "./compose.js";
export type { AttachmentKind, MessageStream, OutgoingMessage } from "./compose.js";
export type { FilesApi, ReadFileOptions, UploadOptions } from "./files.js";

// ---- conversations: stop, regenerate, edit-and-resubmit, resume, switching ----
export type { Conversation, ConversationOptions, RegenerateOptions, SentInput, TurnRecord, TurnStatus } from "./conversation.js";

// ---- what a UI renders from: blocks, citations, source links, the turn as one object ----
export { blocksOfType, citationLink, viewTurn } from "./view.js";
export type { CitationLink, TurnView } from "./view.js";

// ---- history, settings, exports ----
export type { HistoryApi, HistoryExport, SegmentOptions } from "./history.js";
export type { SettingsApi } from "./settings.js";
export type { ExportCreateInput, ExportsApi } from "./exports/index.js";

// ---- errors ----
export { AlixAIError, ERROR_CODES, isAlixAIError, isErrorCode, isRetryableCode } from "./errors.js";
export type { AlixAIErrorInit } from "./errors.js";

// ---- the operations the SDK speaks (keyed by the OpenAPI operationId) ----
export { OPERATIONS } from "./operations.js";
export type { HttpMethod, OperationId, OperationSpec } from "./operations.js";

// ---- wire types of /v1 that the OpenAPI document defines (hand-written, checked against it) ----
export type * from "./api-types.js";

// ---- generated types (src/gen, from schemas/): the most used ones flat, all of them under `types` ----
export type { ErrorCode } from "./gen/common/index.js";
export type { Error as ErrorBody, Scope } from "./gen/common/index.js";
export type { ExportRequest, ExportRequestFormat, FileRef, TaskProposal } from "./gen/conversation/index.js";
export type {
  ActionReceipt,
  PreparedAction,
  Effect,
  Fact,
  CanonicalValue,
  Choice,
  RiskLevel,
} from "./gen/actions/index.js";
export type {
  BlockEvent,
  Citation,
  CitationEvent,
  ClarificationEvent,
  DialogueDirective,
  DirectiveEvent,
  DoneEvent,
  ErrorEvent,
  PreparedActionEvent,
  ProgressEvent,
  ReceiptEvent,
  RichBlock,
  TokenEvent,
  TurnEvent,
  MemoryProposalEvent,
} from "./gen/conversation/index.js";
export type * as types from "./gen/index.js";

// ---- namespaces owned by later milestones (empty until then) ----
export * as rag from "./rag/index.js";
export * as voice from "./voice/index.js";
export * as providers from "./providers/index.js";
export * as decision from "./decision/index.js";
export * as me from "./me/index.js";
export * as memory from "./memory/index.js";
export * as exports from "./exports/index.js";

