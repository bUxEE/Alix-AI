import { useState } from "react";
import type { ReactElement } from "react";
import type { Citation } from "@alixai/sdk";
import { formatCanonicalValue } from "../activities.js";
import type { AlixAIAttachment, AlixAIBlock, AlixAIProgress } from "../activities.js";

// Plain, unstyled renderers for the validated blocks and activities AlixAI sends (docs/integration/agui.md section 2).
// Every value is rendered as text. A host that wants its own look passes its own components to the activity renderers
// (see hooks.tsx) or styles the `data-alixai` hooks. Nothing here executes anything: a diagram is shown as its source and
// a chart is data, never code.

export const AlixAITable = ({ block }: { block: AlixAIBlock<"table"> }): ReactElement => (
  <table className="alixai-table" data-alixai="table">
    {block.caption !== undefined && <caption>{block.caption}</caption>}
    <thead>
      <tr>
        {block.columns.map((column) => (
          <th key={column.key} scope="col">
            {column.label}
          </th>
        ))}
      </tr>
    </thead>
    <tbody>
      {block.rows.map((row, rowIndex) => (
        <tr key={rowIndex}>
          {row.map((cell, cellIndex) => (
            <td key={cellIndex}>{cell === null ? "" : String(cell)}</td>
          ))}
        </tr>
      ))}
    </tbody>
  </table>
);

/**
 * A chart block: its description is the accessible name, the data is a table, and a bar chart also draws its bars. The
 * block is declarative data; there is no script and no URL in it to follow.
 */
export const AlixAIChart = ({ block }: { block: AlixAIBlock<"chart"> }): ReactElement => {
  const x = block.encoding.x?.field;
  const y = block.encoding.y?.field;
  const values = y === undefined ? [] : block.data.map((record) => Number(record[y] ?? 0)).filter((value) => Number.isFinite(value));
  const max = Math.max(1, ...values);
  return (
    <figure className="alixai-chart" data-alixai="chart" data-mark={block.mark} aria-label={block.title ?? block.description}>
      {block.title !== undefined && <figcaption>{block.title}</figcaption>}
      <p>{block.description}</p>
      {block.mark === "bar" && x !== undefined && y !== undefined && (
        <svg role="img" aria-label={block.description} viewBox={`0 0 ${block.data.length * 24} 60`} width="100%" height="60">
          {block.data.map((record, index) => {
            const height = (Number(record[y] ?? 0) / max) * 50;
            return <rect key={index} x={index * 24 + 4} y={55 - height} width={16} height={height} fill="currentColor" />;
          })}
        </svg>
      )}
      <table>
        <thead>
          <tr>
            {Object.keys(block.data[0] ?? {}).map((key) => (
              <th key={key} scope="col">
                {key}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {block.data.map((record, index) => (
            <tr key={index}>
              {Object.values(record).map((value, cell) => (
                <td key={cell}>{value === null ? "" : String(value)}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </figure>
  );
};

export const AlixAIDiagram = ({ block }: { block: AlixAIBlock<"diagram"> }): ReactElement => (
  <pre className="alixai-diagram" data-alixai="diagram" data-language={block.language}>
    {block.source}
  </pre>
);

export const AlixAIActionCard = ({ block }: { block: AlixAIBlock<"action_card"> }): ReactElement => (
  <section className="alixai-action-card" data-alixai="action-card">
    <h3>{block.title}</h3>
    <dl>
      {block.facts.map((fact, index) => (
        <div key={index}>
          <dt>{fact.field}</dt>
          <dd>{fact.display ?? formatCanonicalValue(fact.value)}</dd>
        </div>
      ))}
    </dl>
  </section>
);

export interface AlixAIChoicesProps {
  block: AlixAIBlock<"clarification_choices">;
  /** Called with the chosen choice id: the host runs the agent with `forwardedProps.alixai.choiceId` (no model reads it). */
  onChoose: (choiceId: string) => void;
  /** Called with free text, when the block allows it: sent as an ordinary user message. */
  onFreeText?: (text: string) => void;
  sendLabel?: string;
}

/** The form of a clarification: the question, its choices as buttons, and optional free text. A form is data, not executable UI. */
export const AlixAIChoices = ({ block, onChoose, onFreeText, sendLabel = "Send" }: AlixAIChoicesProps): ReactElement => {
  const [text, setText] = useState("");
  return (
    <form
      className="alixai-choices"
      data-alixai="choices"
      onSubmit={(event) => {
        event.preventDefault();
        if (onFreeText !== undefined && text.trim() !== "") onFreeText(text.trim());
      }}
    >
      {block.prompt !== undefined && <p>{block.prompt}</p>}
      <div role="group" aria-label={block.field}>
        {block.choices.map((choice) => (
          <button key={choice.choice_id} type="button" title={choice.description} onClick={() => onChoose(choice.choice_id)}>
            {choice.label}
          </button>
        ))}
      </div>
      {block.allow_free_text && onFreeText !== undefined && (
        <label>
          <span>{block.field}</span>
          <input type="text" value={text} onChange={(event) => setText(event.target.value)} />
          <button type="submit">{sendLabel}</button>
        </label>
      )}
    </form>
  );
};

/** Citations with the opaque, scope-checked reference of each source. The reference is shown, never opened by this component. */
export const AlixAICitationList = ({
  citations,
  onOpen,
}: {
  citations: readonly Citation[];
  onOpen?: (citation: Citation) => void;
}): ReactElement => (
  <ol className="alixai-citations" data-alixai="citations">
    {citations.map((citation, index) => (
      <li key={`${citation.source_id}-${index}`} data-availability={citation.availability} data-staleness={citation.staleness}>
        {onOpen !== undefined && citation.availability === "available" ? (
          <button type="button" onClick={() => onOpen(citation)}>
            {citation.display_name}
          </button>
        ) : (
          <span>{citation.display_name}</span>
        )}
        <span data-alixai="citation-version"> {citation.version}</span>
        {citation.locator?.section !== undefined && <span data-alixai="citation-locator"> {citation.locator.section}</span>}
        {citation.locator?.page !== undefined && <span data-alixai="citation-locator"> p. {citation.locator.page}</span>}
        <ul>
          {citation.claims.map((claim, claimIndex) => (
            <li key={claimIndex}>{claim}</li>
          ))}
        </ul>
      </li>
    ))}
  </ol>
);

export interface AlixAIAttachmentPreviewProps {
  files: readonly AlixAIAttachment[];
  /**
   * Renders the preview of a previewable file, for example an `<img>` whose source your backend serves from AlixAI's files
   * service under the user's own scope. Default: the name only. The adapter never builds a file URL itself.
   */
  renderPreview?: (file: AlixAIAttachment) => ReactElement | null;
}

const formatSize = (bytes: number): string => {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 102.4) / 10} KB`;
  return `${Math.round(bytes / 104857.6) / 10} MB`;
};

export const AlixAIAttachmentPreview = ({ files, renderPreview }: AlixAIAttachmentPreviewProps): ReactElement => (
  <ul className="alixai-attachments" data-alixai="attachments">
    {files.map((file) => (
      <li key={file.fileId} data-previewable={file.previewable}>
        {file.previewable && renderPreview?.(file)}
        <span>{file.name}</span>
        <span data-alixai="attachment-meta">
          {" "}
          {file.mimeType}, {formatSize(file.sizeBytes)}
        </span>
      </li>
    ))}
  </ul>
);

export const AlixAIProgressLine = ({ progress }: { progress: AlixAIProgress }): ReactElement => (
  <p className="alixai-progress" data-alixai="progress" data-state={progress.state}>
    {progress.stage}
    {progress.percent !== undefined && ` ${Math.round(progress.percent)}%`}
  </p>
);
