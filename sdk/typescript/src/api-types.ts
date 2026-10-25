import type { ActionReceipt, Choice, PreparedAction } from "./gen/actions/index.js";
import type { FileRefTreatment, TurnEvent } from "./gen/conversation/index.js";
import type { DeploymentSettings, EffectiveSettings, TenantSettings, UserSettings } from "./gen/settings/index.js";

// Wire types of the /v1 API that are defined by the OpenAPI document (schemas/api/openapi.yaml and
// its fragments) rather than by a JSON Schema, so they are not in src/gen. They are written by hand
// and kept in step with the document by test/openapi-contract.test.ts. Member names are the wire's
// (snake_case); only the SDK's own options and method parameters are camelCase.

// ---- sessions ----

/** Where a collected field of the pending goal came from. */
export type FieldOrigin = "user_turn" | "correction" | "entity_resolution" | "choice";

export interface PendingField {
  name: string;
  origin: FieldOrigin;
}

export interface PendingQuestion {
  field: string;
  answer_kind: "text" | "choice" | "entity";
  choices?: Choice[];
}

/**
 * What the active goal of a session waits for: a confirmation of a prepared action, an answer to a
 * clarification, or nothing blocking. Collected values are never returned, only field names. The
 * confirmation secret is never part of it: it cannot be recovered after the turn that issued it.
 */
export interface PendingState {
  kind: "confirmation" | "clarification" | "goal";
  capability_id: string;
  phase: string;
  collected_fields: PendingField[];
  missing_fields?: string[];
  question?: PendingQuestion;
  preparation?: PreparedAction;
  expires_at?: string;
}

export interface Session {
  session_id: string;
  status: "active" | "ended";
  turn_count: number;
  created_at: string;
  updated_at: string;
  pending: PendingState | null;
}

export interface SessionEnded {
  session_id: string;
  status: "ended";
}

// ---- turns ----

/**
 * What a user can send in one turn: exactly one of a text, a choice, a confirmation or a rejection.
 * A confirmation is a structured input from the client UI, `{preparation_id, token}`; no text can
 * ever confirm anything. A text turn may also name the files the user uploaded for it (`attachments`: ids of
 * `client.files.upload`, at most 4, never with a choice, a confirmation or a rejection). They are resolved under the
 * turn's scope before anything runs: a file that is not the caller's, one deleted, expired or unknown fails the whole turn
 * with one `scope_violation`. `client.sendMessage` uploads and names them for you.
 */
export type TurnInput =
  | { text: string; language?: string; attachments?: string[] }
  | { choice_id: string; language?: string }
  | { confirmation: { preparation_id: string; token: string } }
  | { reject: { preparation_id: string } };

/**
 * The single-use secret that lets the user's client confirm one prepared action. It is not a
 * TurnEvent: it arrives in the X-AlixAI-Confirmation-Token header of an event stream, or in the
 * `confirmation` member of a JSON turn. Show it to the user's client only, present it once.
 */
export interface ConfirmationGrant {
  preparation_id: string;
  token: string;
}

/** A finished turn: its events in order (the last is `done`) and, when it prepared an action that needs the user, the grant. */
export interface TurnResult {
  turn_id: string;
  events: TurnEvent[];
  confirmation?: ConfirmationGrant;
}

// ---- actions ----

/**
 * The body of a confirmation that reached the host and committed (or queued) the action. The OpenAPI
 * document lists both members as optional because the same shape also carries a failure; a 200 always
 * has the receipt, and the SDK refuses a 200 without one.
 */
export interface ActionOutcome {
  preparation?: PreparedAction;
  receipt: ActionReceipt;
}

// ---- applications ----

export type ApplicationStatus = "active" | "quarantined" | "rejected" | "superseded" | "not_installed";

export interface PackageRecord {
  manifest_hash: string;
  package_version: string;
  state: "quarantined" | "active" | "rejected" | "superseded";
  status_reason?: string;
  signer_key_id?: string;
  warnings: number;
  audit_report_id?: string;
  installed_at: string;
  status_changed_at: string;
}

export interface Registration {
  application_id: string;
  package: PackageRecord;
  /** True when this call installed the package, false when the same content was already installed. */
  created: boolean;
  findings: Array<{
    check_id: string;
    severity: "warning" | "info";
    capability?: string;
    path?: string;
    message: string;
    remediation: string;
  }>;
}

export interface Application {
  application_id: string;
  status: ApplicationStatus;
  packages: PackageRecord[];
  health?: {
    status: "ok" | "degraded" | "unavailable";
    reason?: string;
    application_version?: string;
  };
}

export interface Capability {
  id: string;
  title?: string;
  description?: string;
  /** The effective risk level the policy gateway assigns, 0 to 3. */
  risk_level: 0 | 1 | 2 | 3;
  requires_confirmation: boolean;
  requires_step_up: boolean;
  fields: string[];
}

export interface CapabilityList {
  capabilities: Capability[];
}

// ---- operations ----

export type ReadinessStatus = "ok" | "degraded" | "unavailable";

export interface ReadyRole {
  role: string;
  configured: boolean;
  status: ReadinessStatus;
  reason?: string;
  effect?: string;
}

export interface ReadyCheck {
  name: string;
  ok: boolean;
  reason?: string;
}

/** The readiness report of /readyz (200 when ready, 503 when not: both are returned, neither is thrown). */
export interface ReadyReport {
  status: ReadinessStatus;
  ready: boolean;
  roles: ReadyRole[];
  checks: ReadyCheck[];
}

/** The authenticated report of /v1/health: the readiness report plus the caller's application. */
export interface Health extends ReadyReport {
  application: {
    application_id: string;
    status: ApplicationStatus;
  };
}

export interface Liveness {
  status: "ok";
}

// ---- files (schemas/api/paths/files.yaml) ----

/** How an uploaded file is treated, which the host's attachment policy decides (`FileRef.treatment` has the same values). */
export type FileTreatment = FileRefTreatment;

/** What a client sends to store one file. The core sniffs the bytes: a declared type that is not what the bytes are stores nothing. */
export interface FileUpload {
  /** The bytes. */
  data: Blob | Uint8Array | ArrayBuffer;
  /** A display name; the core reduces it to a bare, sanitised file name. */
  name: string;
  /** The media type the sender declares, for example `image/png`. HTML, SVG, XML and script types are never stored. */
  mimeType: string;
  /** Without one the host policy's default applies (`ephemeral_context`: readable by the model inside this session, for a short time). */
  treatment?: FileTreatment;
}

/** The bytes of a downloaded file and what the response said about them. */
export interface FileContent {
  data: Uint8Array;
  /** The sniffed media type. */
  contentType: string;
  /** The name from `Content-Disposition`, when it had one. */
  fileName: string | undefined;
  size: number;
}

/** A safe rendering of a file: raster images as themselves, text as `text/plain`, cut to the deployment's limit. */
export interface FilePreview {
  data: Uint8Array;
  contentType: string;
  /** True when a text preview was cut to its limit. */
  truncated: boolean;
}

// ---- history (schemas/api/paths/history.yaml) ----

export type HistoryRole = "user" | "assistant";

/** One row of a transcript. An assistant row is Markdown that has been sanitised exactly like the `token` events of a live turn. */
export interface HistoryTurn {
  turn_id: string;
  /** The turn number inside the session; the user's row and the assistant's row of one turn share it. */
  n: number;
  role: HistoryRole;
  /** The directive act of an assistant row. */
  act?: string;
  /** A user row: the user's own text, plain. An assistant row: sanitised Markdown. Empty when `withheld`. */
  body: string;
  /** True when the text was cut when it was stored. */
  clipped: boolean;
  created_at: string;
  /** True when `body` is empty because the sanitiser refused the text of an assistant row. */
  withheld?: boolean;
}

export interface HistorySegmentPage {
  session_id: string;
  turns: HistoryTurn[];
  /** The cursor of the next page, or null at the end of the transcript. */
  next_cursor: string | null;
  /** How many rows the session's transcript holds. */
  total: number;
}

export interface HistorySessionCleared {
  session_id: string;
  rows_deleted: number;
}

export interface HistorySearchRequest {
  /** The words to find; every one must occur in the row. At most 512 characters. Plain text: nothing in it is syntax. */
  text: string;
  /** Restrict the search to one of the caller's sessions. */
  session_id?: string;
  /** How many hits a page holds at most, 1 to 50 (10 when not given). */
  limit?: number;
  /** How many hits to skip: the `next_offset` of the previous page. */
  offset?: number;
}

export interface HistorySearchHit {
  session_id: string;
  turn_id: string;
  n: number;
  role: HistoryRole;
  created_at: string;
  /** A short excerpt around the match; an assistant excerpt is sanitised Markdown, a user excerpt plain text. Empty if the sanitiser refused it. */
  snippet: string;
}

export interface HistorySearchPage {
  hits: HistorySearchHit[];
  /** The offset of the next page, or null at the end. */
  next_offset: number | null;
}

export interface HistoryExportRequest {
  session_id?: string;
  /** Include rows created at or after this time (RFC 3339). */
  since?: string;
  /** Include rows created before this time (RFC 3339). */
  until?: string;
}

export interface HistoryExportRow {
  session_id: string;
  turn_id: string;
  n: number;
  role: HistoryRole;
  act?: string;
  body: string;
  clipped: boolean;
  created_at: string;
  withheld?: boolean;
}

export interface HistoryCleanupResult {
  rows_deleted: number;
}

// ---- settings (schemas/api/paths/settings.yaml, documents in schemas/settings) ----

/** A settings document with its revision. Version 0 and the empty document mean the level has none yet. */
export interface SettingsRecord<L extends "user" | "tenant" | "deployment", S> {
  level: L;
  /** 0 when the level has no document yet, otherwise 1 for the first write and one more for every change. */
  version: number;
  schema: string;
  updated_at?: string;
  updated_by?: string;
  settings: S;
}

export type SettingsUserRecord = SettingsRecord<"user", UserSettings>;
export type SettingsTenantRecord = SettingsRecord<"tenant", TenantSettings>;
export type SettingsDeploymentRecord = SettingsRecord<"deployment", DeploymentSettings>;

/** What the three levels add up to for the caller (read only; never an input to the policy gateway). */
export type SettingsEffective = EffectiveSettings;

/**
 * The body of a settings write: the whole document and the version it replaces. A write whose `expected_version` is not the stored
 * one is `conflict`, with the stored version in `error.details.current_version`; read again, merge and write again. The SDK never
 * retries a write by itself.
 */
export interface SettingsUpdate<S> {
  /** The version that was read, or 0 to create the document. */
  expected_version: number;
  settings: S;
}

export interface SettingsProfile {
  id: string;
  /** The profile in force when no level states one. */
  default: boolean;
  /** The profile the caller's effective settings select. */
  selected: boolean;
}

export interface SettingsProfileList {
  profiles: SettingsProfile[];
}

export interface SettingsCapabilityState {
  id: string;
  risk_level: 0 | 1 | 2 | 3;
  /** False when any level switched the capability off. */
  enabled: boolean;
  /** The levels that switched it off, most general first. */
  disabled_by: Array<"deployment" | "tenant" | "user">;
  requires_confirmation: boolean;
  requires_step_up: boolean;
  /** True when `requires_confirmation` is true only because settings asked for it. */
  extra_confirmation: boolean;
  fields: string[];
}

export interface SettingsLevelAccess {
  read: boolean;
  write: boolean;
}

export interface SettingsCapabilityView {
  capabilities: SettingsCapabilityState[];
  /** What the token permits on the settings themselves. Informational: every operation checks the token again. */
  settings_access: { user: SettingsLevelAccess; tenant: SettingsLevelAccess; deployment: SettingsLevelAccess };
}

export interface SettingsAuditFinding {
  key: string;
  check_id: string;
  severity: "critical" | "warning" | "info";
  capability?: string;
  path?: string;
  message: string;
  remediation: string;
  status: "open" | "accepted" | "informational";
}

export interface SettingsAuditStatus {
  application_id: string;
  state: ApplicationStatus;
  package_version?: string;
  manifest_hash?: string;
  /** False when the installed package has never been audited. */
  audited: boolean;
  run_id?: string;
  outcome?: "blocked" | "needs_acceptance" | "clear";
  counts: { critical: number; warnings: number; info: number; unverified: number };
  findings: SettingsAuditFinding[];
}

export interface SettingsProviderRole {
  role: string;
  configured: boolean;
  status: ReadinessStatus;
  /** The stable degradation code when the status is not ok. */
  effect?: string;
}

export interface SettingsProviderHealth {
  /** The worst status of any role. */
  status: ReadinessStatus;
  roles: SettingsProviderRole[];
}
