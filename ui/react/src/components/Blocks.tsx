import { useId, useState } from "react";
import type { CSSProperties, ReactElement } from "react";
import { formatCanonicalValue } from "@alixai/copilotkit-adapter";
import type { AlixAIBlock, AlixAIProgress } from "@alixai/copilotkit-adapter";
import { useAlixExports } from "../exports-context.js";
import { formatNumber, useAlixIntl, useAlixText } from "../i18n/context.js";
import { AlixExportMenu } from "./ExportMenu.js";

// The validated blocks AlixAI sends, drawn with the kit's tokens. Every value is rendered as text, so nothing in a payload
// can become markup; a diagram is shown as its source and a chart is data, never code. Each block is data in, events out.

const BlockExport = ({ blockId, turnId }: { blockId: string; turnId: string }): ReactElement | null => {
  const exports = useAlixExports();
  const text = useAlixText().export;
  if (!exports.enabled || exports.exporter === undefined || exports.sessionId === "") return null;
  return (
    <div className="alix-block-export">
      <AlixExportMenu
        exporter={exports.exporter}
        {...(exports.download !== undefined && { download: exports.download })}
        label={text.block}
        formats={["csv", "xlsx"]}
        params={{ sessionId: exports.sessionId, turnId, blockIds: [blockId] }}
      />
    </div>
  );
};

// ---- table ----

export interface AlixTableBlockProps {
  block: AlixAIBlock<"table">;
  /** Activity / message id used as the block id for host exports. */
  blockId?: string;
  /** Turn id for host exports (defaults to blockId). */
  turnId?: string;
}

export const AlixTableBlock = ({ block, blockId, turnId }: AlixTableBlockProps): ReactElement => {
  const intl = useAlixIntl();
  const text = intl.messages.table;
  const captionId = useId();
  const exportId = blockId ?? "table";
  const cell = (value: string | number | boolean | null, kind: string | undefined): string => {
    if (value === null) return "";
    if (typeof value === "boolean") return value ? text.yes : text.no;
    if (typeof value === "number" && kind === "number") return formatNumber(intl, value);
    return String(value);
  };
  return (
    <div className="alix-block-with-export">
      <BlockExport blockId={exportId} turnId={turnId ?? exportId} />
      <figure className="alix-table" data-alixai="table">
        {block.caption !== undefined && (
          <figcaption id={captionId} className="alix-table__caption">
            {block.caption}
          </figcaption>
        )}
        <div
          className="alix-table__scroll"
          role="region"
          tabIndex={0}
          {...(block.caption !== undefined ? { "aria-labelledby": captionId } : { "aria-label": block.columns.map((column) => column.label).join(", ") })}
        >
          <table>
            <thead>
              <tr>
                {block.columns.map((column) => (
                  <th key={column.key} scope="col" {...(column.kind !== undefined && { "data-kind": column.kind })}>
                    {column.label}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {block.rows.length === 0 && (
                <tr>
                  <td colSpan={Math.max(1, block.columns.length)}>{text.empty}</td>
                </tr>
              )}
              {block.rows.map((row, rowIndex) => (
                <tr key={rowIndex}>
                  {row.map((value, cellIndex) => {
                    const kind = block.columns[cellIndex]?.kind;
                    return (
                      <td key={cellIndex} {...(kind !== undefined && { "data-kind": kind })}>
                        {cell(value, kind)}
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </figure>
    </div>
  );
};

// ---- chart ----

export interface AlixChartBlockProps {
  block: AlixAIBlock<"chart">;
  blockId?: string;
  turnId?: string;
}

const W = 320;
const H = 160;
const PAD = { left: 36, right: 8, top: 10, bottom: 26 } as const;

const seriesColour = (index: number): string => `var(--alixai-chart-${(index % 6) + 1})`;
const asNumber = (value: unknown): number | undefined => {
  const n = typeof value === "number" ? value : typeof value === "string" ? Number(value) : Number.NaN;
  return Number.isFinite(n) ? n : undefined;
};
const label = (value: unknown): string => (value === null || value === undefined ? "" : String(value));

/** Slices of a circle as SVG paths (a full circle is two half arcs: one arc cannot start and end on the same point). */
const arcPath = (cx: number, cy: number, r: number, from: number, to: number): string => {
  const point = (angle: number): string => `${(cx + r * Math.sin(angle)).toFixed(2)} ${(cy - r * Math.cos(angle)).toFixed(2)}`;
  if (to - from >= Math.PI * 2 - 1e-6) return `M ${point(0)} A ${r} ${r} 0 1 1 ${point(Math.PI)} A ${r} ${r} 0 1 1 ${point(0)} Z`;
  return `M ${cx} ${cy} L ${point(from)} A ${r} ${r} 0 ${to - from > Math.PI ? 1 : 0} 1 ${point(to)} Z`;
};

/**
 * A chart block: bar, line, area, point or pie, drawn from the declarative data with the theme's chart colours. The
 * description is the accessible name of the drawing and the same data is always available as a table.
 */
export const AlixChartBlock = ({ block, blockId, turnId }: AlixChartBlockProps): ReactElement => {
  const intl = useAlixIntl();
  const text = intl.messages.chart;
  const [open, setOpen] = useState(false);
  const dataId = useId();
  const exportId = blockId ?? "chart";
  const { x, y, color, theta } = block.encoding;
  const keys = Array.from(new Set(block.data.flatMap((record) => Object.keys(record))));

  let drawing: ReactElement | null = null;
  const legend: { name: string; index: number }[] = [];

  if (block.mark === "pie") {
    const labelField = color?.field ?? x?.field;
    const slices = theta === undefined ? [] : block.data.map((record, index) => ({ name: labelField === undefined ? String(index + 1) : label(record[labelField]), value: asNumber(record[theta.field]) ?? 0 })).filter((slice) => slice.value > 0);
    const total = slices.reduce((sum, slice) => sum + slice.value, 0);
    let angle = 0;
    drawing = (
      <svg className="alix-chart__svg" role="img" aria-label={block.description} viewBox={`0 0 ${W} ${H}`}>
        {total > 0 &&
          slices.map((slice, index) => {
            const from = angle;
            angle += (slice.value / total) * Math.PI * 2;
            legend.push({ name: slice.name, index });
            return <path key={index} className="alix-chart__mark" style={{ "--series": seriesColour(index) } as CSSProperties} d={arcPath(W / 2, H / 2, H / 2 - 6, from, angle)} />;
          })}
      </svg>
    );
  } else if (x !== undefined && y !== undefined) {
    const categories = Array.from(new Set(block.data.map((record) => label(record[x.field]))));
    const seriesNames = color === undefined ? [""] : Array.from(new Set(block.data.map((record) => label(record[color.field]))));
    seriesNames.forEach((name, index) => {
      if (color !== undefined) legend.push({ name, index });
    });
    const points = block.data.flatMap((record) => {
      const value = asNumber(record[y.field]);
      if (value === undefined) return [];
      return [{ category: categories.indexOf(label(record[x.field])), series: color === undefined ? 0 : seriesNames.indexOf(label(record[color.field])), value }];
    });
    const top = Math.max(0, ...points.map((point) => point.value));
    const bottom = Math.min(0, ...points.map((point) => point.value));
    const span = top - bottom || 1;
    const plotW = W - PAD.left - PAD.right;
    const plotH = H - PAD.top - PAD.bottom;
    const band = plotW / Math.max(1, categories.length);
    const yOf = (value: number): number => PAD.top + ((top - value) / span) * plotH;
    const xOf = (category: number): number => PAD.left + band * (category + 0.5);
    const zero = yOf(0);
    const every = Math.ceil(categories.length / 6);
    drawing = (
      <svg className="alix-chart__svg" role="img" aria-label={block.description} viewBox={`0 0 ${W} ${H}`}>
        <line className="alix-chart__axis" x1={PAD.left} x2={W - PAD.right} y1={zero} y2={zero} />
        <line className="alix-chart__axis" x1={PAD.left} x2={PAD.left} y1={PAD.top} y2={PAD.top + plotH} />
        <text className="alix-chart__label" x={PAD.left - 4} y={PAD.top + 8} textAnchor="end">
          {formatNumber(intl, top)}
        </text>
        {bottom < 0 && (
          <text className="alix-chart__label" x={PAD.left - 4} y={PAD.top + plotH} textAnchor="end">
            {formatNumber(intl, bottom)}
          </text>
        )}
        {categories.map((name, index) =>
          index % every === 0 ? (
            <text key={index} className="alix-chart__label" x={xOf(index)} y={H - 8} textAnchor="middle">
              {name.length > 10 ? `${name.slice(0, 9)}...` : name}
            </text>
          ) : null,
        )}
        {block.mark === "bar" &&
          points.map((point, index) => {
            const groupWidth = band * 0.7;
            const width = groupWidth / seriesNames.length;
            const left = xOf(point.category) - groupWidth / 2 + point.series * width;
            const topEdge = Math.min(yOf(point.value), zero);
            return (
              <rect
                key={index}
                className="alix-chart__mark"
                style={{ "--series": seriesColour(point.series) } as CSSProperties}
                x={left + 1}
                y={topEdge}
                width={Math.max(1, width - 2)}
                height={Math.max(1, Math.abs(yOf(point.value) - zero))}
                rx={2}
              />
            );
          })}
        {(block.mark === "line" || block.mark === "area") &&
          seriesNames.map((_, series) => {
            const own = points.filter((point) => point.series === series).sort((a, b) => a.category - b.category);
            if (own.length === 0) return null;
            const path = own.map((point, index) => `${index === 0 ? "M" : "L"} ${xOf(point.category).toFixed(2)} ${yOf(point.value).toFixed(2)}`).join(" ");
            const style = { "--series": seriesColour(series) } as CSSProperties;
            const first = own[0];
            const last = own[own.length - 1];
            return (
              <g key={series} style={style}>
                {block.mark === "area" && first !== undefined && last !== undefined && (
                  <path className="alix-chart__area" d={`${path} L ${xOf(last.category).toFixed(2)} ${zero} L ${xOf(first.category).toFixed(2)} ${zero} Z`} />
                )}
                <path className="alix-chart__line" d={path} />
              </g>
            );
          })}
        {(block.mark === "point" || block.mark === "line" || block.mark === "area") &&
          points.map((point, index) => (
            <circle key={index} className="alix-chart__mark" style={{ "--series": seriesColour(point.series) } as CSSProperties} cx={xOf(point.category)} cy={yOf(point.value)} r={block.mark === "point" ? 4 : 2.5} />
          ))}
      </svg>
    );
  }

  return (
    <div className="alix-block-with-export">
      <BlockExport blockId={exportId} turnId={turnId ?? exportId} />
    <figure className="alix-chart" data-alixai="chart" data-mark={block.mark} aria-label={block.title ?? block.description}>
      {block.title !== undefined && <figcaption className="alix-chart__title">{block.title}</figcaption>}
      <p className="alix-chart__description">{block.description}</p>
      {drawing}
      {legend.length > 1 && (
        <ul className="alix-chart__legend" aria-label={text.series}>
          {legend.map((entry) => (
            <li key={entry.index} style={{ "--series": seriesColour(entry.index) } as CSSProperties}>
              <span className="alix-chart__swatch" aria-hidden="true" />
              {entry.name}
            </li>
          ))}
        </ul>
      )}
      <div className="alix-chart__data">
        <button
          type="button"
          className="alix-chart__toggle"
          aria-expanded={open}
          aria-controls={dataId}
          onClick={() => setOpen((value) => !value)}
        >
          {open ? text.hideData : text.showData}
        </button>
        <div id={dataId} hidden={!open}>
          <table>
            <thead>
              <tr>
                {keys.map((key) => (
                  <th key={key} scope="col">
                    {key}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {block.data.map((record, index) => (
                <tr key={index}>
                  {keys.map((key) => (
                    <td key={key}>{label(record[key])}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </figure>
    </div>
  );
};

// ---- diagram ----

export interface AlixDiagramBlockProps {
  block: AlixAIBlock<"diagram">;
}

/** A diagram needs a renderer only the host can choose (`requires_host_renderer`), so the kit shows its source and its description. */
export const AlixDiagramBlock = ({ block }: AlixDiagramBlockProps): ReactElement => {
  const text = useAlixText().diagram;
  return (
    <figure className="alix-diagram" data-alixai="diagram" data-language={block.language}>
      <figcaption className="alix-diagram__caption">{block.title ?? block.description}</figcaption>
      <pre className="alix-diagram__source" tabIndex={0} aria-label={text.source}>
        {block.source}
      </pre>
    </figure>
  );
};

// ---- action card ----

export interface AlixActionCardProps {
  block: AlixAIBlock<"action_card">;
  /** The person picked one of the card's permitted next actions. The host routes it through the action protocol; the card commits nothing. */
  onAction?: (action: AlixAIBlock<"action_card">["actions"][number]) => void;
}

export const AlixActionCard = ({ block, onAction }: AlixActionCardProps): ReactElement => {
  const titleId = useId();
  const text = useAlixText().actions;
  return (
    <section className="alix-action-card" data-alixai="action-card" aria-labelledby={titleId}>
      <h3 id={titleId} className="alix-action-card__title">
        {block.title}
      </h3>
      <dl className="alix-action-card__facts">
        {block.facts.map((fact, index) => (
          <div key={index}>
            <dt>{fact.field}</dt>
            <dd>{fact.display ?? formatCanonicalValue(fact.value)}</dd>
          </div>
        ))}
      </dl>
      {onAction !== undefined && block.actions.length > 0 && (
        <div className="alix-action-card__actions">
          {block.actions.map((action, index) => (
            <button key={index} type="button" className="alix-action-card__action" onClick={() => onAction(action)}>
              {action.label ?? text[action.action]}
            </button>
          ))}
        </div>
      )}
    </section>
  );
};

// ---- clarification choices ----

export interface AlixClarificationFormProps {
  block: AlixAIBlock<"clarification_choices">;
  /** The person picked a choice: called with its `choice_id`. The host runs the agent with `forwardedProps.alixai.choiceId`. */
  onChoose: (choiceId: string) => void;
  /** The person wrote an answer (only when the block allows free text). Sent as an ordinary user message. */
  onFreeText?: (text: string) => void;
}

/** The form of a clarification: the question, its choices as buttons, and optional free text. A form is data, not executable UI. */
export const AlixClarificationForm = ({ block, onChoose, onFreeText }: AlixClarificationFormProps): ReactElement => {
  const text = useAlixText().choices;
  const [value, setValue] = useState("");
  const promptId = useId();
  const inputId = useId();
  return (
    <form
      className="alix-choices"
      data-alixai="choices"
      onSubmit={(event) => {
        event.preventDefault();
        const trimmed = value.trim();
        if (onFreeText !== undefined && trimmed !== "") {
          onFreeText(trimmed);
          setValue("");
        }
      }}
    >
      {block.prompt !== undefined && (
        <p id={promptId} className="alix-choices__prompt">
          {block.prompt}
        </p>
      )}
      <div className="alix-choices__options" role="group" {...(block.prompt !== undefined ? { "aria-labelledby": promptId } : { "aria-label": block.field })}>
        {block.choices.map((choice) => (
          <button key={choice.choice_id} type="button" className="alix-choices__option" {...(choice.description !== undefined && { title: choice.description })} onClick={() => onChoose(choice.choice_id)}>
            {choice.label}
          </button>
        ))}
      </div>
      {block.allow_free_text && onFreeText !== undefined && (
        <div className="alix-choices__free">
          <label className="alix-choices__label" htmlFor={inputId}>
            {text.freeText}
            <input id={inputId} className="alix-choices__input" type="text" value={value} onChange={(event) => setValue(event.target.value)} />
          </label>
          <button type="submit" className="alix-choices__send" disabled={value.trim() === ""}>
            {text.send}
          </button>
        </div>
      )}
    </form>
  );
};

// ---- progress ----

export const AlixProgressLine = ({ progress }: { progress: AlixAIProgress }): ReactElement => {
  const intl = useAlixIntl();
  const stages = useAlixText().progress.stages;
  const stage = Object.hasOwn(stages, progress.stage) ? stages[progress.stage] : progress.stage.replace(/[_-]+/g, " ");
  return (
    <p className="alix-progress" data-alixai="progress" role="status" {...(progress.state !== undefined && { "data-state": progress.state })}>
      {stage}
      {progress.percent !== undefined && ` ${formatNumber(intl, Math.round(progress.percent))}%`}
    </p>
  );
};
