import { createServer } from "node:http";
import type { Server } from "node:http";
import type { AddressInfo } from "node:net";
import { EventSchemas } from "@ag-ui/core/schemas";

// A loopback gateway that answers POST /v1/agui with a stream the test scripts. It exists for the streams the golden files do
// not hold (a message streamed in several fragments, a diagram block, a connect run): they are SYNTHETIC, built from the mapping
// of docs/integration/agui.md, and every event is validated against the real @ag-ui/core schemas before it is served. The token
// rule is the real gateway's: a Bearer context token whose session is the thread.

export interface ScriptedGateway {
  readonly url: string;
  /** The bodies of the run requests received, parsed. */
  readonly requests: Array<Record<string, any>>;
  /** Lets a stream that was scripted with `pause` continue. */
  release: () => void;
  close: () => Promise<void>;
}

/** An event, or the marker where the stream stops until `release()` is called. */
export type Step = Record<string, unknown> | "pause";

export interface ScriptedOptions {
  /** Frames get `id:` lines (the cursor of a resumable stream). A connect run has none: every event of it carries seq 0. */
  ids?: boolean;
}

export const startScriptedGateway = async (steps: readonly Step[], options: ScriptedOptions = {}): Promise<ScriptedGateway> => {
  for (const step of steps) {
    if (step !== "pause") EventSchemas.parse(step);
  }
  const requests: Array<Record<string, any>> = [];
  let resume: (() => void) | undefined;
  let released = false;

  const server: Server = createServer(async (request, response) => {
    const chunks: Buffer[] = [];
    for await (const chunk of request) chunks.push(chunk as Buffer);
    const match = /^Bearer ([A-Za-z0-9._~+/=-]+)$/.exec(request.headers["authorization"] ?? "");
    const body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}") as Record<string, any>;
    if (request.method !== "POST" || request.url !== "/v1/agui" || match === null || match[1]?.split(".")[0] !== body["threadId"]) {
      response.writeHead(403, { "Content-Type": "application/json" });
      response.end(JSON.stringify({ error: { code: "scope_violation", message: "the thread does not resolve for this caller", retryable: false } }));
      return;
    }
    requests.push(body);
    response.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-store" });
    let seq = 0;
    for (const step of steps) {
      if (step === "pause") {
        if (!released) await new Promise<void>((resolveStep) => void (resume = resolveStep));
        continue;
      }
      seq += 1;
      const event = JSON.stringify(step)
        .replace(/"threadId":"[^"]*"/g, `"threadId":${JSON.stringify(body["threadId"])}`)
        .replace(/"runId":"[^"]*"/g, `"runId":${JSON.stringify(body["runId"])}`);
      response.write(`${options.ids === false ? "" : `id: ${seq}\n`}data: ${event}\n\n`);
      await new Promise((resolveStep) => setImmediate(resolveStep));
    }
    response.end();
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  return {
    url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
    requests,
    release: () => {
      released = true;
      resume?.();
    },
    close: () =>
      new Promise<void>((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  };
};

// ---- event builders (the shapes of internal/agui/events.go) ----

export const runStarted = (): Record<string, unknown> => ({ type: "RUN_STARTED", threadId: "t", runId: "r" });

export const runFinished = (result: Record<string, unknown> = {}): Record<string, unknown> => ({
  type: "RUN_FINISHED",
  threadId: "t",
  runId: "r",
  result,
  outcome: { type: "success" },
});

/** A text message streamed in fragments, optionally pausing the stream after the n-th fragment. */
export const textMessage = (messageId: string, fragments: readonly string[], pauseAfter?: number): Step[] => [
  { type: "TEXT_MESSAGE_START", messageId, role: "assistant" },
  ...fragments.flatMap((delta, index): Step[] => {
    const content: Step = { type: "TEXT_MESSAGE_CONTENT", messageId, delta };
    return pauseAfter === index + 1 ? [content, "pause"] : [content];
  }),
  { type: "TEXT_MESSAGE_END", messageId },
];
