import { readFileSync } from "node:fs";
import { resolve } from "node:path";

// The golden streams of M3-25 (tests/agui/testdata): one .json (the request, the response head, the events) and one .sse
// (the exact bytes the gateway sent) per scenario. The adapter tests replay them; they are never edited here.

// The suite runs with the package directory as its working directory (pnpm --filter, vitest).
const DIR = `${resolve(process.cwd(), "../../tests/agui/testdata")}/`;

export interface GoldenRequest {
  readonly method: string;
  readonly path: string;
  readonly headers?: Record<string, string>;
  readonly body: Record<string, unknown> & {
    threadId: string;
    runId: string;
    messages: Array<Record<string, unknown>>;
    forwardedProps?: Record<string, unknown>;
    resume?: Array<{ interruptId: string; status: string; payload?: unknown }>;
  };
}

export interface GoldenMeta {
  readonly name: string;
  readonly description: string;
  readonly request?: GoldenRequest;
  readonly response: {
    readonly status: number;
    readonly headers?: Record<string, string>;
    readonly events: Array<Record<string, unknown> & { type: string }>;
  };
}

export interface SseFrameText {
  readonly id: string | undefined;
  readonly data: string;
  /** The frame as it is on the wire, including its blank line. */
  readonly raw: string;
}

export interface Golden {
  readonly meta: GoldenMeta;
  readonly sse: string;
  readonly frames: readonly SseFrameText[];
}

export const GOLDEN_NAMES = [
  "attachments_preview",
  "chart",
  "choice_answer",
  "client_context",
  "clarification",
  "error",
  "forms",
  "plain_answer",
  "prepare_interrupt",
  "read_with_citations",
  "resume_approve",
  "resume_reject",
  "stale",
  "state_delta",
  "state_snapshot",
  "suggestions",
  "table",
  "ui_tool_call",
  "ui_tool_capability_named",
  "ui_tool_result",
  "ui_tool_undeclared",
  "ui_tool_unregistered",
] as const;

export type GoldenName = (typeof GOLDEN_NAMES)[number];

export const framesOf = (sse: string): SseFrameText[] =>
  sse
    .split("\n\n")
    .filter((chunk) => chunk.trim().length > 0)
    .map((chunk) => {
      const lines = chunk.split("\n");
      const id = lines.find((line) => line.startsWith("id:"))?.slice(3).trim();
      const data = lines
        .filter((line) => line.startsWith("data:"))
        .map((line) => line.slice(5).trimStart())
        .join("\n");
      return { id, data, raw: `${chunk}\n\n` };
    });

const cache = new Map<string, Golden>();

export const golden = (name: GoldenName): Golden => {
  const cached = cache.get(name);
  if (cached !== undefined) return cached;
  const meta = JSON.parse(readFileSync(`${DIR}${name}.json`, "utf8")) as GoldenMeta;
  const sse = readFileSync(`${DIR}${name}.sse`, "utf8");
  const loaded: Golden = { meta, sse, frames: framesOf(sse) };
  cache.set(name, loaded);
  return loaded;
};
