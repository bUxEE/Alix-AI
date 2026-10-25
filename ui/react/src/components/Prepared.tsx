import { useId } from "react";
import type { ReactElement, ReactNode } from "react";
import type { AlixAIBlock } from "@alixai/copilotkit-adapter";
import type { Effect } from "@alixai/sdk";
import { fill } from "../i18n/messages.js";
import { formatDateTime, useAlixIntl } from "../i18n/context.js";
import { AlixEffectList, AlixRiskMeter, clampRisk } from "./Confirmation.js";

// A prepared action, as the transcript keeps it. The `confirmation_panel` block is the core's projection of the PreparedAction
// (summary, exact effects, risk, expiry, the confirmation it needs); it decides nothing and carries no secret. The answer itself
// is given elsewhere: an explicit confirmation in the confirmation sheet (the interrupt), a step-up in the application, which the
// `alixai_continue_in_app` call hands the person to. The panel stays in the conversation after the sheet is gone, so the record
// of what was prepared does not disappear with the question.

export interface AlixConfirmationPanelProps {
  block: AlixAIBlock<"confirmation_panel">;
  /** Replaces the line under the effects (by default: what happens next, by confirmation mode). */
  note?: ReactNode;
  /** Anything the host adds at the foot, for example its own "Continue in the application" control. */
  children?: ReactNode;
}

const modeOf = (block: AlixAIBlock<"confirmation_panel">): "explicit" | "step_up" => (block.confirmation?.mode === "step_up" ? "step_up" : "explicit");

/** The prepared action: summary, risk, the exact effects (folded) and expiry, read only. */
export const AlixConfirmationPanel = ({ block, note, children }: AlixConfirmationPanelProps): ReactElement => {
  const intl = useAlixIntl();
  const text = intl.messages.prepared;
  const titleId = useId();
  const mode = modeOf(block);
  const effects = (block.effects ?? []) as readonly Effect[];
  const risk = clampRisk(typeof block.risk_level === "number" ? block.risk_level : 0);
  return (
    <section className="alix-prepared" data-alixai="confirmation-panel" data-mode={mode} data-risk={risk} aria-labelledby={titleId}>
      <header className="alix-prepared__head">
        <h3 id={titleId} className="alix-prepared__title">
          {mode === "step_up" ? text.stepUpTitle : text.title}
        </h3>
        <AlixRiskMeter risk={risk} />
      </header>
      <p className="alix-prepared__summary">{block.summary}</p>
      {effects.length > 0 && (
        <details className="alix-prepared__details">
          <summary className="alix-prepared__toggle">{fill(text.changes, { count: effects.length })}</summary>
          <AlixEffectList effects={effects} />
        </details>
      )}
      <p className="alix-prepared__note">
        {note ?? (mode === "step_up" ? text.stepUpNote : text.explicitNote)}
        {typeof block.expires_at === "string" && (
          <>
            {" "}
            {intl.messages.confirmation.expires} <time dateTime={block.expires_at}>{formatDateTime(intl, block.expires_at)}</time>
          </>
        )}
      </p>
      {children}
    </section>
  );
};

/** What `alixai_continue_in_app` hands over: the prepared action the application has to complete. */
export interface AlixContinueInAppRequest {
  readonly preparationId?: string;
  readonly capabilityId?: string;
  readonly summary?: string;
}

/** Reads the arguments of an `alixai_continue_in_app` call. Only the identifiers and the summary are kept. */
export const parseContinueInApp = (args: unknown): AlixContinueInAppRequest => {
  if (typeof args !== "object" || args === null || Array.isArray(args)) return {};
  const record = args as Record<string, unknown>;
  const pick = (key: string): string | undefined => (typeof record[key] === "string" ? (record[key] as string) : undefined);
  const preparationId = pick("preparationId");
  const capabilityId = pick("capabilityId");
  const summary = pick("summary");
  return { ...(preparationId !== undefined && { preparationId }), ...(capabilityId !== undefined && { capabilityId }), ...(summary !== undefined && { summary }) };
};

export interface AlixContinueInAppProps {
  request: AlixContinueInAppRequest;
  /** The person chose to continue. The host opens its own authenticated interface; without it the card only explains. */
  onContinue?: (request: AlixContinueInAppRequest) => void;
  /** Replaces the explanation. */
  children?: ReactNode;
}

/** A step-up action: the chat cannot complete it, the application can. The button is the host's way there. */
export const AlixContinueInApp = ({ request, onContinue, children }: AlixContinueInAppProps): ReactElement => {
  const text = useAlixIntl().messages.continueInApp;
  const titleId = useId();
  return (
    <section className="alix-continue" data-alixai="continue-in-app" aria-labelledby={titleId}>
      <div className="alix-continue__body">
        <h3 id={titleId} className="alix-continue__title">
          {text.title}
        </h3>
        <p className="alix-continue__text">{children ?? text.body}</p>
      </div>
      {onContinue !== undefined && (
        <button type="button" className="alix-continue__button" onClick={() => onContinue(request)}>
          {text.cta}
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
            <path d="M14 4h6v6" />
            <path d="M20 4l-9 9" />
            <path d="M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5" />
          </svg>
        </button>
      )}
    </section>
  );
};
