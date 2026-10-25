// A small reader and layout for the subset of Mermaid flowcharts the demo's diagrams use (`flowchart LR|TD`, nodes `A[text]`,
// `A{text}`, `A(text)`, edges `A --> B` and `A -->|label| B`). It is the demo host's own diagram renderer: the kit ships none,
// and Mermaid itself is a heavy dependency this demo does not add. Anything outside the subset returns undefined, and the
// caller shows the source as text instead. Nothing here builds markup: the result is numbers and strings for an SVG template.

export type FlowShape = "box" | "decision" | "round";

export interface FlowNode {
  readonly id: string;
  readonly label: string;
  readonly shape: FlowShape;
  readonly x: number;
  readonly y: number;
  readonly width: number;
  readonly height: number;
}

export interface FlowEdge {
  readonly key: string;
  readonly path: string;
  readonly label?: string;
  readonly labelX: number;
  readonly labelY: number;
  readonly back: boolean;
}

export interface FlowLayout {
  readonly width: number;
  readonly height: number;
  readonly nodes: readonly FlowNode[];
  readonly edges: readonly FlowEdge[];
}

const NODE = String.raw`([A-Za-z][\w-]*)\s*(?:\[([^\]]*)\]|\{([^}]*)\}|\(([^)]*)\))?`;
const EDGE = new RegExp(String.raw`^${NODE}\s*-->\s*(?:\|([^|]*)\|\s*)?${NODE}$`);
const ALONE = new RegExp(String.raw`^${NODE}$`);

const GAP_MAIN = 64;
const GAP_CROSS = 28;
const PAD = 16;
const CHAR = 7.4;

interface Parsed {
  readonly direction: "LR" | "TD";
  readonly nodes: Map<string, { label: string; shape: FlowShape; order: number }>;
  readonly edges: { from: string; to: string; label?: string }[];
}

const parse = (source: string): Parsed | undefined => {
  const lines = source
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "" && !line.startsWith("%%"));
  const head = /^(flowchart|graph)\s+(LR|RL|TD|TB)$/.exec(lines.shift() ?? "");
  if (head === null) return undefined;
  const direction = head[2] === "LR" || head[2] === "RL" ? "LR" : "TD";
  const nodes: Parsed["nodes"] = new Map();
  const edges: Parsed["edges"] = [];
  const note = (id: string, box?: string, decision?: string, round?: string): void => {
    const label = box ?? decision ?? round;
    const known = nodes.get(id);
    const shape: FlowShape = decision !== undefined ? "decision" : round !== undefined ? "round" : "box";
    if (known === undefined) nodes.set(id, { label: (label ?? id).trim(), shape, order: nodes.size });
    else if (label !== undefined) nodes.set(id, { ...known, label: label.trim(), shape });
  };
  for (const line of lines) {
    const edge = EDGE.exec(line);
    if (edge !== null) {
      const [, from, fromBox, fromDecision, fromRound, label, to, toBox, toDecision, toRound] = edge;
      if (from === undefined || to === undefined) return undefined;
      note(from, fromBox, fromDecision, fromRound);
      note(to, toBox, toDecision, toRound);
      edges.push({ from, to, ...(label !== undefined && label.trim() !== "" && { label: label.trim() }) });
      continue;
    }
    const alone = ALONE.exec(line);
    if (alone === null || alone[1] === undefined) return undefined;
    note(alone[1], alone[2], alone[3], alone[4]);
  }
  return nodes.size === 0 ? undefined : { direction, nodes, edges };
};

/** Lays out a flowchart, or undefined when the source is outside the supported subset. */
export const layoutFlowchart = (source: string): FlowLayout | undefined => {
  const parsed = parse(source);
  if (parsed === undefined) return undefined;
  const ids = [...parsed.nodes.keys()];

  // Back edges (to a node still on the depth-first path) close loops; they are drawn around the chart, not used for ranks.
  const back = new Set<number>();
  const state = new Map<string, "open" | "done">();
  const visit = (id: string): void => {
    state.set(id, "open");
    parsed.edges.forEach((edge, index) => {
      if (edge.from !== id) return;
      const seen = state.get(edge.to);
      if (seen === "open") back.add(index);
      else if (seen === undefined) visit(edge.to);
    });
    state.set(id, "done");
  };
  for (const id of ids) if (!state.has(id)) visit(id);

  const rank = new Map<string, number>(ids.map((id) => [id, 0]));
  for (let pass = 0; pass < ids.length; pass += 1) {
    parsed.edges.forEach((edge, index) => {
      if (back.has(index)) return;
      const next = (rank.get(edge.from) ?? 0) + 1;
      if (next > (rank.get(edge.to) ?? 0)) rank.set(edge.to, next);
    });
  }

  const columns: string[][] = [];
  for (const id of ids) (columns[rank.get(id) ?? 0] ??= []).push(id);

  const size = (id: string): { width: number; height: number } => {
    const node = parsed.nodes.get(id);
    const text = Math.max(56, (node?.label.length ?? 1) * CHAR);
    return node?.shape === "decision" ? { width: text + 56, height: 56 } : { width: text + 32, height: 40 };
  };

  const horizontal = parsed.direction === "LR";
  const placed = new Map<string, FlowNode>();
  const mainSizes = columns.map((column) => Math.max(...column.map((id) => (horizontal ? size(id).width : size(id).height))));
  const crossSizes = columns.map((column) => column.reduce((sum, id) => sum + (horizontal ? size(id).height : size(id).width), 0) + GAP_CROSS * (column.length - 1));
  const crossMax = Math.max(...crossSizes);
  let main = PAD;
  columns.forEach((column, at) => {
    let cross = PAD + ((crossMax - (crossSizes[at] ?? 0)) / 2);
    for (const id of column) {
      const { width, height } = size(id);
      const node = parsed.nodes.get(id);
      const along = main + ((mainSizes[at] ?? 0) - (horizontal ? width : height)) / 2;
      placed.set(id, {
        id,
        label: node?.label ?? id,
        shape: node?.shape ?? "box",
        x: horizontal ? along : cross,
        y: horizontal ? cross : along,
        width,
        height,
      });
      cross += (horizontal ? height : width) + GAP_CROSS;
    }
    main += (mainSizes[at] ?? 0) + GAP_MAIN;
  });

  const width = horizontal ? main - GAP_MAIN + PAD : crossMax + PAD * 2;
  const height = horizontal ? crossMax + PAD * 2 : main - GAP_MAIN + PAD;
  const loopLine = (horizontal ? height : width) + 20;

  const edges: FlowEdge[] = parsed.edges.flatMap((edge, index) => {
    const from = placed.get(edge.from);
    const to = placed.get(edge.to);
    if (from === undefined || to === undefined) return [];
    const isBack = back.has(index);
    let path: string;
    let labelX: number;
    let labelY: number;
    if (isBack && horizontal) {
      const sx = from.x + from.width / 2;
      const tx = to.x + to.width / 2;
      const sy = from.y + from.height;
      const ty = to.y + to.height;
      path = `M ${sx} ${sy} C ${sx} ${loopLine}, ${tx} ${loopLine}, ${tx} ${ty + 2}`;
      labelX = (sx + tx) / 2;
      labelY = loopLine - 8;
    } else if (isBack) {
      const sx = from.x + from.width;
      const tx = to.x + to.width;
      const sy = from.y + from.height / 2;
      const ty = to.y + to.height / 2;
      path = `M ${sx} ${sy} C ${loopLine} ${sy}, ${loopLine} ${ty}, ${tx + 2} ${ty}`;
      labelX = loopLine - 8;
      labelY = (sy + ty) / 2;
    } else if (horizontal) {
      const sx = from.x + from.width;
      const sy = from.y + from.height / 2;
      const tx = to.x - 2;
      const ty = to.y + to.height / 2;
      const mid = (sx + tx) / 2;
      path = `M ${sx} ${sy} C ${mid} ${sy}, ${mid} ${ty}, ${tx} ${ty}`;
      labelX = mid;
      labelY = (sy + ty) / 2;
    } else {
      const sx = from.x + from.width / 2;
      const sy = from.y + from.height;
      const tx = to.x + to.width / 2;
      const ty = to.y - 2;
      const mid = (sy + ty) / 2;
      path = `M ${sx} ${sy} C ${sx} ${mid}, ${tx} ${mid}, ${tx} ${ty}`;
      labelX = (sx + tx) / 2;
      labelY = mid;
    }
    return [{ key: `${edge.from}-${edge.to}-${index}`, path, labelX, labelY, back: isBack, ...(edge.label !== undefined && { label: edge.label }) }];
  });

  // Loops run outside the chart, below it (LR) or beside it (TD): the canvas grows to hold them.
  const loops = back.size > 0 ? loopLine + 12 : undefined;
  return {
    width: Math.ceil(!horizontal && loops !== undefined ? loops : width),
    height: Math.ceil(horizontal && loops !== undefined ? loops : height),
    nodes: [...placed.values()],
    edges,
  };
};
