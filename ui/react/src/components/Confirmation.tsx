import { useEffect, useId, useRef, useState } from "react";
import type { ReactElement } from "react";
import { approvalPayload, formatCanonicalValue, parseConfirmation } from "@alixai/copilotkit-adapter";
import type { AlixAIInterrupt } from "@alixai/copilotkit-adapter";
import type { Effect } from "@alixai/sdk";
import { formatDateTime, useAlixIntl } from "../i18n/context.js";

// The confirmation sheet for the interrupt of a prepared action. What it shows is projected by the AlixAI core from the
// prepared action (the exact effects the application will carry out), never from the model's text. The sheet decides
// nothing: it reports the person's answer once, and the host (or `AlixConfirmationInterrupt`) turns it into the one
// payload the protocol knows, {"approved": true | false}. The confirmation secret is not here and never will be.

export interface AlixConfirmationSheetProps {
  /** The interrupt of the run, as CopilotKit's `useInterrupt` hands it over. */
  interrupt: AlixAIInterrupt | null | undefined;
  /** The person approved. Called at most once. */
  onApprove: () => void;
  /** The person declined. Called at most once. */
  onDecline: () => void;
  /** The sheet was dismissed without an answer (the unavailable state's only button). Defaults to `onDecline`: AlixAI treats a dismissal as a decline. */
  onDismiss?: () => void;
}

export const clampRisk = (risk: number): 0 | 1 | 2 | 3 => (risk >= 3 ? 3 : risk >= 2 ? 2 : risk >= 1 ? 1 : 0);

const OPERATIONS = ["create", "update", "delete", "send", "other"] as const;
type Operation = (typeof OPERATIONS)[number];
const operationOf = (effect: Effect): Operation => (OPERATIONS.includes(effect.operation as Operation) ? (effect.operation as Operation) : "other");

/** The risk level as a meter and its name (never colour alone). Shared by the sheet and the prepared-action panel. */
export const AlixRiskMeter = ({ risk }: { risk: number }): ReactElement => {
  const text = useAlixIntl().messages.confirmation;
  const level = clampRisk(risk);
  return (
    <span className="alix-sheet__risk" data-alixai="risk">
      <span className="alix-sheet__meter" aria-hidden="true">
        {[1, 2, 3].map((step) => (
          <i key={step} {...(step <= level && { "data-on": "" })} />
        ))}
      </span>
      {text.risk}: {text.riskLevels[level]}
    </span>
  );
};

/** The exact effects of a prepared action, each with what it changes. Shared by the sheet and the prepared-action panel. */
export const AlixEffectList = ({ effects }: { effects: readonly Effect[] }): ReactElement => {
  const text = useAlixIntl().messages.confirmation;
  return (
    <ul className="alix-sheet__effects" data-alixai="effects">
      {effects.map((effect, index) => (
        <li key={index} className="alix-sheet__effect">
          <div className="alix-sheet__effect-head">
            <span className="alix-sheet__op" data-op={operationOf(effect)}>
              {text.operations[operationOf(effect)]}
            </span>
            <span data-alixai="effect-description">{effect.description}</span>
            {effect.external && <span className="alix-sheet__external">{text.external}</span>}
          </div>
          {effect.changes.length > 0 && (
            <dl className="alix-sheet__changes">
              {effect.changes.map((change, changeIndex) => (
                <div key={changeIndex}>
                  <dt>{change.field}</dt>
                  <dd>{change.display ?? formatCanonicalValue(change.value)}</dd>
                </div>
              ))}
            </dl>
          )}
        </li>
      ))}
    </ul>
  );
};

export const AlixConfirmationSheet = ({ interrupt, onApprove, onDecline, onDismiss }: AlixConfirmationSheetProps): ReactElement | null => {
  const intl = useAlixIntl();
  const text = intl.messages.confirmation;
  const titleId = useId();
  const summaryId = useId();
  const [answered, setAnswered] = useState(false);
  const sheet = useRef<HTMLElement>(null);
  const status = useRef<HTMLParagraphElement>(null);
  const confirmation = parseConfirmation(interrupt);
  const shown = interrupt !== null && interrupt !== undefined;

  // The run is waiting for a decision: bring the person to it, once, so a screen reader reads what is being asked.
  useEffect(() => {
    if (shown) sheet.current?.focus({ preventScroll: false });
  }, [shown]);
  useEffect(() => {
    if (answered) status.current?.focus();
  }, [answered]);

  if (!shown) return null;
  const answer = (callback: () => void): void => {
    if (answered) return;
    setAnswered(true);
    callback();
  };

  if (confirmation === undefined) {
    return (
      <section ref={sheet} tabIndex={-1} className="alix-sheet" data-alixai="confirmation" data-state="unavailable" aria-labelledby={titleId}>
        <h3 id={titleId} className="alix-sheet__title">
          {text.title}
        </h3>
        <p className="alix-sheet__summary">{text.unavailable}</p>
        <div className="alix-sheet__actions">
          <button type="button" className="alix-sheet__button" data-variant="decline" disabled={answered} onClick={() => answer(onDismiss ?? onDecline)}>
            {text.decline}
          </button>
        </div>
        {answered && (
          <p ref={status} tabIndex={-1} role="status" className="alix-sheet__status">
            {text.answered}
          </p>
        )}
      </section>
    );
  }

  const risk = clampRisk(confirmation.riskLevel);
  const destructive = risk === 3 || confirmation.effects.some((effect) => effect.operation === "delete");
  return (
    <section
      ref={sheet}
      tabIndex={-1}
      className="alix-sheet"
      data-alixai="confirmation"
      data-risk={risk}
      aria-labelledby={titleId}
      aria-describedby={summaryId}
    >
      <div className="alix-sheet__head">
        <h3 id={titleId} className="alix-sheet__title">
          {text.title}
        </h3>
        <AlixRiskMeter risk={risk} />
      </div>
      <p id={summaryId} className="alix-sheet__summary" data-alixai="summary">
        {confirmation.summary}
      </p>
      <AlixEffectList effects={confirmation.effects} />
      <div className="alix-sheet__foot">
        <p className="alix-sheet__expires" data-alixai="expires">
          {text.expires} <time dateTime={confirmation.expiresAt}>{formatDateTime(intl, confirmation.expiresAt)}</time>
        </p>
        {answered ? (
          <p ref={status} tabIndex={-1} role="status" className="alix-sheet__status">
            {text.answered}
          </p>
        ) : (
          <div className="alix-sheet__actions">
            <button type="button" className="alix-sheet__button" data-variant="decline" onClick={() => answer(onDecline)}>
              {text.decline}
            </button>
            <button
              type="button"
              className="alix-sheet__button"
              data-variant="approve"
              {...(destructive && { "data-tone": "danger" })}
              onClick={() => answer(onApprove)}
            >
              {text.approve}
            </button>
          </div>
        )}
      </div>
    </section>
  );
};

/** The render props of CopilotKit's `useInterrupt`: the interrupt, and the two ways to answer it. */
export interface AlixConfirmationInterruptProps {
  interrupt: AlixAIInterrupt | null | undefined;
  resolve: (payload?: unknown, interruptId?: string) => unknown;
  cancel?: (interruptId?: string) => unknown;
}

/**
 * The sheet wired to the interrupt: Approve resolves it with exactly {"approved": true}, Decline with {"approved": false},
 * and a dismissal is `cancel` (or a decline when there is none). CopilotKit then resumes the run.
 */
export const AlixConfirmationInterrupt = ({ interrupt, resolve, cancel }: AlixConfirmationInterruptProps): ReactElement | null => {
  const id = interrupt?.id;
  return (
    <AlixConfirmationSheet
      key={id}
      interrupt={interrupt}
      onApprove={() => void resolve(approvalPayload(true), id)}
      onDecline={() => void resolve(approvalPayload(false), id)}
      onDismiss={() => void (cancel !== undefined ? cancel(id) : resolve(approvalPayload(false), id))}
    />
  );
};
