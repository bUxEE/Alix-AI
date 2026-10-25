// The feature matrix of the gate: one entry per row of section 12 of docs/integration/agui.md (the CopilotKit feature column,
// word for word), plus the rows the task adds (thread isolation). test/matrix.test.ts fails when a row of agui.md has no entry
// here, when an entry that claims a test is never the subject of a `describe(row("<id>"))`, and when the published matrix of
// docs/integration/copilotkit-compatibility.md misses a row or names another status.

export type RowStatus =
  /** Shown by a test of this gate through the real @ag-ui/client and @copilotkit/runtime. */
  | "shown"
  /** Shown, with a limit of CopilotKit's own runtime that the test pins and the document states. */
  | "limited"
  /** Not emitted by design: the test shows that nothing of it ever reaches a client. */
  | "not-emitted"
  /** Not claimed: a statement, with a test that nothing implies the opposite. */
  | "not-claimed"
  /** The row cannot be shown through a CopilotKit or AG-UI client of the pinned releases; the test records why. */
  | "not-shown";

export interface Row {
  readonly id: string;
  /** The first cell of the row in section 12 of docs/integration/agui.md, or the task's own name for a row it adds. */
  readonly feature: string;
  readonly status: RowStatus;
  /** True for the rows of agui.md section 12. */
  readonly inAgui: boolean;
}

export const ROWS: readonly Row[] = [
  { id: "chat", feature: "Chat, sidebar, popup and headless components (`useCopilotChat`)", status: "shown", inAgui: true },
  { id: "streaming", feature: "Streaming messages", status: "shown", inAgui: true },
  { id: "lifecycle", feature: "Run lifecycle, steps, errors", status: "shown", inAgui: true },
  { id: "history", feature: "Message history on load (`connect`)", status: "shown", inAgui: true },
  { id: "frontend-actions", feature: "Frontend actions and tools the model calls (`useCopilotAction`)", status: "shown", inAgui: true },
  { id: "backend-tool-calls", feature: "Backend tool calls rendered in the UI", status: "shown", inAgui: true },
  { id: "tool-results", feature: "Deterministic tool results", status: "shown", inAgui: true },
  { id: "approval", feature: "Human-in-the-loop approval", status: "shown", inAgui: true },
  { id: "generative-ui", feature: "Generative UI", status: "shown", inAgui: true },
  { id: "blocks", feature: "Tables, charts, diagrams, forms", status: "shown", inAgui: true },
  { id: "citations", feature: "Citations and source references", status: "shown", inAgui: true },
  { id: "attachments", feature: "Attachments and previews", status: "shown", inAgui: true },
  { id: "shared-state", feature: "Shared state (`useCoAgent`)", status: "shown", inAgui: true },
  { id: "readable-context", feature: "Readable context (`useCopilotReadable`)", status: "shown", inAgui: true },
  { id: "state-render", feature: "Agent state rendering (`useCoAgentStateRender`)", status: "shown", inAgui: true },
  { id: "predictive-state", feature: "Predictive state updates", status: "not-emitted", inAgui: true },
  { id: "suggestions", feature: "Chat suggestions", status: "shown", inAgui: true },
  { id: "run-control", feature: "Stop, regenerate, retry, reconnect", status: "shown", inAgui: true },
  { id: "reasoning", feature: "Reasoning display", status: "not-emitted", inAgui: true },
  { id: "subagents", feature: "Sub-agents", status: "not-emitted", inAgui: true },
  { id: "raw", feature: "Raw provider events", status: "not-emitted", inAgui: true },
  { id: "voice", feature: "Voice", status: "not-claimed", inAgui: true },
  { id: "no-cloud", feature: "Copilot Cloud, telemetry, public API key", status: "shown", inAgui: true },
  { id: "transports", feature: "Transports", status: "limited", inAgui: true },
  { id: "thread-isolation", feature: "Thread isolation (thread A cannot resume thread B's interrupt)", status: "shown", inAgui: false },
];

/** The title of the `describe` that proves a row. The matrix test finds the rows a gate file proves by this call. */
export const row = (id: string): string => {
  const found = ROWS.find((candidate) => candidate.id === id);
  if (found === undefined) throw new Error(`unknown matrix row ${id}`);
  return `[${id}] ${found.feature}`;
};
