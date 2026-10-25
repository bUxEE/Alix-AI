import type { AguiClient, RunHandlers } from "../src/agui/client";
import type { AguiEvent, RunAgentInput } from "../src/agui/types";

// Test helpers: a scripted AG-UI client and an SSE response builder. Nothing here touches the network.

export const ev = (type: string, members: Record<string, unknown> = {}): AguiEvent => ({ type, ...members });

export const runStarted = (runId = "run_1", threadId = "ses_1"): AguiEvent => ev("RUN_STARTED", { threadId, runId });
export const runFinished = (outcome: unknown = { type: "success" }, runId = "run_1", threadId = "ses_1"): AguiEvent =>
  ev("RUN_FINISHED", { threadId, runId, outcome });

export interface ScriptedClient extends AguiClient {
  readonly inputs: RunAgentInput[];
  readonly cancelled: Array<[string, string]>;
}

/** A client whose runs play back the events `script` returns for each input, with no transport. */
export const scriptedClient = (script: (input: RunAgentInput) => AguiEvent[] | Error): ScriptedClient => {
  const inputs: RunAgentInput[] = [];
  const cancelled: Array<[string, string]> = [];
  return {
    inputs,
    cancelled,
    run: async (input: RunAgentInput, handlers: RunHandlers) => {
      inputs.push(input);
      const played = script(input);
      if (played instanceof Error) throw played;
      for (const event of played) handlers.onEvent(event, undefined);
      return { terminal: played.some((event) => event.type === "RUN_FINISHED" || event.type === "RUN_ERROR"), aborted: false, reconnects: 0 };
    },
    cancel: async (threadId: string, runId: string) => {
      cancelled.push([threadId, runId]);
      return true;
    },
    capabilities: async () => ({}),
  };
};

const encoder = new TextEncoder();

/** An SSE body from frames: `[id, event]` pairs (id undefined for none). `cutAfter` ends the stream early without closing the run. */
export const sseBody = (frames: Array<[string | undefined, AguiEvent]>, cutAfter?: number): ReadableStream<Uint8Array> => {
  const chosen = cutAfter === undefined ? frames : frames.slice(0, cutAfter);
  const text = chosen.map(([id, event]) => `${id === undefined ? "" : `id: ${id}\n`}data: ${JSON.stringify(event)}\n\n`).join("");
  return new ReadableStream<Uint8Array>({
    start: (controller) => {
      controller.enqueue(encoder.encode(text));
      controller.close();
    },
  });
};

export const sseResponse = (frames: Array<[string | undefined, AguiEvent]>, init: { cutAfter?: number; headers?: Record<string, string> } = {}): Response =>
  new Response(sseBody(frames, init.cutAfter), { status: 200, headers: { "Content-Type": "text/event-stream", ...init.headers } });

export const flush = async (): Promise<void> => {
  await new Promise((resolve) => setTimeout(resolve, 0));
};
