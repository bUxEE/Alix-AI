import { useId } from "react";
import type { ReactElement } from "react";
import type { Citation } from "@alixai/sdk";
import { fill } from "../i18n/messages.js";
import { useAlixText } from "../i18n/context.js";

export interface AlixCitationListProps {
  citations: readonly Citation[];
  /**
   * The person opened a source. Only offered for a source that is `available`: the host resolves the opaque `open_ref`
   * under the user's own scope. The kit never opens it, and never hides a citation.
   */
  onOpenCitation?: (citation: Citation) => void;
  /** Show the "Sources" heading (default). */
  heading?: boolean;
}

const stalenessTone = (staleness: Citation["staleness"]): "warn" | undefined => (staleness === "stale" ? "warn" : undefined);
const availabilityTone = (availability: Citation["availability"]): "danger" | undefined => (availability === "available" ? undefined : "danger");

/** The sources that support an answer, each with its version, where in the source it sits and what it supports. */
export const AlixCitationList = ({ citations, onOpenCitation, heading = true }: AlixCitationListProps): ReactElement => {
  const text = useAlixText().citations;
  const headingId = useId();
  return (
    <section className="alix-citations" data-alixai="citations" {...(heading ? { "aria-labelledby": headingId } : { "aria-label": text.heading })}>
      {heading && (
        <h4 id={headingId} className="alix-citations__heading">
          {text.heading}
        </h4>
      )}
      <ol className="alix-citations__list">
        {citations.map((citation, index) => (
          <li key={`${citation.source_id}-${index}`} className="alix-citations__item" data-availability={citation.availability} data-staleness={citation.staleness}>
            <div className="alix-citations__head">
              {onOpenCitation !== undefined && citation.availability === "available" ? (
                <button
                  type="button"
                  className="alix-citations__open"
                  aria-label={fill(text.open, { name: citation.display_name })}
                  onClick={() => onOpenCitation(citation)}
                >
                  {citation.display_name}
                </button>
              ) : (
                <span className="alix-citations__name">{citation.display_name}</span>
              )}
              <span className="alix-citations__version" data-alixai="citation-version">
                {citation.version}
              </span>
              {citation.locator?.section !== undefined && <span className="alix-citations__locator">{citation.locator.section}</span>}
              {citation.locator?.page !== undefined && <span className="alix-citations__locator">{fill(text.page, { page: citation.locator.page })}</span>}
              {citation.availability !== "available" && (
                <span className="alix-citations__tag" data-tone={availabilityTone(citation.availability)}>
                  {text.availability[citation.availability]}
                </span>
              )}
              {citation.staleness !== "current" && (
                <span className="alix-citations__tag" {...(stalenessTone(citation.staleness) !== undefined && { "data-tone": stalenessTone(citation.staleness) })}>
                  {text.staleness[citation.staleness]}
                </span>
              )}
            </div>
            {citation.claims.length > 0 && (
              <ul className="alix-citations__claims">
                {citation.claims.map((claim, claimIndex) => (
                  <li key={claimIndex}>{claim}</li>
                ))}
              </ul>
            )}
          </li>
        ))}
      </ol>
    </section>
  );
};
