import { useId } from "react";
import type { ReactElement, ReactNode } from "react";
import { formatCanonicalValue } from "@alixai/copilotkit-adapter";
import type { ActionReceipt } from "@alixai/sdk";
import { fill } from "../i18n/messages.js";
import { formatDateTime, useAlixIntl } from "../i18n/context.js";

// The durable record of what happened to a prepared action: the application's own deterministic facts. The model's wording never
// decides the outcome shown here; the status, the facts and the errors come from the receipt. It arrives as the result of the
// confirmation's tool call (a resumed run) or as an `alixai.receipt` activity (a queued or reported outcome).

const STATUSES = ["committed", "rejected", "queued", "partial", "uncertain"] as const;
const FOLLOW_UP_STATES = ["queued", "running", "succeeded", "failed", "unknown"] as const;

const isRecord = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

/**
 * Reads a receipt from a stream (the activity content, or the parsed result of the confirmation's tool call). Anything that is
 * not a receipt is undefined, so nothing is drawn on trust.
 */
export const parseReceipt = (content: unknown): ActionReceipt | undefined => {
  if (!isRecord(content)) return undefined;
  const { receipt_id: receiptId, preparation_id: preparationId, status, facts, errors } = content;
  if (typeof receiptId !== "string" || typeof preparationId !== "string") return undefined;
  if (typeof status !== "string" || !(STATUSES as readonly string[]).includes(status)) return undefined;
  if (!Array.isArray(facts) || !facts.every((fact) => isRecord(fact) && typeof fact["field"] === "string")) return undefined;
  if (errors !== undefined && (!Array.isArray(errors) || !errors.every(isRecord))) return undefined;
  return { ...(content as unknown as ActionReceipt), errors: (errors ?? []) as unknown as ActionReceipt["errors"] };
};

type Tone = "ok" | "accent" | "warn" | "danger";
const toneOf = (status: ActionReceipt["status"]): Tone =>
  status === "committed" ? "ok" : status === "queued" ? "accent" : status === "rejected" ? "danger" : "warn";

export interface AlixReceiptCardProps {
  receipt: ActionReceipt;
  /** Replaces the "Result" heading. */
  title?: ReactNode;
  /** Anything the host adds under the facts, for example a link to the record in its own interface. */
  children?: ReactNode;
}

/** The outcome of a confirmed (or queued) action: its status, the facts the application reported, problems and follow-up. */
export const AlixReceiptCard = ({ receipt, title, children }: AlixReceiptCardProps): ReactElement => {
  const intl = useAlixIntl();
  const text = intl.messages.receipt;
  const titleId = useId();
  const followUp = receipt.follow_up;
  const followState = followUp !== undefined && (FOLLOW_UP_STATES as readonly string[]).includes(followUp.state) ? followUp.state : "unknown";
  return (
    <section className="alix-receipt" data-alixai="receipt" data-status={receipt.status} aria-labelledby={titleId}>
      <header className="alix-receipt__head">
        <h3 id={titleId} className="alix-receipt__title">
          {title ?? text.title}
        </h3>
        <span className="alix-receipt__status" data-tone={toneOf(receipt.status)}>
          {text.status[receipt.status]}
        </span>
      </header>
      {receipt.facts.length > 0 && (
        <dl className="alix-receipt__facts">
          {receipt.facts.map((fact, index) => (
            <div key={`${fact.field}-${index}`}>
              <dt>{fact.field}</dt>
              <dd>{fact.display ?? formatCanonicalValue(fact.value)}</dd>
            </div>
          ))}
        </dl>
      )}
      {receipt.errors.length > 0 && (
        <ul className="alix-receipt__errors" aria-label={text.errors}>
          {receipt.errors.map((issue, index) => (
            <li key={index}>{issue.message}</li>
          ))}
        </ul>
      )}
      {followUp !== undefined && <p className="alix-receipt__meta">{fill(text.followUp, { state: text.followUpStates[followState as keyof typeof text.followUpStates] })}</p>}
      {children}
      <p className="alix-receipt__meta" data-alixai="receipt-issued">
        <time dateTime={receipt.issued_at}>{fill(text.issued, { time: formatDateTime(intl, receipt.issued_at) })}</time>
      </p>
    </section>
  );
};

export interface AlixConfirmationOutcomeProps {
  /** The `status` of a confirmation result that is not a receipt (`rejected`, `failed`, ...). */
  status: string;
}

/** One line for a confirmation that ended without a receipt: declined, or not carried out. */
export const AlixConfirmationOutcome = ({ status }: AlixConfirmationOutcomeProps): ReactElement => {
  const text = useAlixIntl().messages.receipt.outcomes;
  const line = status === "rejected" ? text.rejected : status === "failed" ? text.failed : fill(text.other, { status });
  return (
    <p className="alix-outcome" data-alixai="confirmation-outcome" data-status={status} role="status">
      {line}
    </p>
  );
};
