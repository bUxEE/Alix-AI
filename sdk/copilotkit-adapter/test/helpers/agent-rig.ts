import type { BaseEvent, RunAgentInput } from "@ag-ui/client";
import type { Observable } from "rxjs";
import { AlixAIAgent } from "../../src/agent.js";
import type { AlixAIAgentConfig, AlixAIContextTokenRequest } from "../../src/agent.js";
import { createMemorySecretStore } from "../../src/secrets.js";
import type { ConfirmationSecretStore } from "../../src/secrets.js";
import { startFakeGateway } from "./fake-gateway.js";
import type { FakeGateway, FakeGatewayOptions } from "./fake-gateway.js";
import { golden } from "./golden.js";
import type { GoldenName } from "./golden.js";

export interface Rig {
  readonly gateway: FakeGateway;
  readonly agent: AlixAIAgent;
  readonly secrets: ConfirmationSecretStore;
  /** What the host's token callback was asked for, in order. */
  readonly tokenRequests: AlixAIContextTokenRequest[];
  close: () => Promise<void>;
}

export interface RigOptions {
  gateway?: FakeGatewayOptions;
  agent?: Partial<AlixAIAgentConfig>;
}

/** A host token callback that mints `<session>.<n>`: single use, with the session the gateway checks against the thread. */
export const mintTokens = (log: AlixAIContextTokenRequest[]): AlixAIAgentConfig["getContextToken"] => {
  let counter = 0;
  return (request) => {
    log.push(request);
    counter += 1;
    return `${request.sessionId}.${counter}`;
  };
};

export const startRig = async (options: RigOptions = {}): Promise<Rig> => {
  const gateway = await startFakeGateway(options.gateway);
  const tokenRequests: AlixAIContextTokenRequest[] = [];
  const secrets = options.agent?.secrets ?? createMemorySecretStore();
  const agent = new AlixAIAgent({
    baseUrl: gateway.url,
    getContextToken: mintTokens(tokenRequests),
    secrets,
    sleep: () => Promise.resolve(),
    ...options.agent,
  });
  return { gateway, agent, secrets, tokenRequests, close: () => gateway.close() };
};

/** The AG-UI input a golden request recorded, as the CopilotRuntime would hand it to the agent. */
export const inputOf = (name: GoldenName): RunAgentInput => {
  const request = golden(name).meta.request;
  if (request === undefined) throw new Error(`the golden ${name} has no request`);
  const body = request.body;
  return {
    threadId: body.threadId,
    runId: body.runId,
    state: body["state"],
    messages: body.messages,
    tools: body["tools"] ?? [],
    context: body["context"] ?? [],
    forwardedProps: body.forwardedProps ?? {},
    ...(body.resume !== undefined && { resume: body.resume }),
  } as RunAgentInput;
};

export const collect = (events: Observable<BaseEvent>): Promise<BaseEvent[]> =>
  new Promise((resolve, reject) => {
    const seen: BaseEvent[] = [];
    events.subscribe({ next: (event) => seen.push(event), error: reject, complete: () => resolve(seen) });
  });

export const goldenEvents = (name: GoldenName): unknown[] => golden(name).meta.response.events;
