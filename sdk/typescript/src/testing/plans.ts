import type { ErrorBody, ErrorCode, Effect, Fact, RecordRef, RiskLevel } from "./types.js";

// What a host-application test says about one turn of the fake AlixAI server. A plan describes the
// outcome in terms of the API (a reply, a question, a prepared action, a failure); the fake turns it
// into the events, headers and state a real service would produce, so the test cannot forge an
// invalid stream by hand.

export interface ChoicePlan {
  choice_id: string;
  label: string;
  /** The canonical value the choice stands for. Defaults to an identifier value with the choice id. */
  value?: { kind: "identifier"; value: string } | { kind: "text"; value: string };
}

/** What committing a prepared action does, when the user confirms it. */
export interface CommitPlan {
  /** The receipt status. Default "committed". */
  status?: "committed" | "queued" | "rejected" | "partial" | "uncertain";
  /** The typed reason for a rejected, partial or uncertain commit. */
  error?: { code: ErrorCode; message: string };
  facts?: Fact[];
  records?: RecordRef[];
}

export interface PrepareInput {
  capability_id: string;
  /** Canonical summary of the action (what the user confirms). */
  summary: string;
  effects?: Effect[];
  /** Effective risk level. 0 and 1 commit at once, 2 needs explicit confirmation (default), 3 needs step-up. */
  risk_level?: RiskLevel;
  /** How long the preparation stays valid, in milliseconds. Default: the fake's preparationTtlMs. */
  validForMs?: number;
  commit?: CommitPlan;
  /** The assistant's text for the turn. */
  text?: string;
}

export type TurnPlan =
  | { kind: "reply"; text: string }
  | { kind: "clarify"; field: string; question: string; choices: ChoicePlan[] }
  | { kind: "prepare"; input: PrepareInput }
  | { kind: "fail"; error: { code: ErrorCode; message: string }; text?: string }
  | { kind: "http_error"; status: number; error: ErrorBody };

/** The assistant answers with text. */
export const reply = (text: string): TurnPlan => ({ kind: "reply", text });

/** The assistant asks the user to pick one of several choices (the pending state becomes a clarification). */
export const askChoice = (field: string, question: string, choices: ChoicePlan[]): TurnPlan => {
  // The directive schema requires at least two choices to disambiguate between.
  if (choices.length < 2) throw new RangeError("askChoice needs at least two choices");
  return { kind: "clarify", field, question, choices };
};

/**
 * The assistant prepares an action. At risk level 2 (the default) the turn ends awaiting
 * confirmation and the confirmation secret comes in the response header (stream) or the
 * `confirmation` member (JSON); at level 3 the directive is `request_step_up` and there is no
 * secret; at level 0 or 1 the action is committed in the same turn and a receipt event is sent.
 */
export const prepareAction = (input: PrepareInput): TurnPlan => ({ kind: "prepare", input });

/** The turn ends in a typed failure: an `error` event, then `done` with finish_reason `failed` (still HTTP 200). */
export const failTurn = (code: ErrorCode, message: string, text?: string): TurnPlan => ({
  kind: "fail",
  error: { code, message },
  ...(text !== undefined && { text }),
});

/** The turn could not be run at all: an error response with this status, never a stream. */
export const rejectTurn = (status: number, error: ErrorBody): TurnPlan => ({ kind: "http_error", status, error });
