import { useState } from "react";
import type { ReactElement } from "react";
import { approvalPayload, formatCanonicalValue, parseConfirmation } from "../activities.js";
import type { AlixAIInterrupt } from "../activities.js";

// The confirmation card for the interrupt of a prepared action (the renderAndWaitForResponse style: it renders the
// request and waits for the person). What it shows is projected by the AlixAI core from the prepared action, the exact
// effects the application will carry out, never the model's text. Approving answers the interrupt with exactly
// {"approved": true}; the confirmation secret is not here and never will be: it stays with AlixAIAgent in the host's
// backend. Everything is rendered as text, so nothing in a payload can become markup.

export interface AlixAIConfirmationLabels {
  title: string;
  approve: string;
  decline: string;
  risk: string;
  expires: string;
  unavailable: string;
}

export const DEFAULT_CONFIRMATION_LABELS: AlixAIConfirmationLabels = {
  title: "Please confirm",
  approve: "Approve",
  decline: "Decline",
  risk: "Risk level",
  expires: "Valid until",
  unavailable: "This confirmation cannot be shown. You can decline it.",
};

export interface AlixAIConfirmationCardProps {
  /** The interrupt of the run, as `useInterrupt` renders it (`interrupt` of its render props). */
  interrupt: AlixAIInterrupt | null | undefined;
  /** `resolve` of the interrupt: called with {"approved": true} or {"approved": false}. */
  resolve: (payload?: unknown, interruptId?: string) => unknown;
  /** `cancel` of the interrupt: the interrupt is dismissed, which AlixAI treats as a decline. */
  cancel?: (interruptId?: string) => unknown;
  labels?: Partial<AlixAIConfirmationLabels>;
}

export const AlixAIConfirmationCard = ({ interrupt, resolve, cancel, labels }: AlixAIConfirmationCardProps): ReactElement | null => {
  const text = { ...DEFAULT_CONFIRMATION_LABELS, ...labels };
  const [answered, setAnswered] = useState(false);
  if (interrupt === null || interrupt === undefined) return null;
  const confirmation = parseConfirmation(interrupt);
  const answer = (approved: boolean): void => {
    if (answered) return;
    setAnswered(true);
    if (approved) void resolve(approvalPayload(true), interrupt.id);
    else void resolve(approvalPayload(false), interrupt.id);
  };
  const dismiss = (): void => {
    if (answered) return;
    setAnswered(true);
    if (cancel !== undefined) void cancel(interrupt.id);
    else void resolve(approvalPayload(false), interrupt.id);
  };

  if (confirmation === undefined) {
    return (
      <section className="alixai-confirmation" data-alixai="confirmation" data-state="unavailable" aria-label={text.title}>
        <p>{text.unavailable}</p>
        <button type="button" disabled={answered} onClick={dismiss}>
          {text.decline}
        </button>
      </section>
    );
  }
  return (
    <section className="alixai-confirmation" data-alixai="confirmation" data-risk={confirmation.riskLevel} aria-label={text.title}>
      <h3>{text.title}</h3>
      <p data-alixai="summary">{confirmation.summary}</p>
      <ul data-alixai="effects">
        {confirmation.effects.map((effect, index) => (
          <li key={index}>
            <span data-alixai="effect-description">{effect.description}</span>
            <dl>
              {effect.changes.map((change, changeIndex) => (
                <div key={changeIndex}>
                  <dt>{change.field}</dt>
                  <dd>{formatCanonicalValue(change.value)}</dd>
                </div>
              ))}
            </dl>
          </li>
        ))}
      </ul>
      <p data-alixai="risk">
        {text.risk}: {confirmation.riskLevel}
      </p>
      <p data-alixai="expires">
        {text.expires}: {confirmation.expiresAt}
      </p>
      <div>
        <button type="button" data-alixai="approve" disabled={answered} onClick={() => answer(true)}>
          {text.approve}
        </button>
        <button type="button" data-alixai="decline" disabled={answered} onClick={() => answer(false)}>
          {text.decline}
        </button>
      </div>
    </section>
  );
};
