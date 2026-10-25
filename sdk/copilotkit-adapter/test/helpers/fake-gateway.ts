import { createServer } from "node:http";
import type { IncomingMessage, Server, ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { golden } from "./golden.js";
import type { GoldenName, SseFrameText } from "./golden.js";

// A loopback stand-in for the AlixAI gateway's AG-UI face (internal/gateway/routes_v1_agui.go), serving the golden
// streams of M3-25 byte for byte. It enforces what the adapter must respect: a single-use Bearer context token whose
// session is the thread, the confirmation secret only in its header, one approval per preparation (409), a missing secret
// (428), the identical 403 for a foreign thread, recorded runs replayed under the same runId and a cursor that resumes
// at a TurnEvent boundary.

/** A secret that looks like a real one (`cfm_` and 43 URL-safe characters) and is not in any golden file. */
export const CONFIRMATION_SECRET = `cfm_${"Kq7_-".repeat(8)}ZZZ`;
export const SECRET_HEADER = "x-alixai-confirmation-token";

export interface Script {
  golden: GoldenName;
  /** Answer with this AlixAI error instead of a stream. */
  fail?: { status: number; code: string; message?: string; details?: Record<string, unknown>; retryAfterSeconds?: number };
  /** Cut the connection after this many frames. */
  cutAfterFrames?: number;
  /** Close the connection before any byte of the answer. */
  dropBeforeHead?: boolean;
  /** Send this many frames, then keep the stream open until the client goes away or the run is cancelled. */
  holdAfterFrames?: number;
}

export interface RecordedRequest {
  readonly method: string;
  readonly path: string;
  readonly headers: Readonly<Record<string, string>>;
  readonly body: Record<string, any> | undefined;
  readonly rawBody: string;
}

export interface FakeGatewayOptions {
  /** Rewrite the threadId and runId of the golden events to the ones the request carries, and move every expiry into the future. */
  rewriteIds?: boolean;
  /** Answer every connect run only after this many milliseconds: the gateway's history read being slow. */
  connectDelayMs?: number;
  /** Take a run request only after this many milliseconds, so a connect that arrives meanwhile reads the history without it. */
  runDelayMs?: number;
}

export interface FakeGateway {
  readonly url: string;
  readonly requests: RecordedRequest[];
  /** Scripts are used in order, one per run request, before the gateway infers the scenario from the request. */
  queue: (...scripts: Script[]) => void;
  /** The next `count` reads of a run's events answer 409 run_in_progress. */
  holdEvents: (count: number) => void;
  /** The runs the gateway has been asked to cancel. */
  readonly cancelled: string[];
  /** The connect runs (no message, no resume) it has been asked for. They are not in `requests`. */
  readonly connects: RecordedRequest[];
  close: () => Promise<void>;
}

const TOKEN = /^Bearer ([A-Za-z0-9._~+/=-]+)$/;

const lastUserText = (messages: Array<Record<string, any>>): string | undefined => {
  const last = messages[messages.length - 1];
  if (last === undefined || last.role !== "user") return undefined;
  if (typeof last.content === "string") return last.content;
  const text = Array.isArray(last.content) ? last.content.find((part: any) => part?.type === "text") : undefined;
  return typeof text?.text === "string" ? text.text : undefined;
};

/** The golden scenario a request corresponds to, by the request each golden file recorded. */
const infer = (body: Record<string, any>): GoldenName | undefined => {
  const resume = body["resume"] as Array<{ status: string; payload?: any }> | undefined;
  if (Array.isArray(resume) && resume.length > 0) {
    return resume[0]?.status === "resolved" && resume[0]?.payload?.approved === true ? "resume_approve" : "resume_reject";
  }
  const lastMessage = (body["messages"] as Array<Record<string, any>> | undefined)?.at(-1);
  if (lastMessage?.role === "tool" && String(lastMessage["toolCallId"]).startsWith("alixai_ui_")) return "ui_tool_result";
  const choice = body["forwardedProps"]?.alixai?.choiceId;
  if (typeof choice === "string") return "choice_answer";
  const text = lastUserText(body["messages"] ?? []);
  const byText: Record<string, GoldenName> = {
    "show item item-1": "plain_answer",
    "create an item named widget with quantity 3": "prepare_interrupt",
    "create an item named widget, small": "clarification",
    "what can you do": "suggestions",
    "what is the warranty on the Model X kettle": "read_with_citations",
    "what is the weather like in Rome tomorrow": "error",
  };
  return text === undefined ? undefined : byText[text];
};

const json = (response: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void => {
  response.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store", ...headers });
  response.end(JSON.stringify(body));
};

const errorBody = (code: string, message: string, details?: Record<string, unknown>): unknown => ({
  error: { code, message, retryable: ["unavailable", "degraded", "timeout", "rate_limited", "resource_exhausted"].includes(code), ...(details !== undefined && { details }) },
});

const readBody = async (request: IncomingMessage): Promise<string> => {
  const chunks: Buffer[] = [];
  for await (const chunk of request) chunks.push(chunk as Buffer);
  return Buffer.concat(chunks).toString("utf8");
};

interface Run {
  frames: SseFrameText[];
  held?: ServerResponse;
}

export const startFakeGateway = async (options: FakeGatewayOptions = {}): Promise<FakeGateway> => {
  const requests: RecordedRequest[] = [];
  const connects: RecordedRequest[] = [];
  const cancelled: string[] = [];
  /** What a connect run replays: per session, the user messages it sent and the assistant text the goldens answered with. */
  const history = new Map<string, Array<{ id: string; role: "user" | "assistant"; content: string }>>();
  const scripts: Script[] = [];
  const usedTokens = new Set<string>();
  const runs = new Map<string, Run>();
  const issued = new Map<string, string>();
  const consumed = new Set<string>();
  let eventHolds = 0;

  // The goldens are fixed in time: their expiry is in the past by now, and CopilotKit refuses to resume an expired interrupt.
  // With rewriteIds the thread, the run and every expiry follow the request and the clock.
  const expiry = new Date(Date.now() + 10 * 60 * 1000).toISOString();
  const adapt = (text: string, threadId: string, runId: string): string =>
    text
      .replace(/"threadId":"[^"]*"/g, `"threadId":${JSON.stringify(threadId)}`)
      .replace(/"runId":"[^"]*"/g, `"runId":${JSON.stringify(runId)}`)
      .replace(/"(expiresAt|expires_at)":"[^"]*"/g, `"$1":"${expiry}"`);
  const rewrite = (frame: SseFrameText, threadId: string, runId: string): SseFrameText =>
    options.rewriteIds === true ? { ...frame, raw: adapt(frame.raw, threadId, runId), data: adapt(frame.data, threadId, runId) } : frame;

  /** The session of the token, once the token is accepted. A token is single use, like the gateway's. */
  const authenticate = (request: IncomingMessage, response: ServerResponse): string | undefined => {
    const match = TOKEN.exec(request.headers["authorization"] ?? "");
    if (match === null) {
      json(response, 401, errorBody("unauthenticated", "a context token is required"), { "WWW-Authenticate": "Bearer" });
      return undefined;
    }
    const token = match[1] as string;
    if (usedTokens.has(token)) {
      json(response, 401, errorBody("replayed", "the context token was already used"));
      return undefined;
    }
    usedTokens.add(token);
    return token.split(".")[0];
  };

  const notVisible = (response: ServerResponse): void =>
    json(response, 403, errorBody("scope_violation", "the thread or the interrupt does not resolve for this caller"));

  const writeStream = async (
    response: ServerResponse,
    frames: SseFrameText[],
    extraHeaders: Record<string, string>,
    script: Script | undefined,
    run: Run | undefined,
  ): Promise<void> => {
    response.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-store", ...extraHeaders });
    const limit = script?.cutAfterFrames ?? script?.holdAfterFrames ?? frames.length;
    for (const frame of frames.slice(0, limit)) {
      response.write(frame.raw);
      await new Promise((resolve) => setImmediate(resolve));
    }
    if (script?.cutAfterFrames !== undefined && script.cutAfterFrames < frames.length) {
      response.destroy();
      return;
    }
    if (script?.holdAfterFrames !== undefined && run !== undefined) {
      run.held = response;
      response.on("close", () => {
        run.held = undefined;
      });
      return;
    }
    response.end();
  };

  const remember = (session: string, body: Record<string, any>, frames: SseFrameText[]): void => {
    const turns = history.get(session) ?? [];
    const asked = lastUserText(body["messages"] ?? []);
    if (asked !== undefined) turns.push({ id: `msg_${session}_${turns.length + 1}`, role: "user", content: asked });
    const answered = frames
      .map((frame) => JSON.parse(frame.data) as Record<string, any>)
      .filter((event) => event["type"] === "TEXT_MESSAGE_CONTENT")
      .map((event) => String(event["delta"]))
      .join("");
    if (answered !== "") turns.push({ id: `msg_${session}_${turns.length + 1}`, role: "assistant", content: answered });
    history.set(session, turns);
  };

  const handleRun = async (request: IncomingMessage, response: ServerResponse): Promise<void> => {
    const session = authenticate(request, response);
    if (session === undefined) return;
    const rawBody = await readBody(request);
    let body: Record<string, any> | undefined;
    try {
      body = JSON.parse(rawBody) as Record<string, any>;
    } catch {
      body = undefined;
    }
    const isConnect =
      body !== undefined && Array.isArray(body["messages"]) && body["messages"].length === 0 && !(Array.isArray(body["resume"]) && body["resume"].length > 0);
    (isConnect ? connects : requests).push(record(request, rawBody, body));
    if (body === undefined || typeof body["threadId"] !== "string" || typeof body["runId"] !== "string") {
      json(response, 400, errorBody("validation_failed", "the run input is malformed"));
      return;
    }
    if (body["threadId"] !== session) {
      notVisible(response);
      return;
    }
    if (isConnect) {
      // internal/agui/connect.go: the session's history and its state, no turn; a connect run has no cursor and is never recorded.
      const thread = body["threadId"] as string;
      const run = body["runId"] as string;
      const events = [
        { type: "RUN_STARTED", threadId: thread, runId: run },
        { type: "MESSAGES_SNAPSHOT", messages: history.get(session) ?? [] },
        // The state mode follows the client's own state (internal/agui/input.go): an object gets `add /alixai`, so the members the
        // page keeps for itself are never replaced; anything else gets a snapshot.
        typeof body["state"] === "object" && body["state"] !== null && !Array.isArray(body["state"])
          ? { type: "STATE_DELTA", delta: [{ op: "add", path: "/alixai", value: { pending: null } }] }
          : { type: "STATE_SNAPSHOT", snapshot: { alixai: { pending: null } } },
        { type: "RUN_FINISHED", threadId: thread, runId: run, result: { connect: true }, outcome: { type: "success" } },
      ];
      if (options.connectDelayMs !== undefined) await new Promise((resolve) => setTimeout(resolve, options.connectDelayMs));
      response.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-store" });
      for (const event of events) response.write(`data: ${JSON.stringify(event)}

`);
      response.end();
      return;
    }
    if (options.runDelayMs !== undefined) await new Promise((resolve) => setTimeout(resolve, options.runDelayMs));
    const key = `${session}|${body["runId"]}`;
    const prior = runs.get(key);
    if (prior !== undefined) {
      if (prior.held !== undefined) {
        json(response, 409, errorBody("conflict", "the run is already in progress", { reason: "run_in_progress" }));
        return;
      }
      await writeStream(response, prior.frames, {}, undefined, undefined);
      return;
    }

    const script = scripts.shift();
    if (script?.dropBeforeHead === true) {
      request.socket.destroy();
      return;
    }
    if (script?.fail !== undefined) {
      const { status, code, message, details, retryAfterSeconds } = script.fail;
      json(response, status, errorBody(code, message ?? code, details), retryAfterSeconds === undefined ? {} : { "Retry-After": String(retryAfterSeconds) });
      return;
    }
    const name = script?.golden ?? infer(body);
    if (name === undefined) {
      json(response, 400, errorBody("validation_failed", "the fake gateway has no scenario for this request"));
      return;
    }

    // Approval: the secret travels in the header only, once, for a preparation issued in this thread.
    const resume = body["resume"] as Array<{ interruptId: string; status: string; payload?: any }> | undefined;
    if (Array.isArray(resume) && resume.length > 0) {
      const entry = resume[0] as { interruptId: string; status: string; payload?: any };
      const approving = entry.status === "resolved" && entry.payload?.approved === true;
      const preparation = `${session}|${entry.interruptId}`;
      if (!issued.has(preparation)) {
        notVisible(response);
        return;
      }
      if (approving) {
        const presented = request.headers[SECRET_HEADER];
        if (typeof presented !== "string" || presented === "") {
          json(response, 428, errorBody("confirmation_required", "the confirmation secret is required"));
          return;
        }
        if (presented !== issued.get(preparation)) {
          notVisible(response);
          return;
        }
        if (consumed.has(preparation)) {
          json(response, 409, errorBody("conflict", "the preparation was already decided"));
          return;
        }
      }
      consumed.add(preparation);
    }

    const source = golden(name);
    const frames = source.frames.map((frame) => rewrite(frame, body["threadId"], body["runId"]));
    const extra: Record<string, string> = {};
    if (source.meta.response.headers?.["X-AlixAI-Confirmation-Token"] !== undefined) {
      extra["X-AlixAI-Confirmation-Token"] = CONFIRMATION_SECRET;
      issued.set(`${session}|prep_01J00000000000000000000001`, CONFIRMATION_SECRET);
    }
    const run: Run = { frames };
    runs.set(key, run);
    remember(session, body, frames);
    await writeStream(response, frames, extra, script, run);
  };

  const handleEvents = async (request: IncomingMessage, response: ServerResponse, thread: string, runId: string): Promise<void> => {
    const session = authenticate(request, response);
    if (session === undefined) return;
    requests.push(record(request, "", undefined));
    const run = runs.get(`${session}|${runId}`);
    if (run === undefined || session !== thread) {
      notVisible(response);
      return;
    }
    if (eventHolds > 0) {
      eventHolds -= 1;
      json(response, 409, errorBody("conflict", "the run has not produced its events yet", { reason: "run_in_progress" }));
      return;
    }
    const last = request.headers["last-event-id"];
    let from = 0;
    if (typeof last === "string" && last !== "") {
      const at = run.frames.findIndex((frame) => frame.id === last);
      if (at === -1) {
        json(response, 400, errorBody("validation_failed", "the cursor is not an id of this run"));
        return;
      }
      from = at + 1;
    }
    if (from >= run.frames.length) {
      response.writeHead(204, { "Cache-Control": "no-store" });
      response.end();
      return;
    }
    await writeStream(response, run.frames.slice(from), {}, undefined, undefined);
  };

  const handleCancel = (request: IncomingMessage, response: ServerResponse, thread: string, runId: string): void => {
    const session = authenticate(request, response);
    if (session === undefined) return;
    requests.push(record(request, "", undefined));
    const run = runs.get(`${session}|${runId}`);
    if (run === undefined || session !== thread) {
      notVisible(response);
      return;
    }
    cancelled.push(runId);
    const held = run.held;
    if (held !== undefined) {
      held.write(
        `id: 99\ndata: ${JSON.stringify({ type: "RUN_FINISHED", threadId: thread, runId, result: { finishReason: "cancelled" }, outcome: { type: "cancelled" } })}\n\n`,
      );
      held.end();
      run.held = undefined;
    }
    json(response, 200, { thread_id: thread, run_id: runId, status: held === undefined ? "finished" : "cancelling" });
  };

  const server: Server = createServer((request, response) => {
    const url = new URL(request.url ?? "/", "http://127.0.0.1");
    const events = /^\/v1\/agui\/threads\/([^/]+)\/runs\/([^/]+)\/(events|cancel)$/.exec(url.pathname);
    const run = async (): Promise<void> => {
      if (request.method === "POST" && url.pathname === "/v1/agui") return handleRun(request, response);
      if (events !== null && request.method === "GET" && events[3] === "events") {
        return handleEvents(request, response, decodeURIComponent(events[1] as string), decodeURIComponent(events[2] as string));
      }
      if (events !== null && request.method === "POST" && events[3] === "cancel") {
        return handleCancel(request, response, decodeURIComponent(events[1] as string), decodeURIComponent(events[2] as string));
      }
      json(response, 404, errorBody("unknown_capability", "no such route"));
    };
    run().catch((error: unknown) => {
      if (!response.headersSent) json(response, 500, errorBody("internal", String(error)));
      else response.destroy();
    });
  });

  const record = (request: IncomingMessage, rawBody: string, body: Record<string, any> | undefined): RecordedRequest => {
    const headers: Record<string, string> = {};
    for (const [name, value] of Object.entries(request.headers)) if (typeof value === "string") headers[name] = value;
    return { method: request.method ?? "", path: request.url ?? "", headers, body, rawBody };
  };

  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${address.port}`,
    requests,
    connects,
    cancelled,
    queue: (...more) => {
      scripts.push(...more);
    },
    holdEvents: (count) => {
      eventHolds = count;
    },
    close: () =>
      new Promise<void>((resolve) => {
        for (const run of runs.values()) run.held?.destroy();
        server.closeAllConnections();
        server.close(() => resolve());
      }),
  };
};
