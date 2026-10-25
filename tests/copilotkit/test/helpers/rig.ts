import { createServer } from "node:http";
import type { Server } from "node:http";
import type { AddressInfo } from "node:net";
import { HttpAgent } from "@ag-ui/client";
import type { AgentSubscriber, BaseEvent, Message, RunAgentParameters, RunAgentResult } from "@ag-ui/client";
import { createMemorySecretStore } from "@alixai/copilotkit-adapter";
import type { AlixAIContextTokenCallback, AlixAIContextTokenRequest, ConfirmationSecretStore } from "@alixai/copilotkit-adapter";
import { createAlixAINodeHandler, createAlixAIRuntime } from "@alixai/copilotkit-adapter/runtime";
import type { AlixAIRuntime } from "@alixai/copilotkit-adapter/runtime";
// After the adapter's runtime entry, which switches CopilotKit's telemetry off before @copilotkit/runtime loads (the network guard fails the suite otherwise).
import { CopilotSseRuntime, createCopilotRuntimeHandler } from "@copilotkit/runtime/v2";
import { createCopilotNodeHandler } from "@copilotkit/runtime/v2/node";
import { startFakeGateway } from "../../../../sdk/copilotkit-adapter/test/helpers/fake-gateway.js";
import type { FakeGateway } from "../../../../sdk/copilotkit-adapter/test/helpers/fake-gateway.js";
import { golden } from "../../../../sdk/copilotkit-adapter/test/helpers/golden.js";
import type { GoldenName } from "../../../../sdk/copilotkit-adapter/test/helpers/golden.js";

export { CONFIRMATION_SECRET, SECRET_HEADER } from "../../../../sdk/copilotkit-adapter/test/helpers/fake-gateway.js";
export type { FakeGateway, Script } from "../../../../sdk/copilotkit-adapter/test/helpers/fake-gateway.js";
export { golden, GOLDEN_NAMES } from "../../../../sdk/copilotkit-adapter/test/helpers/golden.js";
export type { GoldenName } from "../../../../sdk/copilotkit-adapter/test/helpers/golden.js";

// The whole server side of a host on loopback, and a browser-side AG-UI client in front of it:
//
//   @ag-ui/client HttpAgent  ->  @copilotkit/runtime (CopilotRuntime v2 over node:http)  ->  AlixAIAgent  ->  gateway
//
// The gateway is the replay stand-in of the adapter suite: it serves the golden streams of the Go gateway (tests/agui/testdata)
// byte for byte and enforces the rules the adapter must respect (single-use context token whose session is the thread,
// the confirmation secret in its header only, one approval per preparation, the identical 403 for a foreign thread).
// Streams that no golden covers (token streaming, a diagram block, a connect run) come from scripted-gateway.ts.

/** One response the browser side received from the runtime: everything a page could ever read. */
export interface Traffic {
  readonly url: string;
  readonly status: number;
  /** Every response header, one per line. */
  readonly headers: string;
  readonly body: Promise<string>;
}

export interface Host {
  readonly alixai: AlixAIRuntime;
  readonly runtimeUrl: string;
  readonly secrets: ConfirmationSecretStore;
  /** What the host's token callback was asked for, in order. */
  readonly tokenRequests: AlixAIContextTokenRequest[];
  /** Every request the browser side made to the runtime: method and path. */
  readonly runtimeRequests: string[];
  /** Everything the browser side received from the runtime. */
  readonly traffic: Traffic[];
  /** A browser-side AG-UI client for one thread (one AlixAI session). */
  browser: (threadId?: string, headers?: Record<string, string>) => HttpAgent;
  close: () => Promise<void>;
}

export interface Gate extends Host {
  readonly gateway: FakeGateway;
}

// CopilotKit keeps per-thread state for the life of the process (the runtime's runner is process-global), so every run
// of every test gets a thread of its own: a thread is an AlixAI session id.
let threads = 0;
export const newThread = (): string => `ses_gate_${String((threads += 1)).padStart(3, "0")}`;

/** A host callback that mints `<session>.<n>`: single use, with the session the gateway checks against the thread. */
const mintTokens = (log: AlixAIContextTokenRequest[]): ((request: AlixAIContextTokenRequest) => string) => {
  let counter = 0;
  return (request) => {
    log.push(request);
    counter += 1;
    return `${request.sessionId}.${counter}`;
  };
};

export interface HostOptions {
  /** The host's token callback. Default: mints a token for whatever session is asked for (a host that checks nothing). */
  getContextToken?: AlixAIContextTokenCallback;
}

/** The host side in front of a gateway that is already listening at `baseUrl`. */
export const startHost = async (baseUrl: string, options: HostOptions = {}): Promise<Host> => {
  const tokenRequests: AlixAIContextTokenRequest[] = [];
  const secrets = createMemorySecretStore();
  const custom = options.getContextToken;
  const getContextToken: AlixAIContextTokenCallback =
    custom === undefined
      ? mintTokens(tokenRequests)
      : (request) => {
          tokenRequests.push(request);
          return custom(request);
        };
  const alixai = createAlixAIRuntime({ baseUrl, getContextToken, secrets, sleep: () => Promise.resolve() });
  const server: Server = createServer(createAlixAINodeHandler(alixai, { basePath: "/copilotkit" }));
  const runtimeRequests: string[] = [];
  const traffic: Traffic[] = [];
  server.on("request", (request) => void runtimeRequests.push(`${request.method} ${request.url}`));
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const runtimeUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}/copilotkit`;

  // The browser's fetch, recording every response it reads (body included).
  const recordingFetch = async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const response = await fetch(input, init);
    traffic.push({
      url: input instanceof Request ? input.url : String(input),
      status: response.status,
      headers: [...response.headers].map(([name, value]) => `${name}: ${value}`).join("\n"),
      body: response.clone().text().catch(() => ""),
    });
    return response;
  };

  return {
    alixai,
    runtimeUrl,
    secrets,
    tokenRequests,
    runtimeRequests,
    traffic,
    browser: (threadId = newThread(), headers = {}) =>
      new HttpAgent({ url: `${runtimeUrl}/agent/default/run`, threadId, headers, fetch: recordingFetch as never }),
    close: async () => {
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
};

/**
 * CONTROL for the security tests, never a configuration to use: the same AlixAIAgent in a CopilotRuntime that keeps CopilotKit's
 * own default runner. Its connect route, and its thread routes, answer from the runtime's memory with no callback and no gateway,
 * which is what AlixAIRunner removes. The tests that show a refusal run the same probe against this host first, so that a probe
 * that could never succeed cannot pass for a guard.
 */
export const startUnguardedHost = async (alixai: AlixAIRuntime): Promise<{ runtimeUrl: string; close: () => Promise<void> }> => {
  const runtime = new CopilotSseRuntime({ agents: { [alixai.agentId]: alixai.agent } });
  const handler = createCopilotRuntimeHandler({ runtime, basePath: "/copilotkit", cors: false, activateChannels: false });
  const server: Server = createServer(createCopilotNodeHandler(handler));
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  return {
    runtimeUrl: `http://127.0.0.1:${(server.address() as AddressInfo).port}/copilotkit`,
    close: async () => {
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    },
  };
};

/** The replay gateway, the runtime and the browser client of one test. */
export const startGate = async (options: HostOptions = {}): Promise<Gate> => {
  const gateway = await startFakeGateway({ rewriteIds: true });
  const host = await startHost(gateway.url, options);
  return {
    ...host,
    gateway,
    close: async () => {
      await host.close();
      await gateway.close();
    },
  };
};

// ---- what the page does ----

/** What the golden recorded as the page's request: the agent's messages and state, and the parameters of runAgent. */
export interface PageRun {
  readonly messages: Message[];
  readonly state: unknown;
  readonly parameters: RunAgentParameters;
}

export const pageRunOf = (name: GoldenName): PageRun => {
  const request = golden(name).meta.request;
  if (request?.body === undefined) throw new Error(`the golden ${name} has no recorded request body`);
  const body = request.body;
  return {
    messages: body.messages as unknown as Message[],
    state: body["state"],
    parameters: {
      runId: body.runId,
      tools: (body["tools"] ?? []) as RunAgentParameters["tools"],
      context: (body["context"] ?? []) as RunAgentParameters["context"],
      forwardedProps: body.forwardedProps ?? {},
    },
  };
};

export interface Observed {
  /** What runAgent resolved with, or undefined when it threw. */
  readonly result: RunAgentResult | undefined;
  /** What runAgent threw, if it did. A run that ends in RUN_ERROR is reported by the events, not thrown. */
  readonly error: unknown;
  /** Every AG-UI event the client received, in order, as the real client parsed it. */
  readonly events: BaseEvent[];
}

/** One run of the page's agent, recording every event the real @ag-ui/client received. Never throws. */
export const runAs = async (agent: HttpAgent, parameters: RunAgentParameters, subscriber: AgentSubscriber = {}): Promise<Observed> => {
  const events: BaseEvent[] = [];
  const recorder: AgentSubscriber = {
    ...subscriber,
    onEvent: (params) => {
      events.push(params.event);
      return subscriber.onEvent?.(params);
    },
  };
  try {
    const result = await agent.runAgent(parameters, recorder);
    return { result, error: undefined, events };
  } catch (error) {
    return { result: undefined, error, events };
  }
};

/** Sets what the golden request carried (messages, state) and runs it as the page would. */
export const run = (agent: HttpAgent, name: GoldenName, extra: Partial<RunAgentParameters> = {}): Promise<Observed> => {
  const page = pageRunOf(name);
  agent.setMessages(page.messages);
  agent.setState(page.state);
  return runAs(agent, { ...page.parameters, ...extra });
};

/** The page sends a new user message on the thread (the conversation so far stays in the agent). */
export const say = (agent: HttpAgent, text: string, runId: string, extra: Partial<RunAgentParameters> = {}): Promise<Observed> => {
  agent.addMessage({ id: `u_${runId}`, role: "user", content: text });
  return runAs(agent, { runId, ...extra });
};

/** The page answers the pending interrupt, as CopilotKit's useInterrupt does. */
export const answer = (agent: HttpAgent, approved: boolean, runId: string): Promise<Observed> => {
  const interrupt = agent.pendingInterrupts[0];
  if (interrupt === undefined) throw new Error("the agent has no pending interrupt");
  return runAs(agent, { runId, resume: [{ interruptId: interrupt.id, status: "resolved", payload: { approved } }] });
};

/** What CopilotKit's own client posts when the user presses Stop. */
export const stop = async (host: Host, threadId: string, runId: string): Promise<{ status: number; body: unknown }> => {
  const response = await fetch(`${host.runtimeUrl}/agent/default/stop/${encodeURIComponent(threadId)}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ runId }),
  });
  return { status: response.status, body: await response.json().catch(() => undefined) };
};

/** What CopilotKit's own client posts when a page opens a thread: the runtime's connect route. */
export const connect = async (
  host: Pick<Host, "runtimeUrl">,
  threadId: string,
  headers: Record<string, string> = {},
): Promise<{ status: number; events: Array<Record<string, any>>; text: string }> => {
  const response = await fetch(`${host.runtimeUrl}/agent/default/connect`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "text/event-stream", ...headers },
    body: JSON.stringify({ threadId, runId: `run_connect_${threadId}`, messages: [], state: {}, tools: [], context: [], forwardedProps: {} }),
  });
  const text = await response.text();
  return { status: response.status, events: dataOf(text), text };
};

export const dataOf = (sse: string): Array<Record<string, any>> =>
  sse
    .split("\n\n")
    .flatMap((frame) => frame.split("\n").filter((line) => line.startsWith("data:")).map((line) => JSON.parse(line.slice(5).trim()) as Record<string, any>));

export const typesOf = (events: readonly BaseEvent[]): string[] => events.map((event) => event.type);

/** The golden events of a scenario as the Go gateway recorded them. */
export const goldenEvents = (name: GoldenName): Array<Record<string, any> & { type: string }> => golden(name).meta.response.events;

/** Replaces the parts of an event list that legitimately differ between the recording and a run: thread and expiry. */
export const normalised = (events: readonly unknown[], threadId: string): unknown[] =>
  JSON.parse(
    JSON.stringify(events)
      .replace(/"threadId":"[^"]*"/g, `"threadId":${JSON.stringify(threadId)}`)
      .replace(/"(expiresAt|expires_at)":"[^"]*"/g, '"$1":"<expiry>"'),
  ) as unknown[];

/** The events the real client received, as comparable data: the runtime echoes the browser's input in RUN_STARTED, which the gateway never sends. */
export const received = (events: readonly BaseEvent[], threadId: string): unknown[] =>
  normalised(
    events.map((event) => {
      if (event.type !== "RUN_STARTED") return event;
      const { input: _echo, ...rest } = event as BaseEvent & { input?: unknown };
      return rest;
    }),
    threadId,
  );

/** Every message of a given role. */
export const messagesOf = (agent: HttpAgent, role: Message["role"]): Message[] => agent.messages.filter((message) => message.role === role);

// ---- one golden, through the whole chain ----

/** The runs that answer an interrupt need the run that prepared it, on the same thread. */
const AFTER_PREPARE: Partial<Record<GoldenName, boolean>> = { resume_approve: true, resume_reject: false, stale: true };

/** The goldens that were recorded from the events route and have no request body of their own. */
const WITHOUT_REQUEST: readonly GoldenName[] = ["chart", "table"];

export interface Replay {
  readonly thread: string;
  readonly agent: HttpAgent;
  readonly observed: Observed;
  /** What the page sent (a golden with a request of its own), in the shape the gateway should have received it. */
  readonly sent: Record<string, unknown> | undefined;
}

/**
 * Replays one golden stream the way a page would produce it: the run the golden recorded, preceded by the run that prepared
 * the action when the golden answers an interrupt. The gateway stand-in infers a scenario from the request text, which several
 * goldens share, so every run names its golden.
 */
export const replay = async (gate: Gate, name: GoldenName): Promise<Replay> => {
  const thread = newThread();
  const agent = gate.browser(thread);
  const approved = AFTER_PREPARE[name];

  if (approved !== undefined) {
    gate.gateway.queue({ golden: "prepare_interrupt" }, { golden: name });
    const prepared = await run(agent, "prepare_interrupt");
    if (prepared.error !== undefined || agent.pendingInterrupts.length !== 1) throw new Error(`the run that prepares ${name} did not end in an interrupt`);
    return { thread, agent, observed: await answer(agent, approved, pageRunOf(name).parameters.runId as string), sent: undefined };
  }

  gate.gateway.queue({ golden: name });
  if (WITHOUT_REQUEST.includes(name)) {
    agent.setMessages([{ id: "m1", role: "user", content: `show the ${name}` }]);
    agent.setState({});
    return { thread, agent, observed: await runAs(agent, { runId: `run_${name}` }), sent: undefined };
  }
  const page = pageRunOf(name);
  agent.setMessages(page.messages);
  agent.setState(page.state);
  const sent = {
    threadId: thread,
    runId: page.parameters.runId,
    state: page.state ?? undefined,
    messages: page.messages,
    tools: page.parameters.tools,
    context: page.parameters.context,
    forwardedProps: page.parameters.forwardedProps,
  };
  return { thread, agent, observed: await runAs(agent, page.parameters), sent };
};
