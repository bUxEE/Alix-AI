// The part of JSON Patch (RFC 6902) that AG-UI state and activity deltas use: add, replace, remove, with array append (`/-`).
// move, copy and test are accepted by the protocol and refused here: the AlixAI mapping never emits them, so a stream that
// does is malformed. Path segments that could reach a prototype are refused. The input is never mutated.

export interface PatchOp {
  readonly op: string;
  readonly path: string;
  readonly value?: unknown;
}

const FORBIDDEN = new Set(["__proto__", "constructor", "prototype"]);

const isObject = (value: unknown): value is Record<string, unknown> => typeof value === "object" && value !== null && !Array.isArray(value);

const unescape = (segment: string): string => segment.replace(/~1/g, "/").replace(/~0/g, "~");

const clone = <T>(value: T): T => (value === undefined ? value : (JSON.parse(JSON.stringify(value)) as T));

const segmentsOf = (path: string): string[] | undefined => {
  if (path === "") return [];
  if (!path.startsWith("/")) return undefined;
  const parts = path.slice(1).split("/").map(unescape);
  return parts.some((part) => FORBIDDEN.has(part)) ? undefined : parts;
};

const index = (segment: string, length: number, allowEnd: boolean): number | undefined => {
  if (segment === "-") return allowEnd ? length : undefined;
  if (!/^(0|[1-9]\d*)$/.test(segment)) return undefined;
  const n = Number(segment);
  return n < length || (allowEnd && n === length) ? n : undefined;
};

/** Applies the operations to a copy of `target`. Returns undefined when any operation is not applicable (the patch is refused whole). */
export const applyPatch = (target: unknown, ops: readonly PatchOp[]): { value: unknown } | undefined => {
  let doc: unknown = clone(target);
  for (const op of ops) {
    if (op.op !== "add" && op.op !== "replace" && op.op !== "remove") return undefined;
    const path = segmentsOf(op.path);
    if (path === undefined) return undefined;
    if (path.length === 0) {
      if (op.op === "remove") return undefined;
      doc = clone(op.value);
      continue;
    }
    let parent: unknown = doc;
    for (const segment of path.slice(0, -1)) {
      if (Array.isArray(parent)) {
        const at = index(segment, parent.length, false);
        if (at === undefined) return undefined;
        parent = parent[at];
      } else if (isObject(parent) && Object.prototype.hasOwnProperty.call(parent, segment)) {
        parent = parent[segment];
      } else return undefined;
    }
    const last = path[path.length - 1] as string;
    const value = clone(op.value);
    if (Array.isArray(parent)) {
      if (op.op === "add") {
        const at = index(last, parent.length, true);
        if (at === undefined) return undefined;
        parent.splice(at, 0, value);
      } else {
        const at = index(last, parent.length, false);
        if (at === undefined) return undefined;
        if (op.op === "replace") parent[at] = value;
        else parent.splice(at, 1);
      }
    } else if (isObject(parent)) {
      const has = Object.prototype.hasOwnProperty.call(parent, last);
      if (op.op === "add") parent[last] = value;
      else if (!has) return undefined;
      else if (op.op === "replace") parent[last] = value;
      else delete parent[last];
    } else return undefined;
  }
  return { value: doc };
};

/** Reads the `patch` or `delta` member of an event as a list of operations; undefined unless it is one. */
export const readPatch = (value: unknown): PatchOp[] | undefined => {
  if (!Array.isArray(value)) return undefined;
  const ops: PatchOp[] = [];
  for (const item of value) {
    if (!isObject(item) || typeof item["op"] !== "string" || typeof item["path"] !== "string") return undefined;
    ops.push({ op: item["op"], path: item["path"], value: item["value"] });
  }
  return ops;
};
