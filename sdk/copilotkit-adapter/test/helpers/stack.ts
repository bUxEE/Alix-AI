import { createServer } from "node:http";
import type { Server } from "node:http";
import type { AddressInfo } from "node:net";
import { createAlixAINodeHandler, createAlixAIRuntime } from "../../src/runtime.js";
import type { AlixAIContextTokenRequest } from "../../src/agent.js";
import { createMemorySecretStore } from "../../src/secrets.js";
import type { ConfirmationSecretStore } from "../../src/secrets.js";
import { mintTokens } from "./agent-rig.js";
import { startFakeGateway } from "./fake-gateway.js";
import type { FakeGateway } from "./fake-gateway.js";

// The whole server side on loopback: the fake AlixAI gateway, and the real CopilotRuntime with AlixAIAgent served over
// node:http at /copilotkit. A page (jsdom) points CopilotKit's runtimeUrl at `runtimeUrl`.

export interface Stack {
  readonly gateway: FakeGateway;
  readonly runtimeUrl: string;
  /** The AlixAI session and AG-UI thread of this stack's page. */
  readonly threadId: string;
  readonly secrets: ConfirmationSecretStore;
  readonly tokenRequests: AlixAIContextTokenRequest[];
  /** What the browser asked the runtime for: method and path. */
  readonly runtimeRequests: string[];
  /** How many responses of the runtime are still open (a connect the runner holds, a run still streaming). */
  lateConnectOpen: () => number;
  close: () => Promise<void>;
}

// CopilotKit keeps per-thread state for the life of the page module, so every stack gets a thread (a session id) of its own.
let threads = 0;

export const startStack = async (options: { connectDelayMs?: number; runDelayMs?: number } = {}): Promise<Stack> => {
  const gateway = await startFakeGateway({ rewriteIds: true, ...options });
  const tokenRequests: AlixAIContextTokenRequest[] = [];
  const secrets = createMemorySecretStore();
  const alixai = createAlixAIRuntime({ baseUrl: gateway.url, getContextToken: mintTokens(tokenRequests), secrets, sleep: () => Promise.resolve() });
  const server: Server = createServer(createAlixAINodeHandler(alixai, { basePath: "/copilotkit" }));
  const runtimeRequests: string[] = [];
  // The responses still open: a connect the runner holds while a run overlaps it, a run that is just ending.
  const open = new Set<object>();
  server.on("request", (request, response) => {
    runtimeRequests.push(`${request.method} ${request.url}`);
    open.add(response);
    response.on("close", () => open.delete(response));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as AddressInfo;
  return {
    gateway,
    runtimeUrl: `http://127.0.0.1:${port}/copilotkit`,
    threadId: `ses_ui_${String((threads += 1)).padStart(2, "0")}`,
    secrets,
    tokenRequests,
    runtimeRequests,
    lateConnectOpen: () => open.size,
    close: async () => {
      // Cutting a connection under a response that is still being written makes the page's fetch reject outside any test: let those end.
      for (let waited = 0; open.size > 0 && waited < 1500; waited += 20) await new Promise((resolve) => setTimeout(resolve, 20));
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
      await gateway.close();
    },
  };
};
