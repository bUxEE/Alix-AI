import type { ChartBlock, ChartChannel, ChartRecord } from "./sdk-types";

// Pure geometry for the declarative chart block (bar, line, area, point, pie). The block carries inline data and field names, never
// an expression or a URL, so the picture is exactly reproducible from the data it declares. This file turns a block into numbers
// and SVG path strings; the component draws them. Anything that does not hold together (an encoded field that is not in the data,
// a pie without a positive total) gives `valid: false` and the component shows the text alternative instead of a wrong picture.

export const MAX_RECORDS = 500;
export const SERIES_COLORS = 6;

export interface Tick {
  readonly pos: number;
  readonly label: string;
}

export interface BarShape {
  readonly x: number;
  readonly y: number;
  readonly width: number;
  readonly height: number;
}

export interface SeriesShape {
  readonly name: string;
  /** 0-based index into the six series colours. */
  readonly color: number;
  readonly points: readonly { readonly x: number; readonly y: number; readonly label: string }[];
  readonly bars: readonly BarShape[];
  readonly line: string;
  readonly area: string;
}

export interface SliceShape {
  readonly label: string;
  readonly value: number;
  readonly share: number;
  readonly color: number;
  readonly path: string;
}

export interface ChartModel {
  readonly valid: boolean;
  readonly mark: ChartBlock["mark"];
  readonly width: number;
  readonly height: number;
  readonly plot: { readonly left: number; readonly top: number; readonly right: number; readonly bottom: number };
  readonly xTicks: readonly Tick[];
  readonly yTicks: readonly Tick[];
  readonly series: readonly SeriesShape[];
  readonly slices: readonly SliceShape[];
  readonly truncated: boolean;
}

const EMPTY: Omit<ChartModel, "mark" | "width" | "height"> = {
  valid: false,
  plot: { left: 0, top: 0, right: 0, bottom: 0 },
  xTicks: [],
  yTicks: [],
  series: [],
  slices: [],
  truncated: false,
};

const num = (value: unknown): number | undefined => (typeof value === "number" && Number.isFinite(value) ? value : undefined);

const toNumber = (value: unknown, channel: ChartChannel): number | undefined => {
  if (channel.type === "temporal") {
    const time = typeof value === "string" ? Date.parse(value) : num(value);
    return time === undefined || Number.isNaN(time) ? undefined : time;
  }
  if (typeof value === "string" && channel.type === "quantitative" && value.trim() !== "") {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : undefined;
  }
  return num(value);
};

const label = (value: unknown): string => (value === null || value === undefined ? "" : String(value));

/** "Nice" tick values over [min, max]: about `count` of them, on 1, 2, 5 times a power of ten. */
export const niceTicks = (min: number, max: number, count = 5): number[] => {
  if (!(max > min)) return [min];
  const span = max - min;
  const rough = span / Math.max(1, count);
  const power = 10 ** Math.floor(Math.log10(rough));
  const unit = [1, 2, 5, 10].map((m) => m * power).find((step) => step >= rough) ?? 10 * power;
  const start = Math.ceil(min / unit) * unit;
  const ticks: number[] = [];
  for (let value = start; value <= max + unit * 1e-9; value += unit) ticks.push(Math.round(value / unit) * unit);
  return ticks;
};

const polar = (cx: number, cy: number, radius: number, angle: number): [number, number] => [cx + radius * Math.sin(angle), cy - radius * Math.cos(angle)];

const fmt = (n: number): string => String(Math.round(n * 100) / 100);

export const buildChart = (block: ChartBlock, width = 560, height = 300): ChartModel => {
  const base = { mark: block.mark, width, height };
  const truncated = block.data.length > MAX_RECORDS;
  const data: ChartRecord[] = block.data.slice(0, MAX_RECORDS);
  const { encoding } = block;
  const present = (channel: ChartChannel | undefined): channel is ChartChannel =>
    channel !== undefined && data.every((record) => Object.prototype.hasOwnProperty.call(record, channel.field));

  if (block.mark === "pie") {
    const { theta, color } = encoding;
    if (!present(theta) || !present(color)) return { ...base, ...EMPTY, truncated };
    const totals = new Map<string, number>();
    for (const record of data) {
      const value = toNumber(record[theta.field], theta);
      if (value === undefined || value < 0) return { ...base, ...EMPTY, truncated };
      const key = label(record[color.field]);
      totals.set(key, (totals.get(key) ?? 0) + value);
    }
    const total = [...totals.values()].reduce((sum, value) => sum + value, 0);
    if (!(total > 0)) return { ...base, ...EMPTY, truncated };
    const cx = width / 2;
    const cy = height / 2;
    const radius = Math.min(width, height) / 2 - 12;
    let angle = 0;
    const slices: SliceShape[] = [];
    let colorIndex = 0;
    for (const [name, value] of totals) {
      const share = value / total;
      const next = angle + share * Math.PI * 2;
      let path: string;
      if (share >= 0.9999) {
        path = `M${fmt(cx)} ${fmt(cy - radius)} A${fmt(radius)} ${fmt(radius)} 0 1 1 ${fmt(cx - 0.01)} ${fmt(cy - radius)} Z`;
      } else {
        const [x1, y1] = polar(cx, cy, radius, angle);
        const [x2, y2] = polar(cx, cy, radius, next);
        path = `M${fmt(cx)} ${fmt(cy)} L${fmt(x1)} ${fmt(y1)} A${fmt(radius)} ${fmt(radius)} 0 ${share > 0.5 ? 1 : 0} 1 ${fmt(x2)} ${fmt(y2)} Z`;
      }
      slices.push({ label: name, value, share, color: colorIndex % SERIES_COLORS, path });
      colorIndex += 1;
      angle = next;
    }
    return { ...base, valid: true, plot: { left: 0, top: 0, right: width, bottom: height }, xTicks: [], yTicks: [], series: [], slices, truncated };
  }

  const { x, y, color } = encoding;
  if (!present(x) || !present(y) || (color !== undefined && !present(color))) return { ...base, ...EMPTY, truncated };

  const plot = { left: 52, top: 12, right: width - 12, bottom: height - 36 };
  const plotWidth = plot.right - plot.left;
  const plotHeight = plot.bottom - plot.top;

  const categorical = x.type === "nominal" || x.type === "ordinal" || block.mark === "bar";
  const categories: string[] = [];
  const xNumbers: number[] = [];
  const rows: { series: string; xKey: string; xNum: number | undefined; y: number }[] = [];
  for (const record of data) {
    const yValue = toNumber(record[y.field], y);
    if (yValue === undefined) return { ...base, ...EMPTY, truncated };
    const xKey = label(record[x.field]);
    const xNum = categorical ? undefined : toNumber(record[x.field], x);
    if (!categorical && xNum === undefined) return { ...base, ...EMPTY, truncated };
    if (categorical && !categories.includes(xKey)) categories.push(xKey);
    if (xNum !== undefined) xNumbers.push(xNum);
    rows.push({ series: color === undefined ? "" : label(record[color.field]), xKey, xNum, y: yValue });
  }
  if (rows.length === 0) return { ...base, ...EMPTY, truncated };

  const seriesNames = [...new Set(rows.map((row) => row.series))];
  const yMin = Math.min(0, ...rows.map((row) => row.y));
  const yMax = Math.max(0, ...rows.map((row) => row.y));
  const yTickValues = niceTicks(yMin, yMax === yMin ? yMin + 1 : yMax);
  const lo = Math.min(yMin, yTickValues[0] ?? yMin);
  const hi = Math.max(yMax, yTickValues[yTickValues.length - 1] ?? yMax);
  const scaleY = (value: number): number => plot.bottom - ((value - lo) / (hi - lo || 1)) * plotHeight;
  const yTicks: Tick[] = yTickValues.map((value) => ({ pos: scaleY(value), label: String(value) }));

  let xTicks: Tick[];
  let scaleX: (row: { xKey: string; xNum: number | undefined }) => number;
  let band = 0;
  if (categorical) {
    band = plotWidth / Math.max(1, categories.length);
    scaleX = (row) => plot.left + band * (categories.indexOf(row.xKey) + 0.5);
    xTicks = categories.map((name, index) => ({ pos: plot.left + band * (index + 0.5), label: name }));
  } else {
    const xMin = Math.min(...xNumbers);
    const xMax = Math.max(...xNumbers);
    const span = xMax - xMin || 1;
    scaleX = (row) => plot.left + (((row.xNum as number) - xMin) / span) * plotWidth;
    const temporal = x.type === "temporal";
    const tickValues = xMax === xMin ? [xMin] : niceTicks(xMin, xMax, 5).filter((value) => value >= xMin && value <= xMax);
    xTicks = tickValues.map((value) => ({
      pos: plot.left + ((value - xMin) / span) * plotWidth,
      label: temporal ? new Date(value).toISOString().slice(0, 10) : String(value),
    }));
  }

  const series: SeriesShape[] = seriesNames.map((name, index) => {
    const mine = rows.filter((row) => row.series === name);
    const points = mine.map((row) => ({ x: scaleX(row), y: scaleY(row.y), label: `${row.xKey}: ${row.y}` }));
    const sorted = categorical ? points : [...points].sort((a, b) => a.x - b.x);
    const zero = scaleY(0);
    const groupWidth = band * 0.7;
    const barWidth = groupWidth / Math.max(1, seriesNames.length);
    const bars: BarShape[] =
      block.mark === "bar"
        ? mine.map((row) => {
            const cx = scaleX(row);
            const top = scaleY(row.y);
            return {
              x: cx - groupWidth / 2 + barWidth * index,
              y: Math.min(top, zero),
              width: Math.max(1, barWidth - 2),
              height: Math.abs(zero - top),
            };
          })
        : [];
    const line = sorted.length > 1 && (block.mark === "line" || block.mark === "area") ? sorted.map((p, i) => `${i === 0 ? "M" : "L"}${fmt(p.x)} ${fmt(p.y)}`).join(" ") : "";
    const first = sorted[0];
    const last = sorted[sorted.length - 1];
    const area = block.mark === "area" && first !== undefined && last !== undefined && sorted.length > 1 ? `${line} L${fmt(last.x)} ${fmt(zero)} L${fmt(first.x)} ${fmt(zero)} Z` : "";
    return { name, color: index % SERIES_COLORS, points, bars, line, area };
  });

  return { ...base, valid: true, plot, xTicks, yTicks, series, slices: [], truncated };
};
